package fileconvert

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"
)

// lvlInfo is one list level's definition. fmt keeps the raw w:numFmt val;
// only "bullet" gets special rendering, everything else degrades to a
// decimal counter (decision Q5 — no letter/roman/CJK counter forms).
type lvlInfo struct {
	start int
	fmt   string
}

// styleListLink is the reverse pStyle → (numId, ilvl) association harvested
// from numbering.xml's <w:lvl><w:pStyle> entries. Word's numbered headings
// commonly carry no numPr on the paragraph or style; the numbering part is
// the only place the link exists (docx-extract-design.md §2.4).
type styleListLink struct {
	numID string
	ilvl  int
}

// numberingIndex resolves (numId, ilvl) → level definition, built from
// word/numbering.xml in the pre-pass.
type numberingIndex struct {
	numToAbs map[string]string          // numId → abstractNumId
	lvls     map[string]map[int]lvlInfo // abstractNumId → ilvl → def
	byStyle  map[string]styleListLink   // pStyle → (numId, ilvl) reverse link
}

// parseNumbering digests word/numbering.xml. The part is a two-way indirection
// — <w:num> maps numId → abstractNumId, <w:abstractNum> carries the per-level
// defs — so parsing collects both first, then joins them; nums are kept in
// document order so the style-link join is deterministic when several nums
// share one abstractNum. nil/garbage input yields an empty (non-nil) index.
func parseNumbering(data []byte) *numberingIndex {
	idx := &numberingIndex{
		numToAbs: map[string]string{},
		lvls:     map[string]map[int]lvlInfo{},
		byStyle:  map[string]styleListLink{},
	}
	if len(data) == 0 {
		return idx
	}
	type numRef struct{ numID, absID string }
	var nums []numRef
	// absStyleLinks collects (absID, ilvl, pStyle) as abstractNums parse;
	// the numId half of the link only exists once <w:num> entries are read.
	type absStyleLink struct {
		absID  string
		ilvl   int
		pStyle string
	}
	var links []absStyleLink

	dec := xml.NewDecoder(bytes.NewReader(data))
	var absID string // current abstractNum, "" = outside
	var lvlIlvl int  // current lvl, valid only insideLvl
	insideLvl := false
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != nsW {
				continue
			}
			switch t.Name.Local {
			case "num":
				// <w:num> carries its numId; the abstractNumId child follows.
				if id := attrW(t, "numId"); id != "" {
					nums = append(nums, numRef{numID: id})
				}
			case "abstractNumId":
				if n := len(nums); n > 0 && nums[n-1].absID == "" {
					nums[n-1].absID = attrW(t, "val")
				}
			case "abstractNum":
				absID = attrW(t, "abstractNumId")
				if absID != "" && idx.lvls[absID] == nil {
					idx.lvls[absID] = map[int]lvlInfo{}
				}
			case "lvl":
				if absID == "" {
					continue
				}
				lvlIlvl = clampIlvl(attrW(t, "ilvl"))
				insideLvl = true
				idx.lvls[absID][lvlIlvl] = lvlInfo{start: 1}
			case "start":
				if insideLvl {
					if v, err := strconv.Atoi(attrW(t, "val")); err == nil {
						l := idx.lvls[absID][lvlIlvl]
						l.start = v
						idx.lvls[absID][lvlIlvl] = l
					}
				}
			case "numFmt":
				if insideLvl {
					l := idx.lvls[absID][lvlIlvl]
					l.fmt = attrW(t, "val")
					idx.lvls[absID][lvlIlvl] = l
				}
			case "pStyle":
				if insideLvl {
					if ps := attrW(t, "val"); ps != "" {
						links = append(links, absStyleLink{absID: absID, ilvl: lvlIlvl, pStyle: ps})
					}
				}
			}
		case xml.EndElement:
			if t.Name.Space != nsW {
				continue
			}
			switch t.Name.Local {
			case "lvl":
				insideLvl = false
			case "abstractNum":
				absID = ""
			}
		}
	}

	for _, n := range nums {
		if n.absID != "" {
			idx.numToAbs[n.numID] = n.absID
		}
	}
	// Join style links through the num→abs map in document order so the
	// outcome is deterministic even when several nums alias one abstractNum.
	for _, l := range links {
		for _, n := range nums {
			if n.absID == l.absID {
				if _, taken := idx.byStyle[l.pStyle]; !taken {
					idx.byStyle[l.pStyle] = styleListLink{numID: n.numID, ilvl: l.ilvl}
				}
				break
			}
		}
	}
	return idx
}

// styleLink returns the numbering.xml-side pStyle association ("" when the
// style has none). Consulted after styles.xml's own numPr link misses.
func (n *numberingIndex) styleLink(pStyle string) (styleListLink, bool) {
	l, ok := n.byStyle[pStyle]
	return l, ok
}

// listKey identifies one counter in an open list: the numbering instance and
// the level within it.
type listKey struct {
	numID string
	ilvl  int
}

// listState tracks open list levels across paragraphs (docx-extract-design.md
// §2.4). Counters key on (numId, ilvl); emitting any item resets all deeper
// counters of the same numId (OOXML restart semantics — a new "1." at level 0
// restarts the "(a),(b),…" beneath it).
type listState struct {
	counters map[listKey]int
}

func newListState() *listState {
	return &listState{counters: map[listKey]int{}}
}

// marker returns the GFM prefix for entering (numID, ilvl): indent of two
// spaces per level, then "- " for bullet formats or "N. " for everything
// else (decision Q5). ok=false when numID has no definition — the caller
// falls back to a bullet and counts a missingNumPr placeholder.
func (l *listState) marker(idx *numberingIndex, numID string, ilvl int) (prefix string, ok bool) {
	indent := strings.Repeat("  ", min(max(ilvl, 0), maxListLevel))
	absID, found := idx.numToAbs[numID]
	if !found {
		return "", false
	}
	lvl, found := idx.lvls[absID][ilvl]
	if !found {
		return "", false
	}
	// Deeper levels of this list restart whenever any shallower-or-equal
	// item appears, bullet or numbered alike.
	for k := range l.counters {
		if k.numID == numID && k.ilvl > ilvl {
			delete(l.counters, k)
		}
	}
	if lvl.fmt == "bullet" {
		return indent + "- ", true
	}
	k := listKey{numID: numID, ilvl: ilvl}
	if _, seen := l.counters[k]; !seen {
		l.counters[k] = lvl.start
	} else {
		l.counters[k]++
	}
	return indent + strconv.Itoa(l.counters[k]) + ". ", true
}
