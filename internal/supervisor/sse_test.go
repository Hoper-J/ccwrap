package supervisor

import (
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
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

// A client that stops reading an event stream ends it when a write times
// out, so the handler and its subscription do not stay for the rest of the
// session.
func TestEventStreamsEndWhenTheClientStopsReading(t *testing.T) {
	prev := sseWriteTimeout
	sseWriteTimeout = 200 * time.Millisecond
	t.Cleanup(func() { sseWriteTimeout = prev })
	srv, sess := newTestSessionForCreate(t)
	for path, h := range map[string]http.HandlerFunc{
		"/api/v1/events": func(w http.ResponseWriter, r *http.Request) { srv.apiEvents(w, r, sess) },
		"/v1/events":     srv.handleEvents,
	} {
		done := make(chan struct{})
		go func() {
			h(&stalledWriter{}, httptest.NewRequest(http.MethodGet, path, nil))
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatalf("%s is still writing to a client that stopped reading", path)
		}
		srv.mu.Lock()
		n := len(srv.subscribers)
		srv.mu.Unlock()
		if n != 0 {
			t.Fatalf("%s: %d subscriber(s) left after the stream ended", path, n)
		}
	}
}
