package preflight

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/settings"
)

func inspectWithUserModel(t *testing.T, modelSetting string) *settings.InspectionResult {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	t.Setenv("CCWRAP_MANAGED_SETTINGS_DIR", t.TempDir())
	if modelSetting != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(`{"model":"`+modelSetting+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	inspect, err := settings.InspectLaunch(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return inspect
}

// Claude Code's own precedence: --model, then ANTHROPIC_MODEL, then settings.
func TestClaudeMainModelPrecedence(t *testing.T) {
	inspect := inspectWithUserModel(t, "fable")
	cases := []struct {
		args          []string
		env           map[string]string
		value, source string
	}{
		{nil, nil, "fable", "user settings"},
		{nil, map[string]string{"ANTHROPIC_MODEL": "sonnet"}, "sonnet", "ANTHROPIC_MODEL"},
		{[]string{"--model", "opus"}, map[string]string{"ANTHROPIC_MODEL": "sonnet"}, "opus", "--model"},
		{[]string{"--model=haiku", "-p", "hi"}, nil, "haiku", "--model"},
		// Anything after "--" is not a flag.
		{[]string{"--", "--model", "opus"}, nil, "fable", "user settings"},
	}
	for _, tc := range cases {
		v, src := ClaudeMainModel(tc.args, tc.env, inspect)
		if v != tc.value || src != tc.source {
			t.Errorf("args=%v env=%v: got (%q, %q), want (%q, %q)", tc.args, tc.env, v, src, tc.value, tc.source)
		}
	}
}

func TestUncoveredMainModel(t *testing.T) {
	inspect := inspectWithUserModel(t, "fable")
	opusOnly, _ := modelalias.New(map[string]string{"claude-opus-5-5": "gpt-6"}, "t", true)
	withDefault, _ := modelalias.New(map[string]string{"claude-opus-5-5": "gpt-6", modelalias.DefaultKey: "gpt-6"}, "t", true)
	got := uncoveredMainModel(model.RouteClassThirdPartyHidden, opusOnly, nil, nil, inspect)
	if got == nil || got.Model != "fable" || got.Source != "user settings" {
		t.Fatalf("uncovered = %+v", got)
	}
	if uncoveredMainModel(model.RouteClassThirdPartyHidden, withDefault, nil, nil, inspect) != nil {
		t.Fatal("a default covers every Claude model")
	}
	if uncoveredMainModel(model.RouteClassFirstParty, opusOnly, nil, nil, inspect) != nil {
		t.Fatal("first-party routes serve every Claude model")
	}
	if uncoveredMainModel(model.RouteClassThirdPartyHidden, modelalias.Config{}, nil, nil, inspect) != nil {
		t.Fatal("a route without aliases passes names through by design")
	}
	if uncoveredMainModel(model.RouteClassThirdPartyHidden, opusOnly, []string{"--model", "opus"}, nil, inspect) != nil {
		t.Fatal("opus is covered by the opus alias")
	}
}

func TestDoubledV1Base(t *testing.T) {
	cases := []struct {
		in, fixed string
		ok        bool
	}{
		{"https://gw.example/v1", "https://gw.example", true},
		{"https://gw.example/v1/", "https://gw.example", true},
		{"https://user:pass@gw.example/api/v1", "https://gw.example/api", true},
		{"https://gw.example", "", false},
		{"https://api.deepseek.com/anthropic", "", false},
		{"https://gw.example/v10", "", false},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		fixed, ok := DoubledV1Base(u)
		if fixed != tc.fixed || ok != tc.ok {
			t.Errorf("%s: got (%q, %v), want (%q, %v)", tc.in, fixed, ok, tc.fixed, tc.ok)
		}
	}
	if _, ok := DoubledV1Base(nil); ok {
		t.Fatal("nil base")
	}
}
