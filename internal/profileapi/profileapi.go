// Package profileapi edits and tests the profiles in profiles.json for
// ccwrap ui and the sessions' API. Every edit runs under the cross-process
// profiles lock, so the CLI, sessions and the ui service can edit the same
// file.
package profileapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// Store edits one state directory's profiles.json.
type Store struct {
	stateDir string
	// mu serializes this process's load-check-write sequences; the file
	// lock (profiles.Lock) covers other processes.
	mu sync.Mutex
}

func NewStore(stateDir string) *Store { return &Store{stateDir: stateDir} }

// Path is the profiles.json the store edits.
func (s *Store) Path() string { return profiles.DefaultPath(s.stateDir) }

// Error is a refused edit: an HTTP status, a stable kind (usage, conflict,
// not_found, validation_error or internal), and for a file that failed
// validation the paths at fault.
type Error struct {
	Status  int
	Kind    string
	Message string
	Paths   []string
}

func (e *Error) Error() string { return e.Message }

func usage(msg string) *Error { return &Error{http.StatusBadRequest, "usage", msg, nil} }
func internal(err error) *Error {
	return &Error{http.StatusInternalServerError, "internal", err.Error(), nil}
}
func notFound(msg string) *Error { return &Error{http.StatusNotFound, "not_found", msg, nil} }

// checkName refuses an empty name or one with surrounding whitespace.
func checkName(name string) *Error {
	if strings.TrimSpace(name) != name || name == "" {
		return usage("name must not be empty or contain leading/trailing whitespace")
	}
	return nil
}

// edit runs fn on the loaded file under both locks and writes the file back
// when fn returns nil. op labels validation errors ("profile add x").
func (s *Store) edit(op string, fn func(f *profiles.File) (*profiles.File, *Error)) *Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := profiles.Lock(s.stateDir)
	if err != nil {
		return internal(err)
	}
	defer unlock()
	path := s.Path()
	f, err := profiles.Load(path)
	if err != nil {
		return internal(err)
	}
	f, e := fn(f)
	if e != nil {
		return e
	}
	if err := profiles.OverwriteFile(path, f, op); err != nil {
		var perr *profiles.ParseErrors
		if errors.As(err, &perr) {
			return validationError(perr)
		}
		return internal(err)
	}
	return nil
}

// validationError words a failed validation without the on-disk path: the
// source must be an operation label ("profile add foo"), never the path
// profiles.Load reports; one that looks like a path is cut to its base name.
func validationError(perr *profiles.ParseErrors) *Error {
	var b strings.Builder
	src := perr.Source
	if strings.ContainsAny(src, "/\\") {
		src = filepath.Base(src)
	}
	fmt.Fprintf(&b, "%s invalid: %d errors", src, len(perr.Items))
	paths := make([]string, 0, len(perr.Items))
	for _, it := range perr.Items {
		fmt.Fprintf(&b, "\n  - %s", it.Error())
		paths = append(paths, it.Path)
	}
	return &Error{http.StatusUnprocessableEntity, "validation_error", b.String(), paths}
}

// Load reads the file; nil when there is none.
func (s *Store) Load() (*profiles.File, error) { return profiles.Load(s.Path()) }

// Get returns profile name, its Name set.
func (s *Store) Get(name string) (profiles.Profile, *Error) {
	f, err := s.Load()
	if err != nil {
		return profiles.Profile{}, internal(err)
	}
	if f == nil {
		return profiles.Profile{}, notFound("no profiles.json present")
	}
	p, ok := f.Profiles[name]
	if !ok {
		return profiles.Profile{}, notFound("no such profile")
	}
	p.Name = name
	return p, nil
}

// AddRequest is a new profile. Auth nil means ccwrap does not own auth for
// it; SetDefault makes it the default in the same write.
type AddRequest struct {
	Name         string            `json:"name"`
	Provider     string            `json:"provider"`
	BaseURL      string            `json:"base_url"`
	Auth         *AuthInput        `json:"auth"`
	Egress       EgressInput       `json:"egress"`
	ModelAliases map[string]string `json:"model_aliases,omitempty"`
	SetDefault   bool              `json:"set_default"`
}

type AuthInput struct {
	Mode   string `json:"mode"`
	Key    string `json:"key,omitempty"`
	KeyEnv string `json:"key_env,omitempty"`
}

type EgressInput struct {
	Mode string `json:"mode,omitempty"`
	URL  string `json:"url,omitempty"`
}

// Saved is an added or edited profile (Name set) and the file's default
// after the write.
type Saved struct {
	Profile profiles.Profile
	Default string
}

// Add creates a profile. Without a profiles.json it starts one holding the
// official profile and the new one.
func (s *Store) Add(req AddRequest) (Saved, *Error) {
	if e := checkName(req.Name); e != nil {
		return Saved{}, e
	}
	if strings.TrimSpace(req.BaseURL) == "" {
		return Saved{}, usage("base_url is required")
	}
	// Auth rules (mode, key) are Validate's, so every way of breaking them
	// reads the same; only the egress mode/url pair is checked here.
	egressMode := strings.ToLower(strings.TrimSpace(req.Egress.Mode))
	if egressMode != "" && egressMode != "http" && strings.TrimSpace(req.Egress.URL) != "" {
		return Saved{}, &Error{http.StatusUnprocessableEntity, "validation_error",
			fmt.Sprintf("egress.mode %s is incompatible with egress.url", egressMode), nil}
	}
	p := profiles.Profile{
		Name:     req.Name,
		Provider: req.Provider,
		BaseURL:  req.BaseURL,
		Egress:   profiles.EgressSpec{Mode: req.Egress.Mode, URL: req.Egress.URL},
	}
	if req.Auth != nil {
		p.Auth = &profiles.AuthSpec{Mode: req.Auth.Mode, Key: req.Auth.Key, KeyEnv: req.Auth.KeyEnv}
	}
	if len(req.ModelAliases) > 0 {
		p.ModelAliases = maps.Clone(req.ModelAliases)
	}
	var def string
	e := s.edit("profile add "+req.Name, func(f *profiles.File) (*profiles.File, *Error) {
		if f == nil {
			// The official profile is restored on every launch anyway; a
			// file created here starts with it.
			f = &profiles.File{
				Default:  profiles.OfficialProfileName,
				Profiles: map[string]profiles.Profile{profiles.OfficialProfileName: profiles.OfficialProfile()},
			}
		}
		if _, exists := f.Profiles[req.Name]; exists {
			return nil, &Error{http.StatusConflict, "conflict", fmt.Sprintf("profile %q already exists", req.Name), nil}
		}
		f.Profiles[req.Name] = p
		if req.SetDefault {
			f.Default = req.Name
		}
		def = f.Default
		return f, nil
	})
	if e != nil {
		return Saved{}, e
	}
	return Saved{Profile: p, Default: def}, nil
}

// EditRequest changes the fields it carries. Auth is three-state: absent
// keeps it, JSON null removes it, an object (AuthDelta) patches it.
// ModelAliases: nil keeps them, empty clears them, otherwise replaces them.
type EditRequest struct {
	Name         string             `json:"name"`
	Provider     *string            `json:"provider,omitempty"`
	BaseURL      *string            `json:"base_url,omitempty"`
	Auth         json.RawMessage    `json:"auth,omitempty"`
	ModelAliases *map[string]string `json:"model_aliases,omitempty"`
	Egress       *EgressDelta       `json:"egress,omitempty"`
	SetDefault   *bool              `json:"set_default,omitempty"`
}

// AuthDelta patches a profile's auth. KeySource picks where the key comes
// from: "inline" (Key), "env_var" (KeyEnv), or "" / "unchanged".
type AuthDelta struct {
	Mode      *string `json:"mode,omitempty"`
	KeySource string  `json:"key_source,omitempty"`
	Key       string  `json:"key,omitempty"`
	KeyEnv    string  `json:"key_env,omitempty"`
}

type EgressDelta struct {
	Mode *string `json:"mode,omitempty"`
	URL  *string `json:"url,omitempty"`
}

// ApplyEdit applies req to p in place. Mirrors cmd/ccwrap's applyEditOpts.
func ApplyEdit(p *profiles.Profile, req EditRequest) error {
	if req.Provider != nil {
		p.Provider = *req.Provider
	}
	if req.BaseURL != nil {
		p.BaseURL = *req.BaseURL
	}
	if len(req.Auth) > 0 {
		if bytes.Equal(bytes.TrimSpace(req.Auth), []byte("null")) {
			p.Auth = nil
		} else {
			var delta AuthDelta
			if err := json.Unmarshal(req.Auth, &delta); err != nil {
				return fmt.Errorf("auth: %v", err)
			}
			if p.Auth == nil {
				p.Auth = &profiles.AuthSpec{}
			}
			if delta.Mode != nil {
				p.Auth.Mode = *delta.Mode
			}
			switch delta.KeySource {
			case "", "unchanged":
			case "inline":
				p.Auth.Key = delta.Key
				p.Auth.KeyEnv = ""
			case "env_var":
				p.Auth.Key = ""
				p.Auth.KeyEnv = delta.KeyEnv
			default:
				return fmt.Errorf("auth.key_source: unknown value %q", delta.KeySource)
			}
		}
	}
	if req.ModelAliases != nil {
		if len(*req.ModelAliases) == 0 {
			p.ModelAliases = nil
		} else {
			p.ModelAliases = maps.Clone(*req.ModelAliases)
		}
	}
	if req.Egress != nil {
		if req.Egress.Mode != nil {
			p.Egress.Mode = *req.Egress.Mode
			newMode := strings.ToLower(strings.TrimSpace(*req.Egress.Mode))
			if newMode != "http" && newMode != "socks5" && newMode != "socks5h" {
				p.Egress.URL = ""
			}
		}
		if req.Egress.URL != nil {
			modeForCheck := p.Egress.Mode
			if req.Egress.Mode != nil {
				modeForCheck = *req.Egress.Mode
			}
			lowered := strings.ToLower(strings.TrimSpace(modeForCheck))
			urlBearing := lowered == "http" || lowered == "socks5" || lowered == "socks5h"
			if lowered != "" && !urlBearing && strings.TrimSpace(*req.Egress.URL) != "" {
				return fmt.Errorf("egress.mode %s is incompatible with egress.url", lowered)
			}
			p.Egress.URL = *req.Egress.URL
		}
	}
	return nil
}

// Edit changes an existing profile; it is never renamed.
func (s *Store) Edit(req EditRequest) (Saved, *Error) {
	if e := checkName(req.Name); e != nil {
		return Saved{}, e
	}
	var saved Saved
	e := s.edit("profile edit "+req.Name, func(f *profiles.File) (*profiles.File, *Error) {
		if f == nil {
			return nil, notFound("no profiles.json present")
		}
		p, ok := f.Profiles[req.Name]
		if !ok {
			return nil, notFound("no such profile")
		}
		p.Name = req.Name
		if err := ApplyEdit(&p, req); err != nil {
			return nil, &Error{http.StatusUnprocessableEntity, "validation_error", err.Error(), nil}
		}
		f.Profiles[req.Name] = p
		if req.SetDefault != nil && *req.SetDefault {
			f.Default = req.Name
		}
		saved = Saved{Profile: p, Default: f.Default}
		return f, nil
	})
	if e != nil {
		return Saved{}, e
	}
	return saved, nil
}

// Removed is what is left after a removal.
type Removed struct {
	Default   string
	Remaining int
}

// Remove deletes profile name; confirm must repeat it (surrounding
// whitespace aside). Removing the default picks another
// (ChooseFallbackDefault).
func (s *Store) Remove(name, confirm string) (Removed, *Error) {
	if e := checkName(name); e != nil {
		return Removed{}, e
	}
	if strings.TrimSpace(confirm) != name {
		return Removed{}, usage("confirm_name must match name")
	}
	var out Removed
	e := s.edit("profile rm "+name, func(f *profiles.File) (*profiles.File, *Error) {
		if f == nil {
			return nil, notFound("no profiles.json present")
		}
		if _, ok := f.Profiles[name]; !ok {
			return nil, notFound("no such profile")
		}
		delete(f.Profiles, name)
		if name == f.Default {
			f.Default = ChooseFallbackDefault(f, name)
		}
		out = Removed{Default: f.Default, Remaining: len(f.Profiles)}
		return f, nil
	})
	if e != nil {
		return Removed{}, e
	}
	return out, nil
}

// ChooseFallbackDefault picks the default after the default profile is
// removed: official when it is still there, else the alphabetically first
// remaining profile, else the inherit-env sentinel (Validate accepts it, and
// the next launch re-seeds official).
func ChooseFallbackDefault(f *profiles.File, removingName string) string {
	if _, ok := f.Profiles[profiles.OfficialProfileName]; ok && profiles.OfficialProfileName != removingName {
		return profiles.OfficialProfileName
	}
	names := make([]string, 0, len(f.Profiles))
	for n := range f.Profiles {
		if n != removingName {
			names = append(names, n)
		}
	}
	if len(names) == 0 {
		return profiles.InheritEnv
	}
	sort.Strings(names)
	return names[0]
}

// SetDefault makes name the default. inherit-env stands for official,
// restored if it was removed.
func (s *Store) SetDefault(name string) (string, *Error) {
	if e := checkName(name); e != nil {
		return "", e
	}
	var def string
	e := s.edit("profile set-default", func(f *profiles.File) (*profiles.File, *Error) {
		if f == nil {
			return nil, notFound("no profiles.json present")
		}
		switch _, ok := f.Profiles[name]; {
		case name == profiles.InheritEnv:
			f.Default = profiles.OfficialProfileName
			if _, ok := f.Profiles[profiles.OfficialProfileName]; !ok {
				f.Profiles[profiles.OfficialProfileName] = profiles.OfficialProfile()
			}
		case ok:
			f.Default = name
		default:
			return nil, notFound("no such profile")
		}
		def = f.Default
		return f, nil
	})
	if e != nil {
		return "", e
	}
	return def, nil
}

// SetModelDefault sets profile name's "*" alias to target (a model, or "*"
// to keep Claude names), leaving its other aliases as they are.
func (s *Store) SetModelDefault(name, target string) *Error {
	return s.edit("profile model default "+name, func(f *profiles.File) (*profiles.File, *Error) {
		if f == nil {
			return nil, notFound("no profiles.json present")
		}
		p, ok := f.Profiles[name]
		if !ok {
			return nil, notFound(fmt.Sprintf("profile %q no longer exists", name))
		}
		aliases := maps.Clone(p.ModelAliases)
		if aliases == nil {
			aliases = map[string]string{}
		}
		aliases[modelalias.DefaultKey] = target
		if _, err := modelalias.New(aliases, "profile "+name, true); err != nil {
			return nil, &Error{http.StatusUnprocessableEntity, "validation_error", err.Error(), nil}
		}
		p.ModelAliases = aliases
		f.Profiles[name] = p
		return f, nil
	})
}
