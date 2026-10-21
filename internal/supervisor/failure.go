package supervisor

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// failureBodyCapBytes bounds how much of an error response ccwrap keeps.
// Error bodies are small; the cap only guards against an HTML error page.
const failureBodyCapBytes = 16 << 10

// failureMessageMax bounds the message extracted from an error body.
const failureMessageMax = 300

// failureContext is what the hint needs to know about the request.
type failureContext struct {
	status        int
	firstParty    bool
	unmapped      bool
	model         string
	upstreamModel string
	profile       string
	aliasTarget   string // a target the route already uses, for the hint
	// fixedBase is the route's base URL minus a trailing /v1, set when the
	// base ends in /v1 (requests then go to /v1/v1/...).
	fixedBase string
}

// upstreamFailure builds the Failure for an upstream error response from
// the (decoded) body ccwrap kept.
func upstreamFailure(fc failureContext, body []byte, truncated, unmask bool) *model.RequestFailure {
	f := &model.RequestFailure{Source: "upstream", Truncated: truncated}
	if len(body) > 0 {
		f.Body = maskFailureBody(body, unmask)
		f.Type, f.Message = errorBodyMessage(body)
		if !unmask {
			f.Message = maskKeyLikeTokens(f.Message)
		}
	}
	f.Hint = failureHint(fc, f)
	return f
}

// ccwrapFailure is the Failure for a request that never got an upstream
// response.
func ccwrapFailure(summary, hint string) *model.RequestFailure {
	return &model.RequestFailure{Source: "ccwrap", Message: truncateRunes(summary, failureMessageMax), Hint: hint}
}

// errorBodyMessage pulls the error type and message out of the shapes error
// bodies come in: Anthropic and OpenAI style JSON, RFC 9457 problem JSON
// (Cloudflare), an HTML page's title, or plain text.
func errorBodyMessage(body []byte) (typ, msg string) {
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
			return typ, truncateRunes(strings.TrimSpace(msg), failureMessageMax)
		}
	}
	if m := htmlTitle.FindStringSubmatch(trimmed); m != nil {
		return "", truncateRunes(strings.TrimSpace(html.UnescapeString(m[1])), failureMessageMax)
	}
	line, _, _ := strings.Cut(trimmed, "\n")
	return "", truncateRunes(strings.TrimSpace(line), failureMessageMax)
}

var htmlTitle = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

// keyLikeToken matches API-key-shaped strings an error body may echo back.
var keyLikeToken = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{2})[A-Za-z0-9_-]{6,}`)

func maskKeyLikeTokens(s string) string {
	return keyLikeToken.ReplaceAllString(s, "$1…")
}

// maskFailureBody masks credentials in an error body: JSON credential fields
// and key-shaped tokens. Unlike captured bodies, a non-JSON error body (plain
// text, an HTML page) is shown, not withheld: it comes from the upstream's
// error path, not a credential exchange.
func maskFailureBody(body []byte, unmask bool) string {
	if unmask {
		return string(body)
	}
	if json.Valid(body) {
		body = redactJSONBody(body, false)
	}
	return maskKeyLikeTokens(string(body))
}

// failureHint is ccwrap's one-line suggestion for a failed request.
func failureHint(fc failureContext, f *model.RequestFailure) string {
	target := fc.aliasTarget
	if target == "" {
		target = "<model>"
	}
	editCmd := "--model-alias"
	if fc.profile != "" {
		editCmd = "ccwrap profile edit " + fc.profile + " --model-alias"
	}
	switch status := fc.status; {
	case fc.fixedBase != "" && status == http.StatusNotFound:
		fix := "set the base URL to " + fc.fixedBase
		if fc.profile != "" {
			fix = "ccwrap profile edit " + fc.profile + " --base-url " + fc.fixedBase
		}
		return "The base URL ends in /v1 and Claude Code adds /v1 itself, so this went to /v1/v1/messages. Drop the /v1: " + fix
	case fc.unmapped && (status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusUnprocessableEntity):
		return fmt.Sprintf("No alias for %s on this route, so the upstream got that name as-is. Fix: %s '%s=%s' (this model) or '%s=%s' (every unaliased Claude model)",
			fc.model, editCmd, fc.model, target, modelalias.DefaultKey, target)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		if fc.firstParty {
			return "Anthropic rejected Claude Code's credentials — run /login in Claude Code"
		}
		if fc.profile != "" {
			return "the upstream rejected the credentials — check the key in profile " + fc.profile + " (ccwrap profile edit " + fc.profile + ")"
		}
		return "the upstream rejected the credentials — check the key ccwrap sends for this route"
	case status == http.StatusNotFound:
		if fc.upstreamModel != "" && !fc.firstParty {
			return "the upstream has no such model or path — check that it serves " + fc.upstreamModel + " and that the base URL is right"
		}
		return "the upstream has no such model or path"
	case status == http.StatusRequestEntityTooLarge:
		return "the request is too large for the upstream"
	case status == http.StatusTooManyRequests:
		return "rate limited by the upstream — Claude Code retries on its own"
	case status >= 500:
		return "the upstream failed — usually temporary, and Claude Code retries on its own; if it keeps failing, the upstream cannot handle this request"
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// failureErrorRecord turns a failed model request into the session's error
// list entry: warn severity, since the upstream (not ccwrap) failed.
func failureErrorRecord(rec model.RequestRecord) model.ErrorRecord {
	f := rec.Failure
	what := fmt.Sprintf("%d from %s", rec.StatusCode, fallback(rec.ActualUpstreamHost, rec.LogicalTargetHost))
	if f.Message != "" {
		what += ": " + truncateRunes(f.Message, 160)
	}
	if label := ui.RequestModelLabel(rec); label != "" {
		what = label + " · " + what
	}
	return model.ErrorRecord{
		Timestamp:       rec.Timestamp,
		SessionID:       rec.SessionID,
		ErrorClass:      fmt.Sprintf("%s%d", upstreamHTTPErrorClassPrefix, rec.StatusCode),
		Summary:         what,
		UpstreamHost:    fallback(rec.ActualUpstreamHost, rec.LogicalTargetHost),
		SuggestedAction: f.Hint,
	}
}

// upstreamHTTPErrorClassPrefix marks error-list entries for model requests
// the upstream answered with an error status.
const upstreamHTTPErrorClassPrefix = "upstream_http_"

// failureTap follows one request through the reverse proxy and records why
// it failed: the upstream's error body (kept whether or not body capture is
// on) or ccwrap's own reason when no upstream response arrived.
type failureTap struct {
	fc      failureContext
	unmask  bool
	failure *model.RequestFailure
}

func newFailureTap(ap *posture, aliasCtx *modelalias.Context, unmask bool) *failureTap {
	t := &failureTap{unmask: unmask}
	t.fc.firstParty = ap.r.routeClass == model.RouteClassFirstParty
	t.fc.profile = ap.r.profileName
	t.fc.aliasTarget = aliasHintTarget(ap.r.modelAlias)
	if !t.fc.firstParty {
		t.fc.fixedBase, _ = preflight.DoubledV1Base(ap.r.apiBaseURL)
	}
	if aliasCtx != nil {
		t.fc.model, t.fc.upstreamModel = aliasCtx.LogicalModel, aliasCtx.UpstreamModel
		// First-party routes serve every Claude model; "no alias" only
		// matters where the upstream speaks other model names.
		t.fc.unmapped = aliasCtx.Unmapped && !t.fc.firstParty
	}
	return t
}

// stamp copies the model view and the failure onto the record.
func (t *failureTap) stamp(rec *model.RequestRecord) {
	rec.Model, rec.UpstreamModel, rec.ModelUnmapped = t.fc.model, t.fc.upstreamModel, t.fc.unmapped
	rec.Failure = t.failure
}

// observe runs in ModifyResponse: an error status gets a Failure right away,
// filled in with the body once the response has been relayed.
func (t *failureTap) observe(resp *http.Response) {
	if resp.StatusCode < 400 {
		return
	}
	t.fc.status = resp.StatusCode
	t.failure = upstreamFailure(t.fc, nil, false, t.unmask)
	if resp.Body == nil {
		return
	}
	enc := resp.Header.Get("Content-Encoding")
	resp.Body = newResponseBodyTee(resp.Body, failureBodyCapBytes, func(buf []byte, truncated bool) {
		if decoded, decTrunc, ok := decodeCapturedBody(buf, enc, failureBodyCapBytes); ok {
			t.failure = upstreamFailure(t.fc, decoded, truncated || decTrunc, t.unmask)
		}
	})
}

// noUpstream records ccwrap's reason when the proxy never got a response.
func (t *failureTap) noUpstream(summary, hint string) {
	t.failure = ccwrapFailure(summary, hint)
}

// aliasHintTarget picks an upstream model to suggest in alias hints: the
// default target, else one the route already maps to.
func aliasHintTarget(cfg modelalias.Config) string {
	if d := cfg.Default(); d != "" {
		return d
	}
	targets := make([]string, 0, len(cfg.Forward))
	for _, target := range cfg.Forward {
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return ""
	}
	sort.Strings(targets)
	return targets[0]
}
