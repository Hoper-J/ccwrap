package profiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/modelalias"
)

// InheritEnv is the sentinel "default" value meaning "no profile —
// resolve from the ambient environment exactly as ccwrap does today".
const InheritEnv = "inherit-env"

// AuthSpec is a profile's upstream-auth posture. Mode is "ccwrap_bearer"
// or "ccwrap_x_api_key". Key is an inline secret (permitted under local
// trust); KeyEnv is an env reference (recommended, not mandated). ccwrap
// never rewrites either back into profiles.json.
//
// A profile that does not want ccwrap to inject auth omits the auth block
// entirely (Profile.Auth == nil). The pointer makes "present vs absent" a
// compile-time-checkable distinction.
type AuthSpec struct {
	Mode   string `json:"mode"`
	Key    string `json:"key,omitempty"`
	KeyEnv string `json:"key_env,omitempty"`
}

// EgressSpec mirrors the inputs ResolveEgressFromInspection accepts.
// Mode is "inherit" | "direct" | "http"; URL is required for "http".
type EgressSpec struct {
	Mode string `json:"mode"`
	URL  string `json:"url,omitempty"`
}

// Profile is one named provider/model preset. Name is the map key,
// injected by Parse (it has no JSON tag of its own). Provider is the
// group label used purely for UI organization.
//
// Auth is nullable: a nil pointer means "ccwrap does not own auth for
// this profile" (the upstream call goes out without an injected auth
// header). A non-nil pointer means ccwrap injects an auth header per its
// Mode/Key/KeyEnv.
type Profile struct {
	Name            string            `json:"-"`
	Provider        string            `json:"provider"`
	BaseURL         string            `json:"base_url"`
	Auth            *AuthSpec         `json:"auth,omitempty"`
	ModelAliases    map[string]string `json:"model_aliases,omitempty"`
	UpstreamHeaders map[string]string `json:"upstream_headers,omitempty"`
	Egress          EgressSpec        `json:"egress"`
}

// File is the parsed profiles.json. Default is a profile name or the
// InheritEnv sentinel.
type File struct {
	Default  string             `json:"default"`
	Profiles map[string]Profile `json:"profiles"`
}

// DefaultPath returns the profiles.json location. ccwrap has no general
// user-config dir; the persistent convention ccwrap already uses is
// app.Paths.StateDir (see internal/app/paths.go: the CA lives at
// filepath.Join(stateDir, "certs")). profiles.json sits beside it at
// filepath.Join(stateDir, "profiles.json"). Honors CCWRAP_STATE_DIR via
// the caller passing the resolved app.Paths.StateDir.
func DefaultPath(stateDir string) string {
	return filepath.Join(stateDir, "profiles.json")
}

// Load reads profiles.json. A missing file returns (nil, nil) — that
// is the zero-touch path: behavior is exactly today's inherit-env.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("read profiles file %s: %w", path, err)
	}
	return Parse(data, path)
}

// Parse decodes profiles.json. The model_aliases submap of each
// profile is coerced through modelalias.ParseJSON so it shares the
// exact alias-coercion rules (and error messages) used everywhere
// else in ccwrap. The Profile.Name is injected from its map key.
func Parse(data []byte, source string) (*File, error) {
	var rawFile struct {
		Default  string                     `json:"default"`
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(data, &rawFile); err != nil {
		return nil, fmt.Errorf("parse profiles %s: %w", source, err)
	}
	out := &File{Default: rawFile.Default, Profiles: map[string]Profile{}}
	if out.Default == "" {
		out.Default = InheritEnv
	}
	for name, rawProfile := range rawFile.Profiles {
		var p Profile
		if err := json.Unmarshal(rawProfile, &p); err != nil {
			return nil, fmt.Errorf("parse profiles %s[%s]: %w", source, name, err)
		}
		// Re-coerce ONLY the model_aliases submap through
		// modelalias.ParseJSON so the alias rules and error register
		// match the rest of ccwrap. Passing the whole profile object would
		// (when model_aliases is omitted — a valid minimal profile)
		// make ParseJSON's bare-map fallback mis-coerce the profile's
		// own fields. Absent or null submap => no aliases.
		var rawFields struct {
			ModelAliases json.RawMessage `json:"model_aliases"`
		}
		if err := json.Unmarshal(rawProfile, &rawFields); err != nil {
			return nil, fmt.Errorf("parse profiles %s[%s]: %w", source, name, err)
		}
		p.ModelAliases = nil
		if len(rawFields.ModelAliases) > 0 && string(rawFields.ModelAliases) != "null" {
			aliases, err := modelalias.ParseJSON(rawFields.ModelAliases, fmt.Sprintf("%s[%s].model_aliases", source, name))
			if err != nil {
				return nil, err
			}
			if len(aliases) > 0 {
				p.ModelAliases = aliases
			}
		}
		p.Name = name
		out.Profiles[name] = p
	}
	if perr := validate(out, source); perr != nil {
		return nil, perr
	}
	return out, nil
}

// Label is the switcher group/row label "name (Provider)". When
// Provider is empty it is just the name. Never includes credentials.
func (p *Profile) Label() string {
	name := strings.TrimSpace(p.Name)
	provider := strings.TrimSpace(p.Provider)
	if provider == "" {
		return name
	}
	return name + " (" + provider + ")"
}

// BaseURLHost is the host[:port] of BaseURL, for the switcher row.
// Returns the raw trimmed BaseURL if it does not parse as a URL.
func (p *Profile) BaseURLHost() string {
	raw := strings.TrimSpace(p.BaseURL)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// ModelAliasCount is the number of configured alias rules.
func (p *Profile) ModelAliasCount() int { return len(p.ModelAliases) }

// EgressSummary is the switcher's one-token egress description:
// "inherit" | "direct" | "http <host:port>". Never a credential.
func (p *Profile) EgressSummary() string {
	mode := strings.TrimSpace(strings.ToLower(p.Egress.Mode))
	switch mode {
	case "", "inherit":
		return "inherit"
	case "direct":
		return "direct"
	case "http":
		raw := strings.TrimSpace(p.Egress.URL)
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			return "http " + u.Host
		}
		return "http"
	default:
		return mode
	}
}

// Select resolves a request to a concrete profile, implementing the
// selection rule. requested is:
//   - ""              → the persisted Default ("" Default treated as
//     InheritEnv); InheritEnv → (nil, true, nil)
//   - "inherit-env"   → (nil, true, nil) explicitly
//   - an exact name   → that profile
//   - a provider/group→ that group's default: the persisted Default if
//     it is in the group, else the sole profile if
//     the group has exactly one, else an ambiguous
//     error listing the group's profiles
//   - unknown         → error naming the request
//
// A nil *File (no profiles.json) with requested=="" or InheritEnv is
// inherit-env; with an explicit name it is an error (an explicit
// --profile must never silently degrade to inherit-env).
func (f *File) Select(requested string) (*Profile, bool, error) {
	req := strings.TrimSpace(requested)
	if f == nil {
		if req == "" || req == InheritEnv {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("profile %q requested but no profiles.json is configured", req)
	}
	if req == "" {
		req = strings.TrimSpace(f.Default)
		if req == "" || req == InheritEnv {
			return nil, true, nil
		}
	}
	if req == InheritEnv {
		return nil, true, nil
	}
	if p, ok := f.Profiles[req]; ok {
		cp := p
		cp.Name = req
		return &cp, false, nil
	}
	var inGroup []string
	for name, p := range f.Profiles {
		if strings.EqualFold(strings.TrimSpace(p.Provider), req) {
			inGroup = append(inGroup, name)
		}
	}
	sort.Strings(inGroup)
	switch len(inGroup) {
	case 0:
		avail := make([]string, 0, len(f.Profiles))
		for name := range f.Profiles {
			avail = append(avail, name)
		}
		sort.Strings(avail)
		if len(avail) == 0 {
			return nil, false, fmt.Errorf("unknown profile or provider group %q (no profiles defined yet — add one with `ccwrap profile add`)", req)
		}
		return nil, false, fmt.Errorf("unknown profile or provider group %q; available: %s (see `ccwrap profile ls`)", req, strings.Join(avail, ", "))
	case 1:
		p := f.Profiles[inGroup[0]]
		p.Name = inGroup[0]
		return &p, false, nil
	default:
		def := strings.TrimSpace(f.Default)
		for _, name := range inGroup {
			if name == def {
				p := f.Profiles[name]
				p.Name = name
				return &p, false, nil
			}
		}
		return nil, false, fmt.Errorf("provider group %q has multiple profiles (%s) and no group default; pass --profile <name>", req, strings.Join(inGroup, ", "))
	}
}
