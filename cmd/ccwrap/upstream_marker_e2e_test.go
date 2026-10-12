package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Opt-in check that nothing ccwrap-specific reaches the request Claude Code
// sends upstream. Claude Code keeps adding request fields derived from local
// state — auto mode's classifier context, for one, reports the directory of
// the --settings file — so a ccwrap path can surface upstream without any
// ccwrap change.
//
// It drives the REAL claude through `ccwrap capture` in your own environment
// (real HOME and credentials; the default profile, or CCWRAP_E2E_PROFILE),
// so it sends one real request upstream:
//
//	CCWRAP_E2E_UPSTREAM_MARKER=1 go test ./cmd/ccwrap -run TestUpstreamRequestHasNoWrapperMarker -v -timeout 10m
//
// Re-run after Claude Code upgrades. Your own config can also trip it (a
// CLAUDE.md, skill or MCP server that names ccwrap); the failure shows the
// surrounding text so you can tell which.
func TestUpstreamRequestHasNoWrapperMarkerE2E(t *testing.T) {
	if os.Getenv("CCWRAP_E2E_UPSTREAM_MARKER") == "" {
		t.Skip("opt-in e2e: set CCWRAP_E2E_UPSTREAM_MARKER=1 (sends one real request through the real claude binary)")
	}
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		t.Skip("claude not on PATH")
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go not on PATH (needed to build ccwrap)")
	}

	ccwrapBin := filepath.Join(t.TempDir(), "ccwrap-e2e")
	if out, err := exec.Command(goBin, "build", "-o", ccwrapBin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build ccwrap: %v\n%s", err, out)
	}
	// Claude Code puts its cwd into the request (system prompt, classifier
	// context): run it from a neutral dir, not this repo or a test temp dir
	// named after the test.
	workDir, err := os.MkdirTemp("", "e2e-upstream-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(workDir)
	if workDir, err = filepath.EvalSymlinks(workDir); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(ccwrapBin, "capture", "--headers", "--main-inference", "--no-response",
		"--timeout", "180s", "--claude-bin", claudeBin, "--", "-p", "Reply with exactly the single word: ok")
	cmd.Dir = workDir
	cmd.Env = os.Environ()
	if p := os.Getenv("CCWRAP_E2E_PROFILE"); p != "" {
		cmd.Env = append(cmd.Env, "CCWRAP_PROFILE="+p)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ccwrap capture: %v\n%s", err, stderr.String())
	}

	var got struct {
		Request struct {
			Headers      map[string][]string `json:"headers"`
			Body         json.RawMessage     `json:"body"`
			BodyEncoding string              `json:"body_encoding"`
		} `json:"request"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse capture output: %v\n%s", err, out)
	}
	if got.Request.BodyEncoding != "json" || len(got.Request.Body) == 0 {
		t.Fatalf("no inspectable JSON request body captured (encoding %q)", got.Request.BodyEncoding)
	}

	assertNoWrapperMarker(t, "request body", string(got.Request.Body))
	for name, values := range got.Request.Headers {
		switch http.CanonicalHeaderKey(name) {
		case "Authorization", "X-Api-Key", "Proxy-Authorization":
			// In placeholder sessions these carry ccwrap's auth placeholder,
			// which the proxy replaces before forwarding.
			continue
		}
		assertNoWrapperMarker(t, "header "+name, strings.Join(values, ", "))
	}

	// Show the field that leaked the runtime path in Claude Code 2.1.287+.
	var body struct {
		Safeguards []struct {
			ClassifierContext struct {
				RuleRoots map[string]string `json:"rule_roots"`
			} `json:"classifier_context"`
		} `json:"safeguards"`
	}
	if json.Unmarshal(got.Request.Body, &body) == nil && len(body.Safeguards) > 0 {
		t.Logf("safeguards rule_roots.flagSettings = %q", body.Safeguards[0].ClassifierContext.RuleRoots["flagSettings"])
	} else {
		t.Log("request carries no auto-mode classifier context (safeguards)")
	}
}

func assertNoWrapperMarker(t *testing.T, where, s string) {
	t.Helper()
	i := strings.Index(strings.ToLower(s), "ccwrap")
	if i < 0 {
		return
	}
	lo, hi := max(i-120, 0), min(i+120, len(s))
	t.Errorf("%s mentions ccwrap upstream: …%s…", where, s[lo:hi])
}
