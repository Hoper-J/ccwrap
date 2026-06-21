package profiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// OfficialProfileName is the reserved name for the auto-created
// "official Anthropic" profile. EnsureOfficialProfile creates an
// entry with this name on first launch; users may customize or
// delete it, but restart restores presence (not content).
const OfficialProfileName = "official"

// OfficialProfile returns the canonical shape ccwrap writes when
// EnsureOfficialProfile creates the entry on first launch. Auth is
// nil — claude-code's OAuth flow owns request-time auth. BaseURL
// is empty so preflight.resolveAPIBase falls back to
// https://api.anthropic.com.
func OfficialProfile() Profile {
	return Profile{
		Name:     OfficialProfileName,
		Provider: "anthropic",
		BaseURL:  "",
		Auth:     nil,
		Egress:   EgressSpec{Mode: "inherit"},
	}
}

// EnsureOfficialProfile guarantees that the profiles file at the
// canonical path contains an "official" entry:
//   - File doesn't exist → create {default: official, profiles: {official}}
//   - File exists, no official entry → add official; do NOT touch Default
//   - File exists, official entry present → no-op (preserve customizations)
//   - File has Default == InheritEnv (legacy) → migrate Default to "official"
//
// Uses a raw JSON parse path (NOT Load) for the existing-file case so
// legacy files with Default=inherit-env (which Validate rejects) can
// still be migrated. Atomic write via OverwriteFile (or WriteFile for
// fresh install) ensures the post-state passes Validate.
//
// Returns error only on I/O failures. Idempotent: safe to call on
// every launch.
func EnsureOfficialProfile(stateDir string) error {
	// Cross-process lock across this read→mutate→write so concurrent ccwrap
	// launches (each runs EnsureOfficialProfile) cannot clobber a profile a
	// peer added between our read and write. No interactive work happens here,
	// so holding it for the whole function is safe. See Lock.
	unlock, err := Lock(stateDir)
	if err != nil {
		return fmt.Errorf("ensure official: lock: %w", err)
	}
	defer unlock()
	path := DefaultPath(stateDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// Fresh install — create with official as default.
			f := &File{
				Default:  OfficialProfileName,
				Profiles: map[string]Profile{OfficialProfileName: OfficialProfile()},
			}
			if err := WriteFile(path, f); err != nil {
				return fmt.Errorf("ensure official: write fresh: %w", err)
			}
			return nil
		}
		return fmt.Errorf("ensure official: read: %w", err)
	}
	// Raw parse: tolerates legacy Default=inherit-env (which Parse's
	// Validate would reject). Mirrors Parse's structure minus the
	// validation step.
	var raw struct {
		Default  string                     `json:"default"`
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("ensure official: parse: %w", err)
	}
	f := &File{Default: raw.Default, Profiles: map[string]Profile{}}
	if f.Default == "" {
		f.Default = InheritEnv
	}
	for name, rawProfile := range raw.Profiles {
		var p Profile
		if err := json.Unmarshal(rawProfile, &p); err != nil {
			return fmt.Errorf("ensure official: parse %s[%s]: %w", path, name, err)
		}
		p.Name = name
		f.Profiles[name] = p
	}
	changed := false
	// Migration: legacy default=inherit-env → default=official.
	if f.Default == InheritEnv {
		f.Default = OfficialProfileName
		changed = true
	}
	// Add official if missing — preserve existing entries.
	if _, exists := f.Profiles[OfficialProfileName]; !exists {
		f.Profiles[OfficialProfileName] = OfficialProfile()
		changed = true
	}
	if !changed {
		return nil
	}
	if err := OverwriteFile(path, f, "ensure-official"); err != nil {
		return fmt.Errorf("ensure official: overwrite: %w", err)
	}
	return nil
}

// SeedSpec describes the inputs used to build a seed Profile from launch
// env. It is populated from settings.EffectiveProviderEnvFromInspection;
// future `ccwrap profile init` subcommand and similar tooling populate it
// from their own sources.
//
// APIKey / AuthToken carry the actual env VALUE (not just presence). The
// seeded Profile records the VALUE inline at Auth.Key — the profile is
// self-contained and survives env rotation/unset on subsequent launches.
// This is a deliberate doctrine evolution: the earlier design wrote only
// env-NAME references (Auth.KeyEnv) to avoid landing credentials on disk,
// but that coupling made the profile fragile (rotating the env would
// silently break the profile until the user noticed). The trade-off:
// profiles.json now contains the secret VALUE in a 0600-mode file —
// equivalent to the trust posture of any other 0600 credential file.
type SeedSpec struct {
	Name      string
	BaseURL   string
	APIKey    string // ANTHROPIC_API_KEY value at migration time; empty = not set
	AuthToken string // ANTHROPIC_AUTH_TOKEN value at migration time; empty = not set
}

// SeedFromEnv builds a single-profile *File from a SeedSpec, suitable to
// pass to WriteFile. The resulting File has:
//   - Default: spec.Name (so profile selection picks it up next launch)
//   - Profiles: { spec.Name: <derived Profile> }
//
// Auth precedence: API_KEY beats AUTH_TOKEN beats neither. Both flags
// false → Profile.Auth nil (ccwrap does not own auth; the upstream client's
// own auth flow takes over at request time).
//
// Returns error on a malformed BaseURL (url.Parse fail), empty Name, or
// the reserved sentinel name "inherit-env".
func SeedFromEnv(spec SeedSpec) (*File, error) {
	name := strings.TrimSpace(spec.Name)
	if name == "" {
		return nil, fmt.Errorf("seed profile: empty name")
	}
	// Reserved-name guard. InheritEnv is the sentinel meaning "no
	// profile". A profile literally named "inherit-env" would shadow
	// the sentinel: File.Select("") with Default=="inherit-env" returns
	// the sentinel (profiles.go), making the seeded profile unreachable.
	// Reject the name to keep the profile usable.
	if name == InheritEnv {
		return nil, fmt.Errorf("seed profile: %q is reserved (sentinel for 'no profile'); pick another name", InheritEnv)
	}
	parsed, err := url.Parse(strings.TrimSpace(spec.BaseURL))
	if err != nil || parsed.Hostname() == "" {
		return nil, fmt.Errorf("seed profile: malformed base_url %q: %v", spec.BaseURL, err)
	}
	p := Profile{
		Name:            name,
		Provider:        providerFromHost(parsed.Hostname()),
		BaseURL:         spec.BaseURL,
		Auth:            seedAuth(spec),
		Egress:          EgressSpec{Mode: "inherit"},
		ModelAliases:    map[string]string{},
		UpstreamHeaders: map[string]string{},
	}
	return &File{
		Default:  name,
		Profiles: map[string]Profile{name: p},
	}, nil
}

// seedAuth pairs Mode with the inline Key (the actual credential VALUE
// at migration time), so profileAuthEnvKey re-injects it through the
// same hot-swap + upstream auth-header path as any user-edited inline
// profile. Mode↔credential pairing is load-bearing — applyProfileOverlay
// reads the inline value first, so it is the authoritative source.
// KeyEnv is intentionally left empty: a profile that carries both Key
// and KeyEnv has a dead key_env field (Key always wins) and the
// redundancy is noise.
//
//	APIKey    set → &AuthSpec{Mode: ccwrap_x_api_key, Key: <value>}
//	AuthToken set → &AuthSpec{Mode: ccwrap_bearer,    Key: <value>}
//	neither       → nil (ccwrap does not own auth for this profile)
//
// APIKey beats AuthToken when both are set, matching applyProfileOverlay's
// explicitAuth precedence. Returning nil for the no-credential case
// matches the invariant that the auth block is whole-or-nothing.
func seedAuth(spec SeedSpec) *AuthSpec {
	if spec.APIKey != "" {
		return &AuthSpec{Mode: "ccwrap_x_api_key", Key: spec.APIKey}
	}
	if spec.AuthToken != "" {
		return &AuthSpec{Mode: "ccwrap_bearer", Key: spec.AuthToken}
	}
	return nil
}

// providerFromHost extracts a short Provider label from a hostname using
// a best-effort heuristic that skips common prefixes and numeric labels.
func providerFromHost(host string) string {
	parts := strings.Split(host, ".")
	if len(parts) < 2 {
		return host
	}
	skip := map[string]bool{"api": true, "www": true, "v1": true, "v2": true}
	for _, p := range parts {
		if !skip[strings.ToLower(p)] && !isAllDigits(p) {
			return p
		}
	}
	return host
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// WriteFile atomically persists a *File to path via O_CREATE|O_EXCL —
// the EXCL guards against a race where profiles.json appears between
// the seed trigger detection (os.Stat returned NotExist) and this
// write. Perms are 0o600 (owner-only): profiles.json now carries the
// credential VALUE inline (see SeedSpec docstring), so the file is a
// secret-bearing surface and must match the trust posture of any other
// credential file on the user's machine.
//
// On EEXIST (race lost), returns a wrapped error that satisfies
// errors.Is(err, fs.ErrExist) == true. WriteFile uses fmt.Errorf %w to
// preserve the chain so the caller can branch on the race condition.
func WriteFile(path string, f *File) error {
	if f == nil {
		return fmt.Errorf("write profiles: nil file")
	}
	blob, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal profiles: %w", err)
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir profiles dir: %w", err)
	}
	// Atomic + exclusive create. Write to a temp file in the same dir, fsync,
	// then hard-link it into place: os.Link is atomic and fails with EEXIST when
	// path already exists, preserving the original O_CREATE|O_EXCL "don't clobber
	// an existing profiles.json" contract (callers branch on
	// errors.Is(err, fs.ErrExist)) — while a crash mid-write can no longer leave
	// a truncated profiles.json at path, only an orphaned temp the defer removes.
	fd, err := os.CreateTemp(dir, ".profiles.json.*.tmp")
	if err != nil {
		return fmt.Errorf("create tmp: %w", err)
	}
	tmpPath := fd.Name()
	defer os.Remove(tmpPath)
	if err := fd.Chmod(0o600); err != nil {
		_ = fd.Close()
		return fmt.Errorf("chmod tmp: %w", err)
	}
	if _, err := fd.Write(blob); err != nil {
		_ = fd.Close()
		return fmt.Errorf("write tmp: %w", err)
	}
	if err := fd.Sync(); err != nil {
		_ = fd.Close()
		return fmt.Errorf("sync tmp: %w", err)
	}
	if err := fd.Close(); err != nil {
		return fmt.Errorf("close tmp: %w", err)
	}
	if err := os.Link(tmpPath, path); err != nil {
		// EEXIST preserves the fresh-install contract; %w keeps the chain so
		// callers can still errors.Is(err, fs.ErrExist).
		return fmt.Errorf("create profiles.json: %w", err)
	}
	return nil
}
