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
)

// Message is one chat message. Role is "system" | "user" | "assistant" |
// "tool". Content is the text body; ToolCallID tags a role="tool" message
// with the call it answers (OpenAI requires this correlation).
type Message struct {
	Role      string     `json:"role"`
	Content   string     `json:"content"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string    `json:"tool_call_id,omitempty"`
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
	Model      string
	System     string
	Messages   []Message
	MaxTokens  int
	Tools      []ToolSpec // P1+; empty in P0
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

// HTTPClient calls an OpenAI-compatible chat completions endpoint via
// net/http. BaseURL must NOT end with a slash or /v1; the path is appended.
type HTTPClient struct {
	APIKey  string
	BaseURL string // e.g. "https://api.openai.com"
	HTTP    *http.Client
}

// Do posts req to {BaseURL}/v1/chat/completions and parses the response.
func (c *HTTPClient) Do(ctx context.Context, req Request) (Response, error) {
	if c.APIKey == "" {
		return Response{}, fmt.Errorf("miniagent: api_key is empty")
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: 120 * time.Second}
	}
	body, err := buildChatBody(req)
	if err != nil {
		return Response{}, fmt.Errorf("build request body: %w", err)
	}
	url := strings.TrimRight(c.BaseURL, "/") + "/v1/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Response{}, fmt.Errorf("llm request: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Response{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("llm returned %d: %s", resp.StatusCode, truncate(string(raw), 500))
	}
	return parseChatResponse(raw)
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
		"model":       req.Model,
		"messages":    msgs,
		"max_tokens":  req.MaxTokens,
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

func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
