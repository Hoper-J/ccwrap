package supervisor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

func TestWriteMutationError_SetsStatusAndBody(t *testing.T) {
	w := httptest.NewRecorder()
	writeMutationError(w, http.StatusBadRequest, "usage", "bad json")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type: got %q", ct)
	}
	var resp mutationResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.OK || resp.Kind != "usage" || resp.Message != "bad json" {
		t.Fatalf("got %+v", resp)
	}
}

func TestWriteMutationSuccess_SetsItemAndDefault(t *testing.T) {
	w := httptest.NewRecorder()
	writeMutationSuccess(w, nil, true, "alpha")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", w.Code)
	}
	var resp mutationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.OK || !resp.HasProfilesFile || resp.Default != "alpha" {
		t.Fatalf("got %+v", resp)
	}
}

func TestWriteMutationResponse_HasProfilesFile_FalseIsEmitted(t *testing.T) {
	w := httptest.NewRecorder()
	writeMutationError(w, http.StatusNotFound, "not_found", "no profiles.json")
	body := w.Body.String()
	if !strings.Contains(body, `"has_profiles_file":false`) {
		t.Fatalf("expected has_profiles_file:false in JSON; got %s", body)
	}
}

func TestWriteMutationValidationError_FormatsMultiLine(t *testing.T) {
	perr := &profiles.ParseErrors{
		Source: "profile edit glm",
		Items: []profiles.ValidationError{
			{Path: "profiles.glm.base_url", Want: "URL with http or https", Got: "ftp://x"},
			{Path: "profiles.glm.auth.mode", Want: "ccwrap_bearer | ccwrap_x_api_key"},
		},
	}
	w := httptest.NewRecorder()
	writeMutationValidationError(w, perr)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", w.Code)
	}
	var resp mutationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Kind != "validation_error" {
		t.Fatalf("kind: got %q", resp.Kind)
	}
	if !strings.Contains(resp.Message, "profile edit glm invalid: 2 errors") {
		t.Fatalf("message header missing: %q", resp.Message)
	}
	if !strings.Contains(resp.Message, "profiles.glm.base_url") {
		t.Fatalf("first item missing: %q", resp.Message)
	}
	if len(resp.ErrorPaths) != 2 ||
		resp.ErrorPaths[0] != "profiles.glm.base_url" ||
		resp.ErrorPaths[1] != "profiles.glm.auth.mode" {
		t.Fatalf("error_paths: got %v", resp.ErrorPaths)
	}
}

func TestWriteMutationValidationError_NoDiskPathLeak(t *testing.T) {
	perr := &profiles.ParseErrors{
		Source: "profile add foo",
		Items: []profiles.ValidationError{
			{Path: "profiles.foo.base_url", Want: "URL"},
		},
	}
	w := httptest.NewRecorder()
	writeMutationValidationError(w, perr)
	var resp mutationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	for _, banned := range []string{"/Users/", "profiles.json", "/state/", "StateDir"} {
		if strings.Contains(resp.Message, banned) {
			t.Fatalf("disk path leaked: %q in %q", banned, resp.Message)
		}
	}
}

func TestWriteMutationValidationError_DiskPathSource_Sanitized(t *testing.T) {
	// Caller-contract violation: Source contains a disk path (would happen
	// if a future handler forwarded a Load-produced *ParseErrors directly).
	// The runtime guard must clamp Source to its base name to prevent leak.
	perr := &profiles.ParseErrors{
		Source: "/Users/home/.local/state/ccwrap/profiles.json",
		Items: []profiles.ValidationError{
			{Path: "profiles.foo.base_url", Want: "URL"},
		},
	}
	w := httptest.NewRecorder()
	writeMutationValidationError(w, perr)
	var resp mutationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	// The disk path components must NOT appear in the wire message.
	for _, banned := range []string{"/Users/", "/home/", "/state/", "/.local/", "/ccwrap/"} {
		if strings.Contains(resp.Message, banned) {
			t.Fatalf("disk path leaked through Source: %q in message %q", banned, resp.Message)
		}
	}
	// The base name "profiles.json" SHOULD appear (filepath.Base output).
	if !strings.Contains(resp.Message, "profiles.json invalid:") {
		t.Fatalf("expected basename header; got %q", resp.Message)
	}
}

// TestListSessionsUsingProfile_NoMatch_EmptySlice covers the in-process scan:
// when no reachable session has ActiveProfileName
// matching the query, the helper returns nil/empty. The session is freshly
// created by newTestSessionForCreate so its ActiveProfileName is the
// zero string — querying for "ghost" must not match.
func TestListSessionsUsingProfile_NoMatch_EmptySlice(t *testing.T) {
	_, state := newTestSessionForCreate(t)
	if state.proxy == nil {
		t.Fatal("sess.proxy is nil after CreateSession")
	}
	got := state.proxy.listSessionsUsingProfile("ghost")
	if len(got) != 0 {
		t.Fatalf("listSessionsUsingProfile(\"ghost\") = %v, want empty", got)
	}
}

// TestListSessionsUsingProfile_MatchByActiveProfileName covers the positive
// path: a session whose public.ActiveProfileName == "glm" must surface its
// model.Session.ID in the returned slice. Directly mutating sess.public
// under the session mutex is the minimal seam — the helper only reads
// listSessions() snapshots and the full SwitchProfile machinery is
// orthogonal to this scan-semantics test.
func TestListSessionsUsingProfile_MatchByActiveProfileName(t *testing.T) {
	_, state := newTestSessionForCreate(t)
	if state.proxy == nil {
		t.Fatal("sess.proxy is nil after CreateSession")
	}
	state.mu.Lock()
	state.public.ActiveProfileName = "glm"
	wantID := state.public.ID
	state.mu.Unlock()
	got := state.proxy.listSessionsUsingProfile("glm")
	if len(got) != 1 {
		t.Fatalf("listSessionsUsingProfile(\"glm\") len = %d, want 1; got %v", len(got), got)
	}
	if got[0] != wantID {
		t.Fatalf("listSessionsUsingProfile(\"glm\")[0] = %q, want %q", got[0], wantID)
	}
}
