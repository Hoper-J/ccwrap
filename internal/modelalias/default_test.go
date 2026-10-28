package modelalias

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func mustNew(t *testing.T, forward map[string]string) Config {
	t.Helper()
	cfg, err := New(forward, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func requestModel(t *testing.T, body []byte) string {
	t.Helper()
	var head struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &head); err != nil {
		t.Fatal(err)
	}
	return head.Model
}

func TestDefaultAliasMapsOtherClaudeModels(t *testing.T) {
	// The default may share its target with an explicit alias.
	cfg := mustNew(t, map[string]string{"claude-opus-5-5": "gpt-6", DefaultKey: "gpt-6"})
	cases := []struct {
		model, upstream string
		rewritten       bool
	}{
		{"claude-opus-5-5", "gpt-6", true},
		{"claude-fable-5-1", "gpt-6", true},
		{"claude-haiku-4-5-20251001", "gpt-6", true},
		// Only Claude models fall to the default.
		{"gpt-4o", "gpt-4o", false},
	}
	for _, tc := range cases {
		body := []byte(`{"model":"` + tc.model + `","max_tokens":8,"messages":[]}`)
		out, ctx, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
		if err != nil {
			t.Fatalf("%s: %v", tc.model, err)
		}
		if got := requestModel(t, out); got != tc.upstream {
			t.Errorf("%s: upstream model = %q, want %q", tc.model, got, tc.upstream)
		}
		if ctx.Rewritten != tc.rewritten || ctx.LogicalModel != tc.model || ctx.UpstreamModel != tc.upstream {
			t.Errorf("%s: ctx = %+v", tc.model, ctx)
		}
		if ctx.Unmapped {
			t.Errorf("%s: unmapped with a default configured", tc.model)
		}
	}
}

// A response is normalized to the model its own request asked for, even when
// several logical models share the upstream target.
func TestDefaultAliasResponseRestoresTheRequestedModel(t *testing.T) {
	cfg := mustNew(t, map[string]string{"claude-opus-5-5": "gpt-6", DefaultKey: "gpt-6"})
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-fable-5-1","messages":[]}`)))
	req.Header.Set("Content-Type", "application/json")
	ctx, err := RewriteRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(bytes.NewReader([]byte(`{"type":"message","model":"gpt-6"}`))),
	}
	if err := RewriteResponse(resp, ctx); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	if got := requestModel(t, out); got != "claude-fable-5-1" {
		t.Fatalf("response model = %q, want claude-fable-5-1", got)
	}
}

func TestUnmappedClaudeModelIsReported(t *testing.T) {
	cfg := mustNew(t, map[string]string{"claude-opus-5-5": "gpt-6"})
	out, ctx, err := RewriteJSONRequestBody("/v1/messages", []byte(`{"model":"claude-fable-5-1","messages":[]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestModel(t, out); got != "claude-fable-5-1" {
		t.Fatalf("unmapped model must pass through unchanged, got %q", got)
	}
	if !ctx.Unmapped || ctx.Rewritten || ctx.LogicalModel != "claude-fable-5-1" || ctx.UpstreamModel != "claude-fable-5-1" {
		t.Fatalf("ctx = %+v", ctx)
	}
}

func TestDefaultAliasStaysOutOfReverseLookups(t *testing.T) {
	cfg := mustNew(t, map[string]string{DefaultKey: "gpt-6"})
	if got := cfg.Default(); got != "gpt-6" {
		t.Fatalf("Default() = %q", got)
	}
	if logical, ok := cfg.LogicalForProvider("gpt-6"); ok {
		t.Fatalf("LogicalForProvider must not return the default key, got %q", logical)
	}
	if _, ok := reverseMap(cfg.Forward)["gpt-6"]; ok {
		t.Fatal("reverseMap must skip the default key")
	}
	if got, changed, _ := NormalizeModelID("gpt-6", cfg); changed {
		t.Fatalf("NormalizeModelID mapped a provider ID to %q through the default", got)
	}
}

func TestCovers(t *testing.T) {
	explicit := mustNew(t, map[string]string{"claude-opus-5-5": "gpt-6"})
	withDefault := mustNew(t, map[string]string{"claude-opus-5-5": "gpt-6", DefaultKey: "gpt-6"})
	cases := []struct {
		cfg   Config
		model string
		want  bool
	}{
		{explicit, "claude-opus-5-5", true},
		{explicit, "claude-fable-5-1", false},
		{explicit, "opus", true},
		{explicit, "opus[1m]", true},
		{explicit, "fable", false},
		{explicit, "", true},
		{withDefault, "fable", true},
		{withDefault, "claude-fable-5-1", true},
	}
	for _, tc := range cases {
		if got := tc.cfg.Covers(tc.model); got != tc.want {
			t.Errorf("Covers(%q) with %v = %v, want %v", tc.model, tc.cfg.Forward, got, tc.want)
		}
	}
}

func TestUnmaps(t *testing.T) {
	explicit := mustNew(t, map[string]string{"claude-opus-5-5": "gpt-6"})
	withDefault := mustNew(t, map[string]string{"claude-opus-5-5": "gpt-6", DefaultKey: "gpt-6"})
	keepNames := mustNew(t, map[string]string{"claude-opus-5-5": "gpt-6", DefaultKey: KeepName})
	cases := []struct {
		cfg   Config
		model string
		want  bool
	}{
		{explicit, "claude-fable-5-1", true},
		{explicit, "claude-opus-5-5", false},
		{explicit, "gpt-6", false}, // not a Claude model
		{withDefault, "claude-fable-5-1", false},
		{keepNames, "claude-fable-5-1", false},
		{Config{}, "claude-fable-5-1", false}, // no aliases: names pass by design
	}
	for _, tc := range cases {
		if got := tc.cfg.Unmaps(tc.model); got != tc.want {
			t.Errorf("Unmaps(%q) with %v = %v, want %v", tc.model, tc.cfg.Forward, got, tc.want)
		}
	}
}

// Routes without aliases still report the request's model for the inspector,
// and leave the body intact.
func TestRewriteRequestNotesModelWithoutAliases(t *testing.T) {
	body := `{"model":"claude-opus-5-5","messages":[]}`
	req, _ := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/json")
	ctx, err := RewriteRequest(req, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if ctx.LogicalModel != "claude-opus-5-5" || ctx.UpstreamModel != "claude-opus-5-5" || ctx.Rewritten || ctx.Unmapped {
		t.Fatalf("ctx = %+v", ctx)
	}
	got, _ := io.ReadAll(req.Body)
	if string(got) != body {
		t.Fatalf("body changed: %s", got)
	}
}
