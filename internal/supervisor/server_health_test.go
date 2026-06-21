package supervisor

import (
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// TestRecordError_SetsHealthNotState locks the orthogonality invariant: a
// recorded error updates the session's Health dimension (warn/error per the
// error_class) WITHOUT mutating lifecycle State. recordError no longer drives
// the session to a (now-removed) Degraded state.
func TestRecordError_SetsHealthNotState(t *testing.T) {
	t.Run("error_class maps to HealthError, State unchanged", func(t *testing.T) {
		sv, sess := newTestSessionForCreate(t)

		sess.mu.RLock()
		stateBefore := sess.public.State
		sess.mu.RUnlock()

		sv.recordError(sess.public.ID, model.ErrorRecord{ErrorClass: "upstream_unreachable"})

		sess.mu.RLock()
		stateAfter := sess.public.State
		health := sess.public.Health
		sess.mu.RUnlock()

		if stateAfter != stateBefore {
			t.Errorf("State changed by recordError: before=%q after=%q, want unchanged", stateBefore, stateAfter)
		}
		if health != model.HealthError {
			t.Errorf("Health = %q after recordError(upstream_unreachable), want %q", health, model.HealthError)
		}
	})

	t.Run("ccwrap_auth_missing maps to HealthWarn", func(t *testing.T) {
		sv, sess := newTestSessionForCreate(t)

		sv.recordError(sess.public.ID, model.ErrorRecord{ErrorClass: "ccwrap_auth_missing"})

		sess.mu.RLock()
		health := sess.public.Health
		sess.mu.RUnlock()

		if health != model.HealthWarn {
			t.Errorf("Health = %q after recordError(ccwrap_auth_missing), want %q", health, model.HealthWarn)
		}
	})
}

// TestRecordError_StampsSeverityFromClass locks that recordError is the single
// source of truth for ErrorRecord.Severity: when the caller leaves Severity
// empty, recordError fills it from severityForClass (policy-refusal=warn vs
// failure=error). A caller that DID set Severity is respected verbatim — the
// stamp only fills the empty case.
func TestRecordError_StampsSeverityFromClass(t *testing.T) {
	// lastStoredErr reads the most-recently appended error record under RLock,
	// mirroring the sess.requests read-back idiom used elsewhere in this package.
	lastStoredErr := func(t *testing.T, sess *sessionState) model.ErrorRecord {
		t.Helper()
		sess.mu.RLock()
		defer sess.mu.RUnlock()
		if len(sess.errors) == 0 {
			t.Fatal("session errors ring empty after recordError")
		}
		return sess.errors[len(sess.errors)-1]
	}

	t.Run("auth-missing without Severity stamps warn", func(t *testing.T) {
		sv, sess := newTestSessionForCreate(t)
		sv.recordError(sess.public.ID, model.ErrorRecord{ErrorClass: "ccwrap_auth_missing"})
		if got := lastStoredErr(t, sess).Severity; got != "warn" {
			t.Errorf("stored Severity = %q for ccwrap_auth_missing (no explicit Severity), want %q", got, "warn")
		}
	})

	t.Run("failure class without Severity stamps error", func(t *testing.T) {
		sv, sess := newTestSessionForCreate(t)
		sv.recordError(sess.public.ID, model.ErrorRecord{ErrorClass: "upstream_unreachable"})
		if got := lastStoredErr(t, sess).Severity; got != "error" {
			t.Errorf("stored Severity = %q for upstream_unreachable (no explicit Severity), want %q", got, "error")
		}
	})

	t.Run("explicit Severity is respected, not overwritten", func(t *testing.T) {
		sv, sess := newTestSessionForCreate(t)
		sv.recordError(sess.public.ID, model.ErrorRecord{ErrorClass: "ccwrap_auth_missing", Severity: "error"})
		if got := lastStoredErr(t, sess).Severity; got != "error" {
			t.Errorf("stored Severity = %q; explicit caller value 'error' must be respected, not overwritten by class default 'warn'", got)
		}
	})
}

// TestRecordRequest_HealsToOK locks that a recorded request heals Health back
// to ok without touching lifecycle State.
func TestRecordRequest_HealsToOK(t *testing.T) {
	sv, sess := newTestSessionForCreate(t)

	// Drive Health to error first so the heal is observable.
	sv.recordError(sess.public.ID, model.ErrorRecord{ErrorClass: "upstream_unreachable"})

	sess.mu.RLock()
	stateBefore := sess.public.State
	sess.mu.RUnlock()

	sv.recordRequest(sess.public.ID, model.RequestRecord{
		SessionID: sess.public.ID,
		Method:    "POST",
		Path:      "/v1/messages",
	})

	sess.mu.RLock()
	stateAfter := sess.public.State
	health := sess.public.Health
	sess.mu.RUnlock()

	if stateAfter != stateBefore {
		t.Errorf("State changed by recordRequest: before=%q after=%q, want unchanged", stateBefore, stateAfter)
	}
	if health != model.HealthOK {
		t.Errorf("Health = %q after recordRequest, want %q", health, model.HealthOK)
	}
}
