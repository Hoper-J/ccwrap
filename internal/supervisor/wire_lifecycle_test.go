package supervisor

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// TestPopoverWireLifecycle exercises all 4 mutation endpoints in sequence.
// This is "wire e2e" — pure HTTP round-trip; no browser driver.
func TestPopoverWireLifecycle(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	path := profiles.DefaultPath(dir)

	post := func(endpoint, body string) (*http.Response, mutationResponse) {
		req, _ := http.NewRequest(http.MethodPost,
			"http://"+sess.ProxyListenAddr+endpoint,
			strings.NewReader(body))
		req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		defer resp.Body.Close()
		var env mutationResponse
		_ = json.NewDecoder(resp.Body).Decode(&env)
		return resp, env
	}

	// 1. Add foo with set_default=true.
	resp, env := post("/profile/add", `{
		"name":"foo",
		"base_url":"https://foo.example.com",
		"auth":{"mode":"ccwrap_bearer","key":"sk-foo"},
		"set_default":true
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 1 add foo: %d", resp.StatusCode)
	}
	if env.Default != "foo" {
		t.Fatalf("step 1: default got %q", env.Default)
	}

	// 2. Edit foo --provider acme
	resp, _ = post("/profile/edit", `{"name":"foo","provider":"acme"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 2 edit: %d", resp.StatusCode)
	}

	// 3. Add bar with inline key
	resp, _ = post("/profile/add", `{
		"name":"bar",
		"base_url":"https://bar.example.com",
		"auth":{"mode":"ccwrap_bearer","key":"sk-bar-secret"}
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 3 add bar: %d", resp.StatusCode)
	}

	// 4. Set-default bar
	resp, env = post("/profile/set-default", `{"name":"bar"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 4 set-default: %d", resp.StatusCode)
	}
	if env.Default != "bar" {
		t.Fatalf("step 4: default got %q", env.Default)
	}

	// 5. Rm foo (not default — quiet)
	resp, _ = post("/profile/rm", `{"name":"foo","confirm_name":"foo"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 5 rm foo: %d", resp.StatusCode)
	}

	// 6. Rm bar (was default — fallback chooses official, which is
	// present since /profile/add seeded it in step 1).
	resp, env = post("/profile/rm", `{"name":"bar","confirm_name":"bar"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 6 rm bar: %d", resp.StatusCode)
	}
	if env.Default != profiles.OfficialProfileName {
		t.Fatalf("step 6: default got %q, want %q (fallback to official)",
			env.Default, profiles.OfficialProfileName)
	}
	if env.LenItems == nil || *env.LenItems != 1 {
		t.Fatalf("step 6: LenItems got %v, want 1 (official remaining)", env.LenItems)
	}

	// 7. File persists at 0o600 with official remaining
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("step 7: file should exist; %v", err)
	}
	if got := st.Mode().Perm(); got != 0o600 {
		t.Fatalf("step 7: file mode got %o, want 0o600", got)
	}
	f, _ := profiles.Load(path)
	if f == nil || f.Default != profiles.OfficialProfileName || len(f.Profiles) != 1 {
		t.Fatalf("step 7: file content got %+v", f)
	}

	// 8. Subsequent /profile/edit on a removed profile returns 404
	resp, _ = post("/profile/edit", `{"name":"foo","provider":"x"}`)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("step 8: rm-then-edit got %d, want 404", resp.StatusCode)
	}
}

// TestPopoverWireLifecycle_NoAuthProfile exercises the Auth=nil branch
// end-to-end: add a no-auth profile (toggle-off path) → set-default →
// edit to add an auth block → edit to remove it (auth:null) → rm.
// Verifies the 3-state RawMessage logic through the full mutation
// envelope and persistence layer.
func TestPopoverWireLifecycle_NoAuthProfile(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	path := profiles.DefaultPath(dir)

	post := func(endpoint, body string) (*http.Response, mutationResponse) {
		req, _ := http.NewRequest(http.MethodPost,
			"http://"+sess.ProxyListenAddr+endpoint,
			strings.NewReader(body))
		req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", endpoint, err)
		}
		defer resp.Body.Close()
		var env mutationResponse
		_ = json.NewDecoder(resp.Body).Decode(&env)
		return resp, env
	}

	// 1. Add a no-auth profile (auth: null). Wire item.Auth should be nil.
	resp, env := post("/profile/add", `{
		"name":"only-routing",
		"base_url":"https://api.anthropic.com",
		"auth":null,
		"set_default":true
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 1 add: %d", resp.StatusCode)
	}
	if env.Item == nil || env.Item.Auth != nil {
		t.Fatalf("step 1: response item.Auth = %+v, want nil", env.Item.Auth)
	}
	f, _ := profiles.Load(path)
	if f.Profiles["only-routing"].Auth != nil {
		t.Fatalf("step 1: on-disk Auth = %+v, want nil", f.Profiles["only-routing"].Auth)
	}

	// 2. Edit: add an auth block via auth:{...}. Auth=nil → Auth set.
	resp, env = post("/profile/edit", `{
		"name":"only-routing",
		"auth":{"mode":"ccwrap_bearer","key_source":"inline","key":"sk-new"}
	}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 2 add-auth: %d", resp.StatusCode)
	}
	if env.Item == nil || env.Item.Auth == nil || env.Item.Auth.Mode != "ccwrap_bearer" {
		t.Fatalf("step 2: response item.Auth = %+v, want non-nil ccwrap_bearer", env.Item.Auth)
	}
	f, _ = profiles.Load(path)
	got := f.Profiles["only-routing"].Auth
	if got == nil || got.Mode != "ccwrap_bearer" || got.Key != "sk-new" {
		t.Fatalf("step 2: on-disk Auth = %+v", got)
	}

	// 3. Edit again without auth field — Auth must be preserved.
	resp, _ = post("/profile/edit", `{"name":"only-routing","provider":"acme"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 3 no-auth-field: %d", resp.StatusCode)
	}
	f, _ = profiles.Load(path)
	got = f.Profiles["only-routing"].Auth
	if got == nil || got.Mode != "ccwrap_bearer" || got.Key != "sk-new" {
		t.Fatalf("step 3 preserve-auth: on-disk Auth = %+v", got)
	}

	// 4. Edit: remove auth via auth:null. Auth → nil.
	resp, env = post("/profile/edit", `{"name":"only-routing","auth":null}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 4 rm-auth: %d", resp.StatusCode)
	}
	if env.Item == nil || env.Item.Auth != nil {
		t.Fatalf("step 4: response item.Auth = %+v, want nil", env.Item.Auth)
	}
	f, _ = profiles.Load(path)
	if f.Profiles["only-routing"].Auth != nil {
		t.Fatalf("step 4: on-disk Auth must be nil; got %+v", f.Profiles["only-routing"].Auth)
	}

	// 5. Rm the profile.
	resp, _ = post("/profile/rm", `{"name":"only-routing","confirm_name":"only-routing"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 5 rm: %d", resp.StatusCode)
	}
	f, _ = profiles.Load(path)
	if _, exists := f.Profiles["only-routing"]; exists {
		t.Fatal("step 5: profile must be gone from disk")
	}
}

// TestWireLifecycle_EnsureOfficialProfile — auto-restoration through
// the supervisor lifecycle:
//  1. Fresh state dir → EnsureOfficialProfile creates {official}.
//  2. rm official via /profile/rm → file has zero profiles, Default
//     falls back to inherit-env sentinel.
//  3. Re-run EnsureOfficialProfile → official restored, Default
//     migrated from inherit-env back to official.
func TestWireLifecycle_EnsureOfficialProfile(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	path := profiles.DefaultPath(dir)

	// 1. EnsureOfficialProfile on fresh dir.
	if err := profiles.EnsureOfficialProfile(dir); err != nil {
		t.Fatalf("step 1 ensure: %v", err)
	}
	f, _ := profiles.Load(path)
	if _, ok := f.Profiles[profiles.OfficialProfileName]; !ok {
		t.Fatal("step 1: official missing after ensure")
	}

	// 2. rm official via wire.
	req, _ := http.NewRequest(http.MethodPost,
		"http://"+sess.ProxyListenAddr+"/profile/rm",
		strings.NewReader(`{"name":"official","confirm_name":"official"}`))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("step 2 rm: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("step 2 rm: status %d", resp.StatusCode)
	}
	f, _ = profiles.Load(path)
	if _, ok := f.Profiles[profiles.OfficialProfileName]; ok {
		t.Error("step 2: official must be gone after rm")
	}

	// 3. Re-run ensure → official restored, Default migrated.
	if err := profiles.EnsureOfficialProfile(dir); err != nil {
		t.Fatalf("step 3 ensure: %v", err)
	}
	f, _ = profiles.Load(path)
	if _, ok := f.Profiles[profiles.OfficialProfileName]; !ok {
		t.Error("step 3: official must be restored")
	}
	if f.Default != profiles.OfficialProfileName {
		t.Errorf("step 3: Default = %q, want official (migration from inherit-env)", f.Default)
	}
}
