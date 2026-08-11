package bridgebase

import (
	"testing"
	"time"
)

// TestFormatDuration covers the three time bands.
func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45秒"},
		{90 * time.Second, "1分30秒"},
		{2 * time.Minute, "2分钟"},
		{70 * time.Minute, "1小时10分"},
		{2 * time.Hour, "2小时"},
	}
	for _, c := range cases {
		if got := FormatDuration(c.d); got != c.want {
			t.Errorf("FormatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}
