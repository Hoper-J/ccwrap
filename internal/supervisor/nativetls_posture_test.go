package supervisor

import (
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// newTestSessionProxyWithSession builds a *sessionProxy whose sp.session is a
// registered-enough *sessionState that sess.snapshot() reads its public
// projection and recordNativeTLS's touch()/broadcast() calls are safe. A
// zero-value *Supervisor is sufficient: touch() only writes lastActive under
// s.mu and broadcast() ranges a nil subscribers map (a no-op fan-out), so no
// real listener registration is required for the posture assertions.
func newTestSessionProxyWithSession(t *testing.T) (*sessionProxy, *sessionState) {
	t.Helper()
	sess := &sessionState{}
	sess.public.ID = "sess-native-tls-posture"
	sp := &sessionProxy{supervisor: &Supervisor{}, session: sess}
	return sp, sess
}

// sessionHealthIsError reads the real session Health field (the same one
// recordError drives via severityForClass) off the snapshot and reports whether
// it is the error level.
func sessionHealthIsError(t *testing.T, sess *sessionState) bool {
	t.Helper()
	return sess.snapshot().Health == model.HealthError
}

// TestNativeTLSFirstPaintSeed locks the live-cell fix: publishing a route whose
// nativeTLS is on seeds public.NativeTLS="active" at publish time — BEFORE any
// upstream dial — so the dashboard NATIVE TLS cell renders from first paint
// (the JS live-updater only patches an existing cell, never creates one). A
// route with nativeTLS off leaves it empty so no cell is shown.
func TestNativeTLSFirstPaintSeed(t *testing.T) {
	baseReq := func(nativeTLS bool) model.SessionRouteRequest {
		return model.SessionRouteRequest{
			RouteClass:  model.RouteClassFirstParty,
			RouteSource: model.RouteSourceExplicit,
			AuthMode:    model.AuthModePassthrough,
			AuthSource:  model.AuthSourceNone,
			Egress:      model.EgressConfig{Mode: "direct", Source: "test", Summary: "direct"},
			FailPolicy:  model.FailClosed,
			NativeTLS:   nativeTLS,
		}
	}
	t.Run("on => active from first paint", func(t *testing.T) {
		srv, sess := newTestSessionForCreate(t)
		if err := srv.setRoute(sess.public.ID, baseReq(true)); err != nil {
			t.Fatalf("setRoute: %v", err)
		}
		if got := sess.snapshot().NativeTLS; got != "active" {
			t.Fatalf("nativeTLS route must seed public.NativeTLS=active before any dial, got %q", got)
		}
	})
	t.Run("off => empty (no cell)", func(t *testing.T) {
		srv, sess := newTestSessionForCreate(t)
		if err := srv.setRoute(sess.public.ID, baseReq(false)); err != nil {
			t.Fatalf("setRoute: %v", err)
		}
		if got := sess.snapshot().NativeTLS; got != "" {
			t.Fatalf("non-nativeTLS route must leave public.NativeTLS empty, got %q", got)
		}
	})
}

func TestRecordNativeTLSPosture(t *testing.T) {
	sp, sess := newTestSessionProxyWithSession(t) // a sessionProxy whose sp.session==sess and is registered so snapshot()/broadcast work

	sp.recordNativeTLS(true, "")
	if got := sess.snapshot().NativeTLS; got != "active" {
		t.Fatalf("after active: NativeTLS=%q want active", got)
	}

	sp.recordNativeTLS(false, "handshake failed")
	snap := sess.snapshot()
	if snap.NativeTLS != "blocked: handshake failed" {
		t.Fatalf("after block: NativeTLS=%q", snap.NativeTLS)
	}
	if snap.NativeTLSFallbacks != 1 {
		t.Fatalf("block count=%d want 1", snap.NativeTLSFallbacks)
	}
	if !sessionHealthIsError(t, sess) { // assert Health == error (use the real Health accessor)
		t.Fatalf("a native-tls block must drive session Health to error")
	}

	// episode-count, not dial-count: a second identical block (same reason, no
	// state transition) must NOT inflate the counter.
	sp.recordNativeTLS(false, "handshake failed")
	if c := sess.snapshot().NativeTLSFallbacks; c != 1 {
		t.Fatalf("a repeated identical block must not increment the episode count, got %d", c)
	}

	// idempotent on same state: a second active does not spuriously churn the count
	sp.recordNativeTLS(true, "")
	if c := sess.snapshot().NativeTLSFallbacks; c != 1 {
		t.Fatalf("count must not change on re-activate, got %d", c)
	}

	// a fresh block episode after recovery DOES increment (transition into blocked).
	sp.recordNativeTLS(false, "handshake failed")
	if c := sess.snapshot().NativeTLSFallbacks; c != 2 {
		t.Fatalf("a new block episode after recovery must increment, got %d", c)
	}
}

// TestNativeTLSErrorPersistsAcrossSuccessfulRequest locks the observability
// promise: a native-TLS block drives Health=error, and that error MUST survive a
// later successful request (recordRequest) — e.g. one reusing a still-pooled good
// conn — otherwise the error never sticks and the user never SEES that new
// connections are being blocked. When mirroring resumes (recordNativeTLS(true,"")),
// the next successful request restores Health=ok. This wires the
// sess.nativeTLSBlocked flag.
func TestNativeTLSErrorPersistsAcrossSuccessfulRequest(t *testing.T) {
	// A real, registered session so the supervisor's recordRequest path mutates
	// the same *sessionState recordNativeTLS writes (both key off this session).
	srv, sess := newTestSessionForCreate(t)
	sp := &sessionProxy{supervisor: srv, session: sess}

	// Block -> Health=error and nativeTLSBlocked=true.
	sp.recordNativeTLS(false, "handshake failed")
	if got := sess.snapshot().Health; got != model.HealthError {
		t.Fatalf("after block: Health=%q want error", got)
	}

	// A successful request through the REAL recordRequest path must NOT clear the
	// error while we are still blocked.
	srv.recordRequest(sess.public.ID, model.RequestRecord{
		SessionID: sess.public.ID,
		Method:    "POST",
		Path:      "/v1/messages",
	})
	if got := sess.snapshot().Health; got != model.HealthError {
		t.Fatalf("a successful request must NOT clear native-tls error while blocked: Health=%q want error", got)
	}

	// Mirroring resumes; the next successful request restores Health=ok.
	sp.recordNativeTLS(true, "")
	srv.recordRequest(sess.public.ID, model.RequestRecord{
		SessionID: sess.public.ID,
		Method:    "POST",
		Path:      "/v1/messages",
	})
	if got := sess.snapshot().Health; got != model.HealthOK {
		t.Fatalf("after mirror resumes + a successful request: Health=%q want ok", got)
	}
}
