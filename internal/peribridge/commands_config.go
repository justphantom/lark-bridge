package peribridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hu/lark-bridge/internal/cmdutil"
)

// cmdModel pins or clears the model alias for the current chat. Forms:
//   - /model clear   → clear the pin (fall back to ~/.peri/settings.json)
//   - /model <alias> → pin <alias> (e.g. /model sonnet), passed as --model
//
// There is no interactive picker: peri exposes no list subcommand, so the
// available aliases are whatever the user configured in ~/.peri/settings.json
// (provider.models.opus/sonnet/haiku). A typo'd alias fails on the next run.
func (h *Handler) cmdModel(_ context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}
	if len(args) == 0 {
		return commandResult{Body: "用法：`/model <别名>` 设置（如 opus/sonnet/haiku），或 `/model clear` 清除。模型别名对应 ~/.peri/settings.json 中 provider.models 的配置。"}, nil
	}
	if args[0] == "clear" {
		old := b.ModelSpec
		if old == "" {
			old = "默认"
		}
		h.router.SetModelSpec(chatID, "")
		cmdutil.LogSettingChange(h.logger, chatID, "model", "")
		return cmdutil.ChangeResult("模型", old, "默认", "已清除模型设置，将使用 ~/.peri/settings.json 的默认配置。"), nil
	}
	spec := strings.Join(args, " ")
	old := b.ModelSpec
	if old == "" {
		old = "默认"
	}
	h.router.SetModelSpec(chatID, spec)
	cmdutil.LogSettingChange(h.logger, chatID, "model", spec)
	return cmdutil.ChangeResult("模型", old, spec, "下次提问生效。"), nil
}

// validateAbsDir checks that dir is an absolute, existing, writable directory.
// Used by /cd before pinning a path.
func validateAbsDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("目录不可访问：%w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("路径不是目录：%s", dir)
	}
	probe, err := os.MkdirTemp(dir, ".cdprobe-*")
	if err != nil {
		return fmt.Errorf("目录不可写（权限不足）：%w", err)
	}
	_ = os.Remove(probe)
	return nil
}

// validateSessionDirPath checks the shape of a session directory the bridge is
// about to create from an Event-carried override: it must be an absolute path.
func validateSessionDirPath(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}
	return nil
}
