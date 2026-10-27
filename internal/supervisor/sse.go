package supervisor

import (
	"io"
	"net/http"
	"time"
)

// sseWriter sends event-stream frames under a write deadline, so a client
// that stops reading ends the stream instead of holding the handler and its
// subscription for the rest of the session.
type sseWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func newSSEWriter(w http.ResponseWriter) *sseWriter {
	return &sseWriter{w: w, rc: http.NewResponseController(w)}
}

// send writes one frame under a fresh deadline and flushes it; false means
// the client is gone or stopped reading.
func (s *sseWriter) send(write func(io.Writer)) bool {
	_ = s.rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
	write(s.w)
	return s.rc.Flush() == nil
}

// done clears the deadline, so a connection the client goes on to reuse is
// not cut by it.
func (s *sseWriter) done() { _ = s.rc.SetWriteDeadline(time.Time{}) }
