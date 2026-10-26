package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/testutil"
	"github.com/Hoper-J/ccwrap/internal/unmapped"
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
	got := strings.Join(modelCoverageLines(pre, nil), "\n")
	for _, want := range []string{"Claude Code's model fable (from user settings) has no alias", "ccwrap profile edit enjoy --model-alias '*=gpt-6'", "keep them: ccwrap profile edit enjoy --model-alias '*=*'"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if modelCoverageLines(&preflight.Result{}, nil) != nil {
		t.Fatal("a covered model prints nothing")
	}
}

// With aliases to several models, the fix names none of them as the default
// target: it lists them for the user to choose.
func TestAliasFixLinesListSeveralTargets(t *testing.T) {
	cfg, err := modelalias.New(map[string]string{"claude-opus-4-8": "big", "claude-haiku-4-5": "small"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(aliasFixLines(cfg, ""), "\n")
	if !strings.Contains(got, "--model-alias '*=<model>' (this route uses big, small)") || !strings.Contains(got, "--model-alias '*=*'") {
		t.Fatalf("fix lines:\n%s", got)
	}
}

// "*=*" covers Claude Code's model on purpose; doctor says so.
func TestDoctorReportsKeptClaudeNames(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "k.sock")
	_, claudeDir := isolateLaunchEnv(t)
	t.Setenv("ANTHROPIC_MODEL", "")
	writeTestFile(t, filepath.Join(claudeDir, "settings.json"), `{"model":"fable"}`)
	t.Setenv("CCWRAP_UPSTREAM", "https://gw.example")
	t.Setenv("CCWRAP_UPSTREAM_AUTH_TOKEN", "tok")
	t.Setenv(modelalias.EnvAliasesJSON, `{"claude-opus-5-5":"gpt-6","*":"*"}`)
	c := doctorChecksByName(runDoctorJSON(t, paths, []string{"--json"}))["model_aliases"]
	if c.Status != "pass" || !strings.Contains(c.Summary, "keep their Claude names") {
		t.Fatalf("model_aliases = %#v", c)
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

// Claude Code's built-in default model is invisible before launch; once a
// session on the profile sent it with no alias, the next launch says so —
// until the route covers it.
func TestLaunchWarnsAboutRememberedUnaliasedModels(t *testing.T) {
	stateDir := t.TempDir()
	if err := unmapped.Record(stateDir, "deepseek", "claude-opus-5-5", time.Now()); err != nil {
		t.Fatal(err)
	}
	pre := func(forward map[string]string) *preflight.Result {
		cfg, err := modelalias.New(forward, "test", true)
		if err != nil {
			t.Fatal(err)
		}
		return &preflight.Result{ModelAlias: cfg, ActiveProfileName: "deepseek", RouteClass: model.RouteClassThirdPartyHidden}
	}
	got := strings.Join(launchWarningLines(pre(map[string]string{"claude-opus-4-8": "deepseek-v4-flash"}), stateDir), "\n")
	for _, want := range []string{
		"Claude Code sent claude-opus-5-5 with no alias on deepseek before; the upstream got that name as-is.",
		"ccwrap profile edit deepseek --model-alias '*=deepseek-v4-flash'",
		"ccwrap profile edit deepseek --model-alias '*=*'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	for _, forward := range []map[string]string{
		{"claude-opus-4-8": "deepseek-v4-flash", "*": "deepseek-v4-flash"},
		{"claude-opus-4-8": "deepseek-v4-flash", "*": "*"},
		{"claude-opus-5-5": "deepseek-v4-flash"},
	} {
		if lines := launchWarningLines(pre(forward), stateDir); len(lines) != 0 {
			t.Fatalf("%v covers it, got:\n%s", forward, strings.Join(lines, "\n"))
		}
	}
}

func TestUnmappedExitLines(t *testing.T) {
	sess := &model.Session{ActiveProfileName: "deepseek", UnmappedModels: []string{"claude-opus-5-5", "claude-haiku-4-5"},
		ModelAliasForward: map[string]string{"claude-opus-4-8": "deepseek-v4-flash"}}
	got := strings.Join(unmappedExitLines(sess, nil), "\n")
	for _, want := range []string{
		"ccwrap: Claude Code sent claude-opus-5-5, claude-haiku-4-5 with no alias on deepseek; the upstream got those names as-is.",
		"ccwrap profile edit deepseek --model-alias '*=deepseek-v4-flash'",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	if unmappedExitLines(&model.Session{}, nil) != nil {
		t.Fatal("nothing unmapped prints nothing")
	}
	// Models already named at launch are not repeated.
	got = strings.Join(unmappedExitLines(sess, []string{"claude-opus-5-5"}), "\n")
	if strings.Contains(got, "claude-opus-5-5") || !strings.Contains(got, "sent claude-haiku-4-5 with no alias on deepseek; the upstream got that name as-is.") {
		t.Fatalf("skip launch-named models:\n%s", got)
	}
	if unmappedExitLines(sess, sess.UnmappedModels) != nil {
		t.Fatal("all named at launch prints nothing")
	}
}

// doctor reports models a session on the profile sent with no alias before.
func TestDoctorReportsRememberedUnaliasedModels(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "r.sock")
	isolateLaunchEnv(t)
	t.Setenv("ANTHROPIC_MODEL", "")
	path := profiles.DefaultPath(paths.StateDir)
	writeTestFile(t, path, `{"profiles":{"gw":{"provider":"Gateway","base_url":"https://gw.example","auth":{"mode":"ccwrap_bearer","key":"tok"},"model_aliases":{"claude-opus-4-8":"gpt-6"}}}}`)
	if err := unmapped.Record(paths.StateDir, "gw", "claude-opus-5-5", time.Now()); err != nil {
		t.Fatal(err)
	}
	c := doctorChecksByName(runDoctorJSON(t, paths, []string{"--json", "--profile", "gw"}))["model_aliases"]
	if c.Status != "warn" || !strings.Contains(c.Summary, "sent claude-opus-5-5 with no alias on gw before") || !strings.Contains(c.Detail, "ccwrap profile edit gw --model-alias '*=gpt-6'") {
		t.Fatalf("model_aliases = %#v", c)
	}
}
