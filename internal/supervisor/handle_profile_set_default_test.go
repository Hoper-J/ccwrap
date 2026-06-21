package supervisor

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// TestHandleProfileSetDefault_RejectsMissingToken — POST /profile/set-default
// with no X-CCWRAP-Profile-Token must return 403 before any side effect.
// Mirrors TestHandleProfileTest_RejectsMissingToken (probe handler).
func TestHandleProfileSetDefault_RejectsMissingToken(t *testing.T) {
	sess, _, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/set-default"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"foo"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}
}

// TestHandleProfileSetDefault_RejectsWrongMethod — GET (or any non-POST)
// must surface 405. Handler-level 405, not route-fallthrough 405.
func TestHandleProfileSetDefault_RejectsWrongMethod(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/set-default"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status: got %d, want 405", resp.StatusCode)
	}
}

// TestHandleProfileSetDefault_KnownName_Updates — POST with a known
// profile name flips file.Default and returns the mutationResponse
// envelope.
func TestHandleProfileSetDefault_KnownName_Updates(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir) // "alpha" (default) + "beta"

	url := "http://" + sess.ProxyListenAddr + "/profile/set-default"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"beta"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := readAllString(resp.Body)
		t.Fatalf("status: got %d, want 200. body=%q", resp.StatusCode, body)
	}
	var env mutationResponse
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !env.OK || env.Default != "beta" {
		t.Fatalf("response: got %+v", env)
	}
	// File mutation landed.
	f, err := profiles.Load(profiles.DefaultPath(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f == nil {
		t.Fatal("profiles.json missing after set-default")
	}
	if f.Default != "beta" {
		t.Fatalf("file.Default: got %q, want beta", f.Default)
	}
}

// TestHandleProfileSetDefault_InheritEnvSentinel_Updates — the literal
// string "inherit-env" must be accepted even though no such profile
// exists in the map. file.Default flips to the sentinel.
func TestHandleProfileSetDefault_InheritEnvSentinel_Updates(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)

	url := "http://" + sess.ProxyListenAddr + "/profile/set-default"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"inherit-env"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := readAllString(resp.Body)
		t.Fatalf("status: got %d, want 200. body=%q", resp.StatusCode, body)
	}
	var env mutationResponse
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// set-default to "inherit-env" reroutes to the modern equivalent
	// (official profile). The sentinel is no longer surfaced as a
	// default value.
	if env.Default != profiles.OfficialProfileName {
		t.Fatalf("Default: got %q, want %q", env.Default, profiles.OfficialProfileName)
	}
	f, err := profiles.Load(profiles.DefaultPath(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f == nil || f.Default != profiles.OfficialProfileName {
		t.Fatalf("file.Default: %+v", f)
	}
	// official entry should now be present.
	if _, ok := f.Profiles[profiles.OfficialProfileName]; !ok {
		t.Errorf("official entry must be present after set-default reroute")
	}
}

func TestHandleProfileSetDefault_UnknownName_404(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)

	url := "http://" + sess.ProxyListenAddr + "/profile/set-default"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"ghost"}`))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404", resp.StatusCode)
	}
	var env mutationResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Kind != "not_found" {
		t.Fatalf("kind: got %q", env.Kind)
	}
}

func TestHandleProfileSetDefault_MixedCaseInheritEnv_404(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)

	url := "http://" + sess.ProxyListenAddr + "/profile/set-default"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"Inherit-Env"}`))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 (case-sensitive)", resp.StatusCode)
	}
}

func TestHandleProfileSetDefault_EmptyName_400(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)
	url := "http://" + sess.ProxyListenAddr + "/profile/set-default"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":""}`))
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

func TestHandleProfileSetDefault_BadJSON_400(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/set-default"
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
