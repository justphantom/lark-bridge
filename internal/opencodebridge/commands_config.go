package opencodebridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hu/lark-bridge/internal/cmdutil"
)

// cmdModel pins, clears, or interactively selects the model for the current
// chat. Forms:
//   - /model            → pop a selection card listing `opencode models` output
//   - /model clear      → clear the pin (fall back to opencode's default)
//   - /model <spec>     → pin <spec> directly (e.g. /model anthropic/claude)
//
// The spec is passed to the CLI as --model on the next run.
func (h *Handler) cmdModel(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		// Interactive picker runs async (opencode models is slow); emit a
		// placeholder Notice immediately, then Handled so the dispatcher
		// does not also send one.
		return h.runModelPicker(chatID, b.ModelSpec), nil
	}
	if args[0] == "clear" {
		return clearModelSpec(h, chatID, b.ModelSpec), nil
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

// runModelPicker drives the interactive model selection in a background
// goroutine. The opencode CLI takes 25–50s to list models, so the command
// returns immediately with a placeholder Notice (Handled=true, dispatcher
// skips its own Notice); goSafe runs the slow list → Question → wait →
// confirm loop on h.appCtx, emitting the selection card and result Notice
// itself. oldSpec is captured by value so concurrent /model calls do not race
// on the binding snapshot.
func (h *Handler) runModelPicker(chatID, oldSpec string) commandResult {
	h.emitNoticeLogged(chatID, "info", "正在加载模型列表", "正在获取可用模型，请稍候（约半分钟）…")
	goSafe(h.logger, "model-picker:"+chatID, func() {
		choice, err := h.askAndWait(chatID, "", "模型", "选择模型", h.agent.ListModels, true)
		if err != nil {
			h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
			return
		}
		old := oldSpec
		if old == "" {
			old = "默认"
		}
		h.router.SetModelSpec(chatID, choice)
		cmdutil.LogSettingChange(h.logger, chatID, "model", choice)
		res := cmdutil.ChangeResult("模型", old, choice, "下次提问生效。")
		h.emitNoticeLogged(chatID, "success", "已切换模型", res.Body, res.Field, res.Before, res.After)
	})
	return commandResult{Handled: true}
}

// clearModelSpec is the /model clear path, factored out so runModelPicker and
// the direct clear command share one implementation.
func clearModelSpec(h *Handler, chatID, oldSpec string) commandResult {
	old := oldSpec
	if old == "" {
		old = "默认"
	}
	h.router.SetModelSpec(chatID, "")
	cmdutil.LogSettingChange(h.logger, chatID, "model", "")
	return cmdutil.ChangeResult("模型", old, "默认", "已清除模型设置，将使用 opencode 默认模型。")
}

// cmdAgent pins, clears, or interactively selects the opencode agent for the
// current chat. Forms mirror /model:
//   - /agent            → pop a selection card listing `opencode agent list`
//   - /agent clear      → clear the pin (fall back to opencode's default)
//   - /agent <name>     → pin <name> directly (e.g. /agent build)
//
// The agent is passed to the CLI as --agent on the next run.
func (h *Handler) cmdAgent(ctx context.Context, chatID string, args []string) (commandResult, error) {
	b, err := h.ensureBinding(chatID, "", "", "", "")
	if err != nil {
		return commandResult{Body: err.Error()}, err
	}

	if len(args) == 0 {
		return h.runAgentPicker(chatID, b.Agent), nil
	}
	if args[0] == "clear" {
		return clearAgentSpec(h, chatID, b.Agent), nil
	}

	agent := strings.Join(args, " ")
	old := b.Agent
	if old == "" {
		old = "默认"
	}
	h.router.SetAgent(chatID, agent)
	cmdutil.LogSettingChange(h.logger, chatID, "agent", agent)
	return cmdutil.ChangeResult("agent", old, agent, "下次提问生效。"), nil
}

// runAgentPicker is the agent analogue of runModelPicker. See that function
// for why it runs async, emits its own notices, and returns Handled.
func (h *Handler) runAgentPicker(chatID, oldAgent string) commandResult {
	h.emitNoticeLogged(chatID, "info", "正在加载 agent 列表", "正在获取可用 agent，请稍候（约半分钟）…")
	goSafe(h.logger, "agent-picker:"+chatID, func() {
		choice, err := h.askAndWait(chatID, "", "agent", "选择 agent", h.agent.ListAgents, true)
		if err != nil {
			h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
			return
		}
		old := oldAgent
		if old == "" {
			old = "默认"
		}
		h.router.SetAgent(chatID, choice)
		cmdutil.LogSettingChange(h.logger, chatID, "agent", choice)
		res := cmdutil.ChangeResult("agent", old, choice, "下次提问生效。")
		h.emitNoticeLogged(chatID, "success", "已切换 agent", res.Body, res.Field, res.Before, res.After)
	})
	return commandResult{Handled: true}
}

// clearAgentSpec is the /agent clear path.
func clearAgentSpec(h *Handler, chatID, oldAgent string) commandResult {
	old := oldAgent
	if old == "" {
		old = "默认"
	}
	h.router.SetAgent(chatID, "")
	cmdutil.LogSettingChange(h.logger, chatID, "agent", "")
	return cmdutil.ChangeResult("agent", old, "默认", "已清除 agent 设置，将使用 opencode 默认 agent。")
}

// cmdDirectory is implemented in dir_cache.go alongside the workspace scan
// and validation helpers.

// validateAbsDir checks that dir is an absolute path, an existing directory,
// and writable by the current uid -- the same uid the opencode subprocess will
// run as, so the probe result is authoritative.
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
// Event.Directory is empty in production (the frontend never sets it), so this
// is defence in depth — the workspace boundary itself is enforced by /cd. A
// relative path is rejected so an untrusted Event cannot make the subprocess
// CWD relative to the process working directory. ".." is not checked: for an
// absolute path filepath.Clean resolves it away, so IsAbs is sufficient here.
// Existence is not required (unlike /cd's validateAbsDir) — ensureBinding
// creates the dir via MkdirAll on demand.
func validateSessionDirPath(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}
	return nil
}
