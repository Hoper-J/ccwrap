package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/settings"
)

// migrationDisabled reads from parentEnv (snapshot) — consistent with
// every other env read in maybeMigrateFromEnv. truthyEnv accepts only
// "1" or case-insensitive "true".
func migrationDisabled(parentEnv []string, noInit bool) bool {
	if noInit {
		return true
	}
	return truthyEnv(lookupEnv(parentEnv, "CCWRAP_NO_INIT"))
}

// resolveSeedName reads the profile name from stdin if it's a TTY,
// returning the user's input (with empty → "local"). Non-TTY returns
// "local" directly. Returns empty string on EOF (Ctrl-D), which the
// caller interprets as "skip migration". SIGINT (Ctrl-C) does NOT reach
// this function — Go's default handler exits the process before Scan
// returns (accepted limitation).
func resolveSeedName(stdin *os.File) string {
	if !isTerminalFn(stdin) {
		return "local"
	}
	fmt.Fprintln(os.Stderr, "ccwrap: detected non-default ANTHROPIC_BASE_URL — seeding initial profile")
	fmt.Fprintln(os.Stderr, "ccwrap:   (run with --no-init or set CCWRAP_NO_INIT=1 to skip; Ctrl-D to abort)")
	fmt.Fprint(os.Stderr, "ccwrap: profile name [local]: ")
	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		return "" // EOF/Ctrl-D → caller aborts
	}
	name := strings.TrimSpace(scanner.Text())
	if name == "" {
		return "local"
	}
	return name
}

// maybeMigrateFromEnv is the first-run env-→-profiles.json migration
// entry. Called from runClaude between parseLaunchArgs and
// resolveLaunchProfile so the just-written profile is visible to the
// SAME launch.
//
// All non-prompt failure modes fall back to today's inherit-env
// behavior; SIGINT during the prompt terminates the entire ccwrap process
// via Go's default signal handler (forwardSignalsToChild is not yet
// installed at this point in the launch flow).
//
// stateDir is paths.StateDir (the only field needed; no preflight.Options).
// parentEnv is os.Environ() snapshot.
// cwd is the launch working directory.
// childArgs are launch.ClaudeArgs (passed through to settings.InspectLaunch).
// noInit is launch.NoInit (parsed by parseLaunchArgs).
func maybeMigrateFromEnv(stateDir string, parentEnv []string, cwd string, childArgs []string, noInit bool) {
	// Trigger condition 4 — opt-out gates
	if migrationDisabled(parentEnv, noInit) {
		return
	}
	// Load existing file (may be non-nil after EnsureOfficialProfile
	// ran). Trigger 1 now means "no env-migrated profile yet" — file
	// existence alone no longer aborts.
	path := profiles.DefaultPath(stateDir)
	existing, err := profiles.Load(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "ccwrap: skip init: load %s: %v\n", path, err)
		return
	}
	inspect, err := settings.InspectLaunch(cwd, childArgs)
	if err != nil {
		return
	}
	// Trigger condition 2 — env must carry ANTHROPIC_BASE_URL
	provider, err := settings.EffectiveProviderEnvFromInspection(parentEnv, inspect)
	if err != nil || provider == nil {
		return
	}
	baseURL := strings.TrimSpace(provider.Env["ANTHROPIC_BASE_URL"])
	if baseURL == "" {
		return
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Hostname() == "" {
		fmt.Fprintf(os.Stderr, "ccwrap: skip init: malformed ANTHROPIC_BASE_URL\n")
		return
	}
	// Trigger condition 3 — must be third-party-hidden (not canonical Anthropic)
	if preflight.ClassifyRoute(parsed) != model.RouteClassThirdPartyHidden {
		return
	}
	// Trigger condition 5 (poison-pill guard)
	apiKey := provider.Env["ANTHROPIC_API_KEY"]
	authToken := provider.Env["ANTHROPIC_AUTH_TOKEN"]
	if apiKey == "" && authToken == "" {
		return
	}
	// Skip if an env-migrated profile already exists (same BaseURL).
	// Avoids double-prompting on subsequent launches.
	if existing != nil {
		for _, p := range existing.Profiles {
			if strings.TrimSpace(p.BaseURL) == baseURL {
				return
			}
		}
	}
	// Resolve name via TTY prompt or non-TTY fallback.
	name := resolveSeedName(os.Stdin)
	if name == "" {
		fmt.Fprintln(os.Stderr, "ccwrap: init aborted (EOF on prompt)")
		return
	}
	spec := profiles.SeedSpec{
		Name:      name,
		BaseURL:   baseURL,
		APIKey:    apiKey,
		AuthToken: authToken,
	}
	seedFile, err := profiles.SeedFromEnv(spec)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap: init failed: %v\n", err)
		return
	}
	// Take the cross-process lock ONLY now — after the interactive name
	// prompt — so it is never held across user input. Then RE-LOAD under the
	// lock: another ccwrap may have written profiles.json while we were
	// prompting, so the pre-prompt `existing` snapshot is stale and writing it
	// back would clobber the peer's add. See profiles.Lock.
	unlock, err := profiles.Lock(stateDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap: init failed: lock: %v\n", err)
		return
	}
	defer unlock()
	fresh, err := profiles.Load(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "ccwrap: init failed: reload %s: %v\n", path, err)
		return
	}
	if fresh != nil {
		// A peer may have seeded the same BaseURL during the prompt — skip.
		for _, p := range fresh.Profiles {
			if strings.TrimSpace(p.BaseURL) == baseURL {
				return
			}
		}
		// A peer may have added a DIFFERENT profile under this same name during
		// the prompt — do NOT clobber it. `add` 409s on a name collision; match
		// that here rather than blind-overwrite (the lost update the lock exists
		// to prevent).
		if _, taken := fresh.Profiles[spec.Name]; taken {
			fmt.Fprintf(os.Stderr, "ccwrap: init skipped: profile %q already exists (added concurrently)\n", spec.Name)
			return
		}
		// Append: add the new profile alongside existing ones; do NOT
		// steal file.Default (user/ensureOfficial already set it).
		if fresh.Profiles == nil {
			fresh.Profiles = map[string]profiles.Profile{}
		}
		fresh.Profiles[spec.Name] = seedFile.Profiles[spec.Name]
		if err := profiles.OverwriteFile(path, fresh, "sp4a-add-"+spec.Name); err != nil {
			fmt.Fprintf(os.Stderr, "ccwrap: init failed: %v\n", err)
			return
		}
	} else {
		// Fresh file — write the seed file as-is. Rare path now since
		// EnsureOfficialProfile runs first; kept for safety.
		if err := profiles.WriteFile(path, seedFile); err != nil {
			fmt.Fprintf(os.Stderr, "ccwrap: init failed: %v\n", err)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "ccwrap: seeded initial profile %q from environment\n", name)
}
