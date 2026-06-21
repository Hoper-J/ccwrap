package supervisor

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// oneShotReader returns its whole payload plus a terminal error in a single
// Read, modelling an upstream that errors mid-stream (err != io.EOF) or
// completes cleanly (err == io.EOF).
type oneShotReader struct {
	data string
	err  error
	done bool
}

func (r *oneShotReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, r.err
	}
	r.done = true
	n := copy(p, r.data)
	if n < len(r.data) {
		// Caller's buffer was smaller than our payload; deliver the rest on
		// the next Read, error after.
		r.data = r.data[n:]
		r.done = false
		return n, nil
	}
	return n, r.err
}

func (r *oneShotReader) Close() error { return nil }

// A mid-stream read error means the tee holds only a PARTIAL body. It must NOT
// be spilled as if complete — that would lie in the inspector and (on the
// abort-panic path) orphan a file. Capture is withheld entirely.
func TestResponseBodyTeeSkipsSpillOnMidStreamError(t *testing.T) {
	boom := errors.New("upstream exploded")
	var spilled bool
	tee := newResponseBodyTee(&oneShotReader{data: "Hel", err: boom}, 0, func([]byte, bool) {
		spilled = true
	})
	// Drive Read like ReverseProxy's copy loop, then Close.
	_, err := io.Copy(io.Discard, tee)
	if !errors.Is(err, boom) {
		t.Fatalf("io.Copy err = %v, want the upstream error", err)
	}
	_ = tee.Close()
	if spilled {
		t.Fatal("partial body captured before a mid-stream error must NOT be spilled as complete")
	}
}

// A clean EOF means the full body was read — it MUST be spilled.
func TestResponseBodyTeeSpillsOnCleanEOF(t *testing.T) {
	var got []byte
	var gotTruncated bool
	tee := newResponseBodyTee(io.NopCloser(strings.NewReader("Hello")), 0, func(b []byte, truncated bool) {
		got = append([]byte(nil), b...)
		gotTruncated = truncated
	})
	if _, err := io.Copy(io.Discard, tee); err != nil {
		t.Fatalf("io.Copy err = %v, want nil", err)
	}
	_ = tee.Close()
	if string(got) != "Hello" {
		t.Fatalf("spilled body = %q, want %q", got, "Hello")
	}
	if gotTruncated {
		t.Fatal("clean full body must not be flagged truncated")
	}
}
