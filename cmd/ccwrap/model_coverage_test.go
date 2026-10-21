package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// doctor flags a gateway route whose aliases miss the model Claude Code
// starts on, with the one-line fix, and passes once a default covers it.
func TestDoctorReportsModelAliasCoverage(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "m.sock")
	_, claudeDir := isolateLaunchEnv(t)
	t.Setenv("ANTHROPIC_MODEL", "")
	writeTestFile(t, filepath.Join(claudeDir, "settings.json"), `{"model":"fable"}`)
	t.Setenv("CCWRAP_UPSTREAM", "https://gw.example/v1")
	t.Setenv("CCWRAP_UPSTREAM_AUTH_TOKEN", "tok")
	t.Setenv(modelalias.EnvAliasesJSON, `{"claude-opus-5-5":"gpt-6"}`)

	c := doctorChecksByName(runDoctorJSON(t, paths, []string{"--json"}))["model_aliases"]
	if c.Status != "warn" || !strings.Contains(c.Summary, "model fable (from user settings) has no alias") || !strings.Contains(c.Detail, "--model-alias '*=gpt-6'") {
		t.Fatalf("uncovered: model_aliases = %#v", c)
	}

	t.Setenv(modelalias.EnvAliasesJSON, `{"claude-opus-5-5":"gpt-6","*":"gpt-6"}`)
	c = doctorChecksByName(runDoctorJSON(t, paths, []string{"--json"}))["model_aliases"]
	if c.Status != "pass" || !strings.Contains(c.Summary, "go to gpt-6") {
		t.Fatalf("default: model_aliases = %#v", c)
	}

	t.Setenv(modelalias.EnvAliasesJSON, `{"claude-fable-5-1":"gpt-6"}`)
	c = doctorChecksByName(runDoctorJSON(t, paths, []string{"--json"}))["model_aliases"]
	if c.Status != "pass" || !strings.Contains(c.Summary, "fable (from user settings) has an alias") {
		t.Fatalf("family covered: model_aliases = %#v", c)
	}
}

func TestModelCoverageLinesNameTheProfileFix(t *testing.T) {
	cfg, err := modelalias.New(map[string]string{"claude-opus-5-5": "gpt-6"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	pre := &preflight.Result{
		ModelAlias:        cfg,
		ActiveProfileName: "enjoy",
		UncoveredModel:    &preflight.UncoveredModel{Model: "fable", Source: "user settings"},
	}
	got := strings.Join(modelCoverageLines(pre), "\n")
	for _, want := range []string{"Claude Code's model fable (from user settings) has no alias", "ccwrap profile edit enjoy --model-alias '*=gpt-6'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if modelCoverageLines(&preflight.Result{}) != nil {
		t.Fatal("a covered model prints nothing")
	}
}

// A gateway base URL ending in /v1 is flagged at launch with the fix.
func TestDoctorReportsDoubledV1Base(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "v.sock")
	isolateLaunchEnv(t)
	t.Setenv("ANTHROPIC_MODEL", "")
	t.Setenv("CCWRAP_UPSTREAM_AUTH_TOKEN", "tok")
	t.Setenv("CCWRAP_UPSTREAM", "https://gw.example/v1")
	c := doctorChecksByName(runDoctorJSON(t, paths, []string{"--json"}))["upstream_path"]
	if c.Status != "warn" || !strings.Contains(c.Detail, "set the base URL to https://gw.example") {
		t.Fatalf("upstream_path = %#v", c)
	}
	t.Setenv("CCWRAP_UPSTREAM", "https://gw.example")
	if c := doctorChecksByName(runDoctorJSON(t, paths, []string{"--json"}))["upstream_path"]; c.Status != "pass" {
		t.Fatalf("upstream_path = %#v", c)
	}
}
