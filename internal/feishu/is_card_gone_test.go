package feishu

import (
	"errors"
	"testing"
)

func TestIsCardGone(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"withdrawn", errors.New("code:230011 msg:The message was withdrawn."), true},
		{"invalid id", errors.New("code:99992354 msg:not a valid open_message_id"), true},
		{"content too large (not gone)", errors.New("code:230025 msg:content reaches limit"), false},
		{"network error", errors.New("dial tcp: i/o timeout"), false},
	}
	for _, tt := range tests {
		if got := IsCardGone(tt.err); got != tt.want {
			t.Errorf("%s: IsCardGone = %v, want %v (err=%v)", tt.name, got, tt.want, tt.err)
		}
	}
}
