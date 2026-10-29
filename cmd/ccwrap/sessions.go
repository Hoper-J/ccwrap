package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/dashboard"
	"github.com/Hoper-J/ccwrap/internal/discovery"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/procmeta"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

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
	}
	// Each session forwards the stop to its Claude Code and waits for it to
	// exit, killing it after stopGrace; wait that long plus teardown time.
	deadline := time.Now().Add(stopGrace + 5*time.Second)
	for _, ds := range targets {
		if waitForSessionStop(paths, ds.Manifest.SessionID, time.Until(deadline)) {
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

func fetchSnapshot(ds model.DiscoveredSession) (*model.Session, error) {
	if !ds.Reachable {
		return &model.Session{
			ID:                                 ds.Manifest.SessionID,
			Name:                               ds.Manifest.Name,
			ProjectDir:                         ds.Manifest.ProjectDir,
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
		if p := ui.SessionPosture(sess); p != "" {
			fmt.Println("  " + p)
		}
		fmt.Println()
		auth := ui.HumanAuth(sess.AuthMode, sess.AuthSource) + " · " + ui.HumanAuthPolicy(sess.AuthPolicy) + " · " + ui.HumanAuthBootstrap(sess.AuthBootstrap, sess.AuthBootstrapKind)
		if sess.ProjectDir != "" {
			fmt.Println("  " + pal.Dim("project") + "   " + ui.DisplayDir(sess.ProjectDir))
		}
		fmt.Println("  " + pal.Dim("proxy") + "     " + proxyURL(sess.ProxyListenAddr))
		fmt.Println("  " + pal.Dim("route") + "     " + ui.HumanRouteClass(sess.RouteClass) + " · " + strings.ToLower(ui.HumanRouteSource(sess.RouteSource)))
		fmt.Println("  " + pal.Dim("auth") + "      " + auth)
		fmt.Println("  " + pal.Dim("models") + "    " + statusModelsLine(sess))
		if eg := ui.HumanEgress(sess.EgressMode, sess.EgressSource, sess.EgressSummary); eg != "Direct" {
			fmt.Println("  " + pal.Dim("egress") + "    " + eg)
		}
		fmt.Println("  " + pal.Dim("traffic") + "   " + fmt.Sprintf("%d req · %d failed", sess.RecentRequestCount, sess.RecentFailedCount))
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

// proxyURL is a session's proxy address as Claude Code's HTTPS_PROXY has it.
func proxyURL(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return ""
	}
	return "http://" + addr
}
