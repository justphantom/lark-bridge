package miniagent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestHTTPClient_Success verifies a 200 response with one choice is parsed
// into Text + Usage, and that the request carried the Bearer key + the
// user message.
func TestHTTPClient_Success(t *testing.T) {
	var gotAuth string
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role":"assistant","content":"hi there"}}],
			"usage": {"prompt_tokens": 8, "completion_tokens": 3}
		}`))
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk-test", BaseURL: srv.URL}
	resp, err := c.Do(context.Background(), Request{
		Model:    "gpt-4o-mini",
		System:   "be brief",
		Messages: []Message{{Role: "user", Content: "hello"}},
	})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Text != "hi there" {
		t.Errorf("Text = %q, want 'hi there'", resp.Text)
	}
	if resp.Usage.InputTokens != 8 || resp.Usage.OutputTokens != 3 {
		t.Errorf("Usage = %+v, want {8 3}", resp.Usage)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Auth = %q, want 'Bearer sk-test'", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"gpt-4o-mini"`) {
		t.Errorf("body missing model: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"role":"system"`) || !strings.Contains(gotBody, "be brief") {
		t.Errorf("body missing system message: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"role":"user"`) || !strings.Contains(gotBody, "hello") {
		t.Errorf("body missing user message: %s", gotBody)
	}
}

// TestHTTPClient_ToolCallsParsed verifies a function tool_call in the
// response decodes into Response.ToolCalls (P1+ path, but the wire format
// is exercised now).
func TestHTTPClient_ToolCallsParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role":"assistant","content":"","tool_calls":[
				{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"/x\"}"}}
			]}}]
		}`))
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk-test", BaseURL: srv.URL}
	resp, err := c.Do(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(resp.ToolCalls))
	}
	tc := resp.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "read_file" || !strings.Contains(tc.Args, "/x") {
		t.Errorf("ToolCall = %+v", tc)
	}
}

// TestHTTPClient_Non200Errors verifies a non-200 response returns an error
// mentioning the status and a snippet of the body.
func TestHTTPClient_Non200Errors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid api key"}`))
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk-bad", BaseURL: srv.URL}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error for 401, got nil")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %v, want contains '401'", err)
	}
}

// TestHTTPClient_RetriesThenSucceeds verifies a 503→503→200 sequence returns
// the success body and the handler observed 3 attempts.
func TestHTTPClient_RetriesThenSucceeds(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer srv.Close()

	// Short-circuit the real backoff so the test is fast.
	orig := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { retryDelays = orig }()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL}
	resp, err := c.Do(context.Background(), Request{Model: "m"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q, want ok", resp.Text)
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

// TestHTTPClient_RetryExhausted verifies that after len(retryDelays) retries
// the final failure is surfaced (not retried forever).
func TestHTTPClient_RetryExhausted(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":{"message":"bad gateway"}}`))
	}))
	defer srv.Close()

	orig := retryDelays
	retryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	defer func() { retryDelays = orig }()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error after retries exhausted, got nil")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error = %v, want contains 502", err)
	}
	// 1 initial + 3 retries = 4 attempts.
	if got := atomic.LoadInt32(&attempts); got != 4 {
		t.Errorf("attempts = %d, want 4", got)
	}
}

// TestHTTPClient_4xxNotRetried verifies a non-retryable status (400) fails
// immediately without consuming the retry budget.
func TestHTTPClient_4xxNotRetried(t *testing.T) {
	var attempts int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer srv.Close()

	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Errorf("error = %v, want contains 400", err)
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (400 must not retry)", got)
	}
}

// TestHTTPClient_RetryAbortable verifies ctx cancellation during a backoff
// wait aborts the retry loop promptly (so /abort or Close is responsive).
func TestHTTPClient_RetryAbortable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"down"}}`))
	}))
	defer srv.Close()

	orig := retryDelays
	retryDelays = []time.Duration{10 * time.Second, 10 * time.Second}
	defer func() { retryDelays = orig }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	c := &HTTPClient{APIKey: "sk", BaseURL: srv.URL}
	_, err := c.Do(ctx, Request{Model: "m"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// ctx deadline should propagate, not the 503.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
}

// TestHTTPClient_EmptyAPIKeyErrors verifies the guard fires before any HTTP.
func TestHTTPClient_EmptyAPIKeyErrors(t *testing.T) {
	c := &HTTPClient{APIKey: "", BaseURL: "http://example.invalid"}
	_, err := c.Do(context.Background(), Request{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "api_key is empty") {
		t.Errorf("error = %v, want 'api_key is empty'", err)
	}
}

// TestParseChatResponse_NoChoices verifies a choices-less body errors.
func TestParseChatResponse_NoChoices(t *testing.T) {
	_, err := parseChatResponse([]byte(`{"choices":[]}`))
	if err == nil || !strings.Contains(err.Error(), "no choices") {
		t.Errorf("error = %v, want 'no choices'", err)
	}
}
