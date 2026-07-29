package fileconvert

import (
	"bytes"
	"encoding/xml"
	"strings"
)

// nsS is the spreadsheetml main namespace. As with the other parsers,
// matching on Space (not the prefix a producer happened to bind) keeps
// parsing robust across MS Office / WPS / LibreOffice exports.
const nsS = "http://schemas.openxmlformats.org/spreadsheetml/2006/main"

// parseSharedStrings digests xl/sharedStrings.xml into the pool indexed by
// t="s" cells (xlsx-extract-design.md §2.3). Each <si> is either a plain
// <t> or a sequence of rich-text <r>/<t> runs that concatenate; phonetic
// <rPh> content (furigana readings) is skipped — it is annotation, not body
// text. nil/garbage input yields nil, and t="s" cells then degrade to empty
// strings with an aggregate placeholder (§4.3).
func parseSharedStrings(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	var out []string
	dec := xml.NewDecoder(bytes.NewReader(data))
	var buf strings.Builder
	inSi, inT, inRPh := false, false, false
	for {
		tok, err := dec.Token()
		if err != nil {
			return out
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Space != nsS {
				continue
			}
			switch t.Name.Local {
			case "si":
				inSi = true
				buf.Reset()
			case "t":
				if inSi && !inRPh {
					inT = true
				}
			case "rPh":
				if inSi {
					inRPh = true
				}
			}
		case xml.EndElement:
			if t.Name.Space != nsS {
				continue
			}
			switch t.Name.Local {
			case "si":
				out = append(out, buf.String())
				inSi = false
			case "t":
				inT = false
			case "rPh":
				inRPh = false
			}
		case xml.CharData:
			if inT {
				buf.Write(t)
			}
		}
	}
}
