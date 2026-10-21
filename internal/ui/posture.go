package ui

import (
	"fmt"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// SessionPosture returns the one-line plain-English hero summary
// rendered identically by every session-bearing surface. Wire/enum
// strings never appear here. Recorded problems appear only as a count:
// the error list itself carries the details.
func SessionPosture(sess *model.Session) string {
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
	// "issues": the list holds ccwrap's errors and the upstream's error
	// responses (warnings) alike.
	if sess.RecentErrorCount > 0 {
		noun := "issues"
		if sess.RecentErrorCount == 1 {
			noun = "issue"
		}
		base += fmt.Sprintf(" — %d %s recorded since launch.", sess.RecentErrorCount, noun)
	}
	return base
}
