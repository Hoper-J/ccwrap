package uiservice

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/testutil"
)

// stalledWriter is the response side of a client that stopped reading: every
// write waits for the write deadline and then fails, or waits forever when
// no deadline is set. Once a write failed, flushing reports it.
type stalledWriter struct {
	mu       sync.Mutex
	header   http.Header
	deadline time.Time
	err      error
}

func (w *stalledWriter) Header() http.Header {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *stalledWriter) WriteHeader(int) {}
func (w *stalledWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	d, err := w.deadline, w.err
	w.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if d.IsZero() {
		select {} // nobody reads, and nothing ever gives up
	}
	time.Sleep(time.Until(d))
	w.mu.Lock()
	w.err = os.ErrDeadlineExceeded
	w.mu.Unlock()
	return 0, os.ErrDeadlineExceeded
}
func (w *stalledWriter) Flush() {}
func (w *stalledWriter) FlushError() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}
func (w *stalledWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.deadline = t
	w.mu.Unlock()
	return nil
}

// A page whose connection stops reading (a sleeping laptop behind ssh -L)
// ends its event stream when a write times out, instead of holding the
// handler, its queue and the ui's idle-exit count until TCP gives up.
func TestEventsEndWhenThePageStopsReading(t *testing.T) {
	prev := sseWriteTimeout
	sseWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sseWriteTimeout = prev })
	r := startUI(t, testutil.ShortAppPaths(t, "ui.sock"), nil, time.Hour)

	done := make(chan struct{})
	go func() {
		r.srv.handleEvents(&stalledWriter{}, httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the events handler is still writing to a page that stopped reading")
	}
	if n := r.srv.clients.Load(); n != 0 {
		t.Fatalf("%d page(s) still counted after the stream ended", n)
	}
}

// A follow whose stream ends on its own (the session ended, or the
// supervisor dropped a slow subscriber) releases its context, instead of
// leaving it registered under the hub's context until the ui exits.
func TestFollowReleasesItsContextWhenTheStreamEnds(t *testing.T) {
	r := startUI(t, testutil.ShortAppPaths(t, "ui.sock"), nil, time.Hour)
	sock := filepath.Join(testutil.ShortAppPaths(t, "ev.sock").RuntimeDir, "..", "ev.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "event: hello\ndata: {}\n\n") // and the stream ends
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	var once sync.Once
	released := make(chan struct{})
	f := &follower{cancel: func() { cancel(); once.Do(func() { close(released) }) }}
	h := r.srv.hub
	h.mu.Lock()
	h.follows["s1"] = f
	h.mu.Unlock()
	h.follow(ctx, SessionSummary{ID: "s1", socket: sock}, f)
	select {
	case <-released:
	default:
		t.Fatal("the follow ended without releasing its context")
	}
}
