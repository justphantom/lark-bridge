package ompbridge

import (
	"errors"
	"testing"
)

// TestIsStaleSessionErr locks in the stale-session signature confirmed
// empirically against omp/17.1.8 (exit 1, empty stdout): stderr reads
// `Error: Session "<id>" not found.`, which the client's pump rolls into the
// synthesised EventError text as `<waitErr>; stderr: Error: Session "…" not
// found. Run \`omp --resume\` …`. The match keys on "session" + "not found"
// (case-insensitive) so it stays stable across id values without
// over-matching unrelated errors.
func TestIsStaleSessionErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "confirmed stale signature",
			err:  errors.New(`exit status 1; stderr: Error: Session "defunct-0123456789abcdef0123456789abcdef" not found. Run 'omp --resume' without an argument to pick from recent sessions, or 'omp' to start a new one.`),
			want: true,
		},
		{
			name: "bare stderr form",
			err:  errors.New(`Error: Session "abc" not found.`),
			want: true,
		},
		{
			name: "case-insensitive",
			err:  errors.New("could not find SESSION with id x — NOT FOUND"),
			want: true,
		},
		{
			name: "403 api error does not match",
			err:  errors.New(`exit status 1; stderr: 403 {"error":{"type":"forbidden","message":"Request not allowed"}}`),
			want: false,
		},
		{
			name: "generic network error does not match",
			err:  errors.New("dial tcp: connection refused"),
			want: false,
		},
		{
			name: "only session keyword without not found",
			err:  errors.New("session started successfully"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isStaleSessionErr(tt.err); got != tt.want {
				t.Errorf("isStaleSessionErr(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
