package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/supervisor"
	"github.com/Hoper-J/ccwrap/internal/tlsfp"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

func main() {
	if err := realMain(); err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap: %v\n", err)
		os.Exit(1)
	}
}

func realMain() error {
	// version dispatches BEFORE DefaultPaths so it works in degraded
	// environments — bug reports about "ccwrap version exits with state-
	// dir error" are exactly the contexts (containers without HOME,
	// sandbox without CCWRAP_STATE_DIR, fresh systemd unit) where the
	// version string is most useful. Help is also degraded-safe for the
	// same reason: a user troubleshooting startup should be able to
	// discover the available subcommands without first fixing their env.
	if versionDispatchEarly(os.Args, os.Stdout) {
		return nil
	}
	if helpDispatchEarly(os.Args, os.Stdout) {
		return nil
	}

	paths, err := app.DefaultPaths()
	if err != nil {
		return err
	}
	if len(os.Args) == 1 {
		return runClaude(paths, nil)
	}
	switch os.Args[1] {
	case "status":
		return statusCommand(paths, os.Args[2:])
	case "dashboard":
		return dashboardCommand(paths, os.Args[2:])
	case "doctor":
		return doctorCommand(paths, os.Args[2:])
	case "stop":
		return stopCommand(paths, os.Args[2:])
	case "gc":
		return gcCommand(paths, os.Args[2:])
	case "capture":
		return captureCommand(paths, os.Args[2:])
	case "profile":
		os.Exit(runProfileSubcommand(paths, os.Args[2:]))
		return nil
	// "update" aliases "upgrade" (the rustup/uv convention). This
	// shadows the passthrough to `claude update` — deliberately: the
	// first instinct behind typing `ccwrap update` is to upgrade ccwrap
	// itself; to upgrade Claude proper, run `claude update` directly,
	// or `ccwrap -- update` to go through the session environment.
	case "upgrade", "update":
		return upgradeCommand(paths, os.Args[2:])
	case "version", "--version":
		return versionCommand(paths, os.Args[2:])
	case "run":
		return runClaude(paths, os.Args[2:])
	default:
		return runClaude(paths, os.Args[1:])
	}
}

// versionDispatchEarly handles `ccwrap version` and `ccwrap --version`
// before any state-dir or config resolution. Returns true when the
// version line was written, signaling the caller to exit 0.
func versionDispatchEarly(args []string, out io.Writer) bool {
	if len(args) < 2 {
		return false
	}
	switch args[1] {
	case "version", "--version":
		if len(args) != 2 {
			return false // argument forms like `version --check` go to the main switch's versionCommand
		}
		fmt.Fprintln(out, "ccwrap "+versionString())
		return true
	}
	return false
}

// helpDispatchEarly handles `ccwrap help`, `-h`, and `--help` before
// state-dir resolution so users can discover commands even in broken
// environments.
func helpDispatchEarly(args []string, out io.Writer) bool {
	if len(args) < 2 {
		return false
	}
	switch args[1] {
	case "help", "-h", "--help":
		fprintUsage(out)
		return true
	}
	return false
}

func printUsage() { fprintUsage(os.Stdout) }

func fprintUsage(out io.Writer) {
	fmt.Fprint(out, `ccwrap - Claude Code proxy launcher

Usage:
  ccwrap [CCWRAP_FLAGS...] [CLAUDE_ARGS...]
  ccwrap [CCWRAP_FLAGS...] -- [CLAUDE_ARGS...]
  ccwrap run [CCWRAP_FLAGS...] [--] [CLAUDE_ARGS...]

Launch flags:
  --upstream URL                            (or env CCWRAP_UPSTREAM / ANTHROPIC_BASE_URL)
  --egress-proxy auto|direct|URL
  --session-name NAME
  --claude-bin PATH
  --profile NAME                            (or env CCWRAP_PROFILE; from profiles.json)
  --model-alias LOGICAL=PROVIDER            (repeatable; '*=PROVIDER' maps every other Claude model, '*=*' keeps their names)
  --model-alias-file PATH                   (or env CCWRAP_MODEL_ALIASES_FILE / _JSON)
  --upstream-header NAME=VALUE              (repeatable; CCWRAP-owned, never Claude-visible)
  --upstream-headers-file PATH              (or env CCWRAP_UPSTREAM_HEADERS_FILE / _JSON)
  --capture-bodies                          (or env CCWRAP_CAPTURE_BODIES=1; default off) capture request + response bodies (alias: --capture-request-bodies)
  --capture-telemetry                       (or env CCWRAP_CAPTURE_TELEMETRY=1; default off) MITM+capture allowlisted telemetry bodies
  --quiet                                   (or env CCWRAP_QUIET=1; collapse the launch banner to one line)
  --timezone IANA                           (or env CCWRAP_TZ; inject TZ into the Claude Code child so its request "Today's date" matches the chosen zone. First run in a China timezone prompts to align to America/Los_Angeles unless opted out via CCWRAP_NO_TZ_PROMPT=1 or --no-init)
  --no-init                                 (or env CCWRAP_NO_INIT=1; skip the first-run env->profiles migration prompt and the timezone prompt)
  --no-update-check                         (or env CCWRAP_NO_UPDATE_CHECK=1; skip the daily update check and its notices)
  --allow-provider-model-passthrough        (compat; degrades hidden-mode guarantee)
  --allow-auth-passthrough-to-third-party   (debug; unsafe — Claude-side auth may leak)

Management commands:
  ccwrap status     [--json] [--session ID]
  ccwrap dashboard  [--session ID] [--view overview|requests|errors|diagnostics]
  ccwrap doctor     [--json] [--verbose] [--session ID] [--profile NAME]
  ccwrap stop       [--session ID | --all]
  ccwrap gc         [--json]
  ccwrap capture    [--with-tls|--tls-only] [--main-inference] [--no-response]
                    [--headers] [--full] [--unmask] [--host H] [--path P]
                    [--timeout DUR] [--print-diff-filter] [--claude-bin PATH]
                    [--timezone IANA] [-- CLAUDE_ARGS]
  ccwrap upgrade    [--egress-proxy auto|direct|URL]  (channel-aware self-upgrade; alias: update)
  ccwrap version    [--check] [--egress-proxy auto|direct|URL] (print version; --check queries the registry)
  ccwrap profile    {ls | status | switch <name> | test [name] | test-egress [name] |
                     add <name> | edit <name> | rm <name> | set-default <name>}
                    [--session ID]
`)
}

// resolveLaunchProfile loads profiles.json (path), selects the
// requested profile (--profile flag → CCWRAP_PROFILE env → provider-group
// default → persisted default → inherit-env) and returns the preflight overlay.
// nil overlay == inherit-env (zero-touch: byte-identical to today).
// An explicit --profile with no profiles.json is an error (an
// explicit selection must never silently degrade to inherit-env).
func resolveLaunchProfile(path, requested string) (*preflight.ProfileInput, error) {
	if requested == "" {
		// CCWRAP_PROFILE is the env-level default (precedence tier 2);
		// an explicit --profile (non-empty requested) overrides it.
		requested = strings.TrimSpace(os.Getenv("CCWRAP_PROFILE"))
	}
	file, err := profiles.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load profiles: %w", err)
	}
	selected, inheritEnv, err := file.Select(requested)
	if err != nil {
		return nil, err
	}
	if inheritEnv {
		return nil, nil
	}
	return preflight.FromProfile(selected), nil
}

// composeLaunch is the launch composition: runs settings.InspectLaunch, then
// snapshots all file-backed launch inputs into opts.*FileContent (mutating-by-value),
// then runs preflight.RunWithInspection. Returns (inspect, pre, opts, err) where opts
// is the input opts with the four content snapshots populated when their respective
// file paths were set.
//
// Caller MUST use the returned opts (or use it implicitly via the supervisor's
// LaunchContext built from these values). The doctor command does NOT use this
// helper — it stays on the disk-read path.
//
// Coverage (the resolver-options content fields):
//   - opts.ModelAliasFile         → opts.ModelAliasExplicitFileContent
//   - $CCWRAP_MODEL_ALIASES_FILE     → opts.ModelAliasEnvFileContent
//   - opts.UpstreamHeaderFile     → opts.UpstreamHeaderExplicitFileContent
//   - $CCWRAP_UPSTREAM_HEADERS_FILE  → opts.UpstreamHeaderEnvFileContent
//
// Inline env JSON inputs (CCWRAP_MODEL_ALIASES_JSON, CCWRAP_UPSTREAM_HEADERS,
// CCWRAP_UPSTREAM_HEADERS_JSON) are byte-faithful via the existing opts.ParentEnv
// snapshot — no separate snapshot needed (no disk involved).
//
// SECRETS: opts.UpstreamHeader*FileContent may carry secret-bearing values
// (auth headers like Authorization, X-API-Key). Treat all four content fields
// as secret-bearing: never log raw bytes; never serialize across the unix
// control socket; use preflight.SafeProfileView for any display surfacing of
// the resolved configuration. The bytes live only in the supervisor's
// in-process LaunchContext and are GC'd on supervisor exit.
func composeLaunch(opts preflight.Options) (*settings.InspectionResult, *preflight.Result, preflight.Options, error) {
	inspect, err := settings.InspectLaunch(opts.WorkingDirectory, opts.ChildArgs)
	if err != nil {
		return nil, nil, opts, err
	}
	if opts.ModelAliasFile != "" {
		data, err := os.ReadFile(opts.ModelAliasFile)
		if err != nil {
			return nil, nil, opts, err
		}
		opts.ModelAliasExplicitFileContent = data
	}
	if envPath := lookupEnv(opts.ParentEnv, "CCWRAP_MODEL_ALIASES_FILE"); envPath != "" {
		data, err := os.ReadFile(envPath)
		if err != nil {
			return nil, nil, opts, err
		}
		opts.ModelAliasEnvFileContent = data
	}
	if opts.UpstreamHeaderFile != "" {
		data, err := os.ReadFile(opts.UpstreamHeaderFile)
		if err != nil {
			return nil, nil, opts, err
		}
		opts.UpstreamHeaderExplicitFileContent = data
	}
	if envPath := lookupEnv(opts.ParentEnv, "CCWRAP_UPSTREAM_HEADERS_FILE"); envPath != "" {
		data, err := os.ReadFile(envPath)
		if err != nil {
			return nil, nil, opts, err
		}
		opts.UpstreamHeaderEnvFileContent = data
	}
	pre, err := preflight.RunWithInspection(opts, inspect)
	if err != nil {
		return inspect, pre, opts, err
	}
	return inspect, pre, opts, preflight.LaunchBypass(pre, opts, inspect)
}

// lookupEnv returns the value for key in a []string of "KEY=VALUE" entries
// (os.Environ() shape). Returns "" when key is absent. Mirrors
// internal/supervisor.lookupEnv (duplicate to avoid the cmd→internal/supervisor
// dependency).
func lookupEnv(env []string, key string) string {
	prefix := key + "="
	for _, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			return kv[len(prefix):]
		}
	}
	return ""
}

func runClaude(paths app.Paths, args []string) error {
	launch, err := parseLaunchArgs(args)
	if err != nil {
		return err
	}
	// Loudly flag the security downgrade if the operator turned off native-TLS
	// fingerprint mirroring — this session will be identifiable as non-native.
	if w := nativeTLSDisabledWarning(os.Getenv("CCWRAP_NATIVE_TLS")); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	// The riskiest auth opt-out deserves a danger line as loud as the native-TLS
	// kill switch: it can forward a first-party credential to a third party.
	if w := authPassthroughWarning(launch.AllowAuthPassthroughToThirdParty); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	// Fail-fast load + validate of an externally pinned ClientHello. When set,
	// it pre-seeds the mirror and disables live capture; a bad file aborts the
	// launch before any session work begins.
	hello, err := loadNativeTLSHello(os.Getenv("CCWRAP_NATIVE_TLS_HELLO"), launch.NativeTLS)
	if err != nil {
		return err
	}
	if hello != nil {
		launch.NativeTLSHello = hello
		if r, e := tlsfp.Compute(hello); e == nil {
			fmt.Fprintf(os.Stderr, "ccwrap: loaded native-TLS hello JA3=%s JA4=%s (%d bytes) — live capture disabled\n", r.JA3, r.JA4, len(hello))
		}
	}
	// Crash-safe startup sweep: reap orphaned runtime dirs,
	// including per-session bodies/, of no-longer-live sessions before
	// launching this one. Best-effort; never blocks a fresh launch.
	sweepOrphanSessions(paths)
	cwd, _ := os.Getwd()
	// Auto-restore the reserved "official" profile. Runs before the
	// first-run migration so the file exists when migration inspects it.
	// Idempotent; non-fatal on I/O failure (downstream consumers handle a
	// missing official gracefully — preflight returns nil overlay).
	if err := profiles.EnsureOfficialProfile(paths.StateDir); err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap: ensure official profile: %v\n", err)
	}
	// First-run env→profiles.json migration. Runs BEFORE
	// resolveLaunchProfile so the just-written profile is visible to
	// the same launch. Best-effort: failures fall back to today's
	// inherit-env behavior. SIGINT during the prompt terminates the
	// entire ccwrap process (default Go handler; signal forwarding is
	// installed later in l.Attach).
	maybeMigrateFromEnv(paths.StateDir, os.Environ(), cwd, launch.ClaudeArgs, launch.NoInit)
	// Timezone alignment. Resolve + validate the TZ to inject into the child
	// (--timezone/CCWRAP_TZ → persisted <stateDir>/timezone → first-run China-TZ
	// prompt) and overwrite launch.Timezone with the effective value; the
	// launcher threads it to preflight.BuildChildEnv. Runs after the profile
	// prompt so at most one first-run prompt fires per stdin turn. "" leaves the
	// child env byte-identical to today's.
	launch.Timezone = resolveEffectiveTimezone(paths.StateDir, launch.Timezone, os.Environ(), launch.NoInit)
	// Select the active profile (--profile name/group → persisted
	// default → inherit-env) and pass it as the preflight overlay.
	// nil overlay == inherit-env: byte-identical to the pre-feature
	// env-only path.
	profileOverlay, err := resolveLaunchProfile(profiles.DefaultPath(paths.StateDir), launch.Profile)
	if err != nil {
		return err
	}
	// composeLaunch is the single launch-composition entry. It runs
	// settings.InspectLaunch once, snapshots the 4 file-backed launch inputs into
	// opts.*FileContent, then runs preflight.RunWithInspection — so the resolver
	// sees the byte-faithful content at launch AND at every subsequent mid-session
	// switch (file content lives in retained Options for the session lifetime).
	// inspect + outOpts are plumbed into the launcher so StartSupervisor can build
	// the supervisor's secret-bearing LaunchContext — never serialized, never
	// crosses the control socket, GC'd on supervisor exit.
	inspect, pre, outOpts, err := composeLaunch(preflight.Options{
		Upstream:                         launch.Upstream,
		EgressProxy:                      launch.EgressProxy,
		ParentEnv:                        os.Environ(),
		WorkingDirectory:                 cwd,
		ChildArgs:                        launch.ClaudeArgs,
		ModelAliasFile:                   launch.ModelAliasFile,
		ModelAliasPairs:                  launch.ModelAliases,
		UpstreamHeaderFile:               launch.UpstreamHeadersFile,
		UpstreamHeaderPairs:              launch.UpstreamHeaders,
		AllowProviderModelPassthrough:    launch.AllowProviderModelPassthrough,
		AllowAuthPassthroughToThirdParty: launch.AllowAuthPassthroughToThirdParty,
		Profile:                          profileOverlay,
	})
	if err != nil {
		return err
	}

	l := newSessionLauncher(paths, launch, pre, inspect, outOpts, cwd, newID())
	defer l.rollback.run()
	defer l.cancelCtx()

	steps := []func() error{
		l.ResolveClaudeBin,
		l.PreparePaths,
		l.StartSupervisor,
		l.AwaitControl,
		l.CreateSession,
		l.WriteSessionSettings,
		l.SpawnChild,
		l.Attach,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	l.PrintSummary()
	updHandle := startBackgroundUpdateCheck(l.ctx, l.pre.Egress, paths.StateDir, versionString(), launch.NoUpdateCheck, os.Getenv, time.Now)
	code, err := l.Wait()
	l.PrintUnmappedNotice(os.Stderr)
	// The exit notice prints after Wait and before any return/os.Exit:
	// the banner covers "the next launch"; this line covers "this run,
	// whose check just finished". Non-blocking read — it never delays
	// exit.
	printUpdateExitNotice(os.Stderr, updHandle, versionString(), ui.IsTerminal(os.Stderr))
	if err != nil {
		return err
	}
	if code != 0 {
		os.Exit(code)
	}
	return nil
}

type launchArgs struct {
	Upstream                         string
	EgressProxy                      string
	SessionName                      string
	ClaudeBin                        string
	ModelAliasFile                   string
	ModelAliases                     []string
	UpstreamHeadersFile              string
	UpstreamHeaders                  []string
	AllowProviderModelPassthrough    bool
	AllowAuthPassthroughToThirdParty bool
	CaptureBodies                    bool
	CaptureTelemetry                 bool
	NativeTLS                        bool
	NativeTLSHello                   []byte
	Profile                          string
	ClaudeArgs                       []string
	NoInit                           bool   // skip first-run profile auto-migration
	Quiet                            bool   // collapse the launch banner to one line
	NoUpdateCheck                    bool   // --no-update-check / CCWRAP_NO_UPDATE_CHECK; passive update check + notices off
	Timezone                         string // --timezone / CCWRAP_TZ; runClaude overwrites with the resolved+validated effective TZ
}

func parseLaunchArgs(args []string) (launchArgs, error) {
	out := launchArgs{
		EgressProxy:      "auto",
		ClaudeBin:        envDefault("CLAUDE_BIN", "claude"),
		CaptureBodies:    truthyEnv(os.Getenv("CCWRAP_CAPTURE_BODIES")),
		CaptureTelemetry: truthyEnv(os.Getenv("CCWRAP_CAPTURE_TELEMETRY")),
		NativeTLS:        nativeTLSEnabled(os.Getenv("CCWRAP_NATIVE_TLS")),
		Quiet:            truthyEnv(os.Getenv("CCWRAP_QUIET")),
		NoUpdateCheck:    truthyEnv(os.Getenv("CCWRAP_NO_UPDATE_CHECK")),
		Timezone:         strings.TrimSpace(os.Getenv("CCWRAP_TZ")),
		ClaudeArgs:       nil,
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			out.ClaudeArgs = append(out.ClaudeArgs, args[i+1:]...)
			return out, nil
		}
		if arg == "--allow-provider-model-passthrough" {
			out.AllowProviderModelPassthrough = true
			continue
		}
		if strings.HasPrefix(arg, "--allow-provider-model-passthrough=") {
			value, err := parseBoolLaunchFlag(arg, "--allow-provider-model-passthrough")
			if err != nil {
				return out, err
			}
			out.AllowProviderModelPassthrough = value
			continue
		}
		if arg == "--no-init" {
			out.NoInit = true
			continue
		}
		if strings.HasPrefix(arg, "--no-init=") {
			value, err := parseBoolLaunchFlag(arg, "--no-init")
			if err != nil {
				return out, err
			}
			out.NoInit = value
			continue
		}
		if arg == "--quiet" {
			out.Quiet = true
			continue
		}
		if strings.HasPrefix(arg, "--quiet=") {
			value, err := parseBoolLaunchFlag(arg, "--quiet")
			if err != nil {
				return out, err
			}
			out.Quiet = value
			continue
		}
		if arg == "--no-update-check" {
			out.NoUpdateCheck = true
			continue
		}
		if strings.HasPrefix(arg, "--no-update-check=") {
			value, err := parseBoolLaunchFlag(arg, "--no-update-check")
			if err != nil {
				return out, err
			}
			out.NoUpdateCheck = value
			continue
		}
		if arg == "--allow-auth-passthrough-to-third-party" {
			out.AllowAuthPassthroughToThirdParty = true
			continue
		}
		if strings.HasPrefix(arg, "--allow-auth-passthrough-to-third-party=") {
			value, err := parseBoolLaunchFlag(arg, "--allow-auth-passthrough-to-third-party")
			if err != nil {
				return out, err
			}
			out.AllowAuthPassthroughToThirdParty = value
			continue
		}
		// --capture-bodies is canonical (one switch captures request +
		// response). --capture-request-bodies is kept as a back-compat alias;
		// both drive CaptureBodies.
		if arg == "--capture-bodies" {
			out.CaptureBodies = true
			continue
		}
		if strings.HasPrefix(arg, "--capture-bodies=") {
			value, err := parseBoolLaunchFlag(arg, "--capture-bodies")
			if err != nil {
				return out, err
			}
			out.CaptureBodies = value
			continue
		}
		if arg == "--capture-request-bodies" {
			out.CaptureBodies = true
			continue
		}
		if strings.HasPrefix(arg, "--capture-request-bodies=") {
			value, err := parseBoolLaunchFlag(arg, "--capture-request-bodies")
			if err != nil {
				return out, err
			}
			out.CaptureBodies = value
			continue
		}
		if arg == "--capture-telemetry" {
			out.CaptureTelemetry = true
			continue
		}
		if strings.HasPrefix(arg, "--capture-telemetry=") {
			value, err := parseBoolLaunchFlag(arg, "--capture-telemetry")
			if err != nil {
				return out, err
			}
			out.CaptureTelemetry = value
			continue
		}
		name, value, hasInlineValue, known := splitKnownLaunchFlag(arg)
		if !known {
			out.ClaudeArgs = append(out.ClaudeArgs, args[i:]...)
			return out, nil
		}
		if !hasInlineValue {
			if i+1 >= len(args) || args[i+1] == "--" || strings.HasPrefix(args[i+1], "-") {
				return out, fmt.Errorf("%s requires a value", arg)
			}
			i++
			value = args[i]
		}
		if strings.TrimSpace(value) == "" {
			return out, fmt.Errorf("%s requires a non-empty value", name)
		}
		switch name {
		case "--upstream":
			out.Upstream = value
		case "--egress-proxy":
			out.EgressProxy = value
		case "--session-name":
			out.SessionName = value
		case "--claude-bin":
			out.ClaudeBin = value
		case "--model-alias-file":
			out.ModelAliasFile = value
		case "--model-alias":
			out.ModelAliases = append(out.ModelAliases, value)
		case "--upstream-headers-file":
			out.UpstreamHeadersFile = value
		case "--upstream-header":
			out.UpstreamHeaders = append(out.UpstreamHeaders, value)
		case "--profile":
			out.Profile = value
		case "--timezone":
			out.Timezone = value
		default:
			return out, fmt.Errorf("internal error: unknown launch flag %s", name)
		}
	}
	return out, nil
}

func parseBoolLaunchFlag(arg, name string) (bool, error) {
	raw := strings.TrimSpace(strings.TrimPrefix(arg, name+"="))
	switch strings.ToLower(raw) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("%s expects true or false", name)
	}
}

func splitKnownLaunchFlag(arg string) (name, value string, hasInlineValue, known bool) {
	knownNames := []string{"--upstream", "--egress-proxy", "--session-name", "--claude-bin", "--model-alias-file", "--model-alias", "--upstream-headers-file", "--upstream-header", "--profile", "--timezone"}
	for _, candidate := range knownNames {
		if arg == candidate {
			return candidate, "", false, true
		}
		prefix := candidate + "="
		if strings.HasPrefix(arg, prefix) {
			return candidate, arg[len(prefix):], true, true
		}
	}
	return "", "", false, false
}

func splitDoubleDash(args []string) ([]string, []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func printJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func envDefault(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func truthyEnv(v string) bool {
	v = strings.TrimSpace(v)
	return v == "1" || strings.EqualFold(v, "true")
}

// nativeTLSEnabled defaults native-TLS fingerprint mirroring ON. It is disabled
// only by an explicit off value in CCWRAP_NATIVE_TLS (a hidden field kill-switch
// for the rare case utls cannot reproduce a Claude Code hello). There is no CLI
// flag and it is not a user-facing config knob -- looking native is the default.
//
// DANGER of disabling it: with mirroring off, ccwrap dials Anthropic with Go's
// stdlib TLS, so the upstream sees a Go fingerprint under Claude Code's HTTP
// headers -- the exact mismatch that marks the traffic as a non-native
// (rewritten) client. nativeTLSDisabledWarning surfaces this loudly at launch.
func nativeTLSEnabled(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// nativeTLSDisabledWarning returns a stderr warning when CCWRAP_NATIVE_TLS is set
// to an explicit off value, and "" otherwise. Disabling the mirror is a security
// downgrade: the upstream sees ccwrap's Go TLS fingerprint instead of Claude
// Code's, so the session becomes identifiable as a non-native client. The
// warning is printed once per launch (see runClaude) so the choice is never silent.
func nativeTLSDisabledWarning(v string) string {
	if nativeTLSEnabled(v) {
		return ""
	}
	return "ccwrap: WARNING native TLS fingerprint mirroring is DISABLED (CCWRAP_NATIVE_TLS) — " +
		"the upstream will see ccwrap's Go TLS fingerprint, not Claude Code's, so this session is " +
		"identifiable as a non-native client. Unset CCWRAP_NATIVE_TLS to restore fingerprint parity."
}

// authPassthroughWarning returns a stderr danger line when Claude-side auth
// passthrough to a third-party upstream is enabled
// (--allow-auth-passthrough-to-third-party). It is the riskiest opt-out — it
// can forward a first-party credential to a third-party gateway — so it is
// surfaced as loudly as the native-TLS kill switch, never silently.
func authPassthroughWarning(enabled bool) string {
	if !enabled {
		return ""
	}
	return "ccwrap: WARNING --allow-auth-passthrough-to-third-party is ON — " +
		"Claude-side auth may be forwarded to a third-party upstream, which can leak a first-party " +
		"credential. Use it only for debugging against a trusted upstream; drop the flag to fail closed."
}

// loadNativeTLSHello reads + validates the CCWRAP_NATIVE_TLS_HELLO file (fail-fast
// at launch). Returns (nil,nil) when unset. Errors when: native-TLS is disabled
// (mutually exclusive), the file is unreadable, or ValidateLoadedHello rejects it.
func loadNativeTLSHello(path string, nativeTLSEnabled bool) ([]byte, error) {
	if path == "" {
		return nil, nil
	}
	if !nativeTLSEnabled {
		return nil, errors.New("CCWRAP_NATIVE_TLS_HELLO is set but native-TLS is off (CCWRAP_NATIVE_TLS=0) — mutually exclusive")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("CCWRAP_NATIVE_TLS_HELLO: %w", err)
	}
	if err := supervisor.ValidateLoadedHello(raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// isTerminalFn is the unit-testable seam for ui.IsTerminal. Production
// code keeps the default; tests in cmd/ccwrap/main_test.go override it via
// stubIsTerminal. Defined as a var (not const) so it can be swapped.
var isTerminalFn = ui.IsTerminal

// newAuthPlaceholder returns the non-secret bootstrap credential injected
// into the child env (hidden-auth contract: the session proxy replaces
// upstream auth fail-closed, so this value carries no authority and gains
// nothing from being unpredictable). The placeholder is always injected as
// ANTHROPIC_AUTH_TOKEN (see placeholderKindForAuthMode), which interactive
// Claude Code accepts without the customApiKeyResponses approval dialog it
// applies to env ANTHROPIC_API_KEY. It stays stable per profile rather than
// per session: deterministic child envs, and a downgrade to a ccwrap that
// still injects ANTHROPIC_API_KEY keeps any previously approved fingerprint
// valid instead of re-prompting every launch.
//
// The child echoes the value back in the Authorization header to the
// loopback proxy, so it must stay within header-safe token characters no
// matter what the profile is named; the short digest keeps sanitized names
// ("a b" vs "ab") and the no-profile sentinel from colliding.
func newAuthPlaceholder(profileName string) string {
	name := strings.TrimSpace(profileName)
	if name == "" {
		name = "<inherit-env>"
	}
	var safe strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			safe.WriteRune(r)
		}
	}
	sum := sha256.Sum256([]byte(name))
	out := "ccwrap-placeholder-"
	if safe.Len() > 0 {
		out += safe.String() + "-"
	}
	return out + hex.EncodeToString(sum[:4])
}

func newID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}
