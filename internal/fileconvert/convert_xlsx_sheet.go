package fileconvert

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

// xlsxCtxCheckInterval mirrors the docx parser's budget: poll ctx once per
// N rows so a hostile workbook cannot pin the dispatcher goroutine.
const xlsxCtxCheckInterval = 64

// xlsxMaxColumns is the OOXML column ceiling (XFD). colLettersToIdx clamps to
// it so a hostile cell reference cannot trigger unbounded row padding.
const xlsxMaxColumns = 16384

// xlsxMaxPadCells bounds the empty-string padding one sheet may accumulate
// (sparse-column gaps in flushCell + rectangular fill in normalise). Real
// cell content costs the attacker XML bytes proportionally, but padding is
// free amplification: ~50 bytes of XML per 128 KB row of empties. Past the
// budget the sheet degrades to a 读取失败 placeholder via the parseSheet
// recover, same as any other broken part.
const xlsxMaxPadCells = 1 << 20

// errSheetTooSparse is the panic sentinel for the padding budget; parseSheet's
// recover maps it to a plain error instead of a "parser panic" message.
var errSheetTooSparse = errors.New("sheet exceeds padding budget (hostile sparse columns)")

// sheetResult carries one parsed sheet: the rectangular row set for
// renderTable plus the per-sheet caveat counters that become aggregate
// placeholders after the table (decisions Q4/Q5, §4.3).
type sheetResult struct {
	rows          [][]string
	unknownFmt    int // unrecognised custom date patterns, raw value emitted
	sharedFormula int // shared-formula followers, cached value emitted
	noCache       int // formula cells without a cached value
	sstOverrun    int // shared-string indexes past the pool
}

// parseSheet streams one worksheet XML into rows (xlsx-extract-design.md
// §2.5/§2.6). Type dispatch follows the t attribute; dates convert through
// the numFmt index; formula text is collected for free during the same pass
// (decision Q5). Sparse columns pad with empty strings and all rows are
// normalised to the sheet's max width afterwards, matching the rectangular
// output renderTable expects. Fully empty rows are dropped: renderTable
// treats row 0 as the GFM header, and a blank header row would corrupt the
// table (excelize's GetRows behaves the same way).
//
// A parser panic on a hostile sheet is recovered into an error (same
// contract as the docx safeRun): the caller renders a 读取失败 placeholder
// for this sheet and continues with the next, never crashing the process.
func parseSheet(ctx context.Context, data []byte, sst []string, fmts *numFmtIndex, date1904 bool, formulaMode string) (res *sheetResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			res = nil
			if r == errSheetTooSparse {
				err = errSheetTooSparse
			} else {
				err = fmt.Errorf("parser panic: %v", r)
			}
		}
	}()
	p := &sheetParser{
		sst:         sst,
		fmts:        fmts,
		date1904:    date1904,
		formulaMode: formulaMode,
		res:         &sheetResult{},
	}
	dec := xml.NewDecoder(bytes.NewReader(data))
	rowCount := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// A malformed worksheet demotes the whole sheet to a 读取失败
			// placeholder (§4.3); the caller continues with the next sheet.
			return nil, fmt.Errorf("parse worksheet XML: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			p.start(t)
		case xml.EndElement:
			if p.end(t) {
				rowCount++
				if rowCount%xlsxCtxCheckInterval == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
			}
		case xml.CharData:
			p.charData(t)
		}
	}
	p.normalise()
	return p.res, nil
}

// sheetParser is the streaming state for one worksheet. It mirrors the docx
// parser's shape: just enough context to rebuild rows without modelling the
// full spreadsheetml tree.
type sheetParser struct {
	sst         []string
	fmts        *numFmtIndex
	date1904    bool
	formulaMode string
	res         *sheetResult

	// row state
	inRow  bool
	curRow []string
	curCol int // next expected column index when r attributes are absent

	// cell state
	inC      bool
	cellCol  int
	cellType string
	cellSty  int
	hasV     bool
	vBuf     strings.Builder
	inV      bool
	hasF     bool
	fBuf     strings.Builder
	inF      bool
	fShared  bool // <f t="shared"> follower with no text of its own
	isBuf    strings.Builder
	inIs     bool // inside <is> (inlineStr)
	inT      bool // inside <is><t>
	maxCols  int

	padCells int // cumulative empty-string padding, capped by xlsxMaxPadCells
}

func (p *sheetParser) start(se xml.StartElement) {
	if se.Name.Space != nsS {
		return
	}
	switch se.Name.Local {
	case "row":
		p.inRow = true
		p.curRow = nil
		p.curCol = 0
	case "c":
		p.inC = true
		p.cellType = attrS(se, "t")
		p.cellSty = atoiSafe(attrS(se, "s"))
		p.cellCol = p.curCol
		if ref := attrS(se, "r"); ref != "" {
			p.cellCol = colLettersToIdx(ref)
		}
		p.hasV, p.hasF, p.fShared = false, false, false
		p.vBuf.Reset()
		p.fBuf.Reset()
		p.isBuf.Reset()
	case "v":
		if p.inC {
			p.inV = true
			p.hasV = true
		}
	case "f":
		if p.inC {
			p.inF = true
			p.hasF = true
			p.fShared = attrS(se, "t") == "shared"
		}
	case "is":
		if p.inC {
			p.inIs = true
		}
	case "t":
		if p.inIs {
			p.inT = true
		}
	}
}

// end returns true when a row closed (the caller's ctx-check tick).
func (p *sheetParser) end(ee xml.EndElement) bool {
	if ee.Name.Space != nsS {
		return false
	}
	switch ee.Name.Local {
	case "v":
		p.inV = false
	case "f":
		p.inF = false
		// A shared-formula follower carries t="shared" and no text: the
		// expansion is not reconstructed at L1 (decision Q5 caveat).
		if p.hasF && p.fShared && p.fBuf.Len() == 0 {
			p.res.sharedFormula++
			p.hasF = false
		}
	case "is":
		p.inIs = false
	case "t":
		p.inT = false
	case "c":
		if p.inRow {
			p.flushCell()
		}
		p.inC = false
	case "row":
		p.inRow = false
		if !rowIsEmpty(p.curRow) {
			p.res.rows = append(p.res.rows, p.curRow)
		}
		if len(p.curRow) > p.maxCols {
			p.maxCols = len(p.curRow)
		}
		return true
	}
	return false
}

func (p *sheetParser) charData(cd xml.CharData) {
	switch {
	case p.inV:
		p.vBuf.Write(cd)
	case p.inF:
		p.fBuf.Write(cd)
	case p.inT:
		p.isBuf.Write(cd)
	}
}

// flushCell converts the open cell into its output value and lands it in the
// current row at its column index, padding gaps with empty strings.
func (p *sheetParser) flushCell() {
	for len(p.curRow) < p.cellCol {
		p.curRow = append(p.curRow, "")
		p.padCells++
	}
	if p.padCells > xlsxMaxPadCells {
		panic(errSheetTooSparse)
	}
	p.curRow = append(p.curRow, p.cellValue())
	p.curCol = p.cellCol + 1
}

// cellValue applies type dispatch (§2.5) then the formula mode (§2.6).
func (p *sheetParser) cellValue() string {
	raw := p.vBuf.String()
	value := raw
	switch p.cellType {
	case "s":
		if idx, ok := parseSSTIndex(raw); ok && idx < len(p.sst) {
			value = p.sst[idx]
		} else {
			// Garbage or out-of-pool index: count an overrun rather than
			// emitting sst[0]'s wrong data (and never index with it).
			p.res.sstOverrun++
			value = ""
		}
	case "b":
		switch raw {
		case "1":
			value = "TRUE"
		case "":
			value = ""
		default:
			value = "FALSE"
		}
	case "e", "str", "d":
		value = raw
	case "inlineStr":
		value = p.isBuf.String()
	default: // "n" / absent — numeric, possibly a date
		if raw != "" {
			switch cls := p.fmts.classify(p.cellSty); cls {
			case classDate, classDatetime, classTime:
				if f, ok := parseSerial(raw); ok {
					value = serialToISO(f, cls, p.date1904)
				}
			case classUnknownDate:
				p.res.unknownFmt++
			case classNumber:
				// raw passthrough (decision Q2)
			}
		}
	}

	formula := p.fBuf.String()
	switch p.formulaMode {
	case "formula":
		if p.hasF && formula != "" {
			return formula
		}
	case "both":
		if p.hasF && formula != "" {
			if value != "" {
				return value + " (" + formula + ")"
			}
			return "(" + formula + ")"
		}
	}
	// value mode (and the formula-mode fallback for formula-less cells):
	// count formulas whose cache is missing so the caveat stays honest.
	if p.hasF && !p.hasV {
		p.res.noCache++
	}
	return value
}

// normalise pads every row to the sheet's max width so renderTable (and the
// fenced-CSV fallback, which does no padding of its own) stays rectangular.
func (p *sheetParser) normalise() {
	for i, r := range p.res.rows {
		for len(r) < p.maxCols {
			r = append(r, "")
			p.padCells++
		}
		if p.padCells > xlsxMaxPadCells {
			panic(errSheetTooSparse)
		}
		p.res.rows[i] = r
	}
}

// parseSSTIndex parses a sharedStrings index strictly: all digits, at most 9
// (a 9-digit cap keeps the multiply-accumulate far from int overflow, and no
// real workbook approaches 10^9 shared strings). ok=false marks garbage the
// caller counts as an overrun instead of trusting.
func parseSSTIndex(raw string) (int, bool) {
	if raw == "" || len(raw) > 9 {
		return 0, false
	}
	n := 0
	for i := range len(raw) {
		if raw[i] < '0' || raw[i] > '9' {
			return 0, false
		}
		n = n*10 + int(raw[i]-'0')
	}
	return n, true
}

// colLettersToIdx converts a cell reference's column letters ("A", "BC") to
// a 0-based column index. Parsing stops at the first non-letter, so the row
// digits are simply ignored; a reference with no letters yields 0. The index
// is clamped to the OOXML maximum (XFD = 16384 columns): a hostile reference
// like "ZZZZZZZ1" would otherwise make flushCell pad the row with billions
// of empty strings (memory-amplification DoS).
func colLettersToIdx(ref string) int {
	idx := 0
	for i := range len(ref) {
		c := ref[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		if c < 'A' || c > 'Z' {
			break
		}
		idx = idx*26 + int(c-'A'+1)
		if idx > xlsxMaxColumns {
			return xlsxMaxColumns - 1
		}
	}
	if idx == 0 {
		return 0
	}
	return idx - 1
}
