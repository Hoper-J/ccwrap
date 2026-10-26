package modelalias

import (
	"reflect"
	"testing"
)

// "*=*" keeps Claude models without their own alias under their names: no
// rewrite, not reported as unmapped, and it counts as covering any model.
func TestKeepNameDefaultPassesClaudeNamesThrough(t *testing.T) {
	cfg := mustNew(t, map[string]string{"claude-opus-4-8": "gpt-6", DefaultKey: KeepName})
	if !cfg.KeepsNames() || cfg.Default() != "" || !cfg.Covers("claude-opus-5-5") || !cfg.Covers("fable") {
		t.Fatalf("keeps=%v default=%q covers=%v", cfg.KeepsNames(), cfg.Default(), cfg.Covers("claude-opus-5-5"))
	}
	body := []byte(`{"model":"claude-opus-5-5","max_tokens":8,"messages":[]}`)
	out, ctx, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := requestModel(t, out); got != "claude-opus-5-5" || ctx.Rewritten || ctx.Unmapped {
		t.Fatalf("upstream model %q, ctx %+v", got, ctx)
	}
	// The explicit alias still applies.
	out, ctx, err = RewriteJSONRequestBody("/v1/messages", []byte(`{"model":"claude-opus-4-8","messages":[]}`), cfg)
	if err != nil || requestModel(t, out) != "gpt-6" || !ctx.Rewritten {
		t.Fatalf("explicit alias: %s %+v %v", out, ctx, err)
	}
}

// Only the default can keep names; "*" is never sent upstream as a model, and
// a "*=*"-only route does not strip Claude Code's extras.
func TestKeepNameIsOnlyADefault(t *testing.T) {
	if _, err := New(map[string]string{"claude-opus-5-5": KeepName}, "test", true); err == nil {
		t.Fatal(`"claude-opus-5-5": "*" must be rejected`)
	}
	if mustNew(t, map[string]string{DefaultKey: KeepName}).StripsClaudeCodeExtras() {
		t.Fatal(`"*=*" alone must not strip Claude Code's extras`)
	}
	if !mustNew(t, map[string]string{DefaultKey: KeepName, "claude-opus-4-8": "gpt-6"}).StripsClaudeCodeExtras() {
		t.Fatal("an alias to a non-Claude model still strips the extras")
	}
}

func TestSuggestedDefault(t *testing.T) {
	cases := []struct {
		forward    map[string]string
		target     string
		candidates []string
	}{
		{map[string]string{"claude-opus-4-8": "deepseek-v4-flash"}, "deepseek-v4-flash", []string{"deepseek-v4-flash"}},
		{map[string]string{"claude-opus-4-8": "m", "claude-haiku-4-5": "m", DefaultKey: KeepName}, "m", []string{"m"}},
		{map[string]string{"claude-opus-4-8": "big", "claude-haiku-4-5": "small"}, "", []string{"big", "small"}},
		{nil, "", nil},
	}
	for _, tc := range cases {
		cfg := Config{Forward: tc.forward}
		target, candidates := cfg.SuggestedDefault()
		if target != tc.target || !reflect.DeepEqual(candidates, tc.candidates) {
			t.Errorf("%v: got %q %v, want %q %v", tc.forward, target, candidates, tc.target, tc.candidates)
		}
	}
}
