package ui

import (
	"fmt"
	"net/http"
	"strings"
)

// MaskCredentialHeaders returns a COPY of h with every credential-class header
// value (per the shared deny-list — see ClassifyHeader) masked
// structure-preservingly: the auth scheme and a short leading prefix are kept,
// the rest of the secret is replaced with a length marker
// (e.g. "Bearer sk-ant-‹redacted 26 chars›"). Non-credential headers pass
// through verbatim; the input is never mutated.
//
// This is the WIRE masker. The supervisor applies it to a RequestRecord's
// headers BEFORE the record enters the activity ring, so the raw credential
// never reaches /recent, the page bootstrap, the SSE stream, a HAR export, or
// a spill file — masking is correct-by-construction at the single store-side
// convergence point, not a property each emit site must remember. Render-time
// redaction (RenderHeaderGroupsWithRedaction and the web.go JS twin) is now
// defense in depth on top of it. The only bypass is the launch-time
// CCWRAP_UNMASK_CREDENTIALS=1 opt-in, gated by the caller (the supervisor
// stores raw under that flag, and the ribbon shows a persistent UNMASKED
// marker while it is set).
//
// The masked form is deterministic across runs, so it does not pollute
// `ccwrap capture` version diffs. Shares ClassifyHeader's deny-list as the
// single source of truth — there is no second copy to drift.
func MaskCredentialHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		cp := make([]string, len(vs))
		copy(cp, vs)
		if ClassifyHeader(k) == HeaderCredential {
			for i := range cp {
				cp[i] = maskCredentialValue(cp[i])
			}
		}
		out[k] = cp
	}
	return out
}

// maskCredentialValue keeps an auth scheme (e.g. "Bearer ") and a short leading
// prefix of the secret (at most half its length, capped at 7 chars — e.g.
// "sk-ant-"), then replaces the rest with a length marker. The half-length cap
// ensures a short secret never leaks a majority of itself.
func maskCredentialValue(v string) string {
	if v == "" {
		return v
	}
	scheme := ""
	secret := v
	if sp := strings.IndexByte(v, ' '); sp >= 0 {
		scheme = v[:sp+1]
		secret = v[sp+1:]
	}
	keep := min(len(secret)/2, 7) // never expose a majority of a short secret
	prefix := secret[:keep]
	return fmt.Sprintf("%s%s‹redacted %d chars›", scheme, prefix, len(secret))
}
