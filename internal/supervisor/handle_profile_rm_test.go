package supervisor

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

func TestHandleProfileRm_RejectsMissingToken(t *testing.T) {
	sess, _, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)
	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"beta","confirm_name":"beta"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}
}

func TestHandleProfileRm_RemovesNonDefault(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir) // alpha (default) + beta

	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	body := `{"name":"beta","confirm_name":"beta"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		bod, _ := readAllString(resp.Body)
		t.Fatalf("status: got %d, want 200. body=%q", resp.StatusCode, bod)
	}
	var env mutationResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if !env.OK || env.RemovedName != "beta" || env.Default != "alpha" {
		t.Fatalf("env: got %+v", env)
	}
	if env.LenItems == nil || *env.LenItems != 1 {
		t.Fatalf("LenItems: got %v", env.LenItems)
	}
	f, _ := profiles.Load(profiles.DefaultPath(dir))
	if _, exists := f.Profiles["beta"]; exists {
		t.Fatalf("beta should be gone")
	}
}

func TestHandleProfileRm_ConfirmNameMismatch_400(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)

	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	body := `{"name":"beta","confirm_name":"wrong"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

// TestHandleProfileRm_DefaultProfile_FallsBackToRemaining — when rm
// removes the current default and other profiles remain, the new
// Default is the alphabetically-first remaining one (deterministic),
// per chooseFallbackDefault.
func TestHandleProfileRm_DefaultProfile_FallsBackToRemaining(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir) // alpha (default) + beta

	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	body := `{"name":"alpha","confirm_name":"alpha"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var env mutationResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Default != "beta" {
		t.Fatalf("Default: got %q, want beta (alphabetically-first remaining)", env.Default)
	}
	f, _ := profiles.Load(profiles.DefaultPath(dir))
	if f.Default != "beta" {
		t.Fatalf("on-disk Default: got %q", f.Default)
	}
}

func TestHandleProfileRm_LastProfile_LeavesEmptyFile(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://a.example.com",
			Auth: nil},
	}}
	_ = profiles.OverwriteFile(profiles.DefaultPath(dir), seed, "test-seed")

	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	body := `{"name":"alpha","confirm_name":"alpha"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200", resp.StatusCode)
	}
	var env mutationResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if !env.HasProfilesFile || env.LenItems == nil || *env.LenItems != 0 {
		t.Fatalf("env: got %+v", env)
	}
}

func TestHandleProfileRm_NoSuchProfile_404(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)
	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	body := `{"name":"ghost","confirm_name":"ghost"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", resp.StatusCode)
	}
}

func TestHandleProfileRm_NoProfilesJson_404(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	// Do NOT seed.
	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	body := `{"name":"foo","confirm_name":"foo"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", resp.StatusCode)
	}
}

func TestHandleProfileRm_ActiveSessionReported(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://x.example.com",
			Auth: nil},
		"beta": {Name: "beta", BaseURL: "https://y.example.com",
			Auth: nil},
	}}
	_ = profiles.OverwriteFile(profiles.DefaultPath(dir), seed, "test-seed")

	// Directly mutate public.ActiveProfileName under the session mutex —
	// mirrors the pattern from TestListSessionsUsingProfile_MatchByActiveProfileName
	// (profile_mutation_test.go). No helper exists; the inline
	// 3-line mutation is the minimal seam to record an in-process match.
	state.mu.Lock()
	state.public.ActiveProfileName = "alpha"
	state.mu.Unlock()

	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	body := `{"name":"alpha","confirm_name":"alpha"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (warn-but-proceed)", resp.StatusCode)
	}
	var env mutationResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if len(env.ActiveSessions) == 0 {
		t.Fatalf("expected active_sessions to be populated; got %+v", env)
	}
}

func TestHandleProfileRm_EmptyName_400(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	body := `{"name":"","confirm_name":""}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}

func TestHandleProfileRm_ConfirmNameTrimmed(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)

	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	body := `{"name":"beta","confirm_name":"beta  "}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200 (trim matches)", resp.StatusCode)
	}
}

func TestHandleProfileRm_BadJSON_400(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/rm"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{not json`))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400", resp.StatusCode)
	}
}
