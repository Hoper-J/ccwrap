// Package profiles validation.
//
// validate.go owns:
//   - ValidationError + ParseErrors types (multi-error report)
//   - ErrValidationFailed sentinel
//   - exported Validate(*File, sourceLabel) error
//   - private validate(*File, source) *ParseErrors called by Parse
//   - private scrubURL helper (modeled on
//     internal/preflight/safeview.go::stripUserinfo — duplicated here
//     because supervisor->profiles cycle blocks direct reuse)
package profiles

import (
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ValidationError describes one semantic invariant violation. Path is
// a dotted field path rooted at the profiles.json file (e.g.,
// "profiles.glm.auth.mode" or "default"). Want and Got give the user
// enough context to fix without consulting docs. URL-typed Got values
// are scrubbed by validate.go's private scrubURL() helper.
type ValidationError struct {
	Path string
	Want string
	Got  string
}

func (v ValidationError) Error() string {
	if v.Got != "" {
		return fmt.Sprintf("%s: want %s, got %q", v.Path, v.Want, v.Got)
	}
	return fmt.Sprintf("%s: %s", v.Path, v.Want)
}

// ParseErrors wraps a multi-error report from Parse / Validate.
// Returned only when at least one ValidationError exists.
type ParseErrors struct {
	Source string
	Items  []ValidationError
}

func (e *ParseErrors) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s invalid: %d errors\n", e.Source, len(e.Items))
	for i, it := range e.Items {
		if i > 0 {
			b.WriteByte('\n')
		}
		fmt.Fprintf(&b, "  - %s", it.Error())
	}
	return b.String()
}

// Is matches the sentinel ErrValidationFailed. Requires len(Items) > 0
// — a zero-item ParseErrors (which internal code paths never produce
// but external constructions could) returns false to avoid misleading
// "validation failed but no items" matches.
func (e *ParseErrors) Is(target error) bool {
	return target == ErrValidationFailed && len(e.Items) > 0
}

// ErrValidationFailed is a sentinel for errors.Is matching against
// any *ParseErrors with at least one ValidationError.
var ErrValidationFailed = errors.New("profiles.json validation failed")

// Validate runs semantic invariant checks against an already-parsed
// *File (e.g., one built in memory by the add/edit flow before
// write). Returns nil on clean, *ParseErrors with all violations
// otherwise. sourceLabel is rendered into multi-line output as
// "<sourceLabel> invalid: N errors" — callers brand it for context
// (e.g. "edit profile glm" or "preview profiles.json").
func Validate(f *File, sourceLabel string) error {
	if perr := validate(f, sourceLabel); perr != nil {
		return perr
	}
	return nil
}

// ValidateEgressSpec runs the egress invariants from validateEgress
// against a single EgressSpec value (no surrounding Profile/File).
// Returns a *ParseErrors with one Item on failure, nil on success.
//
// Used by HTTP handlers (e.g. /profile/test-egress) that need to
// validate a draft EgressSpec from a request body without constructing
// a synthetic *File. Returned ValidationError.Path is rooted at
// "egress.mode" / "egress.url" — callers may rewrap with a richer
// path if they want to expose the surrounding profile name.
func ValidateEgressSpec(spec EgressSpec) error {
	fake := Profile{Egress: spec}
	perr := &ParseErrors{Source: "egress"}
	validateEgress("", fake, perr)
	if len(perr.Items) == 0 {
		return nil
	}
	// validateEgress builds "profiles.<name>.egress.<field>"; with empty
	// name that becomes "profiles..egress.<field>". Strip the dead prefix.
	for i := range perr.Items {
		perr.Items[i].Path = strings.TrimPrefix(perr.Items[i].Path, "profiles..")
	}
	return perr
}

// validate runs all rules against f. Returns nil when clean. Items
// are sorted by Path for deterministic output across map-iteration
// random ordering. Internal — Parse calls this directly to avoid the
// error-interface wrap that Validate adds.
func validate(f *File, source string) *ParseErrors {
	perr := &ParseErrors{Source: source}
	if f == nil {
		return nil
	}
	validateDefault(f, perr)
	for name, p := range f.Profiles {
		validateProfile(name, p, perr)
	}
	if len(perr.Items) == 0 {
		return nil
	}
	sort.Slice(perr.Items, func(i, j int) bool {
		return perr.Items[i].Path < perr.Items[j].Path
	})
	return perr
}

// validateDefault enforces that default (when non-empty) must be a key
// in profiles or the InheritEnv sentinel. EnsureOfficialProfile
// auto-migrates Default=inherit-env to "official" on every launch, so
// the sentinel is a transient state in practice — Validate still accepts
// it to keep migration paths working without a schema-level
// chicken-and-egg.
func validateDefault(f *File, perr *ParseErrors) {
	d := strings.TrimSpace(f.Default)
	if d == "" {
		return // Parse coerces empty to InheritEnv; not a validation failure
	}
	if d == InheritEnv {
		return
	}
	if _, ok := f.Profiles[d]; ok {
		return
	}
	names := make([]string, 0, len(f.Profiles))
	for n := range f.Profiles {
		names = append(names, n)
	}
	sort.Strings(names)
	want := fmt.Sprintf("one of: [%s] or %q", strings.Join(names, " "), InheritEnv)
	perr.Items = append(perr.Items, ValidationError{
		Path: "default",
		Want: want,
		Got:  d,
	})
}

// validateProfile runs per-profile rules: name must not equal a
// sentinel, plus base-URL, auth, and egress invariants.
func validateProfile(name string, p Profile, perr *ParseErrors) {
	validateProfileName(name, perr)
	validateBaseURL(name, p, perr)
	validateAuth(name, p, perr)
	validateEgress(name, p, perr)
}

// validateEgress checks egress.mode as a case-insensitive enum (empty
// treated as inherit) and egress.url: required + url.Parse OK when the
// mode requires it, with a scheme that must match the mode.
//
// Modes:
//   - inherit (or empty), direct: url MUST be empty. A non-empty URL
//     under these modes would be silently dropped at runtime by
//     probe.go's profileEgressToTransportFlag, so we reject at
//     validation time instead of letting orphaned URLs sit on disk.
//   - http:      url scheme must be http:// or https://
//   - socks5:    url scheme must be socks5:// (DNS resolved locally)
//   - socks5h:   url scheme must be socks5h:// (DNS resolved at proxy)
func validateEgress(name string, p Profile, perr *ParseErrors) {
	mode := strings.ToLower(strings.TrimSpace(p.Egress.Mode))
	rawURL := strings.TrimSpace(p.Egress.URL)
	if mode == "" {
		if rawURL != "" {
			perr.Items = append(perr.Items, ValidationError{
				Path: "profiles." + name + ".egress.url",
				Want: "egress.url must be empty when egress.mode is empty/inherit (URL would be silently dropped)",
				Got:  scrubURL(rawURL),
			})
		}
		return
	}
	switch mode {
	case "inherit", "direct":
		if rawURL != "" {
			perr.Items = append(perr.Items, ValidationError{
				Path: "profiles." + name + ".egress.url",
				Want: "egress.url must be empty when egress.mode=" + mode + " (URL would be silently dropped)",
				Got:  scrubURL(rawURL),
			})
		}
		return
	case "http", "socks5", "socks5h":
		if rawURL == "" {
			perr.Items = append(perr.Items, ValidationError{
				Path: "profiles." + name + ".egress.url",
				Want: "non-empty URL when egress.mode=" + mode,
			})
			return
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			perr.Items = append(perr.Items, ValidationError{
				Path: "profiles." + name + ".egress.url",
				Want: "parseable URL",
				Got:  scrubURL(rawURL),
			})
			return
		}
		scheme := strings.ToLower(u.Scheme)
		wantSchemes := schemesForMode(mode)
		ok := false
		for _, s := range wantSchemes {
			if scheme == s {
				ok = true
				break
			}
		}
		if !ok {
			perr.Items = append(perr.Items, ValidationError{
				Path: "profiles." + name + ".egress.url",
				Want: "URL scheme " + strings.Join(wantSchemes, " or ") + " when egress.mode=" + mode,
				Got:  scrubURL(rawURL),
			})
		}
	default:
		perr.Items = append(perr.Items, ValidationError{
			Path: "profiles." + name + ".egress.mode",
			Want: "one of: inherit, direct, http, socks5, socks5h (case-insensitive)",
			Got:  p.Egress.Mode,
		})
	}
}

func schemesForMode(mode string) []string {
	switch mode {
	case "http":
		return []string{"http", "https"}
	case "socks5":
		return []string{"socks5"}
	case "socks5h":
		return []string{"socks5h"}
	}
	return nil
}

// validateAuth enforces that auth.mode is strictly ∈ {ccwrap_bearer,
// ccwrap_x_api_key} (case-insensitive). Old data with mode=passthrough
// is rejected with a special-case message naming the fix ("remove the
// auth block instead"). The ccwrap_* modes require at least one of key
// or key_env, and key and key_env are mutually exclusive — together
// these enforce "exactly one key source" when the auth block exists.
//
// A nil p.Auth means the profile expresses "ccwrap does not own auth".
// No auth-shaped rules apply.
func validateAuth(name string, p Profile, perr *ParseErrors) {
	if p.Auth == nil {
		return
	}
	mode := strings.ToLower(strings.TrimSpace(p.Auth.Mode))
	hasKey := strings.TrimSpace(p.Auth.Key) != ""
	hasKeyEnv := strings.TrimSpace(p.Auth.KeyEnv) != ""
	switch mode {
	case "ccwrap_bearer", "ccwrap_x_api_key":
		if !hasKey && !hasKeyEnv {
			// At least one key source required.
			perr.Items = append(perr.Items, ValidationError{
				Path: "profiles." + name + ".auth",
				Want: fmt.Sprintf("key or key_env required when mode=%s", mode),
			})
		}
		if hasKey && hasKeyEnv {
			// Mutually exclusive. Previously "key beats key_env"
			// silently dropped key_env at runtime; now it's an explicit
			// error so the user knows their key_env is dead.
			perr.Items = append(perr.Items, ValidationError{
				Path: "profiles." + name + ".auth",
				Want: "auth.key and auth.key_env are mutually exclusive — set exactly one",
				Got:  "both set",
			})
		}
	case "passthrough":
		// Special case for the old data shape. Name the fix in the message
		// so users can self-correct without consulting docs.
		perr.Items = append(perr.Items, ValidationError{
			Path: "profiles." + name + ".auth.mode",
			Want: `remove the auth block instead — "passthrough" is no longer a mode value; absent auth block expresses "ccwrap does not own auth"`,
			Got:  p.Auth.Mode,
		})
	default:
		perr.Items = append(perr.Items, ValidationError{
			Path: "profiles." + name + ".auth.mode",
			Want: "one of: ccwrap_bearer, ccwrap_x_api_key (case-insensitive)",
			Got:  p.Auth.Mode,
		})
	}
}

// validateProfileName enforces that a profile name must not collide
// with any reserved sentinel. InheritEnv is the long-standing one
// (File.Select has a sentinel branch). The two angle-bracket synthetic names
// — "<active-session>" and "<draft>" — are reserved by handlers in
// the egress-probe surface (handle_egress_probe.go and the popover
// add-mode flow). Without this guard a user could save a profile
// literally named "<active-session>" that becomes unreachable through
// /profile/test-egress: the handler short-circuits on the name and
// returns the posture-derived synthetic profile before the disk
// lookup runs, silently shadowing the user's entry.
func validateProfileName(name string, perr *ParseErrors) {
	reserved := []string{InheritEnv, "<active-session>", "<draft>"}
	for _, sentinel := range reserved {
		if name == sentinel {
			perr.Items = append(perr.Items, ValidationError{
				Path: "profiles." + name,
				Want: fmt.Sprintf("name must not equal sentinel %q", sentinel),
				Got:  name,
			})
			return
		}
	}
}

// validateBaseURL enforces that, when present, url.Parse succeeds, the
// scheme ∈ {http, https}, and Host is non-empty. Empty is valid ("profile
// does not override base_url; preflight falls back to env or
// api.anthropic.com"). URL-typed Got passes through scrubURL.
func validateBaseURL(name string, p Profile, perr *ParseErrors) {
	raw := strings.TrimSpace(p.BaseURL)
	if raw == "" {
		// Empty is allowed — profile contributes no base_url override.
		// The official profile uses this shape.
		return
	}
	u, err := url.Parse(raw)
	if err != nil {
		perr.Items = append(perr.Items, ValidationError{
			Path: "profiles." + name + ".base_url",
			Want: "URL with http or https scheme and host",
			Got:  scrubURL(raw),
		})
		return
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		perr.Items = append(perr.Items, ValidationError{
			Path: "profiles." + name + ".base_url",
			Want: "URL with http or https scheme and host",
			Got:  scrubURL(raw),
		})
		return
	}
	if u.Host == "" {
		perr.Items = append(perr.Items, ValidationError{
			Path: "profiles." + name + ".base_url",
			Want: "URL with http or https scheme and host",
			Got:  scrubURL(raw),
		})
		return
	}
}

// scrubURL strips URL userinfo (user:pw@) from value for safe display
// in ValidationError.Got. Modeled after internal/preflight/safeview.go's
// stripUserinfo (we can't import preflight from profiles — preflight
// already imports profiles, would be a cycle).
//
// Returns "<malformed>" when url.Parse fails — guarantees no
// secret-bearing raw string leaks.
func scrubURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return "<malformed>"
	}
	if u.User == nil {
		return s
	}
	u.User = nil
	return u.String()
}
