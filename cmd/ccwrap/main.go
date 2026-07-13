package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/certs"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/dashboard"
	"github.com/Hoper-J/ccwrap/internal/discovery"
	"github.com/Hoper-J/ccwrap/internal/egress"
	"github.com/Hoper-J/ccwrap/internal/manifest"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/procmeta"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/supervisor"
	"github.com/Hoper-J/ccwrap/internal/tlsfp"
	"github.com/Hoper-J/ccwrap/internal/ui"
	"github.com/Hoper-J/ccwrap/internal/upstreamheaders"
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
  --model-alias LOGICAL=PROVIDER            (repeatable)
  --model-alias-file PATH                   (or env CCWRAP_MODEL_ALIASES_FILE / _JSON)
  --upstream-header NAME=VALUE              (repeatable; CCWRAP-owned, never Claude-visible)
  --upstream-headers-file PATH              (or env CCWRAP_UPSTREAM_HEADERS_FILE / _JSON)
  --capture-bodies                          (or env CCWRAP_CAPTURE_BODIES=1; default off) capture request + response bodies (alias: --capture-request-bodies)
  --capture-telemetry                       (or env CCWRAP_CAPTURE_TELEMETRY=1; default off) MITM+capture allowlisted telemetry bodies
  --quiet                                   (or env CCWRAP_QUIET=1; collapse the launch banner to one line)
  --timezone IANA                           (or env CCWRAP_TZ; inject TZ into the Claude Code child so its request "Today's date" matches the chosen zone. First run in a China timezone prompts to align to America/Los_Angeles unless opted out via CCWRAP_NO_TZ_PROMPT=1 or --no-init)
  --no-init                                 (or env CCWRAP_NO_INIT=1; skip the first-run env->profiles migration prompt and the timezone prompt)
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
  ccwrap version    (print version + git short-sha)
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
	return inspect, pre, opts, err
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
	code, err := l.Wait()
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

func statusCommand(paths app.Paths, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output JSON")
	sessionID := fs.String("session", "", "specific session id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	discovered, err := discovery.Scan(paths)
	if err != nil {
		return err
	}
	if *sessionID != "" {
		for _, ds := range discovered {
			if ds.Manifest.SessionID != *sessionID {
				continue
			}
			snap, ferr := fetchSnapshot(ds)
			if ferr != nil {
				fmt.Fprintf(os.Stderr, "ccwrap: fetch snapshot for %s: %v\n", ds.Manifest.SessionID, ferr)
			}
			if *jsonOut {
				return printJSON(struct {
					Discovery model.DiscoveredSession `json:"discovery"`
					Session   *model.Session          `json:"session,omitempty"`
				}{ds, snap})
			}
			printStatus(discovered, []*model.Session{snap})
			return nil
		}
		return fmt.Errorf("session %s not found", *sessionID)
	}
	var sessions []*model.Session
	for _, ds := range discovered {
		snap, ferr := fetchSnapshot(ds)
		if ferr != nil {
			fmt.Fprintf(os.Stderr, "ccwrap: fetch snapshot for %s: %v\n", ds.Manifest.SessionID, ferr)
		}
		if snap != nil {
			sessions = append(sessions, snap)
		}
	}
	if *jsonOut {
		return printJSON(struct {
			Sessions  []*model.Session          `json:"sessions"`
			Discovery []model.DiscoveredSession `json:"discovery"`
		}{sessions, discovered})
	}
	printStatus(discovered, sessions)
	return nil
}

func dashboardCommand(paths app.Paths, args []string) error {
	fs := flag.NewFlagSet("dashboard", flag.ContinueOnError)
	sessionID := fs.String("session", "", "filter by session")
	view := fs.String("view", "overview", "view name (overview|requests|errors|diagnostics)")
	interval := fs.Duration("interval", 700*time.Millisecond, "data refresh interval")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return dashboard.Run(ctx, paths, dashboard.Options{SessionID: *sessionID, View: *view, Interval: *interval})
}

func doctorCommand(paths app.Paths, args []string) error {
	flagArgs, childArgs := splitDoubleDash(args)
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output JSON")
	verbose := fs.Bool("verbose", false, "verbose details")
	sessionID := fs.String("session", "", "inspect a specific session")
	egressProxy := fs.String("egress-proxy", "auto", "effective supervisor egress proxy mode/url for launch-contract checks (auto, direct, or absolute proxy URL)")
	profileFlag := fs.String("profile", "", "resolve as this profile would launch (default: profiles.json default; `inherit-env` forces the env-only path)")
	if err := fs.Parse(flagArgs); err != nil {
		return err
	}
	cwd, _ := os.Getwd()
	report := model.DoctorReport{Overall: "ok"}
	addWithFields := func(name, status, summary, detail string, fields map[string]any) {
		report.Checks = append(report.Checks, model.DoctorCheck{Name: name, Status: status, Summary: summary, Detail: detail, Fields: fields})
		if status == "fail" {
			report.Overall = "fail"
		} else if status == "warn" && report.Overall == "ok" {
			report.Overall = "warn"
		}
	}
	add := func(name, status, summary, detail string) {
		addWithFields(name, status, summary, detail, nil)
	}
	if err := app.EnsurePaths(paths); err != nil {
		add("paths", "fail", "runtime/state directories unavailable", err.Error())
	} else {
		add("paths", "pass", "runtime and state directories ready", maybeDetail(*verbose, fmt.Sprintf("runtime=%s state=%s", paths.RuntimeDir, paths.StateDir)))
	}
	ca := certs.NewManager(paths)
	if err := ca.EnsureCA(); err != nil {
		add("ca", "fail", "local CA unavailable", err.Error())
	} else {
		status := ca.BundleStatus()
		detail := fmt.Sprintf("mode=%s bundle=%s system_roots=%t ccwrap_root=%t", status.Mode, status.BundlePath, status.HasSystemCA, status.HasCCWRAPRoot)
		fields := map[string]any{
			"mode":         status.Mode,
			"bundle_path":  status.BundlePath,
			"system_roots": status.HasSystemCA,
			"ccwrap_root":  status.HasCCWRAPRoot,
		}
		if status.Mode == "ccwrap_only" || !status.HasSystemCA {
			reason := status.SystemCAWarning
			if reason == "" {
				reason = "system CA bundle not discoverable on this host"
			}
			fields["system_ca_warning"] = reason
			warnDetail := reason + "; child tools (curl, git, python) will trust only the CCWRAP root. Install OS trust-store roots or set CCWRAP_SYSTEM_CA_BUNDLE to restore public-host TLS."
			if *verbose {
				warnDetail = detail + " ; " + warnDetail
			}
			addWithFields("ca", "warn", "local CA bundle ready but falling back to CCWRAP-only (no system roots)", warnDetail, fields)
		} else {
			addWithFields("ca", "pass", "local composite CA bundle is ready", maybeDetail(*verbose, detail), fields)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		add("session_listener", "fail", "session proxy port cannot bind", err.Error())
	} else {
		_ = ln.Close()
		add("session_listener", "pass", "session proxy port can bind", "")
	}
	parentEnv := os.Environ()
	envMap := preflight.ParentEnvMap(parentEnv)
	doctorRouteClass := model.RouteClassFirstParty
	doctorAuthMode := model.AuthModePassthrough
	if upstream := strings.TrimSpace(envMap["ANTHROPIC_BASE_URL"]); upstream != "" {
		if u, _, err := preflight.ResolveAPIBase("", upstream); err != nil {
			add("inherited_upstream", "fail", "ANTHROPIC_BASE_URL is invalid", err.Error())
		} else {
			add("inherited_upstream", "pass", "ANTHROPIC_BASE_URL is valid and will be captured", maybeDetail(*verbose, u.String()))
		}
	} else {
		add("inherited_upstream", "pass", "no inherited ANTHROPIC_BASE_URL; fallback default will be used", "")
	}
	// Provider-selection transparency: ccwrap scrubs CLAUDE_CODE_USE_BEDROCK/
	// VERTEX/FOUNDRY from the child env to keep getAPIProvider()==='firstParty'
	// (the first-party posture the gateway path depends on). When a user has set
	// one truthy intending Bedrock/Vertex/Foundry, surface that the selection is
	// being overridden — ccwrap does not route to those providers.
	var providerSelect []string
	for _, k := range []string{"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		if v := strings.ToLower(strings.TrimSpace(envMap[k])); v == "1" || v == "true" || v == "yes" || v == "on" {
			providerSelect = append(providerSelect, k)
		}
	}
	if len(providerSelect) > 0 {
		sort.Strings(providerSelect)
		add("provider_selection", "pass", "Bedrock/Vertex/Foundry selection env is set but will be overridden to keep the first-party posture; ccwrap does not route to those providers", strings.Join(providerSelect, ", "))
	} else {
		add("provider_selection", "pass", "no Bedrock/Vertex/Foundry provider-selection env to override", "")
	}
	unsupported := preflight.DetectUnsupportedEnv(envMap)
	if len(unsupported) > 0 {
		sort.Strings(unsupported)
		add("unsupported_env", "fail", "unsupported auth/tunnel env detected", strings.Join(unsupported, ", "))
	} else {
		add("unsupported_env", "pass", "no unsupported file-descriptor, unix-socket, or custom oauth env detected", "")
	}
	if mode, source, _, err := preflight.ResolveAuth(envMap); err != nil {
		add("parent_env_auth_sources", "fail", "conflicting parent-env upstream auth sources detected", err.Error())
	} else {
		add("parent_env_auth_sources", "pass", "parent-env upstream auth sources resolved", maybeDetail(*verbose, fmt.Sprintf("mode=%s source=%s", mode, source)))
	}
	// Resolve the SAME profile overlay an actual launch would apply (--profile,
	// else the profiles.json default, else inherit-env) so the effective-upstream
	// / auth / hidden-contract / launch-contract checks reflect what `ccwrap`
	// would really do — not the env-only path.
	profileInput, profileErr := resolveLaunchProfile(profiles.DefaultPath(paths.StateDir), *profileFlag)
	profileUpstream := ""
	if profileErr != nil {
		add("profile", "fail", "could not resolve the requested launch profile", profileErr.Error())
	} else if profileInput == nil {
		add("profile", "pass", "inherit-env (no profile overlay; env + Claude settings only)", maybeDetail(*verbose, fallbackText(strings.TrimSpace(*profileFlag), "no --profile, no profiles.json default")))
	} else {
		profileUpstream = strings.TrimSpace(profileInput.BaseURL)
		add("profile", "pass", fmt.Sprintf("launch profile overlay: %s", profileInput.Name), maybeDetail(*verbose, fmt.Sprintf("base_url=%s owns_auth=%t", fallbackText(profileUpstream, "(inherit)"), profileInput.Auth != nil)))
	}
	inspect, inspectErr := settings.InspectLaunch(cwd, childArgs)
	if inspectErr != nil {
		add("settings_inspection", "fail", "unable to inspect Claude settings", inspectErr.Error())
		add("egress_proxy", "fail", "failed to resolve effective egress proxy", inspectErr.Error())
	} else {
		egressCfg, notes, err := preflight.ResolveEgressFromInspection(*egressProxy, parentEnv, inspect)
		if err != nil {
			add("egress_proxy", "fail", "failed to resolve effective egress proxy", err.Error())
		} else {
			detail := egress.Summary(egressCfg)
			if len(notes) > 0 {
				detail += " ; " + strings.Join(notes, "; ")
			}
			add("egress_proxy", "pass", "effective supervisor egress proxy configuration resolved", maybeDetail(*verbose, detail))
		}
		active := strings.Join(inspect.ActiveSources, ", ")
		if active == "" {
			active = "none"
		}
		add("active_setting_sources", "pass", "active Claude setting sources resolved", maybeDetail(*verbose, active))
		if u, routeSource, configSource, err := preflight.ResolveAPIBaseFromInspection(profileUpstream, parentEnv, inspect); err != nil {
			add("effective_upstream", "fail", "effective provider upstream is invalid", err.Error())
		} else {
			doctorRouteClass = preflight.ClassifyRoute(u)
			add("effective_upstream", "pass", "effective provider upstream resolved", maybeDetail(*verbose, fmt.Sprintf("url=%s route=%s source=%s class=%s", u.String(), routeSource, configSource, doctorRouteClass)))
		}
		if profileInput != nil && profileInput.Auth != nil {
			// The active profile OWNS upstream auth — that beats any ambient/settings
			// source at launch, so the effective auth is the profile's, not passthrough.
			if strings.EqualFold(strings.TrimSpace(profileInput.Auth.Mode), "ccwrap_x_api_key") {
				doctorAuthMode = model.AuthModeOverrideXAPIKey
			} else {
				doctorAuthMode = model.AuthModeOverrideBearer
			}
			add("auth_sources", "pass", "upstream auth is owned by the active profile", maybeDetail(*verbose, fmt.Sprintf("mode=%s source=profile:%s", doctorAuthMode, profileInput.Name)))
		} else if mode, source, configSource, _, err := preflight.ResolveAuthFromInspection(parentEnv, inspect); err != nil {
			add("auth_sources", "fail", "conflicting effective upstream auth sources detected", err.Error())
		} else {
			doctorAuthMode = mode
			add("auth_sources", "pass", "effective upstream auth source resolved", maybeDetail(*verbose, fmt.Sprintf("mode=%s source=%s config_source=%s", mode, source, configSource)))
		}
		ccwrapUpstream := strings.TrimRight(strings.TrimSpace(envMap["CCWRAP_UPSTREAM"]), "/")
		var anthBase, anthSource string
		if provider, _ := settings.EffectiveProviderEnvFromInspection(parentEnv, inspect); provider != nil {
			anthBase = strings.TrimRight(strings.TrimSpace(provider.Env["ANTHROPIC_BASE_URL"]), "/")
			if provider.KeySources != nil {
				anthSource = provider.KeySources["ANTHROPIC_BASE_URL"]
			}
		}
		if ccwrapUpstream != "" && anthBase != "" && !strings.EqualFold(ccwrapUpstream, anthBase) {
			detail := fmt.Sprintf("CCWRAP_UPSTREAM wins; ANTHROPIC_BASE_URL (source=%s) is silently ignored", fallbackText(anthSource, "unknown"))
			add("upstream_inputs", "warn", "CCWRAP_UPSTREAM and ANTHROPIC_BASE_URL set with different values", detail)
		} else {
			add("upstream_inputs", "pass", "no CCWRAP_UPSTREAM / ANTHROPIC_BASE_URL conflict", "")
		}
		if doctorRouteClass == model.RouteClassThirdPartyHidden && doctorAuthMode == model.AuthModePassthrough {
			add("hidden_auth_contract", "fail", "third-party hidden mode requires CCWRAP-owned upstream auth", "Refusing to forward Claude-side authentication to third-party upstream. Provide ANTHROPIC_API_KEY/ANTHROPIC_AUTH_TOKEN as compatibility input or CCWRAP_UPSTREAM_* when available.")
		} else if doctorRouteClass == model.RouteClassThirdPartyHidden {
			add("hidden_auth_contract", "pass", "third-party hidden auth will be CCWRAP-owned and fail-closed", maybeDetail(*verbose, fmt.Sprintf("auth_mode=%s bootstrap=placeholder", doctorAuthMode)))
		} else {
			add("hidden_auth_contract", "pass", "first-party route does not require hidden auth fail-closed", "")
		}
		if len(inspect.UnsupportedEnv) > 0 {
			add("settings_unsupported_env", "fail", "unsupported auth/tunnel env detected in Claude settings", formatEnvConflicts(inspect.UnsupportedEnv))
		} else {
			add("settings_unsupported_env", "pass", "no unsupported auth/tunnel env in active Claude settings", "")
		}
		if len(inspect.MalformedEnv) > 0 {
			add("settings_malformed_env", "fail", "malformed env object detected in active Claude settings", formatMalformedEnvIssues(inspect.MalformedEnv))
		} else {
			add("settings_malformed_env", "pass", "no malformed env objects in active Claude settings", "")
		}
		if len(inspect.OverriddenNetworkEnv) > 0 {
			add("settings_overridden_network_env", "pass", "CCWRAP will override network/trust env from non-policy Claude settings", formatEnvConflicts(inspect.OverriddenNetworkEnv))
		} else {
			add("settings_overridden_network_env", "pass", "no overridden network/trust env in non-policy Claude settings", "")
		}
		if len(inspect.PolicyNetworkEnv) > 0 {
			detail := formatEnvConflicts(inspect.PolicyNetworkEnv) + "; detectable local/cache policy-managed network/trust env are unsupported with stock Claude Code under CCWRAP; CCWRAP only inspects local/cache policy sources pre-launch, and remote managed settings, MDM, and HKCU may still exist and remain unsupported; move proxy settings to --egress-proxy, launcher shell env, or non-policy user/global Claude settings, install enterprise CA trust in the host OS trust store, and remove these keys from managed policy settings"
			add("settings_policy_network_env", "fail", "detectable local/cache policy-managed network/trust env are unsupported with stock Claude Code under CCWRAP", detail)
		} else {
			add("settings_policy_network_env", "pass", "no detectable local/cache policy-managed network/trust env in active Claude settings", maybeDetail(*verbose, "Best-effort local/cache inspection only; remote managed settings, MDM, and HKCU may still exist and remain unsupported."))
		}
		if len(inspect.APIKeyHelperHits) > 0 {
			detail := "apiKeyHelper is a Claude-side auth path. It is blocked for third-party hidden mode; future CCWRAP-side upstreamAuthHelper must be owned and executed by CCWRAP only."
			if *verbose {
				detail += " sources=" + strings.Join(inspect.APIKeyHelperHits, ", ")
			}
			if doctorRouteClass == model.RouteClassThirdPartyHidden {
				add("api_key_helper", "fail", "apiKeyHelper is blocked in third-party hidden mode", detail)
			} else {
				add("api_key_helper", "warn", "Claude-side dynamic auth helper detected", detail)
			}
		} else {
			add("api_key_helper", "pass", "no apiKeyHelper detected in active Claude settings", "")
		}
		if len(inspect.DangerousShellSettings) > 0 {
			detail := "Claude shell-exec settings run commands at request time and can leak credentials or inject auth-like headers. They are blocked for third-party hidden mode."
			if *verbose {
				detail += " sources=" + formatEnvConflicts(inspect.DangerousShellSettings)
			}
			if doctorRouteClass == model.RouteClassThirdPartyHidden {
				add("dangerous_shell_settings", "fail", "shell-exec settings are blocked in third-party hidden mode", detail)
			} else {
				add("dangerous_shell_settings", "warn", "Claude shell-exec settings detected", detail)
			}
		} else {
			add("dangerous_shell_settings", "pass", "no non-apiKeyHelper shell-exec settings detected", "")
		}
		if len(inspect.CCWRAPInternalEnvHits) > 0 {
			detail := "CCWRAP_* env keys in Claude settings are launcher-process inputs only; they are silently dropped from the generated session settings file. Set them in the shell or systemd unit instead."
			if *verbose {
				detail += " sources=" + formatEnvConflicts(inspect.CCWRAPInternalEnvHits)
			}
			add("ccwrap_internal_keys_in_settings", "warn", "CCWRAP_* env keys in Claude settings are silently dropped", detail)
		} else {
			add("ccwrap_internal_keys_in_settings", "pass", "no CCWRAP_* env keys in Claude settings", "")
		}
		customHeaderHits := inspect.CustomAuthHeaderEnv
		if inherited := inheritedCustomHeadersDoctorConflict(envMap); len(inherited) > 0 {
			customHeaderHits = append(customHeaderHits, inherited...)
		}
		if len(customHeaderHits) > 0 {
			detail := "ANTHROPIC_CUSTOM_HEADERS contains auth-like header names; values are not shown. These headers are blocked for third-party hidden mode; use CCWRAP-owned upstream headers when available."
			if *verbose {
				detail += " sources=" + formatEnvConflicts(customHeaderHits)
			}
			if doctorRouteClass == model.RouteClassThirdPartyHidden {
				add("custom_headers_auth", "fail", "auth-like ANTHROPIC_CUSTOM_HEADERS are blocked in third-party hidden mode", detail)
			} else {
				add("custom_headers_auth", "warn", "auth-like headers detected in ANTHROPIC_CUSTOM_HEADERS", detail)
			}
		} else {
			add("custom_headers_auth", "pass", "no auth-like ANTHROPIC_CUSTOM_HEADERS detected", "")
		}
		if inspect.ParsedFlagSettings != nil && inspect.ParsedFlagSettings.OriginalArgValue != "" {
			summary := inspect.ParsedFlagSettings.Path
			if inspect.ParsedFlagSettings.Inline {
				summary = "inline JSON"
			}
			add("flag_settings", "pass", "Claude --settings flag will be merged into a CCWRAP session settings file", maybeDetail(*verbose, summary))
		} else {
			add("flag_settings", "pass", "no user-provided Claude --settings flag detected", "")
		}
	}
	if profileErr != nil {
		add("launch_contract", "fail", "current launch would be rejected: could not resolve the launch profile", profileErr.Error())
	} else if _, err := preflight.RunWithInspection(preflight.Options{
		Profile:          profileInput,
		EgressProxy:      *egressProxy,
		ParentEnv:        parentEnv,
		WorkingDirectory: cwd,
		ChildArgs:        childArgs,
	}, inspect); err != nil {
		add("launch_contract", "fail", "current launch would be rejected by CCWRAP preflight", err.Error())
	} else {
		add("launch_contract", "pass", "current launch satisfies CCWRAP MITM preflight checks", "")
	}
	discovered, err := discovery.Scan(paths)
	if err != nil {
		add("discovery", "fail", "unable to scan session manifests", err.Error())
	} else {
		stale := 0
		for _, ds := range discovered {
			if ds.Stale || !ds.Reachable {
				stale++
			}
		}
		add("discovery", "pass", fmt.Sprintf("found %d session manifests", len(discovered)), maybeDetail(*verbose, fmt.Sprintf("reachable=%d stale=%d", len(discovered)-stale, stale)))
	}
	if *sessionID != "" {
		ds, err := discovery.Find(paths, *sessionID)
		if err != nil {
			add("session", "fail", "session manifest not found", err.Error())
		} else if ds.Stale {
			add("session", "warn", "session manifest is stale", ds.Error)
		} else if ds.Reachable {
			add("session", "pass", "session control socket is reachable", maybeDetail(*verbose, ds.Manifest.ControlSocket))
		} else {
			add("session", "warn", "session manifest exists but control socket is not reachable", ds.Error)
		}
	}
	if *jsonOut {
		return printJSON(report)
	}
	pal := ui.New(ui.IsTerminal(os.Stdout))

	passed, warned, failed := 0, 0, 0
	for _, c := range report.Checks {
		switch c.Status {
		case "pass":
			passed++
		case "warn":
			warned++
		case "fail":
			failed++
		}
	}
	fmt.Printf("%s — %s · %s · %s\n",
		pal.Bold("ccwrap doctor"),
		pal.Green(fmt.Sprintf("%d passed", passed)),
		pal.Yellow(fmt.Sprintf("%d warn", warned)),
		pal.Red(fmt.Sprintf("%d fail", failed)),
	)

	byGroup := map[string][]model.DoctorCheck{}
	for _, c := range report.Checks {
		g := doctorGroupForCheck(c.Name)
		byGroup[g] = append(byGroup[g], c)
	}
	nameW := 0
	for _, c := range report.Checks {
		if len(c.Name) > nameW {
			nameW = len(c.Name)
		}
	}
	for _, g := range doctorGroupOrder {
		checks := byGroup[g]
		if len(checks) == 0 {
			continue
		}
		fmt.Printf("\n  %s\n", pal.Bold(g))
		for _, c := range checks {
			fmt.Printf("    %s %-*s  %s\n", ui.StatusGlyph(pal, c.Status), nameW, c.Name, c.Summary)
			if *verbose && c.Detail != "" {
				fmt.Printf("      %s%s\n", strings.Repeat(" ", nameW), c.Detail)
			}
		}
	}

	// Overall: is ACTIONABLE — it names the specific failing/warning
	// checks, not a generic "see above".
	var warnNames, failNames []string
	for _, c := range report.Checks {
		switch c.Status {
		case "warn":
			warnNames = append(warnNames, c.Name)
		case "fail":
			failNames = append(failNames, c.Name)
		}
	}
	switch report.Overall {
	case "ok":
		fmt.Printf("\nOverall: %s — all checks passed.\n", pal.Green("ok"))
	case "warn":
		fmt.Printf("\nOverall: %s — %d warning(s): %s. Run with --verbose for remediation detail.\n",
			pal.Yellow("warn"), len(warnNames), strings.Join(warnNames, ", "))
	default:
		fmt.Printf("\nOverall: %s — %d failure(s): %s. Resolve before launch (--verbose for detail).\n",
			pal.Red("fail"), len(failNames), strings.Join(failNames, ", "))
	}
	return nil
}

func stopCommand(paths app.Paths, args []string) error {
	fs := flag.NewFlagSet("stop", flag.ContinueOnError)
	sessionID := fs.String("session", "", "specific session id")
	all := fs.Bool("all", false, "stop all live sessions")
	if err := fs.Parse(args); err != nil {
		return err
	}
	discovered, err := discovery.Scan(paths)
	if err != nil {
		return err
	}
	var targets []model.DiscoveredSession
	var skipped []string
	switch {
	case *all:
		for _, ds := range discovered {
			if !ds.Reachable {
				if ds.Manifest.SessionID != "" {
					skipped = append(skipped, ds.Manifest.SessionID)
				}
				continue
			}
			targets = append(targets, ds)
		}
	case *sessionID != "":
		found := false
		for _, ds := range discovered {
			if ds.Manifest.SessionID != *sessionID {
				continue
			}
			found = true
			if !ds.Reachable {
				return fmt.Errorf("session %s is not reachable; refuse pid-only stop and run ccwrap gc after the process exits", ds.Manifest.SessionID)
			}
			targets = append(targets, ds)
			break
		}
		if !found {
			return fmt.Errorf("session %s not found", *sessionID)
		}
	default:
		return fmt.Errorf("use --session ID or --all")
	}
	if len(skipped) > 0 {
		sort.Strings(skipped)
		fmt.Fprintf(os.Stderr, "ccwrap: skipped unreachable/stale sessions: %s\n", strings.Join(skipped, ", "))
	}
	if len(targets) == 0 {
		return fmt.Errorf("no matching reachable sessions")
	}
	for _, ds := range targets {
		if err := requestStop(ds); err != nil {
			return err
		}
		if waitForSessionStop(paths, ds.Manifest.SessionID, 3*time.Second) {
			fmt.Println(stopMessage(ds.Manifest.SessionID))
		} else {
			fmt.Printf("stop requested %s\n", ui.ShortID(ds.Manifest.SessionID))
		}
	}
	return nil
}

func gcCommand(paths app.Paths, args []string) error {
	fs := flag.NewFlagSet("gc", flag.ContinueOnError)
	jsonOut := fs.Bool("json", false, "output JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	removed, err := discovery.Cleanup(paths)
	if err != nil {
		return err
	}
	if *jsonOut {
		return printJSON(map[string]any{"removed": removed})
	}
	if len(removed) == 0 {
		fmt.Println("no stale sessions removed")
		return nil
	}
	fmt.Print(gcRemovedLine(removed))
	return nil
}

// sweepOrphanSessions is the crash-safe startup sweep: on every
// `ccwrap` launch, reap the runtime dirs (manifest, socket, log, and the
// per-session bodies/ subdir) of sessions whose supervisor is no longer
// live. It reuses the exact same liveness signal and removal as
// `ccwrap gc` (discovery.Cleanup → staleState + os.RemoveAll of the whole
// sessions/<id> dir). Best-effort: a cleanup failure (e.g. a racing
// peer, permission) must never fail or slow a fresh launch.
func sweepOrphanSessions(paths app.Paths) {
	_, _ = discovery.Cleanup(paths)
}

// stopMessage / gcRemovedLine are cosmetic helpers: 8-char IDs in human
// success messages, behavior otherwise unchanged (gcRemovedLine still
// emits one "removed <id>" line per id; --json paths keep full IDs as
// wire output).
func stopMessage(id string) string { return "stopped " + ui.ShortID(id) }

func gcRemovedLine(ids []string) string {
	var b strings.Builder
	for _, id := range ids {
		b.WriteString("removed " + ui.ShortID(id) + "\n")
	}
	return b.String()
}

func verifyStopTarget(ds model.DiscoveredSession) error {
	if !ds.Reachable {
		return fmt.Errorf("session %s is not reachable; refusing stop request", ds.Manifest.SessionID)
	}
	if strings.TrimSpace(ds.Manifest.SupervisorStartToken) != "" {
		exists, match, err := procmeta.Matches(ds.Manifest.SupervisorPID, ds.Manifest.SupervisorStartToken)
		if err == nil && (!exists || !match) {
			return fmt.Errorf("session %s supervisor identity no longer matches its manifest", ds.Manifest.SessionID)
		}
	}
	client := control.NewClient(ds.Manifest.ControlSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	status, err := client.Status(ctx)
	if err != nil {
		return fmt.Errorf("session %s control plane verification failed: %w", ds.Manifest.SessionID, err)
	}
	if status.SocketPath != "" && status.SocketPath != ds.Manifest.ControlSocket {
		return fmt.Errorf("session %s control socket mismatch", ds.Manifest.SessionID)
	}
	if status.SessionID != "" && status.SessionID != ds.Manifest.SessionID {
		return fmt.Errorf("session %s control session mismatch: got %s", ds.Manifest.SessionID, status.SessionID)
	}
	sess, err := client.GetSession(ctx, ds.Manifest.SessionID)
	if err != nil {
		return fmt.Errorf("session %s identity verification failed: %w", ds.Manifest.SessionID, err)
	}
	if sess.ID != ds.Manifest.SessionID {
		return fmt.Errorf("session %s identity verification returned %s", ds.Manifest.SessionID, sess.ID)
	}
	if sess.SupervisorPID > 0 && sess.SupervisorPID != ds.Manifest.SupervisorPID {
		return fmt.Errorf("session %s supervisor PID mismatch", ds.Manifest.SessionID)
	}
	return nil
}

func requestStop(ds model.DiscoveredSession) error {
	if err := verifyStopTarget(ds); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := control.NewClient(ds.Manifest.ControlSocket)
	if err := client.Shutdown(ctx); err != nil {
		return fmt.Errorf("session %s shutdown request failed: %w", ds.Manifest.SessionID, err)
	}
	return nil
}

func waitForSessionStop(paths app.Paths, sessionID string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ds, err := discovery.Find(paths, sessionID)
		if err != nil {
			return true
		}
		if ds == nil || ds.Stale || !ds.Reachable {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func writeManifest(sessionDir, controlSocket string, sess *model.Session, eg model.EgressConfig) error {
	if sess == nil {
		return fmt.Errorf("nil session")
	}
	startToken, err := procmeta.CurrentStartToken()
	if err != nil {
		return fmt.Errorf("resolve supervisor start token: %w", err)
	}
	m := model.SessionManifest{
		SessionID:                          sess.ID,
		CreatedAt:                          sess.CreatedAt,
		UpdatedAt:                          time.Now(),
		State:                              sess.State,
		SupervisorPID:                      os.Getpid(),
		SupervisorStartToken:               startToken,
		ClaudePID:                          sess.ClaudePID,
		Name:                               sess.Name,
		ControlSocket:                      controlSocket,
		ProxyListenAddr:                    sess.ProxyListenAddr,
		ProxyInfoURL:                       proxyInfoURL(sess.ProxyListenAddr),
		RouteClass:                         sess.RouteClass,
		RouteSource:                        sess.RouteSource,
		RouteConfigSource:                  sess.RouteConfigSource,
		ExactUpstreamBase:                  sess.ExactUpstreamBase,
		AuthMode:                           sess.AuthMode,
		AuthSource:                         sess.AuthSource,
		AuthConfigSource:                   sess.AuthConfigSource,
		AuthPolicy:                         sess.AuthPolicy,
		AuthBootstrap:                      sess.AuthBootstrap,
		AuthBootstrapKind:                  sess.AuthBootstrapKind,
		EgressMode:                         eg.Mode,
		EgressSource:                       eg.Source,
		EgressSummary:                      egress.Summary(eg),
		ModelAliasMode:                     sess.ModelAliasMode,
		ModelAliasSource:                   sess.ModelAliasSource,
		ModelAliasCount:                    sess.ModelAliasCount,
		ModelAliasProviderModelPassthrough: sess.ModelAliasProviderModelPassthrough,
		ModelAliasFingerprint:              sess.ModelAliasFingerprint,
	}
	return manifest.Write(manifest.Path(sessionDir), m)
}

func fetchSnapshot(ds model.DiscoveredSession) (*model.Session, error) {
	if !ds.Reachable {
		return &model.Session{
			ID:                                 ds.Manifest.SessionID,
			Name:                               ds.Manifest.Name,
			CreatedAt:                          ds.Manifest.CreatedAt,
			UpdatedAt:                          ds.Manifest.UpdatedAt,
			State:                              ds.Manifest.State,
			SupervisorPID:                      ds.Manifest.SupervisorPID,
			ClaudePID:                          ds.Manifest.ClaudePID,
			ProxyListenAddr:                    ds.Manifest.ProxyListenAddr,
			RouteClass:                         ds.Manifest.RouteClass,
			RouteSource:                        ds.Manifest.RouteSource,
			RouteConfigSource:                  ds.Manifest.RouteConfigSource,
			ExactUpstreamBase:                  ds.Manifest.ExactUpstreamBase,
			AuthMode:                           ds.Manifest.AuthMode,
			AuthSource:                         ds.Manifest.AuthSource,
			AuthConfigSource:                   ds.Manifest.AuthConfigSource,
			AuthPolicy:                         ds.Manifest.AuthPolicy,
			AuthBootstrap:                      ds.Manifest.AuthBootstrap,
			AuthBootstrapKind:                  ds.Manifest.AuthBootstrapKind,
			EgressMode:                         ds.Manifest.EgressMode,
			EgressSource:                       ds.Manifest.EgressSource,
			EgressSummary:                      ds.Manifest.EgressSummary,
			ModelAliasMode:                     ds.Manifest.ModelAliasMode,
			ModelAliasSource:                   ds.Manifest.ModelAliasSource,
			ModelAliasCount:                    ds.Manifest.ModelAliasCount,
			ModelAliasProviderModelPassthrough: ds.Manifest.ModelAliasProviderModelPassthrough,
			ModelAliasFingerprint:              ds.Manifest.ModelAliasFingerprint,
		}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client := control.NewClient(ds.Manifest.ControlSocket)
	sess, err := client.GetSession(ctx, ds.Manifest.SessionID)
	if err == nil {
		return sess, nil
	}
	list, err2 := client.ListSessions(ctx)
	if err2 == nil && len(list) > 0 {
		return &list[0], nil
	}
	return nil, err
}

func waitForControl(ctx context.Context, client *control.Client) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if _, err := client.Status(ctx); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("session supervisor did not become ready on %s", client.SocketPath())
}

func splitDoubleDash(args []string) ([]string, []string) {
	for i, arg := range args {
		if arg == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func forwardSignalsToChild(cancel context.CancelFunc, p *os.Process) func() {
	sigCh := make(chan os.Signal, 8)
	stopCh := make(chan struct{})
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		for {
			select {
			case sig := <-sigCh:
				if p != nil {
					_ = p.Signal(sig)
				}
				if cancel != nil {
					cancel()
				}
			case <-stopCh:
				return
			}
		}
	}()
	return func() {
		signal.Stop(sigCh)
		close(stopCh)
	}
}

// wrapHero soft-wraps a sentence at width on word boundaries.
// Returns one entry per visual line (no leading indent — caller adds it).
func wrapHero(s string, width int) []string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return []string{""}
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) > width {
			out = append(out, line)
			line = w
		} else {
			line += " " + w
		}
	}
	return append(out, line)
}

// authMissingRecoverHint returns the one-line recovery instruction shown when a
// launch proceeds with no usable upstream auth. Shared by the full banner and
// the --quiet one-liner so the wording can't drift.
func authMissingRecoverHint(missingAuthEnv string) string {
	if missingAuthEnv == "" {
		return "edit profiles.json to add auth.key_env, switch profile via inspect, or use `ccwrap --profile inherit-env`"
	}
	return "set $" + missingAuthEnv + ", switch profile via inspect, or use `ccwrap --profile inherit-env`"
}

// quietSummaryLine renders the one-line launch summary used by --quiet /
// CCWRAP_QUIET: where traffic exits (host) plus the inspect URL, with the
// profile name and a degraded marker when relevant. The full multi-row banner
// is suppressed in quiet mode.
func quietSummaryLine(pal ui.Palette, host, profileName string, degraded bool, proxyURL string) string {
	line := pal.Bold("ccwrap") + " → " + host
	if strings.TrimSpace(profileName) != "" {
		line += " · " + profileName
	}
	if degraded {
		line += " · " + pal.Status("degraded")
	}
	line += " · " + pal.Dim("inspect") + " " + proxyURL
	return line
}

func printLaunchSummary(sessionID, proxyURL, upstream string, routeSource model.RouteSource, routeClass model.RouteClass, authMode model.AuthMode, authSource model.AuthSource, authPolicy model.AuthPolicy, authBootstrap model.AuthBootstrap, authBootstrapKind model.AuthBootstrapKind, egressMode, egressSource, egressSummary string, alias modelalias.Config, headers upstreamheaders.Config, pid int, profileName, profileProvider, missingAuthEnv string) {
	pal := ui.New(ui.IsTerminal(os.Stderr))

	// Build a transient session so the hero sentence comes from the
	// single source (ui.SessionPosture) shared with status/web/TUI.
	host := upstream
	if u, err := url.Parse(upstream); err == nil && u.Host != "" {
		host = u.Host
	}
	posture := ui.SessionPosture(&model.Session{
		RouteClass:        routeClass,
		ExactUpstreamHost: host,
		AuthPolicy:        authPolicy,
		ModelAliasCount:   alias.Count(),
		State:             model.StateActive,
	}, nil) // launch summary has no traffic/errors yet

	var state string
	if routeClass == model.RouteClassThirdPartyCompatible || authPolicy == model.AuthPolicyUnsafePassthrough {
		state = pal.Status("ready") + pal.Status(" · degraded")
	} else {
		state = pal.Status("ready")
	}

	// Model alias: 1 → "1 alias · logical → provider";
	// >1 → "N aliases · first → firstval, +N more" (deterministic:
	// keys sorted so output is stable across runs).
	modelsLine := "0 aliases"
	if alias.Enabled() && alias.Count() > 0 {
		keys := make([]string, 0, len(alias.Forward))
		for k := range alias.Forward {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		first := keys[0]
		if alias.Count() == 1 {
			modelsLine = "1 alias · " + first + " → " + alias.Forward[first]
		} else {
			modelsLine = fmt.Sprintf("%d aliases · %s → %s, +%d more",
				alias.Count(), first, alias.Forward[first], alias.Count()-1)
		}
	}

	lines := []string{pal.Bold("ccwrap") + "  " + state, ""}
	// The hero sentence wraps. cmd/ccwrap has no stderr width detection;
	// wrap at a fixed 76 cols, matching the ASCII block width.
	for _, hl := range wrapHero(posture, 76) {
		lines = append(lines, "  "+hl)
	}
	// auth = HumanAuthPolicy, plus HumanAuthBootstrap ONLY when meaningful
	// (not the suppressed "none"/not-needed). The HumanAuth scheme prefix
	// is intentionally dropped — it duplicated HumanAuthPolicy (first-party
	// passthrough rendered the degenerate "Passthrough · Passthrough ·
	// none") and the scheme is available in --verbose / the Web config
	// drawer.
	// When preflight resolved auth as Missing (active profile asked for
	// ccwrap-owned auth but no source available), the auth row shows a
	// danger-prefixed line + the banner gains a recovery hint at the
	// bottom. Branches on missingAuthEnv emptiness: non-empty = Case A
	// (concrete env), empty = Case B (no source configured).
	authMissing := authBootstrap == model.AuthBootstrapMissing
	authLine := ui.HumanAuthPolicy(authPolicy)
	if authMissing {
		profileLabel := profileName
		if strings.TrimSpace(profileLabel) == "" {
			profileLabel = "(no profile)"
		}
		if missingAuthEnv != "" {
			authLine = "⚠ MISSING — profile " + strconv.Quote(profileLabel) + " needs $" + missingAuthEnv
		} else {
			authLine = "⚠ MISSING — profile " + strconv.Quote(profileLabel) + " has no auth source configured"
		}
	} else if bs := ui.HumanAuthBootstrap(authBootstrap, authBootstrapKind); bs != "" && bs != "none" {
		authLine += " · " + bs
	}
	lines = append(lines, "",
		"  "+pal.Dim("session")+"   "+pal.Cyan(ui.ShortID(sessionID))+" · "+fmt.Sprintf("pid %d", pid),
		// No separate `proxy` row — the proxy endpoint IS the `inspect`
		// endpoint (one row carries that URL, below).
		"  "+pal.Dim("upstream")+"  "+host+" · "+ui.HumanRouteClass(routeClass)+" · "+ui.HumanRouteSource(routeSource),
	)
	// When a profile is active, one extra row names it (name · Provider),
	// in the existing label/`·` grid — no new color/vocabulary.
	// inherit-env (empty name) prints NO profile row: the zero-touch look
	// is unchanged. Never a credential — only the non-secret
	// name/provider.
	if strings.TrimSpace(profileName) != "" {
		profileLine := profileName
		if strings.TrimSpace(profileProvider) != "" {
			profileLine += " · " + profileProvider
		}
		lines = append(lines, "  "+pal.Dim("profile")+"   "+profileLine)
	}
	lines = append(lines,
		"  "+pal.Dim("auth")+"      "+authLine,
		"  "+pal.Dim("models")+"    "+modelsLine,
	)
	if len(headers.Headers) > 0 {
		lines = append(lines, "  "+pal.Dim("headers")+"   "+fmt.Sprintf("%d · %s", len(headers.Headers), headers.Fingerprint))
	}
	if eg := ui.HumanEgress(egressMode, egressSource, egressSummary); eg != "Direct" {
		lines = append(lines, "  "+pal.Dim("egress")+"    "+eg)
	}
	lines = append(lines, "  "+pal.Dim("inspect")+"   "+proxyURL+"  ·  ccwrap dashboard (open in browser)")
	// When launch proceeds with missing auth, a one-time banner-bottom
	// warning lists the concrete recovery options. Single blank line
	// above for visual separation from the inspect row.
	if authMissing {
		lines = append(lines, "",
			"  ⚠ Requests will fail until you "+authMissingRecoverHint(missingAuthEnv)+".",
		)
	}

	for _, line := range lines {
		fmt.Fprintln(os.Stderr, line)
	}
}

func printStatus(discovered []model.DiscoveredSession, sessions []*model.Session) {
	pal := ui.New(ui.IsTerminal(os.Stdout))
	active, stale := 0, 0
	for _, ds := range discovered {
		if ds.Stale || !ds.Reachable {
			stale++
		} else {
			active++
		}
	}
	fmt.Printf("%s — %d active · %d stale\n\n", pal.Bold("CCWRAP Status"), active, stale)
	if len(sessions) == 0 {
		fmt.Println("No sessions")
		return
	}
	for _, sess := range sessions {
		if sess == nil {
			continue
		}
		head := pal.Cyan(ui.ShortID(sess.ID)) + "  " + pal.Status(string(sess.State))
		if sess.ClaudePID > 0 {
			head += "   " + pal.Dim(fmt.Sprintf("claude pid %d", sess.ClaudePID))
		}
		fmt.Println(head)
		if p := ui.SessionPosture(sess, nil); p != "" { // status has no error list in scope
			fmt.Println("  " + p)
		}
		fmt.Println()
		auth := ui.HumanAuth(sess.AuthMode, sess.AuthSource) + " · " + ui.HumanAuthPolicy(sess.AuthPolicy) + " · " + ui.HumanAuthBootstrap(sess.AuthBootstrap, sess.AuthBootstrapKind)
		fmt.Println("  " + pal.Dim("proxy") + "     " + proxyInfoURL(sess.ProxyListenAddr))
		fmt.Println("  " + pal.Dim("route") + "     " + ui.HumanRouteClass(sess.RouteClass) + " · " + strings.ToLower(ui.HumanRouteSource(sess.RouteSource)))
		fmt.Println("  " + pal.Dim("auth") + "      " + auth)
		fmt.Println("  " + pal.Dim("models") + "    " + statusModelsLine(sess))
		if eg := ui.HumanEgress(sess.EgressMode, sess.EgressSource, sess.EgressSummary); eg != "Direct" {
			fmt.Println("  " + pal.Dim("egress") + "    " + eg)
		}
		fmt.Println("  " + pal.Dim("traffic") + "   " + fmt.Sprintf("%d req · %d err", sess.RecentRequestCount, sess.RecentErrorCount))
		fmt.Println()
	}
}

// statusModelsLine renders the models field from a *model.Session.
// model.Session carries only ModelAliasCount, not the logical→provider
// map, so ccwrap status shows the count form. The ccwrap launch summary
// renders the full mapping (it has modelalias.Config.Forward).
func statusModelsLine(sess *model.Session) string {
	switch {
	case sess.ModelAliasCount == 1:
		return "1 alias"
	case sess.ModelAliasCount > 1:
		return fmt.Sprintf("%d aliases", sess.ModelAliasCount)
	default:
		return "0 aliases"
	}
}

func proxyInfoURL(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return ""
	}
	return "http://" + addr + "/"
}

func printJSON(v interface{}) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func nilSafeSettings(parsed *settings.ParsedFlagSettings) map[string]any {
	if parsed == nil || parsed.Settings == nil {
		return nil
	}
	return parsed.Settings
}

func writeSessionSettingsFile(path string, doc map[string]any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func inheritedCustomHeadersDoctorConflict(env map[string]string) []settings.EnvConflict {
	if len(settings.AuthLikeCustomHeaderNames(env["ANTHROPIC_CUSTOM_HEADERS"])) == 0 {
		return nil
	}
	return []settings.EnvConflict{{Source: "inherited_env", Path: "process env", Keys: []string{"ANTHROPIC_CUSTOM_HEADERS"}}}

}

func formatEnvConflicts(conflicts []settings.EnvConflict) string {
	parts := make([]string, 0, len(conflicts))
	for _, conflict := range conflicts {
		parts = append(parts, fmt.Sprintf("%s[%s]: %s", conflict.Source, conflict.Path, strings.Join(conflict.Keys, ", ")))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
}

func formatMalformedEnvIssues(issues []settings.MalformedEnvIssue) string {
	parts := make([]string, 0, len(issues))
	for _, issue := range issues {
		parts = append(parts, fmt.Sprintf("%s[%s]: %s", issue.Source, issue.Path, issue.Error))
	}
	sort.Strings(parts)
	return strings.Join(parts, "; ")
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
// for the rare case utls misbehaves on a Go/undici combo). There is no CLI flag
// and it is not a user-facing config knob -- looking native is the default.
//
// DANGER of disabling it: with mirroring off, ccwrap dials Anthropic with Go's
// stdlib TLS, so the upstream sees a Go fingerprint under Claude Code's undici
// HTTP headers -- the exact mismatch that marks the traffic as a non-native
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

// migrationDisabled reads from parentEnv (snapshot) — consistent with
// every other env read in maybeMigrateFromEnv. truthyEnv accepts only
// "1" or case-insensitive "true".
func migrationDisabled(parentEnv []string, noInit bool) bool {
	if noInit {
		return true
	}
	return truthyEnv(lookupEnv(parentEnv, "CCWRAP_NO_INIT"))
}

// isTerminalFn is the unit-testable seam for ui.IsTerminal. Production
// code keeps the default; tests in cmd/ccwrap/main_test.go override it via
// stubIsTerminal. Defined as a var (not const) so it can be swapped.
var isTerminalFn = ui.IsTerminal

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

// --- Timezone alignment ------------------------------------------------------
//
// Claude Code stamps "Today's date is <YYYY-MM-DD>." into its request system
// prompt, computed from the LOCAL timezone. A China-local session therefore
// leaks a +8h date that can be one day ahead of a US "first-party" session and
// de-anonymizes its region. ccwrap does not scrub TZ, so setting env["TZ"] on
// the child aligns that date. These helpers resolve the effective TZ to inject
// (flag/env → persisted → first-run China-TZ prompt), validate it, and persist
// the first-run decision so it applies on every future launch without
// re-prompting.

// defaultInjectTimezone is offered as the Enter-default in the first-run prompt.
const defaultInjectTimezone = "America/Los_Angeles"

// timezoneSkipSentinel is the value persisted to <stateDir>/timezone when the
// user declines injection. It is a deliberately non-IANA token: reading it back
// suppresses both re-prompting and injection. time.LoadLocation is never called
// on it (the sentinel is mapped to "no injection" before validation).
const timezoneSkipSentinel = "none"

// chinaZoneNames are the IANA zone names that resolve to China Standard Time.
var chinaZoneNames = map[string]bool{
	"Asia/Shanghai":  true,
	"Asia/Urumqi":    true,
	"Asia/Chongqing": true,
	"Asia/Harbin":    true,
	"Asia/Kashgar":   true,
}

// isChinaTimezone reports whether the local zone looks like China. It gates ONLY
// the first-run prompt, so a loose heuristic is fine: a known China IANA name
// (from the TZ env or the /etc/localtime symlink target) matches directly;
// otherwise a current local UTC offset of exactly +8h (28800s) is a weak signal.
// An empty name with a non-+8h offset is not China.
func isChinaTimezone(zoneName string, offsetSeconds int) bool {
	if name := strings.TrimSpace(zoneName); name != "" {
		return chinaZoneNames[name]
	}
	return offsetSeconds == 28800
}

// detectLocalTimezone returns the local zone name (TZ env, else the IANA name
// parsed from the /etc/localtime symlink target) and the current local UTC
// offset in seconds (used only for the weak +8h fallback in isChinaTimezone).
func detectLocalTimezone(parentEnv []string) (string, int) {
	name := strings.TrimSpace(lookupEnv(parentEnv, "TZ"))
	if name == "" {
		name = zoneNameFromLocaltimeLink()
	}
	_, offset := time.Now().Zone()
	return name, offset
}

// zoneNameFromLocaltimeLink reads /etc/localtime and, if it is a symlink into a
// zoneinfo tree ("/usr/share/zoneinfo/Asia/Shanghai"), returns the IANA suffix
// ("Asia/Shanghai"). Returns "" when /etc/localtime is not a zoneinfo symlink.
func zoneNameFromLocaltimeLink() string {
	target, err := os.Readlink("/etc/localtime")
	if err != nil {
		return ""
	}
	const marker = "/zoneinfo/"
	if idx := strings.LastIndex(target, marker); idx >= 0 {
		return target[idx+len(marker):]
	}
	return ""
}

// timezonePersistPath is the small per-user file that records the first-run
// timezone decision (a chosen IANA name, or timezoneSkipSentinel).
func timezonePersistPath(stateDir string) string {
	return filepath.Join(stateDir, "timezone")
}

// readPersistedTimezone returns the persisted decision, or "" when there is no
// decision yet. Read errors (missing file, unreadable) are tolerated as "no
// decision" so a first run is never blocked by a bad state dir.
func readPersistedTimezone(stateDir string) string {
	data, err := os.ReadFile(timezonePersistPath(stateDir))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writePersistedTimezone records the decision, creating the state dir if needed.
func writePersistedTimezone(stateDir, value string) error {
	path := timezonePersistPath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(value+"\n"), 0o644)
}

// resolveInjectedTimezone returns the first non-empty of, in precedence order:
// the --timezone flag, the CCWRAP_TZ env, the persisted decision, then the
// first-run prompt result. "" means no override. It is pure (no I/O, no
// validation) so precedence is unit-testable in isolation; the caller validates
// and interprets the skip sentinel.
func resolveInjectedTimezone(flag, env, persisted, promptResult string) string {
	for _, v := range []string{flag, env, persisted, promptResult} {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// timezonePromptDisabled gates the first-run timezone prompt. It reuses the
// profile-migration opt-out (--no-init / CCWRAP_NO_INIT) and adds a dedicated
// CCWRAP_NO_TZ_PROMPT for users who want profile migration but not this prompt.
func timezonePromptDisabled(parentEnv []string, noInit bool) bool {
	if migrationDisabled(parentEnv, noInit) {
		return true
	}
	return truthyEnv(lookupEnv(parentEnv, "CCWRAP_NO_TZ_PROMPT"))
}

// promptTimezoneDecision prints the date-leak explanation and reads one line
// from stdin, returning the decision: Enter → defaultInjectTimezone, "n"/"no"
// (case-insensitive) → timezoneSkipSentinel, any other text → that text (a
// candidate IANA name, validated later by the caller). EOF (Ctrl-D) is treated
// as skip so the prompt persists a decision and never nags again.
func promptTimezoneDecision(stdin io.Reader) string {
	fmt.Fprintln(os.Stderr, "ccwrap: 检测到当前为中国时区，Claude Code 会把本机当天日期写进请求，")
	fmt.Fprintln(os.Stderr, "ccwrap:   这可能暴露会话所在地区（+8 时区的日期可能比美国早一天）。")
	fmt.Fprintln(os.Stderr, "ccwrap:   (Detected a China timezone; the stamped date can reveal your region.)")
	fmt.Fprintln(os.Stderr, "ccwrap:   是否为 Claude Code 指定时区（让请求里的日期按该时区计算）？")
	fmt.Fprintf(os.Stderr, "ccwrap:   回车=%s，或输入一个 IANA 时区名，输入 n 跳过：", defaultInjectTimezone)
	scanner := bufio.NewScanner(stdin)
	if !scanner.Scan() {
		return timezoneSkipSentinel // EOF → persist skip, never re-prompt
	}
	answer := strings.TrimSpace(scanner.Text())
	switch strings.ToLower(answer) {
	case "":
		return defaultInjectTimezone
	case "n", "no":
		return timezoneSkipSentinel
	default:
		return answer
	}
}

// maybePromptTimezone runs the first-run China-timezone prompt and returns the
// user's decision (an IANA name, or timezoneSkipSentinel), persisting it so
// future launches never re-prompt. It returns "" without prompting or
// persisting when any precondition fails: opted out, non-TTY stdin, a decision
// already persisted, or the local zone is not China. stdin is read for the
// answer; the TTY gate goes through the isTerminalFn seam (stubbed in tests).
func maybePromptTimezone(stateDir string, parentEnv []string, noInit bool, stdin io.Reader) string {
	if timezonePromptDisabled(parentEnv, noInit) {
		return ""
	}
	if !isTerminalFn(os.Stdin) {
		return ""
	}
	if readPersistedTimezone(stateDir) != "" {
		return "" // already decided on a prior run
	}
	zoneName, offset := detectLocalTimezone(parentEnv)
	if !isChinaTimezone(zoneName, offset) {
		return ""
	}
	decision := promptTimezoneDecision(stdin)
	if err := writePersistedTimezone(stateDir, decision); err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap: could not persist timezone decision: %v\n", err)
	}
	return decision
}

// resolveEffectiveTimezone resolves and validates the TZ to inject into the
// child. flagOrEnvTZ is launch.Timezone (--timezone over CCWRAP_TZ, collapsed by
// parseLaunchArgs). It consults the persisted decision and, only when nothing
// explicit or persisted exists, the first-run prompt. The result is validated
// via time.LoadLocation: an invalid zone warns on stderr and is dropped (no
// injection). The skip sentinel maps to "" (no injection). "" means the child
// env is byte-identical to today's.
func resolveEffectiveTimezone(stateDir, flagOrEnvTZ string, parentEnv []string, noInit bool) string {
	persisted := readPersistedTimezone(stateDir)
	var promptResult string
	if strings.TrimSpace(flagOrEnvTZ) == "" && persisted == "" {
		promptResult = maybePromptTimezone(stateDir, parentEnv, noInit, os.Stdin)
	}
	// env slot stays empty: parseLaunchArgs already folded CCWRAP_TZ into
	// flagOrEnvTZ (flag winning), so the precedence flag>env is settled upstream.
	resolved := resolveInjectedTimezone(flagOrEnvTZ, "", persisted, promptResult)
	if resolved == "" || resolved == timezoneSkipSentinel {
		return ""
	}
	if _, err := time.LoadLocation(resolved); err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap: ignoring invalid timezone %q (not loadable): %v\n", resolved, err)
		return ""
	}
	return resolved
}

func maybeDetail(verbose bool, detail string) string {
	if !verbose {
		return ""
	}
	return detail
}

func fallbackText(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

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
