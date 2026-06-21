package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/profiletest"
)

func TestParseProfileTestEgressArgs_Defaults(t *testing.T) {
	name, opts, err := parseProfileTestEgressArgs(nil)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if name != "" {
		t.Errorf("name: want \"\", got %q", name)
	}
	if opts.Format != "table" {
		t.Errorf("format: want table, got %q", opts.Format)
	}
	if opts.Timeout != 5*time.Second {
		t.Errorf("timeout: want 5s, got %v", opts.Timeout)
	}
	if opts.Target != "" {
		t.Errorf("target: want empty, got %q", opts.Target)
	}
}

func TestParseProfileTestEgressArgs_PositionalName(t *testing.T) {
	name, _, err := parseProfileTestEgressArgs([]string{"gateway"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if name != "gateway" {
		t.Errorf("name: want gateway, got %q", name)
	}
}

func TestParseProfileTestEgressArgs_AllFlags(t *testing.T) {
	name, opts, err := parseProfileTestEgressArgs([]string{
		"gateway", "--timeout", "10s", "--format", "json", "--target", "https://example.test/ip",
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if name != "gateway" {
		t.Errorf("name: %q", name)
	}
	if opts.Timeout != 10*time.Second {
		t.Errorf("timeout: %v", opts.Timeout)
	}
	if opts.Format != "json" {
		t.Errorf("format: %q", opts.Format)
	}
	if opts.Target != "https://example.test/ip" {
		t.Errorf("target: %q", opts.Target)
	}
}

func TestParseProfileTestEgressArgs_HelpFlag(t *testing.T) {
	_, _, err := parseProfileTestEgressArgs([]string{"--help"})
	if err != errHelpRequested {
		t.Fatalf("want errHelpRequested, got %v", err)
	}
}

func TestParseProfileTestEgressArgs_BadFormat(t *testing.T) {
	_, _, err := parseProfileTestEgressArgs([]string{"--format", "yaml"})
	if err == nil {
		t.Fatal("want error for invalid format")
	}
	if !strings.Contains(err.Error(), "table or json") {
		t.Errorf("err message should hint at valid formats, got %v", err)
	}
}

func TestParseProfileTestEgressArgs_NonPositiveTimeout(t *testing.T) {
	_, _, err := parseProfileTestEgressArgs([]string{"--timeout", "0s"})
	if err == nil {
		t.Fatal("want error for zero timeout")
	}
}

func TestSelectEgressProbeTargets_AllProfiles(t *testing.T) {
	file := &profiles.File{
		Profiles: map[string]profiles.Profile{
			"alpha": {Name: "alpha"},
			"beta":  {Name: "beta"},
		},
	}
	out, err := selectEgressProbeTargets(file, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 profiles, got %d", len(out))
	}
	if out[0].Name != "alpha" || out[1].Name != "beta" {
		t.Errorf("not sorted: %v", out)
	}
}

func TestSelectEgressProbeTargets_SingleByName(t *testing.T) {
	file := &profiles.File{
		Profiles: map[string]profiles.Profile{"gateway": {Name: "gateway"}},
	}
	out, err := selectEgressProbeTargets(file, "gateway")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 || out[0].Name != "gateway" {
		t.Fatalf("got %v", out)
	}
}

func TestSelectEgressProbeTargets_UnknownName(t *testing.T) {
	file := &profiles.File{Profiles: map[string]profiles.Profile{"a": {}}}
	_, err := selectEgressProbeTargets(file, "missing")
	if err == nil {
		t.Fatal("want error for unknown profile")
	}
}

func TestSelectEgressProbeTargets_NoFile(t *testing.T) {
	_, err := selectEgressProbeTargets(nil, "")
	if err == nil {
		t.Fatal("want error when no profiles.json and no name")
	}
}

func TestSelectEgressProbeTargets_InheritEnv(t *testing.T) {
	out, err := selectEgressProbeTargets(nil, "inherit-env")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(out) != 1 || out[0].Name != "inherit-env" {
		t.Fatalf("got %v", out)
	}
}

func TestRenderEgressTestTable_HappyAndFailureRows(t *testing.T) {
	results := []profiletest.EgressProbeResult{
		{
			Profile: "gateway", Status: profiletest.StatusOK, LatencyMs: 142,
			EgressVia: "socks5h://corp:1080", PublicIP: "1.2.3.4",
			City: "Seattle", Region: "WA", Country: "US",
			Org: "AS1 Acme",
		},
		{
			Profile: "broken", Status: profiletest.StatusNetFail, LatencyMs: 32,
			EgressVia: "socks5h://nope:1081",
			Err:       "dial tcp nope:1081: connect refused",
		},
	}
	var buf bytes.Buffer
	renderEgressTestTable(&buf, results)
	out := buf.String()
	if !strings.Contains(out, "PROFILE") || !strings.Contains(out, "STATUS") {
		t.Errorf("header missing: %q", out)
	}
	if !strings.Contains(out, "gateway") || !strings.Contains(out, "OK") {
		t.Errorf("happy row missing: %q", out)
	}
	if !strings.Contains(out, "NET_FAIL") || !strings.Contains(out, "broken") {
		t.Errorf("fail row missing: %q", out)
	}
	if !strings.Contains(out, "Seattle, WA, US") {
		t.Errorf("geo not formatted: %q", out)
	}
}

func TestRenderEgressTestTable_TruncatesLongError(t *testing.T) {
	long := strings.Repeat("x", 200)
	results := []profiletest.EgressProbeResult{
		{Profile: "p", Status: profiletest.StatusNetFail, Err: long},
	}
	var buf bytes.Buffer
	renderEgressTestTable(&buf, results)
	// the err column must be truncated to <= 80 chars in the rendered output
	lines := strings.Split(buf.String(), "\n")
	for _, line := range lines {
		if strings.Contains(line, "xxx") && strings.Count(line, "x") > 80 {
			t.Fatalf("err column not truncated to 80 chars: %d x's", strings.Count(line, "x"))
		}
	}
}

// TestRenderEgressTestTable_TruncatesByRune — error messages containing
// multi-byte UTF-8 codepoints must truncate at rune boundaries, not
// byte boundaries. A byte slice on "中" (3 bytes per char) at index 79
// would emit half a rune and corrupt terminal rendering.
func TestRenderEgressTestTable_TruncatesByRune(t *testing.T) {
	// 100 copies of "中" = 100 runes, 300 bytes — well beyond the
	// 80-rune column max in either dimension.
	long := strings.Repeat("中", 100)
	results := []profiletest.EgressProbeResult{
		{Profile: "p", Status: profiletest.StatusNetFail, Err: long},
	}
	var buf bytes.Buffer
	renderEgressTestTable(&buf, results)
	out := buf.String()
	// Output must be valid UTF-8 — a byte-slice truncation would
	// produce invalid UTF-8 at the cut point.
	if !utf8.ValidString(out) {
		t.Fatalf("rendered output is not valid UTF-8 (byte-slice truncation bug)")
	}
	// And the truncation must end with the ellipsis marker.
	if !strings.Contains(out, "…") {
		t.Fatal("expected ellipsis after truncation, not present")
	}
}

func TestRenderEgressTestJSON_RoundTrip(t *testing.T) {
	results := []profiletest.EgressProbeResult{
		{Profile: "p", Status: profiletest.StatusOK, LatencyMs: 80, PublicIP: "1.1.1.1", Country: "US"},
	}
	var buf bytes.Buffer
	if err := renderEgressTestJSON(&buf, results); err != nil {
		t.Fatalf("render: %v", err)
	}
	// Decode into a wire-shaped struct (Status is serialized as a
	// string on the wire). EgressProbeResult.MarshalJSON renders Status as the
	// string form ("OK") rather than the underlying int — callers in
	// other languages / the browser consume strings, not enum ints.
	type wireResult struct {
		Profile   string `json:"profile"`
		Status    string `json:"status"`
		LatencyMs int64  `json:"latency_ms"`
		PublicIP  string `json:"public_ip"`
		Country   string `json:"country"`
		EgressVia string `json:"egress_via"`
		Target    string `json:"target"`
	}
	var doc struct {
		TestedAt string       `json:"tested_at"`
		Results  []wireResult `json:"results"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, buf.String())
	}
	if doc.TestedAt == "" {
		t.Error("tested_at missing")
	}
	if len(doc.Results) != 1 {
		t.Fatalf("results len: got %d, want 1", len(doc.Results))
	}
	if doc.Results[0].Status != "OK" {
		t.Errorf("status: got %q, want OK (string, not int)", doc.Results[0].Status)
	}
	if doc.Results[0].PublicIP != "1.1.1.1" {
		t.Errorf("round-trip mismatch: %+v", doc.Results)
	}
}

// TestRunEgressProbes_BoundsConcurrency — when the number of probe
// targets exceeds maxEgressProbeWorkers, the runner must never have
// more than that many probes in flight simultaneously. Verified by
// instrumenting the probe target with a counter that records the peak
// concurrent in-flight count.
func TestRunEgressProbes_BoundsConcurrency(t *testing.T) {
	var inflight int32
	var peak int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := atomic.AddInt32(&inflight, 1)
		defer atomic.AddInt32(&inflight, -1)
		// Update peak if this request bumped the count.
		for {
			old := atomic.LoadInt32(&peak)
			if cur <= old || atomic.CompareAndSwapInt32(&peak, old, cur) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ip":"1.2.3.4"}`))
	}))
	defer srv.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", srv.URL)

	// 24 targets ≫ 8 workers: pool must clamp peak parallelism.
	targets := make([]profiles.Profile, 24)
	for i := range targets {
		targets[i] = profiles.Profile{
			Name:   fmt.Sprintf("p%02d", i),
			Egress: profiles.EgressSpec{Mode: "direct"},
		}
	}
	results := runEgressProbes(targets, profiletest.EgressProbeOptions{Timeout: 2 * time.Second})
	if len(results) != len(targets) {
		t.Fatalf("results len: got %d, want %d", len(results), len(targets))
	}
	if got := atomic.LoadInt32(&peak); int(got) > maxEgressProbeWorkers {
		t.Fatalf("peak concurrency %d exceeds cap %d", got, maxEgressProbeWorkers)
	}
}

func TestExitCodeForEgressTestResults(t *testing.T) {
	if c := exitCodeForEgressTestResults([]profiletest.EgressProbeResult{{Status: profiletest.StatusOK}}); c != 0 {
		t.Errorf("OK → want 0, got %d", c)
	}
	if c := exitCodeForEgressTestResults([]profiletest.EgressProbeResult{{Status: profiletest.StatusNetFail}}); c != 1 {
		t.Errorf("NetFail → want 1, got %d", c)
	}
	if c := exitCodeForEgressTestResults([]profiletest.EgressProbeResult{{Status: profiletest.StatusOK}, {Status: profiletest.StatusTimeout}}); c != 1 {
		t.Errorf("any fail → want 1, got %d", c)
	}
}

func TestRunProfileTestEgress_E2E_TableFormat(t *testing.T) {
	// Stub the probe target
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Millisecond) // ensure LatencyMs > 0 on fast loopback
		_, _ = w.Write([]byte(`{"ip":"9.9.9.9","country":"US","city":"Seattle","region":"WA"}`))
	}))
	defer srv.Close()
	t.Setenv("CCWRAP_EGRESS_TEST_URL", srv.URL)

	// Set up a temporary profiles.json
	tmp := t.TempDir()
	profilesPath := filepath.Join(tmp, "profiles.json")
	jsonBody := `{
		"default": "gw",
		"profiles": {
			"gw": {
				"base_url": "https://api.anthropic.com",
				"egress": {"mode": "direct"}
			}
		}
	}`
	if err := os.WriteFile(profilesPath, []byte(jsonBody), 0600); err != nil {
		t.Fatalf("write profiles.json: %v", err)
	}

	// Capture stdout
	var stdoutBuf bytes.Buffer
	origOut := stdOut
	stdOut = func() io.Writer { return &stdoutBuf }
	defer func() { stdOut = origOut }()

	paths := app.Paths{StateDir: tmp}
	code := runProfileTestEgress(paths, []string{"gw"})
	if code != 0 {
		t.Fatalf("exit code: want 0, got %d (stdout=%s)", code, stdoutBuf.String())
	}
	if !strings.Contains(stdoutBuf.String(), "9.9.9.9") {
		t.Errorf("output should contain probed IP: %s", stdoutBuf.String())
	}
}
