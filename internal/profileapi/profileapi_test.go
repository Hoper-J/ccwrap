package profileapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/profiles"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	return NewStore(dir)
}

func mustAdd(t *testing.T, s *Store, req AddRequest) Saved {
	t.Helper()
	saved, e := s.Add(req)
	if e != nil {
		t.Fatalf("add %s: %+v", req.Name, e)
	}
	return saved
}

func gw(name string) AddRequest {
	return AddRequest{Name: name, Provider: "Gateway", BaseURL: "https://" + name + ".example", Auth: &AuthInput{Mode: "ccwrap_bearer", Key: "sk-" + name + "-secret"}}
}

func TestAddStartsAFileWithOfficial(t *testing.T) {
	s := newStore(t)
	saved := mustAdd(t, s, gw("gw"))
	if saved.Default != profiles.OfficialProfileName || saved.Profile.Name != "gw" {
		t.Fatalf("saved = %+v", saved)
	}
	f, err := s.Load()
	if err != nil || f == nil || len(f.Profiles) != 2 {
		t.Fatalf("file = %+v, %v", f, err)
	}
	if _, e := s.Add(gw("gw")); e == nil || e.Status != http.StatusConflict || e.Kind != "conflict" {
		t.Fatalf("duplicate add: %+v", e)
	}
	if saved := mustAdd(t, s, AddRequest{Name: "gw2", BaseURL: "https://gw2.example", SetDefault: true}); saved.Default != "gw2" {
		t.Fatalf("set_default: %+v", saved)
	}
}

func TestAddRefusesBadInput(t *testing.T) {
	s := newStore(t)
	for _, c := range []struct {
		req    AddRequest
		status int
		kind   string
	}{
		{AddRequest{Name: " gw", BaseURL: "https://x"}, 400, "usage"},
		{AddRequest{Name: "gw"}, 400, "usage"},
		{AddRequest{Name: "gw", BaseURL: "https://x", Egress: EgressInput{Mode: "direct", URL: "http://p:1"}}, 422, "validation_error"},
		{AddRequest{Name: "gw", BaseURL: "ftp://x"}, 422, "validation_error"},
	} {
		if _, e := s.Add(c.req); e == nil || e.Status != c.status || e.Kind != c.kind {
			t.Errorf("%+v: %+v, want %d %s", c.req, e, c.status, c.kind)
		}
	}
	if f, _ := s.Load(); f != nil {
		t.Fatal("a refused add wrote the file")
	}
}

func TestEdit(t *testing.T) {
	s := newStore(t)
	mustAdd(t, s, gw("gw"))
	base := "https://gw.example/anthropic"
	aliases := map[string]string{"claude-opus-5-5": "gpt-6"}
	saved, e := s.Edit(EditRequest{Name: "gw", BaseURL: &base, ModelAliases: &aliases, Auth: json.RawMessage(`{"key_source":"env_var","key_env":"GW_KEY"}`)})
	if e != nil {
		t.Fatal(e)
	}
	p := saved.Profile
	if p.BaseURL != base || p.ModelAliases["claude-opus-5-5"] != "gpt-6" || p.Auth.KeyEnv != "GW_KEY" || p.Auth.Key != "" || p.Auth.Mode != "ccwrap_bearer" {
		t.Fatalf("edited = %+v / %+v", p, p.Auth)
	}
	if _, e := s.Edit(EditRequest{Name: "gw", Auth: json.RawMessage(`null`)}); e != nil {
		t.Fatal(e)
	}
	if got, _ := s.Get("gw"); got.Auth != nil {
		t.Fatalf("auth null kept %+v", got.Auth)
	}
	if _, e := s.Edit(EditRequest{Name: "nope"}); e == nil || e.Status != http.StatusNotFound {
		t.Fatalf("unknown profile: %+v", e)
	}
	if _, e := s.Edit(EditRequest{Name: "gw", Auth: json.RawMessage(`{"key_source":"vault"}`)}); e == nil || e.Kind != "validation_error" {
		t.Fatalf("bad key_source: %+v", e)
	}
}

func TestRemoveAndFallbackDefault(t *testing.T) {
	s := newStore(t)
	mustAdd(t, s, gw("beta"))
	mustAdd(t, s, AddRequest{Name: "alpha", BaseURL: "https://alpha.example", SetDefault: true})
	if _, e := s.Remove("alpha", "beta"); e == nil || e.Kind != "usage" {
		t.Fatalf("confirm mismatch: %+v", e)
	}
	removed, e := s.Remove("alpha", " alpha ")
	if e != nil || removed.Default != profiles.OfficialProfileName || removed.Remaining != 2 {
		t.Fatalf("removed = %+v, %+v", removed, e)
	}
	if _, e := s.Remove("alpha", "alpha"); e == nil || e.Status != http.StatusNotFound {
		t.Fatalf("second removal: %+v", e)
	}
}

func TestChooseFallbackDefault(t *testing.T) {
	f := &profiles.File{Profiles: map[string]profiles.Profile{"zeta": {}, "beta": {}}}
	if got := ChooseFallbackDefault(f, "zeta"); got != "beta" {
		t.Fatalf("got %q, want the alphabetically first remaining", got)
	}
	f.Profiles[profiles.OfficialProfileName] = profiles.Profile{}
	if got := ChooseFallbackDefault(f, "zeta"); got != profiles.OfficialProfileName {
		t.Fatalf("got %q, want official", got)
	}
	if got := ChooseFallbackDefault(&profiles.File{Profiles: map[string]profiles.Profile{"only": {}}}, "only"); got != profiles.InheritEnv {
		t.Fatalf("got %q, want inherit-env", got)
	}
}

func TestSetDefault(t *testing.T) {
	s := newStore(t)
	if _, e := s.SetDefault("gw"); e == nil || e.Status != http.StatusNotFound {
		t.Fatalf("no file: %+v", e)
	}
	mustAdd(t, s, gw("gw"))
	if def, e := s.SetDefault("gw"); e != nil || def != "gw" {
		t.Fatalf("%q %+v", def, e)
	}
	if def, e := s.SetDefault(profiles.InheritEnv); e != nil || def != profiles.OfficialProfileName {
		t.Fatalf("inherit-env stands for official: %q %+v", def, e)
	}
}

func TestSetModelDefault(t *testing.T) {
	s := newStore(t)
	req := gw("gw")
	req.ModelAliases = map[string]string{"claude-opus-5-5": "gpt-6"}
	mustAdd(t, s, req)
	if e := s.SetModelDefault("gw", "gpt-6"); e != nil {
		t.Fatal(e)
	}
	p, _ := s.Get("gw")
	if p.ModelAliases["*"] != "gpt-6" || p.ModelAliases["claude-opus-5-5"] != "gpt-6" {
		t.Fatalf("aliases = %v", p.ModelAliases)
	}
	if e := s.SetModelDefault("nope", "gpt-6"); e == nil || e.Status != http.StatusNotFound {
		t.Fatalf("unknown profile: %+v", e)
	}
}

// Concurrent edits in one process take turns: every add lands.
func TestConcurrentAddsAllLand(t *testing.T) {
	s := newStore(t)
	var wg sync.WaitGroup
	for _, name := range []string{"a1", "a2", "a3", "a4", "a5", "a6"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			if _, e := s.Add(gw(name)); e != nil {
				t.Error(e)
			}
		}(name)
	}
	wg.Wait()
	if f, _ := s.Load(); len(f.Profiles) != 7 {
		t.Fatalf("profiles = %d, want 6 + official", len(f.Profiles))
	}
}

func TestValidationErrorWording(t *testing.T) {
	perr := &profiles.ParseErrors{
		Source: "profile edit glm",
		Items: []profiles.ValidationError{
			{Path: "profiles.glm.base_url", Want: "URL with http or https", Got: "ftp://x"},
			{Path: "profiles.glm.auth.mode", Want: "ccwrap_bearer | ccwrap_x_api_key"},
		},
	}
	e := validationError(perr)
	if e.Status != http.StatusUnprocessableEntity || e.Kind != "validation_error" ||
		!strings.HasPrefix(e.Message, "profile edit glm invalid: 2 errors") || !strings.Contains(e.Message, "profiles.glm.base_url") ||
		len(e.Paths) != 2 || e.Paths[0] != "profiles.glm.base_url" || e.Paths[1] != "profiles.glm.auth.mode" {
		t.Fatalf("%+v", e)
	}
}

// A source that is a path (a caller forwarding profiles.Load's error) is cut
// to its base name, so no directory reaches the wire.
func TestValidationErrorHidesDiskPaths(t *testing.T) {
	e := validationError(&profiles.ParseErrors{
		Source: "/Users/home/.local/state/ccwrap/profiles.json",
		Items:  []profiles.ValidationError{{Path: "profiles.foo.base_url", Want: "URL"}},
	})
	for _, banned := range []string{"/Users/", "/home/", "/state/", "/.local/", "/ccwrap/"} {
		if strings.Contains(e.Message, banned) {
			t.Fatalf("disk path leaked: %q in %q", banned, e.Message)
		}
	}
	if !strings.HasPrefix(e.Message, "profiles.json invalid:") {
		t.Fatalf("message = %q", e.Message)
	}
}

// An edit the file's validation refuses never names the file's path.
func TestStoreErrorsNeverNameThePath(t *testing.T) {
	s := newStore(t)
	mustAdd(t, s, gw("gw"))
	bad := "ftp://nope"
	_, e := s.Edit(EditRequest{Name: "gw", BaseURL: &bad})
	if e == nil || e.Kind != "validation_error" || strings.Contains(e.Message, filepath.Dir(s.Path())) {
		t.Fatalf("%+v", e)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveInheritEnv(t *testing.T) {
	p, err := ResolveInheritEnv(env(map[string]string{"ANTHROPIC_API_KEY": "sk-fake"}))
	if err != nil || p.Name != profiles.InheritEnv || p.Auth.Mode != "ccwrap_x_api_key" || p.Auth.Key != "sk-fake" || p.BaseURL != "https://api.anthropic.com" {
		t.Fatalf("api key: %+v %+v %v", p, p.Auth, err)
	}
	p, _ = ResolveInheritEnv(env(map[string]string{"ANTHROPIC_AUTH_TOKEN": "tok-fake", "ANTHROPIC_BASE_URL": "https://custom.example"}))
	if p.Auth.Mode != "ccwrap_bearer" || p.Auth.Key != "tok-fake" || p.BaseURL != "https://custom.example" {
		t.Fatalf("bearer: %+v %+v", p, p.Auth)
	}
	p, _ = ResolveInheritEnv(env(map[string]string{"ANTHROPIC_API_KEY": "sk-wins", "ANTHROPIC_AUTH_TOKEN": "tok-loses"}))
	if p.Auth.Mode != "ccwrap_x_api_key" {
		t.Fatalf("ANTHROPIC_API_KEY must win: %+v", p.Auth)
	}
	if _, err := ResolveInheritEnv(env(nil)); !errors.Is(err, ErrInheritEnvMissing) {
		t.Fatalf("no env: %v", err)
	}
}

func TestProfileForTest(t *testing.T) {
	s := newStore(t)
	if _, err := s.ProfileForTest("gw", env(nil)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no file: %v", err)
	}
	mustAdd(t, s, gw("gw"))
	if p, err := s.ProfileForTest("gw", env(nil)); err != nil || p.Name != "gw" || p.Auth.Key != "sk-gw-secret" {
		t.Fatalf("%+v %v", p, err)
	}
	if err := os.WriteFile(s.Path(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ProfileForTest("gw", env(nil)); !errors.Is(err, ErrConfig) {
		t.Fatalf("broken file: %v", err)
	}
	for _, c := range []struct {
		err    error
		status int
	}{{ErrNotFound, 404}, {ErrInheritEnvMissing, 404}, {ErrConfig, 500}, {errors.New("x"), 500}} {
		if status, _ := TestErrorStatus(c.err); status != c.status {
			t.Errorf("%v: %d, want %d", c.err, status, c.status)
		}
	}
}
