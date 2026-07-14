package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
	"github.com/Hoper-J/ccwrap/internal/update"
)

func plainPal() ui.Palette { return ui.New(false) }

func envWith(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestUpdateBannerLines(t *testing.T) {
	dir := t.TempDir()
	// No cache → first-run disclosure, and no notice lines.
	full, mark, disc := updateBannerLines(dir, "0.2.0+abc1234", false, envWith(nil), true, plainPal())
	if full != "" || mark != "" {
		t.Fatalf("no cache must not notify: full=%q mark=%q", full, mark)
	}
	if !strings.Contains(disc, "registry.npmjs.org") || !strings.Contains(disc, "CCWRAP_NO_UPDATE_CHECK=1") {
		t.Fatalf("disclosure = %q", disc)
	}
	// Cache with an update → notice line + quiet mark, disclosure gone.
	if err := update.SaveCache(dir, update.Cache{CheckedAt: time.Now(), Latest: "0.3.1"}); err != nil {
		t.Fatal(err)
	}
	full, mark, disc = updateBannerLines(dir, "0.2.0+abc1234", false, envWith(nil), true, plainPal())
	if !strings.Contains(full, "0.2.0+abc1234 → 0.3.1") || !strings.Contains(full, "ccwrap upgrade") ||
		!strings.Contains(full, "releases/tag/v0.3.1") {
		t.Fatalf("full = %q", full)
	}
	if mark != "update 0.3.1↑" {
		t.Fatalf("quiet mark = %q", mark)
	}
	if disc != "" {
		t.Fatalf("disclosure must vanish once cache exists: %q", disc)
	}
	// Gating matrix: disabled / CI / non-TTY / dev build → fully silent.
	for name, call := range map[string]func() (string, string, string){
		"disabled": func() (string, string, string) {
			return updateBannerLines(dir, "0.2.0+abc1234", true, envWith(nil), true, plainPal())
		},
		"ci": func() (string, string, string) {
			return updateBannerLines(dir, "0.2.0+abc1234", false, envWith(map[string]string{"CI": "1"}), true, plainPal())
		},
		"non-tty": func() (string, string, string) {
			return updateBannerLines(dir, "0.2.0+abc1234", false, envWith(nil), false, plainPal())
		},
		"dev build": func() (string, string, string) {
			return updateBannerLines(dir, "0.0.0+abc1234", false, envWith(nil), true, plainPal())
		},
	} {
		if f, m, d := call(); f != "" || m != "" || d != "" {
			t.Fatalf("%s must be fully silent: %q %q %q", name, f, m, d)
		}
	}
	// Already the latest → no notice.
	full, mark, _ = updateBannerLines(dir, "0.3.1", false, envWith(nil), true, plainPal())
	if full != "" || mark != "" {
		t.Fatalf("up-to-date must not notify: %q %q", full, mark)
	}
}

func TestUpdateBannerDisclosureHostOverride(t *testing.T) {
	dir := t.TempDir()
	env := envWith(map[string]string{"CCWRAP_UPDATE_CHECK_URL": "https://user:pass@mirror.example/v"})
	_, _, disc := updateBannerLines(dir, "0.2.0", false, env, true, plainPal())
	if !strings.Contains(disc, "mirror.example") {
		t.Fatalf("disclosure should name override host: %q", disc)
	}
	if strings.Contains(disc, "pass") {
		t.Fatalf("disclosure leaked credentials: %q", disc)
	}
}

func TestStartBackgroundUpdateCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"version":"99.0.0"}`)
	}))
	defer srv.Close()
	t.Setenv("CCWRAP_UPDATE_CHECK_URL", srv.URL)
	dir := t.TempDir()
	fixed := func() time.Time { return time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC) }

	// current is a valid but tiny version, guaranteeing Eligible and
	// Newer are both true.
	h := startBackgroundUpdateCheck(t.Context(), model.EgressConfig{Mode: "direct"}, dir, "0.0.1", false, envWith(map[string]string{"CCWRAP_UPDATE_CHECK_URL": srv.URL}), fixed)
	h.waitDone()
	if got := h.newerVersion(); got != "99.0.0" {
		t.Fatalf("newerVersion = %q", got)
	}
	if c, ok := update.LoadCache(dir); !ok || c.Latest != "99.0.0" || !c.CheckedAt.Equal(fixed().UTC()) {
		t.Fatalf("cache = %+v ok=%v", c, ok)
	}

	// Fresh cache → fully inert: no request, handle done immediately,
	// empty result. (The next launch's banner renders the notice from
	// the cache; the background check only refreshes the facts.)
	h2 := startBackgroundUpdateCheck(t.Context(), model.EgressConfig{Mode: "direct"}, dir, "0.0.1", false, envWith(map[string]string{"CCWRAP_UPDATE_CHECK_URL": srv.URL}), fixed)
	h2.waitDone()
	if h2.newerVersion() != "" {
		t.Fatalf("fresh cache should skip silently, got %q", h2.newerVersion())
	}
}

// TestStartBackgroundUpdateCheckSkips: under disabled / CI the
// background check must be fully inert — handle done immediately, no
// result, no outbound traffic, no cache file written.
func TestStartBackgroundUpdateCheckSkips(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"version":"99.0.0"}`)
	}))
	defer srv.Close()
	now := func() time.Time { return time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC) }
	cases := map[string]struct {
		disabled bool
		env      map[string]string
	}{
		"disabled": {true, map[string]string{"CCWRAP_UPDATE_CHECK_URL": srv.URL}},
		"ci":       {false, map[string]string{"CCWRAP_UPDATE_CHECK_URL": srv.URL, "CI": "1"}},
	}
	for name, tc := range cases {
		dir := t.TempDir()
		h := startBackgroundUpdateCheck(t.Context(), model.EgressConfig{Mode: "direct"}, dir, "0.0.1", tc.disabled, envWith(tc.env), now)
		select {
		case <-h.done:
		default:
			t.Fatalf("%s: handle must be done immediately (skip path closes synchronously)", name)
		}
		if got := h.newerVersion(); got != "" {
			t.Fatalf("%s: newerVersion = %q, want empty", name, got)
		}
		if _, err := os.Stat(filepath.Join(dir, update.CacheFile)); !os.IsNotExist(err) {
			t.Fatalf("%s: skipped check must not write a cache file (stat err=%v)", name, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("skipped checks must not hit the endpoint, got %d hits", hits.Load())
	}
}

func TestPrintUpdateExitNotice(t *testing.T) {
	h := &updateCheckHandle{}
	h.done = make(chan struct{})
	close(h.done)
	h.result.Store(strPtr("0.3.1"))
	var buf bytes.Buffer
	printUpdateExitNotice(&buf, h, "0.2.0", true)
	if !strings.Contains(buf.String(), "0.2.0 → 0.3.1") || !strings.Contains(buf.String(), "ccwrap upgrade") {
		t.Fatalf("exit notice = %q", buf.String())
	}
	buf.Reset()
	printUpdateExitNotice(&buf, h, "0.2.0", false)
	if buf.Len() != 0 {
		t.Fatal("non-TTY exit notice must be silent")
	}
	buf.Reset()
	printUpdateExitNotice(&buf, &updateCheckHandle{}, "0.2.0", true)
	if buf.Len() != 0 {
		t.Fatal("nil result must be silent")
	}
	// Literal nil handle: early-exit launch paths may never create a
	// handle at all — no output, no panic.
	buf.Reset()
	printUpdateExitNotice(&buf, nil, "0.2.0", true)
	if buf.Len() != 0 {
		t.Fatal("nil handle must be silent")
	}
}

func strPtr(s string) *string { return &s }
