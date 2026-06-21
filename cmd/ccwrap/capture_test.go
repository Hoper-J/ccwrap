package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/tlsfp"
)

func TestParseCaptureArgs_Defaults(t *testing.T) {
	got, err := parseCaptureArgs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Response || got.TLS || got.TLSOnly || got.Headers || got.Unmask {
		t.Fatalf("bad defaults: %+v", got)
	}
	if got.Path != "/v1/messages" || got.Host != "api.anthropic.com" {
		t.Fatalf("bad host/path defaults: %+v", got)
	}
	if got.Timeout != 30*time.Second {
		t.Fatalf("timeout=%v want 30s", got.Timeout)
	}
	if !reflect.DeepEqual(got.ClaudeArgs, []string{"-p", "hello"}) {
		t.Fatalf("default probe = %v want [-p hello]", got.ClaudeArgs)
	}
}

func TestParseCaptureArgs_FullExpands(t *testing.T) {
	got, err := parseCaptureArgs([]string{"--full"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.TLS || !got.ClientHello || !got.Headers {
		t.Fatalf("--full must set tls+clienthello+headers: %+v", got)
	}
	if !got.Response {
		t.Fatalf("--full keeps response on")
	}
}

func TestParseCaptureArgs_ClientHelloImpliesTLS(t *testing.T) {
	got, _ := parseCaptureArgs([]string{"--clienthello"})
	if !got.TLS {
		t.Fatalf("--clienthello must imply --with-tls")
	}
}

func TestParseCaptureArgs_NoResponse(t *testing.T) {
	got, _ := parseCaptureArgs([]string{"--no-response"})
	if got.Response {
		t.Fatalf("--no-response must turn response off")
	}
}

func TestParseCaptureArgs_TLSOnlyRejectsBodyFlags(t *testing.T) {
	for _, bad := range [][]string{
		{"--tls-only", "--headers"},
		{"--tls-only", "--full"},
		{"--tls-only", "--no-response"},
		{"--tls-only", "--unmask"},
		{"--tls-only", "--with-tls"},
	} {
		if _, err := parseCaptureArgs(bad); err == nil {
			t.Errorf("parseCaptureArgs(%v) must error", bad)
		}
	}
}

func TestParseCaptureArgs_AllRejected(t *testing.T) {
	if _, err := parseCaptureArgs([]string{"--all"}); err == nil {
		t.Fatalf("--all must be rejected as unimplemented, got nil error")
	}
}

func TestParseCaptureArgs_UserPromptOverridesProbe(t *testing.T) {
	got, _ := parseCaptureArgs([]string{"--", "-p", "do a thing"})
	if reflect.DeepEqual(got.ClaudeArgs, []string{"-p", "hello"}) {
		t.Fatalf("user args must override the default probe; got %v", got.ClaudeArgs)
	}
	if !reflect.DeepEqual(got.ClaudeArgs, []string{"-p", "do a thing"}) {
		t.Fatalf("claude args = %v", got.ClaudeArgs)
	}
}

// The struct-preserving credential masker moved to internal/ui
// (ui.MaskCredentialHeaders); its unit tests live in
// internal/ui/credmask_test.go. Capture no longer masks headers itself — it
// trusts the server-masked /recent wire (supervisor.recordRequest), so the
// emit-path coverage is now the supervisor wire test plus the ui masker tests.

func TestBuildCaptureResult_RequestPlusResponse(t *testing.T) {
	rec := model.RequestRecord{
		Method: "POST", LogicalTargetHost: "api.anthropic.com",
		Path: "/v1/messages", StatusCode: 200,
	}
	in := captureInputs{
		record:     rec,
		reqBody:    []byte(`{"model":"claude-x","max_tokens":1}`),
		respBody:   []byte("event: message_start\n\n"),
		respStatus: 200,
	}
	res := buildCaptureResult(captureOpts{Response: true}, in)

	if res.Request == nil || res.Request.BodyEncoding != "json" {
		t.Fatalf("request body_encoding=json expected: %+v", res.Request)
	}
	if res.Response == nil || res.Response.BodyEncoding != "sse" {
		t.Fatalf("response body_encoding=sse expected: %+v", res.Response)
	}
	if res.TLS != nil {
		t.Fatalf("tls must be nil without --with-tls")
	}
	b, _ := json.Marshal(res.Request.Body)
	if string(b) != `{"max_tokens":1,"model":"claude-x"}` {
		t.Fatalf("request body not parsed as JSON: %s", b)
	}
}

func TestBuildCaptureResult_NoResponse(t *testing.T) {
	in := captureInputs{record: model.RequestRecord{Path: "/v1/messages"}, reqBody: []byte("{}")}
	res := buildCaptureResult(captureOpts{Response: false}, in)
	if res.Response != nil {
		t.Fatalf("--no-response must omit response")
	}
}

func TestBuildCaptureResult_TLSOnly(t *testing.T) {
	in := captureInputs{tls: &tlsfp.Result{JA3: "x", JA4: "y", Peetprint: "z"}}
	res := buildCaptureResult(captureOpts{TLSOnly: true}, in)
	if res.Request != nil || res.Response != nil || res.TLS == nil {
		t.Fatalf("--tls-only emits only the tls block: %+v", res)
	}
}

func TestBuildCaptureResult_RawBodyEncoding(t *testing.T) {
	in := captureInputs{record: model.RequestRecord{Path: "/v1/messages"}, reqBody: []byte("not json{")}
	res := buildCaptureResult(captureOpts{Response: false}, in)
	if res.Request.BodyEncoding != "raw" {
		t.Fatalf("non-JSON body must be body_encoding=raw, got %s", res.Request.BodyEncoding)
	}
	if res.Request.Body.(string) != "not json{" {
		t.Fatalf("raw body must be the string")
	}
}

func TestBuildCaptureResult_401Note(t *testing.T) {
	in := captureInputs{
		record:     model.RequestRecord{Path: "/v1/messages", StatusCode: 401},
		reqBody:    []byte("{}"),
		respBody:   []byte(`{"error":"x"}`),
		respStatus: 401,
	}
	res := buildCaptureResult(captureOpts{Response: true}, in)
	joined := ""
	for _, n := range res.Meta.Notes {
		joined += n
	}
	if !strings.Contains(joined, "401") {
		t.Fatalf("401 must produce a meta.note: %v", res.Meta.Notes)
	}
}

func TestFetchRecentMatch_FindsExactPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": []model.RequestRecord{
			{Path: "/v1/messages/count_tokens", StatusCode: 200, BodyRef: &model.RequestBodyRef{ID: "ct"}},
			{Path: "/v1/messages", StatusCode: 200,
				BodyRef:         &model.RequestBodyRef{ID: "req1"},
				ResponseBodyRef: &model.RequestBodyRef{ID: "resp1"}},
		}})
	}))
	defer srv.Close()

	rec, ok := fetchRecentMatch(srv.URL, captureOpts{Response: true, Path: "/v1/messages", Host: ""})
	if !ok {
		t.Fatal("expected a match")
	}
	if rec.Path != "/v1/messages" || rec.BodyRef.ID != "req1" {
		t.Fatalf("matched the wrong record (prefix bug?): %+v", rec)
	}
}

func TestFetchRecentMatch_IgnoresQueryString(t *testing.T) {
	// A real Claude request is POST /v1/messages?beta=true; the default matcher
	// path is /v1/messages. The query string must be ignored (else capture hangs
	// "no request to .../v1/messages"), while /v1/messages/count_tokens — a
	// different path segment — must still NOT match.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": []model.RequestRecord{
			{Path: "/v1/messages/count_tokens", StatusCode: 200, BodyRef: &model.RequestBodyRef{ID: "ct"}},
			{Path: "/v1/messages?beta=true", StatusCode: 200,
				BodyRef:         &model.RequestBodyRef{ID: "req1"},
				ResponseBodyRef: &model.RequestBodyRef{ID: "resp1"}},
		}})
	}))
	defer srv.Close()

	rec, ok := fetchRecentMatch(srv.URL, captureOpts{Response: true, Path: "/v1/messages", Host: ""})
	if !ok {
		t.Fatal("expected /v1/messages?beta=true to match /v1/messages")
	}
	if rec.Path != "/v1/messages?beta=true" || rec.BodyRef.ID != "req1" {
		t.Fatalf("matched the wrong record (query-string or prefix bug?): %+v", rec)
	}
}

func TestFetchRecentMatch_MainInferenceSkipsQuota(t *testing.T) {
	// Claude Code fires a lightweight quota/title call (no tools) before the real
	// agent inference (carries the tool definitions). With MainInference set,
	// capture must skip the quota call and pick the tools-bearing request.
	bodies := map[string]string{
		"quota": `{"model":"claude-3-5-haiku","messages":[{"role":"user","content":"q"}]}`,
		"main":  `{"model":"claude-opus-4-8","system":[{"type":"text","text":"x"}],"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"ping"}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/recent/body" {
			_, _ = w.Write([]byte(bodies[r.URL.Query().Get("id")]))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": []model.RequestRecord{
			{Path: "/v1/messages", StatusCode: 200, BodyRef: &model.RequestBodyRef{ID: "quota"}, ResponseBodyRef: &model.RequestBodyRef{ID: "quota"}},
			{Path: "/v1/messages", StatusCode: 200, BodyRef: &model.RequestBodyRef{ID: "main"}, ResponseBodyRef: &model.RequestBodyRef{ID: "main"}},
		}})
	}))
	defer srv.Close()

	rec, ok := fetchRecentMatch(srv.URL, captureOpts{Response: true, Path: "/v1/messages", MainInference: true})
	if !ok {
		t.Fatal("expected the tools-bearing main inference to match")
	}
	if rec.BodyRef.ID != "main" {
		t.Fatalf("MainInference must skip the quota call and pick the tools-bearing record, got %q", rec.BodyRef.ID)
	}

	// Without MainInference, the first record (quota) is returned as before.
	rec2, ok2 := fetchRecentMatch(srv.URL, captureOpts{Response: true, Path: "/v1/messages"})
	if !ok2 || rec2.BodyRef.ID != "quota" {
		t.Fatalf("default behavior must return the first match (quota), got ok=%v rec=%+v", ok2, rec2)
	}
}

func TestBodyIsMainInference(t *testing.T) {
	// The real agent inference carries tools AND is not the startup "Warmup"
	// probe (whose final user content block is the literal text "Warmup", the
	// same slot the -p prompt occupies). Content is a JSON string or an array of
	// {type,text} blocks; the prompt/Warmup is always the trailing text block.
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"warmup as last array block", `{"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"Warmup"}]}]}`, false},
		{"warmup as bare string", `{"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"Warmup"}]}`, false},
		{"warmup with surrounding whitespace", `{"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":[{"type":"text","text":" Warmup\n"}]}]}`, false},
		{"real prompt", `{"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":[{"type":"text","text":"sys"},{"type":"text","text":"Reply with the single word: ping"}]}]}`, true},
		{"no tools is not main inference", `{"messages":[{"role":"user","content":"Warmup"}]}`, false},
		{"prompt merely mentions warmup", `{"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"Run the Warmup script"}]}`, true},
		{"trailing assistant turn ignored, last user is warmup", `{"tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"Warmup"},{"role":"assistant","content":"ok"}]}`, false},
	}
	for _, c := range cases {
		if got := bodyIsMainInference([]byte(c.body)); got != c.want {
			t.Errorf("%s: bodyIsMainInference=%v want %v", c.name, got, c.want)
		}
	}
}

func TestFetchRecentMatch_MainInferenceSkipsWarmup(t *testing.T) {
	// Claude Code fires a tools-bearing "Warmup" inference on startup, before the
	// prompt's real inference. Both carry tools, so the tools filter alone cannot
	// tell them apart; MainInference must additionally skip the warmup probe and
	// hold out for the request that answers the -p prompt.
	bodies := map[string]string{
		"warmup": `{"model":"claude-opus-4-8","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"Warmup"}]}]}`,
		"main":   `{"model":"claude-opus-4-8","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>ctx</system-reminder>"},{"type":"text","text":"Reply with the single word: ping"}]}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/recent/body" {
			_, _ = w.Write([]byte(bodies[r.URL.Query().Get("id")]))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": []model.RequestRecord{
			{Path: "/v1/messages", StatusCode: 200, BodyRef: &model.RequestBodyRef{ID: "warmup"}, ResponseBodyRef: &model.RequestBodyRef{ID: "warmup"}},
			{Path: "/v1/messages", StatusCode: 200, BodyRef: &model.RequestBodyRef{ID: "main"}, ResponseBodyRef: &model.RequestBodyRef{ID: "main"}},
		}})
	}))
	defer srv.Close()

	rec, ok := fetchRecentMatch(srv.URL, captureOpts{Response: true, Path: "/v1/messages", MainInference: true})
	if !ok || rec.BodyRef.ID != "main" {
		t.Fatalf("MainInference must skip the warmup probe and pick the prompt inference, got ok=%v id=%q", ok, rec.BodyRef.ID)
	}

	// Without MainInference, the first record (warmup) is returned unchanged.
	rec2, ok2 := fetchRecentMatch(srv.URL, captureOpts{Response: true, Path: "/v1/messages"})
	if !ok2 || rec2.BodyRef.ID != "warmup" {
		t.Fatalf("default behavior must return the first match (warmup), got ok=%v rec=%+v", ok2, rec2)
	}
}

func TestFetchRecentMatch_SyntheticWakes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"requests": []model.RequestRecord{
			{Path: "/v1/messages", StatusCode: 502, Synthetic: true},
		}})
	}))
	defer srv.Close()
	rec, ok := fetchRecentMatch(srv.URL, captureOpts{Response: true, Path: "/v1/messages"})
	if !ok || !rec.Synthetic {
		t.Fatalf("synthetic 502 must satisfy the matcher, got ok=%v rec=%+v", ok, rec)
	}
}

func TestFetchBody_RetriesOnMiss(t *testing.T) {
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		if n < 2 {
			http.Error(w, "not available", http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	b, err := fetchBody(srv.URL, "id1", 5, 5*time.Millisecond)
	if err != nil || string(b) != `{"ok":true}` {
		t.Fatalf("retry failed: b=%s err=%v", b, err)
	}
}

// TestBuildCaptureResult_EmitsHeadersVerbatim pins that capture no longer masks
// headers itself: the record carries whatever /recent served — already masked
// server-side by supervisor.recordRequest, or raw when this capture launched
// its child with CCWRAP_UNMASK_CREDENTIALS=1 (--unmask). buildCaptureResult
// emits it verbatim. Critically, an already server-masked value must NOT be
// masked a second time (double-masking would corrupt the length marker). The
// masking guarantee itself lives in the ui masker tests + the supervisor wire
// test; capture's job is faithful pass-through of the wire it polled.
func TestBuildCaptureResult_EmitsHeadersVerbatim(t *testing.T) {
	hdr := http.Header{}
	// Exactly what the server-masked /recent wire carries for a credential.
	const wireAuth = "Bearer sk-ant-‹redacted 26 chars›"
	hdr.Set("Authorization", wireAuth)
	hdr.Set("Content-Type", "application/json")

	rec := model.RequestRecord{
		Method: "POST", LogicalTargetHost: "api.anthropic.com",
		Path: "/v1/messages", RequestHeaders: hdr,
	}
	res := buildCaptureResult(
		captureOpts{Headers: true, Response: false},
		captureInputs{record: rec, reqBody: []byte("{}")},
	)
	if res.Request == nil || res.Request.Headers == nil {
		t.Fatalf("expected request headers in the emitted result: %+v", res.Request)
	}
	if got := res.Request.Headers.Get("Authorization"); got != wireAuth {
		t.Fatalf("capture must emit the wire value verbatim (no double-masking); got %q want %q", got, wireAuth)
	}
	if ct := res.Request.Headers.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("benign Content-Type must pass through unchanged, got %q", ct)
	}
}

func TestParseCaptureArgs_TLSOnlyRejectsMainInference(t *testing.T) {
	if _, err := parseCaptureArgs([]string{"--tls-only", "--main-inference"}); err == nil {
		t.Fatal("--tls-only --main-inference must be rejected (no request record is matched)")
	}
}

// TestBuildCaptureResult_MainInferenceFallbackNote — the give-up path (and a
// synthetic record) bypasses the --main-inference filter by design; the
// degradation must be visible in meta.notes so a differ can't mistake a
// quota/title/Warmup exchange for the real inference.
func TestBuildCaptureResult_MainInferenceFallbackNote(t *testing.T) {
	quota := []byte(`{"model":"claude-3-5-haiku","messages":[{"role":"user","content":"q"}]}`)
	main := []byte(`{"model":"claude-opus-4-8","tools":[{"name":"Bash"}],"messages":[{"role":"user","content":"ping"}]}`)
	noteSubstr := "NOT the main agent inference"

	hasNote := func(res captureResult) bool {
		for _, n := range res.Meta.Notes {
			if strings.Contains(n, noteSubstr) {
				return true
			}
		}
		return false
	}

	degraded := buildCaptureResult(captureOpts{Response: true, MainInference: true},
		captureInputs{record: model.RequestRecord{}, reqBody: quota, respAbsent: true})
	if !hasNote(degraded) {
		t.Errorf("tool-less body under --main-inference must carry the fallback note; notes=%v", degraded.Meta.Notes)
	}

	clean := buildCaptureResult(captureOpts{Response: true, MainInference: true},
		captureInputs{record: model.RequestRecord{}, reqBody: main, respAbsent: true})
	if hasNote(clean) {
		t.Errorf("real main inference must NOT carry the fallback note; notes=%v", clean.Meta.Notes)
	}

	off := buildCaptureResult(captureOpts{Response: true},
		captureInputs{record: model.RequestRecord{}, reqBody: quota, respAbsent: true})
	if hasNote(off) {
		t.Errorf("without --main-inference the note must never appear; notes=%v", off.Meta.Notes)
	}
}

// TestStrictMatcherFor_MemoizesBodyVerdicts — the polling loop re-evaluates
// every candidate each 100ms tick; a spilled body is immutable, so its
// not-main verdict must be fetched ONCE per capture run, not per tick.
func TestStrictMatcherFor_MemoizesBodyVerdicts(t *testing.T) {
	var bodyFetches atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/recent/body" {
			bodyFetches.Add(1)
			_, _ = w.Write([]byte(`{"model":"claude-3-5-haiku","messages":[{"role":"user","content":"q"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	match := strictMatcherFor(srv.URL, captureOpts{Response: true, Path: "/v1/messages", MainInference: true})
	rec := model.RequestRecord{Path: "/v1/messages", StatusCode: 200,
		BodyRef:         &model.RequestBodyRef{ID: "quota"},
		ResponseBodyRef: &model.RequestBodyRef{ID: "resp"}}
	for i := 0; i < 5; i++ {
		if match(rec, captureOpts{Response: true, Path: "/v1/messages", MainInference: true}) {
			t.Fatal("quota record must not match under --main-inference")
		}
	}
	if got := bodyFetches.Load(); got != 1 {
		t.Fatalf("body fetched %d times across 5 polls; memo must hold it to 1", got)
	}
}
