// Package upstreamerr reads the error an upstream put in a response body,
// for the session's failure records and the profile connectivity test.
package upstreamerr

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
)

// MessageMax bounds the message Message returns.
const MessageMax = 300

// Message pulls the error type and message out of the shapes error bodies
// come in: Anthropic and OpenAI style JSON, RFC 9457 problem JSON
// (Cloudflare), an HTML page's title, or plain text.
func Message(body []byte) (typ, msg string) {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return "", ""
	}
	if strings.HasPrefix(trimmed, "{") {
		var doc struct {
			Type    string          `json:"type"`
			Message string          `json:"message"`
			Title   string          `json:"title"`
			Detail  string          `json:"detail"`
			Error   json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(trimmed), &doc) == nil {
			var inner struct {
				Type    string `json:"type"`
				Message string `json:"message"`
				Code    any    `json:"code"`
			}
			var errString string
			switch {
			case len(doc.Error) > 0 && json.Unmarshal(doc.Error, &inner) == nil && (inner.Message != "" || inner.Type != ""):
				typ, msg = inner.Type, inner.Message
			case len(doc.Error) > 0 && json.Unmarshal(doc.Error, &errString) == nil:
				msg = errString
			case doc.Message != "":
				msg = doc.Message
			case doc.Title != "" || doc.Detail != "":
				msg = strings.TrimSpace(strings.TrimSuffix(doc.Title, ".") + ": " + doc.Detail)
				msg = strings.Trim(msg, ": ")
			}
			if typ == "" && doc.Type != "error" && !strings.Contains(doc.Type, "://") {
				typ = doc.Type
			}
			return typ, truncate(strings.TrimSpace(msg), MessageMax)
		}
	}
	if m := htmlTitle.FindStringSubmatch(trimmed); m != nil {
		return "", truncate(strings.TrimSpace(html.UnescapeString(m[1])), MessageMax)
	}
	line, _, _ := strings.Cut(trimmed, "\n")
	return "", truncate(strings.TrimSpace(line), MessageMax)
}

var htmlTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// keyLikeToken matches API-key-shaped strings an error body may echo back.
var keyLikeToken = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{2})[A-Za-z0-9_-]{6,}`)

// MaskKeys masks the API-key-shaped strings in s.
func MaskKeys(s string) string {
	return keyLikeToken.ReplaceAllString(s, "$1…")
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
