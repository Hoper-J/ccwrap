package update

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

func TestFetchLatest(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.UserAgent()
		fmt.Fprint(w, `{"name":"ccwrap-cli","version":"0.3.1"}`)
	}))
	defer srv.Close()
	client := NewClient(model.EgressConfig{Mode: "direct"}, CheckTimeout)
	v, err := FetchLatest(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("FetchLatest: %v", err)
	}
	if v != "0.3.1" {
		t.Fatalf("version = %q, want 0.3.1", v)
	}
	// The versionless UA is a hard contract: no build metadata leaks
	// to third parties (precedent: ccwrap-egress-probe in
	// internal/profiletest/egress_probe.go).
	if gotUA != "ccwrap-update-check" {
		t.Fatalf("UA = %q, want ccwrap-update-check", gotUA)
	}
}

func TestFetchLatestTrimsLeadingV(t *testing.T) {
	// Self-hosted endpoints may return GitHub-tag-shaped versions
	// ("v0.3.1"). Without stripping the leading "v", "v"+"v0.3.1" is
	// invalid semver and downstream update notices silently never fire.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"v0.3.1"}`)
	}))
	defer srv.Close()
	client := NewClient(model.EgressConfig{Mode: "direct"}, CheckTimeout)
	v, err := FetchLatest(context.Background(), client, srv.URL)
	if err != nil {
		t.Fatalf("FetchLatest: %v", err)
	}
	if v != "0.3.1" {
		t.Fatalf("version = %q, want 0.3.1", v)
	}
}

func TestFetchLatestErrors(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"http 500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }},
		{"bad json", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "{oops") }},
		{"invalid version", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"version":"banana"}`) }},
		{"empty version", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{"version":""}`) }},
		{"oversized body", func(w http.ResponseWriter, r *http.Request) {
			// a response past the 1 MiB cap is truncated and must then fail to parse
			fmt.Fprint(w, `{"version":"0.3.1","pad":"`+strings.Repeat("x", 2<<20)+`"}`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			client := NewClient(model.EgressConfig{Mode: "direct"}, CheckTimeout)
			if _, err := FetchLatest(context.Background(), client, srv.URL); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

// TestFetchLatestInvalidVersionErrorTruncated: the error message
// reaches the terminal and banner paths and must not be flooded by a
// hostile/broken endpoint's oversized "version" value — the quoted
// value is truncated to 64 characters.
func TestFetchLatestInvalidVersionErrorTruncated(t *testing.T) {
	long := strings.Repeat("A", 1000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"version":%q}`, long)
	}))
	defer srv.Close()
	client := NewClient(model.EgressConfig{Mode: "direct"}, CheckTimeout)
	_, err := FetchLatest(context.Background(), client, srv.URL)
	if err == nil {
		t.Fatal("invalid version must error")
	}
	if len(err.Error()) >= 300 {
		t.Fatalf("error must truncate hostile version value, len=%d: %.120s…", len(err.Error()), err.Error())
	}
}

func TestNewClientHonorsEgressProxy(t *testing.T) {
	// Fake forward proxy: answers any absolute-URI request directly.
	// Successfully fetching a version from an unresolvable host proves
	// the traffic really went through the egress proxy (an update check
	// that bypasses egress and dials direct is unacceptable behavior
	// for this project).
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.RequestURI, "http://") {
			t.Errorf("expected absolute-URI proxy request, got %q", r.RequestURI)
		}
		fmt.Fprint(w, `{"version":"0.3.1"}`)
	}))
	defer proxy.Close()
	cfg := model.EgressConfig{Mode: "http", HTTPProxy: proxy.URL, HTTPSProxy: proxy.URL}
	client := NewClient(cfg, CheckTimeout)
	v, err := FetchLatest(context.Background(), client, "http://ccwrap-update-check.invalid/latest")
	if err != nil {
		t.Fatalf("FetchLatest via proxy: %v", err)
	}
	if v != "0.3.1" {
		t.Fatalf("version = %q", v)
	}
}

func TestNewClientTransportShape(t *testing.T) {
	// Structural regression pin: NewClient must replicate the either-or
	// of probe.go's buildEgressAwareHTTPClient — SOCKS goes through
	// DialContext (http.Transport.Proxy does not speak SOCKS), otherwise
	// only Proxy is set. With both set, the transport would use the
	// egress DialContext to dial "the proxy itself", so an HTTP proxy on
	// a public address would be asked to CONNECT to itself; that failure
	// mode requires a public proxy and cannot be reproduced hermetically,
	// hence pinning the structure.
	socksCfg := model.EgressConfig{Mode: "socks5", HTTPSProxy: "socks5://127.0.0.1:1080", HTTPProxy: "socks5://127.0.0.1:1080"}
	st, ok := NewClient(socksCfg, CheckTimeout).Transport.(*http.Transport)
	if !ok {
		t.Fatal("socks: Transport is not *http.Transport")
	}
	if st.DialContext == nil || st.Proxy != nil {
		t.Fatalf("socks: DialContext set=%v Proxy set=%v, want DialContext only", st.DialContext != nil, st.Proxy != nil)
	}
	if !st.DisableKeepAlives {
		t.Fatal("socks: DisableKeepAlives = false, want true")
	}

	httpCfg := model.EgressConfig{Mode: "http", HTTPProxy: "http://127.0.0.1:8080", HTTPSProxy: "http://127.0.0.1:8080"}
	ht, ok := NewClient(httpCfg, CheckTimeout).Transport.(*http.Transport)
	if !ok {
		t.Fatal("http: Transport is not *http.Transport")
	}
	if ht.Proxy == nil || ht.DialContext != nil {
		t.Fatalf("http: Proxy set=%v DialContext set=%v, want Proxy only", ht.Proxy != nil, ht.DialContext != nil)
	}
	if !ht.DisableKeepAlives {
		t.Fatal("http: DisableKeepAlives = false, want true")
	}
}

func TestCheckURL(t *testing.T) {
	if got := CheckURL(mapGetenv(nil)); got != DefaultCheckURL {
		t.Fatalf("default = %q", got)
	}
	if got := CheckURL(mapGetenv(map[string]string{"CCWRAP_UPDATE_CHECK_URL": "https://mirror.example/v"})); got != "https://mirror.example/v" {
		t.Fatalf("override = %q", got)
	}
}
