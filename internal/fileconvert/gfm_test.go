package fileconvert

import (
	"strings"
	"testing"
)

func TestRenderTable_Gfm(t *testing.T) {
	rows := [][]string{
		{"区域", "销售额"},
		{"华东", "¥1.2M"},
		{"华南", "¥0.8M"},
	}
	got := renderTable(rows)
	if !strings.Contains(got, "| 区域 | 销售额 |") {
		t.Errorf("missing header row:\n%s", got)
	}
	if !strings.Contains(got, "| --- | --- |") {
		t.Errorf("missing separator row:\n%s", got)
	}
	if !strings.Contains(got, "| 华东 | ¥1.2M |") {
		t.Errorf("missing body row:\n%s", got)
	}
}

func TestRenderTable_PipeEscaped(t *testing.T) {
	rows := [][]string{
		{"a", "b"},
		{"x|y", "c"},
	}
	got := renderTable(rows)
	if !strings.Contains(got, "x\\|y") {
		t.Errorf("pipe not escaped:\n%s", got)
	}
}

func TestRenderTable_RaggedRowsPadded(t *testing.T) {
	// A body row shorter than the header must be padded so the table stays
	// rectangular; a longer one must be clipped.
	rows := [][]string{
		{"h1", "h2", "h3"},
		{"only1"},
		{"a", "b", "c", "d"},
	}
	got := renderTable(rows)
	if !strings.Contains(got, "| only1 |  |  |") {
		t.Errorf("short row not padded:\n%s", got)
	}
	if strings.Contains(got, "d |") {
		t.Errorf("long row not clipped:\n%s", got)
	}
}

func TestRenderTable_NewlineToBr(t *testing.T) {
	rows := [][]string{
		{"h"},
		{"line1\nline2"},
	}
	got := renderTable(rows)
	if !strings.Contains(got, "line1<br>line2") {
		t.Errorf("newline not converted to <br>:\n%s", got)
	}
}

func TestRenderTable_WideTableDegradesToCSV(t *testing.T) {
	header := make([]string, 21) // > gfmMaxColumns (20)
	for i := range header {
		header[i] = "c"
	}
	rows := [][]string{header, append([]string{"v"}, make([]string, 20)...)}
	got := renderTable(rows)
	if !strings.HasPrefix(got, "```csv\n") {
		t.Errorf("wide table should be a fenced csv block, got:\n%s", got)
	}
	if !strings.HasSuffix(got, "```\n") {
		t.Errorf("csv block not closed:\n%s", got)
	}
}

func TestRenderTable_Empty(t *testing.T) {
	if got := renderTable(nil); got != "" {
		t.Errorf("nil rows should render empty, got %q", got)
	}
}
