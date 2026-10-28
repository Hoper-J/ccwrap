package supervisor

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/profileapi"
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

// A refused store edit keeps the mutation envelope, paths included.
func TestWriteStoreError_KeepsTheEnvelope(t *testing.T) {
	w := httptest.NewRecorder()
	writeStoreError(w, &profileapi.Error{Status: http.StatusUnprocessableEntity, Kind: "validation_error", Message: "profile edit glm invalid: 1 errors", Paths: []string{"profiles.glm.base_url"}})
	var resp mutationResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if w.Code != http.StatusUnprocessableEntity || resp.OK || resp.Kind != "validation_error" || len(resp.ErrorPaths) != 1 || resp.ErrorPaths[0] != "profiles.glm.base_url" {
		t.Fatalf("%d %+v", w.Code, resp)
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
