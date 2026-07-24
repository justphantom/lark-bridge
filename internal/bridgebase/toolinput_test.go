package bridgebase

import "testing"

// TestSummarizeToolInput_TodoWrite pins the todowrite special path: the
// generic key list and first-string-value fallback both reduce a todos
// array to noise, so a dedicated path folds it to "清单 N/M"
// (N=non-pending, M=total). Cases here cover the empty/partial/full mix
// plus the boundary where every item is pending (N=0).
func TestSummarizeToolInput_TodoWrite(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{
			name:  "all statuses counted",
			input: `{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"in_progress"},{"content":"c","status":"pending"},{"content":"d","status":"cancelled"}]}`,
			want:  "清单 3/4",
		},
		{
			name:  "all pending is zero non-pending",
			input: `{"todos":[{"content":"a","status":"pending"},{"content":"b","status":"pending"}]}`,
			want:  "清单 0/2",
		},
		{
			name:  "all completed is full",
			input: `{"todos":[{"content":"a","status":"completed"},{"content":"b","status":"completed"}]}`,
			want:  "清单 2/2",
		},
		{
			name:  "missing status counts as non-pending",
			input: `{"todos":[{"content":"a"},{"content":"b","status":"pending"}]}`,
			want:  "清单 1/2",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SummarizeToolInput("todowrite", tc.input); got != tc.want {
				t.Errorf("SummarizeToolInput(todowrite, %s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestSummarizeToolInput_TodoWriteDegraded guards the fall-through: a
// todowrite input without a usable todos array must NOT silently render ""
// — it should drop into the generic path so the user still sees something.
func TestSummarizeToolInput_TodoWriteDegraded(t *testing.T) {
	tests := []struct {
		name, input, want string
	}{
		{
			// Empty array: nothing to count, no other field → raw input is
			// the only honest signal.
			name:  "empty todos array falls back to raw input",
			input: `{"todos":[]}`,
			want:  `{"todos":[]}`,
		},
		{
			// Non-array todos: the generic first-string-value fallback
			// picks up the stray string. The point of this case is that
			// the todowrite path does not crash / swallow it silently.
			name:  "todos not an array falls through to generic path",
			input: `{"todos":"not-array"}`,
			want:  "not-array",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SummarizeToolInput("todowrite", tc.input); got != tc.want {
				t.Errorf("SummarizeToolInput(todowrite, %s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestSummarizeToolInput_TodoReadNotSpecialCased locks the exact-match
// rule: todoread is a different tool and must NOT take the todowrite path
// even if its input happens to carry a todos key — otherwise a future
// todoread schema change would silently flip its summary shape.
func TestSummarizeToolInput_TodoReadNotSpecialCased(t *testing.T) {
	// Same shape that todowrite would fold to "清单 1/2"; todoread must
	// instead fall through to the generic first-string-value pick.
	input := `{"todos":[{"content":"a","status":"completed"}],"query":"pending"}`
	got := SummarizeToolInput("todoread", input)
	if got == "清单 1/1" {
		t.Fatalf("todoread must not take the todowrite path; got %q", got)
	}
	if got != "pending" {
		t.Errorf("todoread should hit the generic query key; got %q, want %q", got, "pending")
	}
}
