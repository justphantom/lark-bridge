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
	type entry struct {
		idx int
		val string
	}
	var sels, custs []entry
	for name, v := range fv {
		idx, kind, ok := parseFormName(name)
		if !ok {
			continue
		}
		if kind == "custom" {
			custs = append(custs, entry{idx, toFormString(v)})
		} else {
			sels = append(sels, entry{idx, joinMultiSelect(v)})
		}
	}
	sort.Slice(sels, func(i, j int) bool { return sels[i].idx < sels[j].idx })
	for _, e := range sels {
		choices = append(choices, e.val)
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

func joinMultiSelect(v any) string {
	if arr, ok := v.([]any); ok {
		parts := make([]string, 0, len(arr))
		for _, a := range arr {
			parts = append(parts, toFormString(a))
		}
		return strings.Join(parts, ",")
	}
	return toFormString(v)
}
