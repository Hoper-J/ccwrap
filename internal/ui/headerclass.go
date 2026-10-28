package ui

import (
	"net/http"
	"sort"
	"strings"
)

// HeaderClass is the render-time classification of an HTTP request
// header. Storage is always full-fidelity; this only drives display.
type HeaderClass int

const (
	HeaderShown HeaderClass = iota
	HeaderCredential
)

// credentialDenyList is the built-in, fixed set of header names whose
// VALUE must never render on the Web page. Names are lowercase;
// matching is case-insensitive. Not user-configurable (configurable
// redaction is a footgun). Editing this list instantly reclassifies
// all already-captured requests.
//
// Inbound-only capture means CCWRAP-injected upstream credentials
// never reach this side, so this list is complete for the inbound
// request. NOTE (accepted limitation): the list is enumerated — a
// future credential-shaped header Claude Code might add would render
// until added here. This is intentional and must not be "fixed" with
// heuristics.
//
// MUST stay aligned with the upstream-strip list in
// routeresolve.go::applyAuthOverride: every credential header
// ccwrap strips from the upstream wire must also be masked
// in recorded requests. Otherwise the same credential gets
// removed from the upstream forward but shown in plaintext
// in the session's activity — a one-sided defense-in-depth gap.
var credentialDenyList = map[string]struct{}{
	"authorization":       {},
	"proxy-authorization": {},
	"x-api-key":           {},
	"x-apikey":            {},
	"api-key":             {},
	"x-gateway-key":       {},
	"x-litellm-key":       {},
	"x-provider-key":      {},
	"x-provider-token":    {},
	"cookie":              {},
}

// CredentialDenyList returns the deny-list header names, sorted. ccwrap
// ui's page gets it in its data-deny attribute, so Go and JS cannot
// drift.
func CredentialDenyList() []string {
	out := make([]string, 0, len(credentialDenyList))
	for k := range credentialDenyList {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ClassifyHeader classifies a header name. Everything not in the
// deny-list is HeaderShown, including unknown/future headers (the
// accepted enumerated-deny-list residual).
func ClassifyHeader(name string) HeaderClass {
	if _, ok := credentialDenyList[strings.ToLower(strings.TrimSpace(name))]; ok {
		return HeaderCredential
	}
	return HeaderShown
}

// HeaderRow is one rendered header line. Value is the literal display
// string — for credential headers it is the sentinel, never the
// secret.
type HeaderRow struct {
	Name  string
	Value string
}

// HeaderGroup is a titled, ordered set of header rows.
type HeaderGroup struct {
	Name string
	Rows []HeaderRow
}

const redactedSentinel = "‹redacted by ccwrap›"

// headerGroupOrder is the fixed display order. First matching
// predicate wins; unmatched non-credential headers fall to "Other";
// credential-class headers always go to "Credentials".
var headerGroupOrder = []struct {
	name  string
	match func(lower string) bool
}{
	{"Protocol & versioning", func(l string) bool { return strings.HasPrefix(l, "anthropic-") }},
	// Client identity: calibrated against real Claude Code
	// 2.1.143 traffic (POST /v1/messages). The Stainless SDK block
	// (x-stainless-*, incl. Retry-Count/Timeout) and Claude Code's
	// client-correlation headers (x-app, x-client-request-id,
	// x-claude-code-session-id) are one coherent "who/which client"
	// family — that is how Claude Code actually organizes them.
	{"Client identity", func(l string) bool {
		return l == "user-agent" || strings.HasPrefix(l, "x-stainless-") ||
			l == "x-app" || l == "x-client-request-id" || l == "x-claude-code-session-id"
	}},
	{"Content negotiation", func(l string) bool {
		return l == "content-type" || l == "accept" || l == "accept-encoding"
	}},
	// Reliability: real Claude Code 2.1.143 sends no bare
	// Idempotency-Key (retry/timeout ride x-stainless-*, grouped under
	// Client identity above). Kept for forward-compat / other clients;
	// empty groups are omitted by RenderHeaderGroups, so this is free.
	{"Reliability", func(l string) bool {
		return l == "idempotency-key"
	}},
	{"Credentials", nil}, // filled by classification, not predicate
	{"Other", func(l string) bool { return true }},
}

// RenderHeaderGroups turns a captured header map into ordered, grouped
// display rows. Credential-class values become the sentinel; the name
// is still shown. Multi-value headers join with ", ". Wire order is
// not preserved (http.Header is a map; accepted limitation, not to be
// "fixed").
func RenderHeaderGroups(h http.Header) []HeaderGroup {
	buckets := make(map[string][]HeaderRow)
	for name, vals := range h {
		lower := strings.ToLower(name)
		display := strings.Join(vals, ", ")
		group := "Other"
		if ClassifyHeader(name) == HeaderCredential {
			display = redactedSentinel
			group = "Credentials"
		} else {
			for _, g := range headerGroupOrder {
				if g.match != nil && g.name != "Other" && g.match(lower) {
					group = g.name
					break
				}
			}
		}
		buckets[group] = append(buckets[group], HeaderRow{Name: name, Value: display})
	}
	out := make([]HeaderGroup, 0, len(headerGroupOrder))
	for _, g := range headerGroupOrder {
		rows := buckets[g.name]
		if len(rows) == 0 {
			continue
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
		out = append(out, HeaderGroup{Name: g.name, Rows: rows})
	}
	return out
}
