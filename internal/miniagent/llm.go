// Package miniagent is a self-contained ReAct agent backend: it calls an
// OpenAI-compatible chat completions endpoint directly (no external agent
// CLI), drives a tool loop, and emits Controls back to the frontend. P0
// implements single-turn Q&A with the loop scaffolding in place; tools,
// memory, and permissions land in later phases.
package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hu/lark-bridge/internal/log"
)

// Message is one chat message. Role is "system" | "user" | "assistant" |
// "tool". Content is the text body; ToolCallID tags a role="tool" message
// with the call it answers (OpenAI requires this correlation).
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is one LLM-requested tool invocation. P0 carries the type but
// the loop never executes one (returns an error instead); P1+ dispatches it.
type ToolCall struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Args string `json:"args"` // raw JSON arguments string
}

// Request is the backend-agnostic call to the LLM. System is sent as a
// leading "system" message; Messages is the conversation so far.
type Request struct {
	Model     string
	System    string
	Messages  []Message
	MaxTokens int
	Tools     []ToolSpec // P1+; empty in P0
}

// ToolSpec declares one tool to the LLM (OpenAI function-calling schema).
// P0 leaves this empty; P1 populates it.
type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Response is what the LLM returned for one Request.
type Response struct {
	Text      string     // assistant text (terminal when ToolCalls is empty)
	ToolCalls []ToolCall // non-empty → loop must execute them and continue
	Usage     Usage
}

// Usage is the token accounting for one call.
type Usage struct {
	InputTokens  int
	OutputTokens int
}

// Client calls an LLM. The production impl is HTTPClient (net/http →
// OpenAI-compatible /v1/chat/completions); tests inject Fake to drive the
// loop without network.
type Client interface {
	Do(ctx context.Context, req Request) (Response, error)
}

// ModelLister is optionally implemented by a Client that can enumerate
// available models (e.g. HTTPClient calls GET /v1/models). The /models
// command type-asserts to this; a Client that does not implement it (tests)
// gets a "not supported" reply.
type ModelLister interface {
	ListModels(ctx context.Context) ([]string, error)
}

// HTTPClient calls an OpenAI-compatible chat completions endpoint via
// net/http. BaseURL must NOT end with a slash or /v1; the path is appended.
// Logger is optional (nil → no HTTP-level logs); the loop logs call results
// at a higher level, this logs the raw request/response for triage.
type HTTPClient struct {
	APIKey  string
	BaseURL string // e.g. "https://api.openai.com"
	HTTP    *http.Client
	Logger  *log.Logger
}

// retryableStatus reports whether an HTTP status merits a bounded retry
// (transient server/load conditions). 429 is rate-limiting; 500/502/503/504
// are gateway/availability blips. Other 4xx are request bugs — retrying
// cannot help.
func retryableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// retryDelays is the fixed backoff schedule for LLM retries: ~1s, ~2s, ~4s.
// Bounded (not exponential-forever) because a stuck endpoint should fail the
// turn, not hang it; the user can resend. Mirrors backendrpc's bounded
// backoff style but caps at 3 retries since each LLM call already costs.
var retryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// Do posts req to {BaseURL}/v1/chat/completions and parses the response.
// Transient failures (429/5xx) are retried with bounded backoff up to
// len(retryDelays) times; the wait observes ctx so abort/close interrupt it.
func (c *HTTPClient) Do(ctx context.Context, req Request) (Response, error) {
	if c.APIKey == "" {
		return Response{}, fmt.Errorf("miniagent: api_key is empty")
	}
	// Read c.HTTP into a local once; never write it back here. The lazy-init
	// pattern (c.HTTP = ...) raced when multiple chats called Do concurrently
	// before the first assignment landed. Production sets HTTP at construction
	// (main.go); this fallback covers direct `HTTPClient{}` construction in
	// tests without mutating the shared field.
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	logger := c.Logger
	if logger == nil {
		logger = log.Nop()
	}
	body, err := buildChatBody(req)
	if err != nil {
		return Response{}, fmt.Errorf("build request body: %w", err)
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/v1/chat/completions"
	logger.Debug("miniagent http request",
		"url", url, "model", req.Model,
		"messages", len(req.Messages), "body_bytes", len(body))

	var lastErr error
	for attempt := 0; ; attempt++ {
		raw, status, err := c.doOnce(ctx, client, url, body)
		if err == nil && status == http.StatusOK {
			logger.Debug("miniagent http response", "status", status, "body_bytes", len(raw), "attempt", attempt)
			return parseChatResponse(raw)
		}
		// Surface the concrete failure.
		if err != nil {
			lastErr = fmt.Errorf("llm request: %w", err)
		} else {
			lastErr = fmt.Errorf("llm returned %d: %s", status, truncate(string(raw), 500, "…"))
		}
		// Retry only transient HTTP statuses (not transport errors, which
		// tend to persist; not non-retryable 4xx). Transport errors here are
		// things like a dead host — one quick retry is reasonable but we
		// treat them as lastErr and fall through, since distinguishing
		// "transient dial" from "config wrong" is unreliable.
		if err != nil || !retryableStatus(status) || attempt >= len(retryDelays) {
			if err != nil {
				logger.Warn("miniagent http transport failed", log.FieldError, err, "url", url)
			} else {
				logger.Warn("miniagent http non-200 giving up",
					"status", status, "body_len", len(raw), "attempts", attempt+1)
			}
			return Response{}, lastErr
		}
		delay := retryDelays[attempt]
		// Honor Retry-After when the endpoint provides it (429 often does),
		// clamped to the same cap so a hostile/misconfigured header cannot
		// stall the turn arbitrarily.
		if ra := parseRetryAfter(raw); ra > 0 && ra < delay {
			delay = ra
		}
		logger.Warn("miniagent http retryable status, backing off",
			"status", status, "attempt", attempt+1, "backoff", delay)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Response{}, ctx.Err()
		}
	}
}

// doOnce performs a single POST and returns the raw response body, the HTTP
// status (0 on transport error), and the error. It is the unit of retry.
func (c *HTTPClient) doOnce(ctx context.Context, client *http.Client, url string, body []byte) (raw []byte, status int, err error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		// On some errors (redirect policy, auth negotiation) the http client
		// returns a non-nil resp alongside err; its body must still be closed
		// to avoid leaking the connection.
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if rerr != nil {
		return raw, resp.StatusCode, fmt.Errorf("read response: %w", rerr)
	}
	return raw, resp.StatusCode, nil
}

// parseRetryAfter extracts a Retry-After hint from an error response body.
// OpenAI-compatible endpoints usually echo it as a top-level JSON field when
// 429; returns 0 if absent/unparseable so the caller falls back to its own
// schedule. body is the already-read non-200 response body.
func parseRetryAfter(body []byte) time.Duration {
	var v struct {
		Error struct {
			RetryAfter float64 `json:"retry_after"` // seconds (some endpoints)
		} `json:"error"`
	}
	if json.Unmarshal(body, &v) == nil && v.Error.RetryAfter > 0 {
		return time.Duration(v.Error.RetryAfter * float64(time.Second))
	}
	return 0
}

// ListModels calls GET {BaseURL}/v1/models and returns the model ids. Used by
// the /models command so the user sees what the endpoint actually offers.
func (c *HTTPClient) ListModels(ctx context.Context) ([]string, error) {
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models: %w", err)
	}
	defer func() {
		if resp != nil {
			_ = resp.Body.Close()
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("list models: %d", resp.StatusCode)
	}
	var v struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	out := make([]string, 0, len(v.Data))
	for _, m := range v.Data {
		if m.ID != "" {
			out = append(out, m.ID)
		}
	}
	return out, nil
}

// chatMessage is the OpenAI wire format for one message.
type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Fn   struct {
		Name string `json:"name"`
		Args string `json:"arguments"`
	} `json:"function"`
}

// buildChatBody assembles the OpenAI /v1/chat/completions JSON from req.
func buildChatBody(req Request) ([]byte, error) {
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		cm := chatMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			ctc := chatToolCall{ID: tc.ID, Type: "function"}
			ctc.Fn.Name = tc.Name
			ctc.Fn.Args = tc.Args
			cm.ToolCalls = append(cm.ToolCalls, ctc)
		}
		msgs = append(msgs, cm)
	}
	payload := map[string]any{
		"model":    req.Model,
		"messages": msgs,
	}
	// Only send max_tokens when set; some providers reject max_tokens:0.
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		funcs := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			funcs = append(funcs, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
		payload["tools"] = funcs
	}
	return json.Marshal(payload)
}

// parseChatResponse extracts the assistant text, any tool_calls, and usage
// from an OpenAI chat completion response body.
func parseChatResponse(raw []byte) (Response, error) {
	var cr struct {
		Choices []struct {
			Message struct {
				Role      string         `json:"role"`
				Content   string         `json:"content"`
				ToolCalls []chatToolCall `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &cr); err != nil {
		return Response{}, fmt.Errorf("parse llm response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return Response{}, fmt.Errorf("llm response had no choices")
	}
	ch := cr.Choices[0]
	out := Response{
		Text:  ch.Message.Content,
		Usage: Usage{InputTokens: cr.Usage.PromptTokens, OutputTokens: cr.Usage.CompletionTokens},
	}
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.ID, Name: tc.Fn.Name, Args: tc.Fn.Args})
	}
	return out, nil
}
