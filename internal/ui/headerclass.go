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
// ccwrap strips from the upstream wire must also be redacted
// in the inspect drawer. Otherwise the same credential gets
// removed from the upstream forward but rendered in plaintext
// via /recent — a one-sided defense-in-depth gap.
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

// CredentialDenyList returns the deny-list header names, sorted. It is
// the single source of truth shared with the JS live-patch via the
// page bootstrap so Go and JS cannot drift.
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
//
// This is the always-redact entry — used by CLI surfaces (ccwrap doctor,
// status banners, etc.) that have no per-session context and must stay
// conservative. The inspect web path uses
// RenderHeaderGroupsWithRedaction so the per-session unmask flag can
// route to the raw branch.
func RenderHeaderGroups(h http.Header) []HeaderGroup {
	return RenderHeaderGroupsWithRedaction(h, true)
}

// RenderHeaderGroupsWithRedaction is the variant the inspect-web path
// uses to honor CCWRAP_UNMASK_CREDENTIALS=1. When redact is true the output
// is byte-identical to RenderHeaderGroups (credential values become the
// sentinel); when redact is false credential values are rendered raw —
// the user explicitly opted into seeing them via the env flag, and the
// ribbon Auth/Bodies cell already shows a persistent "⚠ UNMASKED"
// state to keep the choice visible (see web.go bodiesCellPresentation).
//
// Crucially this stays a render-time policy: the underlying RequestRecord
// always carries the raw value. ccwrap's /recent + SSE wire is therefore
// stable across mask/unmask modes, and curl /recent remains a useful
// self-debug surface regardless of unmask state. The mask decision lives
// at exactly two render points (this function for first paint + the
// inline-JS classifyHdr branch in web.go for SSE-driven re-render) and
// both consult the same `redact` boolean (web.go: from
// sess.CaptureBodiesUnmasked).
func RenderHeaderGroupsWithRedaction(h http.Header, redact bool) []HeaderGroup {
	buckets := make(map[string][]HeaderRow)
	for name, vals := range h {
		lower := strings.ToLower(name)
		display := strings.Join(vals, ", ")
		group := "Other"
		if ClassifyHeader(name) == HeaderCredential {
			if redact {
				display = redactedSentinel
			}
			// !redact: keep `display` as the joined raw value. The ribbon
			// UNMASKED marker is the user's persistent "you opted in" signal.
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
