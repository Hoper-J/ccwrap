package supervisor

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/control"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// An upstream that drops a response mid-body makes httputil log a copy
// error. The launcher's stderr is Claude Code's terminal, so the line must
// land in the session log, not the standard logger.
func TestReverseProxyErrorsGoToTheSessionLog(t *testing.T) {
	var std bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&std)
	t.Cleanup(func() { log.SetOutput(prev) })

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, bw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		_, _ = bw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{\"partial\":")
		_ = bw.Flush()
		_ = c.Close()
	}))
	t.Cleanup(up.Close)

	paths := testutil.ShortAppPaths(t, "e.sock")
	srv, err := New(paths, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = srv.Run(ctx) }()
	client := control.NewClient(paths.SocketPath)
	waitForSupervisor(t, client)
	sess, err := client.CreateSession(context.Background(), model.SessionCreateRequest{LauncherPID: os.Getpid(), Name: "errorlog"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetRoute(context.Background(), sess.ID, xAPIKeyRoute(up.URL, model.EgressConfig{Mode: "direct"})); err != nil {
		t.Fatal(err)
	}
	claudeSideClient(t, paths.CABundlePath, sess)(claudeHead("/v1/messages", "2"), "{}")

	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(paths.LogPath)
		if strings.Contains(string(data), "ReverseProxy read error") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session log never got the copy error:\n%s", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(std.String(), "ReverseProxy") {
		t.Fatalf("copy error reached the standard logger (stderr):\n%s", std.String())
	}
}
