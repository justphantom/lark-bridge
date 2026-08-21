package config

import (
	"fmt"
	"strings"
	"text/template"
	"time"
)

// FeishuFrontConfig is the feishu-front binary's view of the union config
// document: the shared Core plus the sections only the frontend owns. Its
// owned top-level keys are exactly what LoadFeishuFront decodes — a
// miniagent or status_monitor section in the same file is skipped.
type FeishuFrontConfig struct {
	Core
	FeishuCreds

	Dedup       DedupConfig `json:"dedup,omitempty"`
	Renderer    Renderer    `json:"renderer,omitempty"`
	FileConvert FileConvert `json:"file_convert,omitempty"`
}

// FeishuCreds are the Feishu open-platform credentials. Only feishu-front
// consumes them; backends ignore. Embedded (untagged) so the fields stay
// flat at the top level of the JSON document — the wire format is unchanged
// from the old union Config (feishu_app_id & co., NOT a nested object).
type FeishuCreds struct {
	FeishuAppID     string `json:"feishu_app_id"`
	FeishuAppSecret string `json:"feishu_app_secret"`
	FeishuDomain    string `json:"feishu_domain,omitempty"`
	FeishuLogLevel  string `json:"feishu_log_level,omitempty"`
}

// applyFeishuDefaults fills the frontend-owned sections' defaults.
func applyFeishuDefaults(cfg *FeishuFrontConfig) {
	if cfg.FeishuDomain == "" {
		cfg.FeishuDomain = "feishu"
	}
	if cfg.FeishuLogLevel == "" {
		cfg.FeishuLogLevel = "info"
	}
	// FileConvert: only apply defaults when the operator has opted in
	// (Enabled). An absent / disabled section keeps the legacy "reject file
	// messages" behaviour; we do not synthesise inbox paths the operator
	// never asked for.
	if cfg.FileConvert.Enabled {
		if cfg.FileConvert.MaxFileSize <= 0 {
			cfg.FileConvert.MaxFileSize = 30 << 20 // 30 MiB
		}
		if cfg.FileConvert.ConvertTimeout == 0 {
			cfg.FileConvert.ConvertTimeout = Duration(60 * time.Second)
		}
		if cfg.FileConvert.Retention == 0 {
			cfg.FileConvert.Retention = Duration(7 * 24 * time.Hour)
		}
		// xlsx formula mode defaults to cached value (decision 6A). An
		// operator who leaves it empty gets "value"; an explicit empty is
		// indistinguishable from absent under JSON, so treat both as default.
		if cfg.FileConvert.XlsxFormulaMode == "" {
			cfg.FileConvert.XlsxFormulaMode = "value"
		}
	}
}

// validateFeishu checks the frontend-owned sections. All rules are inert on
// zero values (means "use the built-in default") so a config that omits a
// section entirely is never punished.
func validateFeishu(cfg *FeishuFrontConfig) error {
	switch cfg.FeishuLogLevel {
	case "debug", "info", "warn", "error", "":
	default:
		return fmt.Errorf("feishu_log_level must be one of debug/info/warn/error, got %q", cfg.FeishuLogLevel)
	}

	const minTunableTimeout = time.Second
	// Replay-guard ranges. Zero values are valid (means "use dispatcher default").
	if d := time.Duration(cfg.Dedup.StaleWindow); d > 0 && d < minTunableTimeout {
		return fmt.Errorf("dedup.stale_window must be >= %s when set, got %s", minTunableTimeout, d)
	}
	if d := time.Duration(cfg.Dedup.EventTTL); d > 0 && d < minTunableTimeout {
		return fmt.Errorf("dedup.event_ttl must be >= %s when set, got %s", minTunableTimeout, d)
	}
	if cfg.Dedup.EventMaxEntries < 0 {
		return fmt.Errorf("dedup.event_max_entries must be >= 0, got %d", cfg.Dedup.EventMaxEntries)
	}

	// Renderer tunables. Zero values are valid (means "use renderer default").
	if cfg.Renderer.MaxThinkingRunes < 0 {
		return fmt.Errorf("renderer.max_thinking_runes must be >= 0, got %d", cfg.Renderer.MaxThinkingRunes)
	}

	// FileConvert. Only enforce when Enabled; disabled sections are inert.
	if cfg.FileConvert.Enabled {
		if err := validateFileConvert(&cfg.FileConvert); err != nil {
			return err
		}
	}
	return nil
}

// DedupConfig configures the frontend's application-layer replay guard.
// All fields optional; zero values mean "use the dispatcher's built-in
// default" (300s stale window, 5m event TTL, 1000 entry cap), so a
// config section that omits dedup entirely behaves the same as one that
// spells out the defaults. Only consumed by feishu-front; backends ignore.
type DedupConfig struct {
	// StaleWindow drops inbound messages whose create_time is older than
	// this. Go duration string ("300s", "5m"). <=0/absent → default 300s.
	StaleWindow Duration `json:"stale_window,omitempty"`
	// EventTTL is the eventIDs dedup table's TTL. Go duration string.
	// <=0/absent → default 5m.
	EventTTL Duration `json:"event_ttl,omitempty"`
	// EventMaxEntries is the eventIDs LRU hard cap. <=0/absent → default 1000.
	EventMaxEntries int `json:"event_max_entries,omitempty"`
}

// Renderer holds feishu-front progress-card rendering tunables. All fields
// optional; zero values keep the renderer's built-in defaults. Only consumed
// by feishu-front; backends ignore.
type Renderer struct {
	// MaxThinkingRunes caps the model's live reasoning shown in the progress
	// card's "思考中" zone — the trailing runes are kept, prefixed with
	// "… （前略）" when the trace is longer. The card is a live dashboard,
	// not a reading surface, so only the tail is useful as a "what it's doing
	// right now" hint; the full trace stays in the session archive for later
	// review. <=0/absent → 100.
	MaxThinkingRunes int `json:"max_thinking_runes,omitempty"`
}

// FileConvert enables and tunes the inbound file-message pipeline in
// feishu-front. When this section is present (even with all fields at their
// defaults), the dispatcher accepts file-type Feishu messages, downloads the
// binary via the IM resources API, converts docx→md with the built-in pure
// Go parser (no external binary required), and exposes the resulting .md
// path in the prompt text so the bound backend can Read it. When the
// section is absent, file-type messages are still rejected with the
// legacy "不支持的消息类型" notice — keeping the feature opt-in per deployment.
//
// Only consumed by feishu-front; backends ignore.
type FileConvert struct {
	// Enabled is the master switch. False by default; the dispatcher keys the
	// "accept file-type messages?" decision on Enabled rather than on the
	// section's mere presence so an operator can stage config (write the
	// block with enabled:false) without flipping the feature on.
	Enabled bool `json:"enabled,omitempty"`
	// InboxDir is the root directory the dispatcher writes uploaded files
	// and converted .md into. Layout: {InboxDir}/{chatID}/{promptID}/… .
	// Empty → {state_dir}/inbox. Created on startup with 0700 perms.
	InboxDir string `json:"inbox_dir,omitempty"`
	// MaxFileSize bounds the size of one accepted upload, in bytes. An
	// upload larger than this is rejected with a notice before any download
	// attempt, so a 1 GiB accidental attachment cannot exhaust memory.
	// <=0 → 30 MiB.
	MaxFileSize int64 `json:"max_file_size,omitempty"`
	// ConvertTimeout bounds one file conversion. <=0 → 60s. A conversion
	// exceeding this is cancelled (ctx) and surfaced as a notice.
	ConvertTimeout Duration `json:"convert_timeout,omitempty"`
	// Retention bounds how long an inbox entry stays on disk after the turn
	// that produced it completed. <=0 → 7d. A background sweep at startup
	// prunes older directories so the inbox cannot grow without bound.
	Retention Duration `json:"retention,omitempty"`
	// PromptTemplate is the Go text/template rendered into the prompt body
	// the agent backend receives when a file is uploaded. Required when
	// Enabled is true; absent/empty makes Load fail so an operator can never
	// silently ship a deployment with no instruction to the agent (the
	// default template lives in config.example.json so the
	// canonical wording stays operator-editable, never compiled in).
	//
	// Variables:
	//   {{.FileName}}  original upload name (e.g. "notes.md")
	//   {{.Path}}      absolute path of the converted .md on disk
	//   {{.UserText}}  accompanying text from the upload message (""
	//                  when the user uploaded bare); use {{if .UserText}}
	//                  ... {{end}} to omit the section entirely
	PromptTemplate string `json:"prompt_template,omitempty"`
	// PostPromptTemplate renders the prompt body for a post-type (rich text)
	// Feishu message. Optional: when empty, post messages still work but
	// degrade to plain Markdown text (no image download, no body.md) so an
	// operator who wants only single-file uploads does not need to configure
	// a second template. When set, the dispatcher materialises inline images
	// into the inbox and writes body.md, naming its absolute path here.
	//
	// Variables:
	//   {{.Path}}     absolute path of body.md (the agent's read entry point)
	//   {{.UserText}} accompanying text (rarely populated for posts; post
	//                content IS the body — kept for parity with file uploads)
	PostPromptTemplate string `json:"post_prompt_template,omitempty"`
	// XlsxPromptTemplate renders the prompt body for an xlsx upload following
	// the C paradigm (office-extract-design.md §3.2): the full data body is
	// on disk, so the prompt carries only a path plus a per-sheet schema
	// summary. Optional — when empty, xlsx uploads fall back to
	// PromptTemplate (path only, no schema) so deployments without it still
	// work.
	//
	// Variables:
	//   {{.FileName}}       original upload name (e.g. "data.xlsx")
	//   {{.Path}}           absolute path of the converted .md
	//   {{.SheetCount}}     number of sheets
	//   {{.SheetsSection}}  pre-rendered per-sheet list (column names + row
	//                       counts + caveats); one bullet per sheet
	//   {{.UserText}}       accompanying text ("")
	XlsxPromptTemplate string `json:"xlsx_prompt_template,omitempty"`

	// —— pptx 提取调优（office-extract-design.md §2 / §4.1）——
	// PptxMaxSlides caps the number of slides emitted. <=0 → unlimited, which
	// is the design default (decision Q1: extract every slide). An operator
	// with a tight LLM context budget can lower it to truncate long decks.
	PptxMaxSlides int `json:"pptx_max_slides,omitempty"`
	// PptxExtractNotes reserves a switch to also emit speaker notes
	// (ppt/notesSlides). v1 ignores it and emits slides only; the field is
	// kept so future enablement is a config flip, not a schema change.
	PptxExtractNotes bool `json:"pptx_extract_notes,omitempty"`
	// PptxTextOnly forces text-only extraction: images are dropped entirely
	// (decision 3A — no download, no reference, no placeholder). v1 always
	// behaves as text-only regardless of this value; the switch is the
	// forward hook for the day image extraction is added.
	PptxTextOnly bool `json:"pptx_text_only,omitempty"`

	// —— xlsx 提取调优（C 范式：数据本体全量落盘，prompt 仅元数据）——
	// XlsxMaxSheets caps the number of sheets emitted. <=0 → unlimited
	// (decision Q10: every sheet). The full data body is always written to
	// the inbox .md; this only bounds how many sheets survive that write.
	XlsxMaxSheets int `json:"xlsx_max_sheets,omitempty"`
	// XlsxFormulaMode selects what an xlsx cell carrying a formula yields:
	//   "value"    → the cached result (default, decision 6A)
	//   "formula"  → the formula text
	//   "both"     → "cached (formula)"
	// An empty value is normalised to "value" by applyDefaults and rejected
	// by validate if it falls outside the enum.
	XlsxFormulaMode string `json:"xlsx_formula_mode,omitempty"`
}

// validateFileConvert enforces the enabled-file_convert rules. Only called
// when Enabled is true; a disabled section is inert.
func validateFileConvert(fc *FileConvert) error {
	if fc.MaxFileSize < 1<<20 {
		return fmt.Errorf("file_convert.max_file_size must be >= 1MiB, got %d bytes", fc.MaxFileSize)
	}
	if d := time.Duration(fc.ConvertTimeout); d > 0 && d < time.Second {
		return fmt.Errorf("file_convert.convert_timeout must be >= %s when set, got %s", time.Second, d)
	}
	if d := time.Duration(fc.Retention); d > 0 && d < time.Hour {
		return fmt.Errorf("file_convert.retention must be >= 1h when set, got %s", d)
	}
	// xlsx_formula_mode must be one of the supported semantics. The
	// default ("value") is filled by applyDefaults, so reaching validate
	// with an empty value means an operator explicitly cleared it.
	switch fc.XlsxFormulaMode {
	case "value", "formula", "both":
	default:
		return fmt.Errorf("file_convert.xlsx_formula_mode must be one of value/formula/both, got %q", fc.XlsxFormulaMode)
	}
	// pptx/xlsx caps: only reject negative limits (0 means unlimited,
	// which is the design default, so 0 is always valid).
	if fc.PptxMaxSlides < 0 {
		return fmt.Errorf("file_convert.pptx_max_slides must be >= 0 (0 = unlimited), got %d", fc.PptxMaxSlides)
	}
	if fc.XlsxMaxSheets < 0 {
		return fmt.Errorf("file_convert.xlsx_max_sheets must be >= 0 (0 = unlimited), got %d", fc.XlsxMaxSheets)
	}
	// PromptTemplate is required: no compiled-in default exists (the
	// canonical wording ships in config.example.json
	// so operators can edit it). Refuse to start so a half-configured
	// deployment cannot ship silent file uploads.
	if strings.TrimSpace(fc.PromptTemplate) == "" {
		return fmt.Errorf("file_convert.prompt_template is required when file_convert.enabled is true (copy the default from config.example.json)")
	}
	// Syntax-check at config load so a typo'd template fails fast at
	// startup, not on the first upload. Variable substitution happens at
	// render time; here we only assert the template parses.
	if err := validatePromptTemplate(fc.PromptTemplate); err != nil {
		return fmt.Errorf("file_convert.prompt_template: %w", err)
	}
	// PostPromptTemplate is optional: when empty, post messages degrade
	// to text-only Markdown (no image download). When non-empty, parse
	// it once here so a typo fails fast at startup, not on the first
	// post.
	if strings.TrimSpace(fc.PostPromptTemplate) != "" {
		if err := validatePromptTemplate(fc.PostPromptTemplate); err != nil {
			return fmt.Errorf("file_convert.post_prompt_template: %w", err)
		}
	}
	// XlsxPromptTemplate is optional (empty → fall back to the generic
	// prompt_template). When set, parse-check it now so a typo fails at
	// startup, not on the first xlsx upload.
	if strings.TrimSpace(fc.XlsxPromptTemplate) != "" {
		if err := validatePromptTemplate(fc.XlsxPromptTemplate); err != nil {
			return fmt.Errorf("file_convert.xlsx_prompt_template: %w", err)
		}
	}
	return nil
}

// promptTemplateFuncs whitelists the template functions exposed to
// file_convert.prompt_template. Kept empty for now; declaring the map
// explicitly means a future addition (e.g. a `truncate` filter) is a
// single-point edit, and the validation parse uses the same surface the
// runtime renderer will use.
var promptTemplateFuncs = template.FuncMap{}

// PromptTemplateFuncs returns the FuncMap used when parsing and rendering
// file_convert.prompt_template. Declared as a public accessor (rather than
// letting callers redeclare the map) so the validation parse in config and
// the runtime parse in cmd/feishu-front cannot drift apart.
func PromptTemplateFuncs() template.FuncMap {
	return promptTemplateFuncs
}

// validatePromptTemplate parses t with the same FuncMap the runtime renderer
// will use, without executing it. Returns the parse error verbatim so the
// operator sees the exact line/column. Called once at config Load time so a
// broken template fails fast at startup instead of on the first upload.
func validatePromptTemplate(t string) error {
	if _, err := template.New("file_convert.prompt_template").Funcs(promptTemplateFuncs).Parse(t); err != nil {
		return err
	}
	return nil
}
