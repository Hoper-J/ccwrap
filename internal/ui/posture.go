package ui

import (
	"fmt"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// nowFunc is overridable in tests so the relative-age tail is
// deterministic; production uses the real clock.
var nowFunc = time.Now

// humanAge renders a coarse "<n><unit> ago" (e.g. "8s ago").
// Mirrors the dashboard's age vocabulary but lives here so ui has
// no dependency on internal/dashboard.
func humanAge(ts time.Time) string {
	d := nowFunc().Sub(ts)
	if d < 0 {
		d = 0 // guard clock skew: never render a negative "-Ns ago"
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// SessionPosture returns the one-line plain-English hero summary
// rendered identically by every session-bearing surface. Wire/enum
// strings never appear here. lastErr may be nil; when nil the
// "Last: …" tail is omitted (faithful degradation for surfaces that
// do not carry the error list, e.g. ccwrap status).
func SessionPosture(sess *model.Session, lastErr *model.ErrorRecord) string {
	if sess == nil {
		return ""
	}
	if sess.State == model.StateEnded {
		// model.Session has no close-reason field, so degrade to
		// the reasonless form. Do not invent a reason.
		return "Session closed. Final state preserved below."
	}
	host := sess.ExactUpstreamHost
	if host == "" {
		host = "api.anthropic.com"
	}
	var base string
	switch sess.RouteClass {
	case model.RouteClassThirdPartyHidden, model.RouteClassThirdPartyCompatible:
		aliases := "aliases"
		if sess.ModelAliasCount == 1 {
			aliases = "alias"
		}
		base = fmt.Sprintf(
			"Routing Claude Code through %s. CCWRAP holds your gateway credentials and applies %d model %s; Claude Code only sees logical names.",
			host, sess.ModelAliasCount, aliases)
	default: // first_party / unset
		if sess.AuthPolicy == model.AuthPolicyCCWRAPOverride || sess.AuthPolicy == model.AuthPolicyCCWRAPOverrideFailClosed {
			base = fmt.Sprintf("Routing Claude Code through %s with CCWRAP-owned auth.", host)
		} else {
			base = fmt.Sprintf("Routing Claude Code through %s with your local credentials.", host)
		}
	}
	if sess.RecentErrorCount > 0 {
		noun := "errors"
		if sess.RecentErrorCount == 1 {
			noun = "error"
		}
		base += fmt.Sprintf(" — %d %s recorded since launch.", sess.RecentErrorCount, noun)
		if lastErr != nil && lastErr.Summary != "" {
			base += fmt.Sprintf(" Last: %s %s.", lastErr.Summary, humanAge(lastErr.Timestamp))
		}
	}
	return base
}
