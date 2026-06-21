package supervisor

import (
	"bytes"
	"io"
)

// responseBodyTee wraps an upstream response body so the bytes streamed to the
// client are ALSO copied aside — without buffering. Each Read copies only the
// chunk just read, so httputil.ReverseProxy keeps forwarding and flushing
// chunks immediately; there is no ReadAll-style stall that would collapse an
// SSE/streaming response into a single buffered blob.
//
// The retained copy is bounded by capBytes (<=0 means unbounded): once the cap
// is reached the tee stops accumulating but KEEPS streaming every byte to the
// client, so the cap never affects the forward — only the captured spill, which
// is then flagged truncated. onClose fires once with the accumulated bytes and
// that flag. ReverseProxy calls Close exactly once — after the body copy
// finishes (EOF) or the client disconnects — so the caller can spill there. The
// record that references the spill is written after ServeHTTP returns (i.e.
// after Close), so a ref set from onClose is visible to it.
//
// Single-goroutine use only (the ReverseProxy copy loop drives both Read and
// Close); no locking required.
type responseBodyTee struct {
	rc        io.ReadCloser
	buf       bytes.Buffer
	capBytes  int
	truncated bool
	sawEOF    bool
	onClose   func(buf []byte, truncated bool)
	done      bool
}

func newResponseBodyTee(rc io.ReadCloser, capBytes int, onClose func(buf []byte, truncated bool)) *responseBodyTee {
	return &responseBodyTee{rc: rc, capBytes: capBytes, onClose: onClose}
}

func (t *responseBodyTee) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		if t.capBytes > 0 && t.buf.Len()+n > t.capBytes {
			if room := t.capBytes - t.buf.Len(); room > 0 {
				t.buf.Write(p[:room])
			}
			t.truncated = true
		} else {
			t.buf.Write(p[:n])
		}
	}
	if err == io.EOF {
		t.sawEOF = true
	}
	return n, err
}

func (t *responseBodyTee) Close() error {
	err := t.rc.Close()
	if !t.done {
		t.done = true
		// Faithfulness: spill ONLY when the upstream body was read to
		// completion (clean io.EOF). A mid-stream read error, or a client-side
		// abort that stops the copy before EOF, leaves only a PARTIAL body —
		// spilling it as complete would lie in the inspector and orphan a file
		// on ReverseProxy's abort-panic path. Truncation (cap reached) is NOT
		// an abort: the stream still drains to EOF, so a capped body is spilled
		// with truncated=true. Mirrors the request-side "only record when the
		// full inbound body was read" rule.
		if t.onClose != nil && t.sawEOF && t.buf.Len() > 0 {
			t.onClose(t.buf.Bytes(), t.truncated)
		}
	}
	return err
}
