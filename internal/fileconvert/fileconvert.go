// Package fileconvert materialises a user-uploaded file into a Markdown file
// the agent backend can Read. The bridge's frontend receives file-type Feishu
// messages, downloads the binary via lark.Client.DownloadResource, and hands
// the bytes to this package to produce a .md path under a per-chat inbox.
//
// Supported source types:
//   - .docx — converted to GitHub-flavoured Markdown in-process by the pure
//     Go parser in convert_docx.go (archive/zip + encoding/xml; L1+ scope:
//     headings, inline emphasis, lists, tables, hyperlinks, footnotes).
//   - .pptx — converted by the pure Go extractor in convert_pptx.go.
//   - .xlsx — converted via ConvertXlsx (pure Go parser in convert_xlsx.go;
//     C-paradigm metadata return, office-extract-design.md §3.2).
//   - .md / .markdown — copied verbatim.
//   - .txt — copied verbatim with a .md extension so the agent treats it as
//     Markdown when reading.
//
// Anything else returns ErrUnsupported so the dispatcher can surface a
// friendly notice rather than silently dropping the upload.
package fileconvert

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ConvertTimeout bounds a single conversion. Conversions are pure in-process
// work, but a hostile document (deep nesting, millions of empty paragraphs)
// could still pin the dispatcher goroutine without a budget; the caller
// (dispatcher) wraps the call in its own ctx with FileConvert.ConvertTimeout,
// this constant is the fallback used when the caller does not configure one.
const ConvertTimeout = 60 * time.Second

// ErrUnsupported signals that the source extension is not handled. The
// dispatcher turns this into a "暂不支持该文件类型" notice rather than a hard
// error so an unsupported upload does not poison the chat's turn manager.
var ErrUnsupported = errors.New("fileconvert: unsupported source type")

// Options configures one Converter for its lifetime.
type Options struct {
	// Timeout bounds each conversion. <=0 → ConvertTimeout. Set from
	// config.file_convert.convert_timeout.
	Timeout time.Duration
	// Logger receives debug lines on each conversion (skip/copy/convert).
	// nil → no logging.
	Logger logger

	// —— pptx (office-extract-design.md §2 / §4.1) ——
	// PptxMaxSlides caps emitted slides; <=0 → unlimited (design default).
	PptxMaxSlides int
	// PptxExtractNotes reserves speaker-notes emission; v1 ignores it
	// (slides only). Kept so future enablement is config-only.
	PptxExtractNotes bool
	// PptxTextOnly forces text-only extraction. v1 always behaves as
	// text-only (images dropped per decision 3A); the switch is the forward
	// hook for image extraction and currently has no behavioural effect.
	PptxTextOnly bool

	// —— xlsx (C paradigm: full data to disk, prompt carries metadata) ——
	// XlsxMaxSheets caps emitted sheets; <=0 → unlimited (decision Q10).
	XlsxMaxSheets int
	// XlsxFormulaMode selects value/formula/both for formula cells;
	// empty → "value" (decision 6A). Validated by config to the enum.
	XlsxFormulaMode string
}

// logger is the minimal surface we use; declared locally to avoid importing
// internal/log here (keeps the package leaf-level, easier to test in
// isolation). slog.Logger satisfies it via an adapter in the caller.
type logger interface {
	Debug(msg string, args ...any)
}

// Converter turns one source file (already on disk) into a .md sibling.
// Safe for concurrent use: each Convert writes to a caller-supplied
// destination path; no shared mutable state.
type Converter struct {
	timeout          time.Duration
	log              logger
	pptxMaxSlides    int
	pptxExtractNotes bool
	pptxTextOnly     bool
	xlsxMaxSheets    int
	xlsxFormulaMode  string
}

// New builds a Converter from opts.
func New(opts Options) *Converter {
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = ConvertTimeout
	}
	formulaMode := opts.XlsxFormulaMode
	if formulaMode == "" {
		formulaMode = "value" // decision 6A default
	}
	c := &Converter{
		timeout:          timeout,
		log:              opts.Logger,
		pptxMaxSlides:    opts.PptxMaxSlides,
		pptxExtractNotes: opts.PptxExtractNotes,
		pptxTextOnly:     opts.PptxTextOnly,
		xlsxMaxSheets:    opts.XlsxMaxSheets,
		xlsxFormulaMode:  formulaMode,
	}
	return c
}

// Convert reads srcPath and writes its Markdown rendering to dstPath.
// dstPath must end in ".md". The source file is left in place; the caller
// (inbox manager) is responsible for the full inbox lifecycle.
//
// Returns ErrUnsupported when the source extension is not in the supported
// set. Other failures (parse/copy IO, timeout) return the underlying error
// with enough context to surface to the user.
func (c *Converter) Convert(ctx context.Context, srcPath, dstPath string) error {
	if !strings.HasSuffix(dstPath, ".md") {
		return fmt.Errorf("fileconvert: dst must end in .md, got %q", dstPath)
	}
	ext := strings.ToLower(filepath.Ext(srcPath))
	switch ext {
	case ".docx":
		return c.convertDocx(ctx, srcPath, dstPath)
	case ".pptx":
		return c.convertPptx(ctx, srcPath, dstPath)
	case ".md", ".markdown", ".txt":
		return copyFile(srcPath, dstPath)
	case ".xlsx":
		// xlsx follows the C paradigm (office-extract-design.md §3.2):
		// the full data body goes to dstPath AND sheet metadata must flow
		// back to the dispatcher to build a path+schema+rows-only prompt.
		// Convert returns only an error, so it cannot carry that metadata;
		// force callers through ConvertXlsx instead of silently dropping it.
		return fmt.Errorf("fileconvert: xlsx must go through ConvertXlsx (C-paradigm metadata return), got %s", filepath.Base(srcPath))
	default:
		return fmt.Errorf("%w: %s", ErrUnsupported, ext)
	}
}

// copyFile duplicates src into dst verbatim. Used for .md/.markdown/.txt
// where parsing would add nothing but latency. dst is created with 0600 via
// os.OpenFile so an inbox never leaks to other local users even if the inbox
// dir perms were misconfigured.
func copyFile(srcPath, dstPath string) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("fileconvert: open src: %w", err)
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dstPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("fileconvert: create dst: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(dstPath)
		return fmt.Errorf("fileconvert: copy: %w", err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dstPath)
		return fmt.Errorf("fileconvert: close dst: %w", err)
	}
	return nil
}
