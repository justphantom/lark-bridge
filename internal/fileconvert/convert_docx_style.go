package fileconvert

import (
	"bytes"
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"
)

// nsW is the wordprocessingml main namespace. As with the pptx parser,
// matching on Space (not the prefix a producer happened to bind) keeps
// parsing robust across MS Office / WPS / LibreOffice exports.
const nsW = "http://schemas.openxmlformats.org/wordprocessingml/2006/main"

// basedOnMaxDepth caps w:basedOn chain resolution. Cyclic stylesheets exist
// in the wild (bad template generators), so an unguarded walk could spin
// forever on a hostile or corrupt docx.
const basedOnMaxDepth = 8

// styleInfo is one paragraph style's digest. outline keeps the raw
// w:outlineLvl val ("" = unset) so the body sentinel "9" stays
// distinguishable from "no opinion" until headingLevel resolves it.
type styleInfo struct {
	name     string // w:name val, lowercased
	outline  string // raw w:outlineLvl val, "" = unset
	basedOn  string // parent styleId, "" = none
	numID    string // style-linked numbering, "" = none
	ilvl     int    // style-linked list level
	hasNumPr bool
}

// styleIndex is the pre-pass digest of word/styles.xml. Only paragraph
// styles are indexed; character/table styles are irrelevant at L1+.
type styleIndex struct {
	byID map[string]styleInfo
}

// parseStyles digests word/styles.xml into a styleIndex. nil/garbage input
// yields an empty (non-nil) index — a docx without usable styles simply
// degrades to body-text paragraphs (docx-extract-design.md §2.2).
func parseStyles(data []byte) *styleIndex {
	idx := &styleIndex{byID: map[string]styleInfo{}}
	if len(data) == 0 {
		return idx
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	var curID string
	var cur styleInfo
	inStyle := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return idx
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != nsW {
				continue
			}
			switch t.Name.Local {
			case "style":
				curID = attrW(t, "styleId")
				cur = styleInfo{}
				inStyle = attrW(t, "type") == "paragraph" && curID != ""
			case "name":
				if inStyle {
					cur.name = strings.ToLower(attrW(t, "val"))
				}
			case "basedOn":
				if inStyle {
					cur.basedOn = attrW(t, "val")
				}
			case "outlineLvl":
				if inStyle {
					cur.outline = attrW(t, "val")
				}
			case "numPr":
				if inStyle {
					cur.hasNumPr = true
				}
			case "numId":
				if inStyle && cur.hasNumPr {
					cur.numID = attrW(t, "val")
				}
			case "ilvl":
				if inStyle && cur.hasNumPr {
					cur.ilvl, _ = strconv.Atoi(attrW(t, "val"))
				}
			}
		case xml.EndElement:
			if t.Name.Space == nsW && t.Name.Local == "style" {
				if inStyle {
					idx.byID[curID] = cur
				}
				inStyle = false
			}
		}
	}
}

// headingLevel resolves the 1-based heading level for a paragraph carrying
// pStyle id (and a direct outlineLvl override, "" = none). Resolution order
// (docx-extract-design.md §2.3): direct outlineLvl → style outlineLvl →
// basedOn chain → name fallback ("heading N" / "标题 N") → 0 (body). The
// outlineLvl=9 sentinel means body text and never yields a heading.
func (s *styleIndex) headingLevel(styleID, directOutline string) int {
	if lvl, ok := outlineToLevel(directOutline); ok {
		return lvl
	}
	id := styleID
	for depth := 0; depth < basedOnMaxDepth && id != ""; depth++ {
		info, found := s.byID[id]
		if !found {
			break
		}
		if lvl, ok := outlineToLevel(info.outline); ok {
			return lvl
		}
		id = info.basedOn
	}
	if info, found := s.byID[styleID]; found {
		return headingFromName(info.name)
	}
	return 0
}

// styleList resolves the style-linked numbering reference for styleID,
// walking the basedOn chain (numbered headings typically inherit from a base
// heading style). ok=false when no ancestor links a numId.
func (s *styleIndex) styleList(styleID string) (numID string, ilvl int, ok bool) {
	id := styleID
	for depth := 0; depth < basedOnMaxDepth && id != ""; depth++ {
		info, found := s.byID[id]
		if !found {
			break
		}
		if info.hasNumPr && info.numID != "" {
			return info.numID, info.ilvl, true
		}
		id = info.basedOn
	}
	return "", 0, false
}

// isCodeStyle reports whether styleID names a code paragraph style
// (decision Q7: name whitelist, case-insensitive — "Code", "HTML Code",
// "Source Code"). "Macro Text" does not contain "code" and stays body.
func (s *styleIndex) isCodeStyle(styleID string) bool {
	info, found := s.byID[styleID]
	return found && strings.Contains(info.name, "code")
}

// outlineToLevel converts a raw w:outlineLvl val to a 1-based heading
// level. ok=false when the val is unset (caller falls through to the next
// resolution step); the 9 sentinel maps to 0 (body) with ok=true so an
// explicit "body" on a child style stops the chain walk.
func outlineToLevel(raw string) (int, bool) {
	if raw == "" {
		return 0, false
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, false
	}
	if n >= 9 {
		return 0, true
	}
	return n + 1, true
}

// headingNameRe matches the localised heading style names seen in MS Office
// ("heading 1") and WPS / Chinese templates ("标题 1").
var headingNameRe = regexp.MustCompile(`^(?:heading|标题)\s*([1-9])$`)

// headingFromName is the last-resort heading detector for stylesheets that
// carry no outlineLvl anywhere (some WPS exports). Returns 0 for non-heading
// names.
func headingFromName(name string) int {
	m := headingNameRe.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	return int(m[1][0] - '0')
}

// attrW returns the value of the w:-namespaced attribute with the given
// local name (w:val, w:styleId, …). The Space check keeps a stray unprefixed
// "val" attribute from being picked up.
func attrW(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local && a.Name.Space == nsW {
			return a.Value
		}
	}
	return ""
}
