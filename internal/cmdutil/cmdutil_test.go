package cmdutil

import (
	"strings"
	"testing"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		in       string
		wantCmd  string
		wantArgs []string
	}{
		{"/model claude-sonnet-4-5", "/model", []string{"claude-sonnet-4-5"}},
		{"/effort max", "/effort", []string{"max"}},
		{"/mode plan", "/mode", []string{"plan"}},
		{"/cd /home/user/repo", "/cd", []string{"/home/user/repo"}},
		{"/help", "/help", []string{}},
		{"hello world", "", nil},
		{"   /current   ", "/current", []string{}},
		{"/new", "/new", []string{}},
		{"/config ~/.claude/kimi-settings.json", "/config", []string{"~/.claude/kimi-settings.json"}},
		{"/model   a   b  c", "/model", []string{"a", "b", "c"}},
	}
	for _, tc := range tests {
		gotCmd, gotArgs := ParseCommand(tc.in)
		if gotCmd != tc.wantCmd {
			t.Errorf("ParseCommand(%q) cmd = %q, want %q", tc.in, gotCmd, tc.wantCmd)
		}
		if len(gotArgs) != len(tc.wantArgs) {
			t.Errorf("ParseCommand(%q) args = %v, want %v", tc.in, gotArgs, tc.wantArgs)
			continue
		}
		for i := range gotArgs {
			if gotArgs[i] != tc.wantArgs[i] {
				t.Errorf("ParseCommand(%q) args[%d] = %q, want %q", tc.in, i, gotArgs[i], tc.wantArgs[i])
			}
		}
	}
}

func TestRenderHelp(t *testing.T) {
	specs := []Spec{
		{Name: "/model", Summary: "设置模型", Args: "[name]", Level: "success"},
		{Name: "/help", Summary: "显示帮助"},
	}
	out := RenderHelp(specs)
	if !strings.Contains(out, "/model [name] — 设置模型\n") {
		t.Errorf("help missing model line with args: %q", out)
	}
	if !strings.Contains(out, "/help — 显示帮助\n") {
		t.Errorf("help missing plain help line: %q", out)
	}
	if !strings.HasPrefix(out, "命令：\n") {
		t.Errorf("help should start with header, got %q", out)
	}
}
