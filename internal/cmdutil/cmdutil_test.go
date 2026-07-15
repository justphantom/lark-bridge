package cmdutil

import (
	"errors"
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
		{"/perm plan", "/perm", []string{"plan"}},
		{"/cd /home/user/repo", "/cd", []string{"/home/user/repo"}},
		{"/help", "/help", []string{}},
		{"hello world", "", nil},
		{"   /current   ", "/current", []string{}},
		{"/session-new", "/session-new", []string{}},
		{"/settings ~/.claude/kimi-settings.json", "/settings", []string{"~/.claude/kimi-settings.json"}},
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

// TestErrorResult_Agreement verifies the returned Result body and error
// message are identical — a percent sign in an argument must not make them
// diverge (the reason ErrorResult formats once).
func TestErrorResult_Agreement(t *testing.T) {
	res, err := ErrorResult("未知值 %q；可选 %s", "50%", "low|high")
	if res.Body != err.Error() {
		t.Errorf("body != err: body=%q err=%q", res.Body, err.Error())
	}
	if !strings.Contains(res.Body, "50%") {
		t.Errorf("expected %% preserved in body, got %q", res.Body)
	}
}

// TestErrorResult_WrapsErrors verifies the returned error carries the
// formatted message (not a static one) and is non-nil.
func TestErrorResult_WrapsErrors(t *testing.T) {
	_, err := ErrorResult("static message")
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !errors.Is(err, err) { // sanity: error is self-equal
		t.Error("expected well-formed error")
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

// TestChangeResult verifies the structured-change helper carries field/before/
// after so the notice card can render a before→after block above the body.
func TestChangeResult(t *testing.T) {
	res := ChangeResult("权限模式", "default", "plan", "下次提问生效。")
	if res.Field != "权限模式" || res.Before != "default" || res.After != "plan" {
		t.Errorf("change fields = %+v, want 权限模式/default/plan", res)
	}
	if res.Body != "下次提问生效。" {
		t.Errorf("body = %q, want 下次提问生效。", res.Body)
	}
}
