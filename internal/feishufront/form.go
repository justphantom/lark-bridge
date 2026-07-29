package feishufront

import (
	"sort"
	"strconv"
	"strings"
)

func requestIDFromValue(value map[string]any) string {
	if v, ok := value["requestID"].(string); ok {
		return v
	}
	return ""
}

func parseQuestionFormValue(fv map[string]any) (choices []string, custom string) {
	// Per-question selected values keyed by question index. A multi_select_static
	// field submits []any and each picked option becomes its own choice (so a
	// true multi-select reaches the consumer as a multi-element Choices slice);
	// a single select_static submits a string and yields one choice. Questions
	// are emitted in index order, with a multi-select's values in pick order.
	type selEntry struct {
		idx  int
		vals []string
	}
	type custEntry struct {
		idx int
		val string
	}
	var sels []selEntry
	var custs []custEntry
	for name, v := range fv {
		idx, kind, ok := parseFormName(name)
		if !ok {
			continue
		}
		if kind == "custom" {
			custs = append(custs, custEntry{idx, toFormString(v)})
		} else {
			sels = append(sels, selEntry{idx, selectValues(v)})
		}
	}
	sort.Slice(sels, func(i, j int) bool { return sels[i].idx < sels[j].idx })
	for _, e := range sels {
		choices = append(choices, e.vals...)
	}
	sort.Slice(custs, func(i, j int) bool { return custs[i].idx < custs[j].idx })
	var parts []string
	for _, e := range custs {
		parts = append(parts, e.val)
	}
	custom = strings.Join(parts, "\n")
	return choices, custom
}

func parseFormName(name string) (idx int, kind string, ok bool) {
	if strings.HasPrefix(name, "q_") {
		if n, err := strconv.Atoi(strings.TrimPrefix(name, "q_")); err == nil {
			return n, "q", true
		}
	}
	if strings.HasPrefix(name, "custom_") {
		if n, err := strconv.Atoi(strings.TrimPrefix(name, "custom_")); err == nil {
			return n, "custom", true
		}
	}
	return 0, "", false
}

func toFormString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// selectValues extracts the selected value(s) from a question form field.
// multi_select_static submits []any (one entry per picked option, each becoming
// a separate choice); select_static submits a single string. Empty entries are
// dropped so a blank selection yields no choices rather than [""].
func selectValues(v any) []string {
	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, a := range arr {
			if s := toFormString(a); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if s := toFormString(v); s != "" {
		return []string{s}
	}
	return nil
}
