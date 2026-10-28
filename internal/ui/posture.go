package ui

import (
	"fmt"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// SessionPosture returns the one-line plain-English hero summary
// rendered identically by every session-bearing surface. Wire/enum
// strings never appear here. Recorded problems appear only as a count:
// the session's issues carry the details.
func SessionPosture(sess *model.Session) string {
	base, note := SessionPostureParts(sess)
	return base + note
}

// SessionPostureParts is SessionPosture in two parts: the route sentence and
// the " — N issues recorded since launch." note ("" when none), so a page can
// color the note.
func SessionPostureParts(sess *model.Session) (base, note string) {
	if sess == nil {
		return "", ""
	}
	if sess.State == model.StateEnded {
		// model.Session has no close-reason field, so degrade to
		// the reasonless form. Do not invent a reason.
		return "Session closed. Final state preserved below.", ""
	}
	host := sess.ExactUpstreamHost
	if host == "" {
		host = "api.anthropic.com"
	}
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
	// The session's issues, open and resolved alike: the count says what
	// went wrong since launch, the issue cards say what and whether it is
	// fixed.
	if n := len(sess.Issues); n > 0 {
		noun := "issues"
		if n == 1 {
			noun = "issue"
		}
		note = fmt.Sprintf(" — %d %s recorded since launch.", n, noun)
	}
	return base, note
}
