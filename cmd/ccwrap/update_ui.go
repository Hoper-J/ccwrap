// cmd/ccwrap/update_ui.go
package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
	"github.com/Hoper-J/ccwrap/internal/update"
)

// updateNoticeLine is the one shared piece of copy across banner and
// exit notice: what changed, the one action, where to read more.
func updateNoticeLine(pal ui.Palette, current, latest string) string {
	return fmt.Sprintf("%s %s → %s available · run %s · https://github.com/Hoper-J/ccwrap/releases/tag/v%s",
		pal.Dim("update:"), current, latest, pal.Bold("ccwrap upgrade"), latest)
}

// checkHost renders the version-endpoint host for the first-run
// disclosure. Credentials in an override URL must never surface.
func checkHost(getenv func(string) string) string {
	raw := update.CheckURL(getenv)
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return "the configured update endpoint"
}

// updateBannerLines computes the banner's update-related lines from the
// CACHE ONLY — the launch path never touches the network. Returns:
// full (its own stderr line in normal mode), quietMark (appended to the
// one-line quiet banner), disclosure (one-time first-check notice).
func updateBannerLines(stateDir, current string, disabled bool, getenv func(string) string, isTTY bool, pal ui.Palette) (full, quietMark, disclosure string) {
	if disabled || update.InCI(getenv) || !isTTY {
		return "", "", ""
	}
	c, ok := update.LoadCache(stateDir)
	if !ok {
		// No cache == this machine's first check is about to happen:
		// this is the user's only chance to learn about the outbound
		// traffic, so it prints in quiet mode too (the one-time cost
		// outranks the quiet contract).
		disclosure = pal.Dim(fmt.Sprintf("update check: at most daily via %s — disable with CCWRAP_NO_UPDATE_CHECK=1", checkHost(getenv)))
		return "", "", disclosure
	}
	if update.Eligible(current) && update.Newer(current, c.Latest) {
		full = updateNoticeLine(pal, current, c.Latest)
		quietMark = "update " + c.Latest + "↑"
	}
	return full, quietMark, ""
}

// updateCheckHandle carries the background check's outcome across the
// session. done is closed when the goroutine finishes OR when the check
// was skipped — tests wait on it; runClaude never does (exit must not
// block on a straggling check).
type updateCheckHandle struct {
	result atomic.Pointer[string]
	done   chan struct{}
}

func (h *updateCheckHandle) newerVersion() string {
	if p := h.result.Load(); p != nil {
		return *p
	}
	return ""
}

func (h *updateCheckHandle) waitDone() {
	if h.done != nil {
		<-h.done
	}
}

// startBackgroundUpdateCheck runs the once-daily version check off the
// launch path. Failures are fully silent by design — there is no debug
// log facility in cmd/ccwrap, and `ccwrap doctor` exposes cache age for
// observability instead.
func startBackgroundUpdateCheck(ctx context.Context, cfg model.EgressConfig, stateDir, current string, disabled bool, getenv func(string) string, now func() time.Time) *updateCheckHandle {
	h := &updateCheckHandle{done: make(chan struct{})}
	c, ok := update.LoadCache(stateDir)
	if disabled || update.InCI(getenv) || !update.Due(c, ok, now()) {
		close(h.done)
		return h
	}
	go func() {
		defer close(h.done)
		defer func() { _ = recover() }() // the update machinery must never break a session
		cctx, cancel := context.WithTimeout(ctx, update.CheckTimeout)
		defer cancel()
		latest, err := update.FetchLatest(cctx, update.NewClient(cfg, update.CheckTimeout), update.CheckURL(getenv))
		if err != nil {
			return
		}
		_ = update.SaveCache(stateDir, update.Cache{CheckedAt: now().UTC(), Latest: latest})
		if update.Eligible(current) && update.Newer(current, latest) {
			h.result.Store(&latest)
		}
	}()
	return h
}

// printUpdateExitNotice complements the banner: the banner shows LAST
// session's finding on the next launch; this shows THIS session's
// finding the moment ccwrap gets the terminal back. Non-blocking read —
// a still-running check simply prints nothing.
func printUpdateExitNotice(w io.Writer, h *updateCheckHandle, current string, isTTY bool) {
	if h == nil || !isTTY {
		return
	}
	latest := h.newerVersion()
	if latest == "" {
		return
	}
	pal := ui.New(isTTY)
	fmt.Fprintln(w, updateNoticeLine(pal, current, latest))
}
