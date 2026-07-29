package bridgebase

import (
	"context"
	"strings"

	"github.com/justphantom/lark-bridge/internal/cmdutil"
)

// EnumPickerConfig describes a "pin / clear / pick one value from a list"
// slash command. /effort (claude) and /thinking (omp) are byte-identical
// modulo field name, option list, and copy — this helper factors the shared
// shape into a single CommandSpec factory.
//
// The handler asks the bridge to (a) read the current pin, (b) write a new
// pin, via the Getter/Setter callbacks. The Setter is responsible for any
// router log line (cmdutil.LogSettingChange) — see the bridges' /effort and
// /thinking call sites for the canonical pattern.
type EnumPickerConfig struct {
	// Spec is the cmdutil.Spec for the command (Name/Summary/Args/Title/Level).
	Spec cmdutil.Spec
	// FieldLabel is the human label shown on the result card ("推理级别" /
	// "思考级别"). Also used in error messages.
	FieldLabel string
	// LogKey is the structured-log key for cmdutil.LogSettingChange
	// (e.g. "effort_level", "thinking_level").
	LogKey string
	// Options feeds the picker card. allowCustom is always false: enum
	// pickers restrict selection to listed values.
	Options []string
	// Default is the value shown when the current pin is empty (display only).
	Default string
	// ErrorHint is appended to the "unknown value" error (e.g.
	// "可选 low | medium | high | xhigh | max").
	ErrorHint string
	// Valid reports whether v is an acceptable direct pin value.
	Valid func(v string) bool
}

// EnumPickerAccessors binds the picker to a backend's binding type: it reads
// the current pin and writes a new one (with the backend's own logging). The
// chatID is passed to Setter so it can call Router.SetEffortLevel etc. and
// emit the structured-log line via cmdutil.LogSettingChange.
type EnumPickerAccessors[H any] struct {
	// Ensure creates the chat binding if it does not exist (so the Set
	// accessor's router mutation lands). Backends wire this to their
	// ensureBinding helper. May be nil if the backend's Set is happy with a
	// missing binding (rare).
	Ensure func(h H, chatID string) error
	// Get reads the current pin from the handler's bound chat.
	Get func(h H, chatID string) string
	// Set writes a new pin (or "" to clear). Responsible for both the router
	// mutation AND the cmdutil.LogSettingChange log line.
	Set func(h H, chatID, v string)
}

// MakeEnumPicker builds a CommandSpec for a /effort-style picker. The result
// is byte-equivalent to what each backend used to spell inline (~70 lines);
// centralising it lets a future enum picker (e.g. a /smol command) drop in
// with a config struct.
func MakeEnumPicker[H any](c *Core, cfg EnumPickerConfig, acc EnumPickerAccessors[H]) CommandSpec[H] {
	return CommandSpec[H]{
		Spec: cfg.Spec,
		Handler: func(h H, ctx context.Context, chatID string, args []string) (cmdutil.Result, error) {
			if acc.Ensure != nil {
				if err := acc.Ensure(h, chatID); err != nil {
					return cmdutil.Result{Body: err.Error()}, err
				}
			}
			old := acc.Get(h, chatID)
			oldDisplay := old
			if oldDisplay == "" {
				oldDisplay = "默认 (" + cfg.Default + ")"
			}

			if len(args) == 0 {
				return runEnumPicker[H](c, h, chatID, ReplyToID(ctx), cfg, acc, oldDisplay), nil
			}
			if args[0] == "clear" {
				return clearEnum[H](h, chatID, cfg, acc, oldDisplay), nil
			}

			val := strings.Join(args, " ")
			if cfg.Valid != nil && !cfg.Valid(val) {
				return cmdutil.ErrorResult("未知%s %q；%s", cfg.FieldLabel, val, cfg.ErrorHint)
			}
			acc.Set(h, chatID, val)
			return cmdutil.ChangeResult(cfg.FieldLabel, oldDisplay, val, "下次提问生效。"), nil
		},
	}
}

// runEnumPicker drives the interactive selection. allowCustom=false so the
// picker restricts selection to the configured options.
func runEnumPicker[H any](c *Core, h H, chatID, replyToID string, cfg EnumPickerConfig, acc EnumPickerAccessors[H], oldDisplay string) cmdutil.Result {
	choice, messageID, err := c.AskAndWait(chatID, replyToID, cfg.FieldLabel, "选择"+cfg.FieldLabel, StaticOptions(cfg.Options), false)
	if err != nil {
		c.EmitPromptNotice(chatID, replyToID, "error", "选择失败", err.Error())
		return cmdutil.Result{Body: err.Error(), Handled: true}
	}
	acc.Set(h, chatID, choice)
	res := cmdutil.ChangeResult(cfg.FieldLabel, oldDisplay, choice, "下次提问生效。")
	c.EmitCardUpdateLogged(chatID, messageID, "success", "已设置"+cfg.FieldLabel, res.Body, res.Field, res.Before, res.After)
	return cmdutil.Result{Handled: true}
}

// clearEnum is the clear path.
func clearEnum[H any](h H, chatID string, cfg EnumPickerConfig, acc EnumPickerAccessors[H], oldDisplay string) cmdutil.Result {
	acc.Set(h, chatID, "")
	return cmdutil.ChangeResult(cfg.FieldLabel, oldDisplay, "默认 ("+cfg.Default+")",
		"已清除"+cfg.FieldLabel+"设置，将使用默认级别。")
}
