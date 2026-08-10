package agnesback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stubHTTP is a minimal *http.Client stand-in: it returns a canned response
// (status, body) for every Do, recording the last request for assertions.
type stubHTTP struct {
	status int
	body   string
	err    error
	last   *http.Request
}

func (s *stubHTTP) Do(req *http.Request) (*http.Response, error) {
	s.last = req
	if s.err != nil {
		return nil, s.err
	}
	return &http.Response{
		StatusCode: s.status,
		Body:       io.NopCloser(strings.NewReader(s.body)),
		Header:     make(http.Header),
	}, nil
}

func testClient(h HTTPClient) *Client {
	return New(ClientConfig{
		BaseURL:    "https://api.example.test",
		APIKey:     "k",
		ChatModel:  "m-chat",
		ImageModel: "m-img",
		VideoModel: "m-vid",
		ImageSize:  "2K",
		ImageRatio: "16:9",
		HTTPClient: h,
	}, nil)
}

func TestGeneratePrompt_OK(t *testing.T) {
	h := &stubHTTP{status: 200, body: `{"choices":[{"message":{"content":"  a prompt  "}}]}`}
	c := testClient(h)
	got, err := c.GeneratePrompt(context.Background(), "sys", "a cat")
	if err != nil {
		t.Fatalf("GeneratePrompt: %v", err)
	}
	if got != "a prompt" {
		t.Errorf("got %q, want %q", got, "a prompt")
	}
	if h.last == nil || h.last.URL.String() != "https://api.example.test/v1/chat/completions" {
		t.Errorf("unexpected request url: %+v", h.last)
	}
	if h.last.Header.Get("X-Test-Api-Key") != "k" {
		t.Errorf("api key not attached")
	}
}

func TestGeneratePrompt_NoChoices(t *testing.T) {
	c := testClient(&stubHTTP{status: 200, body: `{"choices":[]}`})
	if _, err := c.GeneratePrompt(context.Background(), "sys", "x"); err == nil {
		t.Fatal("expected error for empty choices")
	}
}

func TestGeneratePrompt_HTTPError(t *testing.T) {
	c := testClient(&stubHTTP{status: 401, body: `unauthorized`})
	if _, err := c.GeneratePrompt(context.Background(), "sys", "x"); err == nil {
		t.Fatal("expected error for 401")
	}
}

func TestGeneratePrompt_DoError(t *testing.T) {
	c := testClient(&stubHTTP{err: errors.New("boom")})
	if _, err := c.GeneratePrompt(context.Background(), "sys", "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestGenerateImage_OK(t *testing.T) {
	pngBytes := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a}
	var seen []string
	c := testClient(nil)
	c.http = &multiHTTP{seq: []HTTPClient{
		&capturingDoer{status: 200, body: `{"data":[{"url":"https://cdn.test/a.png"}]}`, into: &seen},
		&capturingDoer{status: 200, body: string(pngBytes), into: &seen},
	}}
	data, mime, err := c.GenerateImage(context.Background(), "a cat")
	if err != nil {
		t.Fatalf("GenerateImage: %v", err)
	}
	if string(data) != string(pngBytes) {
		t.Errorf("image bytes mismatch")
	}
	if mime != "image/png" {
		t.Errorf("mime=%q want image/png", mime)
	}
	if len(seen) != 2 {
		t.Errorf("expected 2 HTTP calls, got %d", len(seen))
	}
}

func TestGenerateImage_TooLarge(t *testing.T) {
	c := New(ClientConfig{
		BaseURL: "https://api.example.test", APIKey: "k",
		ImageModel: "m", ImageSize: "1K", ImageRatio: "1:1",
		ImageMaxBytes: 3,
	}, nil)
	c.http = &multiHTTP{seq: []HTTPClient{
		&capturingDoer{status: 200, body: `{"data":[{"url":"https://cdn.test/a.png"}]}`},
		&capturingDoer{status: 200, body: "1234"},
	}}
	if _, _, err := c.GenerateImage(context.Background(), "x"); err == nil {
		t.Fatal("expected too-large error")
	}
}

func TestGenerateVideo_PollUntilCompleted(t *testing.T) {
	c := testClient(nil)
	c.http = &multiHTTP{seq: []HTTPClient{
		&capturingDoer{status: 200, body: `{"video_id":"v1","status":"queued","progress":0}`},
		&capturingDoer{status: 200, body: `{"status":"in_progress","progress":50,"metadata":{}}`},
		&capturingDoer{status: 200, body: `{"status":"completed","progress":100,"metadata":{"url":"https://cdn.test/v.mp4"}}`},
	}}
	c.cfg.VideoPollInterval = time.Millisecond
	var statuses []string
	url, err := c.GenerateVideo(context.Background(), "x", func(s string, p int) { statuses = append(statuses, s) })
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	if url != "https://cdn.test/v.mp4" {
		t.Errorf("url=%q", url)
	}
}

// TestGenerateVideo_TopLevelURL covers the live API's actual behaviour: the
// video url arrives in the top-level "url" field with an empty metadata object,
// NOT in metadata.url as the docs claim. The client must fall back to the
// top-level field.
func TestGenerateVideo_TopLevelURL(t *testing.T) {
	c := testClient(nil)
	c.http = &multiHTTP{seq: []HTTPClient{
		&capturingDoer{status: 200, body: `{"video_id":"v2","status":"queued","progress":0}`},
		&capturingDoer{status: 200, body: `{"status":"completed","progress":100,"metadata":{},"url":"https://cdn.test/top.mp4"}`},
	}}
	c.cfg.VideoPollInterval = time.Millisecond
	url, err := c.GenerateVideo(context.Background(), "x", nil)
	if err != nil {
		t.Fatalf("GenerateVideo: %v", err)
	}
	if url != "https://cdn.test/top.mp4" {
		t.Errorf("top-level url fallback failed, got %q", url)
	}
}

func TestGenerateVideo_Failed(t *testing.T) {
	c := testClient(nil)
	c.http = &multiHTTP{seq: []HTTPClient{
		&capturingDoer{status: 200, body: `{"video_id":"v1","status":"queued"}`},
		&capturingDoer{status: 200, body: `{"status":"failed","progress":0,"metadata":{}}`},
	}}
	c.cfg.VideoPollInterval = time.Millisecond
	if _, err := c.GenerateVideo(context.Background(), "x", nil); err == nil {
		t.Fatal("expected failed error")
	}
}

// --- multi-call helpers ---

func TestDownloadVideo_OK(t *testing.T) {
	c := testClient(&capturingDoer{status: 200, body: string(bytes.Repeat([]byte{0xAB}, 1024))})
	data, err := c.DownloadVideo(context.Background(), "https://cdn.test/v.mp4")
	if err != nil {
		t.Fatalf("DownloadVideo: %v", err)
	}
	if len(data) != 1024 {
		t.Errorf("downloaded %d bytes, want 1024", len(data))
	}
}

func TestDownloadVideo_TooLarge(t *testing.T) {
	c := testClient(&capturingDoer{status: 200, body: string(bytes.Repeat([]byte{0xAB}, int(DefaultVideoMaxBytes)+1))})
	_, err := c.DownloadVideo(context.Background(), "https://cdn.test/v.mp4")
	if err == nil {
		t.Fatal("expected error for oversized video")
	}
}

type capturingDoer struct {
	status int
	body   string
	into   *[]string
}

func (d *capturingDoer) Do(req *http.Request) (*http.Response, error) {
	if d.into != nil {
		*d.into = append(*d.into, req.URL.String())
	}
	return &http.Response{
		StatusCode: d.status, Body: io.NopCloser(strings.NewReader(d.body)),
		Header: make(http.Header),
	}, nil
}

type multiHTTP struct {
	seq []HTTPClient
	n   int
}

func (m *multiHTTP) Do(req *http.Request) (*http.Response, error) {
	if m.n >= len(m.seq) {
		return nil, errors.New("multiHTTP: exhausted sequence")
	}
	d := m.seq[m.n]
	m.n++
	return d.Do(req)
}

// TestGenerateVideo_TransientErrorRetries verifies a transient query error
// (network/5xx) is tolerated up to maxConsecutivePollErrors times, then the
// poll recovers when the next query succeeds.
func TestGenerateVideo_TransientErrorRetries(t *testing.T) {
	c := testClient(nil)
	c.cfg.VideoPollInterval = 5 * time.Millisecond
	c.http = &multiHTTP{seq: []HTTPClient{
		&capturingDoer{status: 200, body: `{"video_id":"v1","status":"queued","progress":0}`},
		&errDoer{err: errors.New("connection reset")},
		&errDoer{err: errors.New("timeout")},
		&capturingDoer{status: 200, body: `{"status":"completed","progress":100,"url":"https://cdn.test/v.mp4"}`},
	}}
	url, err := c.GenerateVideo(context.Background(), "x", nil)
	if err != nil {
		t.Fatalf("expected recovery after transient errors, got: %v", err)
	}
	if url != "https://cdn.test/v.mp4" {
		t.Errorf("url=%q, want https://cdn.test/v.mp4", url)
	}
}

// TestGenerateVideo_TooManyTransientErrors verifies that more than
// maxConsecutivePollErrors consecutive failures abandons the task.
func TestGenerateVideo_TooManyTransientErrors(t *testing.T) {
	c := testClient(nil)
	c.cfg.VideoPollInterval = 5 * time.Millisecond
	c.http = &multiHTTP{seq: []HTTPClient{
		&capturingDoer{status: 200, body: `{"video_id":"v1","status":"queued","progress":0}`},
		&errDoer{err: errors.New("fail1")},
		&errDoer{err: errors.New("fail2")},
		&errDoer{err: errors.New("fail3")},
	}}
	_, err := c.GenerateVideo(context.Background(), "x", nil)
	if err == nil {
		t.Fatal("expected error after too many consecutive failures, got nil")
	}
}

// errDoer always returns the configured error, simulating a network/5xx failure.
type errDoer struct{ err error }

func (e *errDoer) Do(*http.Request) (*http.Response, error) {
	return nil, e.err
}
