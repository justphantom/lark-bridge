package fileconvert

import (
	"bytes"
	"encoding/xml"
	"math"
	"strconv"
	"strings"
	"time"
)

// valClass is the L1 value classification for a cell (xlsx-extract-design.md
// §2.4). It answers only "is this a date/time?" — never "how does it
// display?": classified dates render as ISO 8601 (decision Q3), everything
// else passes the raw <v> through (decision Q2).
type valClass int

const (
	classNumber      valClass = iota // raw <v> passthrough
	classDate                        // → 2006-01-02
	classDatetime                    // → 2006-01-02 15:04:05
	classTime                        // → 15:04:05
	classUnknownDate                 // custom pattern off the L1 whitelist (decision Q4)
)

// numFmtIndex maps a cell's style index (s attribute) to its value class,
// built from xl/styles.xml in the pre-pass: the custom numFmt table plus
// cellXfs (style index → numFmtId).
type numFmtIndex struct {
	custom map[int]string // numFmtId (≥164) → formatCode
	xfs    []int          // cellXfs: style index → numFmtId
}

// parseNumFmts digests xl/styles.xml. Attributes in this part are unprefixed
// (no namespace), so matching is on Local name only. nil/garbage input yields
// an empty index that classifies everything classNumber (§4.3 degradation).
func parseNumFmts(data []byte) *numFmtIndex {
	idx := &numFmtIndex{custom: map[int]string{}}
	if len(data) == 0 {
		return idx
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	inCellXfs := false
	for {
		tok, err := dec.Token()
		if err != nil {
			return idx
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != nsS {
				continue
			}
			switch t.Name.Local {
			case "numFmt":
				id := atoiSafe(attrS(t, "numFmtId"))
				if code := attrS(t, "formatCode"); code != "" {
					idx.custom[id] = code
				}
			case "cellXfs":
				inCellXfs = true
			case "xf":
				if inCellXfs {
					idx.xfs = append(idx.xfs, atoiSafe(attrS(t, "numFmtId")))
				}
			}
		case xml.EndElement:
			if t.Name.Space == nsS && t.Name.Local == "cellXfs" {
				inCellXfs = false
			}
		}
	}
}

// classify maps a cell's s (style) attribute to its value class. Built-in
// date IDs hit the table; custom IDs (≥164) go through the conservative
// pattern recogniser. Out-of-range styles and unknown IDs stay classNumber.
func (n *numFmtIndex) classify(s int) valClass {
	if s < 0 || s >= len(n.xfs) {
		return classNumber
	}
	id := n.xfs[s]
	if cls, ok := builtinDateClass(id); ok {
		return cls
	}
	if code, ok := n.custom[id]; ok {
		return classifyFormat(code)
	}
	return classNumber
}

// builtinDateClass tabulates the ECMA-376 built-in numFmtIds that carry
// date/time semantics (§2.4): 14-17 short dates, 18-21 times, 22 datetime,
// 27-36 CJK date variants, 45-47 stopwatch formats, 50-58 CJK extensions.
func builtinDateClass(id int) (valClass, bool) {
	switch {
	case id >= 14 && id <= 17:
		return classDate, true
	case id >= 18 && id <= 21:
		return classTime, true
	case id == 22:
		return classDatetime, true
	case id >= 27 && id <= 36:
		return classDate, true
	case id >= 45 && id <= 47:
		return classTime, true
	case id >= 50 && id <= 58:
		return classDate, true
	}
	return classNumber, false
}

// classifyFormat is the conservative custom-pattern recogniser (§2.4). It
// strips everything that is not a format token — quoted literals, backslash
// escapes, [Color]/[$-locale]/[condition] sections, _/* padding — keeps only
// the first section, then classifies on the remaining y/d/h/s/m letters.
// The rule is deliberately asymmetric (宁漏不误): a misclassified number
// becomes a nonsense ISO date (bad), a missed date stays a readable serial
// with a placeholder (acceptable, decision Q4).
func classifyFormat(code string) valClass {
	if i := strings.IndexByte(code, ';'); i >= 0 {
		code = code[:i]
	}
	var hasY, hasD, hasH, hasS, hasM bool
	for i := 0; i < len(code); i++ {
		switch c := code[i]; c {
		case '"':
			if j := strings.IndexByte(code[i+1:], '"'); j >= 0 {
				i += j + 1
			}
		case '\\', '_', '*':
			i++ // escaped char / padding: skip the next byte
		case '[':
			if j := strings.IndexByte(code[i+1:], ']'); j >= 0 {
				i += j + 1
			}
		case 'y', 'Y':
			hasY = true
		case 'd', 'D':
			hasD = true
		case 'h', 'H':
			hasH = true
		case 's', 'S':
			hasS = true
		case 'm', 'M':
			hasM = true
		}
	}
	switch {
	case hasY || hasD:
		if hasH || hasS {
			return classDatetime
		}
		return classDate
	case (hasH || hasS) && hasM:
		return classTime
	case hasM:
		// An isolated m with no y/d/h/s anchor is ambiguous (minutes? a
		// literal in a numeric format?) — do not guess.
		return classUnknownDate
	}
	return classNumber
}

// serialToISO converts an Excel serial to ISO 8601 per cls (§2.4). The 1900
// leap-year bug is compensated with the 1899-12-30 epoch, which is exact for
// every serial ≥ 61 (i.e. every date after 1900-02-28 — prehistoric serials
// 1-59 do not occur in uploaded files); date1904 workbooks (old Mac exports)
// use the 1904-01-01 epoch. Fractional days become the time of day, rounded
// to the second.
func serialToISO(serial float64, cls valClass, date1904 bool) string {
	epoch := time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		epoch = time.Date(1904, 1, 1, 0, 0, 0, 0, time.UTC)
	}
	days, frac := math.Modf(serial)
	seconds := int(math.Round(frac * 86400))
	d := int(days)
	if seconds >= 86400 {
		d++
		seconds -= 86400
	}
	t := epoch.AddDate(0, 0, d).Add(time.Duration(seconds) * time.Second)
	switch cls {
	case classDate:
		return t.Format("2006-01-02")
	case classTime:
		return t.Format("15:04:05")
	default: // classDatetime
		return t.Format("2006-01-02 15:04:05")
	}
}

// attrS returns the value of an unprefixed attribute in spreadsheet parts
// (styles.xml / worksheets), where attributes carry no namespace.
func attrS(se xml.StartElement, local string) string {
	for _, a := range se.Attr {
		if a.Name.Local == local && a.Name.Space == "" {
			return a.Value
		}
	}
	return ""
}

// parseSerial parses a raw <v> into a float for date conversion. Returns
// ok=false for non-numeric content so the caller can pass it through raw.
func parseSerial(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil
}
