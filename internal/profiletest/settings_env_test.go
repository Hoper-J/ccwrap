package profiletest

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// claudeSettings points the settings lookup at a temp Claude config dir,
// holding settings.json with body when body is not empty.
func claudeSettings(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	if body != "" {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// Proxy variables from the user's Claude settings are laid over the base
// env, as a session's inherit egress sees them; nothing else is taken.
func TestClaudeSettingsEnv(t *testing.T) {
	claudeSettings(t, `{"env":{"HTTPS_PROXY":"http://127.0.0.1:10800","ANTHROPIC_AUTH_TOKEN":"not-a-proxy-var"}}`)
	env, fromSettings, err := ClaudeSettingsEnv([]string{"HTTP_PROXY=http://from-shell:1", "PATH=/usr/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if !fromSettings || env["HTTPS_PROXY"] != "http://127.0.0.1:10800" || env["HTTP_PROXY"] != "http://from-shell:1" || env["PATH"] != "/usr/bin" {
		t.Fatalf("env = %v, fromSettings = %v", env, fromSettings)
	}
	if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Fatal("a non-proxy settings variable was taken")
	}
}

func TestClaudeSettingsEnvWithoutSettings(t *testing.T) {
	claudeSettings(t, "")
	env, fromSettings, err := ClaudeSettingsEnv([]string{"HTTP_PROXY=http://from-shell:1"})
	if err != nil || fromSettings || len(env) != 1 || env["HTTP_PROXY"] != "http://from-shell:1" {
		t.Fatalf("env = %v, fromSettings = %v, err = %v", env, fromSettings, err)
	}
}

// Malformed settings are an error, not a silent fall back to the process
// env: the test would otherwise take a different path than the session.
func TestClaudeSettingsEnvMalformed(t *testing.T) {
	claudeSettings(t, `{"env":"not an object"}`)
	if _, _, err := ClaudeSettingsEnv(nil); err == nil {
		t.Fatal("malformed settings env gave no error")
	}
}

// fakeProxy answers absolute-form requests for host with body.
func fakeProxy(t *testing.T, host, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Host != host {
			http.Error(w, "unexpected "+r.URL.String(), http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// An inherit egress resolves against the env the caller passes, and the
// result says when that proxy came from Claude settings.
func TestProbeEgress_InheritUsesOptionsEnv(t *testing.T) {
	proxy := fakeProxy(t, "ipinfo.invalid", `{"ip":"203.0.113.7","country":"US"}`)
	opts := EgressProbeOptions{
		Timeout: 2 * time.Second, Target: "http://ipinfo.invalid/json",
		Env: map[string]string{"HTTP_PROXY": proxy.URL, "HTTPS_PROXY": proxy.URL}, EnvFromSettings: true,
	}
	res := ProbeEgress(profiles.Profile{Name: "p"}, opts)
	if res.Status != StatusOK || res.PublicIP != "203.0.113.7" {
		t.Fatalf("status %s ip %q err %q", res.Status, res.PublicIP, res.Err)
	}
	if want := "Claude settings · " + proxy.URL; res.EgressVia != want {
		t.Fatalf("egress_via = %q, want %q", res.EgressVia, want)
	}
	opts.EnvFromSettings = false
	if res := ProbeEgress(profiles.Profile{Name: "p"}, opts); res.EgressVia != "inherit" {
		t.Fatalf("egress_via = %q, want inherit", res.EgressVia)
	}
}

func TestProbe_InheritUsesOptionsEnv(t *testing.T) {
	proxy := fakeProxy(t, "upstream.invalid", `{"type":"message","content":[{"type":"text","text":"pong"}]}`)
	p := profiles.Profile{Name: "gw", BaseURL: "http://upstream.invalid", Auth: &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "k"}}
	res := Probe(p, ProbeOptions{Timeout: 2 * time.Second, Env: map[string]string{"HTTP_PROXY": proxy.URL}})
	if res.Status != StatusOK {
		t.Fatalf("status %s err %q", res.Status, res.Err)
	}
}
