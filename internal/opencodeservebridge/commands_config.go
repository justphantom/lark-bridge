package opencodeservebridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/log"
)

// switchTimeout bounds SwitchModel/SwitchAgent calls. A wedged server must not
// freeze the slash-command goroutine.
const switchTimeout = 10 * time.Second

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
	h.Router.SetModelSpec(chatID, spec)
	cmdutil.LogSettingChange(h.Logger, chatID, "model", spec)
	// 若 session 已存在则立即同步到服务端，下次 prompt 才能用上新 model；
	// session 尚未创建时由 runPrompt 在 CreateSession 时传入。
	if b.SessionID != "" {
		if err := h.agent.SwitchModel(ctx, b.SessionID, spec); err != nil {
			h.Logger.Warn("switch model on server", log.FieldChatID, chatID, log.FieldError, err)
		}
	}
	return cmdutil.ChangeResult("模型", old, spec, "下次提问生效。"), nil
}

// runModelPicker drives the interactive model selection in a background
// goroutine. The opencode serve catalog query can stall on a cold server, so the command
// returns immediately with a placeholder Notice (Handled=true, dispatcher
// skips its own Notice); goSafe runs the slow list → Question → wait →
// confirm loop on h.AppCtx, emitting the selection card and result Notice
// itself. oldSpec is captured by value so concurrent /model calls do not race
// on the binding snapshot.
func (h *Handler) runModelPicker(chatID, oldSpec string) commandResult {
	h.emitNoticeLogged(chatID, "info", "正在加载模型列表", "正在获取可用模型，请稍候（约半分钟）…")
	bridgebase.GoSafe(h.Logger, "model-picker:"+chatID, func() {
		choice, messageID, err := h.AskAndWait(chatID, "", "模型", "选择模型", h.agent.ListModels, true)
		if err != nil {
			h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
			return
		}
		old := oldSpec
		if old == "" {
			old = "默认"
		}
		h.Router.SetModelSpec(chatID, choice)
		cmdutil.LogSettingChange(h.Logger, chatID, "model", choice)
		// 同步到服务端（若 session 已存在）
		if b, ok := h.Router.Lookup(chatID); ok && b.SessionID != "" {
			sctx, cancel := context.WithTimeout(h.AppCtx, switchTimeout)
			defer cancel()
			if err := h.agent.SwitchModel(sctx, b.SessionID, choice); err != nil {
				h.Logger.Warn("switch model on server", log.FieldChatID, chatID, log.FieldError, err)
			}
		}
		res := cmdutil.ChangeResult("模型", old, choice, "下次提问生效。")
		h.emitCardUpdateLogged(chatID, messageID, "success", "已切换模型", res.Body, res.Field, res.Before, res.After)
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
	h.Router.SetModelSpec(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "model", "")
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
	h.Router.SetAgent(chatID, agent)
	cmdutil.LogSettingChange(h.Logger, chatID, "agent", agent)
	// 若 session 已存在则立即同步到服务端。
	if b.SessionID != "" {
		if err := h.agent.SwitchAgent(ctx, b.SessionID, agent); err != nil {
			h.Logger.Warn("switch agent on server", log.FieldChatID, chatID, log.FieldError, err)
		}
	}
	return cmdutil.ChangeResult("agent", old, agent, "下次提问生效。"), nil
}

// runAgentPicker is the agent analogue of runModelPicker. See that function
// for why it runs async, emits its own notices, and returns Handled.
func (h *Handler) runAgentPicker(chatID, oldAgent string) commandResult {
	h.emitNoticeLogged(chatID, "info", "正在加载 agent 列表", "正在获取可用 agent，请稍候（约半分钟）…")
	bridgebase.GoSafe(h.Logger, "agent-picker:"+chatID, func() {
		choice, messageID, err := h.AskAndWait(chatID, "", "agent", "选择 agent", h.agent.ListAgents, true)
		if err != nil {
			h.emitNoticeLogged(chatID, "error", "选择失败", err.Error())
			return
		}
		old := oldAgent
		if old == "" {
			old = "默认"
		}
		h.Router.SetAgent(chatID, choice)
		cmdutil.LogSettingChange(h.Logger, chatID, "agent", choice)
		// 同步到服务端（若 session 已存在）
		if b, ok := h.Router.Lookup(chatID); ok && b.SessionID != "" {
			sctx, cancel := context.WithTimeout(h.AppCtx, switchTimeout)
			defer cancel()
			if err := h.agent.SwitchAgent(sctx, b.SessionID, choice); err != nil {
				h.Logger.Warn("switch agent on server", log.FieldChatID, chatID, log.FieldError, err)
			}
		}
		res := cmdutil.ChangeResult("agent", old, choice, "下次提问生效。")
		h.emitCardUpdateLogged(chatID, messageID, "success", "已切换 agent", res.Body, res.Field, res.Before, res.After)
	})
	return commandResult{Handled: true}
}

// clearAgentSpec is the /agent clear path.
func clearAgentSpec(h *Handler, chatID, oldAgent string) commandResult {
	old := oldAgent
	if old == "" {
		old = "默认"
	}
	h.Router.SetAgent(chatID, "")
	cmdutil.LogSettingChange(h.Logger, chatID, "agent", "")
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
