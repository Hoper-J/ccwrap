package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/profiletest"
)

func TestParseProfileTestArgs_DefaultsAndPositional(t *testing.T) {
	pos, opts, err := parseProfileTestArgs(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pos != "" {
		t.Errorf("positional default: got %q, want empty", pos)
	}
	if opts.Format != "table" {
		t.Errorf("format default: got %q, want table", opts.Format)
	}
	if opts.Timeout != 15*time.Second {
		t.Errorf("timeout default: got %v, want 15s", opts.Timeout)
	}
	if opts.Model != "" {
		t.Errorf("model default must be empty")
	}
}

func TestParseProfileTestArgs_NameAndFlags(t *testing.T) {
	pos, opts, err := parseProfileTestArgs([]string{"glm", "--timeout", "5s", "--format", "json", "--model", "claude-sonnet-4-5-20251001"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if pos != "glm" {
		t.Errorf("positional: got %q, want glm", pos)
	}
	if !reflect.DeepEqual(opts.Format, "json") {
		t.Errorf("format: got %q, want json", opts.Format)
	}
	if opts.Timeout != 5*time.Second {
		t.Errorf("timeout: got %v, want 5s", opts.Timeout)
	}
	if opts.Model != "claude-sonnet-4-5-20251001" {
		t.Errorf("model: got %q", opts.Model)
	}
}

func TestParseProfileTestArgs_InvalidFormat(t *testing.T) {
	_, _, err := parseProfileTestArgs([]string{"--format", "yaml"})
	if err == nil || !strings.Contains(err.Error(), "format") {
		t.Errorf("expected format error, got %v", err)
	}
}

func TestParseProfileTestArgs_InvalidTimeout(t *testing.T) {
	_, _, err := parseProfileTestArgs([]string{"--timeout", "5xyz"})
	if err == nil || !strings.Contains(err.Error(), "timeout") {
		t.Errorf("expected timeout error, got %v", err)
	}
}

func TestSelectTargets_AllNamed(t *testing.T) {
	f := &profiles.File{
		Profiles: map[string]profiles.Profile{
			"b-named": {Name: "b-named", BaseURL: "http://b"},
			"a-named": {Name: "a-named", BaseURL: "http://a"},
		},
	}
	got, err := selectProfileTestTargets(f, "")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("count: got %d, want 2", len(got))
	}
	if got[0].Name != "a-named" || got[1].Name != "b-named" {
		t.Errorf("ordering: got %q,%q — want a-named,b-named (dict)", got[0].Name, got[1].Name)
	}
}

func TestSelectTargets_SingleByName(t *testing.T) {
	f := &profiles.File{
		Profiles: map[string]profiles.Profile{
			"glm": {Name: "glm", BaseURL: "http://g"},
			"a":   {Name: "a", BaseURL: "http://a"},
		},
	}
	got, err := selectProfileTestTargets(f, "glm")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].Name != "glm" {
		t.Errorf("want single glm; got %+v", got)
	}
}

func TestSelectTargets_UnknownName(t *testing.T) {
	f := &profiles.File{Profiles: map[string]profiles.Profile{"a": {Name: "a"}}}
	_, err := selectProfileTestTargets(f, "nope")
	if err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("expected 'no such profile' error; got %v", err)
	}
}

func TestSelectTargets_NoProfilesJSON(t *testing.T) {
	_, err := selectProfileTestTargets(nil, "")
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "no profiles.json") {
		t.Errorf("expected no-profiles-json error; got %v", err)
	}
}

func TestSelectInheritEnv_HasAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	t.Setenv("ANTHROPIC_BASE_URL", "")
	got, err := selectInheritEnvTarget()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1, got %d", len(got))
	}
	p := got[0]
	if p.Name != "inherit-env" {
		t.Errorf("name: got %q, want inherit-env", p.Name)
	}
	if p.Auth.Mode != "ccwrap_x_api_key" || p.Auth.Key != "sk-test" {
		t.Errorf("auth: got %+v", p.Auth)
	}
	if p.BaseURL != "https://api.anthropic.com" {
		t.Errorf("base_url default: got %q", p.BaseURL)
	}
}

func TestSelectInheritEnv_HasBearer(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-x")
	t.Setenv("ANTHROPIC_BASE_URL", "https://custom.example/anthropic")
	got, err := selectInheritEnvTarget()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	p := got[0]
	if p.Auth.Mode != "ccwrap_bearer" || p.Auth.Key != "tok-x" {
		t.Errorf("auth: got %+v", p.Auth)
	}
	if p.BaseURL != "https://custom.example/anthropic" {
		t.Errorf("base_url: got %q", p.BaseURL)
	}
}

func TestSelectInheritEnv_BothSetPrefersAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-x")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "tok-y")
	got, err := selectInheritEnvTarget()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got[0].Auth.Mode != "ccwrap_x_api_key" {
		t.Errorf("API_KEY must win over AUTH_TOKEN; got %s", got[0].Auth.Mode)
	}
}

func TestSelectInheritEnv_NoEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	_, err := selectInheritEnvTarget()
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "inherit-env") {
		t.Errorf("expected no-credentials error; got %v", err)
	}
}

func TestRenderTable_AllStatusKinds(t *testing.T) {
	results := []profiletest.ProbeResult{
		{Profile: "anthropic", Status: profiletest.StatusOK, Latency: 245 * time.Millisecond, ModelSent: "claude-haiku-4-5-20251001", ModelEchoed: "claude-haiku-4-5-20251001", HTTPStatus: 200},
		{Profile: "glm", Status: profiletest.StatusOK, Latency: 720 * time.Millisecond, ModelSent: "glm-4-flash", ModelSentRewroteFrom: "haiku", ModelEchoed: "glm-4-flash"},
		{Profile: "kimi", Status: profiletest.StatusAuthFail, Latency: 45 * time.Millisecond, ModelSent: "claude-haiku-4-5-20251001", Err: "401 Unauthorized", HTTPStatus: 401},
		{Profile: "oauth", Status: profiletest.StatusSkipped, SkippedReason: "passthrough: CCWRAP does not own credential"},
		{Profile: "local", Status: profiletest.StatusTimeout, ModelSent: "claude-haiku-4-5-20251001", Err: "timeout: ..."},
	}
	var buf bytes.Buffer
	renderProfileTestTable(&buf, results)
	got := buf.String()

	for _, want := range []string{
		"PROFILE", "STATUS", "LATENCY", "MODEL_SENT", "MODEL_ECHOED", "ERR",
		"anthropic", "OK", "245ms",
		"glm", "haiku → glm-4-flash",
		"kimi", "AUTH_FAIL", "45ms", "401",
		"oauth", "SKIPPED", "passthrough",
		"local", "TIMEOUT", "timeout",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderTable_EmptyResults(t *testing.T) {
	var buf bytes.Buffer
	renderProfileTestTable(&buf, nil)
	got := buf.String()
	if !strings.Contains(got, "PROFILE") {
		t.Errorf("header still expected on empty input; got %q", got)
	}
}

func TestRenderJSON_Shape(t *testing.T) {
	results := []profiletest.ProbeResult{
		{Profile: "a", Status: profiletest.StatusOK, Latency: 100 * time.Millisecond, ModelSent: "x", ModelEchoed: "x", HTTPStatus: 200, BaseURLHost: "a.example"},
		{Profile: "b", Status: profiletest.StatusSkipped, BaseURLHost: "b.example", SkippedReason: "passthrough"},
	}
	var buf bytes.Buffer
	if err := renderProfileTestJSON(&buf, results); err != nil {
		t.Fatalf("render: %v", err)
	}
	var got struct {
		TestedAt string `json:"tested_at"`
		Results  []struct {
			Profile              string  `json:"profile"`
			Status               string  `json:"status"`
			LatencyMs            *int64  `json:"latency_ms"`
			HTTPStatus           *int    `json:"http_status"`
			BaseURLHost          string  `json:"base_url_host"`
			ModelSent            *string `json:"model_sent"`
			ModelSentRewrittenTo *string `json:"model_sent_rewritten_to"`
			ModelEchoed          *string `json:"model_echoed"`
			Error                *string `json:"error"`
			SkippedReason        *string `json:"skipped_reason"`
		} `json:"results"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(got.Results) != 2 {
		t.Fatalf("results count: got %d, want 2", len(got.Results))
	}
	if got.Results[0].Status != "OK" {
		t.Errorf("status[0]: got %q", got.Results[0].Status)
	}
	if got.Results[0].LatencyMs == nil || *got.Results[0].LatencyMs != 100 {
		t.Errorf("latency_ms[0]: got %v", got.Results[0].LatencyMs)
	}
	if got.Results[1].LatencyMs != nil {
		t.Errorf("latency_ms[1] must be null for SKIPPED; got %v", *got.Results[1].LatencyMs)
	}
}

func TestRenderTable_NoSecretLeak(t *testing.T) {
	// Even if a probe somehow stuffed a key into Err (it shouldn't),
	// the renderer must not invent extra fields. This test pins the
	// invariant: stdout/stderr never contain sentinel secrets that
	// only existed in the profile, not the ProbeResult.
	results := []profiletest.ProbeResult{
		{Profile: "p", Status: profiletest.StatusOK, Latency: 1 * time.Millisecond, ModelSent: "x", ModelEchoed: "x"},
	}
	var buf bytes.Buffer
	renderProfileTestTable(&buf, results)
	out := buf.String()
	for _, secret := range []string{"sk-fake-must-not-leak", "tok-fake-leak", "user:pass@"} {
		if strings.Contains(out, secret) {
			t.Errorf("table output leaked %q (should never appear)", secret)
		}
	}
}

func TestRenderJSON_NoSecretLeak(t *testing.T) {
	results := []profiletest.ProbeResult{
		{Profile: "p", Status: profiletest.StatusOK, Latency: 1 * time.Millisecond, ModelSent: "x", ModelEchoed: "x"},
	}
	var buf bytes.Buffer
	_ = renderProfileTestJSON(&buf, results)
	out := buf.String()
	for _, secret := range []string{"sk-fake-must-not-leak", "tok-fake-leak", "MY_SECRET_VAR", "user:pass@"} {
		if strings.Contains(out, secret) {
			t.Errorf("JSON output leaked %q", secret)
		}
	}
}

func TestRunProbes_ParallelOrderDeterministic(t *testing.T) {
	// Three probes; we'll force B to be slower than A and C — but the
	// output must still be A, B, C (lexicographic by Name).
	mkSlow := func(d time.Duration) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(d)
			w.WriteHeader(200)
			_, _ = io.WriteString(w, `{"model":"x"}`)
		}))
	}
	a := mkSlow(10 * time.Millisecond)
	b := mkSlow(120 * time.Millisecond)
	c := mkSlow(20 * time.Millisecond)
	defer a.Close()
	defer b.Close()
	defer c.Close()

	targets := []profiles.Profile{
		{Name: "b-slow", BaseURL: b.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}},
		{Name: "a-fast", BaseURL: a.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}},
		{Name: "c-mid", BaseURL: c.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}},
	}
	results := runProfileTestProbes(targets, profiletest.ProbeOptions{Timeout: 3 * time.Second})

	if len(results) != 3 {
		t.Fatalf("count: got %d, want 3", len(results))
	}
	if results[0].Profile != "a-fast" || results[1].Profile != "b-slow" || results[2].Profile != "c-mid" {
		t.Errorf("order: got %q,%q,%q — want a-fast,b-slow,c-mid (dict)", results[0].Profile, results[1].Profile, results[2].Profile)
	}
}

func TestRunProbes_ConcurrencyHappensInParallel(t *testing.T) {
	// Three probes each sleeping 400ms. Serial would take >= 1.2s;
	// parallel lands around one sleep. The 800ms bound sits well below
	// the serial floor while leaving a starved CI runner ~400ms of
	// scheduling slack over the parallel ideal.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.WriteHeader(200)
		_, _ = io.WriteString(w, `{"model":"x"}`)
	}))
	defer srv.Close()

	targets := []profiles.Profile{
		{Name: "a", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}},
		{Name: "b", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}},
		{Name: "c", BaseURL: srv.URL, Auth: &profiles.AuthSpec{Mode: "ccwrap_x_api_key", Key: "k"}},
	}
	start := time.Now()
	_ = runProfileTestProbes(targets, profiletest.ProbeOptions{Timeout: 2 * time.Second})
	elapsed := time.Since(start)
	if elapsed > 800*time.Millisecond {
		t.Errorf("parallel wall-clock too slow: %v (want < 800ms; serial would be >= 1.2s)", elapsed)
	}
}

func TestProfileTestHelp_MentionsCostAndSkipped(t *testing.T) {
	help := profileTestHelpText()
	for _, want := range []string{
		"1-2 tokens",
		"passthrough",
		"SKIPPED",
		"/recent",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("help text missing %q", want)
		}
	}
}

func TestExitCodeForResults(t *testing.T) {
	cases := []struct {
		name    string
		results []profiletest.ProbeResult
		want    int
	}{
		{"all-ok", []profiletest.ProbeResult{
			{Status: profiletest.StatusOK}, {Status: profiletest.StatusOK},
		}, 0},
		{"any-fail", []profiletest.ProbeResult{
			{Status: profiletest.StatusOK}, {Status: profiletest.StatusAuthFail},
		}, 1},
		{"only-skipped", []profiletest.ProbeResult{
			{Status: profiletest.StatusSkipped},
		}, 0},
		{"mix-ok-skipped", []profiletest.ProbeResult{
			{Status: profiletest.StatusOK}, {Status: profiletest.StatusSkipped},
		}, 0},
		{"empty", nil, 0},
		{"timeout", []profiletest.ProbeResult{{Status: profiletest.StatusTimeout}}, 1},
		{"net-fail", []profiletest.ProbeResult{{Status: profiletest.StatusNetFail}}, 1},
		{"model-404", []profiletest.ProbeResult{{Status: profiletest.StatusModel404}}, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCodeForProfileTestResults(c.results); got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}
