package control

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
)

// TestRouteError_Error asserts the *RouteError type formats a stable string that
// includes the Code and (when present) Message.
func TestRouteError_Error(t *testing.T) {
	re := &RouteError{Code: "RouteSetupAfterAttach", Message: "post-attach"}
	got := re.Error()
	if got == "" {
		t.Fatal("Error() returned empty string")
	}
	if !strings.Contains(got, "RouteSetupAfterAttach") {
		t.Errorf("Error() = %q, want it to contain the Code", got)
	}
	if !strings.Contains(got, "post-attach") {
		t.Errorf("Error() = %q, want it to contain the Message", got)
	}
}

// TestRouteError_Error_CodeOnly asserts a *RouteError with empty Message still
// formats meaningfully (no trailing ": ").
func TestRouteError_Error_CodeOnly(t *testing.T) {
	re := &RouteError{Code: "RouteSetupAfterAttach"}
	got := re.Error()
	if got == "" {
		t.Fatal("Error() returned empty string for code-only RouteError")
	}
	if !strings.Contains(got, "RouteSetupAfterAttach") {
		t.Errorf("Error() = %q, want it to contain the Code", got)
	}
	if strings.HasSuffix(strings.TrimSpace(got), ":") {
		t.Errorf("Error() = %q, has dangling colon", got)
	}
}

// TestRouteError_ErrorsAs asserts the *RouteError is correctly unwrappable via
// errors.As — the typed-detection contract.
func TestRouteError_ErrorsAs(t *testing.T) {
	re := &RouteError{Code: "X", Message: "y"}
	var err error = re
	var target *RouteError
	if !errors.As(err, &target) {
		t.Fatal("errors.As failed to extract *RouteError")
	}
	if target.Code != "X" {
		t.Errorf("target.Code = %q, want X", target.Code)
	}
	if target.Message != "y" {
		t.Errorf("target.Message = %q, want y", target.Message)
	}
}

// newTestClient spins up a unix-socket-backed http.Server with the caller's
// handler, returns a Client wired to that socket plus a cleanup. Mirrors how
// the real supervisor exposes its control surface.
func newTestClient(t *testing.T, handler http.Handler) (*Client, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("", "ccwrap-control-test-*")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		_ = os.RemoveAll(dir)
		t.Fatal(err)
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	c := NewClient(sock)
	cleanup := func() {
		_ = srv.Close()
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	}
	return c, cleanup
}

// TestSetRoute_Returns409TypedRouteError asserts a /route 409 response whose
// body parses as {"reason_code", "message"} surfaces as a *RouteError on the
// client.
func TestSetRoute_Returns409TypedRouteError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/sess1/route", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"reason_code": "RouteSetupAfterAttach",
			"message":     "/route refused: session is in state \"active\" (post-attach)",
		})
	})
	c, cleanup := newTestClient(t, mux)
	defer cleanup()

	err := c.SetRoute(context.Background(), "sess1", model.SessionRouteRequest{APIBaseURL: "https://api.anthropic.com"})
	if err == nil {
		t.Fatal("SetRoute returned nil error on 409, want *RouteError")
	}
	var re *RouteError
	if !errors.As(err, &re) {
		t.Fatalf("SetRoute err is not *RouteError: got %T: %v", err, err)
	}
	if re.Code != "RouteSetupAfterAttach" {
		t.Errorf("re.Code = %q, want RouteSetupAfterAttach", re.Code)
	}
	if !strings.Contains(re.Message, "post-attach") {
		t.Errorf("re.Message = %q, want it to mention post-attach", re.Message)
	}
}

// TestSetRoute_409UntypedBodyFallsBackToGenericError asserts a non-typed 409
// body (no reason_code) does NOT surface as *RouteError; it falls through to
// today's generic error path, preserving backward compatibility.
func TestSetRoute_409UntypedBodyFallsBackToGenericError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/sess1/route", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte("plain text not JSON"))
	})
	c, cleanup := newTestClient(t, mux)
	defer cleanup()

	err := c.SetRoute(context.Background(), "sess1", model.SessionRouteRequest{APIBaseURL: "https://api.anthropic.com"})
	if err == nil {
		t.Fatal("SetRoute returned nil on 409, want error")
	}
	var re *RouteError
	if errors.As(err, &re) {
		t.Errorf("err surfaced as *RouteError unexpectedly: %v", re)
	}
}

// TestSetRoute_2xxReturnsNil sanity-checks the happy path: a 2xx response from
// the supervisor flows through with err == nil (typed branch must not regress
// the success path).
func TestSetRoute_2xxReturnsNil(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/sess1/route", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	c, cleanup := newTestClient(t, mux)
	defer cleanup()

	err := c.SetRoute(context.Background(), "sess1", model.SessionRouteRequest{APIBaseURL: "https://api.anthropic.com"})
	if err != nil {
		t.Fatalf("SetRoute err on 2xx: %v", err)
	}
}

// TestSwitchProfile_DecodesOutcomeView asserts the SwitchProfile client method
// posts to the supervisor's profile-switch endpoint and decodes the JSON body
// into a *SwitchOutcomeView with both the structured fields and the View
// opaque-passthrough preserved.
func TestSwitchProfile_DecodesOutcomeView(t *testing.T) {
	wireBody := `{
		"result": "switched",
		"class": "live",
		"view": {"name":"alpha","provider_label":"Anthropic","base_url_host":"api.anthropic.com","model_alias_count":0,"egress_summary":"direct","relaunch_class":"live","auth_policy":"first_party_passthrough"},
		"reason_code": "",
		"message": ""
	}`
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/sess1/profile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("SwitchProfile used method %q, want POST", r.Method)
		}
		var got map[string]string
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode SwitchProfile body: %v", err)
		}
		if got["name"] != "alpha" {
			t.Errorf("SwitchProfile body.name = %q, want alpha", got["name"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(wireBody))
	})
	c, cleanup := newTestClient(t, mux)
	defer cleanup()

	out, err := c.SwitchProfile(context.Background(), "sess1", "alpha")
	if err != nil {
		t.Fatalf("SwitchProfile err: %v", err)
	}
	if out == nil {
		t.Fatal("SwitchProfile returned nil outcome")
	}
	if out.Result != "switched" {
		t.Errorf("Result = %q, want switched", out.Result)
	}
	if out.Class != "live" {
		t.Errorf("Class = %q, want live", out.Class)
	}
	if len(out.View) == 0 {
		t.Errorf("View raw is empty; want passthrough JSON")
	}
	// View must be decodable as the profile view shape — assert at least the
	// Name field round-trips.
	var view map[string]any
	if err := json.Unmarshal(out.View, &view); err != nil {
		t.Fatalf("decode View: %v", err)
	}
	if view["name"] != "alpha" {
		t.Errorf("View.name = %v, want alpha", view["name"])
	}
}

// TestSwitchProfile_DecodesRefusedOutcome asserts a refused outcome
// (RejectedInvalid with reason_code + message and an empty/absent View)
// flows through without error — the client returns the outcome verbatim, the
// CLI decides how to render it.
func TestSwitchProfile_DecodesRefusedOutcome(t *testing.T) {
	wireBody := `{
		"result": "rejected_invalid",
		"class": "",
		"reason_code": "auth_key_env_error",
		"message": "<redacted>"
	}`
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/sessions/sess1/profile", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(wireBody))
	})
	c, cleanup := newTestClient(t, mux)
	defer cleanup()

	out, err := c.SwitchProfile(context.Background(), "sess1", "broken")
	if err != nil {
		t.Fatalf("SwitchProfile err: %v", err)
	}
	if out.Result != "rejected_invalid" {
		t.Errorf("Result = %q, want rejected_invalid", out.Result)
	}
	if out.ReasonCode != "auth_key_env_error" {
		t.Errorf("ReasonCode = %q, want auth_key_env_error", out.ReasonCode)
	}
	if out.Message != "<redacted>" {
		t.Errorf("Message = %q, want <redacted>", out.Message)
	}
}
