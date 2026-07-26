package opencodebridge

import (
	"reflect"
	"testing"

	"github.com/justphantom/lark-bridge/internal/protocol"
)

func TestParseTodoItems(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []protocol.TodoItem
		ok    bool
	}{
		{
			name:  "empty input rejected",
			input: "",
			ok:    false,
		},
		{
			name:  "empty object rejected",
			input: "{}",
			ok:    false,
		},
		{
			name:  "non-json rejected",
			input: "not json",
			ok:    false,
		},
		{
			name:  "empty todos array rejected",
			input: `{"todos":[]}`,
			ok:    false,
		},
		{
			name:  "todos not an array rejected",
			input: `{"todos":"not-array"}`,
			ok:    false,
		},
		{
			name:  "standard list with priority",
			input: `{"todos":[{"content":"写测试","status":"in_progress","priority":"high"},{"content":"跑测试","status":"pending"}]}`,
			want: []protocol.TodoItem{
				{Content: "写测试", Status: "in_progress", Priority: "high"},
				{Content: "跑测试", Status: "pending"},
			},
			ok: true,
		},
		{
			name:  "all four statuses preserved",
			input: `{"todos":[{"content":"a","status":"pending"},{"content":"b","status":"in_progress"},{"content":"c","status":"completed"},{"content":"d","status":"cancelled"}]}`,
			want: []protocol.TodoItem{
				{Content: "a", Status: "pending"},
				{Content: "b", Status: "in_progress"},
				{Content: "c", Status: "completed"},
				{Content: "d", Status: "cancelled"},
			},
			ok: true,
		},
		{
			// opencode allows a todo without status; it decodes to "". The
			// frontend's Validate rejects "" status, but parseTodoItems stays
			// liberal — bridge should not silently drop items based on a
			// value check the SDK already enforces upstream.
			name:  "missing status preserved as empty",
			input: `{"todos":[{"content":"a"}]}`,
			want:  []protocol.TodoItem{{Content: "a"}},
			ok:    true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseTodoItems(tc.input)
			if ok != tc.ok {
				t.Fatalf("parseTodoItems(%q) ok = %v, want %v", tc.input, ok, tc.ok)
			}
			if !ok {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseTodoItems(%q) = %+v, want %+v", tc.input, got, tc.want)
			}
		})
	}
}
