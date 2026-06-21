package supervisor

import (
	"bytes"
	"compress/gzip"
	"strings"
	"testing"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

func gzipBytes(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func brotliBytes(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w := brotli.NewWriter(&b)
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func zstdBytes(t *testing.T, s string) []byte {
	t.Helper()
	var b bytes.Buffer
	w, err := zstd.NewWriter(&b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestDecodeCapturedBody(t *testing.T) {
	const payload = `{"msg":"Hello, body capture"}`

	t.Run("identity passthrough", func(t *testing.T) {
		got, trunc, ok := decodeCapturedBody([]byte(payload), "", 1<<20)
		if !ok || trunc || string(got) != payload {
			t.Fatalf("identity: got=%q trunc=%v ok=%v", got, trunc, ok)
		}
	})

	t.Run("gzip roundtrip", func(t *testing.T) {
		got, trunc, ok := decodeCapturedBody(gzipBytes(t, payload), "gzip", 1<<20)
		if !ok || trunc || string(got) != payload {
			t.Fatalf("gzip: got=%q trunc=%v ok=%v", got, trunc, ok)
		}
	})

	t.Run("brotli roundtrip", func(t *testing.T) {
		got, trunc, ok := decodeCapturedBody(brotliBytes(t, payload), "br", 1<<20)
		if !ok || trunc || string(got) != payload {
			t.Fatalf("br: got=%q trunc=%v ok=%v", got, trunc, ok)
		}
	})

	t.Run("zstd roundtrip", func(t *testing.T) {
		got, trunc, ok := decodeCapturedBody(zstdBytes(t, payload), "zstd", 1<<20)
		if !ok || trunc || string(got) != payload {
			t.Fatalf("zstd: got=%q trunc=%v ok=%v", got, trunc, ok)
		}
	})

	t.Run("corrupt gzip fails (ok=false), never raw bytes", func(t *testing.T) {
		got, _, ok := decodeCapturedBody([]byte("\x1f\x8b\x08garbage-not-gzip"), "gzip", 1<<20)
		if ok || got != nil {
			t.Fatalf("corrupt gzip must fail: got=%q ok=%v", got, ok)
		}
	})

	t.Run("unknown encoding fails", func(t *testing.T) {
		got, _, ok := decodeCapturedBody([]byte(payload), "snappy", 1<<20)
		if ok || got != nil {
			t.Fatalf("unknown encoding must fail: got=%q ok=%v", got, ok)
		}
	})

	t.Run("decompression bomb bounded + flagged truncated", func(t *testing.T) {
		big := strings.Repeat("A", 4096)
		got, trunc, ok := decodeCapturedBody(gzipBytes(t, big), "gzip", 100)
		if !ok || !trunc {
			t.Fatalf("oversized decode must be ok+truncated: trunc=%v ok=%v", trunc, ok)
		}
		if len(got) != 100 {
			t.Fatalf("decoded output must be capped at 100, got %d", len(got))
		}
	})
}
