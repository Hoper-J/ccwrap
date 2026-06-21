package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// sensitiveJSONFields is the (deliberately narrow) set of credential-bearing
// JSON keys whose VALUES must be masked before a captured body is spilled to
// disk or rendered in the inspect drawer. Lookup is case-insensitive; keys
// stored lowercase.
//
// The list is narrow on purpose. A broad list (e.g. "code", "token") would
// false-positive on tool params, model names, and ordinary fields in
// /v1/messages bodies. Combined with shouldRedactBody (which scopes
// redaction to OAuth hosts only) this gives a tight blast radius:
// redaction runs ONLY when both (host is OAuth) AND (field name is a known
// credential) are true.
//
// Sources of authority:
//   - OAuth refresh: claude-code/src/services/oauth/client.ts:146-167 sends
//     {grant_type, refresh_token, client_id, scope} — refresh_token is the
//     long-lived credential.
//   - PKCE: code_verifier appears in the same family of OAuth POSTs.
//   - access_token: appears in OAuth refresh RESPONSES; the Anthropic MITM
//     path now captures response bodies (responsetee.go), and the response
//     tap applies this same redaction on credential endpoints, so the token
//     is masked in the spill.
//   - client_secret: Claude Code uses public OAuth clients (no secret),
//     but the field is listed defensively.
var sensitiveJSONFields = map[string]bool{
	"refresh_token": true,
	"access_token":  true,
	"client_secret": true,
	"code_verifier": true,
}

// oauthHosts is the exact-host set whose request bodies are subject to
// JSON-field redaction. Scoped narrowly so /v1/messages bodies (which can
// contain ANY user content — code snippets, file contents, etc.) are
// untouched. Redaction keys on an exact-host match, never a suffix match.
var oauthHosts = map[string]bool{
	"platform.claude.com": true,
	"claude.com":          true,
	"claude.ai":           true,
}

// shouldRedactBody reports whether a captured request body should be walked
// for credential redaction. True when EITHER:
//   - host is a known exact OAuth host (platform.claude.com / claude.com /
//     claude.ai), or
//   - the request PATH is an OAuth/token endpoint (contains "/oauth/").
//
// The path clause exists because capture/MITM scope is broader than the
// OAuth host allowlist (*.anthropic.com is suffix-matched), so a
// relocated token endpoint on e.g. auth.anthropic.com would otherwise be
// captured + spilled WITHOUT redaction. Keying the extra trigger on the OAuth
// path (the request URL, NOT body content) catches a relocated endpoint on
// any MITM'd host while leaving /v1/messages untouched — its path never
// contains "/oauth/", so user content (which may mention credential-named
// fields in chat) is never mangled.
func shouldRedactBody(host, path string) bool {
	if oauthHosts[strings.ToLower(strings.TrimSpace(host))] {
		return true
	}
	return isOAuthCredentialPath(path)
}

// isOAuthCredentialPath reports whether path is an OAuth credential endpoint.
// Matches the "/oauth/" namespace (Claude Code's token URL is
// /v1/oauth/token; authorize is /cai/oauth/authorize). Substring match is
// safe: /v1/messages and /v1/messages/count_tokens never contain "/oauth/".
func isOAuthCredentialPath(path string) bool {
	return strings.Contains(strings.ToLower(path), "/oauth/")
}

// redactJSONBody walks the JSON tree of body, replacing values of credential-
// named fields with a SHA-prefixed sentinel. Returns the redacted body on
// success, or the original body unchanged if (a) the body does not parse as
// JSON (defensive: better to keep raw than emit truncated JSON), or
// (b) unmask is true (CCWRAP_UNMASK_CREDENTIALS=1 escape hatch — see
// Supervisor.unmaskCredentials).
//
// Sentinel format: `‹redacted by ccwrap; sha256:abcde…›` — the first 5 hex
// characters of the SHA256 digest of the original value. SHA256 is
// irreversible by design; 5 hex chars give 20 bits of distinguishability,
// enough to detect rotation between two redacted values (e.g., did the
// refresh_token rotate after the refresh exchange?) without leaking the
// value itself. The only theoretical attack — verifying a previously-stolen
// candidate token via SHA-prefix match — collapses because the attacker
// would already possess the candidate.
func redactJSONBody(body []byte, unmask bool) []byte {
	if unmask {
		return body
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		// Fail CLOSED. redactJSONBody is only ever called behind
		// shouldRedactBody (credential hosts), so a body that is not JSON
		// here is a credential-bearing body in another encoding (e.g.
		// form-urlencoded grant_type=refresh_token&refresh_token=SECRET).
		// Returning it raw would leak the credential into the on-disk spill
		// and inspect drawer; withhold it instead. The
		// upstream already received the original unmodified bytes — this only
		// affects ccwrap's internal observability surface.
		return redactFailClosedSentinel
	}
	walkRedact(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		return redactFailClosedSentinel
	}
	return out
}

// redactTelemetryBody masks credential-named JSON fields in a captured
// telemetry body, reusing sensitiveJSONFields / walkRedact / redactSentinel.
// Unlike redactJSONBody (which fails CLOSED for OAuth credential hosts), the
// telemetry MITM captures arbitrary third-party payloads for transparency, so
// a body that does not parse as JSON is returned UNCHANGED (fail OPEN) rather
// than withheld -- telemetry is not a credential host and showing the raw
// payload is the goal. Credential-named fields are still masked defensively
// when the body IS JSON. unmask (CCWRAP_UNMASK_CREDENTIALS=1) returns the body
// verbatim.
func redactTelemetryBody(body []byte, unmask bool) []byte {
	if unmask {
		return body
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body // fail open: telemetry transparency, not a credential host
	}
	walkRedact(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		return body // keep the original on re-marshal failure
	}
	return out
}

// redactFailClosedSentinel replaces a body that cannot be safely redacted
// (non-JSON on a credential host, or a re-marshal failure). See redactJSONBody.
var redactFailClosedSentinel = []byte("‹ccwrap: body on a credential host was not JSON; withheld from capture (fail-closed redaction)›")

// walkRedact mutates the decoded JSON tree in place. Strings whose parent
// key is in sensitiveJSONFields are replaced with the SHA-prefixed sentinel;
// all other map/array branches are traversed recursively. Non-string values
// at credential-named keys are left alone (e.g., `"refresh_token": null` or
// `"refresh_token": 0` — defensive; the real credential is always a string,
// but mangling non-strings could hide a bug).
func walkRedact(node any) {
	switch v := node.(type) {
	case map[string]any:
		for k, val := range v {
			if sensitiveJSONFields[strings.ToLower(k)] {
				if s, ok := val.(string); ok && s != "" {
					v[k] = redactSentinel(s)
				}
				continue
			}
			walkRedact(val)
		}
	case []any:
		for _, e := range v {
			walkRedact(e)
		}
	}
}

// redactSentinel builds the SHA-prefixed sentinel for a single credential
// value. The prefix is 5 hex chars (20 bits) — same value always produces
// the same prefix (rotation observability) but the value itself is
// cryptographically unrecoverable.
func redactSentinel(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return "‹redacted by ccwrap; sha256:" + hex.EncodeToString(sum[:])[:5] + "…›"
}
