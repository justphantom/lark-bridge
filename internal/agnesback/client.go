// Package agnesback implements the lark-agnes-back backend: it turns Agnes AI's
// image/video generation APIs into Feishu slash commands.
//
// Unlike the CLI backends (claude/miniagent), agnes-back does NOT fork a
// subprocess. It calls the Agnes HTTP API directly via net/http (the project is
// zero-external-dependency). Four commands are exposed:
//
//   - /image-prompt <描述>: uses the chat model (agnes-2.5-flash) to expand a
//     terse description into a full image prompt (text reply).
//   - /image <提示词>: generates an image via agnes-image-2.1-flash. The
//     returned image URL is downloaded and delivered to the chat as a TypeFile
//     control (base64), so the image lands inline in the Feishu chat.
//   - /video-prompt <描述>: uses the chat model to expand a description into a
//     full video prompt (text reply).
//   - /video <提示词>: creates a video task via agnes-video-v2.0 and polls until
//     completion; the final video URL is delivered as a Notice.
//
// The handler mirrors deploy-monitor's shape: register over SSE, dispatch
// commands on the event goroutine, run the (slow) API work on GoSafe
// goroutines, and emit terminal Notice/Result/File controls bound to the
// originating promptID + cardMessageID.
package agnesback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/justphantom/lark-bridge/internal/log"
)

// DefaultBaseURL is the Agnes API origin. Overridable via config base_url.
const DefaultBaseURL = "https://api.agnes-ai.cn"

// apiKeyHeader is injected by tests to assert the bearer token is set.
const apiKeyHeader = "X-Test-Api-Key"

// HTTPClient is the subset of *http.Client the client needs, lifted to an
// interface so tests inject a stub that answers canned JSON without TLS.
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// ClientConfig carries the Agnes API settings resolved from config (defaults
// already applied). All fields are required non-empty except HTTPClient (which
// defaults to a reasonable *http.Client when nil).
type ClientConfig struct {
	BaseURL    string
	APIKey     string
	ChatModel  string
	ImageModel string
	VideoModel string

	// Image generation defaults.
	ImageSize  string // "1K"/"2K"/"3K"/"4K"
	ImageRatio string // "1:1"/"16:9"/...

	// ChatModels/ImageModels/VideoModels feed the /model picker card: the
	// selectable options per slot. config.applyDefaults guarantees each list
	// holds at least the configured default model.
	ChatModels  []string
	ImageModels []string
	VideoModels []string

	// Video polling cadence (package-level defaults override when zero).
	VideoPollInterval time.Duration
	VideoPollTimeout  time.Duration

	// ImageMaxBytes caps how much of a generated image URL the client downloads
	// before failing. The Feishu IM file cap is 30 MiB; default guards a
	// pathological oversized response. 0 → DefaultImageMaxBytes.
	ImageMaxBytes int64

	// HTTPClient, when nil, resolves to a standard *http.Client with a 5-minute
	// timeout (image/video generation can take tens of seconds).
	HTTPClient HTTPClient
}

// DefaultImageMaxBytes caps a downloaded image at 30 MiB (the Feishu IM file
// upload ceiling). A larger generated image is delivered as a URL notice
// instead of a base64 file.
const DefaultImageMaxBytes = 30 << 20

// defaultHTTPTimeout bounds a single Agnes API HTTP call. Image generation can
// take tens of seconds; video task creation is fast but the final result poll
// is governed by VideoPollTimeout, not this.
const defaultHTTPTimeout = 5 * time.Minute

// Client wraps the Agnes API.
type Client struct {
	cfg    ClientConfig
	http   HTTPClient
	logger *log.Logger
}

// New builds a Client. baseURL must be an origin with no trailing slash; the
// client appends "/v1/..." paths to it. logger is optional (defaults to Nop).
func New(cfg ClientConfig, logger *log.Logger) *Client {
	if logger == nil {
		logger = log.Nop()
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	if cfg.ImageMaxBytes <= 0 {
		cfg.ImageMaxBytes = DefaultImageMaxBytes
	}
	if cfg.VideoPollInterval <= 0 {
		cfg.VideoPollInterval = DefaultVideoPollInterval
	}
	if cfg.VideoPollTimeout <= 0 {
		cfg.VideoPollTimeout = DefaultVideoPollTimeout
	}
	return &Client{cfg: cfg, http: cfg.HTTPClient, logger: logger}
}

// Model slots accepted by the /model command flow.
const (
	ModelSlotChat  = "chat"
	ModelSlotImage = "image"
	ModelSlotVideo = "video"
)

// ValidModelSlot reports whether slot is one of chat/image/video.
func ValidModelSlot(slot string) bool {
	return slot == ModelSlotChat || slot == ModelSlotImage || slot == ModelSlotVideo
}

// DefaultVideoPollInterval / DefaultVideoPollTimeout bound the async video
// task poll loop. 10s cadence is gentle on the API; 5m matches typical video
// generation latency (the doc's recommended budget).
const (
	DefaultVideoPollInterval = 10 * time.Second
	DefaultVideoPollTimeout  = 5 * time.Minute
)

// --- chat completions (prompt generation) ---

// chatMessage mirrors one OpenAI-compatible chat message.
type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// MaxTokens is modest for prompt-expansion: we want a single concise
	// prompt, not an essay. The system prompt already caps the output.
	MaxTokens int     `json:"max_tokens,omitempty"`
	Temp      float64 `json:"temperature,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
	// Error mirrors the Responses API's {error: ...}; chat completions surface
	// errors as HTTP status, but some gateways wrap them here too.
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// GeneratePrompt calls the chat model to expand userText under systemPrompt,
// returning the generated prompt text (trimmed).
func (c *Client) GeneratePrompt(ctx context.Context, systemPrompt, userText string) (string, error) {
	c.logger.Info("agnes: generate prompt request",
		"model", c.cfg.ChatModel,
		"user_text", truncateString(userText, 100))
	body, err := json.Marshal(chatRequest{
		Model: c.cfg.ChatModel,
		Messages: []chatMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userText},
		},
		MaxTokens: 256,
		Temp:      0.7,
	})
	if err != nil {
		return "", fmt.Errorf("agnes: marshal chat request: %w", err)
	}

	respBody, err := c.postJSON(ctx, "/v1/chat/completions", body)
	if err != nil {
		return "", err
	}
	var resp chatResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return "", fmt.Errorf("agnes: decode chat response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		c.logger.Error("agnes: generate prompt failed", "error", resp.Error.Message)
		return "", fmt.Errorf("agnes: %s", resp.Error.Message)
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("agnes: chat returned no choices")
	}
	result := strings.TrimSpace(resp.Choices[0].Message.Content)
	c.logger.Info("agnes: generate prompt success",
		"result", truncateString(result, 200))
	return result, nil
}

// --- image generation ---

type imageRequest struct {
	Model        string         `json:"model"`
	Prompt       string         `json:"prompt"`
	Size         string         `json:"size"`
	Ratio        string         `json:"ratio,omitempty"`
	ExtraBody    imageExtraBody `json:"extra_body,omitempty"`
	ReturnBase64 bool           `json:"return_base64,omitempty"`
	// raw is used to inject extra_body for img2img; not exported on the wire.
}

type imageExtraBody struct {
	ResponseFormat string   `json:"response_format,omitempty"`
	Image          []string `json:"image,omitempty"`
}

type imageResponse struct {
	Created int `json:"created"`
	Data    []struct {
		URL           string `json:"url"`
		B64JSON       string `json:"b64_json"`
		RevisedPrompt string `json:"revised_prompt"`
	} `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// GenerateImage generates one image from prompt and returns its bytes (PNG/JPEG
// decoded from the URL Agnes returns). Only text-to-image is supported; the
// img2img `image` input array is left for a future extension.
func (c *Client) GenerateImage(ctx context.Context, prompt string) ([]byte, string, error) {
	c.logger.Info("agnes: generate image request",
		"model", c.cfg.ImageModel,
		"size", c.cfg.ImageSize,
		"ratio", c.cfg.ImageRatio,
		"prompt", truncateString(prompt, 100))
	body, err := json.Marshal(imageRequest{
		Model:  c.cfg.ImageModel,
		Prompt: prompt,
		Size:   c.cfg.ImageSize,
		Ratio:  c.cfg.ImageRatio,
		// The doc's headline use case is URL output; we download it server-side
		// and deliver via TypeFile so the image is inline in the chat.
		ExtraBody: imageExtraBody{ResponseFormat: "url"},
	})
	if err != nil {
		return nil, "", fmt.Errorf("agnes: marshal image request: %w", err)
	}

	respBody, err := c.postJSON(ctx, "/v1/images/generations", body)
	if err != nil {
		return nil, "", err
	}
	var resp imageResponse
	if err := json.Unmarshal(respBody, &resp); err != nil {
		return nil, "", fmt.Errorf("agnes: decode image response: %w", err)
	}
	if resp.Error != nil && resp.Error.Message != "" {
		c.logger.Error("agnes: generate image failed", "error", resp.Error.Message)
		return nil, "", fmt.Errorf("agnes: %s", resp.Error.Message)
	}
	if len(resp.Data) == 0 {
		return nil, "", fmt.Errorf("agnes: image returned no data")
	}
	d := resp.Data[0]
	if d.URL == "" {
		return nil, "", fmt.Errorf("agnes: image response missing url")
	}
	c.logger.Info("agnes: generate image response received",
		"url", d.URL,
		"revised_prompt", truncateString(d.RevisedPrompt, 100))

	// Download the image bytes, bounded by ImageMaxBytes so an oversized
	// response cannot exhaust memory.
	data, err := c.download(ctx, d.URL, "image", c.cfg.ImageMaxBytes)
	if err != nil {
		return nil, "", err
	}
	c.logger.Info("agnes: image downloaded",
		"bytes", len(data),
		"mime", mimeFromURL(d.URL))
	return data, mimeFromURL(d.URL), nil
}

// --- video generation (async) ---

type videoCreateRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	// Agnes Video recommends 1152x768 @ 121 frames / 24 fps (~5s). Width/
	// Height are intentionally NOT user-tunable from the slash command: the
	// doc warns num_frames must follow the 8n+1 rule and the service
	// normalizes unsupported sizes, so exposing them invites footguns.
	Width     int `json:"width,omitempty"`
	Height    int `json:"height,omitempty"`
	NumFrames int `json:"num_frames,omitempty"`
	FrameRate int `json:"frame_rate,omitempty"`
}

type videoCreateResponse struct {
	ID       string `json:"id"`
	TaskID   string `json:"task_id"`
	VideoID  string `json:"video_id"`
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// videoResultResponse decodes GET /agnesapi?video_id=. The docs place the final
// URL at metadata.url, but the live API returns it at the TOP-LEVEL url field
// with metadata as an empty object. We read both and prefer whichever is set,
// so a docs/API drift on either side does not lose the URL.
type videoResultResponse struct {
	Status   string `json:"status"`
	Progress int    `json:"progress"`
	// URL is the top-level video URL the live API actually populates on
	// completion. Prefer this; fall back to metadata.url below.
	URL      string `json:"url,omitempty"`
	Metadata struct {
		URL string `json:"url"`
	} `json:"metadata"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// GenerateVideo creates a video task and polls until it completes (or the poll
// timeout elapses). It returns the final video URL. pollCB (optional) is
// invoked after each poll with the latest status/progress so the handler can
// surface progress on the progress card.
func (c *Client) GenerateVideo(ctx context.Context, prompt string, pollCB func(status string, progress int)) (string, error) {
	c.logger.Info("agnes: generate video request",
		"model", c.cfg.VideoModel,
		"width", DefaultVideoWidth,
		"height", DefaultVideoHeight,
		"num_frames", DefaultVideoNumFrames,
		"frame_rate", DefaultVideoFrameRate,
		"prompt", truncateString(prompt, 100))
	body, err := json.Marshal(videoCreateRequest{
		Model:     c.cfg.VideoModel,
		Prompt:    prompt,
		Width:     DefaultVideoWidth,
		Height:    DefaultVideoHeight,
		NumFrames: DefaultVideoNumFrames,
		FrameRate: DefaultVideoFrameRate,
	})
	if err != nil {
		return "", fmt.Errorf("agnes: marshal video request: %w", err)
	}

	respBody, err := c.postJSON(ctx, "/v1/videos", body)
	if err != nil {
		return "", err
	}
	var created videoCreateResponse
	if err := json.Unmarshal(respBody, &created); err != nil {
		return "", fmt.Errorf("agnes: decode video create response: %w", err)
	}
	if created.Error != nil && created.Error.Message != "" {
		c.logger.Error("agnes: video create failed", "error", created.Error.Message)
		return "", fmt.Errorf("agnes: %s", created.Error.Message)
	}
	// The doc says new integrations should use video_id; fall back to task_id.
	// The recommended GET path /agnesapi?video_id=... is used below.
	vid := created.VideoID
	if vid == "" {
		vid = created.TaskID
	}
	if vid == "" {
		// id/task_id/video_id may all be the same; last-ditch id.
		vid = created.ID
	}
	if vid == "" {
		return "", fmt.Errorf("agnes: video create returned no id")
	}
	c.logger.Info("agnes: video task created",
		"video_id", truncateString(vid, 32),
		"task_id", created.TaskID,
		"status", created.Status,
		"progress", created.Progress)

	if pollCB != nil {
		pollCB(created.Status, created.Progress)
	}
	return c.pollVideo(ctx, vid, pollCB)
}

// DefaultVideo* are the recommended standard generation parameters from the
// Agnes Video V2.0 doc.
const (
	DefaultVideoWidth     = 1152
	DefaultVideoHeight    = 768
	DefaultVideoNumFrames = 121 // 8*15+1, ~5s at 24fps
	DefaultVideoFrameRate = 24
)

// DefaultVideoMaxBytes caps a downloaded video at 30 MiB, matching the Feishu
// IM file upload limit (the TypeFile pipeline ships the bytes through to
// Feishu's im/v1/files, which rejects anything larger).
const DefaultVideoMaxBytes = 30 << 20

// DownloadVideo fetches the video bytes at url, capped at DefaultVideoMaxBytes.
// The handler uses this to ship the video as a TypeFile Control so it lands
// inline in the Feishu chat; a size/network failure surfaces as an error the
// handler falls back to a URL notice for.
func (c *Client) DownloadVideo(ctx context.Context, url string) ([]byte, error) {
	return c.download(ctx, url, "video", DefaultVideoMaxBytes)
}

// maxConsecutivePollErrors is the number of consecutive queryVideo failures
// pollVideo tolerates before giving up. At the default 10s poll interval this
// is a ~30s tolerance window for transient network blips or API 5xx hiccups
// — without it, a single flaky query would abort a task that would have
// succeeded on the next poll.
const maxConsecutivePollErrors = 3

// pollVideo polls GET /agnesapi?video_id=... until the task is completed or the
// poll timeout (derived from ctx + VideoPollTimeout) elapses. Transient query
// errors (network blips, API 5xx) are tolerated up to maxConsecutivePollErrors
// consecutive failures before the task is abandoned.
func (c *Client) pollVideo(ctx context.Context, vid string, pollCB func(string, int)) (string, error) {
	c.logger.Info("agnes: start polling video",
		"video_id", truncateString(vid, 32),
		"poll_interval", c.cfg.VideoPollInterval,
		"poll_timeout", c.cfg.VideoPollTimeout)
	pollCtx, cancel := context.WithTimeout(ctx, c.cfg.VideoPollTimeout)
	defer cancel()

	ticker := time.NewTicker(c.cfg.VideoPollInterval)
	defer ticker.Stop()

	pollCount := 0
	consecutiveErrs := 0
	// The create response may already be terminal on rare fast paths; the loop
	// polls once immediately, then on each tick.
	for {
		status, url, progress, err := c.queryVideo(pollCtx, vid)
		if err != nil {
			consecutiveErrs++
			c.logger.Warn("agnes: video poll query failed",
				"video_id", truncateString(vid, 32),
				"poll_count", pollCount,
				"consecutive_errors", consecutiveErrs,
				"max", maxConsecutivePollErrors,
				"error", err)
			if consecutiveErrs >= maxConsecutivePollErrors {
				return "", fmt.Errorf("agnes: video poll failed after %d consecutive errors: %w", consecutiveErrs, err)
			}
			// Transient error: wait for the next tick, then retry.
			select {
			case <-pollCtx.Done():
				return "", fmt.Errorf("agnes: video poll timed out (last status: querying)")
			case <-ticker.C:
			}
			continue
		}
		consecutiveErrs = 0 // success resets the counter
		pollCount++
		c.logger.Debug("agnes: video poll update",
			"video_id", truncateString(vid, 32),
			"poll_count", pollCount,
			"status", status,
			"progress", progress,
			"url", url != "")
		if pollCB != nil {
			pollCB(status, progress)
		}
		switch status {
		case "completed":
			if url == "" {
				return "", fmt.Errorf("agnes: video completed but no url")
			}
			c.logger.Info("agnes: video completed",
				"video_id", truncateString(vid, 32),
				"poll_count", pollCount,
				"progress", progress,
				"url", url)
			return url, nil
		case "failed":
			c.logger.Error("agnes: video generation failed",
				"video_id", truncateString(vid, 32),
				"poll_count", pollCount)
			return "", fmt.Errorf("agnes: video generation failed")
		}
		select {
		case <-pollCtx.Done():
			c.logger.Warn("agnes: video poll timed out",
				"video_id", truncateString(vid, 32),
				"poll_count", pollCount,
				"last_status", status,
				"last_progress", progress)
			return "", fmt.Errorf("agnes: video poll timed out (last status %s)", status)
		case <-ticker.C:
		}
	}
}

// queryVideo runs one GET /agnesapi?video_id=<vid>.
func (c *Client) queryVideo(ctx context.Context, vid string) (status, url string, progress int, err error) {
	u := c.cfg.BaseURL + "/agnesapi?video_id=" + vid
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", 0, fmt.Errorf("agnes: build video query: %w", err)
	}
	c.setAuth(req)
	resp, err := c.http.Do(req) //nolint:bodyclose // closed via defer drainAndClose below
	if err != nil {
		c.logger.Warn("agnes: video query request failed",
			"video_id", truncateString(vid, 32),
			"error", err)
		return "", "", 0, fmt.Errorf("agnes: video query: %w", err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.logger.Warn("agnes: video query non-200",
			"video_id", truncateString(vid, 32),
			"status_code", resp.StatusCode)
		return "", "", 0, c.httpError("video query", resp)
	}
	var out videoResultResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", 0, fmt.Errorf("agnes: decode video result: %w", err)
	}
	if out.Error != nil && out.Error.Message != "" {
		c.logger.Error("agnes: video query returned error",
			"video_id", truncateString(vid, 32),
			"error", out.Error.Message)
		return "", "", 0, fmt.Errorf("agnes: %s", out.Error.Message)
	}
	// Prefer metadata.url (per docs), fall back to the top-level url field
	// (what the live API actually returns).
	url = out.Metadata.URL
	if url == "" {
		url = out.URL
	}
	// Log the complete response for diagnostics, especially when status=completed but url is empty.
	if out.Status == "completed" && url == "" {
		c.logger.Warn("agnes: video completed but no url",
			"video_id", vid,
			"status", out.Status,
			"progress", out.Progress,
			"top_level_url", out.URL,
			"metadata_url", out.Metadata.URL,
			"raw_error", out.Error)
	}
	return out.Status, url, out.Progress, nil
}

// --- HTTP plumbing ---

// postJSON POSTs body to path (relative to BaseURL) with the bearer token and
// returns the response body bytes. Non-2xx is an error carrying the response
// snippet for diagnostics.
func (c *Client) postJSON(ctx context.Context, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("agnes: build %s request: %w", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	c.setAuth(req)
	c.logger.Debug("agnes: post request",
		"path", path,
		"body_bytes", len(body),
		"body_preview", truncateString(string(body), 200))
	resp, err := c.http.Do(req) //nolint:bodyclose // closed via defer drainAndClose below
	if err != nil {
		c.logger.Warn("agnes: post request failed",
			"path", path,
			"error", err)
		return nil, fmt.Errorf("agnes: %s: %w", path, err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.logger.Warn("agnes: post request non-200",
			"path", path,
			"status_code", resp.StatusCode)
		return nil, c.httpError(path, resp)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("agnes: read %s response: %w", path, err)
	}
	c.logger.Debug("agnes: post response",
		"path", path,
		"status_code", resp.StatusCode,
		"response_bytes", len(out),
		"response_preview", truncateString(string(out), 400))
	return out, nil
}

// setAuth stamps the Authorization: Bearer header (and a test-injected marker).
func (c *Client) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	// Test hook: lets a stub HTTPClient assert the key was attached without
	// parsing the (encoded) Authorization header. Harmless in prod.
	req.Header.Set(apiKeyHeader, c.cfg.APIKey)
}

// httpError reads a snippet of a non-2xx response body for the error message.
func (c *Client) httpError(path string, resp *http.Response) error {
	snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	return fmt.Errorf("agnes: %s returned %d: %s", path, resp.StatusCode, strings.TrimSpace(string(snippet)))
}

// download fetches url and returns its bytes, capped at maxBytes. A body
// exceeding the cap is an error so the caller surfaces "too large" rather
// than silently truncating. kind is a short label ("image"/"video") for logs.
func (c *Client) download(ctx context.Context, url string, kind string, maxBytes int64) ([]byte, error) {
	c.logger.Debug("agnes: download start",
		"url", url,
		"kind", kind,
		"max_bytes", maxBytes)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("agnes: build %s download: %w", kind, err)
	}
	resp, err := c.http.Do(req) //nolint:bodyclose // closed via defer drainAndClose below
	if err != nil {
		c.logger.Warn("agnes: download failed",
			"url", url,
			"kind", kind,
			"error", err)
		return nil, fmt.Errorf("agnes: download %s: %w", kind, err)
	}
	defer drainAndClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		c.logger.Warn("agnes: download non-200",
			"url", url,
			"kind", kind,
			"status_code", resp.StatusCode)
		return nil, fmt.Errorf("agnes: %s download returned %d", kind, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("agnes: read %s bytes: %w", kind, err)
	}
	if int64(len(data)) > maxBytes {
		c.logger.Warn("agnes: download too large",
			"url", url,
			"kind", kind,
			"bytes", len(data),
			"max_bytes", maxBytes)
		return nil, fmt.Errorf("agnes: %s exceeds %d bytes", kind, maxBytes)
	}
	return data, nil
}

// drainAndClose reads remaining bytes (up to a small cap) then closes, so the
// HTTP keep-alive connection can be reused.
func drainAndClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, 4096))
	_ = rc.Close()
}

// mimeFromURL infers an image MIME type from the URL path extension. Defaults
// to image/png when unknown (Agnes returns PNG for most generations).
func mimeFromURL(u string) string {
	switch {
	case strings.HasSuffix(strings.ToLower(u), ".jpg"), strings.HasSuffix(strings.ToLower(u), ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(strings.ToLower(u), ".webp"):
		return "image/webp"
	default:
		return "image/png"
	}
}

// truncateString truncates s to at most n runes, appending "…" if truncated.
// Safe for logging long strings like prompts, URLs, or video IDs.
func truncateString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// For UTF-8, approximate rune truncation (not exact but sufficient for logging).
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n]) + "…"
}
