package uiservice

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/manifest"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/procmeta"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/profiletest"
	"github.com/Hoper-J/ccwrap/internal/projects"
	"github.com/Hoper-J/ccwrap/internal/supervisor"
	"github.com/Hoper-J/ccwrap/internal/termlaunch"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

type running struct {
	srv  *Server
	info Info
	done chan struct{} // closed when Serve returns
}

func startUI(t *testing.T, paths app.Paths, env map[string]string, idle time.Duration) *running {
	t.Helper()
	srv := New(Options{
		Paths: paths, Version: "test", Exe: "/usr/local/bin/ccwrap",
		Getenv:      func(k string) string { return env[k] },
		Logger:      log.New(io.Discard, "", 0),
		IdleTimeout: idle, PollInterval: 30 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var serveErr error
	go func() { serveErr = srv.Serve(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("serve did not return")
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if info, ok := Running(context.Background(), paths); ok {
			return &running{srv: srv, info: info, done: done}
		}
		select {
		case <-done:
			t.Fatalf("serve exited: %v", serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("ui service did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *running) http(t *testing.T, method, path, body string, header map[string]string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, r.info.URL()+strings.TrimPrefix(path, "/"), strings.NewReader(body))
	for k, v := range header {
		if k == "Host" {
			req.Host = v
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (r *running) write(t *testing.T, method, path, body string) (int, map[string]any) {
	return r.http(t, method, path, body, map[string]string{"X-CCWRAP-Token": r.srv.token})
}

func code(out map[string]any) string {
	e, _ := out["error"].(map[string]any)
	c, _ := e["code"].(string)
	return c
}

// One instance per user: a second Serve refuses, Stop ends the first and
// removes its files.
func TestLifecycle(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	r := startUI(t, paths, nil, time.Hour)
	if r.info.Version != "test" || r.info.PID != os.Getpid() || !strings.HasPrefix(r.info.HTTPAddr, "127.0.0.1:") {
		t.Fatalf("info = %+v", r.info)
	}
	second := New(Options{Paths: paths, Logger: log.New(io.Discard, "", 0)})
	if err := second.Serve(context.Background()); !errors.Is(err, ErrRunning) {
		t.Fatalf("second serve: %v", err)
	}
	if stopped, err := Stop(context.Background(), paths, 3*time.Second); !stopped || err != nil {
		t.Fatalf("stop: %v %v", stopped, err)
	}
	select {
	case <-r.done:
	case <-time.After(3 * time.Second):
		t.Fatal("serve did not return")
	}
	for _, f := range []string{infoFile, sockFile} {
		if _, err := os.Stat(filepath.Join(Dir(paths), f)); !os.IsNotExist(err) {
			t.Errorf("%s left behind", f)
		}
	}
	if _, ok := Running(context.Background(), paths); ok {
		t.Fatal("still running")
	}
}

// The port of the last run is reused; CCWRAP_UI_PORT fixes it, and a busy
// fixed port falls back with a note.
func TestPortChoice(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	r := startUI(t, paths, nil, time.Hour)
	first := r.info.HTTPAddr
	if _, err := Stop(context.Background(), paths, 3*time.Second); err != nil {
		t.Fatal(err)
	}
	<-r.done
	if again := startUI(t, paths, nil, time.Hour); again.info.HTTPAddr != first {
		t.Fatalf("port not reused: %s then %s", first, again.info.HTTPAddr)
	}
	if _, err := Stop(context.Background(), paths, 3*time.Second); err != nil {
		t.Fatal(err)
	}

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := strconv.Itoa(busy.Addr().(*net.TCPAddr).Port)
	fixed := startUI(t, paths, map[string]string{PortEnv: port}, time.Hour)
	if strings.HasSuffix(fixed.info.HTTPAddr, ":"+port) || !strings.Contains(fixed.info.PortNote, port) {
		t.Fatalf("busy fixed port: %+v", fixed.info)
	}
}

// Browser-facing checks: unknown Hosts are refused; writes need the page's
// token and a same-site request.
func TestHTTPGuards(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	r := startUI(t, paths, nil, time.Hour)
	if status, _ := r.http(t, "GET", "/api/v1/meta", "", map[string]string{"Host": "evil.example"}); status != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding Host: %d", status)
	}
	if status, out := r.http(t, "GET", "/api/v1/meta", "", nil); status != 200 || out["instance_id"] != r.info.InstanceID {
		t.Fatalf("meta: %d %v", status, out)
	}
	body := `{"terminal":"terminal"}`
	if status, out := r.http(t, "PUT", "/api/v1/settings/terminal", body, nil); status != 403 || code(out) != "token" {
		t.Fatalf("no token: %d %v", status, out)
	}
	if status, out := r.http(t, "PUT", "/api/v1/settings/terminal", body, map[string]string{"X-CCWRAP-Token": r.srv.token, "Origin": "https://evil.example"}); status != 403 || code(out) != "cross_site" {
		t.Fatalf("cross-site: %d %v", status, out)
	}
	if status, out := r.http(t, "PUT", "/api/v1/settings/terminal", body, map[string]string{"X-CCWRAP-Token": r.srv.token, "Sec-Fetch-Site": "cross-site"}); status != 403 || code(out) != "cross_site" {
		t.Fatalf("sec-fetch-site: %d %v", status, out)
	}
	if status, out := r.http(t, "DELETE", "/api/v1/meta", "", map[string]string{"X-CCWRAP-Token": r.srv.token}); status != http.StatusMethodNotAllowed || code(out) != "method_not_allowed" {
		t.Fatalf("wrong method: %d %v", status, out)
	}
	// Over the socket no token is needed.
	var meta map[string]any
	if err := getJSON(context.Background(), r.info.Socket, "/api/v1/meta", &meta); err != nil || meta["api"] != "v1" {
		t.Fatalf("socket meta: %v %v", err, meta)
	}
}

// startSession runs a real session supervisor registered under paths, as a
// launch would, in projectDir. stop ends its process.
func startSession(t *testing.T, paths app.Paths, id, projectDir string) (stop func()) {
	t.Helper()
	sp := paths.SessionPaths(id)
	if err := os.MkdirAll(sp.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	srv, err := supervisor.New(sp, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Run(ctx); close(done) }()
	stop = func() { cancel(); <-done }
	t.Cleanup(stop)
	client := control.NewClient(sp.SocketPath)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := client.Status(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("supervisor did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{ID: id, ProjectDir: projectDir, LauncherPID: os.Getpid()})
	if err != nil {
		t.Fatal(err)
	}
	token, err := procmeta.CurrentStartToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Write(manifest.Path(sp.RuntimeDir), model.SessionManifest{
		SessionID: id, CreatedAt: sess.CreatedAt, State: sess.State, SupervisorPID: os.Getpid(), SupervisorStartToken: token,
		ControlSocket: sp.SocketPath, ProxyListenAddr: sess.ProxyListenAddr, ProjectDir: projectDir,
	}); err != nil {
		t.Fatal(err)
	}
	return stop
}

// Sessions are listed from discovery, their API is reachable through the
// service, and the merged event stream announces new sessions and relays
// their events.
func TestSessionsAndEvents(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	proj := t.TempDir()
	startSession(t, paths, "aaaa000000000001", proj)
	r := startUI(t, paths, nil, time.Hour)

	status, out := r.http(t, "GET", "/api/v1/sessions", "", nil)
	list, _ := out["sessions"].([]any)
	if status != 200 || len(list) != 1 {
		t.Fatalf("sessions: %d %v", status, out)
	}
	first := list[0].(map[string]any)
	disp := first["display"].(map[string]any)
	// Listed under its resolved folder, as the Projects list has it.
	if first["project_dir"] != projects.Canonical(proj) || first["health"] != "ok" || disp["title"] != filepath.Base(proj) || disp["subtitle"] != "no profile" {
		t.Fatalf("summary = %v", first)
	}
	if status, sess := r.http(t, "GET", "/api/v1/sessions/aaaa000000000001/session", "", nil); status != 200 || sess["project_dir"] != proj {
		t.Fatalf("forwarded session: %d %v", status, sess)
	}
	if status, out := r.http(t, "GET", "/api/v1/sessions/ffff/session", "", nil); status != 404 || code(out) != "no_session" {
		t.Fatalf("unknown session: %d %v", status, out)
	}
	if status, out := r.http(t, "POST", "/api/v1/sessions/aaaa000000000001/capture", `{"bodies":true}`, nil); status != 403 || code(out) != "token" {
		t.Fatalf("forwarded write without token: %d %v", status, out)
	}
	if status, out := r.write(t, "POST", "/api/v1/sessions/aaaa000000000001/capture", `{"bodies":true}`); status != 200 || out["capture_bodies"] != true {
		t.Fatalf("forwarded write: %d %v", status, out)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", r.info.URL()+"api/v1/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	events := make(chan [2]string, 64)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		typ := ""
		for sc.Scan() {
			switch line := sc.Text(); {
			case strings.HasPrefix(line, "event: "):
				typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				events <- [2]string{typ, strings.TrimPrefix(line, "data: ")}
			}
		}
	}()
	waitFor := func(typ, contains string) {
		t.Helper()
		for {
			select {
			case ev := <-events:
				if ev[0] == typ && strings.Contains(ev[1], contains) {
					return
				}
			case <-ctx.Done():
				t.Fatalf("no %s event containing %q", typ, contains)
			}
		}
	}
	waitFor("hello", r.info.InstanceID)
	proj2 := t.TempDir()
	startSession(t, paths, "bbbb000000000002", proj2)
	waitFor("sessions.changed", "bbbb000000000002")
	// A capture toggle on the new session reaches the page as session.updated.
	time.Sleep(100 * time.Millisecond) // let the hub start following it
	if status, _ := r.write(t, "POST", "/api/v1/sessions/bbbb000000000002/capture", `{"telemetry":true}`); status != 200 {
		t.Fatalf("capture: %d", status)
	}
	waitFor("session.updated", `"capture_telemetry":true`)
	// Its end reaches the page with its final state, and the list greys it.
	if err := control.NewClient(paths.SessionPaths("bbbb000000000002").SocketPath).CloseSession(context.Background(), "bbbb000000000002", model.SessionCloseRequest{Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	waitFor("session.ended", `"state":"ended"`)
	waitFor("sessions.changed", `"state":"ended"`)
}

// Projects come from Claude Code's history; a launch runs this ccwrap in a
// known folder with a known profile, and nothing else.
func TestProjectsAndLaunch(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	// Claude Code records folders resolved (macOS's temp dir is a symlink).
	proj := projects.Canonical(t.TempDir())
	cfg, _ := json.Marshal(map[string]any{"projects": map[string]any{proj: map[string]any{"hasTrustDialogAccepted": true}}, "deepLinkTerminal": "iTerm"})
	if err := os.WriteFile(filepath.Join(claudeDir, ".claude.json"), cfg, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	profilesJSON := `{"default":"gw","profiles":{"gw":{"provider":"Gateway","base_url":"https://gw.example","auth":{"mode":"ccwrap_bearer","key":"secret-key"}}}}`
	if err := os.WriteFile(filepath.Join(paths.StateDir, "profiles.json"), []byte(profilesJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	apps := t.TempDir()
	for _, b := range []string{"Terminal.app", "iTerm.app"} {
		_ = os.MkdirAll(filepath.Join(apps, b), 0o755)
	}
	installedTerminals = func() []termlaunch.Installation { return termlaunch.InstalledIn([]string{apps}) }
	var launched []termlaunch.Request
	var usedTerminal string
	launchTerminal = func(in termlaunch.Installation, req termlaunch.Request, scriptDir string) error {
		launched = append(launched, req)
		usedTerminal = in.ID
		return nil
	}
	t.Cleanup(func() { installedTerminals, launchTerminal = termlaunch.Installed, termlaunch.Launch })

	r := startUI(t, paths, nil, time.Hour)
	status, out := r.http(t, "GET", "/api/v1/projects", "", nil)
	list, _ := out["projects"].([]any)
	if status != 200 || len(list) != 1 || list[0].(map[string]any)["path"] != proj || list[0].(map[string]any)["trusted"] != true {
		t.Fatalf("projects: %d %v", status, out)
	}
	status, prof := r.http(t, "GET", "/api/v1/profiles", "", nil)
	if status != 200 || prof["default"] != "gw" || strings.Contains(mustJSON(prof), "secret-key") {
		t.Fatalf("profiles: %d %v", status, prof)
	}

	for _, tc := range []struct {
		body   string
		status int
		code   string
	}{
		{`{"path":"` + proj + `","mode":"later"}`, 400, "usage"},
		{`{"path":"/somewhere/else","mode":"new"}`, 404, "unknown_project"},
		{`{"path":"` + proj + `","profile":"nope","mode":"new"}`, 422, "unknown_profile"},
	} {
		if status, out := r.write(t, "POST", "/api/v1/projects/launch", tc.body); status != tc.status || code(out) != tc.code {
			t.Errorf("%s: %d %v", tc.body, status, out)
		}
	}
	if len(launched) != 0 {
		t.Fatal("refused launches must not run anything")
	}
	status, out = r.write(t, "POST", "/api/v1/projects/launch", `{"path":"`+proj+`","profile":"gw","mode":"continue"}`)
	if status != 200 || out["terminal"] != "iTerm" {
		t.Fatalf("launch: %d %v", status, out)
	}
	if len(launched) != 1 || usedTerminal != "iterm" || launched[0].Dir != proj || launched[0].Exe != "/usr/local/bin/ccwrap" || strings.Join(launched[0].Args, " ") != "--profile gw -- --continue" {
		t.Fatalf("launched %+v in %s", launched, usedTerminal)
	}
	if status, out := r.write(t, "POST", "/api/v1/projects/launch", `{"path":"`+proj+`","mode":"new"}`); status != 429 || code(out) != "too_fast" {
		t.Fatalf("second launch within a second: %d %v", status, out)
	}

	if status, out := r.write(t, "PUT", "/api/v1/settings/terminal", `{"terminal":"terminal"}`); status != 200 || out["current"] != "terminal" || out["claude_choice"] != "iterm" {
		t.Fatalf("set terminal: %d %v", status, out)
	}
	if status, out := r.write(t, "PUT", "/api/v1/settings/terminal", `{"terminal":"ghostty"}`); status != 422 || code(out) != "unknown_terminal" {
		t.Fatalf("uninstalled terminal: %d %v", status, out)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// With no page and no session, the service exits after the idle timeout.
func TestIdleExit(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	r := startUI(t, paths, nil, 200*time.Millisecond)
	select {
	case <-r.done:
	case <-time.After(5 * time.Second):
		t.Fatal("did not exit when idle")
	}
}

// A session launch starts ccwrap ui as its child and lives on for hours.
// When the ui exits first (ccwrap ui stop, a replacement, a lost start
// race), the starter reaps it instead of keeping a zombie until the
// session ends.
func TestStartReapsTheService(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	exe := filepath.Join(t.TempDir(), "exits-at-once")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := start(paths, exe); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // the child exits at once
	deadline := time.Now().Add(3 * time.Second)
	for n := zombieChildren(t); n > 0; n = zombieChildren(t) {
		if time.Now().After(deadline) {
			t.Fatalf("%d zombie child(ren) left after the started service exited", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// zombieChildren counts this process's children that exited unreaped.
func zombieChildren(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("ps", "-A", "-o", "ppid=,stat=").Output()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) == 2 && f[0] == strconv.Itoa(os.Getpid()) && strings.HasPrefix(f[1], "Z") {
			n++
		}
	}
	return n
}

// ui.log is appended to by every start. Past maxUILogBytes it is moved to
// ui.log.1 (replacing the one before) when the next ui starts, so the logs
// of past runs do not pile up.
func TestStartRotatesALargeLog(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	if err := os.MkdirAll(Dir(paths), 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(Dir(paths), logFile)
	big := make([]byte, maxUILogBytes+1)
	if err := os.WriteFile(logPath, big, 0o600); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "exits-at-once")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := start(paths, exe); err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(logPath + ".1")
	if err != nil || old.Size() != int64(len(big)) {
		t.Fatalf("ui.log.1 = %v, %v; want the previous log, %d bytes", old, err, len(big))
	}
	if cur, err := os.Stat(logPath); err != nil || cur.Size() >= int64(len(big)) {
		t.Fatalf("ui.log = %v, %v; want a fresh log", cur, err)
	}
}

// Ensure reuses a running service of the same version and executable, even
// when the caller names the executable through a symlink.
func TestEnsureReusesMatchingService(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	dir := t.TempDir()
	real := filepath.Join(dir, "ccwrap")
	if err := os.WriteFile(real, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "ccwrap-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(real)
	srv := New(Options{Paths: paths, Version: "test", Exe: resolved, Logger: log.New(io.Discard, "", 0)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := Running(context.Background(), paths); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, err := Ensure(context.Background(), paths, link, "test", time.Second)
	if err != nil || info.InstanceID != srv.Info().InstanceID {
		t.Fatalf("Ensure = %+v, %v; want the running instance %s", info, err, srv.Info().InstanceID)
	}
}

// startLegacySession registers a session whose supervisor predates /api/v1:
// it answers the control status but nothing under /api/v1.
func startLegacySession(t *testing.T, paths app.Paths, id string) {
	t.Helper()
	sp := paths.SessionPaths(id)
	if err := os.MkdirAll(sp.RuntimeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", sp.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(model.StatusResponse{SessionID: id, SocketPath: sp.SocketPath})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	token, err := procmeta.CurrentStartToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Write(manifest.Path(sp.RuntimeDir), model.SessionManifest{
		SessionID: id, CreatedAt: time.Now(), State: model.StateActive, SupervisorPID: os.Getpid(), SupervisorStartToken: token,
		ControlSocket: sp.SocketPath,
	}); err != nil {
		t.Fatal(err)
	}
}

// listed polls the session list until match accepts it.
func (r *running) listed(t *testing.T, what string, match func(map[string]map[string]any) bool) map[string]map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, out := r.http(t, "GET", "/api/v1/sessions", "", nil)
		byID := map[string]map[string]any{}
		list, _ := out["sessions"].([]any)
		for _, v := range list {
			sum := v.(map[string]any)
			byID[sum["id"].(string)] = sum
		}
		if match(byID) {
			return byID
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: sessions = %v", what, byID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Legacy sessions are not listed. An ended session stays, greyed out, for
// EndedRetention — whether it says it ended or its process went away — and
// is then dropped.
func TestSessionListHidesLegacyAndKeepsEndedForAWhile(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	startLegacySession(t, paths, "cccc000000000003")
	stopA := startSession(t, paths, "aaaa000000000001", t.TempDir())
	_ = startSession(t, paths, "bbbb000000000002", t.TempDir())
	srv := New(Options{
		Paths: paths, Version: "test", Exe: "/usr/local/bin/ccwrap", Logger: log.New(io.Discard, "", 0),
		IdleTimeout: time.Hour, PollInterval: 30 * time.Millisecond, EndedRetention: 600 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = srv.Serve(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	var r *running
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if info, ok := Running(context.Background(), paths); ok {
			r = &running{srv: srv, info: info, done: done}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ui service did not start")
		}
	}

	byID := r.listed(t, "both sessions", func(m map[string]map[string]any) bool { return len(m) == 2 })
	if _, ok := byID["cccc000000000003"]; ok {
		t.Fatal("a legacy session is listed")
	}

	// A session that says it ended.
	if err := control.NewClient(paths.SessionPaths("bbbb000000000002").SocketPath).CloseSession(context.Background(), "bbbb000000000002", model.SessionCloseRequest{Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	// A session whose process went away.
	stopA()
	byID = r.listed(t, "both ended", func(m map[string]map[string]any) bool {
		return m["aaaa000000000001"]["state"] == "ended" && m["bbbb000000000002"]["state"] == "ended"
	})
	for id, sum := range byID {
		if sum["ended_at"] == nil || sum["display"].(map[string]any)["badge"] != nil {
			t.Errorf("%s ended: %v", id, sum)
		}
	}
	r.listed(t, "ended sessions dropped", func(m map[string]map[string]any) bool { return len(m) == 0 })
}

// A session's health and badge come from its open issues.
func TestSessionSummaryShowsIssues(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	startSession(t, paths, "aaaa000000000001", t.TempDir())
	client := control.NewClient(paths.SessionPaths("aaaa000000000001").SocketPath)
	if err := client.SetRoute(context.Background(), "aaaa000000000001", model.SessionRouteRequest{
		APIBaseURL: "https://gw.example/", RouteClass: model.RouteClassThirdPartyHidden, AuthBootstrap: model.AuthBootstrapMissing,
		ActiveProfileName: "gw", MissingAuthEnv: "GW_KEY", FailPolicy: model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}
	r := startUI(t, paths, nil, time.Hour)
	sum := r.listed(t, "the session", func(m map[string]map[string]any) bool { return len(m) == 1 })["aaaa000000000001"]
	if sum["health"] != "warn" || sum["profile"] != "gw" || sum["display"].(map[string]any)["badge"] != "1 issue" || sum["display"].(map[string]any)["subtitle"] != "gw" {
		t.Fatalf("summary = %v", sum)
	}
}

// The Profiles page's API: add, read, edit, set default, remove — writes
// need the token, nothing returns a key, and validation errors name the
// fields at fault.
func TestProfilesAPI(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	r := startUI(t, paths, nil, time.Hour)
	add := `{"name":"gw","provider":"Gateway","base_url":"https://gw.example","auth":{"mode":"ccwrap_bearer","key":"sk-very-secret"}}`
	if status, out := r.http(t, "POST", "/api/v1/profiles", add, nil); status != 403 || code(out) != "token" {
		t.Fatalf("add without token: %d %v", status, out)
	}
	status, out := r.write(t, "POST", "/api/v1/profiles", add)
	if status != http.StatusCreated || out["default"] != "official" || strings.Contains(mustJSON(out), "sk-very-secret") {
		t.Fatalf("add: %d %v", status, out)
	}
	if prof := out["profile"].(map[string]any); prof["name"] != "gw" || prof["is_default"] != false || len(prof["sessions"].([]any)) != 0 {
		t.Fatalf("added profile: %v", prof)
	}
	if status, out := r.write(t, "POST", "/api/v1/profiles", add); status != http.StatusConflict || code(out) != "conflict" {
		t.Fatalf("duplicate: %d %v", status, out)
	}

	status, list := r.http(t, "GET", "/api/v1/profiles", "", nil)
	items, _ := list["profiles"].([]any)
	if status != 200 || len(items) != 2 || list["path"] != filepath.Join(paths.StateDir, "profiles.json") || strings.Contains(mustJSON(list), "sk-very-secret") {
		t.Fatalf("list: %d %v", status, list)
	}
	if status, one := r.http(t, "GET", "/api/v1/profiles/gw", "", nil); status != 200 || one["base_url"] != "https://gw.example" || one["auth"].(map[string]any)["has_inline_key"] != true {
		t.Fatalf("get: %d %v", status, one)
	}
	if status, out := r.http(t, "GET", "/api/v1/profiles/nope", "", nil); status != 404 || code(out) != "not_found" {
		t.Fatalf("get unknown: %d %v", status, out)
	}

	status, out = r.write(t, "PATCH", "/api/v1/profiles/gw", `{"base_url":"https://gw.example/anthropic"}`)
	if status != 200 || out["profile"].(map[string]any)["base_url"] != "https://gw.example/anthropic" || len(out["applied"].([]any)) != 0 {
		t.Fatalf("edit: %d %v", status, out)
	}
	if status, out := r.write(t, "PATCH", "/api/v1/profiles/gw", `{"name":"other"}`); status != 400 || code(out) != "usage" {
		t.Fatalf("rename: %d %v", status, out)
	}
	status, out = r.write(t, "PATCH", "/api/v1/profiles/gw", `{"base_url":"ftp://nope"}`)
	if e, _ := out["error"].(map[string]any); status != 422 || code(out) != "validation_error" || e["paths"] == nil {
		t.Fatalf("invalid edit: %d %v", status, out)
	}

	if status, out := r.write(t, "POST", "/api/v1/profiles/gw/default", ""); status != 200 || out["default"] != "gw" {
		t.Fatalf("set default: %d %v", status, out)
	}
	if status, out := r.write(t, "DELETE", "/api/v1/profiles/gw", `{"confirm_name":"gx"}`); status != 400 || code(out) != "usage" {
		t.Fatalf("remove unconfirmed: %d %v", status, out)
	}
	status, out = r.write(t, "DELETE", "/api/v1/profiles/gw", `{"confirm_name":"gw"}`)
	if status != 200 || out["removed"] != "gw" || out["default"] != "official" {
		t.Fatalf("remove: %d %v", status, out)
	}
	req, _ := http.NewRequest("PUT", r.info.URL()+"api/v1/profiles/official", nil)
	req.Header.Set("X-CCWRAP-Token", r.srv.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, PATCH, DELETE" {
		t.Fatalf("wrong method: %d allow=%q", resp.StatusCode, resp.Header.Get("Allow"))
	}
}

// Editing a profile a session runs on applies it to that session; the list
// shows which sessions use each profile.
func TestProfileEditReachesItsSessions(t *testing.T) {
	paths := testutil.ShortAppPaths(t, "ui.sock")
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(paths.StateDir, "profiles.json"), []byte(`{"default":"gw","profiles":{"gw":{"provider":"Gateway","base_url":"https://gw.example","auth":{"mode":"ccwrap_bearer","key":"k"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	startSession(t, paths, "aaaa000000000001", t.TempDir())
	if err := control.NewClient(paths.SessionPaths("aaaa000000000001").SocketPath).SetRoute(context.Background(), "aaaa000000000001", model.SessionRouteRequest{
		APIBaseURL: "https://gw.example/", RouteClass: model.RouteClassThirdPartyHidden, ActiveProfileName: "gw", FailPolicy: model.FailClosed,
	}); err != nil {
		t.Fatal(err)
	}
	r := startUI(t, paths, nil, time.Hour)
	r.listed(t, "the session", func(m map[string]map[string]any) bool { return m["aaaa000000000001"]["profile"] == "gw" })
	_, list := r.http(t, "GET", "/api/v1/profiles", "", nil)
	if gw := list["profiles"].([]any)[0].(map[string]any); gw["name"] != "gw" || mustJSON(gw["sessions"]) != `["aaaa000000000001"]` {
		t.Fatalf("list: %v", list)
	}
	status, out := r.write(t, "PATCH", "/api/v1/profiles/gw", `{"provider":"Gateway 2"}`)
	applied, _ := out["applied"].([]any)
	if status != 200 || len(applied) != 1 {
		t.Fatalf("edit: %d %v", status, out)
	}
	// The test session has no launch context, so it answers with its
	// refusal — which shows the switch reached it.
	if a := applied[0].(map[string]any); a["session_id"] != "aaaa000000000001" || a["result"] != "rejected_invalid" {
		t.Fatalf("applied: %v", a)
	}
}

// A connection test is kept as the profile's last test until the profile
// changes; tests of unknown profiles say so.
func TestProfileTests(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir()) // tests of an inherit egress read Claude settings
	var probed profiles.Profile
	probeProfile = func(p profiles.Profile, _ profiletest.ProbeOptions) profiletest.ProbeResult {
		probed = p
		return profiletest.ProbeResult{Profile: p.Name, Status: profiletest.StatusAuthFail, HTTPStatus: 401, Latency: 230 * time.Millisecond,
			ModelSent: "glm-4.6", Err: "401 Unauthorized: invalid api key"}
	}
	var egressProbed profiles.Profile
	probeEgress = func(p profiles.Profile, _ profiletest.EgressProbeOptions) profiletest.EgressProbeResult {
		egressProbed = p
		return profiletest.EgressProbeResult{}
	}
	t.Cleanup(func() { probeProfile, probeEgress = profiletest.Probe, profiletest.ProbeEgress })
	paths := testutil.ShortAppPaths(t, "ui.sock")
	r := startUI(t, paths, nil, time.Hour)
	if status, _ := r.write(t, "POST", "/api/v1/profiles", `{"name":"gw","base_url":"https://gw.example","auth":{"mode":"ccwrap_bearer","key":"k"}}`); status != http.StatusCreated {
		t.Fatalf("add: %d", status)
	}
	status, out := r.write(t, "POST", "/api/v1/profiles/gw/test", "")
	if status != 200 || out["status"] != "AUTH_FAIL" || out["text"] != "401 Unauthorized: invalid api key · model glm-4.6 · 230 ms" || probed.Auth == nil || probed.Auth.Key != "k" {
		t.Fatalf("test: %d %v (probed %+v)", status, out, probed)
	}
	if _, one := r.http(t, "GET", "/api/v1/profiles/gw", "", nil); one["last_test"].(map[string]any)["text"] != "401 Unauthorized: invalid api key · model glm-4.6 · 230 ms" {
		t.Fatalf("last test not kept: %v", one)
	}
	r.write(t, "PATCH", "/api/v1/profiles/gw", `{"auth":{"key_source":"inline","key":"k2"}}`)
	if _, one := r.http(t, "GET", "/api/v1/profiles/gw", "", nil); one["last_test"] != nil {
		t.Fatalf("an edit keeps the old test: %v", one)
	}
	if status, out := r.write(t, "POST", "/api/v1/profiles/nope/test", ""); status != 404 || code(out) != "not_found" {
		t.Fatalf("unknown: %d %v", status, out)
	}
	if status, out := r.write(t, "POST", "/api/v1/profiles/inherit-env/test", ""); status != 404 || !strings.Contains(mustJSON(out), "inherit-env credentials missing") {
		t.Fatalf("inherit-env without env: %d %v", status, out)
	}

	if status, out := r.write(t, "POST", "/api/v1/profiles/gw/test-egress", `{"egress":{"mode":"direct","url":"http://p:1"}}`); status != 422 || code(out) != "validation_error" {
		t.Fatalf("bad egress: %d %v", status, out)
	}
	if status, _ := r.write(t, "POST", "/api/v1/profiles/gw/test-egress", `{"egress":{"mode":"http","url":"http://proxy.example:8080"}}`); status != 200 || egressProbed.Egress.URL != "http://proxy.example:8080" {
		t.Fatalf("draft egress: %d %+v", status, egressProbed)
	}
	if status, _ := r.write(t, "POST", "/api/v1/profiles/gw/test-egress", ""); status != 200 || egressProbed.Egress.Mode != "" {
		t.Fatalf("saved egress: %d %+v", status, egressProbed)
	}
	// A new profile's form tests its egress before there is a profile.
	if status, out := r.write(t, "POST", "/api/v1/egress-test", ""); status != 400 || code(out) != "usage" {
		t.Fatalf("draft without egress: %d %v", status, out)
	}
	if status, _ := r.write(t, "POST", "/api/v1/egress-test", `{"egress":{"mode":"socks5","url":"socks5://127.0.0.1:1080"}}`); status != 200 || egressProbed.Egress.URL != "socks5://127.0.0.1:1080" || egressProbed.Name != "" {
		t.Fatalf("draft egress: %d %+v", status, egressProbed)
	}
	if _, list := r.http(t, "GET", "/api/v1/profiles", "", nil); list["display_path"] == nil {
		t.Fatalf("list has no display_path: %v", list)
	}
}
