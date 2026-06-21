package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/discovery"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/profiles"
)

// runProfileSubcommand dispatches `ccwrap profile {ls,status,switch}`.
// `ls` enumerates profiles.json (host-only URLs, alias count,
// stripped egress summary, active marked); `status` prints the active
// profile identity (or `inherit-env`); `switch <name>` invokes the
// SwitchProfile control op and renders the sanitized outcome.
//
// Return value is an exit code (0 success, 1 operational error, 2 usage
// error). Secret-safety invariant: stdout AND stderr must never contain URL
// userinfo, the active profile's auth.key value, its auth.key_env name or
// resolved value, or any upstream-header value.
func runProfileSubcommand(paths app.Paths, args []string) int {
	if len(args) < 1 {
		fmt.Fprintln(os.Stderr, "usage: ccwrap profile {ls,status,switch <name>,test [name],test-egress [name],add,edit,rm,set-default} [--session ID]")
		return 2
	}
	subcmd := args[0]
	rest := args[1:]
	// CRUD verbs (test/add/edit/rm/set-default) own their own flag parsing
	// and do NOT accept --session; dispatch them BEFORE the shared
	// --session strip so their flags aren't mangled.
	switch subcmd {
	case "test":
		return runProfileTest(paths, rest)
	case "test-egress":
		return runProfileTestEgress(paths, rest)
	case "add":
		return profileAdd(paths, rest)
	case "edit":
		return profileEdit(paths, rest)
	case "rm":
		return profileRm(paths, rest)
	case "set-default":
		return profileSetDefault(paths, rest)
	}
	// Lightweight inline flag parsing: callers may pass --session ID for
	// status/switch. ls ignores --session (it is a file-only read).
	sessionID := ""
	positional := make([]string, 0, len(rest))
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--session":
			if i+1 >= len(rest) {
				fmt.Fprintln(os.Stderr, "ccwrap profile: --session requires a value")
				return 2
			}
			i++
			sessionID = rest[i]
		case strings.HasPrefix(a, "--session="):
			sessionID = strings.TrimPrefix(a, "--session=")
		default:
			positional = append(positional, a)
		}
	}
	switch subcmd {
	case "ls":
		return profileLs(paths, sessionID)
	case "status":
		return profileStatus(paths, sessionID)
	case "switch":
		if len(positional) < 1 {
			fmt.Fprintln(os.Stderr, "usage: ccwrap profile switch <name> [--session ID]")
			return 2
		}
		return profileSwitch(paths, sessionID, positional[0])
	default:
		fmt.Fprintf(os.Stderr, "unknown profile subcommand: %q\nusage: ccwrap profile {ls,status,switch <name>,test [name],test-egress [name],add,edit,rm,set-default} [--session ID]\n", subcmd)
		return 2
	}
}

// profileLs enumerates profiles.json entries. The on-disk file
// is the source of truth — when a sessionID is provided AND reachable,
// the active profile is marked with a leading "*"; otherwise we mark the
// persisted file Default (the next ccwrap launch would pick that profile).
// Missing profiles.json prints a friendly "no profiles.json present"
// message and exits 0 — that is the zero-touch path, not an error.
func profileLs(paths app.Paths, sessionID string) int {
	path := profiles.DefaultPath(paths.StateDir)
	file, err := profiles.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap profile ls: %v\n", err)
		return 1
	}
	if file == nil || len(file.Profiles) == 0 {
		fmt.Fprintln(stdOut(), "no profiles.json present (zero-touch / inherit-env)")
		return 0
	}
	activeName := strings.TrimSpace(file.Default)
	if activeName == profiles.InheritEnv {
		activeName = ""
	}
	// If a live session is reachable, prefer its active profile name —
	// that is the truly-active profile (the user may have switched away
	// from the persisted default mid-session). Best-effort: lookup
	// failure falls back to file.Default so `ls` always succeeds.
	if sessionID != "" {
		if name, ok := lookupActiveProfileName(paths, sessionID); ok {
			activeName = name
		}
	} else if ds, ok := singleReachableSession(paths); ok {
		if name, found := lookupActiveProfileNameVia(ds); found {
			activeName = name
		}
	}
	// Stable order: by provider then by name, grouping rows by provider.
	names := make([]string, 0, len(file.Profiles))
	for n := range file.Profiles {
		names = append(names, n)
	}
	sort.SliceStable(names, func(i, j int) bool {
		pi, pj := file.Profiles[names[i]], file.Profiles[names[j]]
		if pi.Provider != pj.Provider {
			return strings.ToLower(pi.Provider) < strings.ToLower(pj.Provider)
		}
		return names[i] < names[j]
	})
	for _, n := range names {
		p := file.Profiles[n]
		fmt.Fprintln(stdOut(), formatProfileLine(p, n == activeName))
	}
	return 0
}

// formatProfileLine renders one `ls` row. Secret-safe: BaseURL is
// reduced to host (userinfo dropped by url.URL.Hostname), egress summary
// goes through stripEgressSummary which re-strips userinfo, the auth
// block and upstream-header values are NEVER referenced. Profile.Name
// and Provider are non-secret identity.
func formatProfileLine(p profiles.Profile, active bool) string {
	marker := " "
	if active {
		marker = "*"
	}
	provider := strings.TrimSpace(p.Provider)
	if provider == "" {
		provider = "-"
	}
	host := profileHostNoUserinfo(p.BaseURL)
	if host == "" {
		host = "-"
	}
	aliasN := len(p.ModelAliases)
	aliasWord := "aliases"
	if aliasN == 1 {
		aliasWord = "alias"
	}
	egress := stripEgressSummary(&p)
	return fmt.Sprintf("%s %s  %s  %s  %d %s  %s", marker, p.Name, provider, host, aliasN, aliasWord, egress)
}

// profileHostNoUserinfo extracts the host (no userinfo) from a base_url
// string. Returns "" for empty input; returns the raw trimmed string when
// it does not parse as a URL with a host (mirrors profiles.BaseURLHost
// EXCEPT that this variant defensively scrubs any userinfo from the
// fallback string — so a malformed URL like "user@noscheme" still does
// not leak credentials to stdout).
func profileHostNoUserinfo(s string) string {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Hostname()
	}
	// Fallback: defensively strip everything before "@" (userinfo) and
	// trim a trailing path/query/fragment. This is belt-and-braces — a
	// parse-failed string should not be a valid URL on disk, but the
	// secret-safety invariant says NO leaks, including from malformed inputs.
	if at := strings.IndexByte(raw, '@'); at >= 0 {
		raw = raw[at+1:]
	}
	for _, sep := range []byte{'/', '?', '#'} {
		if i := strings.IndexByte(raw, sep); i >= 0 {
			raw = raw[:i]
		}
	}
	return raw
}

// stripEgressSummary returns the profile's egress summary userinfo-free.
// profiles.Profile.EgressSummary already returns "inherit" / "direct" /
// "http <host:port>" via url.URL.Host (userinfo-free by construction).
// Defensively re-run url.Parse on any "http <token>" to strip userinfo
// from the token in case the source ever changes.
func stripEgressSummary(p *profiles.Profile) string {
	if p == nil {
		return ""
	}
	s := p.EgressSummary()
	// Re-strip any userinfo that may have leaked through the token.
	if strings.HasPrefix(s, "http ") {
		host := strings.TrimSpace(strings.TrimPrefix(s, "http "))
		host = profileHostNoUserinfo(host)
		return "http " + host
	}
	// "inherit" / "direct" / single-token fall-throughs are userinfo-free
	// by the EgressSummary contract; still run through url.Parse to
	// catch any future drift.
	if u, err := url.Parse(s); err == nil && u.User != nil {
		u.User = nil
		return u.String()
	}
	return s
}

// profileStatus prints the active profile identity for the targeted
// session: the active profile's name + provider when a profile is active,
// or `inherit-env` when none. The Session display fields are already
// userinfo-stripped at publish — we only surface ActiveProfileName/Provider,
// which are by definition non-secret (profile identity, not credentials).
func profileStatus(paths app.Paths, sessionID string) int {
	ds, err := resolveSessionForProfileCmd(paths, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap profile status: %v\n", err)
		return 1
	}
	client := control.NewClient(ds.Manifest.ControlSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sess, err := client.GetSession(ctx, ds.Manifest.SessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap profile status: %v\n", err)
		return 1
	}
	name := strings.TrimSpace(sess.ActiveProfileName)
	if name == "" {
		fmt.Fprintln(os.Stdout, "active profile: inherit-env")
		return 0
	}
	provider := strings.TrimSpace(sess.ActiveProfileProvider)
	if provider != "" {
		fmt.Fprintf(os.Stdout, "active profile: %s (%s)\n", name, provider)
	} else {
		fmt.Fprintf(os.Stdout, "active profile: %s\n", name)
	}
	return 0
}

// profileSwitch invokes the SwitchProfile control op against the
// targeted session and renders the structured outcome:
//   - switched               → exit 0, print new identity to stdout
//   - refused_needs_relaunch → exit 1, pinned advice to stderr
//   - rejected_invalid       → exit 1, sanitized reason to stderr
//   - no_such_session / no_profiles_file / unknown_profile → exit 1
//
// The supervisor pre-strips secrets from out.Message + out.View before
// serializing, so we surface them verbatim — never log the raw switch
// error (there is none across the boundary by design).
func profileSwitch(paths app.Paths, sessionID, name string) int {
	ds, err := resolveSessionForProfileCmd(paths, sessionID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap profile switch: %v\n", err)
		return 1
	}
	client := control.NewClient(ds.Manifest.ControlSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := client.SwitchProfile(ctx, ds.Manifest.SessionID, name)
	if err != nil {
		// Transport-level error (socket unreachable, malformed JSON).
		// The switch outcome is structured at 200; a transport error
		// is operational, not a switch-side outcome — print it
		// verbatim (it carries no credential by construction).
		fmt.Fprintf(os.Stderr, "ccwrap profile switch: %v\n", err)
		return 1
	}
	switch out.Result {
	case "switched":
		// Render the new identity. The View is already SafeProfileView
		// (userinfo-stripped). We re-decode only the non-secret identity
		// fields here — name + provider_label — to avoid pulling in
		// preflight.ProfileView as a CLI dep.
		id := decodeViewIdentity(out.View)
		if id != "" {
			fmt.Fprintf(os.Stdout, "switched to profile %q (%s)\n", name, id)
		} else {
			fmt.Fprintf(os.Stdout, "switched to profile %q\n", name)
		}
		return 0
	case "refused_needs_relaunch":
		fmt.Fprintf(os.Stderr, "ccwrap profile switch refused: %s\n", out.Message)
		return 1
	case "rejected_invalid":
		fmt.Fprintf(os.Stderr, "ccwrap profile switch rejected: %s\n", out.Message)
		return 1
	case "no_such_session":
		fmt.Fprintf(os.Stderr, "ccwrap profile switch: %s\n", out.Message)
		return 1
	case "no_profiles_file":
		fmt.Fprintf(os.Stderr, "ccwrap profile switch: %s\n", out.Message)
		return 1
	case "unknown_profile":
		fmt.Fprintf(os.Stderr, "ccwrap profile switch: %s\n", out.Message)
		return 1
	default:
		fmt.Fprintf(os.Stderr, "ccwrap profile switch: unexpected outcome %q\n", out.Result)
		return 1
	}
}

// decodeViewIdentity decodes the non-secret identity (provider_label or
// name) from a SwitchOutcomeView.View JSON. Returns "" when the view is
// empty or undecodable — the caller falls back to printing just the
// requested name. NEVER decodes URLs or anything else from the view —
// kept narrow to avoid resurrecting any leak surface.
func decodeViewIdentity(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	// Minimal struct mirror — we only want provider_label or name.
	var v struct {
		Name          string `json:"name"`
		ProviderLabel string `json:"provider_label"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	if strings.TrimSpace(v.ProviderLabel) != "" {
		return v.ProviderLabel
	}
	return strings.TrimSpace(v.Name)
}

// resolveSessionForProfileCmd finds the session to target for status/switch.
// When sessionID is explicit, look it up; when empty AND exactly one
// reachable session exists, use that; otherwise return a helpful error.
func resolveSessionForProfileCmd(paths app.Paths, sessionID string) (*model.DiscoveredSession, error) {
	if strings.TrimSpace(sessionID) != "" {
		ds, err := discovery.Find(paths, sessionID)
		if err != nil {
			return nil, err
		}
		if ds == nil || !ds.Reachable {
			return nil, fmt.Errorf("session %s not reachable", sessionID)
		}
		return ds, nil
	}
	discovered, err := discovery.Scan(paths)
	if err != nil {
		return nil, err
	}
	var reachable []model.DiscoveredSession
	for _, ds := range discovered {
		if ds.Reachable {
			reachable = append(reachable, ds)
		}
	}
	switch len(reachable) {
	case 0:
		return nil, fmt.Errorf("no reachable ccwrap session; launch one or pass --session ID")
	case 1:
		ds := reachable[0]
		return &ds, nil
	default:
		ids := make([]string, 0, len(reachable))
		for _, ds := range reachable {
			ids = append(ids, ds.Manifest.SessionID)
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("multiple reachable sessions (%s); pass --session ID", strings.Join(ids, ", "))
	}
}

// singleReachableSession returns the lone reachable session for `ls`
// active-marker resolution. Best-effort: any failure returns (nil, false)
// so `ls` still renders the file's persisted default.
func singleReachableSession(paths app.Paths) (*model.DiscoveredSession, bool) {
	discovered, err := discovery.Scan(paths)
	if err != nil {
		return nil, false
	}
	var found *model.DiscoveredSession
	for i := range discovered {
		if !discovered[i].Reachable {
			continue
		}
		if found != nil {
			return nil, false // multiple reachable — ambiguous
		}
		found = &discovered[i]
	}
	if found == nil {
		return nil, false
	}
	return found, true
}

// lookupActiveProfileName / lookupActiveProfileNameVia are best-effort
// helpers for `ls`. A failed lookup falls back to file.Default — `ls`
// must never fail because the active marker couldn't be resolved.
func lookupActiveProfileName(paths app.Paths, sessionID string) (string, bool) {
	ds, err := discovery.Find(paths, sessionID)
	if err != nil || ds == nil || !ds.Reachable {
		return "", false
	}
	return lookupActiveProfileNameVia(ds)
}

func lookupActiveProfileNameVia(ds *model.DiscoveredSession) (string, bool) {
	if ds == nil {
		return "", false
	}
	client := control.NewClient(ds.Manifest.ControlSocket)
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	sess, err := client.GetSession(ctx, ds.Manifest.SessionID)
	if err != nil || sess == nil {
		return "", false
	}
	return strings.TrimSpace(sess.ActiveProfileName), true
}
