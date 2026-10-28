package supervisor

import (
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
)

// A session's issues are the problems it ran into. The supervisor files what
// happens into them under sess.mu and, in the same critical section, projects
// them onto sess.public: the worded Issues, and Health as the worst open one.
// That projection is the only place Health is set.
//
//   - noteRequestLocked: upstream error statuses (by failureIssue), models
//     sent unaliased, and model requests that succeed, which resolve the
//     issues a working request disproves;
//   - noteErrorLocked: ccwrap's own failures (by issueForError);
//   - noteNativeTLSLocked: native-TLS dials, blocked or mirrored;
//   - notePostureLocked: a published route, which settles what it changes.

// Activity filters a show_activity fix can point at.
const (
	activityFilterRequests = "forwarded-api"
	activityFilterErrors   = "error"
)

// issueEvidenceMax bounds the error text an issue quotes.
const issueEvidenceMax = 160

// issueEntry is one issue's state; renderIssue words it.
type issueEntry struct {
	id   string
	open bool
	// severity is fixed per issue, except that requests_failing takes the
	// worst failure since it opened.
	severity model.SessionHealth
	// count, evidence and lastSeen cover the time since the issue opened:
	// how many requests it hit, the latest error, and when.
	count    int
	evidence string
	lastSeen time.Time
	// at and filter locate the latest occurrence in Activity.
	at     time.Time
	filter string

	hint   string                   // requests_failing: the latest failure's suggestion
	host   string                   // upstream_unreachable: the unreachable host
	base   preflight.BaseURLProblem // base_url: what is wrong with it
	models []string                 // unaliased_model: the models sent unaliased

	// view is the session state the wording depends on: kept current while
	// the issue is open and frozen when it resolves, so a resolved issue
	// still reads as it did.
	view       issueView
	resolution string
}

// seen counts one occurrence, found at the Activity row at in filter.
func (e *issueEntry) seen(now, at time.Time, filter string) {
	e.count++
	e.lastSeen = now
	e.at, e.filter = at, filter
}

func (ss *sessionState) issueLocked(id string) *issueEntry {
	for _, e := range ss.issues {
		if e.id == id {
			return e
		}
	}
	return nil
}

// openIssueLocked returns issue id, open. One that was resolved opens again
// from nothing, in its old place.
func (ss *sessionState) openIssueLocked(id string, severity model.SessionHealth) *issueEntry {
	e := ss.issueLocked(id)
	if e == nil {
		e = &issueEntry{id: id}
		ss.issues = append(ss.issues, e)
	}
	if !e.open {
		*e = issueEntry{id: id, open: true, severity: severity}
	}
	return e
}

// resolveIssueLocked resolves issue id if it is open, saying how.
func (ss *sessionState) resolveIssueLocked(id, resolution string) {
	if e := ss.issueLocked(id); e != nil && e.open {
		e.open = false
		e.resolution = resolution
	}
}

// issueForError is the issue a ccwrap-side error class counts toward.
// Upstream error statuses are filed by failureIssue instead.
func issueForError(class, host string) string {
	switch {
	case class == "ccwrap_auth_missing":
		return model.IssueAuthMissing
	case class == "native_tls_blocked":
		return model.IssueNativeTLSBlocked
	case class == "upstream_unreachable" && !isTelemetryCaptureHost(host):
		return model.IssueUpstreamUnreachable
	}
	return model.IssueRequestsFailing
}

// noteRequestLocked files one recorded request. The caller holds ss.mu and
// has already added rec's model to UnmappedModels.
func (ss *sessionState) noteRequestLocked(rec model.RequestRecord, now time.Time) {
	f := rec.Failure
	failed := rec.FailedModelRequest() && f != nil
	if rec.ModelUnmapped && rec.Model != "" {
		e := ss.openIssueLocked(model.IssueUnaliasedModel, model.HealthWarn)
		e.models = slices.Clone(ss.public.UnmappedModels)
		if len(e.models) == 0 {
			e.models = []string{rec.Model}
		}
		e.seen(now, rec.Timestamp, activityFilterRequests)
		e.evidence = ""
		if failed && f.Issue == model.IssueUnaliasedModel {
			e.evidence = statusEvidence(rec)
		}
	}
	switch {
	case failed && f.Source == "upstream":
		if f.Issue == model.IssueUnaliasedModel {
			return // filed above
		}
		id := f.Issue
		if id == "" {
			id = model.IssueRequestsFailing
		}
		e := ss.openIssueLocked(id, model.HealthWarn)
		e.seen(now, rec.Timestamp, activityFilterRequests)
		e.evidence = statusEvidence(rec)
		switch id {
		case model.IssueBaseURL:
			if ap := ss.active.Load(); ap != nil {
				e.base, _ = preflight.CheckBaseURL(ap.r.apiBaseURL)
			}
		case model.IssueRequestsFailing:
			e.hint = f.Hint
		}
	case failed && f.Source == "ccwrap":
		// Its error filed it already; point the issue at this request.
		if e := ss.issueLocked(f.Issue); e != nil && e.open {
			e.at, e.filter = rec.Timestamp, activityFilterRequests
		}
	case rec.ModelRequest() && rec.StatusCode < http.StatusBadRequest:
		ss.resolveIssueLocked(model.IssueCredentialsRejected, "Requests are accepted again.")
		ss.resolveIssueLocked(model.IssueBaseURL, "Requests succeed again.")
		ss.resolveIssueLocked(model.IssueRateLimited, "Requests go through again.")
		ss.resolveIssueLocked(model.IssueUpstreamUnreachable, "The upstream answers again.")
		ss.resolveIssueLocked(model.IssueRequestsFailing, "Requests succeed again.")
	}
}

// statusEvidence quotes an upstream error status: "404 model not found: x".
func statusEvidence(rec model.RequestRecord) string {
	var msg string
	if f := rec.Failure; f != nil {
		msg = f.Message
		if msg == "" {
			msg = f.Type
		}
	}
	if msg == "" {
		msg = http.StatusText(rec.StatusCode)
	}
	return strings.TrimSpace(fmt.Sprintf("%d %s", rec.StatusCode, truncateRunes(msg, issueEvidenceMax)))
}

// noteErrorLocked files one recorded error. Upstream error statuses are
// filed with their request (noteRequestLocked), not here.
func (ss *sessionState) noteErrorLocked(rec model.ErrorRecord, now time.Time) {
	if strings.HasPrefix(rec.ErrorClass, upstreamHTTPErrorClassPrefix) {
		return
	}
	id := issueForError(rec.ErrorClass, rec.UpstreamHost)
	if id == model.IssueAuthMissing || id == model.IssueNativeTLSBlocked {
		// The route and the native-TLS dials open and resolve these; a
		// refused or blocked request only counts toward an open one.
		if e := ss.issueLocked(id); e != nil && e.open {
			e.seen(now, rec.Timestamp, activityFilterErrors)
		}
		return
	}
	_, severity := severityForClass(rec.ErrorClass)
	e := ss.openIssueLocked(id, severity)
	e.seen(now, rec.Timestamp, activityFilterErrors)
	e.evidence = truncateRunes(rec.Summary, issueEvidenceMax)
	switch id {
	case model.IssueUpstreamUnreachable:
		e.host = rec.UpstreamHost
	case model.IssueRequestsFailing:
		e.hint = rec.SuggestedAction
		e.severity = worseHealth(e.severity, severity)
	}
}

// noteNativeTLSLocked files a native-TLS dial: a blocked one opens the issue
// with its reason, a mirrored one resolves it.
func (ss *sessionState) noteNativeTLSLocked(blocked bool, reason string, now time.Time) {
	if !blocked {
		ss.resolveIssueLocked(model.IssueNativeTLSBlocked, "ccwrap mirrors Claude Code's TLS fingerprint again.")
		return
	}
	e := ss.openIssueLocked(model.IssueNativeTLSBlocked, model.HealthError)
	e.evidence = truncateRunes(reason, issueEvidenceMax)
	if e.lastSeen.IsZero() {
		// Each blocked request counts (noteErrorLocked); the dial itself
		// only dates a fresh block.
		e.lastSeen = now
	}
}

// notePostureLocked settles what a newly published route changes: the models
// it now maps, a key or upstream it replaces, a base URL it fixes, and
// whether it has credentials. The caller holds ss.mu and has derived cur
// into ss.public.
func (ss *sessionState) notePostureLocked(old, cur *posture) {
	switched := old.r.profileName != cur.r.profileName
	upstreamChanged := switched || baseURLString(old.r.apiBaseURL) != baseURLString(cur.r.apiBaseURL) ||
		egressTransportKey(old.r.egress) != egressTransportKey(cur.r.egress)
	authChanged := upstreamChanged || !reflect.DeepEqual(old.r.overrideAuth, cur.r.overrideAuth)
	changed := routeChangeResolution(old, cur)

	if cur.r.authBootstrap == model.AuthBootstrapMissing {
		ss.openIssueLocked(model.IssueAuthMissing, model.HealthWarn)
	} else if switched {
		ss.resolveIssueLocked(model.IssueAuthMissing, changed)
	} else {
		what := "this route"
		if cur.r.profileName != "" {
			what = "profile " + cur.r.profileName
		}
		ss.resolveIssueLocked(model.IssueAuthMissing, "Fixed — "+what+" has a key now.")
	}
	if authChanged {
		ss.resolveIssueLocked(model.IssueCredentialsRejected, changed)
	}
	if e := ss.issueLocked(model.IssueBaseURL); e != nil && e.open {
		var still preflight.BaseURLProblem
		bad := false
		if cur.r.routeClass != model.RouteClassFirstParty {
			still, bad = preflight.CheckBaseURL(cur.r.apiBaseURL)
		}
		switch {
		case switched:
			ss.resolveIssueLocked(model.IssueBaseURL, changed)
		case !bad && e.base.OpenAI:
			ss.resolveIssueLocked(model.IssueBaseURL, "Fixed — the base URL is no longer an OpenAI-compatible endpoint.")
		case !bad:
			ss.resolveIssueLocked(model.IssueBaseURL, "Fixed — the base URL no longer ends in /v1.")
		default:
			e.base = still
		}
	}
	if upstreamChanged {
		ss.resolveIssueLocked(model.IssueRateLimited, changed)
		ss.resolveIssueLocked(model.IssueUpstreamUnreachable, changed)
		ss.resolveIssueLocked(model.IssueRequestsFailing, changed)
	}

	// Of the models sent unaliased, keep the ones the new route still
	// leaves unaliased.
	var still []string
	for _, m := range ss.public.UnmappedModels {
		if cur.r.routeClass != model.RouteClassFirstParty && cur.r.modelAlias.Unmaps(m) {
			still = append(still, m)
		}
	}
	ss.public.UnmappedModels = still
	if e := ss.issueLocked(model.IssueUnaliasedModel); e != nil && e.open {
		if len(still) > 0 {
			e.models = slices.Clone(still)
		} else {
			ss.resolveIssueLocked(model.IssueUnaliasedModel, unaliasedResolution(old, cur, e.models))
		}
	}
}

func baseURLString(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.String()
}

// routeChangeResolution is how a route change resolves an issue it may have
// fixed: a switch names the new profile; an edit leaves it to the next
// requests.
func routeChangeResolution(old, cur *posture) string {
	if old.r.profileName != cur.r.profileName {
		if cur.r.profileName == "" {
			return "Switched to inherit-env."
		}
		return "Switched to profile " + cur.r.profileName + "."
	}
	what := "The route"
	if cur.r.profileName != "" {
		what = "Profile " + cur.r.profileName
	}
	return what + " changed. The next requests show whether it works."
}

func unaliasedResolution(old, cur *posture, models []string) string {
	cfg := cur.r.modelAlias
	switch {
	case cfg.KeepsNames():
		return "Fixed — other Claude models now keep their Claude names."
	case cfg.Default() != "":
		return "Fixed — other Claude models now go to " + cfg.Default() + "."
	case old.r.profileName != cur.r.profileName || cur.r.routeClass == model.RouteClassFirstParty || !cfg.Enabled():
		return routeChangeResolution(old, cur)
	case len(models) == 1:
		return "Fixed — " + models[0] + " now goes to " + cfg.Forward[models[0]] + "."
	}
	return "Fixed — each of them has an alias now."
}

// projectIssuesLocked words the issues onto ss.public and sets Health to the
// worst open one. It reports whether the projection changed.
func (ss *sessionState) projectIssuesLocked() bool {
	v := issueView{
		profile:    ss.public.ActiveProfileName,
		firstParty: ss.public.RouteClass == model.RouteClassFirstParty,
		forward:    ss.public.ModelAliasForward,
		missingEnv: ss.public.MissingAuthEnv,
		authSource: ss.public.AuthSource,
	}
	health := model.HealthOK
	var issues []model.Issue
	for _, e := range ss.issues {
		if e.open {
			e.view = v
			health = worseHealth(health, e.severity)
		}
		issues = append(issues, renderIssue(e))
	}
	changed := ss.public.Health != health || !reflect.DeepEqual(ss.public.Issues, issues)
	ss.public.Issues = issues
	ss.public.Health = health
	return changed
}

func worseHealth(a, b model.SessionHealth) model.SessionHealth {
	if healthRank(b) > healthRank(a) {
		return b
	}
	return a
}

func healthRank(h model.SessionHealth) int {
	switch h {
	case model.HealthWarn:
		return 1
	case model.HealthError:
		return 2
	}
	return 0
}

// issueView is the session state an issue's wording depends on.
type issueView struct {
	profile    string
	firstParty bool
	forward    map[string]string
	missingEnv string
	authSource model.AuthSource
}

// renderIssue words one issue. Fixes and the note are offered only while it
// is open.
func renderIssue(e *issueEntry) model.Issue {
	v := e.view
	is := model.Issue{ID: e.id, State: model.IssueResolved, Severity: e.severity, Resolution: e.resolution}
	if e.open {
		is.State = model.IssueOpen
	}
	if !e.lastSeen.IsZero() {
		t := e.lastSeen
		is.LastSeenAt = &t
	}
	is.Evidence = countedEvidence(e.evidence, e.count, "request", "requests")
	is.Count = e.count
	var fixes []model.IssueFix
	var note string
	editProfile := func(field string) []model.IssueFix {
		return []model.IssueFix{{Label: "Edit profile " + v.profile, Action: model.FixOpenProfile, Value: v.profile, Field: field, Primary: true}}
	}
	switch e.id {
	case model.IssueUnaliasedModel:
		models := e.models
		if len(models) == 1 {
			is.Title, is.TitleCode = models[0]+" has no alias", models[0]
			is.Detail = "Claude Code sent " + models[0] + " with no alias; the upstream got that name as-is."
		} else {
			is.Title = fmt.Sprintf("%d Claude models have no alias", len(models))
			is.Detail = "Claude Code sent " + joinAnd(models) + " with no alias; the upstream got those names as-is."
		}
		_, targets := modelalias.Config{Forward: v.forward}.SuggestedDefault()
		if v.profile != "" {
			for _, t := range targets {
				fixes = append(fixes, model.IssueFix{Label: "Send them to " + t, Action: model.FixModelDefault, Value: t, Primary: len(targets) == 1})
			}
			fixes = append(fixes, model.IssueFix{Label: "Keep Claude names", Action: model.FixModelDefault, Value: modelalias.KeepName})
			note = "Saves the choice to profile " + v.profile + " and applies it now."
		} else if len(targets) == 1 {
			note = "Relaunch with --model-alias '*=" + targets[0] + "' to send them to " + targets[0] + ", or '*=*' to keep their Claude names."
		} else {
			note = "Relaunch with --model-alias '*=<model>' to send them to one upstream model, or '*=*' to keep their Claude names."
		}
	case model.IssueCredentialsRejected:
		switch {
		case v.firstParty:
			is.Title = "Anthropic rejected Claude Code's credentials"
			is.Detail = "Run /login in Claude Code to sign in again."
		case v.profile != "":
			is.Title = "The upstream rejected the credentials"
			is.Detail = "Every request on this route fails until the key in profile " + v.profile + " works again."
			fixes = editProfile("key")
			note = "or run ccwrap profile edit " + v.profile + " in a terminal"
		default:
			is.Title = "The upstream rejected the credentials"
			is.Detail = "Every request on this route fails until the key ccwrap sends for it works again."
			if env := envAuthSource(v.authSource); env != "" {
				note = "Check $" + env + " and relaunch."
			}
		}
	case model.IssueBaseURL:
		switch b := e.base; {
		case b.OpenAI && b.Provider != "":
			is.Title = "This is " + b.Provider + "'s OpenAI-compatible URL"
			is.Detail = "Claude Code speaks the Anthropic API, so requests to this URL fail."
		case b.OpenAI:
			is.Title = "The base URL is an OpenAI-compatible endpoint"
			is.Detail = "Claude Code speaks the Anthropic API, so requests to this URL fail. The provider's docs give its Anthropic-compatible base URL."
		default:
			is.Title = "The base URL ends in /v1"
			is.Detail = "Claude Code adds /v1 itself, so requests went to /v1/v1/messages."
		}
		switch {
		case v.profile != "":
			fixes = editProfile("base_url")
			if e.base.Fixed != "" {
				note = "Change it to " + e.base.Fixed + "."
			}
		case e.base.Fixed != "":
			note = "Set the base URL to " + e.base.Fixed + " and relaunch."
		default:
			note = "Set the provider's Anthropic-compatible base URL and relaunch."
		}
	case model.IssueRateLimited:
		is.Title = "Rate limited by the upstream"
		is.Detail = "Claude Code retries on its own. Nothing to change unless it keeps happening."
		fixes = showActivityFix(e)
	case model.IssueAuthMissing:
		is.Evidence = countedEvidence("", e.count, "request refused", "requests refused")
		switch {
		case v.missingEnv != "":
			is.Title, is.TitleCode = "$"+v.missingEnv+" is not set", "$"+v.missingEnv
			whose := "This route"
			if v.profile != "" {
				whose = "Profile " + v.profile
			}
			is.Detail = whose + " takes its key from $" + v.missingEnv + ", so ccwrap refuses every request until it has one."
		case v.profile != "":
			is.Title = "Profile " + v.profile + " has no key"
			is.Detail = "It sets no key and no key_env, so ccwrap refuses every request until it has one."
		default:
			is.Title = "No key for this route"
			is.Detail = "ccwrap found no key for this route, so it refuses every request."
		}
		switch {
		case v.profile != "" && v.missingEnv != "":
			fixes = editProfile("key")
			note = "or set $" + v.missingEnv + " and restart ccwrap"
		case v.profile != "":
			fixes = editProfile("key")
			note = "or switch to another profile"
		case v.missingEnv != "":
			note = "Set $" + v.missingEnv + " and restart ccwrap, or switch to a profile."
		default:
			note = "Switch to a profile that has a key."
		}
	case model.IssueNativeTLSBlocked:
		is.Title = "Native TLS is blocking requests"
		is.Detail = "ccwrap couldn't mirror Claude Code's TLS fingerprint, so it blocks requests rather than send them with a different one."
		note = "Run ccwrap doctor in a terminal. CCWRAP_NATIVE_TLS=0 turns the mirror off, but makes the session identifiable."
	case model.IssueUpstreamUnreachable:
		if e.host != "" {
			is.Title, is.TitleCode = e.host+" is unreachable", e.host
		} else {
			is.Title = "The upstream is unreachable"
		}
		is.Detail = "ccwrap got no response from it. Check the network, the egress proxy and the base URL."
		fixes = showActivityFix(e)
	case model.IssueRequestsFailing:
		is.Title = "Requests are failing"
		is.Detail = sentence(e.hint)
		if is.Detail == "" {
			is.Detail = "The failed requests are in Activity."
		}
		fixes = showActivityFix(e)
	}
	if e.open {
		is.Fixes, is.Note = fixes, note
	}
	return is
}

func showActivityFix(e *issueEntry) []model.IssueFix {
	if e.at.IsZero() {
		return nil
	}
	at := e.at
	return []model.IssueFix{{Label: "Show in activity", Action: model.FixShowActivity, Value: e.filter, At: &at}}
}

// countedEvidence adds how many requests an issue hit to its latest error.
func countedEvidence(evidence string, n int, one, many string) string {
	if n == 0 {
		return evidence
	}
	c := fmt.Sprintf("%d %s", n, many)
	if n == 1 {
		c = "1 " + one
	}
	if evidence == "" {
		return c
	}
	return evidence + " · " + c
}

// envAuthSource is the env var a no-profile route took its key from, or "".
func envAuthSource(src model.AuthSource) string {
	switch src {
	case model.AuthSourceNone, "":
		return ""
	}
	return string(src)
}

// sentence turns a hint into a sentence: capitalized, with a full stop.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r, n := utf8.DecodeRuneInString(s)
	s = string(unicode.ToUpper(r)) + s[n:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// joinAnd lists items as "a, b and c".
func joinAnd(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// issueBadge is a session's sidebar badge, at most 18 characters: how many
// issues are open, or for a single one the failed requests it counts. ""
// when none is open or the session ended.
func issueBadge(sess model.Session) string {
	if sess.State == model.StateEnded {
		return ""
	}
	var open []model.Issue
	for _, is := range sess.Issues {
		if is.State == model.IssueOpen {
			open = append(open, is)
		}
	}
	switch {
	case len(open) == 0:
		return ""
	case len(open) > 1:
		return fmt.Sprintf("%d issues", len(open))
	case open[0].ID == model.IssueUnaliasedModel || open[0].Count == 0:
		return "1 issue"
	}
	n := open[0].Count
	if n == 1 {
		return "1 failed request"
	}
	if b := fmt.Sprintf("%d failed requests", n); len(b) <= 18 {
		return b
	}
	return fmt.Sprintf("%d failed", n)
}
