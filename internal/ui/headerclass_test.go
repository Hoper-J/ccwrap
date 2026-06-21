package ui

import (
	"net/http"
	"strings"
	"testing"
)

func TestClassifyHeader(t *testing.T) {
	cases := map[string]HeaderClass{
		// Bearer / API-key shapes Claude Code itself uses.
		"Authorization":       HeaderCredential,
		"authorization":       HeaderCredential,
		"X-Api-Key":           HeaderCredential,
		"x-api-key":           HeaderCredential,
		"X-Apikey":            HeaderCredential, // no-hyphen variant
		"Cookie":              HeaderCredential,
		"Proxy-Authorization": HeaderCredential,
		// Gateway / third-party credential shapes (LiteLLM, OpenRouter,
		// custom gateway flows). These are stripped from the upstream
		// wire by routeresolve.go::applyAuthOverride — the inspect
		// drawer MUST also redact them so the same secret doesn't get
		// removed from the forward but rendered raw in /recent.
		"Api-Key":          HeaderCredential,
		"X-Gateway-Key":    HeaderCredential,
		"X-LitellM-Key":    HeaderCredential,
		"X-Provider-Key":   HeaderCredential,
		"X-Provider-Token": HeaderCredential,
		// Plain rendering preserved for non-credential headers.
		"anthropic-version": HeaderShown,
		"Anthropic-Beta":    HeaderShown,
		"User-Agent":        HeaderShown,
		"x-stainless-os":    HeaderShown,
		"content-type":      HeaderShown,
		"x-future-unknown":  HeaderShown,
		"":                  HeaderShown,
	}
	for in, want := range cases {
		if got := ClassifyHeader(in); got != want {
			t.Fatalf("ClassifyHeader(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestRenderHeaderGroups(t *testing.T) {
	h := http.Header{
		"Authorization":     {"Bearer sk-REALSECRET"},
		"X-Api-Key":         {"sk-ALSOSECRET"},
		"Anthropic-Version": {"2023-06-01"},
		"User-Agent":        {"claude-cli/1.2.3"},
		"Content-Type":      {"application/json"},
		"X-Weird-Future":    {"keepme"},
	}
	groups := RenderHeaderGroups(h)

	flat := ""
	for _, g := range groups {
		for _, row := range g.Rows {
			flat += g.Name + "|" + row.Name + "|" + row.Value + "|"
		}
	}

	// LOAD-BEARING: no credential value may appear anywhere.
	if strings.Contains(flat, "sk-REALSECRET") || strings.Contains(flat, "sk-ALSOSECRET") {
		t.Fatalf("credential value leaked into rendered groups:\n%s", flat)
	}
	if !strings.Contains(flat, "Authorization|‹redacted by ccwrap›") &&
		!strings.Contains(flat, "authorization|‹redacted by ccwrap›") {
		t.Fatalf("authorization must render name + sentinel, got:\n%s", flat)
	}
	if !strings.Contains(flat, "2023-06-01") || !strings.Contains(flat, "keepme") {
		t.Fatalf("structural/unknown headers must render verbatim, got:\n%s", flat)
	}
	var order []string
	for _, g := range groups {
		if len(g.Rows) > 0 {
			order = append(order, g.Name)
		}
	}
	posProto, posOther := -1, -1
	for i, n := range order {
		if n == "Protocol & versioning" {
			posProto = i
		}
		if n == "Other" {
			posOther = i
		}
	}
	if posProto == -1 || posOther == -1 || posProto > posOther {
		t.Fatalf("group order wrong: %v", order)
	}
}

// TestRenderHeaderGroupsWithRedaction_Mask is byte-identical to
// RenderHeaderGroups (the always-redact CLI entry); locks the contract
// that the new API's redact=true mode is the historical behavior.
func TestRenderHeaderGroupsWithRedaction_Mask(t *testing.T) {
	h := http.Header{
		"Authorization":     {"Bearer sk-REALSECRET"},
		"X-Api-Key":         {"sk-ALSOSECRET"},
		"Anthropic-Version": {"2023-06-01"},
	}
	masked := RenderHeaderGroupsWithRedaction(h, true)
	flat := ""
	for _, g := range masked {
		for _, r := range g.Rows {
			flat += g.Name + "|" + r.Name + "|" + r.Value + "|"
		}
	}
	if strings.Contains(flat, "sk-REALSECRET") || strings.Contains(flat, "sk-ALSOSECRET") {
		t.Fatalf("masked must not leak credential values:\n%s", flat)
	}
	if !strings.Contains(flat, "‹redacted by ccwrap›") {
		t.Fatalf("masked must use sentinel:\n%s", flat)
	}
}

// TestRenderHeaderGroupsWithRedaction_Unmask locks the contract:
// when redact=false (CCWRAP_UNMASK_CREDENTIALS=1 path), credential header
// VALUES render raw. The classification (which group, name preserved)
// is unchanged — only the sentinel substitution is skipped. The user
// is presumed to have opted in; the ribbon UNMASKED state is the
// persistent visual signal.
func TestRenderHeaderGroupsWithRedaction_Unmask(t *testing.T) {
	h := http.Header{
		"Authorization":     {"Bearer sk-REALSECRET"},
		"X-Api-Key":         {"sk-ALSOSECRET"},
		"Anthropic-Version": {"2023-06-01"},
	}
	raw := RenderHeaderGroupsWithRedaction(h, false)
	flat := ""
	credGroupCount := 0
	for _, g := range raw {
		if g.Name == "Credentials" {
			credGroupCount++
		}
		for _, r := range g.Rows {
			flat += g.Name + "|" + r.Name + "|" + r.Value + "|"
		}
	}
	if strings.Contains(flat, "‹redacted by ccwrap›") {
		t.Fatalf("unmask must NOT emit the sentinel:\n%s", flat)
	}
	if !strings.Contains(flat, "sk-REALSECRET") || !strings.Contains(flat, "sk-ALSOSECRET") {
		t.Fatalf("unmask must render credential values raw:\n%s", flat)
	}
	// Credential-class headers still group under "Credentials" — the
	// classification is unchanged; only the value substitution is skipped.
	if credGroupCount != 1 {
		t.Fatalf("expected 1 Credentials group regardless of redact mode; got %d", credGroupCount)
	}
}

// TestRenderHeaderGroups_AlwaysRedactWrapper — the public entry (CLI
// path) MUST always redact. A future caller adding a bool flag at the
// CLI surface should NOT bypass this — they'd need the explicit
// WithRedaction variant.
func TestRenderHeaderGroups_AlwaysRedactWrapper(t *testing.T) {
	h := http.Header{"Authorization": {"Bearer sk-X"}}
	groups := RenderHeaderGroups(h)
	for _, g := range groups {
		for _, r := range g.Rows {
			if strings.Contains(r.Value, "sk-X") {
				t.Fatalf("RenderHeaderGroups must always redact (CLI safety): got %q", r.Value)
			}
		}
	}
}

// TestRenderHeaderGroupsRealClaudeCode pins the classifier/grouping to
// the ACTUAL inbound header set captured from real Claude Code 2.1.143
// (POST /v1/messages?beta=true) via an e2e ccwrap run — so
// the design stays calibrated to reality, not assumptions. Header
// NAMES are non-secret structural facts; the only value here is a fake
// secret used to re-prove redaction on the real set.
func TestRenderHeaderGroupsRealClaudeCode(t *testing.T) {
	h := http.Header{
		"Accept":          {"application/json"},
		"Accept-Encoding": {"gzip"},
		"Anthropic-Beta":  {"claude-code-20250219"},
		"Anthropic-Dangerous-Direct-Browser-Access": {"true"},
		"Anthropic-Version":                         {"2023-06-01"},
		"Authorization":                             {"Bearer sk-REALCLAUDESECRET"},
		"Connection":                                {"keep-alive"},
		"Content-Length":                            {"42"},
		"Content-Type":                              {"application/json"},
		"User-Agent":                                {"claude-cli/2.1.143"},
		"X-App":                                     {"cli"},
		"X-Claude-Code-Session-Id":                  {"sess-abc"},
		"X-Client-Request-Id":                       {"req-xyz"},
		"X-Stainless-Arch":                          {"arm64"},
		"X-Stainless-Lang":                          {"js"},
		"X-Stainless-Os":                            {"MacOS"},
		"X-Stainless-Package-Version":               {"0.x"},
		"X-Stainless-Retry-Count":                   {"0"},
		"X-Stainless-Runtime":                       {"node"},
		"X-Stainless-Runtime-Version":               {"v22"},
		"X-Stainless-Timeout":                       {"60"},
	}
	groups := RenderHeaderGroups(h)

	groupOf := map[string]string{}
	for _, g := range groups {
		for _, r := range g.Rows {
			groupOf[r.Name] = g.Name
			if strings.Contains(r.Value, "sk-REALCLAUDESECRET") {
				t.Fatalf("LOAD-BEARING: real-set credential value leaked in %s/%s", g.Name, r.Name)
			}
		}
	}
	want := map[string]string{
		"Authorization":     "Credentials",
		"Anthropic-Version": "Protocol & versioning",
		"Anthropic-Beta":    "Protocol & versioning",
		"Anthropic-Dangerous-Direct-Browser-Access": "Protocol & versioning",
		"User-Agent":               "Client identity",
		"X-App":                    "Client identity",
		"X-Client-Request-Id":      "Client identity",
		"X-Claude-Code-Session-Id": "Client identity",
		"X-Stainless-Os":           "Client identity",
		"X-Stainless-Retry-Count":  "Client identity",
		"X-Stainless-Timeout":      "Client identity",
		"Content-Type":             "Content negotiation",
		"Accept":                   "Content negotiation",
		"Accept-Encoding":          "Content negotiation",
		"Connection":               "Other",
		"Content-Length":           "Other",
	}
	for name, wantGroup := range want {
		if got := groupOf[name]; got != wantGroup {
			t.Fatalf("real header %q grouped as %q, want %q", name, got, wantGroup)
		}
	}
	if groupOf["Authorization"] != "Credentials" {
		t.Fatalf("Authorization must be Credentials (redacted)")
	}
}
