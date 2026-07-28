package lark

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// httpDoer is the subset of *http.Client the REST and token code calls.
// Declared as an interface so tests inject a stub without spinning up TLS.
type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// restClient wraps the three IM REST endpoints the bridge uses. It resolves
// the bearer token via tokenManager on every call (cached cheaply there).
type restClient struct {
	baseURL string // origin with scheme, e.g. "https://open.feishu.cn"
	http    httpDoer
	tokens  *tokenManager
}

// imResponse is the common envelope {code, msg, data} returned by every IM
// endpoint. Data is decoded by the caller into endpoint-specific shapes.
type imResponse struct {
	Code int             `json:"code"`
	Msg  string          `json:"msg"`
	Data json.RawMessage `json:"data"`
}

// messageData carries the message_id (and optional chat_id) from
// create/reply responses.
type messageData struct {
	MessageID string `json:"message_id"`
	ChatID    string `json:"chat_id"`
}

// SendMessage creates a new message or replies to one, per ReplyMessageID.
// in must set exactly one of Text (msg_type=text) or Card (msg_type=
// interactive). Returns the new message_id.
func (r *restClient) SendMessage(ctx context.Context, in *SendInput) (*SendResult, error) {
	if in == nil {
		return nil, fmt.Errorf("lark: nil SendInput")
	}
	msgType, content, err := encodeSendContent(in)
	if err != nil {
		return nil, err
	}
	body := map[string]string{"msg_type": msgType, "content": content}
	var data messageData
	if in.ReplyMessageID != "" {
		path := "/open-apis/im/v1/messages/" + url.PathEscape(in.ReplyMessageID) + "/reply"
		if err := r.doJSON(ctx, http.MethodPost, path, "", body, &data); err != nil {
			return nil, err
		}
	} else {
		if in.ChatID == "" {
			return nil, fmt.Errorf("lark: SendInput.ChatID required when not replying")
		}
		body["receive_id"] = in.ChatID
		if err := r.doJSON(ctx, http.MethodPost, "/open-apis/im/v1/messages",
			"receive_id_type=chat_id", body, &data); err != nil {
			return nil, err
		}
	}
	if data.MessageID == "" {
		return nil, fmt.Errorf("lark: send returned no message_id")
	}
	return &SendResult{MessageID: data.MessageID}, nil
}

// PatchMessage updates an existing message body (used to refresh a card).
// content is the raw card JSON string.
func (r *restClient) PatchMessage(ctx context.Context, messageID, content string) error {
	if messageID == "" {
		return fmt.Errorf("lark: empty message_id")
	}
	path := "/open-apis/im/v1/messages/" + url.PathEscape(messageID)
	body := map[string]string{"content": content}
	return r.doJSON(ctx, http.MethodPatch, path, "", body, nil)
}

// encodeSendContent picks msg_type and builds the inner-JSON content string
// for a SendInput. Text → {"text":"..."}; Card → the card string verbatim.
func encodeSendContent(in *SendInput) (string, string, error) {
	switch {
	case in.Text != "" && in.Card != "":
		return "", "", fmt.Errorf("lark: SendInput.Text and Card are mutually exclusive")
	case in.Text != "":
		b, err := json.Marshal(map[string]string{"text": in.Text})
		if err != nil {
			return "", "", fmt.Errorf("lark: encode text: %w", err)
		}
		return "text", string(b), nil
	case in.Card != "":
		return "interactive", in.Card, nil
	default:
		return "", "", fmt.Errorf("lark: SendInput has neither Text nor Card")
	}
}

// doJSON performs a token-authenticated JSON request and decodes the IM
// envelope. query may be "" or a pre-formed "k=v&k=v" string. out may be nil
// to ignore the data field. A non-zero business code returns *APIError.
func (r *restClient) doJSON(ctx context.Context, method, path, query string, body any, out any) error {
	tok, err := r.tokens.Token(ctx)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("lark: encode body: %w", err)
	}
	u := r.baseURL + path
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("lark: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := r.http.Do(req)
	if err != nil {
		return fmt.Errorf("lark: %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("lark: read response: %w", err)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("lark: %s %s http %d: %s", method, path, resp.StatusCode, truncate(string(raw), 200))
	}
	var env imResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("lark: decode response: %w (body: %s)", err, truncate(string(raw), 200))
	}
	if env.Code != 0 {
		return &APIError{Code: env.Code, Msg: env.Msg}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("lark: decode data: %w", err)
		}
	}
	return nil
}

// downloadMaxBytes caps a single resource download. Feishu file messages
// include documents users explicitly attached, which can be large; we keep a
// 30 MiB ceiling so a malicious or accidental huge upload cannot exhaust
// memory or stall the dispatcher goroutine. The dispatcher's FileConvert
// layer enforces its own MaxFileSize earlier (a friendly notice), this is the
// hard backstop for raw byte accounting.
const downloadMaxBytes = 30 << 20

// DownloadResource fetches a binary resource attached to a message (file,
// image). It opens a streaming GET against the IM resources endpoint and
// returns the body for the caller to copy; the caller MUST close the reader.
//
// The query parameter type carries the resource kind ("file" or "image");
// only "file" is used by the bridge today, but the parameter is kept explicit
// so future callers do not need an API change.
//
// Conventions mirror doJSON: token-authenticated, error envelope parsed for
// business codes. Unlike doJSON, the success body is binary and not bounded
// by the 1 MiB JSON cap — a wrapping io.LimitReader enforces downloadMaxBytes
// instead so the response stream cannot exhaust memory if the server misbehaves.
func (r *restClient) DownloadResource(ctx context.Context, messageID, fileKey, fileType string) (io.ReadCloser, error) {
	if messageID == "" || fileKey == "" {
		return nil, fmt.Errorf("lark: messageID and fileKey required")
	}
	if fileType == "" {
		fileType = "file"
	}
	tok, err := r.tokens.Token(ctx)
	if err != nil {
		return nil, err
	}
	path := "/open-apis/im/v1/messages/" + url.PathEscape(messageID) +
		"/resources/" + url.PathEscape(fileKey)
	u := r.baseURL + path + "?type=" + url.QueryEscape(fileType)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("lark: build download request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := r.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("lark: download %s: %w", path, err)
	}
	// On a non-success status the body is a JSON error envelope: read it
	// fully (capped), parse for the business code, and surface an APIError
	// matching doJSON's contract. On success the body stays streaming with a
	// byte-ceiling reader so callers can copy it incrementally.
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		var env imResponse
		if err := json.Unmarshal(raw, &env); err == nil && env.Code != 0 {
			return nil, &APIError{Code: env.Code, Msg: env.Msg}
		}
		return nil, fmt.Errorf("lark: download %s http %d: %s", path, resp.StatusCode, truncate(string(raw), 200))
	}
	return &limitedReadCloser{header: resp.Header, body: io.LimitReader(resp.Body, downloadMaxBytes+1), closer: resp.Body}, nil
}

// limitedReadCloser preserves the original response headers (Content-Type,
// Content-Disposition carry the file name and mime the dispatcher may want)
// while bounding bytes read and forwarding Close to the underlying body.
type limitedReadCloser struct {
	header http.Header
	body   io.Reader
	closer io.Closer
}

func (l *limitedReadCloser) Read(p []byte) (int, error) { return l.body.Read(p) }
func (l *limitedReadCloser) Close() error               { return l.closer.Close() }

// Header exposes the underlying HTTP response headers (Content-Type etc.).
func (l *limitedReadCloser) Header() http.Header { return l.header }

// newHTTPClient returns the default *http.Client used when none is provided.
// A bounded Timeout prevents a stalled Feishu API from wedging dispatcher
// goroutines forever (the SDK left ReqTimeout==0 → http.DefaultClient with no
// timeout, which the bridge previously had to paper over).
func newHTTPClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second}
}
