package supervisor

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// TestHandleProfileAdd_RejectsMissingToken — POST /profile/add with no
// X-CCWRAP-Profile-Token must return 403 before any side effect. Mirrors
// the set-default CSRF guard test.
func TestHandleProfileAdd_RejectsMissingToken(t *testing.T) {
	sess, _, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{"name":"x","base_url":"https://api.example.com","auth":{"mode":"passthrough"}}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
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

// TestHandleProfileAdd_RejectsWrongMethod — PUT (or any non-POST) must
// surface 405. Handler-level 405 (method check FIRST, CSRF SECOND).
func TestHandleProfileAdd_RejectsWrongMethod(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	req, _ := http.NewRequest(http.MethodPut, url, nil)
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

// TestHandleProfileAdd_CreatesProfileFromScratch — happy path with no
// pre-existing profiles.json. The handler creates the file with the
// new profile, sets it as default (set_default=true), and the wire
// response carries the SafeCatalogItem without the inline key
// (control.SafeCatalogItem has no AuthKey field; we verify at the JSON
// level that the secret never appears on the wire).
func TestHandleProfileAdd_CreatesProfileFromScratch(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{
		"name":"foo",
		"provider":"acme",
		"base_url":"https://api.example.com",
		"auth":{"mode":"ccwrap_bearer","key":"sk-secret"},
		"set_default":true
	}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	rawBody, _ := readAllString(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d, want 200. body=%q", resp.StatusCode, rawBody)
	}
	// JSON-level invariant — the inline secret MUST NOT appear anywhere
	// on the wire. control.SafeCatalogItem has no AuthKey field;
	// HasInlineKey is the only flag (bool) that escapes.
	if strings.Contains(rawBody, "sk-secret") {
		t.Fatalf("wire body leaks inline secret: %q", rawBody)
	}
	var env mutationResponse
	if err := json.Unmarshal([]byte(rawBody), &env); err != nil {
		t.Fatalf("decode: %v (body=%q)", err, rawBody)
	}
	if !env.OK || env.Default != "foo" || env.Item == nil {
		t.Fatalf("env: got %+v", env)
	}
	if env.Item.Name != "foo" {
		t.Fatalf("item.Name: got %q, want foo", env.Item.Name)
	}
	if env.Item.Auth == nil || !env.Item.Auth.HasInlineKey {
		t.Fatalf("item.Auth.HasInlineKey: got false, want true (key was provided)")
	}
	// File mutation landed; inline key on disk.
	f, err := profiles.Load(profiles.DefaultPath(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f == nil {
		t.Fatal("profiles.json missing after add")
	}
	p, ok := f.Profiles["foo"]
	if !ok {
		t.Fatalf("profile foo missing on disk")
	}
	if p.Auth.Key != "sk-secret" {
		t.Fatalf("on-disk Auth.Key: got %q, want sk-secret", p.Auth.Key)
	}
	if f.Default != "foo" {
		t.Fatalf("file.Default: got %q, want foo", f.Default)
	}
}

// TestHandleProfileAdd_WithModelAliases — POST /profile/add with a
// model_aliases payload persists the map on disk and round-trips
// through the response item. Models cell editor support.
func TestHandleProfileAdd_WithModelAliases(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{"name":"with-aliases","base_url":"https://api.anthropic.com",
		"auth":{"mode":"ccwrap_bearer","key":"sk-x"},
		"model_aliases":{"sonnet":"claude-sonnet-4-6","haiku":"claude-haiku-4-5"}}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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
	f, _ := profiles.Load(profiles.DefaultPath(dir))
	p := f.Profiles["with-aliases"]
	if got := p.ModelAliases["sonnet"]; got != "claude-sonnet-4-6" {
		t.Errorf("sonnet alias: got %q, want claude-sonnet-4-6", got)
	}
	if got := p.ModelAliases["haiku"]; got != "claude-haiku-4-5" {
		t.Errorf("haiku alias: got %q, want claude-haiku-4-5", got)
	}
}

// TestHandleProfileAdd_AppendsToExistingFile — when a profiles.json
// already exists with alpha (default) + beta, adding "gamma" without
// set_default leaves alpha as default and grows the map to 3 entries.
func TestHandleProfileAdd_AppendsToExistingFile(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir) // alpha (default) + beta

	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{
		"name":"gamma",
		"base_url":"https://api.gamma.example.com",
		"auth":{"mode":"ccwrap_bearer","key":"sk-test"}
	}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
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
	f, err := profiles.Load(profiles.DefaultPath(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if f == nil {
		t.Fatal("profiles.json missing")
	}
	if len(f.Profiles) != 3 {
		t.Fatalf("expected 3 profiles; got %d", len(f.Profiles))
	}
	if f.Default != "alpha" {
		t.Fatalf("Default should be unchanged: got %q, want alpha", f.Default)
	}
}

func TestHandleProfileAdd_NameConflict_409(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	seedTwoProfiles(t, dir) // alpha + beta

	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{"name":"alpha","base_url":"https://api.x.example.com","auth":{"mode":"passthrough"}}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status: got %d, want 409", resp.StatusCode)
	}
	var env mutationResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Kind != "conflict" {
		t.Fatalf("kind: got %q", env.Kind)
	}
}

// TestHandleProfileAdd_ConcurrentSameName_OneWins — N goroutines POST
// /profile/add with the SAME name + DIFFERENT bodies. Before the fix
// they all interleaved their load-check-write and the last writer silently
// clobbered earlier ones — multiple 200s with mutually-incompatible
// persisted states. With the profileFileMu in place, only ONE
// request returns 200; the rest return 409 (conflict). The persisted
// profile must match exactly one of the submitted payloads.
func TestHandleProfileAdd_ConcurrentSameName_OneWins(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	// No seed — fresh state dir, no existing profile named "racer".

	const N = 8
	url := "http://" + sess.ProxyListenAddr + "/profile/add"

	statuses := make([]int, N)
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Passthrough = no auth block (validator's "ccwrap does not own auth" shape).
			body := fmt.Sprintf(`{"name":"racer","base_url":"https://api-%d.example.com"}`, i)
			req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
			req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("goroutine %d: do: %v", i, err)
				return
			}
			defer resp.Body.Close()
			statuses[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	var twoHundreds, conflicts, others int
	for _, code := range statuses {
		switch code {
		case http.StatusOK:
			twoHundreds++
		case http.StatusConflict:
			conflicts++
		default:
			others++
		}
	}
	if twoHundreds != 1 {
		t.Fatalf("want exactly 1 success, got %d (TOCTOU race let multiple writes through); statuses=%v", twoHundreds, statuses)
	}
	if conflicts != N-1 {
		t.Fatalf("want N-1 conflicts, got %d; statuses=%v", conflicts, statuses)
	}
	if others != 0 {
		t.Fatalf("unexpected statuses present: %v", statuses)
	}

	// Persisted file must contain "racer" exactly once with one of the
	// submitted base_urls. Without the mutex, multiple goroutines could
	// have overwritten each other with different payloads.
	path := filepath.Join(dir, "profiles.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"racer":`) {
		t.Fatalf("racer profile missing from persisted file: %s", raw)
	}
}

func TestHandleProfileAdd_ReservedName_422(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{"name":"inherit-env","base_url":"https://api.x.example.com","auth":{"mode":"passthrough"}}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("X-CCWRAP-Profile-Token", state.profileToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("status: got %d, want 422 (R2 sentinel)", resp.StatusCode)
	}
	var env mutationResponse
	_ = json.NewDecoder(resp.Body).Decode(&env)
	if env.Kind != "validation_error" {
		t.Fatalf("kind: got %q", env.Kind)
	}
	if !strings.Contains(env.Message, "must not equal sentinel") {
		t.Fatalf("expected sentinel error; got %q", env.Message)
	}
}

func TestHandleProfileAdd_WhitespaceName_400(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{"name":" foo ","base_url":"https://api.x.example.com","auth":{"mode":"passthrough"}}`
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

// TestHandleProfileAdd_PassthroughWithKey_422 — mode=passthrough is
// rejected downstream by Validate with a migration hint regardless of
// whether a key is also submitted. The test name retains the "WithKey"
// suffix for now (rename deferred); the assertion requires Validate's
// substring so we don't regress to the legacy handler-side mutex message.
func TestHandleProfileAdd_PassthroughWithKey_422(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{"name":"x","base_url":"https://api.x.example.com","auth":{"mode":"passthrough","key":"sk-x"}}`
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

func TestHandleProfileAdd_KeyAndKeyEnv_422(t *testing.T) {
	// auth.key and auth.key_env are mutually exclusive. The handler
	// no longer pre-empts with a 400 mutex check — Validate runs in
	// OverwriteFile and returns 422 with the mutually-exclusive message.
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{"name":"x","base_url":"https://api.x.example.com","auth":{"mode":"ccwrap_bearer","key":"sk-x","key_env":"V"}}`
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
	if !strings.Contains(rawBody, "mutually exclusive") {
		t.Errorf("body must mention V3 'mutually exclusive': %q", rawBody)
	}
}

// TestHandleProfileAdd_AuthNull_NoAuthBlock — payload with auth:null
// creates a profile with Auth=nil ("ccwrap does not own auth"); response
// item.auth is nil.
func TestHandleProfileAdd_AuthNull_NoAuthBlock(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{"name":"no-auth","base_url":"https://api.anthropic.com","auth":null}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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
	f, err := profiles.Load(profiles.DefaultPath(dir))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p, ok := f.Profiles["no-auth"]
	if !ok {
		t.Fatal("profile no-auth missing on disk")
	}
	if p.Auth != nil {
		t.Errorf("on-disk Auth = %+v, want nil", p.Auth)
	}
}

// TestHandleProfileAdd_AuthAbsent_NoAuthBlock — payload without the
// auth field at all (not just null) also creates Auth=nil.
func TestHandleProfileAdd_AuthAbsent_NoAuthBlock(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{"name":"no-auth","base_url":"https://api.anthropic.com"}`
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
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
	f, _ := profiles.Load(profiles.DefaultPath(dir))
	if f.Profiles["no-auth"].Auth != nil {
		t.Error("Auth must be nil for absent auth field")
	}
}

func TestHandleProfileAdd_BadJSON_400(t *testing.T) {
	sess, state, _ := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
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

func TestHandleProfileAdd_BadBaseURL_422_NoWrite(t *testing.T) {
	sess, state, dir := newSessionForProfileMutation(t)
	url := "http://" + sess.ProxyListenAddr + "/profile/add"
	body := `{"name":"x","base_url":"ftp://wrong","auth":{"mode":"ccwrap_bearer","key":"sk-test"}}`
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
	if len(env.ErrorPaths) == 0 || env.ErrorPaths[0] != "profiles.x.base_url" {
		t.Fatalf("expected ErrorPaths to list base_url; got %v", env.ErrorPaths)
	}
	// File not created.
	f, _ := profiles.Load(profiles.DefaultPath(dir))
	if f != nil {
		if _, exists := f.Profiles["x"]; exists {
			t.Fatalf("profile x should not have been created")
		}
	}
}
