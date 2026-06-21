package supervisor

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

func TestParseWebAllowedHosts(t *testing.T) {
	got := parseWebAllowedHosts(" Tunnel.Example.com , dash.example.org:8080 ,, ")
	want := []string{"tunnel.example.com", "dash.example.org"}
	if len(got) != len(want) {
		t.Fatalf("parsed %d entries, want %d: %#v", len(got), len(want), got)
	}
	for _, w := range want {
		if _, ok := got[w]; !ok {
			t.Errorf("missing %q in %#v", w, got)
		}
	}
	if len(parseWebAllowedHosts("")) != 0 {
		t.Error("empty env must parse to an empty allowlist")
	}
}

// TestInfoHostAllowed_Matrix pins the rebinding-guard host policy: loopback
// shapes and *.localhost are admitted, everything else needs the launch-time
// allowlist. Empty Host stays admitted (HTTP/1.0-style local tooling;
// browsers — the rebinding vector — always send Host).
func TestInfoHostAllowed_Matrix(t *testing.T) {
	sp := &sessionProxy{webAllowedHosts: parseWebAllowedHosts("tunnel.example.com")}
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1:54321", true},
		{"127.0.0.1", true},
		{"127.0.0.2:80", true}, // whole 127/8 is loopback
		{"[::1]:54321", true},
		{"::1", true},
		{"localhost:54321", true},
		{"LOCALHOST", true},
		{"dash.localhost:9", true},
		{"", true},
		{"tunnel.example.com", true},      // allowlisted
		{"Tunnel.Example.COM:8443", true}, // case/port-insensitive
		{"evil.example.com", false},       // the rebinding shape
		{"192.168.1.5:54321", false},      // private != loopback
		{"10.0.0.1", false},
		{"localhost.evil.com", false}, // suffix trick: not *.localhost
	}
	for _, tc := range cases {
		if got := sp.infoHostAllowed(tc.host); got != tc.want {
			t.Errorf("infoHostAllowed(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

// TestInfoEndpoints_RefuseRebindingHost runs the guard end-to-end through a
// real session proxy: default Host (loopback) serves, a foreign Host gets 421
// on every info surface (page, /recent, SSE), and CCWRAP_WEB_ALLOWED_HOSTS
// re-admits a deliberate tunnel hostname. CONNECT/forward paths are exempt by
// construction (the guard lives in handleInfoRequest only) — the rest of the
// proxy suite exercises those with target Hosts throughout.
func TestInfoEndpoints_RefuseRebindingHost(t *testing.T) {
	t.Setenv(webAllowedHostsEnv, "tunnel.example.com")
	paths := testutil.ShortAppPaths(t, "s.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)

	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "rebind"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, model.SessionRouteRequest{
		APIBaseURL:        "https://api.example.test",
		RouteSource:       model.RouteSourceExplicit,
		AuthMode:          model.AuthModePassthrough,
		AuthSource:        model.AuthSourceNone,
		ExactUpstreamHost: "api.example.test",
		ExactUpstreamBase: "https://api.example.test",
		FailPolicy:        model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}
	base := "http://" + sess.ProxyListenAddr

	get := func(path, hostOverride string) (*http.Response, string) {
		t.Helper()
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if hostOverride != "" {
			req.Host = hostOverride
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, string(body)
	}

	for _, path := range []string{"/", "/recent", "/healthz"} {
		if resp, _ := get(path, ""); resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s with loopback Host: status %d, want 200", path, resp.StatusCode)
		}
		resp, body := get(path, "evil.example.com")
		if resp.StatusCode != http.StatusMisdirectedRequest {
			t.Fatalf("GET %s with foreign Host: status %d, want 421", path, resp.StatusCode)
		}
		if strings.Contains(body, "evil.example.com") {
			t.Fatalf("421 body must not reflect the hostile Host; got %q", body)
		}
		if !strings.Contains(body, webAllowedHostsEnv) {
			t.Fatalf("421 body should name the escape hatch env; got %q", body)
		}
	}

	// The launch-time allowlist re-admits the deliberate tunnel hostname.
	if resp, _ := get("/healthz", "tunnel.example.com:443"); resp.StatusCode != http.StatusOK {
		t.Fatalf("allowlisted tunnel Host: status %d, want 200", resp.StatusCode)
	}
}
