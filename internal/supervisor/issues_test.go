package supervisor

import (
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
)

// issueRoute is a third-party route under profile, x-api-key auth, with the
// given aliases.
func issueRoute(base, profile string, aliases map[string]string) model.SessionRouteRequest {
	route := xAPIKeyRoute(base, model.EgressConfig{Mode: "direct"})
	route.NativeTLS = false
	route.ActiveProfileName = profile
	if len(aliases) > 0 {
		route.ModelAlias = model.ModelAliasConfig{Mode: model.ModelAliasRewrite, Strict: true, Forward: aliases}
	}
	return route
}

func issueSession(t *testing.T, route model.SessionRouteRequest) (*Supervisor, *sessionState) {
	t.Helper()
	sv, sess := newTestSessionForCreate(t)
	setIssueRoute(t, sv, sess, route)
	return sv, sess
}

func setIssueRoute(t *testing.T, sv *Supervisor, sess *sessionState, route model.SessionRouteRequest) {
	t.Helper()
	if err := sv.setRoute(sess.public.ID, route); err != nil {
		t.Fatal(err)
	}
}

// modelRequest is a model request record that ended with status and f.
func modelRequest(sess *sessionState, status int, f *model.RequestFailure) model.RequestRecord {
	return model.RequestRecord{Timestamp: time.Now(), SessionID: sess.public.ID, Method: "POST", Path: "/v1/messages", StatusCode: status, Failure: f}
}

// upstreamError is the failure ccwrap records for an upstream error status.
func upstreamError(fc failureContext, message string) *model.RequestFailure {
	body, _ := json.Marshal(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": message}})
	return upstreamFailure(fc, body, false, false)
}

func onlyIssue(t *testing.T, sess *sessionState) model.Issue {
	t.Helper()
	got := sess.snapshot().Issues
	if len(got) != 1 {
		t.Fatalf("issues = %+v, want exactly one", got)
	}
	return got[0]
}

func TestIssueCredentialsRejected(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
	id := sess.public.ID
	reject := func() model.RequestRecord {
		rec := modelRequest(sess, 401, upstreamError(failureContext{status: 401, profile: "gw"}, "No auth credentials found"))
		sv.recordRequest(id, rec)
		return rec
	}

	reject()
	reject()
	is := onlyIssue(t, sess)
	want := model.Issue{
		ID: model.IssueCredentialsRejected, State: model.IssueOpen, Severity: model.HealthWarn,
		Title:    "The upstream rejected the credentials",
		Detail:   "Every request on this route fails until the key in profile gw works again.",
		Evidence: "401 No auth credentials found · 2 requests",
		Count:    2,
		Fixes:    []model.IssueFix{{Label: "Edit profile gw", Action: model.FixOpenProfile, Value: "gw", Field: "key", Primary: true}},
		Note:     "or run ccwrap profile edit gw in a terminal",
	}
	if is.LastSeenAt == nil {
		t.Fatal("an open issue says when it last happened")
	}
	is.LastSeenAt = nil
	if !reflect.DeepEqual(is, want) {
		t.Fatalf("issue =\n%+v\nwant\n%+v", is, want)
	}
	if got := sess.snapshot().Health; got != model.HealthWarn {
		t.Fatalf("Health = %q, want warn", got)
	}

	// Only a model request that works disproves it.
	sv.recordRequest(id, model.RequestRecord{Timestamp: time.Now(), SessionID: id, Method: "GET", Path: "/v1/models", StatusCode: 200})
	if is := onlyIssue(t, sess); is.State != model.IssueOpen {
		t.Fatalf("a non-model request resolved it: %+v", is)
	}
	sv.recordRequest(id, modelRequest(sess, 200, nil))
	is = onlyIssue(t, sess)
	if is.State != model.IssueResolved || is.Resolution != "Requests are accepted again." || is.Fixes != nil || is.Note != "" {
		t.Fatalf("after a success: %+v", is)
	}
	if got := sess.snapshot().Health; got != model.HealthOK {
		t.Fatalf("Health = %q after the issue resolved, want ok", got)
	}

	// It comes back from a fresh count, in its old place.
	sv.recordRequest(id, modelRequest(sess, 403, upstreamError(failureContext{status: 403, profile: "gw"}, "forbidden")))
	if is := onlyIssue(t, sess); is.State != model.IssueOpen || is.Evidence != "403 forbidden · 1 request" || is.Resolution != "" {
		t.Fatalf("reopened: %+v", is)
	}
}

func TestIssueCredentialsRejectedFirstParty(t *testing.T) {
	route := issueRoute("https://api.anthropic.com/", "", nil)
	route.RouteClass = model.RouteClassFirstParty
	route.OverrideAuth = nil
	sv, sess := issueSession(t, route)
	sv.recordRequest(sess.public.ID, modelRequest(sess, 401, upstreamError(failureContext{status: 401, firstParty: true}, "invalid x-api-key")))
	is := onlyIssue(t, sess)
	if is.Title != "Anthropic rejected Claude Code's credentials" || is.Detail != "Run /login in Claude Code to sign in again." || is.Fixes != nil {
		t.Fatalf("issue = %+v", is)
	}
}

func TestIssueRateLimitedPointsAtItsRequest(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
	rec := modelRequest(sess, 429, upstreamError(failureContext{status: 429}, "too many requests"))
	sv.recordRequest(sess.public.ID, rec)
	is := onlyIssue(t, sess)
	if is.ID != model.IssueRateLimited || is.Severity != model.HealthWarn || is.Title != "Rate limited by the upstream" || is.Evidence != "429 too many requests · 1 request" {
		t.Fatalf("issue = %+v", is)
	}
	if len(is.Fixes) != 1 || is.Fixes[0].Action != model.FixShowActivity || is.Fixes[0].Value != "forwarded-api" ||
		is.Fixes[0].At == nil || !is.Fixes[0].At.Equal(rec.Timestamp) {
		t.Fatalf("fixes = %+v, want show_activity at the request", is.Fixes)
	}
	sv.recordRequest(sess.public.ID, modelRequest(sess, 200, nil))
	if is := onlyIssue(t, sess); is.State != model.IssueResolved || is.Resolution != "Requests go through again." {
		t.Fatalf("after a success: %+v", is)
	}
}

func TestIssueBaseURLV1(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/v1", "gw", nil))
	sv.recordRequest(sess.public.ID, modelRequest(sess, 404, upstreamError(failureContext{status: 404, profile: "gw", base: &preflight.BaseURLProblem{Fixed: "https://gw.example"}}, "not found")))
	is := onlyIssue(t, sess)
	if is.ID != model.IssueBaseURL || is.Title != "The base URL ends in /v1" || is.Note != "Change it to https://gw.example." ||
		len(is.Fixes) != 1 || is.Fixes[0].Field != "base_url" || is.Fixes[0].Value != "gw" {
		t.Fatalf("issue = %+v", is)
	}
	// An edit that keeps the /v1 leaves it open; one that drops it fixes it.
	setIssueRoute(t, sv, sess, issueRoute("https://gw.example/api/v1", "gw", nil))
	if is := onlyIssue(t, sess); is.State != model.IssueOpen || is.Note != "Change it to https://gw.example/api." {
		t.Fatalf("still /v1: %+v", is)
	}
	setIssueRoute(t, sv, sess, issueRoute("https://gw.example/", "gw", nil))
	if is := onlyIssue(t, sess); is.State != model.IssueResolved || is.Resolution != "Fixed — the base URL no longer ends in /v1." {
		t.Fatalf("fixed: %+v", is)
	}
}

// A provider's OpenAI-compatible URL is named as such, with the provider's
// Anthropic-compatible URL to change it to.
func TestIssueBaseURLOpenAICompatible(t *testing.T) {
	base := "https://dashscope.aliyuncs.com/compatible-mode/v1"
	sv, sess := issueSession(t, issueRoute(base, "bailian", nil))
	u, _ := url.Parse(base)
	b, _ := preflight.CheckBaseURL(u)
	sv.recordRequest(sess.public.ID, modelRequest(sess, 404, upstreamError(failureContext{status: 404, profile: "bailian", base: &b}, "")))
	is := onlyIssue(t, sess)
	if is.ID != model.IssueBaseURL || is.Title != "This is Alibaba Cloud Model Studio's OpenAI-compatible URL" ||
		is.Note != "Change it to https://dashscope.aliyuncs.com/apps/anthropic." || len(is.Fixes) != 1 || is.Fixes[0].Field != "base_url" {
		t.Fatalf("issue = %+v", is)
	}
	setIssueRoute(t, sv, sess, issueRoute("https://dashscope.aliyuncs.com/apps/anthropic", "bailian", nil))
	if is := onlyIssue(t, sess); is.State != model.IssueResolved || is.Resolution != "Fixed — the base URL is no longer an OpenAI-compatible endpoint." {
		t.Fatalf("fixed: %+v", is)
	}
}

func TestIssueUnaliasedModel(t *testing.T) {
	aliases := map[string]string{"claude-sonnet-5-5": "deepseek-v4-flash"}
	sv, sess := issueSession(t, issueRoute("https://api.deepseek.example/", "ds", aliases))
	id := sess.public.ID
	unaliased := func(m string, status int) {
		rec := modelRequest(sess, status, nil)
		rec.Model, rec.UpstreamModel, rec.ModelUnmapped = m, m, true
		if status >= 400 {
			rec.Failure = upstreamError(failureContext{status: status, unmapped: true, model: m, profile: "ds"}, "model not found: "+m)
		}
		sv.recordRequest(id, rec)
	}

	unaliased("claude-opus-5-5", 404)
	is := onlyIssue(t, sess)
	is.LastSeenAt = nil
	want := model.Issue{
		ID: model.IssueUnaliasedModel, State: model.IssueOpen, Severity: model.HealthWarn,
		Title: "claude-opus-5-5 has no alias", TitleCode: "claude-opus-5-5",
		Detail:   "Claude Code sent claude-opus-5-5 with no alias; the upstream got that name as-is.",
		Evidence: "404 model not found: claude-opus-5-5 · 1 request",
		Count:    1,
		Fixes: []model.IssueFix{
			{Label: "Send them to deepseek-v4-flash", Action: model.FixModelDefault, Value: "deepseek-v4-flash", Primary: true},
			{Label: "Keep Claude names", Action: model.FixModelDefault, Value: modelalias.KeepName},
		},
		Note: "Saves the choice to profile ds and applies it now.",
	}
	if !reflect.DeepEqual(is, want) {
		t.Fatalf("issue =\n%+v\nwant\n%+v", is, want)
	}

	// A request that works does not alias the model.
	sv.recordRequest(id, modelRequest(sess, 200, nil))
	unaliased("claude-haiku-5-5", 200)
	is = onlyIssue(t, sess)
	if is.State != model.IssueOpen || is.Title != "2 Claude models have no alias" || is.TitleCode != "" ||
		is.Detail != "Claude Code sent claude-opus-5-5 and claude-haiku-5-5 with no alias; the upstream got those names as-is." ||
		is.Evidence != "2 requests" {
		t.Fatalf("two models: %+v", is)
	}
	if got := sess.snapshot().Health; got != model.HealthWarn {
		t.Fatalf("Health = %q, want warn", got)
	}

	// A route that still leaves them unaliased keeps the issue, now offering
	// that profile's fix.
	setIssueRoute(t, sv, sess, issueRoute("https://api.kimi.example/", "kimi", map[string]string{"claude-sonnet-5-5": "kimi-k3"}))
	is = onlyIssue(t, sess)
	if is.State != model.IssueOpen || is.Fixes[0].Value != "kimi-k3" || is.Note != "Saves the choice to profile kimi and applies it now." {
		t.Fatalf("after a switch that keeps them unaliased: %+v", is)
	}
	if got := sess.snapshot().UnmappedModels; !reflect.DeepEqual(got, []string{"claude-opus-5-5", "claude-haiku-5-5"}) {
		t.Fatalf("UnmappedModels = %v", got)
	}

	// An alias for one leaves the other.
	setIssueRoute(t, sv, sess, issueRoute("https://api.kimi.example/", "kimi", map[string]string{"claude-sonnet-5-5": "kimi-k3", "claude-opus-5-5": "kimi-k3-pro"}))
	if is := onlyIssue(t, sess); is.State != model.IssueOpen || is.Title != "claude-haiku-5-5 has no alias" {
		t.Fatalf("after aliasing one: %+v", is)
	}

	// The default covers the rest.
	setIssueRoute(t, sv, sess, issueRoute("https://api.kimi.example/", "kimi", map[string]string{"claude-sonnet-5-5": "kimi-k3", modelalias.DefaultKey: "kimi-k3"}))
	is = onlyIssue(t, sess)
	if is.State != model.IssueResolved || is.Resolution != "Fixed — other Claude models now go to kimi-k3." || is.Title != "claude-haiku-5-5 has no alias" {
		t.Fatalf("after the default: %+v", is)
	}
	got := sess.snapshot()
	if got.Health != model.HealthOK || len(got.UnmappedModels) != 0 {
		t.Fatalf("Health = %q, UnmappedModels = %v", got.Health, got.UnmappedModels)
	}
}

func TestIssueUnaliasedModelResolutions(t *testing.T) {
	aliases := map[string]string{"claude-sonnet-5-5": "gpt-6"}
	for _, c := range []struct {
		name    string
		route   model.SessionRouteRequest
		resolve string
	}{
		{"keep names", issueRoute("https://gw.example/", "gw", map[string]string{"claude-sonnet-5-5": "gpt-6", modelalias.DefaultKey: modelalias.KeepName}), "Fixed — other Claude models now keep their Claude names."},
		{"own alias", issueRoute("https://gw.example/", "gw", map[string]string{"claude-sonnet-5-5": "gpt-6", "claude-opus-5-5": "gpt-6-pro"}), "Fixed — claude-opus-5-5 now goes to gpt-6-pro."},
		{"switch to no aliases", issueRoute("https://other.example/", "other", nil), "Switched to profile other."},
		{"same profile, no aliases", issueRoute("https://gw.example/", "gw", nil), "Profile gw changed. The next requests show whether it works."},
	} {
		t.Run(c.name, func(t *testing.T) {
			sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", aliases))
			rec := modelRequest(sess, 200, nil)
			rec.Model, rec.ModelUnmapped = "claude-opus-5-5", true
			sv.recordRequest(sess.public.ID, rec)
			setIssueRoute(t, sv, sess, c.route)
			if is := onlyIssue(t, sess); is.State != model.IssueResolved || is.Resolution != c.resolve {
				t.Fatalf("issue = %+v, want resolved with %q", is, c.resolve)
			}
		})
	}
}

func TestIssueUnaliasedModelWithoutProfile(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "", map[string]string{"claude-sonnet-5-5": "gpt-6"}))
	rec := modelRequest(sess, 200, nil)
	rec.Model, rec.ModelUnmapped = "claude-opus-5-5", true
	sv.recordRequest(sess.public.ID, rec)
	is := onlyIssue(t, sess)
	if is.Fixes != nil || is.Note != "Relaunch with --model-alias '*=gpt-6' to send them to gpt-6, or '*=*' to keep their Claude names." {
		t.Fatalf("issue = %+v", is)
	}
}

func TestIssueAuthMissing(t *testing.T) {
	missing := issueRoute("https://gw.example/", "gw", nil)
	missing.OverrideAuth = nil
	missing.AuthBootstrap = model.AuthBootstrapMissing
	missing.MissingAuthEnv = "GW_KEY"
	sv, sess := issueSession(t, missing)

	// The route alone opens it: every request would be refused.
	is := onlyIssue(t, sess)
	want := model.Issue{
		ID: model.IssueAuthMissing, State: model.IssueOpen, Severity: model.HealthWarn,
		Title: "$GW_KEY is not set", TitleCode: "$GW_KEY",
		Detail: "Profile gw takes its key from $GW_KEY, so ccwrap refuses every request until it has one.",
		Fixes:  []model.IssueFix{{Label: "Edit profile gw", Action: model.FixOpenProfile, Value: "gw", Field: "key", Primary: true}},
		Note:   "or set $GW_KEY and restart ccwrap",
	}
	if !reflect.DeepEqual(is, want) {
		t.Fatalf("issue =\n%+v\nwant\n%+v", is, want)
	}
	if got := sess.snapshot().Health; got != model.HealthWarn {
		t.Fatalf("Health = %q, want warn", got)
	}
	sv.recordError(sess.public.ID, model.ErrorRecord{Timestamp: time.Now(), ErrorClass: "ccwrap_auth_missing", Summary: "refused"})
	if is := onlyIssue(t, sess); is.Evidence != "1 request refused" {
		t.Fatalf("after a refusal: %+v", is)
	}

	// Fixing the profile resolves it.
	setIssueRoute(t, sv, sess, issueRoute("https://gw.example/", "gw", nil))
	if is := onlyIssue(t, sess); is.State != model.IssueResolved || is.Resolution != "Fixed — profile gw has a key now." {
		t.Fatalf("after the fix: %+v", is)
	}
}

func TestIssueAuthMissingWithoutKeyEnv(t *testing.T) {
	missing := issueRoute("https://gw.example/", "gw", nil)
	missing.OverrideAuth = nil
	missing.AuthBootstrap = model.AuthBootstrapMissing
	sv, sess := issueSession(t, missing)
	if is := onlyIssue(t, sess); is.Title != "Profile gw has no key" || is.Note != "or switch to another profile" {
		t.Fatalf("issue = %+v", is)
	}
	setIssueRoute(t, sv, sess, issueRoute("https://other.example/", "other", nil))
	if is := onlyIssue(t, sess); is.State != model.IssueResolved || is.Resolution != "Switched to profile other." {
		t.Fatalf("after a switch: %+v", is)
	}
}

func TestIssueNativeTLSBlocked(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
	sp := &sessionProxy{supervisor: sv, session: sess}
	sp.recordNativeTLS(false, "no ClientHello captured")
	is := onlyIssue(t, sess)
	if is.ID != model.IssueNativeTLSBlocked || is.Severity != model.HealthError || is.Evidence != "no ClientHello captured" ||
		is.Title != "Native TLS is blocking requests" || !strings.HasPrefix(is.Note, "Run ccwrap doctor") || is.Fixes != nil {
		t.Fatalf("issue = %+v", is)
	}
	if got := sess.snapshot().Health; got != model.HealthError {
		t.Fatalf("Health = %q, want error", got)
	}
	sv.recordError(sess.public.ID, model.ErrorRecord{Timestamp: time.Now(), ErrorClass: "native_tls_blocked", Summary: "native-tls: cannot mirror"})
	if is := onlyIssue(t, sess); is.Evidence != "no ClientHello captured · 1 request" {
		t.Fatalf("after a blocked request: %+v", is)
	}
	sp.recordNativeTLS(true, "")
	if is := onlyIssue(t, sess); is.State != model.IssueResolved || is.Resolution != "ccwrap mirrors Claude Code's TLS fingerprint again." {
		t.Fatalf("after a mirrored dial: %+v", is)
	}
	if got := sess.snapshot().Health; got != model.HealthOK {
		t.Fatalf("Health = %q, want ok", got)
	}
}

func TestIssueUpstreamUnreachable(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
	id := sess.public.ID
	sv.recordError(id, model.ErrorRecord{Timestamp: time.Now(), ErrorClass: "upstream_unreachable", UpstreamHost: "gw.example", Summary: "dial tcp: connection refused"})
	is := onlyIssue(t, sess)
	if is.ID != model.IssueUpstreamUnreachable || is.Severity != model.HealthError || is.Title != "gw.example is unreachable" || is.TitleCode != "gw.example" ||
		is.Evidence != "dial tcp: connection refused · 1 request" || len(is.Fixes) != 1 || is.Fixes[0].Value != "error" {
		t.Fatalf("issue = %+v", is)
	}
	// The failed request that follows its error is where Activity shows it.
	rec := modelRequest(sess, 502, ccwrapFailure("dial tcp: connection refused", "check upstream reachability", model.IssueUpstreamUnreachable))
	sv.recordRequest(id, rec)
	is = onlyIssue(t, sess)
	if is.Evidence != "dial tcp: connection refused · 1 request" || is.Fixes[0].Value != "forwarded-api" || !is.Fixes[0].At.Equal(rec.Timestamp) {
		t.Fatalf("after its request: %+v", is)
	}
	sv.recordRequest(id, modelRequest(sess, 200, nil))
	if is := onlyIssue(t, sess); is.State != model.IssueResolved || is.Resolution != "The upstream answers again." {
		t.Fatalf("after a success: %+v", is)
	}
}

func TestIssueRequestsFailingCollectsTheRest(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
	id := sess.public.ID
	sv.recordRequest(id, modelRequest(sess, 529, upstreamError(failureContext{status: 529}, "Overloaded")))
	is := onlyIssue(t, sess)
	if is.ID != model.IssueRequestsFailing || is.Severity != model.HealthWarn || is.Title != "Requests are failing" ||
		is.Evidence != "529 Overloaded · 1 request" || !strings.HasPrefix(is.Detail, "The upstream failed — usually temporary") || !strings.HasSuffix(is.Detail, ".") {
		t.Fatalf("issue = %+v", is)
	}
	// A failure of ccwrap's own makes it an error; telemetry that cannot be
	// reached is not the upstream being down.
	sv.recordError(id, model.ErrorRecord{Timestamp: time.Now(), ErrorClass: "upstream_unreachable", UpstreamHost: "http-intake.logs.us5.datadoghq.com", Summary: "dial tcp: i/o timeout", SuggestedAction: "check upstream reachability and route configuration"})
	is = onlyIssue(t, sess)
	if is.Severity != model.HealthError || is.Evidence != "dial tcp: i/o timeout · 2 requests" || is.Detail != "Check upstream reachability and route configuration." {
		t.Fatalf("after a telemetry failure: %+v", is)
	}
	if got := sess.snapshot().Health; got != model.HealthError {
		t.Fatalf("Health = %q, want error", got)
	}
	sv.recordRequest(id, modelRequest(sess, 200, nil))
	if is := onlyIssue(t, sess); is.State != model.IssueResolved || is.Resolution != "Requests succeed again." {
		t.Fatalf("after a success: %+v", is)
	}
}

func TestIssueHealthIsTheWorstOpenIssue(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
	sp := &sessionProxy{supervisor: sv, session: sess}
	sv.recordRequest(sess.public.ID, modelRequest(sess, 429, upstreamError(failureContext{status: 429}, "slow down")))
	sp.recordNativeTLS(false, "handshake failed")
	if got := sess.snapshot().Health; got != model.HealthError {
		t.Fatalf("warn + error: Health = %q", got)
	}
	sp.recordNativeTLS(true, "")
	if got := sess.snapshot().Health; got != model.HealthWarn {
		t.Fatalf("warn left open: Health = %q", got)
	}
	if got := sess.snapshot().Issues; len(got) != 2 || got[0].ID != model.IssueRateLimited || got[1].ID != model.IssueNativeTLSBlocked {
		t.Fatalf("issues keep the order they opened in: %+v", got)
	}
}

func TestIssueRouteChangeSettlesUpstreamIssues(t *testing.T) {
	for _, c := range []struct {
		name    string
		route   model.SessionRouteRequest
		resolve string // "" = stays open
	}{
		{"switch", issueRoute("https://other.example/", "other", nil), "Switched to profile other."},
		{"new base URL", issueRoute("https://gw2.example/", "gw", nil), "Profile gw changed. The next requests show whether it works."},
		{"aliases only", issueRoute("https://gw.example/", "gw", map[string]string{"claude-opus-5-5": "gpt-6"}), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
			sv.recordRequest(sess.public.ID, modelRequest(sess, 429, upstreamError(failureContext{status: 429}, "slow down")))
			setIssueRoute(t, sv, sess, c.route)
			is := onlyIssue(t, sess)
			if c.resolve == "" {
				if is.State != model.IssueOpen {
					t.Fatalf("issue = %+v, want still open", is)
				}
				return
			}
			if is.State != model.IssueResolved || is.Resolution != c.resolve {
				t.Fatalf("issue = %+v, want resolved with %q", is, c.resolve)
			}
		})
	}
}

func TestIssueCredentialsRejectedSettledByNewKey(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
	sv.recordRequest(sess.public.ID, modelRequest(sess, 401, upstreamError(failureContext{status: 401, profile: "gw"}, "bad key")))
	rekeyed := issueRoute("https://gw.example/", "gw", nil)
	rekeyed.OverrideAuth = &model.AuthOverride{Mode: model.AuthModeOverrideXAPIKey, Source: model.AuthSourceAnthropicAPIKey, HeaderName: "X-API-Key", HeaderValue: "new-sekret"}
	setIssueRoute(t, sv, sess, rekeyed)
	if is := onlyIssue(t, sess); is.State != model.IssueResolved || is.Resolution != "Profile gw changed. The next requests show whether it works." {
		t.Fatalf("issue = %+v", is)
	}
}

func TestIssuesStayAsTheyWereOnceEnded(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
	sv.recordRequest(sess.public.ID, modelRequest(sess, 429, upstreamError(failureContext{status: 429}, "slow down")))
	if err := sv.closeSession(sess.public.ID, "test"); err != nil {
		t.Fatal(err)
	}
	before := sess.snapshot()
	sv.recordRequest(sess.public.ID, modelRequest(sess, 200, nil))
	sv.recordError(sess.public.ID, model.ErrorRecord{Timestamp: time.Now(), ErrorClass: "upstream_unreachable", UpstreamHost: "gw.example"})
	after := sess.snapshot()
	if !reflect.DeepEqual(after.Issues, before.Issues) || after.Health != before.Health {
		t.Fatalf("issues changed after the session ended:\n%+v\n%+v", before.Issues, after.Issues)
	}
}

// Every change to the issues reaches subscribers as session_updated, so a
// page never has to work Health out itself.
func TestIssueChangesBroadcastSessionUpdated(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()
	awaitHealth := func(want model.SessionHealth) {
		t.Helper()
		deadline := time.After(2 * time.Second)
		for {
			select {
			case ev := <-ch:
				if s, ok := ev.Data.(model.Session); ok && ev.Type == "session_updated" && s.Health == want {
					return
				}
			case <-deadline:
				t.Fatalf("no session_updated with Health %q", want)
			}
		}
	}
	sv.recordRequest(sess.public.ID, modelRequest(sess, 429, upstreamError(failureContext{status: 429}, "slow down")))
	awaitHealth(model.HealthWarn)
	sv.recordRequest(sess.public.ID, modelRequest(sess, 200, nil))
	awaitHealth(model.HealthOK)
}

// The v1 session carries the issues, on the wire names the UI reads.
func TestAPIv1SessionCarriesIssues(t *testing.T) {
	up := diagUpstream(t, false)
	paths, sess := routedSessionPaths(t, diagRoute(up.URL, map[string]string{"claude-opus-5-5": "gpt-6"}))
	send := messagesClient(t, paths.CABundlePath, sess)
	if status, _ := send("claude-fable-5-1"); status != 404 {
		t.Fatalf("unaliased request status = %d", status)
	}
	waitForRequestRecord(t, control.NewClient(paths.SocketPath), sess.ID, "the unaliased request", func(r model.RequestRecord) bool { return r.ModelUnmapped })
	status, got := apiCall(t, paths.SocketPath, "GET", "/api/v1/session", "")
	issues, _ := got["issues"].([]any)
	if status != 200 || len(issues) != 1 || got["session_health"] != "warn" {
		t.Fatalf("session: %d %v", status, got)
	}
	is := issues[0].(map[string]any)
	fixes, _ := is["fixes"].([]any)
	if is["id"] != "unaliased_model" || is["state"] != "open" || is["severity"] != "warn" || is["title_code"] != "claude-fable-5-1" ||
		is["evidence"] != "404 model not found: claude-fable-5-1 · 1 request" || is["last_seen_at"] == nil || len(fixes) != 2 {
		t.Fatalf("issue: %v", is)
	}
	fix := fixes[0].(map[string]any)
	if fix["label"] != "Send them to gpt-6" || fix["action"] != "model_default" || fix["value"] != "gpt-6" || fix["primary"] != true {
		t.Fatalf("fix: %v", fix)
	}
}

// A resolved issue reads as it did while open, whatever route came after.
func TestResolvedIssueKeepsItsWording(t *testing.T) {
	missing := issueRoute("https://gw.example/", "gw", nil)
	missing.OverrideAuth = nil
	missing.AuthBootstrap = model.AuthBootstrapMissing
	missing.MissingAuthEnv = "GW_KEY"
	sv, sess := issueSession(t, missing)
	setIssueRoute(t, sv, sess, issueRoute("https://other.example/", "other", nil))
	is := onlyIssue(t, sess)
	if is.State != model.IssueResolved || is.Title != "$GW_KEY is not set" || !strings.HasPrefix(is.Detail, "Profile gw ") || is.Resolution != "Switched to profile other." {
		t.Fatalf("issue = %+v", is)
	}
}

// An unaliased model's evidence quotes only the failures the missing alias
// caused.
func TestUnaliasedEvidenceQuotesItsOwnFailures(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", map[string]string{"claude-sonnet-5-5": "gpt-6"}))
	rec := modelRequest(sess, 401, upstreamError(failureContext{status: 401, unmapped: true, model: "claude-opus-5-5", profile: "gw"}, "bad key"))
	rec.Model, rec.ModelUnmapped = "claude-opus-5-5", true
	sv.recordRequest(sess.public.ID, rec)
	got := sess.snapshot().Issues
	if len(got) != 2 || got[0].ID != model.IssueUnaliasedModel || got[0].Evidence != "1 request" ||
		got[1].ID != model.IssueCredentialsRejected || got[1].Evidence != "401 bad key · 1 request" {
		t.Fatalf("issues = %+v", got)
	}
}

func TestIssueBadge(t *testing.T) {
	open := func(id string, count int) model.Issue {
		return model.Issue{ID: id, State: model.IssueOpen, Count: count}
	}
	resolved := model.Issue{ID: model.IssueRateLimited, State: model.IssueResolved, Count: 3}
	for _, c := range []struct {
		name   string
		state  model.SessionState
		issues []model.Issue
		want   string
	}{
		{"none open", model.StateActive, []model.Issue{resolved}, ""},
		{"failed requests", model.StateActive, []model.Issue{resolved, open(model.IssueCredentialsRejected, 4)}, "4 failed requests"},
		{"one failed request", model.StateActive, []model.Issue{open(model.IssueRateLimited, 1)}, "1 failed request"},
		{"too long", model.StateActive, []model.Issue{open(model.IssueRequestsFailing, 250)}, "250 failed"},
		{"unaliased", model.StateActive, []model.Issue{open(model.IssueUnaliasedModel, 9)}, "1 issue"},
		{"nothing refused yet", model.StateActive, []model.Issue{open(model.IssueAuthMissing, 0)}, "1 issue"},
		{"several", model.StateActive, []model.Issue{open(model.IssueUnaliasedModel, 2), open(model.IssueRateLimited, 1)}, "2 issues"},
		{"ended", model.StateEnded, []model.Issue{open(model.IssueRateLimited, 1)}, ""},
	} {
		got := issueBadge(model.Session{State: c.state, Issues: c.issues})
		if got != c.want || len(got) > 18 {
			t.Errorf("%s: badge = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestLastActivityAt(t *testing.T) {
	sv, sess := issueSession(t, issueRoute("https://gw.example/", "gw", nil))
	before := sess.snapshot().LastActivityAt
	at := time.Now().Add(time.Second)
	sv.recordRequest(sess.public.ID, model.RequestRecord{Timestamp: at, SessionID: sess.public.ID, Method: "GET", Path: "/v1/models", StatusCode: 200})
	got := sess.snapshot().LastActivityAt
	if got == nil || !got.Equal(at) || got == before {
		t.Fatalf("LastActivityAt = %v, want %v", got, at)
	}
	// An older entry does not move it back.
	sv.recordError(sess.public.ID, model.ErrorRecord{Timestamp: at.Add(-time.Minute), ErrorClass: "upstream_unreachable"})
	if got := sess.snapshot().LastActivityAt; !got.Equal(at) {
		t.Fatalf("LastActivityAt moved back to %v", got)
	}
}

// An error changes the session's issue count and health, so it is followed
// by a session update carrying them.
func TestRecordErrorBroadcastsSessionUpdate(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)
	ch, unsubscribe := sv.subscribe()
	defer unsubscribe()
	sv.recordError(sess.public.ID, model.ErrorRecord{ErrorClass: "upstream_unreachable", Summary: "boom"})
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != "session_updated" {
				continue
			}
			if s, ok := ev.Data.(model.Session); !ok || s.RecentErrorCount != 1 {
				t.Fatalf("session_updated data = %+v", ev.Data)
			}
			return
		case <-deadline:
			t.Fatal("no session_updated after recordError")
		}
	}
}
