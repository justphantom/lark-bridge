package feishufront

import (
	"strings"
	"testing"
	"text/template"

	"github.com/justphantom/lark-bridge/internal/feishu"
	"github.com/justphantom/lark-bridge/internal/fileconvert"
)

func TestRenderXlsxSheetsSection(t *testing.T) {
	meta := &fileconvert.XlsxMeta{
		Sheets: []fileconvert.SheetMeta{
			{Name: "销售明细", Columns: []string{"订单ID", "日期", "金额"}, RowCount: 1200},
			{Name: "图表 Sheet", Columns: nil, RowCount: 0, Note: "contains 1 chart(s), not extracted"},
		},
		Note: "workbook contains pivot table(s), not extracted",
	}
	got := renderXlsxSheetsSection(meta)
	for _, want := range []string{
		`Sheet "销售明细": 3 columns [订单ID, 日期, 金额], 1200 rows`,
		`Sheet "图表 Sheet": 0 columns [], 0 rows (contains 1 chart(s), not extracted)`,
		`- workbook contains pivot table(s), not extracted`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestExecuteXlsxPromptTemplate_SchemaOnlyNoDataRows(t *testing.T) {
	// Decision Q11: the prompt carries path + column names + row counts and
	// NOTHING else — cell values live only in the .md on disk. Assert both
	// the presence of schema lines and the absence of any data row.
	tmpl := template.Must(template.New("xlsx").Parse(
		`Read {{.Path}} (uploaded as {{.FileName}}, {{.SheetCount}} sheets):
{{.SheetsSection}}{{if .UserText}}
用户的附加说明：{{.UserText}}{{end}}`))
	d := NewDispatcher(&fakeSink{}, NewBackendRegistry(), NewTurnManager(), nil)
	d.SetXlsxPromptTemplate(tmpl)

	meta := &fileconvert.XlsxMeta{
		Sheets: []fileconvert.SheetMeta{
			{Name: "销售明细", Columns: []string{"订单ID", "金额"}, RowCount: 2},
		},
	}
	got, err := d.executeXlsxPromptTemplate("data.xlsx", "/inbox/oc/p/data.md", meta, &feishu.IncomingMessage{})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	for _, want := range []string{
		"/inbox/oc/p/data.md",
		"data.xlsx",
		"1 sheets",
		`Sheet "销售明细": 2 columns [订单ID, 金额], 2 rows`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

func TestExecuteXlsxPromptTemplate_UserTextAppended(t *testing.T) {
	tmpl := template.Must(template.New("xlsx").Parse(
		`{{.Path}}|{{.SheetCount}}|{{.SheetsSection}}{{if .UserText}}|{{.UserText}}{{end}}`))
	d := NewDispatcher(&fakeSink{}, NewBackendRegistry(), NewTurnManager(), nil)
	d.SetXlsxPromptTemplate(tmpl)

	got, err := d.executeXlsxPromptTemplate("d.xlsx", "/p/d.md", &fileconvert.XlsxMeta{}, &feishu.IncomingMessage{Content: "求总和"})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(got, "|求总和") {
		t.Errorf("UserText not appended: %q", got)
	}
}
