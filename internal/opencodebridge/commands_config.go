package opencodebridge

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/lark-bridge/internal/bridgebase"
	"github.com/justphantom/lark-bridge/internal/cmdutil"
	"github.com/justphantom/lark-bridge/internal/protocol"
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
		// Interactive picker runs async (opencode models is slow); the
		// command's progress card becomes the picker card (see
		// runModelPicker), so the dispatcher must not also send one.
		return h.runModelPicker(chatID, b.ModelSpec, bridgebase.ReplyToID(ctx)), nil
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
	return cmdutil.ChangeResult("模型", old, spec, "下次提问生效。"), nil
}

// runModelPicker drives the interactive model selection in a background
// goroutine. The opencode CLI takes 25–50s to list models, so the command
// returns immediately (Handled=true, dispatcher skips its own Notice);
// goSafe runs the slow list → Question → wait → confirm loop on h.AppCtx.
// The whole flow lives on the command's progress card: a text delta marks the
// loading phase, the Question control carries replyToID so the frontend
// morphs that card into the picker (TakeOverProgress), and the result patches
// the same card via UpdateMessageID. oldSpec is captured by value so
// concurrent /model calls do not race on the binding snapshot.
func (h *Handler) runModelPicker(chatID, oldSpec, replyToID string) commandResult {
	h.emitAsync(replyToID, &protocol.Control{
		Type:   protocol.TypeText,
		ChatID: chatID,
		Text:   &protocol.TextPayload{Delta: "🔍 正在获取可用模型，请稍候（约半分钟）…\n"},
	})
	bridgebase.GoSafe(h.Logger, "model-picker:"+chatID, func() {
		choice, messageID, err := h.AskAndWait(chatID, replyToID, "模型", "选择模型", h.agent.ListModels, true)
		if err != nil {
			h.emitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
			return
		}
		old := oldSpec
		if old == "" {
			old = "默认"
		}
		h.Router.SetModelSpec(chatID, choice)
		cmdutil.LogSettingChange(h.Logger, chatID, "model", choice)
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
		return h.runAgentPicker(chatID, b.Agent, bridgebase.ReplyToID(ctx)), nil
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
	return cmdutil.ChangeResult("agent", old, agent, "下次提问生效。"), nil
}

// runAgentPicker is the agent analogue of runModelPicker. See that function
// for why it runs async, keeps the flow on the command's progress card, and
// returns Handled.
func (h *Handler) runAgentPicker(chatID, oldAgent, replyToID string) commandResult {
	h.emitAsync(replyToID, &protocol.Control{
		Type:   protocol.TypeText,
		ChatID: chatID,
		Text:   &protocol.TextPayload{Delta: "🔍 正在获取可用 agent，请稍候（约半分钟）…\n"},
	})
	bridgebase.GoSafe(h.Logger, "agent-picker:"+chatID, func() {
		choice, messageID, err := h.AskAndWait(chatID, replyToID, "agent", "选择 agent", h.agent.ListAgents, true)
		if err != nil {
			h.emitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
			return
		}
		old := oldAgent
		if old == "" {
			old = "默认"
		}
		h.Router.SetAgent(chatID, choice)
		cmdutil.LogSettingChange(h.Logger, chatID, "agent", choice)
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
// is defence in depth — the workspace boundary is enforced by /cd.
//
// IsAbs only, by design: a relative path (including "..") does not begin with
// "/", so IsAbs already rejects it; a ".." segment inside an absolute path
// (e.g. "/a/../b") is resolved by the filesystem to a concrete path at
// MkdirAll/CWD time and is not a traversal escape. The workspace root boundary
// is enforced separately — /cd's validateAbsDir and bridgebase's filepath.Rel
// check both Clean before comparing. Existence is not required (unlike /cd's
// validateAbsDir) — ensureBinding creates the dir via MkdirAll on demand.
func validateSessionDirPath(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("路径必须是绝对路径：%s", dir)
	}
	return nil
}
