package dashboard

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/discovery"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

type Options struct {
	SessionID string
	View      string
	Interval  time.Duration
}

const dashboardDisplayInterval = time.Second

type snapshot struct {
	activeCount        int
	staleCount         int
	hasCurrent         bool
	current            model.DiscoveredSession
	session            *model.Session
	requests           []model.RequestRecord
	errors             []model.ErrorRecord
	trace              []model.TraceRecord
	controlUnavailable bool
	refreshError       string
	otherIDs           []string
}

type snapshotResult struct {
	snap snapshot
	err  error
}

func Run(ctx context.Context, paths app.Paths, opts Options) error {
	view := normalizeView(opts.View)
	if opts.Interval <= 0 {
		opts.Interval = 700 * time.Millisecond
	}
	if opts.Interval < 250*time.Millisecond {
		opts.Interval = 250 * time.Millisecond
	}
	pal := ui.New(ui.IsTerminal(os.Stdout))
	width := termWidth()
	r := newRenderer(os.Stdout, width)
	defer r.Close()

	// Single-key view switching. restoreTTY is deferred
	// HERE (the main goroutine), not inside the reader goroutine —
	// the reader blocks in os.Stdin.Read and its defer would never
	// run on q/ctx exit, leaving the terminal raw.
	keyCh := make(chan rune, 8)
	keyStop := make(chan struct{})
	restoreTTY, _ := startKeyReader(os.Stdin, keyCh, keyStop)
	defer restoreTTY()
	defer close(keyStop)

	reqCh := make(chan struct{}, 1)
	resultCh := make(chan snapshotResult, 1)
	var workerWG sync.WaitGroup
	workerWG.Add(1)
	go func() {
		defer workerWG.Done()
		for range reqCh {
			s, err := collectSnapshot(paths, opts.SessionID)
			select {
			case <-resultCh:
			default:
			}
			resultCh <- snapshotResult{snap: s, err: err}
		}
	}()
	defer func() {
		close(reqCh)
		workerWG.Wait()
	}()

	reqCh <- struct{}{}
	initial := <-resultCh
	snap := initial.snap
	if initial.err != nil {
		snap = snapshot{controlUnavailable: true, refreshError: initial.err.Error()}
	}

	resizeCh := make(chan struct{}, 1)
	stopResize := make(chan struct{})
	go watchTerminalResize(resizeCh, stopResize)
	defer close(stopResize)

	dirty := true
	var lastDraw time.Time
	dataTicker := time.NewTicker(opts.Interval)
	defer dataTicker.Stop()
	displayTicker := time.NewTicker(dashboardDisplayInterval)
	defer displayTicker.Stop()
	for {
		if dirty {
			if nw := termWidth(); nw != width {
				width = nw
				r.resize(width)
			}
			r.Draw(buildFrameFromSnapshot(paths.SessionsDir(), snap, pal, view, opts.Interval, dashboardDisplayInterval, width))
			dirty = false
			lastDraw = time.Now()
		}
		select {
		case <-ctx.Done():
			return nil
		case k := <-keyCh:
			switch k {
			case '1':
				view, dirty = "overview", true
			case '2':
				view, dirty = "requests", true
			case '3', 'e':
				view, dirty = "errors", true
			case '4':
				view, dirty = "diagnostics", true
			case 'r':
				select {
				case reqCh <- struct{}{}:
				default:
				}
			case 'g':
				if snap.staleCount > 0 {
					go func() { _, _ = discovery.Cleanup(paths) }()
					select {
					case reqCh <- struct{}{}:
					default:
					}
				}
			case 'q', 3, 27: // q, Ctrl+C/ETX, ESC
				return nil
			}
		case <-dataTicker.C:
			select {
			case reqCh <- struct{}{}:
			default:
			}
		case res := <-resultCh:
			if res.err != nil {
				snap.controlUnavailable = true
				snap.refreshError = res.err.Error()
			} else {
				snap = res.snap
			}
			dirty = true
		case <-displayTicker.C:
			if !lastDraw.IsZero() && time.Since(lastDraw) < 100*time.Millisecond {
				continue
			}
			dirty = true
		case <-resizeCh:
			dirty = true
		}
	}
}

type renderer struct {
	out   io.Writer
	prev  []string
	alt   bool
	width int
}

func newRenderer(out io.Writer, width int) *renderer {
	return newRendererWithMode(out, width, detectAlt(out))
}

func newRendererWithMode(out io.Writer, width int, alt bool) *renderer {
	r := &renderer{out: out, width: width, alt: alt}
	if r.alt {
		fmt.Fprint(out, "\033[?1049h\033[?25l\033[H\033[2J")
	}
	return r
}

func detectAlt(out io.Writer) bool {
	f, ok := out.(*os.File)
	return ok && ui.IsTerminal(f)
}

func (r *renderer) Close() {
	if r.alt {
		fmt.Fprint(r.out, "\033[?25h\033[?1049l")
	}
}

func (r *renderer) resize(width int) {
	r.width = width
	r.prev = nil
	if r.alt {
		fmt.Fprint(r.out, "\033[H\033[2J")
	}
}

// clampANSIWidth truncates s to at most w visible columns, preserving ANSI SGR
// escape sequences (which have zero display width) and appending a reset when
// the row carried styling so a truncated colored row never bleeds its color
// forward. Used by the alt-screen differ below.
func clampANSIWidth(s string, w int) string {
	if w <= 0 {
		return ""
	}
	var b strings.Builder
	visible := 0
	styled := false
	for i := 0; i < len(s); {
		if s[i] == 0x1b { // ESC: copy the whole CSI sequence at zero width
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
					j++
				}
				if j < len(s) {
					j++
				}
			}
			b.WriteString(s[i:j])
			styled = true
			i = j
			continue
		}
		if visible >= w {
			break
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		b.WriteString(s[i : i+size])
		i += size
		visible++
	}
	out := b.String()
	if styled && !strings.HasSuffix(out, "\033[0m") {
		out += "\033[0m"
	}
	return out
}

func (r *renderer) Draw(lines []string) {
	// The alt-screen diff addresses rows by logical index (\033[N;1H), so a row
	// wider than the terminal would wrap and desync that one-line-per-screen-row
	// assumption — misaligned partial redraws on terminals narrower than the
	// row content. Clamp every line to the terminal width first (ANSI-aware).
	// Non-alt (piped) output keeps full-width lines.
	if r.alt && r.width > 0 {
		clamped := make([]string, len(lines))
		for i, l := range lines {
			clamped[i] = clampANSIWidth(l, r.width)
		}
		lines = clamped
	}
	if len(lines) == 0 {
		lines = []string{""}
	}
	firstChanged := -1
	for i, line := range lines {
		if i >= len(r.prev) || r.prev[i] != line {
			firstChanged = i
			break
		}
	}
	if firstChanged == -1 && len(lines) == len(r.prev) {
		return
	}
	if firstChanged == -1 {
		firstChanged = len(lines)
	}
	if !r.alt {
		var b strings.Builder
		for i, line := range lines {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(line)
		}
		b.WriteString("\n\n")
		_, _ = io.WriteString(r.out, b.String())
		r.prev = append(r.prev[:0], lines...)
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\033[%d;1H\033[J", firstChanged+1)
	for i := firstChanged; i < len(lines); i++ {
		if i > firstChanged {
			b.WriteString("\r\n")
		}
		b.WriteString(lines[i])
	}
	fmt.Fprint(&b, "\033[1;1H")
	_, _ = io.WriteString(r.out, b.String())
	r.prev = append(r.prev[:0], lines...)
}

func collectSnapshot(paths app.Paths, sessionFilter string) (snapshot, error) {
	discovered, err := discovery.Scan(paths)
	if err != nil {
		return snapshot{}, err
	}
	active := make([]model.DiscoveredSession, 0, len(discovered))
	stale := 0
	for _, ds := range discovered {
		if ds.Stale || !ds.Reachable {
			stale++
			continue
		}
		active = append(active, ds)
	}
	current := chooseDiscovered(active, sessionFilter)
	snap := snapshot{activeCount: len(active), staleCount: stale}
	if current != nil {
		for _, ds := range active {
			if ds.Manifest.SessionID == current.Manifest.SessionID {
				continue
			}
			snap.otherIDs = append(snap.otherIDs, ds.Manifest.SessionID)
		}
	}
	if current == nil {
		return snap, nil
	}
	snap.hasCurrent = true
	snap.current = *current

	client := control.NewClient(current.Manifest.ControlSocket)
	sess, err := getSessionWithTimeout(client, current.Manifest.SessionID, 3*time.Second)
	if err != nil || sess == nil {
		snap.controlUnavailable = true
		if err != nil {
			snap.refreshError = err.Error()
		}
		return snap, nil
	}
	snap.session = sess
	var errs []string
	if reqs, reqErr := getRequestsWithTimeout(client, sess.ID, 1500*time.Millisecond); reqErr != nil {
		errs = append(errs, "requests: "+reqErr.Error())
	} else {
		snap.requests = reqs
	}
	if errRows, errErr := getErrorsWithTimeout(client, sess.ID, 1500*time.Millisecond); errErr != nil {
		errs = append(errs, "errors: "+errErr.Error())
	} else {
		snap.errors = errRows
	}
	if trace, traceErr := getTraceWithTimeout(client, sess.ID, 1500*time.Millisecond); traceErr != nil {
		errs = append(errs, "trace: "+traceErr.Error())
	} else {
		snap.trace = trace
	}
	if len(errs) == 1 {
		snap.refreshError = errs[0]
	} else if len(errs) > 1 {
		snap.refreshError = fmt.Sprintf("%d refreshes failed: %s", len(errs), strings.Join(errs, "; "))
	}
	return snap, nil
}

func getSessionWithTimeout(client *control.Client, sessionID string, timeout time.Duration) (*model.Session, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.GetSession(ctx, sessionID)
}

func getRequestsWithTimeout(client *control.Client, sessionID string, timeout time.Duration) ([]model.RequestRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.Requests(ctx, sessionID)
}

func getErrorsWithTimeout(client *control.Client, sessionID string, timeout time.Duration) ([]model.ErrorRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.Errors(ctx, sessionID)
}

func getTraceWithTimeout(client *control.Client, sessionID string, timeout time.Duration) ([]model.TraceRecord, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.Trace(ctx, sessionID)
}

func buildFrameFromSnapshot(runtimeDir string, snap snapshot, pal ui.Palette, view string, dataInterval, displayInterval time.Duration, width int) []string {
	if width < 40 {
		width = 40
	}
	dividerWidth := width
	if dividerWidth > 92 {
		dividerWidth = 92
	}
	_ = runtimeDir // runtime path no longer in the always-on header
	lines := []string{
		pal.Bold(fmt.Sprintf("ccwrap · %d active · %d stale", snap.activeCount, snap.staleCount)) + "  " + renderTabs(pal, view),
	}
	if len(snap.otherIDs) > 0 {
		// Copy-only, non-interactive; switch focus via
		// `ccwrap dashboard --session <ID>`. 8-char IDs.
		lines = append(lines, pal.LabelValue("other", formatOtherIDs(snap.otherIDs, 4)))
	}
	footer := "[1-4] view · [r] refresh · [q] quit"
	if len(snap.errors) > 0 {
		footer += " · [e] focus errors"
	}
	if snap.staleCount > 0 {
		footer += " · [g] gc stale"
	}
	if !snap.hasCurrent {
		lines = append(lines,
			ui.Divider("─", dividerWidth),
			"No live sessions.",
			pal.Dim("Start Claude with `ccwrap` to populate this dashboard."),
		)
		return lines
	}
	if snap.controlUnavailable || snap.session == nil {
		lines = append(lines,
			pal.Cyan(ui.ShortID(snap.current.Manifest.SessionID)),
			ui.Divider("─", dividerWidth),
			pal.LabelValue("upstream", fallbackText(snap.current.Manifest.ExactUpstreamBase, "<unconfigured>")),
			pal.LabelValue("egress", ui.HumanEgress(snap.current.Manifest.EgressMode, snap.current.Manifest.EgressSource, snap.current.Manifest.EgressSummary)),
			pal.Dim(controlUnavailableMessage(snap.refreshError)),
		)
		return lines
	}

	lines = append(lines, renderSessionHeader(pal, snap.session, ui.LatestClaudeSessionID(snap.requests))...)
	if p := ui.SessionPosture(snap.session, lastError(snap.errors)); p != "" {
		lines = append(lines, "", "  "+p)
	}
	lines = append(lines, ui.Divider("─", dividerWidth))
	if snap.refreshError != "" {
		lines = append(lines, pal.Dim("refresh warning: "+trimDisplay(snap.refreshError, width-18)))
	}
	lines = append(lines, renderLiveStrip(pal, dataInterval, displayInterval, snap.requests, snap.errors, snap.trace)...)
	lines = append(lines, "")
	// The hero block + summary fields stay identical across
	// ALL views (no reflow on switch); only the body below changes.
	lines = append(lines, renderSummaryLines(pal, snap.session)...)
	lines = append(lines, "")

	switch view {
	case "requests":
		lines = append(lines, renderRequestLines(pal, snap.requests, 20)...)
	case "errors":
		lines = append(lines, renderErrorLines(pal, snap.errors, 18)...)
	case "diagnostics":
		lines = append(lines, renderDiagnosticsLines(pal, snap.trace)...)
	default:
		lines = append(lines, renderOverviewLines(pal, snap.session, snap.requests, snap.errors, snap.trace)...)
	}

	lines = append(lines, "", pal.Dim(footer))
	return lines
}

// lastError returns the newest error for the degraded hero "Last:"
// tail (slice is oldest-first, matching the row renderers).
func lastError(e []model.ErrorRecord) *model.ErrorRecord {
	if len(e) == 0 {
		return nil
	}
	return &e[len(e)-1]
}

func formatOtherIDs(ids []string, max int) string {
	if len(ids) == 0 {
		return ""
	}
	short := make([]string, len(ids)) // 8-char IDs
	for i, id := range ids {
		short[i] = ui.ShortID(id)
	}
	if len(short) <= max {
		return strings.Join(short, " ")
	}
	return strings.Join(short[:max], " ") + fmt.Sprintf(" +%d more", len(short)-max)
}

func chooseDiscovered(sessions []model.DiscoveredSession, id string) *model.DiscoveredSession {
	if id != "" {
		for i := range sessions {
			if sessions[i].Manifest.SessionID == id {
				return &sessions[i]
			}
		}
	}
	if len(sessions) == 0 {
		return nil
	}
	return &sessions[0]
}

func controlUnavailableMessage(refreshErr string) string {
	if strings.TrimSpace(refreshErr) == "" {
		return "control socket reachable state is stale or unavailable"
	}
	return "control refresh failed; showing last known control-state context: " + refreshErr
}

func renderTabs(pal ui.Palette, active string) string {
	views := []string{"overview", "requests", "errors", "diagnostics"}
	parts := make([]string, 0, len(views))
	for i, v := range views {
		label := fmt.Sprintf("[%d] %s", i+1, strings.ToUpper(v[:1])+v[1:])
		if v == active {
			parts = append(parts, pal.Cyan(label))
		} else {
			parts = append(parts, pal.Dim(label))
		}
	}
	return strings.Join(parts, "  ")
}

// healthLabel maps Health to the cross-surface status vocabulary the web
// hero uses (Active/Degraded/Error/Ended), lowercased per the machine-state
// doctrine — a user switching between TUI and web reads the same words.
func healthLabel(sess *model.Session) string {
	if sess.State == model.StateEnded {
		return "ended"
	}
	switch sess.Health {
	case model.HealthError:
		return "error"
	case model.HealthWarn:
		return "degraded"
	default: // ok or empty
		return "active"
	}
}

func renderSessionHeader(pal ui.Palette, sess *model.Session, claudeID string) []string {
	if sess == nil {
		return nil
	}
	line := pal.Cyan(ui.ShortID(sess.ID)) + "  " + pal.Status(healthLabel(sess))
	if claudeID != "" {
		// The Claude conversation id (what --continue/--resume address) —
		// the web brandbar leads with this same id, so both surfaces speak
		// the same session-identity vocabulary. The ccwrap id stays first:
		// it is what --session switching needs.
		line += "   " + pal.LabelValue("claude session", ui.ShortID(claudeID))
	}
	if sess.Name != "" {
		line += "   " + pal.LabelValue("name", trimDisplay(sess.Name, 36))
	}
	if sess.ClaudePID > 0 {
		meta := fmt.Sprintf("claude pid %d", sess.ClaudePID)
		if up := uptimeLabel(sess.CreatedAt); up != "" {
			// "up 37m", NOT "37m ago" — CreatedAt is an uptime anchor; the
			// ago-suffix read like a last-seen timestamp (web says "up" too).
			meta += " · up " + up
		}
		line += "   " + pal.Dim(meta)
	}
	return []string{line}
}

// uptimeLabel formats a duration since ts as "Xs/Xm/Xh/Xd" without the
// "ago" suffix — for "up 5m" display in the session header. Zero time →
// empty (caller skips the segment). Mirrors the web heroMeta vocabulary.
func uptimeLabel(ts time.Time) string {
	if ts.IsZero() {
		return ""
	}
	age := time.Since(ts)
	switch {
	case age < time.Minute:
		return fmt.Sprintf("%ds", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd", int(age.Hours()/24))
	}
}

// renderLiveStrip uses poll vocabulary only. The TUI is
// poll-based via control.Client, not SSE; "stream"/"connected" words
// belong solely to the Web surface.
func renderLiveStrip(pal ui.Palette, dataInterval, displayInterval time.Duration, reqs []model.RequestRecord, errs []model.ErrorRecord, trace []model.TraceRecord) []string {
	last := latestActivityTimestamp(reqs, errs, trace)
	total := len(reqs) + len(errs) + len(trace)
	return []string{
		pal.LabelValue("updated", fmt.Sprintf("%d events · last %s", total, activityAgeLabel(last))) +
			"   " + pal.Dim(fmt.Sprintf("poll %s · display %s", formatInterval(dataInterval), formatInterval(displayInterval))),
	}
}

// renderOverviewLines is the OVERVIEW VIEW BODY only — the
// hero + summary fields render once in buildFrameFromSnapshot before
// the view switch, so they are NOT repeated here.
func renderOverviewLines(pal ui.Palette, sess *model.Session, reqs []model.RequestRecord, errs []model.ErrorRecord, trace []model.TraceRecord) []string {
	lines := renderActivityLines(pal, reqs, errs, trace, 5)
	lines = append(lines, "")
	lines = append(lines, renderRequestLines(pal, reqs, 8)...)
	if len(errs) > 0 {
		lines = append(lines, "")
		lines = append(lines, renderErrorLines(pal, errs, 6)...)
	}
	lines = append(lines, "")
	lines = append(lines, renderDiagnosticsSummary(pal, trace)...)
	return lines
}

// renderSummaryLines renders per-field rows (no "Summary" title),
// humanized labels consistent with CLI status. Rendered before
// every view body.
func renderSummaryLines(pal ui.Palette, sess *model.Session) []string {
	if sess == nil {
		return nil
	}
	bootstrap := ui.HumanAuthBootstrap(sess.AuthBootstrap, sess.AuthBootstrapKind)
	if sess.AuthBootstrap == model.AuthBootstrapMissing {
		// Same severity + diagnosis as the web's amber "⚠ MISSING" cell:
		// warn tone (Health classifies auth-missing as warn) and the exact
		// env var to set — not a bare lowercase "missing" lost in the line.
		m := "⚠ missing"
		if sess.MissingAuthEnv != "" {
			m += " — needs $" + sess.MissingAuthEnv
		}
		bootstrap = pal.Yellow(m)
	}
	auth := ui.HumanAuth(sess.AuthMode, sess.AuthSource) + " · " + ui.HumanAuthPolicy(sess.AuthPolicy) + " · " + bootstrap
	models := "0 aliases"
	switch {
	case sess.ModelAliasCount == 1:
		models = "1 alias"
	case sess.ModelAliasCount > 1:
		models = fmt.Sprintf("%d aliases", sess.ModelAliasCount)
	}
	lines := []string{
		"  " + pal.Dim("proxy") + "     " + fallbackText("http://"+sess.ProxyListenAddr, "<unbound>"),
		"  " + pal.Dim("route") + "     " + ui.HumanRouteClass(sess.RouteClass) + " · " + strings.ToLower(ui.HumanRouteSource(sess.RouteSource)),
		"  " + pal.Dim("auth") + "      " + auth,
		"  " + pal.Dim("models") + "    " + models,
	}
	if eg := ui.HumanEgress(sess.EgressMode, sess.EgressSource, sess.EgressSummary); eg != "Direct" {
		lines = append(lines, "  "+pal.Dim("egress")+"    "+eg)
	}
	profile := "inherit-env"
	if sess.ActiveProfileName != "" {
		profile = sess.ActiveProfileName
		if sess.ActiveProfileProvider != "" {
			profile += " · " + sess.ActiveProfileProvider
		}
	}
	lines = append(lines, "  "+pal.Dim("profile")+"   "+profile)
	// Native TLS — the web keeps a dedicated cell for this (doctrine);
	// blocked means requests are being fail-closed, which must be readable
	// here, not only inferable from the error list.
	if sess.NativeTLS != "" {
		v, d, st := ui.NativeTLSPresentation(sess.NativeTLS, sess.NativeTLSFallbacks, sess.NativeTLSLoaded)
		switch st {
		case "native-blocked":
			v = pal.Red(v)
		case "native-active":
			v = pal.Green(v)
		default:
			v = pal.Dim(v)
		}
		lines = append(lines, "  "+pal.Dim("tls")+"       "+v+" · "+d)
	}
	// Capture — the UNMASKED state is a persistent danger marker on the
	// web (so a forgotten CCWRAP_UNMASK_CREDENTIALS stays visible); a
	// TUI-only user needs the same standing warning.
	if sess.CaptureBodies || sess.CaptureTelemetry {
		v, d, st := ui.BodiesPresentation(sess.CaptureBodies, sess.CaptureBodiesUnmasked, sess.CaptureTelemetry)
		val := v + " · " + d
		if st == "bodies-unmasked" {
			val = pal.Red(val)
		}
		lines = append(lines, "  "+pal.Dim("capture")+"   "+val)
	}
	lines = append(lines, "  "+pal.Dim("traffic")+"   "+fmt.Sprintf("%d req · %d err", sess.RecentRequestCount, sess.RecentErrorCount))
	return lines
}

func renderActivityLines(pal ui.Palette, reqs []model.RequestRecord, errs []model.ErrorRecord, trace []model.TraceRecord, n int) []string {
	lines := []string{pal.Bold("Recent activity")}
	rows := renderActivityRows(pal, reqs, errs, trace, n)
	if len(rows) == 0 {
		return append(lines, pal.Dim("No recent activity yet."))
	}
	return append(lines, rows...)
}

func renderRequestLines(pal ui.Palette, reqs []model.RequestRecord, n int) []string {
	lines := []string{pal.Bold("Requests")}
	lines = append(lines, renderRequestRows(pal, reqs, n)...)
	return lines
}

func renderErrorLines(pal ui.Palette, errs []model.ErrorRecord, n int) []string {
	lines := []string{pal.Bold("Errors")}
	lines = append(lines, renderErrorRows(pal, errs, n)...)
	return lines
}

func renderDiagnosticsSummary(pal ui.Palette, trace []model.TraceRecord) []string {
	lines := []string{pal.Bold("Network diagnostics")}
	traceCount := len(trace)
	if traceCount == 0 {
		return append(lines, pal.Dim("No network trace yet."))
	}
	if traceCount == 1 {
		lines = append(lines, "1 network trace")
	} else {
		lines = append(lines, fmt.Sprintf("%d network traces", traceCount))
	}
	lines = append(lines, pal.Dim("Use --view diagnostics for route, TLS, and upstream details."))
	return lines
}

func renderDiagnosticsLines(pal ui.Palette, trace []model.TraceRecord) []string {
	lines := []string{pal.Bold("Network diagnostics")}
	lines = append(lines, renderTraceBlock(pal, trace, 12)...)
	return lines
}

func renderTraceBlock(pal ui.Palette, trace []model.TraceRecord, n int) []string {
	lines := []string{pal.Magenta("Network trace")}
	lines = append(lines, renderTraceRows(trace, n)...)
	return lines
}

func renderRequestRows(pal ui.Palette, reqs []model.RequestRecord, n int) []string {
	reqs = tailRequests(reqs, n)
	if len(reqs) == 0 {
		return []string{"No requests yet."}
	}
	lines := make([]string, 0, len(reqs))
	for i := len(reqs) - 1; i >= 0; i-- {
		rec := reqs[i]
		// TUI uses the short "SYNTH GET" label (NOT Web's
		// full "SYNTHETIC"); synthetic rows dim so they recede.
		// Status tier mirrors the web Details cell: 4xx amber, 5xx rose.
		// Pad BEFORE colorizing (ANSI codes would count into %-width), and
		// keep synthetic rows plain so the whole-line Dim wrap stays clean.
		statusStr := fmt.Sprintf("%3d", rec.StatusCode)
		if !rec.Synthetic {
			if rec.StatusCode >= 500 && rec.StatusCode < 600 {
				statusStr = pal.Red(statusStr)
			} else if rec.StatusCode >= 400 && rec.StatusCode < 500 {
				statusStr = pal.Yellow(statusStr)
			}
		}
		line := fmt.Sprintf("%-8s %-9s %-44s %s  %6d ms  %-10s  %s",
			rec.Timestamp.Format("15:04:05"),
			trimDisplay(ui.ShortMethodLabel(rec), 9),
			trimDisplay(rec.Path, 44),
			statusStr,
			rec.LatencyMS,
			trimDisplay(string(rec.StreamState), 10),
			trimDisplay(rec.ActualUpstreamHost, 24),
		)
		if rec.Synthetic {
			line = pal.Dim(line)
		}
		lines = append(lines, line)
	}
	return lines
}

func renderErrorRows(pal ui.Palette, errs []model.ErrorRecord, n int) []string {
	errRows := tailErrors(errs, n)
	if len(errRows) == 0 {
		return []string{"No errors."}
	}
	lines := make([]string, 0, len(errRows))
	for i := len(errRows) - 1; i >= 0; i-- {
		rec := errRows[i]
		// The suggested action follows the
		// summary on the same line, trimmed to width.
		summary := rec.Summary
		if rec.SuggestedAction != "" {
			summary = strings.TrimSpace(summary) + " · " + rec.SuggestedAction
		}
		// Pad before colorizing — ANSI escapes count into %-20s width.
		cls := pal.Red(fmt.Sprintf("%-20s", trimDisplay(rec.ErrorClass, 20)))
		lines = append(lines, fmt.Sprintf("%-8s %s %-24s %s",
			rec.Timestamp.Format("15:04:05"),
			cls,
			trimDisplay(rec.UpstreamHost, 24),
			trimDisplay(summary, 86),
		))
	}
	return lines
}

func renderTraceRows(trace []model.TraceRecord, n int) []string {
	trace = tailTrace(trace, n)
	if len(trace) == 0 {
		return []string{"No network trace entries."}
	}
	lines := make([]string, 0, len(trace))
	for i := len(trace) - 1; i >= 0; i-- {
		rec := trace[i]
		detail := strings.TrimSpace(rec.Summary + "  " + rec.Detail)
		// profile_switch detail is wire JSON — render the shared human
		// sentence instead (ui.HumanProfileSwitch, same wording web-side).
		if rec.Category == "profile_switch" {
			if h := ui.HumanProfileSwitch(rec.Detail); h != "" {
				detail = h
			}
		}
		lines = append(lines, fmt.Sprintf("%-8s %-12s %s",
			rec.Timestamp.Format("15:04:05"),
			trimDisplay(rec.Category, 12),
			trimDisplay(detail, 108),
		))
	}
	return lines
}

type activityRow struct {
	when time.Time
	line string
}

func renderActivityRows(pal ui.Palette, reqs []model.RequestRecord, errs []model.ErrorRecord, trace []model.TraceRecord, n int) []string {
	rows := make([]activityRow, 0, len(reqs)+len(errs)+len(trace))
	for _, rec := range reqs {
		// Short "SYNTH GET" label; synthetic rows dim.
		line := fmt.Sprintf("%-8s %-8s %-44s %s",
			rec.Timestamp.Format("15:04:05"),
			"request",
			trimDisplay(strings.TrimSpace(ui.ShortMethodLabel(rec)+" "+fallbackText(rec.Path, "/")), 44),
			trimDisplay(fmt.Sprintf("%d · %d ms · %s · %s", rec.StatusCode, rec.LatencyMS, rec.StreamState, fallbackText(rec.ActualUpstreamHost, rec.LogicalTargetHost)), 68),
		)
		if rec.Synthetic {
			line = pal.Dim(line)
		}
		rows = append(rows, activityRow{when: rec.Timestamp, line: line})
	}
	for _, rec := range errs {
		right := fallbackText(rec.UpstreamHost, rec.SuggestedAction)
		rows = append(rows, activityRow{
			when: rec.Timestamp,
			line: fmt.Sprintf("%-8s %s %-44s %s",
				rec.Timestamp.Format("15:04:05"),
				pal.Red(fmt.Sprintf("%-8s", "error")),
				trimDisplay(rec.Summary, 44),
				trimDisplay(joinParts(rec.ErrorClass, right), 68),
			),
		})
	}
	for _, rec := range trace {
		detail := fallbackText(rec.Detail, rec.Category)
		// profile_switch detail is wire JSON — render the shared human
		// sentence instead (same wording as the web rows).
		if rec.Category == "profile_switch" {
			if h := ui.HumanProfileSwitch(rec.Detail); h != "" {
				detail = h
			}
		}
		rows = append(rows, activityRow{
			when: rec.Timestamp,
			line: fmt.Sprintf("%-8s %s %-44s %s",
				rec.Timestamp.Format("15:04:05"),
				pal.Magenta(fmt.Sprintf("%-8s", "trace")),
				trimDisplay(rec.Summary, 44),
				trimDisplay(detail, 68),
			),
		})
	}
	if len(rows) == 0 {
		return nil
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].when.After(rows[j].when) })
	if n > 0 && len(rows) > n {
		rows = rows[:n]
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, row.line)
	}
	return lines
}

func tailRequests(in []model.RequestRecord, n int) []model.RequestRecord {
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func tailErrors(in []model.ErrorRecord, n int) []model.ErrorRecord {
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func tailTrace(in []model.TraceRecord, n int) []model.TraceRecord {
	if len(in) <= n {
		return in
	}
	return in[len(in)-n:]
}

func trimDisplay(s string, n int) string {
	if n <= 0 || s == "" {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n == 1 {
		for _, r := range s {
			return string(r)
		}
		return ""
	}
	runes := []rune(s)
	return string(runes[:n-1]) + "…"
}

func fallbackText(v, d string) string {
	if strings.TrimSpace(v) == "" {
		return d
	}
	return v
}

func joinParts(parts ...string) string {
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" || part == "—" {
			continue
		}
		out = append(out, part)
	}
	return strings.Join(out, " · ")
}

func latestActivityTimestamp(reqs []model.RequestRecord, errs []model.ErrorRecord, trace []model.TraceRecord) time.Time {
	var latest time.Time
	for _, rec := range reqs {
		if rec.Timestamp.After(latest) {
			latest = rec.Timestamp
		}
	}
	for _, rec := range errs {
		if rec.Timestamp.After(latest) {
			latest = rec.Timestamp
		}
	}
	for _, rec := range trace {
		if rec.Timestamp.After(latest) {
			latest = rec.Timestamp
		}
	}
	return latest
}

func activityAgeLabel(ts time.Time) string {
	if ts.IsZero() {
		return "idle"
	}
	age := time.Since(ts)
	switch {
	case age < 5*time.Second:
		return "just now"
	case age < time.Minute:
		return fmt.Sprintf("%ds ago", int(age.Seconds()))
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(age.Hours()/24))
	}
}

func formatInterval(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return d.String()
}

func normalizeView(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	switch v {
	case "requests", "errors", "diagnostics":
		return v
	case "trace":
		return "diagnostics"
	default:
		return "overview"
	}
}
