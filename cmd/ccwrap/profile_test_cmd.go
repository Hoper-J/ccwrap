package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/profiletest"
)

// profileTestCmdOpts mirrors the CLI flag surface. Kept separate from
// profiletest.ProbeOptions so the CLI layer can hold extras (Format)
// without leaking into the probe library.
type profileTestCmdOpts struct {
	Format  string
	Timeout time.Duration
	Model   string
}

// errHelpRequested is a sentinel returned by parseProfileTestArgs
// when the user passed --help / -h. The caller (runProfileTest) maps
// it to exit code 0 after the help text has been printed to stdout.
var errHelpRequested = fmt.Errorf("help requested")

// parseProfileTestArgs parses positional + flags for `ccwrap profile
// test`. Returns (positionalName, opts, err). On usage error, returns
// (-,-,err) and main wraps it with the exit-2 code.
//
// Accepted shapes (Go's stdlib flag stops at the first non-flag arg,
// so we pre-extract a leading positional name before delegating):
//
//	ccwrap profile test
//	ccwrap profile test [name]
//	ccwrap profile test [name] [--flags...]
//	ccwrap profile test [--flags...]            (no name → all profiles)
//
// As a special case, `--help` / `-h` anywhere in args prints the
// long-form help to stdout and returns errHelpRequested so the
// caller can return exit 0 (not the usage-error exit 2).
func parseProfileTestArgs(args []string) (string, profileTestCmdOpts, error) {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprint(stdOut(), profileTestHelpText())
			return "", profileTestCmdOpts{}, errHelpRequested
		}
	}
	pos := ""
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos = args[0]
		flagArgs = args[1:]
	}

	fs := flag.NewFlagSet("profile test", flag.ContinueOnError)
	fs.SetOutput(nullWriter{})
	model := fs.String("model", "", "Force the probe model (bypasses alias rewrite)")
	timeout := fs.String("timeout", "15s", "Per-profile timeout (Go duration: e.g. 5s, 30s, 1m)")
	format := fs.String("format", "table", "Output format: table | json")

	if err := fs.Parse(flagArgs); err != nil {
		return "", profileTestCmdOpts{}, err
	}
	switch *format {
	case "table", "json":
	default:
		return "", profileTestCmdOpts{}, fmt.Errorf("invalid --format %q (must be table or json)", *format)
	}
	dur, err := time.ParseDuration(*timeout)
	if err != nil {
		return "", profileTestCmdOpts{}, fmt.Errorf("invalid --timeout %q: %v", *timeout, err)
	}
	if dur <= 0 {
		return "", profileTestCmdOpts{}, fmt.Errorf("--timeout must be positive; got %v", dur)
	}

	if rest := fs.Args(); len(rest) > 0 {
		return "", profileTestCmdOpts{}, fmt.Errorf("unexpected extra args: %v", rest)
	}
	return pos, profileTestCmdOpts{Format: *format, Timeout: dur, Model: *model}, nil
}

// nullWriter swallows flag.FlagSet's auto-printing of usage on parse
// error so cmd/ccwrap owns error formatting.
type nullWriter struct{}

func (nullWriter) Write(p []byte) (int, error) { return len(p), nil }

// Forward-reference to satisfy `var _ profiletest.ProbeOptions` style
// usage in later tasks (avoids unused import).
var _ = profiletest.ProbeOptions{}

// selectProfileTestTargets resolves the positional name into the
// concrete list of profiles to probe.
//   - name == "": all named profiles in the file, sorted by Name.
//   - name == "inherit-env": handled by selectInheritEnvTarget.
//   - name == "<X>": single profile X; error if not present.
func selectProfileTestTargets(f *profiles.File, name string) ([]profiles.Profile, error) {
	if name == "inherit-env" {
		// Defer to the inherit-env-specific path.
		return selectInheritEnvTarget()
	}
	if f == nil || len(f.Profiles) == 0 {
		if name == "" {
			return nil, fmt.Errorf("no profiles.json found and no positional name provided")
		}
		return nil, fmt.Errorf("no such profile %q (no profiles.json found)", name)
	}
	if name != "" {
		p, ok := f.Profiles[name]
		if !ok {
			return nil, fmt.Errorf("no such profile %q", name)
		}
		p.Name = name
		return []profiles.Profile{p}, nil
	}
	out := make([]profiles.Profile, 0, len(f.Profiles))
	for k, p := range f.Profiles {
		p.Name = k
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, nil
}

const defaultInheritBaseURL = "https://api.anthropic.com"

// selectInheritEnvTarget builds an ephemeral profile from the current
// shell's ANTHROPIC_* env. Precedence matches seedAuth:
// ANTHROPIC_API_KEY wins over ANTHROPIC_AUTH_TOKEN.
func selectInheritEnvTarget() ([]profiles.Profile, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	authTok := os.Getenv("ANTHROPIC_AUTH_TOKEN")
	if apiKey == "" && authTok == "" {
		return nil, fmt.Errorf("inherit-env requested but no ANTHROPIC_API_KEY or ANTHROPIC_AUTH_TOKEN in env")
	}
	baseURL := os.Getenv("ANTHROPIC_BASE_URL")
	if baseURL == "" {
		baseURL = defaultInheritBaseURL
	}
	auth := &profiles.AuthSpec{}
	if apiKey != "" {
		auth.Mode = "ccwrap_x_api_key"
		auth.Key = apiKey
	} else {
		auth.Mode = "ccwrap_bearer"
		auth.Key = authTok
	}
	return []profiles.Profile{{
		Name:    "inherit-env",
		BaseURL: baseURL,
		Auth:    auth,
	}}, nil
}

// renderProfileTestTable writes a human-aligned table to w.
// Columns: PROFILE, STATUS, LATENCY, MODEL_SENT, MODEL_ECHOED, ERR.
// Rendering rules:
//   - LATENCY: "<n>ms" for measured (>0); "timeout" if Status==Timeout;
//     "-" otherwise.
//   - MODEL_SENT: "" for SKIPPED; otherwise the sent value; if
//     ModelSentRewroteFrom is set, formatted as "<from> → <sent>".
//   - MODEL_ECHOED: empty cell rendered as "-".
//   - ERR: Err string; "-" if empty.
func renderProfileTestTable(w io.Writer, results []profiletest.ProbeResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROFILE\tSTATUS\tLATENCY\tMODEL_SENT\tMODEL_ECHOED\tERR")
	for _, r := range results {
		latency := "-"
		switch {
		case r.Status == profiletest.StatusTimeout:
			latency = "timeout"
		case r.Latency > 0:
			latency = fmt.Sprintf("%dms", r.Latency.Milliseconds())
		}
		modelSent := r.ModelSent
		if modelSent != "" && r.ModelSentRewroteFrom != "" {
			modelSent = r.ModelSentRewroteFrom + " → " + r.ModelSent
		}
		if modelSent == "" {
			modelSent = "-"
		}
		echoed := r.ModelEchoed
		if echoed == "" {
			echoed = "-"
		}
		errCell := r.Err
		if errCell == "" {
			errCell = r.SkippedReason
		}
		if errCell == "" {
			errCell = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Profile, r.Status, latency, modelSent, echoed, errCell)
	}
	_ = tw.Flush()
}

// profileTestJSONResult is the per-row schema for `--format json`.
// All possibly-empty fields are *T so absent values serialize as
// JSON null (not "" or 0), so consumers can distinguish "not
// reported" from "reported empty". Secrets are deliberately absent
// from the schema: no auth.key, no key_env_var name, no userinfo.
type profileTestJSONResult struct {
	Profile              string  `json:"profile"`
	Status               string  `json:"status"`
	LatencyMs            *int64  `json:"latency_ms"`
	HTTPStatus           *int    `json:"http_status"`
	BaseURLHost          string  `json:"base_url_host"`
	ModelSent            *string `json:"model_sent"`
	ModelSentRewrittenTo *string `json:"model_sent_rewritten_to"`
	ModelEchoed          *string `json:"model_echoed"`
	Error                *string `json:"error"`
	SkippedReason        *string `json:"skipped_reason"`
}

// profileTestJSONDoc is the top-level document for `--format json`.
type profileTestJSONDoc struct {
	TestedAt string                  `json:"tested_at"`
	Results  []profileTestJSONResult `json:"results"`
}

// renderProfileTestJSON emits the JSON schema for profile-test
// results. Fields that are zero in the ProbeResult are encoded as JSON
// null. Latency is omitted for SKIPPED rows (no probe happened).
func renderProfileTestJSON(w io.Writer, results []profiletest.ProbeResult) error {
	doc := profileTestJSONDoc{
		TestedAt: time.Now().Format(time.RFC3339),
		Results:  make([]profileTestJSONResult, 0, len(results)),
	}
	for _, r := range results {
		row := profileTestJSONResult{
			Profile:     r.Profile,
			Status:      r.Status.String(),
			BaseURLHost: r.BaseURLHost,
		}
		if r.Latency > 0 && r.Status != profiletest.StatusSkipped {
			ms := r.Latency.Milliseconds()
			row.LatencyMs = &ms
		}
		if r.HTTPStatus != 0 {
			code := r.HTTPStatus
			row.HTTPStatus = &code
		}
		if r.ModelSent != "" {
			s := r.ModelSent
			row.ModelSent = &s
		}
		if r.ModelSentRewroteFrom != "" {
			// When an alias rewrite happened, expose both sides:
			//   model_sent              = the user-facing alias input
			//   model_sent_rewritten_to = the literal value transmitted
			rs := r.ModelSentRewroteFrom
			row.ModelSent = &rs
			s := r.ModelSent
			row.ModelSentRewrittenTo = &s
		}
		if r.ModelEchoed != "" {
			s := r.ModelEchoed
			row.ModelEchoed = &s
		}
		if r.Err != "" {
			s := r.Err
			row.Error = &s
		}
		if r.SkippedReason != "" {
			s := r.SkippedReason
			row.SkippedReason = &s
		}
		doc.Results = append(doc.Results, row)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// runProfileTestProbes probes all targets concurrently (no cap) and
// returns results sorted by Profile name (case-insensitive). Order
// of completion does not affect the output order.
func runProfileTestProbes(targets []profiles.Profile, opts profiletest.ProbeOptions) []profiletest.ProbeResult {
	results := make([]profiletest.ProbeResult, len(targets))
	var wg sync.WaitGroup
	for i, p := range targets {
		wg.Add(1)
		go func(i int, p profiles.Profile) {
			defer wg.Done()
			results[i] = profiletest.Probe(p, opts)
		}(i, p)
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool {
		return strings.ToLower(results[i].Profile) < strings.ToLower(results[j].Profile)
	})
	return results
}

// exitCodeForProfileTestResults returns 1 if any result is a
// non-SKIPPED failure; 0 otherwise. (Usage errors are exit 2 and
// short-circuit before this is called.)
func exitCodeForProfileTestResults(results []profiletest.ProbeResult) int {
	for _, r := range results {
		if r.Status.IsFailure() {
			return 1
		}
	}
	return 0
}

// runProfileTest is the entry point invoked by profile.go's
// `case "test":` dispatch. Returns the desired process exit code:
//
//	0 — all OK or SKIPPED
//	1 — at least one non-SKIPPED failure
//	2 — usage error (already printed to stderr by this function)
//
// Path resolution mirrors profileLs in cmd/ccwrap/profile.go:
// `profiles.Load(profiles.DefaultPath(paths.StateDir))`. The Load
// function returns (nil, nil) when profiles.json doesn't exist, and
// selectProfileTestTargets handles that gracefully (returns
// "no profiles.json found..." for an empty positional, or
// "no such profile <X> (no profiles.json found)" for an explicit
// name). The inherit-env target path bypasses the file entirely.
func runProfileTest(paths app.Paths, args []string) int {
	pos, opts, err := parseProfileTestArgs(args)
	if err == errHelpRequested {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile test:", err)
		return 2
	}
	file, err := profiles.Load(profiles.DefaultPath(paths.StateDir))
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile test: load profiles.json:", err)
		return 2
	}
	targets, err := selectProfileTestTargets(file, pos)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile test:", err)
		return 2
	}
	results := runProfileTestProbes(targets, profiletest.ProbeOptions{
		Timeout: opts.Timeout,
		Model:   opts.Model,
	})
	switch opts.Format {
	case "json":
		_ = renderProfileTestJSON(stdOut(), results)
	default:
		renderProfileTestTable(stdOut(), results)
	}
	return exitCodeForProfileTestResults(results)
}

// stdOut/stdErr are package-level vars so tests can capture. Default
// to os.Stdout/os.Stderr at runtime.
var (
	stdOut = func() io.Writer { return osStdout }
	stdErr = func() io.Writer { return osStderr }
)

var (
	osStdout io.Writer = os.Stdout
	osStderr io.Writer = os.Stderr
)

// profileTestHelpText returns the long-form help text. It must mention
// cost, no-auth-SKIPPED, and the "not in /recent" guarantee.
func profileTestHelpText() string {
	return `Usage: ccwrap profile test [name] [flags]

Probe one or all profiles end-to-end:
  - With no [name], tests every named profile in profiles.json.
  - With [name], tests just that profile.
  - With "inherit-env", builds an ephemeral profile from the current
    shell's ANTHROPIC_* env and tests it.

Each probe sends one POST /v1/messages with max_tokens=1. Cost is
~1-2 tokens at the configured provider's per-token rate.

Profiles without an auth block are reported as SKIPPED (ccwrap does
not own the credential, so the probe has nothing to inject; legacy
auth.mode = passthrough data gets the same disposition).

Probe traffic does not appear in ccwrap inspect web /recent — it is
sent directly in-process, bypassing the proxy port.

Flags:
  --model <name>     Force the probe model (bypasses alias rewrite)
  --timeout <dur>    Per-profile total timeout (default 15s)
  --format <fmt>     "table" (default) or "json"
`
}
