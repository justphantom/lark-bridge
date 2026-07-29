package fileconvert

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"path"
	"strings"
)

// ooRelsNS is the OOXML relationships namespace. Attributes authored as
// r:id live in this namespace regardless of the prefix a producer chose, so
// matching on Space (not prefix spelling) is the robust check.
const ooRelsNS = "http://schemas.openxmlformats.org/officeDocument/2006/relationships"

// relationship mirrors one <Relationship> element from a .rels part.
type relationship struct {
	ID       string
	Type     string
	Target   string
	External bool
}

// wbSheet mirrors one <sheet> element from xl/workbook.xml.
type wbSheet struct {
	Name string
	RID  string
}

// detectChartsAndPivots inspects the raw xlsx zip to localise charts per
// sheet and flag pivot tables workbook-wide. excelize v2 exposes no read API
// for charts, so this walks the OOXML relationship chain directly:
//
//	xl/workbook.xml <sheet r:id> → xl/_rels/workbook.xml.rels → worksheet part →
//	worksheet .rels (drawing) → drawing .rels (chart) counted per sheet.
//
// Everything here is best-effort: a malformed part is skipped, not fatal —
// losing a placeholder never blocks a conversion excelize could otherwise
// complete. Pivot localisation to a specific sheet is deliberately not
// attempted: the pivotTable part's <location> carries only a cell range, not
// a sheet name, and chasing it through pivotCache records is disproportionate
// to how rare pivot tables are in uploads. The workbook-wide flag is enough
// to honour the "never silently skip" contract (decision 9A).
func detectChartsAndPivots(srcPath string) (map[string]int, bool) {
	chartCounts := map[string]int{}
	zr, err := zip.OpenReader(srcPath)
	if err != nil {
		return chartCounts, false
	}
	defer func() { _ = zr.Close() }()

	parts := make(map[string]*zip.File, len(zr.File))
	for _, zf := range zr.File {
		parts[zf.Name] = zf
	}

	var sheets []wbSheet
	if wb := parts["xl/workbook.xml"]; wb != nil {
		if data, rerr := readZipPart(wb); rerr == nil {
			sheets = parseWorkbookSheets(data)
		}
	}

	wbRels := relsMap(parts, "xl/_rels/workbook.xml.rels")
	for _, sh := range sheets {
		if sh.Name == "" || sh.RID == "" {
			continue
		}
		rel, ok := wbRels[sh.RID]
		if !ok || rel.External {
			continue
		}
		wsPath := path.Clean(path.Join("xl", rel.Target))
		// Walk worksheet (or chartsheet) → drawings → charts. A chartsheet's
		// rels point straight at a drawing, so the same loop covers both.
		for _, drel := range relsMap(parts, relsPathFor(wsPath)) {
			if drel.External || !isDrawingRel(drel.Type) {
				continue
			}
			drawPath := path.Clean(path.Join(path.Dir(wsPath), drel.Target))
			for _, crel := range relsMap(parts, relsPathFor(drawPath)) {
				if !crel.External && isChartRel(crel.Type) {
					chartCounts[sh.Name]++
				}
			}
		}
	}

	hasPivot := false
	for name := range parts {
		if strings.HasPrefix(name, "xl/pivotTables/") && strings.HasSuffix(name, ".xml") {
			hasPivot = true
			break
		}
	}
	return chartCounts, hasPivot
}

// relsMap parses the .rels part at name (if present) into an Id→relationship
// map. Returns an empty (non-nil) map when the part is absent — a worksheet
// with no drawings simply has no .rels, which is the common case.
func relsMap(parts map[string]*zip.File, name string) map[string]relationship {
	out := map[string]relationship{}
	zf := parts[name]
	if zf == nil {
		return out
	}
	data, err := readZipPart(zf)
	if err != nil {
		return out
	}
	for _, rel := range parseRelationships(data) {
		out[rel.ID] = rel
	}
	return out
}

// relsPathFor returns the .rels part path that accompanies a given part, e.g.
// "xl/worksheets/sheet1.xml" → "xl/worksheets/_rels/sheet1.xml.rels".
func relsPathFor(partPath string) string {
	dir := path.Dir(partPath)
	return path.Join(dir, "_rels", path.Base(partPath)+".rels")
}

// readZipPart reads one zip entry fully into memory. These metadata parts are
// tiny (kilobytes), so buffering the whole entry is cheaper than streaming.
func readZipPart(zf *zip.File) ([]byte, error) {
	rc, err := zf.Open()
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// isDrawingRel matches the worksheet→drawing relationship type. containment
// on "/drawing" keeps this prefix-agnostic across producers.
func isDrawingRel(t string) bool { return strings.Contains(t, "/drawing") }

// isChartRel matches the drawing→chart relationship type. within a drawing's
// .rels only chart relationships contain "/chart", so this is unambiguous.
func isChartRel(t string) bool { return strings.Contains(t, "/chart") }

// parseWorkbookSheets extracts the ordered <sheet name= r:id=> list from
// xl/workbook.xml. Order here is the presentation order, so names line up
// with the conversion output.
func parseWorkbookSheets(data []byte) []wbSheet {
	var sheets []wbSheet
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return sheets
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "sheet" {
			continue
		}
		var s wbSheet
		for _, a := range se.Attr {
			switch {
			case a.Name.Local == "name":
				s.Name = a.Value
			case a.Name.Local == "id" && a.Name.Space == ooRelsNS:
				s.RID = a.Value
			}
		}
		sheets = append(sheets, s)
	}
}

// parseDate1904 reports whether the workbook uses the 1904 date system
// (<workbookPr date1904="1"/>, old Mac exports). The serial→ISO conversion
// switches its epoch on this flag (xlsx-extract-design.md §2.4).
func parseDate1904(data []byte) bool {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "workbookPr" {
			continue
		}
		for _, a := range se.Attr {
			if a.Name.Local == "date1904" {
				return a.Value == "1" || a.Value == "true"
			}
		}
		return false
	}
}

// parseRelationships extracts all <Relationship> entries from a .rels part.
// External (hyperlink-style) targets are flagged so callers can skip them
// when resolving internal part paths.
func parseRelationships(data []byte) []relationship {
	var rels []relationship
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return rels
		}
		se, ok := tok.(xml.StartElement)
		if !ok || se.Name.Local != "Relationship" {
			continue
		}
		var rel relationship
		for _, a := range se.Attr {
			switch a.Name.Local {
			case "Id":
				rel.ID = a.Value
			case "Type":
				rel.Type = a.Value
			case "Target":
				rel.Target = a.Value
			case "TargetMode":
				rel.External = a.Value == "External"
			}
		}
		rels = append(rels, rel)
	}
}
