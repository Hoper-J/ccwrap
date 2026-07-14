package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/Hoper-J/ccwrap/internal/egress"
	"github.com/Hoper-J/ccwrap/internal/model"
)

// DefaultCheckURL is npm's per-dist-tag endpoint. npm over the GitHub
// API for two reasons: registry.npmjs.org is far more reachable for the
// project's primary (CN-network) audience, and the `latest` dist-tag
// only moves on stable releases — prereleases publish to `next` (see
// release.yml), so users are never nagged about a prerelease. Versions
// are lockstep across npm and GitHub releases (both cut from the same
// tag), so either source is authoritative.
const DefaultCheckURL = "https://registry.npmjs.org/ccwrap-cli/latest"

// CheckTimeout caps one version check end-to-end — same budget as the
// egress probe.
const CheckTimeout = 5 * time.Second

// checkUserAgent is versionless BY DESIGN: it identifies probe traffic
// in third-party logs without leaking ccwrap build metadata (precedent:
// ccwrap-egress-probe).
const checkUserAgent = "ccwrap-update-check"

// maxCheckBody bounds how much of the response we read. npm's /latest
// payload is a few KB; anything past 1 MiB is a hostile or broken
// endpoint.
const maxCheckBody = 1 << 20

// CheckURL resolves the version-discovery endpoint. The override exists
// for self-hosted mirrors in privacy-strict or GitHub/npm-blocked
// environments — same posture as CCWRAP_EGRESS_TEST_URL. Contract: the
// endpoint returns JSON with a top-level "version" field (npm's /latest
// response is natively compatible).
func CheckURL(getenv func(string) string) string {
	if v := strings.TrimSpace(getenv("CCWRAP_UPDATE_CHECK_URL")); v != "" {
		return v
	}
	return DefaultCheckURL
}

// NewClient builds an HTTP client that dials through the resolved
// egress config — update traffic must exit exactly where the session's
// traffic exits; a check that bypasses the user's egress proxy would
// betray the tool's whole premise. Construction mirrors
// buildEgressAwareHTTPClient (probe.go): SOCKS goes through DialContext
// because http.Transport.Proxy only speaks HTTP/HTTPS proxies;
// otherwise only Proxy is set, so an HTTP egress proxy is never dialed
// through itself. timeout 0 means no client-level cap (the caller's
// context governs; used by the larger `upgrade` download).
func NewClient(cfg model.EgressConfig, timeout time.Duration) *http.Client {
	transport := &http.Transport{
		DisableKeepAlives: true,
	}
	if egress.IsSOCKSEgress(cfg) {
		transport.DialContext = egress.DialContextFunc(cfg)
	} else {
		transport.Proxy = egress.ProxyFunc(cfg)
	}
	return &http.Client{Timeout: timeout, Transport: transport}
}

// FetchLatest performs one plain GET — no query params, no
// machine/version identifiers — and returns the endpoint's version.
func FetchLatest(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", checkUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("update check: unexpected status %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxCheckBody))
	if err != nil {
		return "", err
	}
	var payload struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("update check: parse response: %w", err)
	}
	v := strings.TrimSpace(payload.Version)
	// Tolerate GitHub-tag-shaped versions ("v0.3.1") from self-hosted
	// endpoints — without this, "v"+"v0.3.1" is invalid semver and
	// updates silently never fire downstream.
	v = strings.TrimPrefix(v, "v")
	if !semver.IsValid("v" + v) {
		// The error reaches the terminal; truncate the quoted value so
		// a hostile/broken endpoint's oversized string cannot flood it.
		v64 := payload.Version
		if len(v64) > 64 {
			v64 = v64[:64] + "…"
		}
		return "", fmt.Errorf("update check: endpoint returned invalid version %q", v64)
	}
	return v, nil
}
