package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// maxWebFetchChars bounds one webfetch result.
const maxWebFetchChars = 20000

// webfetchTimeout bounds one HTTP GET.
const webfetchTimeout = 30 * time.Second

// webfetchArgs is the LLM-supplied argument object for webfetch.
type webfetchArgs struct {
	URL string `json:"url"`
}

// WebFetch GETs one http(s) URL and returns its text content. The HTML is
// parsed via golang.org/x/net/html; script/style/title nodes are dropped
// and the remaining text is concatenated, collapsing whitespace, then
// truncated to maxWebFetchChars.
//
// KNOWN SSRF RISK (accepted): scheme is checked to be http/https, but the
// host is NOT screened for private/loopback/link-local addresses. The LLM
// can therefore direct the agent at internal endpoints (e.g. cloud
// metadata 169.254.169.254, localhost services). Treat the miniagent user
// as the boundary, not URL filtering. Add host screening if this backend
// ever runs somewhere where internal reachability is sensitive.
type WebFetch struct {
	HTTP *http.Client // nil → default with webfetchTimeout
}

func (WebFetch) Spec() ToolSpec {
	return ToolSpec{
		Name:        "webfetch",
		Description: "抓取一个 http(s) 网页并返回其纯文本内容（已去掉 script/style/HTML 标签，最长 " + fmt.Sprintf("%d", maxWebFetchChars) + " 字符）。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{
					"type":        "string",
					"description": "要抓取的完整 http(s) URL",
				},
			},
			"required": []string{"url"},
		},
	}
}

// Call GETs the URL, checks for a 2xx, and extracts text. Any failure
// (bad scheme, non-2xx, transport error, parse error) yields IsError=true.
func (w WebFetch) Call(ctx context.Context, args string) ToolResult {
	var a webfetchArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if strings.TrimSpace(a.URL) == "" {
		return ToolResult{IsError: true, Output: "参数缺失：url"}
	}
	if !isHTTPURL(a.URL) {
		return ToolResult{IsError: true, Output: fmt.Sprintf("仅支持 http/https URL，收到 %q", a.URL)}
	}

	client := w.HTTP
	if client == nil {
		client = &http.Client{Timeout: webfetchTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.URL, nil)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("构造请求失败：%v", err)}
	}
	req.Header.Set("User-Agent", "lark-miniagent-back/webfetch")
	resp, err := client.Do(req)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("抓取失败：%v", err)}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20)) // 5MB raw cap
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取响应失败：%v", err)}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ToolResult{IsError: true, Output: fmt.Sprintf("%s 返回 %d：%s", a.URL, resp.StatusCode, truncate(string(body), 200, "…"))}
	}

	ctype := resp.Header.Get("Content-Type")
	if strings.Contains(ctype, "text/html") {
		text, perr := htmlToText(body)
		if perr != nil {
			// Fall back to raw body rather than failing the whole call:
			// the LLM may still glean something from partial markup.
			return ToolResult{Output: truncate(string(body), maxWebFetchChars, "…")}
		}
		return ToolResult{Output: truncate(text, maxWebFetchChars, "…")}
	}
	// Non-HTML (plain text, json, etc.) returned as-is.
	return ToolResult{Output: truncate(string(body), maxWebFetchChars, "…")}
}

// isHTTPURL reports whether u starts with an http:// or https:// scheme
// (case-insensitive). It does NOT validate the rest of the URL — the http
// client will surface malformed URLs as transport errors.
func isHTTPURL(u string) bool {
	low := strings.ToLower(u)
	return strings.HasPrefix(low, "http://") || strings.HasPrefix(low, "https://")
}

// blockTags are HTML elements that force a line break in the extracted
// text. Inline tags (b/i/a/span/…) are NOT here so their text concatenates
// naturally with surrounding text (e.g. "hello <b>world</b>" → "hello world",
// not "hello\nworld"). Used by htmlToText.
var blockTags = map[string]bool{
	"address": true, "article": true, "aside": true, "blockquote": true,
	"br": true, "dd": true, "div": true, "dl": true, "dt": true,
	"fieldset": true, "figcaption": true, "figure": true, "footer": true,
	"form": true, "h1": true, "h2": true, "h3": true, "h4": true, "h5": true,
	"h6": true, "header": true, "hr": true, "li": true, "main": true,
	"nav": true, "ol": true, "p": true, "pre": true, "section": true,
	"table": true, "tbody": true, "td": true, "tfoot": true, "th": true,
	"thead": true, "tr": true, "ul": true,
}

// htmlToText parses an HTML body and returns its visible text: script,
// style, title, and noscript subtrees are skipped; block-level elements
// force a line break (so <p>hello <b>world</b></p> renders as "hello world"
// on one line); inline text concatenates with surrounding text. Used by
// webfetch so the LLM gets readable content instead of raw markup.
func htmlToText(body []byte) (string, error) {
	doc, err := html.Parse(strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch n.Data {
			case "script", "style", "title", "noscript":
				return // skip entire subtree
			}
		}
		if n.Type == html.ElementNode && blockTags[n.Data] && b.Len() > 0 {
			b.WriteByte('\n')
		}
		if n.Type == html.TextNode {
			s := strings.TrimSpace(n.Data)
			if s != "" {
				b.WriteString(s)
				if !strings.HasSuffix(s, " ") {
					b.WriteByte(' ')
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return strings.TrimSpace(b.String()), nil
}
