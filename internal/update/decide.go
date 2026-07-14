// Package update implements ccwrap's update awareness: a rate-limited
// background version check, a fact-only state cache, install-channel
// detection, and the binary self-replace used by `ccwrap upgrade`.
//
// The package is deliberately UI-free — it returns data, never renders
// copy. All user-facing wording lives in cmd/ccwrap.
package update

import (
	"strings"

	"golang.org/x/mod/semver"
)

// Eligible reports whether the running build participates in update
// notifications. Only clean release versions qualify: a developer's
// in-tree build (pseudo-version, dirty tree, or the 0.0.0 sentinel from
// cmd/ccwrap/version.go) must never be nagged about its own older tag.
// Build metadata (+sha) is allowed — release binaries carry it — but a
// ".dirty" marker inside it disqualifies the build.
func Eligible(current string) bool {
	v := "v" + strings.TrimSpace(current)
	if !semver.IsValid(v) {
		return false
	}
	if semver.Prerelease(v) != "" {
		return false
	}
	if strings.Contains(semver.Build(v), "dirty") {
		return false
	}
	return semver.Canonical(v) != "v0.0.0"
}

// Newer reports whether latest is strictly newer than current under
// semver precedence (build metadata ignored, per semver 2.0 §10 — the
// same reading versionString()'s doc comment relies on). Unparseable
// input on either side means "no": a notification must never fire on
// garbage data from the network or an exotic local build.
func Newer(current, latest string) bool {
	cv := "v" + strings.TrimSpace(current)
	lv := "v" + strings.TrimSpace(latest)
	if !semver.IsValid(cv) || !semver.IsValid(lv) {
		return false
	}
	return semver.Compare(lv, cv) > 0
}

// Disabled reports whether the PASSIVE side (background check + all
// notices) is off. Explicit actions (`ccwrap upgrade`,
// `ccwrap version --check`) intentionally ignore this switch.
func Disabled(getenv func(string) string, flagDisabled bool) bool {
	return flagDisabled || truthy(getenv("CCWRAP_NO_UPDATE_CHECK"))
}

// InCI reports whether we are running under CI. Notifications (and the
// background check itself) are suppressed there — nobody reads them and
// CI egress is often restricted.
func InCI(getenv func(string) string) bool {
	return strings.TrimSpace(getenv("CI")) != ""
}

// truthy mirrors cmd/ccwrap's env-flag semantics (parseBoolLaunchFlag):
// 1/true/yes/on enable, anything else does not.
func truthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
