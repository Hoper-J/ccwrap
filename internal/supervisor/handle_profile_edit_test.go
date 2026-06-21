package supervisor

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

func TestHandleProfileEdit_RejectsMissingToken(t *testing.T) {
	sess, _, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)
	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"alpha"}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", resp.StatusCode)
	}
}

func TestHandleProfileEdit_NotFound_404(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)
	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
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
}

func TestHandleProfileEdit_PartialUpdate_PreservesOtherFields(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	// Seed alpha with detailed fields including model_aliases.
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {
			Name:         "alpha",
			Provider:     "old-provider",
			BaseURL:      "https://api.alpha.example.com",
			Auth:         &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-original"},
			ModelAliases: map[string]string{"shortcut": "claude-x"},
		},
	}}
	if err := profiles.OverwriteFile(profiles.DefaultPath(dir), seed, "test-seed"); err != nil {
		t.Fatalf("seed: %v", err)
	}

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","provider":"new-provider"}`
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
	f, err := profiles.Load(profiles.DefaultPath(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := f.Profiles["alpha"]
	if p.Provider != "new-provider" {
		t.Fatalf("Provider: got %q", p.Provider)
	}
	if p.BaseURL != "https://api.alpha.example.com" {
		t.Fatalf("BaseURL changed: got %q", p.BaseURL)
	}
	if p.Auth.Key != "sk-original" {
		t.Fatalf("Auth.Key changed: got %q", p.Auth.Key)
	}
	if p.ModelAliases["shortcut"] != "claude-x" {
		t.Fatalf("ModelAliases lost: got %+v", p.ModelAliases)
	}
}

// TestHandleProfileEdit_PassthroughAutoClearsKey — removed. The
// behavior it tested (handler's "auth.mode=passthrough auto-clears
// key/key_env") is obsolete; passthrough is no longer a valid mode
// value. Removing auth is now expressed by sending auth:null in the
// submit payload. New coverage lives in TestValidate_RejectsPassthrough
// (internal/profiles/validate_test.go).

// TestHandleProfileEdit_PassthroughWithKeySource_422:
// mode=passthrough is rejected downstream by Validate with a migration
// hint regardless of whether an explicit key_source is also submitted.
// The assertion requires the current substring so we don't regress to
// the legacy handler-side mutex message.
func TestHandleProfileEdit_PassthroughWithKeySource_422(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","auth":{"mode":"passthrough","key_source":"inline","key":"sk-x"}}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", resp.StatusCode)
	}
	rawBody, _ := readAllString(resp.Body)
	if !strings.Contains(rawBody, "passthrough") {
		t.Errorf("body must mention 'passthrough': %q", rawBody)
	}
	if !strings.Contains(rawBody, "remove the auth block") {
		t.Errorf("body must name the V1 fix 'remove the auth block': %q", rawBody)
	}
}

func TestHandleProfileEdit_KeySourceUnchanged_LeavesAuthKeyOnDisk(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://x.example.com",
			Auth: &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-original"}},
	}}
	_ = profiles.OverwriteFile(profiles.DefaultPath(dir), seed, "test-seed")

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","provider":"new-name","auth":{"key_source":"unchanged"}}`
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
	f, _ := profiles.Load(profiles.DefaultPath(dir))
	p := f.Profiles["alpha"]
	if p.Auth.Key != "sk-original" {
		t.Fatalf("Auth.Key changed: got %q", p.Auth.Key)
	}
}

func TestHandleProfileEdit_KeySourceInline_ReplacesAuthKey(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://x.example.com",
			Auth: &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-original"}},
	}}
	_ = profiles.OverwriteFile(profiles.DefaultPath(dir), seed, "test-seed")

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","auth":{"key_source":"inline","key":"sk-NEW"}}`
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
	f, _ := profiles.Load(profiles.DefaultPath(dir))
	p := f.Profiles["alpha"]
	if p.Auth.Key != "sk-NEW" || p.Auth.KeyEnv != "" {
		t.Fatalf("Auth: got %+v", p.Auth)
	}
}

func TestHandleProfileEdit_KeySourceEnv_ReplacesAuthKeyEnv(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://x.example.com",
			Auth: &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-original"}},
	}}
	_ = profiles.OverwriteFile(profiles.DefaultPath(dir), seed, "test-seed")

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","auth":{"key_source":"env_var","key_env":"MY_VAR"}}`
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
	f, _ := profiles.Load(profiles.DefaultPath(dir))
	p := f.Profiles["alpha"]
	if p.Auth.Key != "" || p.Auth.KeyEnv != "MY_VAR" {
		t.Fatalf("Auth: got %+v", p.Auth)
	}
}

func TestHandleProfileEdit_EgressNonHttpClearsURL(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seed := &profiles.File{Default: "alpha", Profiles: map[string]profiles.Profile{
		"alpha": {Name: "alpha", BaseURL: "https://x.example.com",
			Auth:   nil,
			Egress: profiles.EgressSpec{Mode: "http", URL: "http://user:pw@proxy:8080"}},
	}}
	_ = profiles.OverwriteFile(profiles.DefaultPath(dir), seed, "test-seed")

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","egress":{"mode":"inherit"}}`
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
	f, _ := profiles.Load(profiles.DefaultPath(dir))
	p := f.Profiles["alpha"]
	if p.Egress.Mode != "inherit" || p.Egress.URL != "" {
		t.Fatalf("Egress: got %+v", p.Egress)
	}
}

func TestHandleProfileEdit_EgressNonHttpWithURL_422(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","egress":{"mode":"inherit","url":"http://x"}}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", resp.StatusCode)
	}
}

func TestHandleProfileEdit_TargetProfileRemovedConcurrently_404(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)

	path := profiles.DefaultPath(dir)
	f, _ := profiles.Load(path)
	delete(f.Profiles, "alpha")
	f.Default = profiles.InheritEnv
	_ = profiles.OverwriteFile(path, f, "test-remove-alpha")

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","provider":"new"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404 (concurrent removal)", resp.StatusCode)
	}
}

// TestHandleProfileEdit_AuthAbsent_NoChange — payload without an "auth"
// field at all leaves stored Auth unchanged.
func TestHandleProfileEdit_AuthAbsent_NoChange(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	// Seed alpha WITH an auth block so we can verify preservation.
	path := profiles.DefaultPath(dir)
	f := &profiles.File{
		Default: "alpha",
		Profiles: map[string]profiles.Profile{
			"alpha": {
				Name:    "alpha",
				BaseURL: "https://api.anthropic.com",
				Auth:    &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-alpha"},
				Egress:  profiles.EgressSpec{Mode: "inherit"},
			},
		},
	}
	_ = profiles.OverwriteFile(path, f, "seed-alpha")

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","base_url":"https://api.alpha-new.example.com"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rawBody, _ := readAllString(resp.Body)
		t.Fatalf("status: got %d, want 200. body=%q", resp.StatusCode, rawBody)
	}
	f2, _ := profiles.Load(path)
	p := f2.Profiles["alpha"]
	if p.Auth == nil || p.Auth.Mode != "ccwrap_bearer" || p.Auth.Key != "sk-alpha" {
		t.Errorf("Auth preserved? got %+v", p.Auth)
	}
}

// TestHandleProfileEdit_AuthNull_RemovesBlock — payload with auth:null
// explicitly removes the auth block. Toggle-off path from popover.
func TestHandleProfileEdit_AuthNull_RemovesBlock(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	path := profiles.DefaultPath(dir)
	f := &profiles.File{
		Default: "alpha",
		Profiles: map[string]profiles.Profile{
			"alpha": {
				Name:    "alpha",
				BaseURL: "https://api.anthropic.com",
				Auth:    &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk-alpha"},
				Egress:  profiles.EgressSpec{Mode: "inherit"},
			},
		},
	}
	_ = profiles.OverwriteFile(path, f, "seed-alpha-with-auth")

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","auth":null}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rawBody, _ := readAllString(resp.Body)
		t.Fatalf("status: got %d, want 200. body=%q", resp.StatusCode, rawBody)
	}
	f2, _ := profiles.Load(path)
	p := f2.Profiles["alpha"]
	if p.Auth != nil {
		t.Errorf("Auth must be nil after auth:null edit; got %+v", p.Auth)
	}
}

// TestHandleProfileEdit_AuthAddFromNil — payload with auth:{...} on a
// profile that started Auth=nil adds the block.
func TestHandleProfileEdit_AuthAddFromNil(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	// Seed a profile with Auth=nil
	path := profiles.DefaultPath(dir)
	f := &profiles.File{
		Default: "no-auth",
		Profiles: map[string]profiles.Profile{
			"no-auth": {Name: "no-auth", BaseURL: "https://api.anthropic.com", Auth: nil},
		},
	}
	_ = profiles.OverwriteFile(path, f, "seed-no-auth")

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"no-auth","auth":{"mode":"ccwrap_bearer","key_source":"inline","key":"sk-new"}}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		rawBody, _ := readAllString(resp.Body)
		t.Fatalf("status: got %d, want 200. body=%q", resp.StatusCode, rawBody)
	}
	f2, _ := profiles.Load(path)
	p := f2.Profiles["no-auth"]
	if p.Auth == nil || p.Auth.Mode != "ccwrap_bearer" || p.Auth.Key != "sk-new" {
		t.Errorf("Auth = %+v, want non-nil with mode+key set", p.Auth)
	}
}

// TestHandleProfileEdit_ModelAliases_3State — pointer-3-state for the
// edit handler: omitted → no change; explicit empty object → clear;
// explicit object → replace. Supports the Models cell editor.
func TestHandleProfileEdit_ModelAliases_3State(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	path := profiles.DefaultPath(dir)
	seed := &profiles.File{
		Default: "p",
		Profiles: map[string]profiles.Profile{
			"p": {
				Name:    "p",
				BaseURL: "https://api.anthropic.com",
				Auth:    &profiles.AuthSpec{Mode: "ccwrap_bearer", Key: "sk"},
				Egress:  profiles.EgressSpec{Mode: "inherit"},
				ModelAliases: map[string]string{
					"sonnet": "claude-sonnet-4-6",
				},
			},
		},
	}
	_ = profiles.OverwriteFile(path, seed, "seed")

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	do := func(body string) int {
		req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
		req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Case 1: absent → preserve.
	if got := do(`{"name":"p","provider":"new"}`); got != http.StatusOK {
		t.Fatalf("case 1 status: %d", got)
	}
	f, _ := profiles.Load(path)
	if got := f.Profiles["p"].ModelAliases["sonnet"]; got != "claude-sonnet-4-6" {
		t.Errorf("case 1: alias must be preserved, got %q", got)
	}

	// Case 2: explicit replace.
	if got := do(`{"name":"p","model_aliases":{"haiku":"claude-haiku-4-5","opus":"claude-opus-4-7"}}`); got != http.StatusOK {
		t.Fatalf("case 2 status: %d", got)
	}
	f, _ = profiles.Load(path)
	als := f.Profiles["p"].ModelAliases
	if len(als) != 2 || als["haiku"] != "claude-haiku-4-5" || als["opus"] != "claude-opus-4-7" {
		t.Errorf("case 2: aliases = %v, want haiku+opus replacement", als)
	}
	if _, ok := als["sonnet"]; ok {
		t.Errorf("case 2: old 'sonnet' should be gone after replace")
	}

	// Case 3: explicit empty → clear.
	if got := do(`{"name":"p","model_aliases":{}}`); got != http.StatusOK {
		t.Fatalf("case 3 status: %d", got)
	}
	f, _ = profiles.Load(path)
	if len(f.Profiles["p"].ModelAliases) != 0 {
		t.Errorf("case 3: aliases must be cleared, got %v", f.Profiles["p"].ModelAliases)
	}
}

func TestHandleProfileEdit_BadBaseURL_422_WireShape(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)
	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	body := `{"name":"alpha","base_url":"ftp://wrong"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422", resp.StatusCode)
	}
	var env mutationResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Kind != "validation_error" {
		t.Fatalf("kind: got %q", env.Kind)
	}
	if !strings.Contains(env.Message, "profile edit alpha invalid") {
		t.Fatalf("message header: got %q", env.Message)
	}
	if len(env.ErrorPaths) == 0 || env.ErrorPaths[0] != "profiles.alpha.base_url" {
		t.Fatalf("error_paths: got %v", env.ErrorPaths)
	}
}

func TestHandleProfileEdit_EmptyBody_400(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400 (name required)", resp.StatusCode)
	}
}

func TestHandleProfileEdit_NameOnlyBody_200_Noop(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)
	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(`{"name":"alpha"}`))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		bod, _ := readAllString(resp.Body)
		t.Fatalf("status: got %d, want 200 (no-op). body=%q", resp.StatusCode, bod)
	}
}

func TestHandleProfileEdit_OversizedBody_413(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir)

	var b strings.Builder
	b.WriteString(`{"name":"alpha","auth":{"key_source":"inline","key":"`)
	b.WriteString(strings.Repeat("x", 6*1024*1024))
	b.WriteString(`"}}`)

	url := "http://" + sess.ProxyListenAddr + "/profile/edit"
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(b.String()))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413", resp.StatusCode)
	}
}
