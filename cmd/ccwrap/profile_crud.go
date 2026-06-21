// cmd/ccwrap/profile_crud.go
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/discovery"
	"github.com/Hoper-J/ccwrap/internal/modelalias"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/upstreamheaders"
)

// repeatedFlag collects a repeatable string flag, e.g.
// `--model-alias a=b --model-alias c=d` → []string{"a=b","c=d"}.
type repeatedFlag []string

func (r *repeatedFlag) String() string     { return strings.Join(*r, ",") }
func (r *repeatedFlag) Set(v string) error { *r = append(*r, v); return nil }

// stdinAuthKeyCapBytes is the maximum stdin payload accepted by
// --auth-key-stdin. Tokens are <10KB realistically; 4 MiB is a
// safety cap.
const stdinAuthKeyCapBytes = 4 * 1024 * 1024

// readStdinAuthKey reads up to stdinAuthKeyCapBytes+1 from r, then
// detects overflow if the read landed exactly at the cap+1 boundary.
// Trims trailing \r\n or \n. Returns an error for empty post-trim.
func readStdinAuthKey(r io.Reader) (string, error) {
	buf, err := io.ReadAll(io.LimitReader(r, int64(stdinAuthKeyCapBytes)+1))
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	if len(buf) > stdinAuthKeyCapBytes {
		return "", fmt.Errorf("--auth-key-stdin: input exceeds 4 MiB cap")
	}
	s := string(buf)
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	if s == "" {
		return "", fmt.Errorf("--auth-key-stdin: stdin was empty")
	}
	return s, nil
}

// authKeySource enumerates the four mutually-exclusive auth-key inputs.
type authKeySource int

const (
	authKeyNone   authKeySource = iota // no --auth-key* flag passed
	authKeyInline                      // --auth-key VALUE
	authKeyStdin                       // --auth-key-stdin
	authKeyEnv                         // --auth-key-env VAR
)

// crudOpts is the parsed-and-validated input bundle shared by add/edit.
// rm + set-default use simpler parsers (they don't touch auth/egress).
type crudOpts struct {
	Name          string
	Provider      string
	BaseURL       string
	AuthMode      string
	AuthKeySource authKeySource
	AuthKeyInline string // populated when AuthKeySource==authKeyInline
	AuthKeyEnv    string // populated when AuthKeySource==authKeyEnv
	EgressMode    string
	EgressURL     string
	SetDefault    bool

	// ModelAliases / UpstreamHeaders are nil when their flags weren't passed
	// (use SetFields to distinguish "not passed" from "passed empty").
	ModelAliases    map[string]string
	UpstreamHeaders map[string]string

	// SetFields tracks which flags were explicitly set (via fs.Visit).
	// Used by edit to distinguish "not passed" from "passed empty".
	// add does not consult SetFields — it treats unset flags as zero values.
	SetFields map[string]bool
}

// parseAddArgs parses `ccwrap profile add <name> [flags]` and validates
// the requireds + mutex rules. The first positional is required.
func parseAddArgs(args []string) (crudOpts, error) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return crudOpts{}, fmt.Errorf("missing profile <name> as first positional argument")
	}
	name := args[0]
	rest := args[1:]
	opts, err := parseCRUDFlags("profile add", name, rest, true)
	if err != nil {
		return crudOpts{}, err
	}
	// add-side required checks beyond shared validation.
	if strings.TrimSpace(opts.BaseURL) == "" {
		return crudOpts{}, fmt.Errorf("--base-url is required for add")
	}
	if strings.TrimSpace(opts.AuthMode) == "" {
		return crudOpts{}, fmt.Errorf("--auth-mode is required for add")
	}
	return opts, nil
}

// parseCRUDFlags is the shared parser used by add (requireAddSemantics=true)
// and edit (requireAddSemantics=false).
//
// requireAddSemantics enforces:
//   - "ccwrap_* MUST have a key" pairing
//   - fast-fail on the removed "passthrough" mode value (V1: a no-auth
//     profile is an ABSENT auth block, a shape add cannot write yet)
//
// edit defers the auth-mode handling to applyEditOpts, which owns the
// same passthrough rejection on the edit path.
func parseCRUDFlags(prog, name string, args []string, requireAddSemantics bool) (crudOpts, error) {
	if name == "" || name != strings.TrimSpace(name) {
		return crudOpts{}, fmt.Errorf("profile name must not be empty or contain leading/trailing whitespace")
	}

	fs := flag.NewFlagSet(prog, flag.ContinueOnError)
	fs.SetOutput(nullWriter{}) // swallow flag's auto-printing — caller owns formatting

	provider := fs.String("provider", "", "Profile provider (group label)")
	baseURL := fs.String("base-url", "", "Upstream base URL (http or https)")
	authMode := fs.String("auth-mode", "", "Auth mode: ccwrap_bearer | ccwrap_x_api_key")
	authKey := fs.String("auth-key", "", "Inline auth key value")
	authKeyStdinFlag := fs.Bool("auth-key-stdin", false, "Read auth key from stdin")
	authKeyEnvFlag := fs.String("auth-key-env", "", "Env-var name holding the auth key")
	egressMode := fs.String("egress-mode", "", "Egress mode: inherit | direct | http | socks5 | socks5h")
	egressURL := fs.String("egress-url", "", "Egress proxy URL (required when egress-mode=http|socks5|socks5h; scheme must match mode)")
	setDefault := fs.Bool("set-default", false, "Set this profile as the file's default after the change")
	var aliasPairs repeatedFlag
	fs.Var(&aliasPairs, "model-alias", "Model alias logical=provider (repeatable)")
	var headerPairs repeatedFlag
	fs.Var(&headerPairs, "upstream-header", "Upstream header Name=Value (repeatable; CCWRAP-owned, never Claude-visible)")

	if err := fs.Parse(args); err != nil {
		return crudOpts{}, err
	}
	if rest := fs.Args(); len(rest) > 0 {
		return crudOpts{}, fmt.Errorf("unexpected extra args: %v", rest)
	}

	// Mutex on the three auth-key sources.
	srcCount := 0
	src := authKeyNone
	if *authKey != "" {
		srcCount++
		src = authKeyInline
	}
	if *authKeyStdinFlag {
		srcCount++
		src = authKeyStdin
	}
	if *authKeyEnvFlag != "" {
		srcCount++
		src = authKeyEnv
	}
	if srcCount > 1 {
		return crudOpts{}, fmt.Errorf("--auth-key, --auth-key-stdin, --auth-key-env are mutually exclusive")
	}

	// Egress flag pairing — privacy guard:
	//   - http|socks5|socks5h + missing URL → error
	//   - non-url-bearing mode (inherit/direct) + supplied URL → error
	//     (would silently store a possibly-credentialed URL under a mode
	//     that ignores it)
	mode := strings.ToLower(strings.TrimSpace(*egressMode))
	hasURL := strings.TrimSpace(*egressURL) != ""
	urlBearing := mode == "http" || mode == "socks5" || mode == "socks5h"
	if urlBearing && !hasURL {
		return crudOpts{}, fmt.Errorf("--egress-mode %s requires --egress-url", mode)
	}
	if (mode == "inherit" || mode == "direct") && hasURL {
		return crudOpts{}, fmt.Errorf("--egress-mode %s is incompatible with --egress-url", mode)
	}

	// Model aliases / upstream headers — reuse the launch-side validators so the
	// "logical=provider" / "Name=Value" rules and error messages are identical.
	var modelAliases map[string]string
	if len(aliasPairs) > 0 {
		m, perr := modelalias.ParsePairs(aliasPairs)
		if perr != nil {
			return crudOpts{}, perr
		}
		modelAliases = m
	}
	var upstreamHeaders map[string]string
	if len(headerPairs) > 0 {
		h, perr := upstreamheaders.ParsePairs(headerPairs)
		if perr != nil {
			return crudOpts{}, perr
		}
		upstreamHeaders = h
	}

	opts := crudOpts{
		Name:            name,
		Provider:        *provider,
		BaseURL:         *baseURL,
		AuthMode:        *authMode,
		AuthKeySource:   src,
		AuthKeyInline:   *authKey,
		AuthKeyEnv:      *authKeyEnvFlag,
		EgressMode:      *egressMode,
		EgressURL:       *egressURL,
		SetDefault:      *setDefault,
		ModelAliases:    modelAliases,
		UpstreamHeaders: upstreamHeaders,
		SetFields:       map[string]bool{},
	}
	fs.Visit(func(fl *flag.Flag) { opts.SetFields[fl.Name] = true })

	// add-side: auth-mode <-> auth-key pairing.
	if requireAddSemantics {
		am := strings.ToLower(strings.TrimSpace(opts.AuthMode))
		switch am {
		case "passthrough":
			// V1 removed "passthrough" as a mode value: a no-auth profile is
			// expressed by an ABSENT auth block (see profiles.validateAuth).
			// add has no way to write that shape yet, so fail fast at parse
			// time with the workaround — building the profile anyway would
			// only defer to a guaranteed write-time validation reject.
			return crudOpts{}, fmt.Errorf(`--auth-mode "passthrough" is no longer a mode value; a no-auth profile has NO auth block — omit "auth" for that profile by editing profiles.json directly (profile add cannot create no-auth profiles yet)`)
		case "ccwrap_bearer", "ccwrap_x_api_key":
			if opts.AuthKeySource == authKeyNone {
				return crudOpts{}, fmt.Errorf("--auth-mode %s requires --auth-key, --auth-key-stdin, or --auth-key-env", am)
			}
		}
	}

	return opts, nil
}

// buildProfileFromOpts constructs a profiles.Profile from the parsed
// crudOpts. stdin is consulted only when opts.AuthKeySource ==
// authKeyStdin; callers may pass nil otherwise.
//
// Name is set on the Profile so callers can pass the result to
// formatProfileLine without an extra assignment
// (cmd/ccwrap/profile.go::formatProfileLine reads p.Name).
func buildProfileFromOpts(opts crudOpts, stdin io.Reader) (profiles.Profile, error) {
	p := profiles.Profile{
		Name:     opts.Name,
		Provider: opts.Provider,
		BaseURL:  opts.BaseURL,
		Auth:     &profiles.AuthSpec{Mode: opts.AuthMode},
		Egress:   profiles.EgressSpec{Mode: opts.EgressMode, URL: opts.EgressURL},
	}
	switch opts.AuthKeySource {
	case authKeyInline:
		p.Auth.Key = opts.AuthKeyInline
	case authKeyEnv:
		p.Auth.KeyEnv = opts.AuthKeyEnv
	case authKeyStdin:
		if stdin == nil {
			return profiles.Profile{}, fmt.Errorf("--auth-key-stdin: no stdin reader available")
		}
		key, err := readStdinAuthKey(stdin)
		if err != nil {
			return profiles.Profile{}, err
		}
		p.Auth.Key = key
	}
	if len(opts.ModelAliases) > 0 {
		p.ModelAliases = opts.ModelAliases
	}
	if len(opts.UpstreamHeaders) > 0 {
		p.UpstreamHeaders = opts.UpstreamHeaders
	}
	return p, nil
}

// profileAdd implements `ccwrap profile add <name> [flags]`. Exit codes:
//
//	0 — success
//	1 — operational failure (load, name conflict, validation, write)
//	2 — usage error (already reported)
func profileAdd(paths app.Paths, args []string) int {
	return profileAddIO(paths, os.Stdin, args)
}

func profileAddIO(paths app.Paths, stdin io.Reader, args []string) int {
	opts, err := parseAddArgs(args)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile add:", err)
		return 2
	}
	// Hold the cross-process lock across load→mutate→write so a
	// concurrent ccwrap process (CLI or supervisor) cannot clobber this
	// add. See profiles.Lock.
	unlock, err := profiles.Lock(paths.StateDir)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile add:", err)
		return 1
	}
	defer unlock()
	path := profiles.DefaultPath(paths.StateDir)
	f, err := profiles.Load(path)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile add:", err)
		return 1
	}
	if f == nil {
		f = &profiles.File{Default: profiles.InheritEnv, Profiles: map[string]profiles.Profile{}}
	}
	if _, exists := f.Profiles[opts.Name]; exists {
		fmt.Fprintf(stdErr(), "ccwrap profile add: profile %q already exists; use 'edit' to modify\n", opts.Name)
		return 1
	}
	p, err := buildProfileFromOpts(opts, stdin)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile add:", err)
		return 1
	}
	f.Profiles[opts.Name] = p
	if opts.SetDefault {
		f.Default = opts.Name
	}
	label := "profile add " + opts.Name
	if err := profiles.OverwriteFile(path, f, label); err != nil {
		var perr *profiles.ParseErrors
		if errors.As(err, &perr) {
			fmt.Fprintln(stdErr(), perr.Error())
		} else {
			fmt.Fprintln(stdErr(), "ccwrap profile add:", err)
		}
		return 1
	}
	fmt.Fprintln(stdOut(), formatProfileLine(p, opts.Name == f.Default))
	return 0
}

// parseEditArgs parses `ccwrap profile edit <name> [flags]` with edit
// semantics (all flags optional; no auth-mode<->auth-key pairing at
// parse time — see profileEdit for the deferred check).
func parseEditArgs(args []string) (crudOpts, error) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return crudOpts{}, fmt.Errorf("missing profile <name> as first positional argument")
	}
	name := args[0]
	rest := args[1:]
	opts, err := parseCRUDFlags("profile edit", name, rest, false)
	if err != nil {
		return crudOpts{}, err
	}
	// edit with no flags is "nothing to do".
	editFields := []string{"provider", "base-url", "auth-mode", "auth-key", "auth-key-stdin", "auth-key-env", "egress-mode", "egress-url", "set-default", "model-alias", "upstream-header"}
	gotAny := false
	for _, k := range editFields {
		if opts.SetFields[k] {
			gotAny = true
			break
		}
	}
	if !gotAny {
		return crudOpts{}, fmt.Errorf("nothing to edit; pass at least one flag")
	}
	return opts, nil
}

func profileEdit(paths app.Paths, args []string) int {
	return profileEditIO(paths, os.Stdin, args)
}

func profileEditIO(paths app.Paths, stdin io.Reader, args []string) int {
	opts, err := parseEditArgs(args)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile edit:", err)
		return 2
	}
	unlock, err := profiles.Lock(paths.StateDir)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile edit:", err)
		return 1
	}
	defer unlock()
	path := profiles.DefaultPath(paths.StateDir)
	f, err := profiles.Load(path)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile edit:", err)
		return 1
	}
	if f == nil {
		fmt.Fprintf(stdErr(), "ccwrap profile edit: no profiles.json (use 'add' to create %q)\n", opts.Name)
		return 1
	}
	p, ok := f.Profiles[opts.Name]
	if !ok {
		fmt.Fprintf(stdErr(), "ccwrap profile edit: no such profile %q; use 'add' to create\n", opts.Name)
		return 1
	}
	if err := applyEditOpts(&p, opts, stdin); err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile edit:", err)
		return determineEditExitCode(err)
	}
	p.Name = opts.Name
	f.Profiles[opts.Name] = p
	if opts.SetFields["set-default"] && opts.SetDefault {
		f.Default = opts.Name
	}
	label := "profile edit " + opts.Name
	if err := profiles.OverwriteFile(path, f, label); err != nil {
		var perr *profiles.ParseErrors
		if errors.As(err, &perr) {
			fmt.Fprintln(stdErr(), perr.Error())
		} else {
			fmt.Fprintln(stdErr(), "ccwrap profile edit:", err)
		}
		return 1
	}
	fmt.Fprintln(stdOut(), formatProfileLine(p, opts.Name == f.Default))
	return 0
}

// determineEditExitCode maps applyEditOpts errors to exit code.
// Usage errors (mutex, mode pairing, removed mode values) → 2;
// everything else → 1.
func determineEditExitCode(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	if strings.Contains(msg, "incompatible with") ||
		strings.Contains(msg, "mutually exclusive") ||
		strings.Contains(msg, "no longer a mode value") {
		return 2
	}
	return 1
}

type rmOpts struct {
	Name  string
	Force bool
}

func parseRmArgs(args []string) (rmOpts, error) {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return rmOpts{}, fmt.Errorf("missing profile <name> as first positional argument")
	}
	name := args[0]
	if name != strings.TrimSpace(name) || name == "" {
		return rmOpts{}, fmt.Errorf("profile name must not be empty or contain leading/trailing whitespace")
	}
	rest := args[1:]
	fs := flag.NewFlagSet("profile rm", flag.ContinueOnError)
	fs.SetOutput(nullWriter{})
	force := fs.Bool("force", false, "Allow removing the file's default profile")
	if err := fs.Parse(rest); err != nil {
		return rmOpts{}, err
	}
	if extra := fs.Args(); len(extra) > 0 {
		return rmOpts{}, fmt.Errorf("unexpected extra args: %v", extra)
	}
	return rmOpts{Name: name, Force: *force}, nil
}

// sessionLooker enumerates session IDs currently using profileName.
// Production impl uses discovery.Scan + control.GetSession; tests
// pass a fake. Empty slice (or nil) means none found.
type sessionLooker func(ctx context.Context, paths app.Paths, profileName string) []string

// defaultSessionLooker mirrors the pattern in cmd/ccwrap/profile.go:
// discovery.Scan all reachable sessions, query each via control.GetSession,
// return IDs whose ActiveProfileName matches.
func defaultSessionLooker(ctx context.Context, paths app.Paths, profileName string) []string {
	discovered, err := discovery.Scan(paths)
	if err != nil {
		return nil
	}
	var hits []string
	for _, ds := range discovered {
		if !ds.Reachable {
			continue
		}
		client := control.NewClient(ds.Manifest.ControlSocket)
		subCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
		sess, err := client.GetSession(subCtx, ds.Manifest.SessionID)
		cancel()
		if err != nil || sess == nil {
			continue
		}
		if strings.TrimSpace(sess.ActiveProfileName) == profileName {
			hits = append(hits, ds.Manifest.SessionID)
		}
	}
	return hits
}

// profileRm is the production entry point; delegates to profileRmWithLooker.
func profileRm(paths app.Paths, args []string) int {
	return profileRmWithLooker(paths, args, defaultSessionLooker)
}

func profileRmWithLooker(paths app.Paths, args []string, looker sessionLooker) int {
	opts, err := parseRmArgs(args)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile rm:", err)
		return 2
	}
	path := profiles.DefaultPath(paths.StateDir)
	// Validation + the (possibly multi-second, networked) live-session scan run
	// WITHOUT the cross-process lock: reads are safe (OverwriteFile renames
	// atomically), and holding the lock across the scan would stall every other
	// ccwrap's profile mutation (the supervisor blocks on this flock while
	// holding profileFileMu). The authoritative delete+write re-loads under the
	// lock below.
	f, err := profiles.Load(path)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile rm:", err)
		return 1
	}
	if f == nil {
		fmt.Fprintf(stdErr(), "ccwrap profile rm: no profiles.json\n")
		return 1
	}
	if _, ok := f.Profiles[opts.Name]; !ok {
		fmt.Fprintf(stdErr(), "ccwrap profile rm: no such profile %q\n", opts.Name)
		return 1
	}
	if opts.Name == f.Default && !opts.Force {
		fmt.Fprintf(stdErr(), "ccwrap profile rm: refusing to remove the default profile.\n")
		fmt.Fprintf(stdErr(), "pass --force to remove anyway; the file's default will be reset to\n")
		fmt.Fprintf(stdErr(), "inherit-env, and the next ccwrap launch will resolve auth from the ambient\n")
		fmt.Fprintf(stdErr(), "ANTHROPIC_* env (or fail at preflight if no env credentials are set).\n")
		return 1
	}
	// Live-session warning (best-effort; warns both for false-positive
	// stale switches AND false-negative racing launches) — OUTSIDE the lock.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if active := looker(ctx, paths, opts.Name); len(active) > 0 {
		for _, sid := range active {
			fmt.Fprintf(stdErr(), "warning: session %s is currently active on profile %q.\n", sid, opts.Name)
			fmt.Fprintf(stdErr(), "The session will keep running on its in-memory snapshot until you\n")
			fmt.Fprintf(stdErr(), "switch or relaunch; new launches will use the next-resolution rule\n")
			fmt.Fprintf(stdErr(), "(file Default if set, else inherit-env).\n")
		}
	}
	// Authoritative delete under the cross-process lock; RE-LOAD because the
	// scan window may have let a peer mutate profiles.json.
	unlock, err := profiles.Lock(paths.StateDir)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile rm:", err)
		return 1
	}
	defer unlock()
	f, err = profiles.Load(path)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile rm:", err)
		return 1
	}
	if f == nil {
		fmt.Fprintf(stdErr(), "ccwrap profile rm: no profiles.json\n")
		return 1
	}
	if _, ok := f.Profiles[opts.Name]; !ok {
		// A peer removed it during the scan — nothing left to do.
		fmt.Fprintf(stdErr(), "ccwrap profile rm: no such profile %q\n", opts.Name)
		return 1
	}
	delete(f.Profiles, opts.Name)
	if opts.Name == f.Default {
		f.Default = profiles.InheritEnv
	}
	emptied := len(f.Profiles) == 0
	if err := profiles.OverwriteFile(path, f, "profile rm "+opts.Name); err != nil {
		var perr *profiles.ParseErrors
		if errors.As(err, &perr) {
			fmt.Fprintln(stdErr(), perr.Error())
		} else {
			fmt.Fprintln(stdErr(), "ccwrap profile rm:", err)
		}
		return 1
	}
	if emptied {
		fmt.Fprintf(stdOut(), "removed profile %q; profiles.json now empty, default=inherit-env\n", opts.Name)
	} else {
		fmt.Fprintf(stdOut(), "removed profile %q\n", opts.Name)
	}
	return 0
}

// profileSetDefault implements `ccwrap profile set-default <name|inherit-env>`.
// Exit codes:
//
//	0 — success
//	1 — operational failure (load, unknown profile)
//	2 — usage error (missing / extra args)
func profileSetDefault(paths app.Paths, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(stdErr(), "ccwrap profile set-default: missing <name|inherit-env> argument")
		return 2
	}
	if len(args) > 1 {
		fmt.Fprintln(stdErr(), "ccwrap profile set-default: unexpected extra args")
		return 2
	}
	arg := args[0]
	if arg != strings.TrimSpace(arg) || arg == "" {
		fmt.Fprintln(stdErr(), "ccwrap profile set-default: argument must not be empty or contain leading/trailing whitespace")
		return 2
	}
	unlock, err := profiles.Lock(paths.StateDir)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile set-default:", err)
		return 1
	}
	defer unlock()
	path := profiles.DefaultPath(paths.StateDir)
	f, err := profiles.Load(path)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile set-default:", err)
		return 1
	}
	if f == nil {
		fmt.Fprintf(stdErr(), "ccwrap profile set-default: no profiles.json\n")
		return 1
	}
	// Case-sensitive comparison.
	if arg == profiles.InheritEnv {
		f.Default = profiles.InheritEnv
	} else if _, ok := f.Profiles[arg]; ok {
		f.Default = arg
	} else {
		fmt.Fprintf(stdErr(), "ccwrap profile set-default: no such profile %q\n", arg)
		return 1
	}
	if err := profiles.OverwriteFile(path, f, "profile set-default"); err != nil {
		var perr *profiles.ParseErrors
		if errors.As(err, &perr) {
			fmt.Fprintln(stdErr(), perr.Error())
		} else {
			fmt.Fprintln(stdErr(), "ccwrap profile set-default:", err)
		}
		return 1
	}
	if arg == profiles.InheritEnv {
		fmt.Fprintln(stdOut(), "default = inherit-env (no profile)")
	} else {
		fmt.Fprintf(stdOut(), "default = %s\n", arg)
	}
	return 0
}

// applyEditOpts mutates p in place based on which crudOpts fields were
// explicitly set (via opts.SetFields populated by fs.Visit).
//
// Auth handling ordering:
//  1. Reject the removed "passthrough" mode value (V1: a no-auth profile
//     is an ABSENT auth block; edit cannot remove the block yet)
//  2. Mode set
//
// Then a separate AuthKeySource switch applies inline/env/stdin sources.
//
// Egress handling: any non-URL-bearing mode change clears Egress.URL.
// The parse-time guard in parseCRUDFlags rejects the combined
// "URL-less mode + --egress-url" case so it can't reach here.
// URL-bearing modes are: http, socks5, socks5h.
func applyEditOpts(p *profiles.Profile, opts crudOpts, stdin io.Reader) error {
	if opts.SetFields["provider"] {
		p.Provider = opts.Provider
	}
	if opts.SetFields["base-url"] {
		p.BaseURL = opts.BaseURL
	}
	// Auth handling ordering rule:
	//   1. Reject mode==passthrough outright — V1 removed it as a mode value
	//      (a no-auth profile is an ABSENT auth block, see
	//      profiles.validateAuth) and edit has no "remove the auth block"
	//      surface yet. Writing it through would only defer to a guaranteed
	//      write-time validation reject; the old auto-clear-then-write flow
	//      was that dead end.
	//   2. Otherwise: apply the new mode and/or new key as set.
	//
	// p.Auth may be nil (profile without an auth block). Mutations to
	// auth fields allocate one on demand; reading is guarded.
	if opts.SetFields["auth-mode"] {
		newMode := strings.ToLower(strings.TrimSpace(opts.AuthMode))
		if newMode == "passthrough" {
			return fmt.Errorf(`--auth-mode "passthrough" is no longer a mode value; a no-auth profile has NO auth block — remove "auth" from this profile by editing profiles.json directly (profile edit cannot remove it yet)`)
		}
		if p.Auth == nil {
			p.Auth = &profiles.AuthSpec{}
		}
		p.Auth.Mode = opts.AuthMode
	}
	if opts.AuthKeySource != authKeyNone {
		if p.Auth == nil {
			p.Auth = &profiles.AuthSpec{}
		}
		switch opts.AuthKeySource {
		case authKeyInline:
			p.Auth.Key = opts.AuthKeyInline
			p.Auth.KeyEnv = ""
		case authKeyEnv:
			p.Auth.Key = ""
			p.Auth.KeyEnv = opts.AuthKeyEnv
		case authKeyStdin:
			key, err := readStdinAuthKey(stdin)
			if err != nil {
				return err
			}
			p.Auth.Key = key
			p.Auth.KeyEnv = ""
		}
	}
	// Egress handling: any non-URL-bearing mode clears URL (defense
	// against stale URLs that may carry credentials).
	// URL-bearing modes: http, socks5, socks5h.
	if opts.SetFields["egress-mode"] {
		p.Egress.Mode = opts.EgressMode
		newMode := strings.ToLower(strings.TrimSpace(opts.EgressMode))
		if newMode != "http" && newMode != "socks5" && newMode != "socks5h" {
			p.Egress.URL = ""
		}
	}
	if opts.SetFields["egress-url"] {
		p.Egress.URL = opts.EgressURL
	}
	// Model aliases / upstream headers MERGE (upsert): the passed pairs are
	// added/updated; existing entries for other keys are kept. Remove an entry
	// by editing profiles.json directly.
	if opts.SetFields["model-alias"] {
		if p.ModelAliases == nil {
			p.ModelAliases = map[string]string{}
		}
		for k, v := range opts.ModelAliases {
			p.ModelAliases[k] = v
		}
	}
	if opts.SetFields["upstream-header"] {
		if p.UpstreamHeaders == nil {
			p.UpstreamHeaders = map[string]string{}
		}
		for k, v := range opts.UpstreamHeaders {
			p.UpstreamHeaders[k] = v
		}
	}
	return nil
}
