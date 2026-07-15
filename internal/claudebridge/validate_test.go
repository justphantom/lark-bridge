package claudebridge

import "testing"

// TestValidateSessionDirPath covers the shape check ensureBinding applies to
// an Event-carried directory before MkdirAll: relative paths and parent-
// traversal segments are rejected; clean absolute paths pass (existence is
// established by MkdirAll, not here).
func TestValidateSessionDirPath(t *testing.T) {
	tests := []struct {
		name    string
		dir     string
		wantErr bool
	}{
		{"clean absolute path", "/opt/ws/proj", false},
		{"relative path rejected", "opt/proj", true},
		{"relative traversal rejected", "../../etc", true},
		{"dotdot-only rejected", "..", true},
		// "/a/../b" Clean()s to "/b": no ".." segment remains, so it passes —
		// the resolved target is a legitimate absolute path.
		{"absolute that collapses clean", "/a/../b", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSessionDirPath(tc.dir)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateSessionDirPath(%q) err = %v, wantErr = %v", tc.dir, err, tc.wantErr)
			}
		})
	}
}
