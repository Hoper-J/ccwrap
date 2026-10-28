package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/certs"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/egress"
	"github.com/Hoper-J/ccwrap/internal/manifest"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/procmeta"
	"github.com/Hoper-J/ccwrap/internal/projects"
	"github.com/Hoper-J/ccwrap/internal/settings"
	"github.com/Hoper-J/ccwrap/internal/supervisor"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// cleanupFn wraps a teardown action with sync.Once so it can be safely
// invoked twice — once explicitly by the success path and once by the
// deferred rollback. The second call becomes a no-op.
type cleanupFn struct {
	once sync.Once
	fn   func()
}

func (c *cleanupFn) call() {
	if c == nil {
		return
	}
	c.once.Do(c.fn)
}

// rollbackStack holds cleanups in push order; run executes them LIFO.
// Each cleanupFn is sync.Once-protected so calling run after a partially
// completed success path is harmless.
type rollbackStack struct {
	fns []*cleanupFn
}

func (r *rollbackStack) push(fn func()) *cleanupFn {
	c := &cleanupFn{fn: fn}
	r.fns = append(r.fns, c)
	return c
}

func (r *rollbackStack) run() {
	for i := len(r.fns) - 1; i >= 0; i-- {
		c := r.fns[i]
		func() {
			defer func() { _ = recover() }()
			c.call()
		}()
	}
}

// sessionLauncher orchestrates the per-launch lifecycle: preflight has
// already produced a result; the launcher prepares paths and CA,
// brings up a supervisor goroutine, registers the session, configures
// route/auth/alias state, builds child env + settings file, spawns
// Claude, attaches it to the supervisor, and finally waits.
//
// Each step that opens a resource pushes a teardown onto rollback.
// The deferred rollback.run() at the top of runClaude makes both
// success and error paths converge on identical cleanup ordering.
type sessionLauncher struct {
	// warnedModels are the unaliased models the launch banner already named;
	// the exit notice does not repeat them.
	warnedModels []string
	paths        app.Paths
	sessionPaths app.Paths
	sessionID    string
	pre          *preflight.Result
	launch       launchArgs
	cwd          string

	// Secret-bearing launch inputs the supervisor retains
	// in-process via LaunchContext. inspect is settings.InspectLaunch's
	// on-disk settings snapshot; opts carries the four *FileContent
	// snapshots (file-backed launch inputs frozen at composeLaunch time).
	// Never serialized; never crosses the control socket; GC'd on
	// supervisor exit.
	inspect *settings.InspectionResult
	opts    preflight.Options

	ctx       context.Context
	cancelCtx context.CancelFunc

	ca            *certs.Manager
	supervisor    *supervisor.Supervisor
	supervisorErr chan error
	client        *control.Client
	session       *model.Session
	cmd           *exec.Cmd
	stopSignals   func()
	proxyURL      string
	// appLink is the session in ccwrap ui, when the app was up in time for
	// the banner; without it the banner says to run ccwrap open.
	appLink          string
	settingsFilePath string // inspection copy of the generated settings
	settingsArg      string // what Claude Code receives as --settings
	childEnv         []string
	bin              string
	childStdout      io.Writer // nil -> os.Stdout; capture sets this to redirect the child's stdout off our stdout

	closeReason string

	rollback rollbackStack
}

// workingDir is the current folder with symlinks resolved, the form Claude
// Code records folders in (projects.Canonical), so the session's project
// folder and ccwrap's per-folder memory match Claude Code's; "" when unknown.
func workingDir() string {
	cwd, err := os.Getwd()
	if err != nil || cwd == "" {
		return ""
	}
	return projects.Canonical(cwd)
}

func newSessionLauncher(paths app.Paths, launch launchArgs, pre *preflight.Result, inspect *settings.InspectionResult, opts preflight.Options, cwd, sessionID string) *sessionLauncher {
	ctx, cancel := context.WithCancel(context.Background())
	return &sessionLauncher{
		paths:       paths,
		sessionID:   sessionID,
		pre:         pre,
		inspect:     inspect,
		opts:        opts,
		launch:      launch,
		cwd:         cwd,
		ctx:         ctx,
		cancelCtx:   cancel,
		closeReason: "launcher rollback",
	}
}

// PreparePaths ensures runtime + session directories exist and the
// local CA is materialized. RemoveAll on the per-session runtime dir
// is the broadest cleanup (covers manifest, settings file, control
// socket, log).
func (l *sessionLauncher) PreparePaths() error {
	if err := app.EnsurePaths(l.paths); err != nil {
		return err
	}
	l.sessionPaths = l.paths.SessionPaths(l.sessionID)
	// Surface the macOS unix-socket length limit here, in the main goroutine,
	// before StartSupervisor — otherwise the supervisor's own guard fires inside
	// its goroutine and AwaitControl just times out with a generic "did not
	// become ready", hiding the actionable cause.
	if runtime.GOOS == "darwin" && len(l.sessionPaths.SocketPath) >= 104 {
		return fmt.Errorf("control socket path is too long for macOS (%d ≥ 104 bytes): %q\n"+
			"  ccwrap puts the session socket under $TMPDIR — set a shorter TMPDIR (e.g. TMPDIR=/tmp) and relaunch",
			len(l.sessionPaths.SocketPath), l.sessionPaths.SocketPath)
	}
	if err := app.EnsurePaths(l.sessionPaths); err != nil {
		return err
	}
	runtimeDir := l.sessionPaths.RuntimeDir
	l.rollback.push(func() { _ = os.RemoveAll(runtimeDir) })
	l.ca = certs.NewManager(l.sessionPaths)
	if err := l.ca.EnsureCA(); err != nil {
		return fmt.Errorf("ensure CA: %w", err)
	}
	return nil
}

// StartSupervisor spawns the per-session supervisor goroutine. The
// rollback cancels the launcher context (causing Run to return) and
// then issues an explicit Shutdown so connections drain inside a
// bounded window before draining supervisorErr.
//
// The LaunchContext built here carries the launch's
// secret-bearing in-process snapshots (settings inspection +
// preflight options including the four *FileContent fields +
// preflight result) into the supervisor. It is package-private to
// the supervisor; never serialized; never crosses the control
// socket; GC'd on supervisor exit.
func (l *sessionLauncher) StartSupervisor() error {
	lc := &supervisor.LaunchContext{
		Version:          versionString(),
		Options:          l.opts,
		Inspection:       l.inspect,
		Launch:           l.pre,
		CaptureBodies:    l.launch.CaptureBodies,
		CaptureTelemetry: l.launch.CaptureTelemetry,
		NativeTLS:        l.launch.NativeTLS,
		NativeTLSHello:   l.launch.NativeTLSHello,
	}
	sv, err := supervisor.New(l.sessionPaths, 0, lc)
	if err != nil {
		return err
	}
	l.supervisor = sv
	l.supervisorErr = make(chan error, 1)
	go func() { l.supervisorErr <- sv.Run(l.ctx) }()
	l.rollback.push(func() {
		l.cancelCtx()
		shutCtx, sCancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = sv.Shutdown(shutCtx)
		sCancel()
		select {
		case <-time.After(250 * time.Millisecond):
		case <-l.supervisorErr:
		}
	})
	return nil
}

// AwaitControl polls the session control socket until the supervisor
// is ready to accept RPCs.
func (l *sessionLauncher) AwaitControl() error {
	l.client = control.NewClient(l.sessionPaths.SocketPath)
	return waitForControl(l.ctx, l.client)
}

// CreateSession registers the session in the supervisor. The
// rollback closes the session through the same control RPC; the
// reason field reads l.closeReason which Wait() updates to
// "claude process exited" on the success path.
func (l *sessionLauncher) CreateSession() error {
	created, err := l.client.CreateSession(l.ctx, model.SessionCreateRequest{
		ID:          l.sessionID,
		Name:        l.launch.SessionName,
		ProjectDir:  l.cwd,
		LauncherPID: os.Getpid(),
	})
	if err != nil {
		return err
	}
	l.session = created
	l.rollback.push(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = l.client.CloseSession(cctx, l.sessionID, model.SessionCloseRequest{Reason: l.closeReason})
		cancel()
	})
	// The supervisor publishes the launch posture in-process during
	// createSession, so `created` is already the routed snapshot. Write the
	// initial manifest from it — this was ConfigureRoute's tail before the
	// SetRoute RPC was removed. Manifest cleanup is covered by the runtime-dir
	// RemoveAll registered in PreparePaths.
	if err := writeManifest(l.sessionPaths.RuntimeDir, l.sessionPaths.SocketPath, created, l.pre.Egress); err != nil {
		return err
	}
	// The Projects page offers this folder's last profile by default.
	_ = projects.RememberLaunch(l.paths.StateDir, l.cwd, l.pre.ActiveProfileName, time.Now())
	return nil
}

// WriteSessionSettings builds the CCWRAP-generated session settings doc
// (proxy + CA env injected, host-managed provider keys stripped) and
// composes the child environment, including the placeholder auth
// bootstrap when the route requires it.
func (l *sessionLauncher) WriteSessionSettings() error {
	l.proxyURL = "http://" + l.session.ProxyListenAddr
	injectedEnv := preflight.InjectedEnv(l.proxyURL, l.ca.CertPath(), l.ca.BundlePath())
	doc, err := settings.MergeUserSettingsIntoCCWRAPSessionSettings(nilSafeSettings(l.pre.ParsedFlagSettings), injectedEnv)
	if err != nil {
		return fmt.Errorf("build session settings: %w", err)
	}
	// Inspection copy only: Claude Code gets the doc via sessionSettingsArg,
	// never this path (it would report the directory upstream).
	l.settingsFilePath = filepath.Join(l.sessionPaths.RuntimeDir, "ccwrap-session-settings.json")
	if err := writeSessionSettingsFile(l.settingsFilePath, doc); err != nil {
		return fmt.Errorf("write session settings: %w", err)
	}
	userSettingsFile := l.pre.ParsedFlagSettings != nil && l.pre.ParsedFlagSettings.Settings != nil && !l.pre.ParsedFlagSettings.Inline
	arg, cleanup, err := sessionSettingsArg(doc, userSettingsFile)
	if err != nil {
		return fmt.Errorf("session settings: %w", err)
	}
	l.rollback.push(cleanup)
	l.settingsArg = arg
	authBootstrap := preflight.ChildAuthBootstrap{}
	if l.pre.AuthBootstrap == model.AuthBootstrapPlaceholderActive {
		authBootstrap = preflight.ChildAuthBootstrap{
			EnvKey: l.pre.AuthBootstrapEnvKey,
			Value:  newAuthPlaceholder(l.pre.ActiveProfileName),
		}
	}
	// l.launch.Timezone is the resolved + validated effective TZ (runClaude
	// overwrote it via resolveEffectiveTimezone); "" means no override.
	childModelEnv := l.pre.ModelEnv
	if off := preflight.ClaudeReminderOffEnv(l.pre.ModelAlias, os.Environ()); len(off) > 0 {
		childModelEnv = make(map[string]string, len(l.pre.ModelEnv)+len(off))
		for k, v := range l.pre.ModelEnv {
			childModelEnv[k] = v
		}
		for k, v := range off {
			childModelEnv[k] = v
		}
	}
	l.childEnv = preflight.BuildChildEnv(os.Environ(), l.proxyURL, l.ca.CertPath(), l.ca.BundlePath(), childModelEnv, authBootstrap, l.launch.Timezone)
	// Commands Claude Code runs (its Bash tool, "!" commands) inherit this,
	// so `ccwrap open` inside a session knows which one it is in. Replaces
	// any value from an outer session.
	l.childEnv = setEnvVar(l.childEnv, sessionIDEnv, l.sessionID)
	return nil
}

// sessionIDEnv names the ccwrap session a process runs under.
const sessionIDEnv = "CCWRAP_SESSION_ID"

// setEnvVar returns env with key set to value, dropping earlier entries.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, prefix) {
			out = append(out, kv)
		}
	}
	return append(out, prefix+value)
}

// ResolveClaudeBin locates the Claude Code executable up front — it is the
// first launch step — so a missing `claude` fails fast with an actionable
// message instead of only after CA generation, proxy bind, and session
// bring-up. Idempotent: SpawnChild calls it too, so the capture path (which
// has no early step) still resolves.
func (l *sessionLauncher) ResolveClaudeBin() error {
	if l.bin != "" {
		return nil
	}
	bin, err := exec.LookPath(l.launch.ClaudeBin)
	if err != nil {
		return fmt.Errorf("could not find the Claude Code executable %q on your PATH: %w\n"+
			"  ccwrap launches Claude Code — install it and make sure `claude` is on PATH, "+
			"or point ccwrap at it with --claude-bin /path/to/claude (or set CLAUDE_BIN=/path/to/claude)",
			l.launch.ClaudeBin, err)
	}
	l.bin = bin
	return nil
}

// SpawnChild starts the child process, installs the signal forwarder, and
// registers two rollbacks: kill the child (in case Attach fails) and uninstall
// the signal handler. On the success path Wait() reaps the child via cmd.Wait,
// which makes the kill rollback a no-op.
func (l *sessionLauncher) SpawnChild() error {
	if err := l.ResolveClaudeBin(); err != nil {
		return err
	}
	bin := l.bin
	args := make([]string, 0, len(l.pre.RewrittenChildArgs)+2)
	args = append(args, "--settings", l.settingsArg)
	args = append(args, l.pre.RewrittenChildArgs...)
	cmd := exec.Command(bin, args...)
	cmd.Env = l.childEnv
	// Bind the child's lifetime to ours where the OS allows (Linux
	// Pdeathsig) so a hard death of ccwrap — SIGKILL/OOM/bare-goroutine
	// panic, none of which run Wait()'s in-process kill — does not leave
	// Claude orphaned against a dead proxy. No-op off Linux.
	cmd.SysProcAttr = childSysProcAttr()
	cmd.Stdin = os.Stdin
	if l.childStdout != nil {
		cmd.Stdout = l.childStdout
	} else {
		cmd.Stdout = os.Stdout
	}
	cmd.Stderr = os.Stderr
	cmd.Dir = l.cwd
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}
	l.cmd = cmd
	l.rollback.push(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	l.stopSignals = forwardSignalsToChild(l.cancelCtx, cmd.Process)
	l.rollback.push(l.stopSignals)
	return nil
}

// Attach registers the child PID + start token with the supervisor
// and refreshes the manifest with the now-known ClaudePID.
func (l *sessionLauncher) Attach() error {
	token, tokErr := procmeta.StartToken(l.cmd.Process.Pid)
	if tokErr != nil {
		fmt.Fprintf(os.Stderr, "warning: unable to capture Claude start token; session lifecycle will fall back to PID-only checks: %v\n", tokErr)
	}
	if err := l.client.Attach(l.ctx, l.sessionID, model.SessionAttachRequest{
		ClaudePID:        l.cmd.Process.Pid,
		ClaudeStartToken: token,
	}); err != nil {
		return err
	}
	// Use a fresh context for the post-attach manifest refresh: l.ctx may
	// be near-cancellation but the manifest update should still happen.
	sess, _ := l.client.GetSession(context.Background(), l.sessionID)
	if sess != nil {
		l.session = sess
		_ = writeManifest(l.sessionPaths.RuntimeDir, l.sessionPaths.SocketPath, sess, l.pre.Egress)
	}
	return nil
}

// inspectLink is where the banner sends the user: the session in ccwrap ui,
// or "" when the app was not up in time.
func (l *sessionLauncher) inspectLink() string {
	return l.appLink
}

// PrintSummary emits the launch banner. Called after Attach succeeds.
func (l *sessionLauncher) PrintSummary() {
	l.warnedModels = seenUnaliasedModels(l.pre, l.paths.StateDir)
	if l.launch.Quiet {
		pal := ui.New(ui.IsTerminal(os.Stderr))
		host := l.pre.APIBaseURL.Host
		if host == "" {
			host = l.pre.APIBaseURL.String()
		}
		degraded := l.pre.RouteClass == model.RouteClassThirdPartyCompatible || l.pre.AuthPolicy == model.AuthPolicyUnsafePassthrough
		line := quietSummaryLine(pal, host, l.pre.ActiveProfileName, degraded, l.inspectLink())
		_, quietMark, disclosure := updateBannerLines(l.paths.StateDir, versionString(), l.launch.NoUpdateCheck, os.Getenv, ui.IsTerminal(os.Stderr), pal)
		if quietMark != "" {
			line += " · " + quietMark
		}
		fmt.Fprintln(os.Stderr, line)
		if disclosure != "" {
			fmt.Fprintln(os.Stderr, disclosure)
		}
		// A "requests will fail" condition is too important to hide in quiet mode.
		if l.pre.AuthBootstrap == model.AuthBootstrapMissing {
			fmt.Fprintln(os.Stderr, "  ⚠ Requests will fail until you "+authMissingRecoverHint(l.pre.MissingAuthEnv)+".")
		}
		for _, line := range launchWarningLines(l.pre, l.paths.StateDir) {
			fmt.Fprintln(os.Stderr, line)
		}
		return
	}
	printLaunchSummary(
		l.sessionID,
		l.inspectLink(),
		l.pre.APIBaseURL.String(),
		l.pre.RouteSource,
		l.pre.RouteClass,
		l.pre.AuthMode,
		l.pre.AuthSource,
		l.pre.AuthPolicy,
		l.pre.AuthBootstrap,
		l.pre.AuthBootstrapKind,
		l.pre.Egress.Mode,
		l.pre.Egress.Source,
		egress.Summary(l.pre.Egress),
		l.pre.ModelAlias,
		l.pre.UpstreamHeaders,
		l.cmd.Process.Pid,
		l.pre.ActiveProfileName,
		l.pre.ActiveProfileProvider,
		l.pre.MissingAuthEnv,
	)
	if lines := launchWarningLines(l.pre, l.paths.StateDir); len(lines) > 0 {
		fmt.Fprintln(os.Stderr)
		for _, line := range lines {
			fmt.Fprintln(os.Stderr, line)
		}
	}
	pal := ui.New(ui.IsTerminal(os.Stderr))
	full, _, disclosure := updateBannerLines(l.paths.StateDir, versionString(), l.launch.NoUpdateCheck, os.Getenv, ui.IsTerminal(os.Stderr), pal)
	if full != "" {
		fmt.Fprintln(os.Stderr, full)
	}
	if disclosure != "" {
		fmt.Fprintln(os.Stderr, disclosure)
	}
}

// Wait blocks until Claude exits OR the supervisor goroutine returns,
// whichever happens first.
//
// The normal path is Claude exiting: the deferred rollback.run() in
// runClaude then handles CloseSession + Shutdown + RemoveAll in LIFO
// order (each cleanup sync.Once-protected, so a post-graceful-exit
// rollback is harmless). Returns the child's exit code (0 if absent)
// and any non-exit error from cmd.Wait.
//
// The other path is the supervisor returning FIRST. The supervisor runs
// as an in-process goroutine (StartSupervisor), so if its Serve loop
// ends — error or otherwise — the proxy Claude depends on is gone and
// every subsequent model request can only fail with connection-refused.
// Previously Wait blocked solely on cmd.Wait(), so this state was
// SILENT: Claude kept running against a dead proxy and ccwrap reported
// nothing until the user manually quit. Now we select on supervisorErr
// too, kill the child, and surface the cause — failing loudly instead of
// degrading invisibly. (A hard kill of the whole ccwrap PROCESS — SIGKILL
// / OOM / panic in a bare goroutine — can't be handled from here; that is
// covered out-of-band by Pdeathsig on Linux and the next-launch orphan
// sweep. This path is specifically the supervisor-goroutine-returns case.)
func (l *sessionLauncher) Wait() (int, error) {
	childDone := make(chan error, 1)
	go func() { childDone <- l.cmd.Wait() }()

	select {
	case waitErr := <-childDone:
		l.closeReason = "claude process exited"
		if waitErr == nil {
			return 0, nil
		}
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return 0, waitErr
	case svErr := <-l.supervisorErr:
		// Supervisor died before Claude — the data-plane proxy is gone.
		// Kill the now-useless child and reap it so cmd resources are
		// released, then surface the cause. closeReason feeds the rollback
		// CloseSession so the session record reflects the real reason.
		l.closeReason = "supervisor exited before claude"
		if l.cmd.Process != nil {
			_ = l.cmd.Process.Kill()
		}
		<-childDone // reap the child Wait so we never leak the zombie
		if svErr != nil {
			return 0, fmt.Errorf("supervisor exited before Claude: %w", svErr)
		}
		return 0, fmt.Errorf("supervisor exited before Claude (proxy no longer available)")
	}
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
		ProjectDir:                         sess.ProjectDir,
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

func nilSafeSettings(parsed *settings.ParsedFlagSettings) map[string]any {
	if parsed == nil || parsed.Settings == nil {
		return nil
	}
	return parsed.Settings
}

// PrintUnmappedNotice reports, after Claude Code exits, the Claude models the
// session sent with no alias. Best effort: a supervisor that is already gone
// prints nothing.
func (l *sessionLauncher) PrintUnmappedNotice(w io.Writer) {
	if l.client == nil || l.sessionID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sess, err := l.client.GetSession(ctx, l.sessionID)
	if err != nil {
		return
	}
	var skip []string
	if sess.ActiveProfileName == l.pre.ActiveProfileName {
		skip = l.warnedModels
	}
	for _, line := range unmappedExitLines(sess, skip) {
		fmt.Fprintln(w, line)
	}
}
