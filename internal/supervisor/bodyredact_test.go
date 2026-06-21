package supervisor

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestShouldRedactBody(t *testing.T) {
	cases := []struct {
		host string
		path string
		want bool
	}{
		// Known OAuth hosts: redact regardless of path.
		{"platform.claude.com", "/v1/oauth/token", true},
		{"claude.com", "/cai/oauth/authorize", true},
		{"claude.ai", "/anything", true},
		{"PLATFORM.CLAUDE.COM", "/x", true},     // case-insensitive host
		{"  platform.claude.com  ", "/x", true}, // whitespace-trimmed host
		// api.anthropic.com /v1/messages — user content, must NOT redact.
		{"api.anthropic.com", "/v1/messages", false},
		{"mcp-proxy.anthropic.com", "/v1/messages", false},
		{"code.claude.com", "/docs", false},
		{"docs.claude.com", "/docs", false},
		{"random.example.com", "/x", false},
		{"", "/x", false},
		// A relocated OAuth/token endpoint on a *.anthropic.com
		// subdomain (or any MITM'd host) must be redacted by PATH even though the
		// host is not in the exact OAuth allowlist. /v1/messages never matches.
		{"auth.anthropic.com", "/v1/oauth/token", true},
		{"api.anthropic.com", "/v1/oauth/token", true},
		{"some-gateway.example.com", "/oauth/token", true},
		{"api.anthropic.com", "/v1/messages/count_tokens", false},
	}
	for _, tc := range cases {
		if got := shouldRedactBody(tc.host, tc.path); got != tc.want {
			t.Errorf("shouldRedactBody(%q, %q) = %v, want %v", tc.host, tc.path, got, tc.want)
		}
	}
}

func TestRedactJSONBody_TopLevelRefreshToken(t *testing.T) {
	body := []byte(`{"grant_type":"refresh_token","refresh_token":"sk-ant-ort01-AbCdEf","client_id":"9d1c"}`)
	out := redactJSONBody(body, false)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("redacted body must remain valid JSON: %v\nbody: %s", err, out)
	}
	rt, _ := parsed["refresh_token"].(string)
	if !strings.HasPrefix(rt, "‹redacted by ccwrap; sha256:") {
		t.Errorf("refresh_token not redacted: %q", rt)
	}
	if strings.Contains(string(out), "sk-ant-ort01-AbCdEf") {
		t.Errorf("plaintext credential leaked through redaction: %s", out)
	}
	// Non-sensitive fields preserved verbatim.
	if parsed["grant_type"] != "refresh_token" {
		t.Errorf("grant_type value lost: %v", parsed["grant_type"])
	}
	if parsed["client_id"] != "9d1c" {
		t.Errorf("client_id lost: %v", parsed["client_id"])
	}
}

func TestRedactJSONBody_AllSensitiveKeys(t *testing.T) {
	body := []byte(`{
		"refresh_token": "rt-secret",
		"access_token":  "at-secret",
		"client_secret": "cs-secret",
		"code_verifier": "cv-secret",
		"scope":         "user:inference"
	}`)
	out := redactJSONBody(body, false)
	str := string(out)
	for _, leaked := range []string{"rt-secret", "at-secret", "cs-secret", "cv-secret"} {
		if strings.Contains(str, leaked) {
			t.Errorf("credential %q leaked: %s", leaked, str)
		}
	}
	if !strings.Contains(str, "user:inference") {
		t.Errorf("non-sensitive scope value lost: %s", str)
	}
}

func TestRedactJSONBody_CaseInsensitiveFieldName(t *testing.T) {
	body := []byte(`{"Refresh_Token":"AbCdEf","REFRESH_TOKEN":"GhIjKl"}`)
	out := redactJSONBody(body, false)
	if strings.Contains(string(out), "AbCdEf") || strings.Contains(string(out), "GhIjKl") {
		t.Errorf("case-variant credential keys must still redact: %s", out)
	}
}

func TestRedactJSONBody_NestedField(t *testing.T) {
	body := []byte(`{"outer":{"refresh_token":"nested-secret","other":"keep"}}`)
	out := redactJSONBody(body, false)
	if strings.Contains(string(out), "nested-secret") {
		t.Errorf("nested credential not redacted: %s", out)
	}
	if !strings.Contains(string(out), "keep") {
		t.Errorf("sibling non-sensitive value lost: %s", out)
	}
}

func TestRedactJSONBody_ArrayOfObjects(t *testing.T) {
	body := []byte(`{"items":[{"refresh_token":"a"},{"refresh_token":"b"}]}`)
	out := redactJSONBody(body, false)
	if strings.Contains(string(out), `"a"`) || strings.Contains(string(out), `"b"`) {
		t.Errorf("array-element credentials must redact: %s", out)
	}
}

// TestRedactJSONBody_NonJSON_FailsClosed — redactJSONBody
// is only ever called behind shouldRedactBody (credential hosts). A body that
// is not parseable JSON (e.g. form-urlencoded grant_type=refresh_token&
// refresh_token=SECRET) must NOT be spilled raw — that would leak the
// credential into the on-disk spill + inspect drawer. Fail CLOSED: withhold
// the body. The upstream still receives the original bytes; only ccwrap's
// observability surface is affected.
func TestRedactJSONBody_NonJSON_FailsClosed(t *testing.T) {
	secret := "sk-ant-ort01-SUPERSECRET"
	body := []byte("grant_type=refresh_token&refresh_token=" + secret)
	out := redactJSONBody(body, false)
	if strings.Contains(string(out), secret) {
		t.Errorf("non-JSON credential body leaked the secret into the spill: %s", out)
	}
	if string(out) == string(body) {
		t.Errorf("non-JSON body must be withheld, not passed through raw")
	}
}

// Unmask bypass still returns the raw non-JSON body (explicit operator opt-out).
func TestRedactJSONBody_NonJSON_UnmaskReturnsRaw(t *testing.T) {
	body := []byte("grant_type=refresh_token&refresh_token=SECRET")
	out := redactJSONBody(body, true)
	if string(out) != string(body) {
		t.Errorf("unmask=true must return raw body even when non-JSON: got %s", out)
	}
}

func TestRedactJSONBody_EmptyAndNilValues(t *testing.T) {
	// Empty-string credential value: leave as empty rather than emit a
	// sentinel for "" (no rotation observability there anyway). Null/numeric
	// values at sensitive keys are unusual but must not mangle.
	body := []byte(`{"refresh_token":"","access_token":null,"code_verifier":0}`)
	out := redactJSONBody(body, false)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("output must be valid JSON: %v", err)
	}
	if parsed["refresh_token"] != "" {
		t.Errorf("empty refresh_token mangled: %v", parsed["refresh_token"])
	}
	if parsed["access_token"] != nil {
		t.Errorf("null access_token mangled: %v", parsed["access_token"])
	}
}

func TestRedactJSONBody_UnmaskBypass(t *testing.T) {
	body := []byte(`{"refresh_token":"sk-ant-ort01-AbCdEf"}`)
	out := redactJSONBody(body, true) // unmask = true
	if string(out) != string(body) {
		t.Errorf("unmask=true must return body unchanged: got %s, want %s", out, body)
	}
}

// TestRedactTelemetryBody — telemetry redaction masks credential-named JSON
// fields like redactJSONBody, but FAILS OPEN on non-JSON: telemetry captures
// arbitrary third-party payloads for transparency, not credential hosts, so a
// non-JSON body must be returned UNCHANGED rather than withheld.
func TestRedactTelemetryBody(t *testing.T) {
	// JSON with a credential field → value masked, sibling untouched, valid JSON.
	t.Run("masks credential field, keeps the rest", func(t *testing.T) {
		body := []byte(`{"access_token":"supersecret","event":"login"}`)
		out := redactTelemetryBody(body, false)
		var parsed map[string]any
		if err := json.Unmarshal(out, &parsed); err != nil {
			t.Fatalf("output must be valid JSON: %v\nbody: %s", err, out)
		}
		if got, want := parsed["access_token"], redactSentinel("supersecret"); got != want {
			t.Errorf("access_token = %v, want sentinel %q", got, want)
		}
		if strings.Contains(string(out), "supersecret") {
			t.Errorf("plaintext credential leaked: %s", out)
		}
		if parsed["event"] != "login" {
			t.Errorf("non-sensitive event field mangled: %v", parsed["event"])
		}
	})

	// Non-JSON body → returned UNCHANGED (fail OPEN — the KEY difference from
	// redactJSONBody, which withholds it).
	t.Run("fails open on non-JSON: returns input unchanged", func(t *testing.T) {
		for _, body := range [][]byte{
			[]byte("not json at all"),
			[]byte("grant_type=refresh_token&refresh_token=SECRET"),
		} {
			out := redactTelemetryBody(body, false)
			if string(out) != string(body) {
				t.Errorf("non-JSON telemetry body must pass through unchanged: got %q, want %q", out, body)
			}
		}
	})

	// unmask=true → body returned verbatim, even with a credential field.
	t.Run("unmask returns body verbatim", func(t *testing.T) {
		body := []byte(`{"access_token":"supersecret","event":"login"}`)
		out := redactTelemetryBody(body, true)
		if string(out) != string(body) {
			t.Errorf("unmask=true must return body unchanged: got %s, want %s", out, body)
		}
	})

	// Nested credential field → masked (walkRedact recurses).
	t.Run("masks nested credential field", func(t *testing.T) {
		body := []byte(`{"meta":{"refresh_token":"abc"}}`)
		out := redactTelemetryBody(body, false)
		if strings.Contains(string(out), `"abc"`) {
			t.Errorf("nested credential not redacted: %s", out)
		}
	})
}

func TestRedactSentinel_DeterministicAndDistinguishing(t *testing.T) {
	// Same input → same sentinel (rotation observability).
	s1 := redactSentinel("abc")
	s2 := redactSentinel("abc")
	if s1 != s2 {
		t.Errorf("same input produced different sentinels: %q vs %q", s1, s2)
	}
	// Different input → different sentinel (basically; collision odds ~1/1M).
	s3 := redactSentinel("def")
	if s1 == s3 {
		t.Errorf("different inputs produced identical sentinel: %q", s1)
	}
	// Format check.
	if !strings.HasPrefix(s1, "‹redacted by ccwrap; sha256:") || !strings.HasSuffix(s1, "…›") {
		t.Errorf("sentinel format off: %q", s1)
	}
	// SHA prefix is irreversible — no way the original value appears in the
	// sentinel. Defensive check.
	if strings.Contains(s1, "abc") {
		t.Errorf("sentinel must not contain the plaintext: %q", s1)
	}
}

func TestRedactJSONBody_NonSensitiveFieldUntouched(t *testing.T) {
	// /v1/messages-style body with a tool-arg field named coincidentally
	// like a credential field but not in our narrow set — must be left alone.
	body := []byte(`{"messages":[{"role":"user","content":"my refresh_token is in the keychain"}]}`)
	out := redactJSONBody(body, false)
	if !strings.Contains(string(out), "my refresh_token is in the keychain") {
		t.Errorf("substring 'refresh_token' inside string value must NOT redact: %s", out)
	}
}
