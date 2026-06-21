package supervisor

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"strings"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// bodyDecodeFailedSentinel is spilled in place of a response body that could
// not be decoded (unsupported/unknown Content-Encoding, or a corrupt/truncated
// compressed stream). Observability over silence: the inspector shows the body
// existed but was unreadable, and the raw compressed bytes never hit disk.
var bodyDecodeFailedSentinel = []byte("‹ccwrap: response body could not be decoded (unsupported or truncated Content-Encoding); withheld from capture›")

// decodeCapturedBody decompresses a captured response body according to its
// Content-Encoding, so the on-disk spill / inspector shows readable bytes
// WITHOUT ccwrap having to alter the upstream request (no Accept-Encoding
// rewrite → full request fidelity to Anthropic). The client still receives the
// original compressed stream; only this captured COPY is decoded.
//
// Output is bounded to max bytes (decompression-bomb guard): decoding beyond max
// stops and returns truncated=true. Returns ok=false when the body cannot be
// decoded — an unsupported/unknown encoding, or a corrupt/truncated compressed
// stream — so the caller withholds it rather than spilling unreadable bytes
// (never returns the raw compressed bytes on failure). An empty/identity
// encoding passes through unchanged (only length-capped).
func decodeCapturedBody(buf []byte, encoding string, max int) (decoded []byte, truncated bool, ok bool) {
	enc := strings.ToLower(strings.TrimSpace(encoding))
	if enc == "" || enc == "identity" {
		if max > 0 && len(buf) > max {
			return buf[:max], true, true
		}
		return buf, false, true
	}

	var r io.Reader
	switch enc {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(buf))
		if err != nil {
			return nil, false, false
		}
		defer zr.Close()
		r = zr
	case "br":
		r = brotli.NewReader(bytes.NewReader(buf))
	case "zstd":
		zr, err := zstd.NewReader(bytes.NewReader(buf))
		if err != nil {
			return nil, false, false
		}
		defer zr.Close()
		r = zr
	case "deflate":
		zr, err := zlib.NewReader(bytes.NewReader(buf))
		if err != nil {
			return nil, false, false
		}
		defer zr.Close()
		r = zr
	default:
		return nil, false, false
	}

	var lim io.Reader = r
	if max > 0 {
		lim = io.LimitReader(r, int64(max)+1)
	}
	out, err := io.ReadAll(lim)
	if err != nil {
		return nil, false, false // corrupt / truncated compressed stream
	}
	if max > 0 && len(out) > max {
		return out[:max], true, true
	}
	return out, false, true
}
