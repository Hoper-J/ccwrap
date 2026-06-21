package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
	"unicode/utf8"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/profiletest"
)

// profileTestEgressOpts is the CLI flag surface for `ccwrap profile
// test-egress`. Kept separate from profiletest.EgressProbeOptions so
// the CLI layer can hold extras (Format) without leaking into the lib.
type profileTestEgressOpts struct {
	Format  string
	Timeout time.Duration
	Target  string
}

// parseProfileTestEgressArgs parses positional + flags for `ccwrap
// profile test-egress`. Returns (positionalName, opts, err).
//
// Accepted shapes:
//
//	ccwrap profile test-egress
//	ccwrap profile test-egress [name]
//	ccwrap profile test-egress [name] [--flags...]
//	ccwrap profile test-egress [--flags...]
//	ccwrap profile test-egress --help   → errHelpRequested
func parseProfileTestEgressArgs(args []string) (string, profileTestEgressOpts, error) {
	for _, a := range args {
		if a == "-h" || a == "--help" {
			fmt.Fprint(stdOut(), profileTestEgressHelpText())
			return "", profileTestEgressOpts{}, errHelpRequested
		}
	}
	pos := ""
	flagArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		pos = args[0]
		flagArgs = args[1:]
	}

	fs := flag.NewFlagSet("profile test-egress", flag.ContinueOnError)
	fs.SetOutput(nullWriter{})
	timeout := fs.String("timeout", "5s", "Per-profile timeout (Go duration: e.g. 2s, 10s)")
	format := fs.String("format", "table", "Output format: table | json")
	target := fs.String("target", "", "Override probe target URL (defaults to $CCWRAP_EGRESS_TEST_URL or ipinfo.io/json)")

	if err := fs.Parse(flagArgs); err != nil {
		return "", profileTestEgressOpts{}, err
	}
	switch *format {
	case "table", "json":
	default:
		return "", profileTestEgressOpts{}, fmt.Errorf("invalid --format %q (must be table or json)", *format)
	}
	dur, err := time.ParseDuration(*timeout)
	if err != nil {
		return "", profileTestEgressOpts{}, fmt.Errorf("invalid --timeout %q: %v", *timeout, err)
	}
	if dur <= 0 {
		return "", profileTestEgressOpts{}, fmt.Errorf("--timeout must be positive; got %v", dur)
	}

	if rest := fs.Args(); len(rest) > 0 {
		return "", profileTestEgressOpts{}, fmt.Errorf("unexpected extra args: %v", rest)
	}
	return pos, profileTestEgressOpts{Format: *format, Timeout: dur, Target: *target}, nil
}

// selectEgressProbeTargets resolves the positional name into the
// concrete list of profiles to probe.
//   - name == "":            all named profiles in the file, sorted
//   - name == "inherit-env": ephemeral profile (Egress empty → direct)
//   - name == "<X>":         single profile X; error if not present
func selectEgressProbeTargets(f *profiles.File, name string) ([]profiles.Profile, error) {
	if name == "inherit-env" {
		return []profiles.Profile{{Name: "inherit-env"}}, nil
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
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out, nil
}

const egressErrColumnMax = 80

// renderEgressTestTable writes a tab-aligned table to w.
// Columns: PROFILE | STATUS | LATENCY | EGRESS_VIA | PUBLIC_IP | GEO | ORG | ERR
//
// Rules:
//   - LATENCY: "<n>ms" — always rendered. 0ms is a real value (sub-ms
//     probe or early-failure pre-network latency), not "missing".
//   - GEO: "City, Region, Country" with blanks skipped. "-" if all empty.
//   - ERR: truncated to egressErrColumnMax chars (full text in --format json).
func renderEgressTestTable(w io.Writer, results []profiletest.EgressProbeResult) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROFILE\tSTATUS\tLATENCY\tEGRESS_VIA\tPUBLIC_IP\tGEO\tORG\tERR")
	for _, r := range results {
		latency := fmt.Sprintf("%dms", r.LatencyMs)
		geo := formatEgressGeo(r.City, r.Region, r.Country)
		if geo == "" {
			geo = "-"
		}
		org := r.Org
		if org == "" {
			org = "-"
		}
		ip := r.PublicIP
		if ip == "" {
			ip = "-"
		}
		errCell := r.Err
		if errCell == "" {
			errCell = "-"
		}
		// Truncate by rune count, not byte count. A multi-byte UTF-8
		// codepoint straddling the byte cut would otherwise emit half
		// a codepoint and corrupt terminal output. Non-ASCII shows up
		// in net.OpError messages from localized proxies and any
		// upstream that emits non-English diagnostics.
		if utf8.RuneCountInString(errCell) > egressErrColumnMax {
			runes := []rune(errCell)
			errCell = string(runes[:egressErrColumnMax-1]) + "…"
		}
		egress := r.EgressVia
		if egress == "" {
			egress = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.Profile, r.Status, latency, egress, ip, geo, org, errCell)
	}
	_ = tw.Flush()
}

func formatEgressGeo(city, region, country string) string {
	parts := []string{}
	if city != "" {
		parts = append(parts, city)
	}
	if region != "" {
		parts = append(parts, region)
	}
	if country != "" {
		parts = append(parts, country)
	}
	return strings.Join(parts, ", ")
}

// egressTestJSONDoc is the top-level JSON document shape.
type egressTestJSONDoc struct {
	TestedAt string                          `json:"tested_at"`
	Results  []profiletest.EgressProbeResult `json:"results"`
}

func renderEgressTestJSON(w io.Writer, results []profiletest.EgressProbeResult) error {
	doc := egressTestJSONDoc{
		TestedAt: time.Now().Format(time.RFC3339),
		Results:  results,
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// maxEgressProbeWorkers caps concurrent probes when CLI fans out across
// many profiles. Tuned for the common ipinfo.io target — its free-tier
// rate-limit (1k req/day) and macOS default fd cap (256) both reward
// modest parallelism. A user with 50+ profiles previously fired all of
// them simultaneously; 429s and ephemeral-port pressure were the result.
const maxEgressProbeWorkers = 8

// runEgressProbes probes all targets through a worker pool capped at
// maxEgressProbeWorkers and returns results sorted by Profile name
// (case-insensitive). The pool guarantees we never have more than N
// outbound network connections in flight regardless of profile count.
func runEgressProbes(targets []profiles.Profile, opts profiletest.EgressProbeOptions) []profiletest.EgressProbeResult {
	results := make([]profiletest.EgressProbeResult, len(targets))
	if len(targets) == 0 {
		return results
	}
	workers := maxEgressProbeWorkers
	if workers > len(targets) {
		workers = len(targets)
	}
	jobs := make(chan int, len(targets))
	for i := range targets {
		jobs <- i
	}
	close(jobs)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				results[i] = profiletest.ProbeEgress(targets[i], opts)
			}
		}()
	}
	wg.Wait()
	sort.Slice(results, func(i, j int) bool {
		return strings.ToLower(results[i].Profile) < strings.ToLower(results[j].Profile)
	})
	return results
}

func exitCodeForEgressTestResults(results []profiletest.EgressProbeResult) int {
	for _, r := range results {
		if r.Status != profiletest.StatusOK {
			return 1
		}
	}
	return 0
}

// runProfileTestEgress is the entry point invoked from profile.go's
// `case "test-egress":` dispatch (wired in T8).
//
//	0 — all OK
//	1 — at least one non-OK
//	2 — usage error (already printed)
func runProfileTestEgress(paths app.Paths, args []string) int {
	pos, opts, err := parseProfileTestEgressArgs(args)
	if err == errHelpRequested {
		return 0
	}
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile test-egress:", err)
		return 2
	}
	file, err := profiles.Load(profiles.DefaultPath(paths.StateDir))
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile test-egress: load profiles.json:", err)
		return 2
	}
	targets, err := selectEgressProbeTargets(file, pos)
	if err != nil {
		fmt.Fprintln(stdErr(), "ccwrap profile test-egress:", err)
		return 2
	}
	results := runEgressProbes(targets, profiletest.EgressProbeOptions{
		Timeout: opts.Timeout,
		Target:  opts.Target,
	})
	switch opts.Format {
	case "json":
		_ = renderEgressTestJSON(stdOut(), results)
	default:
		renderEgressTestTable(stdOut(), results)
	}
	return exitCodeForEgressTestResults(results)
}

func profileTestEgressHelpText() string {
	return `Usage: ccwrap profile test-egress [name] [flags]

Probe each profile's egress connectivity:
  - With no [name], tests every named profile in profiles.json.
  - With [name], tests just that profile.
  - With "inherit-env", probes via the CLI process's own environment
    (HTTPS_PROXY / HTTP_PROXY / ALL_PROXY) — direct only when no proxy
    env is set. The CLI cannot see the running session's launcher
    --egress-proxy flag; for that, use the dashboard's <active-session>
    probe instead.

Each probe sends ONE HTTPS GET to https://ipinfo.io/json (or
$CCWRAP_EGRESS_TEST_URL if set; or --target if given), routed through
the profile's egress config. The probe sends NO Claude API traffic
and uses NO profile auth credentials.

The probe target sees the IP your egress is exiting from. By default
ccwrap uses ipinfo.io's free /json endpoint; set CCWRAP_EGRESS_TEST_URL
to a self-hosted endpoint to avoid third-party visibility.

Probe traffic does NOT appear in ccwrap inspect web /recent.

Flags:
  --timeout <dur>    Per-profile timeout (default 5s)
  --format <fmt>     "table" (default) or "json"
  --target <url>     Override probe target (default $CCWRAP_EGRESS_TEST_URL
                     or ipinfo.io/json)
`
}
