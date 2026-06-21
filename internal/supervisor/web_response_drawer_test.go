package supervisor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

// reconstructFn rebuilds a full `function NAME(params){…}` from the rendered
// inline script so a single helper can run under bare node (mirrors the
// per-name reconstruction in liftRenderedFns).
func reconstructFn(t *testing.T, js, name string) string {
	t.Helper()
	sig := "function " + name + "("
	i := strings.Index(js, sig)
	if i < 0 {
		t.Fatalf("function %s not found in inline script", name)
	}
	params := js[i+len(sig):]
	params = params[:strings.IndexByte(params, ')')]
	return "function " + name + "(" + params + ")" + scriptFnBody(t, js, name)
}

func inlineScript(t *testing.T) string {
	t.Helper()
	html := renderTestPage(t, webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Activity",
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(html)
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	return m[1]
}

// TestForwardedRowRendersResponseDrawer pins the SSR side: a forwarded-api
// record carrying a ResponseBodyRef exposes ResponseBodyRefID and renders
// EXACTLY one resp-drawer sub-detail (DOM byte-identical to respBodyDrawerEl),
// with the bytes never inlined. The fixture has NO request body — the response
// drawer must render independently of the request-body drawer.
func TestForwardedRowRendersResponseDrawer(t *testing.T) {
	rec := model.RequestRecord{
		Timestamp:       time.Now(),
		Method:          "POST",
		Path:            "/v1/messages",
		RequestHeaders:  http.Header{"Anthropic-Version": {"2023-06-01"}},
		ResponseBodyRef: &model.RequestBodyRef{ID: "aabbccddeeff0011"},
	}
	rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 50, false, false)
	var fapi *webRow
	for i := range rows {
		if rows[i].Class == "forwarded-api" {
			fapi = &rows[i]
		}
	}
	if fapi == nil {
		t.Fatalf("no forwarded-api row produced: %+v", rows)
	}
	if fapi.ResponseBodyRefID != "aabbccddeeff0011" {
		t.Fatalf("forwarded row must expose ResponseBodyRefID for lazy fetch, got %q", fapi.ResponseBodyRefID)
	}
	if fapi.BodyRefID != "" {
		t.Fatalf("this fixture has no request body; BodyRefID should be empty, got %q", fapi.BodyRefID)
	}

	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true, ActivityTitle: "Live activity",
		ActivityRows: rows,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	html := buf.String()

	if n := strings.Count(html, `<details class="req-sub body-drawer resp-drawer"`); n != 1 {
		t.Fatalf("forwarded row must render exactly 1 resp-drawer, got %d; html=%s", n, html)
	}
	const want = `<details class="req-sub body-drawer resp-drawer" data-reqid="aabbccddeeff0011"><summary>response body</summary><div class="sub-body body-panel">response body — loading…</div></details>`
	if !strings.Contains(html, want) {
		t.Fatalf("resp-drawer DOM mismatch.\nwant substring: %s\nhtml: %s", want, html)
	}
}

// TestForwardedResponseDrawerCSRWiring pins the live (SSE-appended) path mirrors
// the SSR side: activityRowData reads response_body_ref, makeRowEl appends
// respBodyDrawerEl, the toggle listener routes .resp-drawer to fetchResponseInto
// → renderResponseView, and renderResponseView/renderSSEView build via
// textContent only (no innerHTML).
func TestForwardedResponseDrawerCSRWiring(t *testing.T) {
	js := inlineScript(t)

	// (a) forwarded-api data builder copies response_body_ref → responseBodyRefId.
	mustContainAllWeb(t, js, "response_body_ref", "responseBodyRefId")

	// (b) makeRowEl appends the response drawer on forwarded rows.
	if mk := scriptFnBody(t, js, "makeRowEl"); !strings.Contains(mk, "respBodyDrawerEl(row)") {
		t.Fatalf("makeRowEl must append respBodyDrawerEl on forwarded rows")
	}

	// (c) respBodyDrawerEl builds .body-drawer.resp-drawer via textContent only.
	rb := scriptFnBody(t, js, "respBodyDrawerEl")
	mustContainAllWeb(t, rb, "req-sub body-drawer resp-drawer", "response body", "response body — loading…", "data-reqid", "responseBodyRefId")
	if strings.Contains(rb, ".innerHTML") {
		t.Fatalf("respBodyDrawerEl must be textContent/setAttribute only, never innerHTML")
	}

	// (d) the toggle listener dispatches resp-drawer to the SSE-aware fetcher,
	//     BEFORE the tele-drawer / request-body fall-through.
	if !strings.Contains(js, "d.classList.contains('resp-drawer')) { fetchResponseInto(panel, d.getAttribute('data-reqid')); return; }") {
		t.Fatalf("toggle listener must dispatch .resp-drawer to fetchResponseInto")
	}

	// (e) fetchResponseInto fetches /recent/body and routes to renderResponseView.
	mustContainAllWeb(t, scriptFnBody(t, js, "fetchResponseInto"), "'/recent/body?id='", "renderResponseView")

	// (f) renderResponseView routes SSE → renderSSEView, else renderJsonView.
	mustContainAllWeb(t, scriptFnBody(t, js, "renderResponseView"), "isSSEBody", "renderSSEView", "renderJsonView")

	// (g) renderSSEView paints via textContent only (no innerHTML), and the Raw
	//     SSE section offers a download (mirrors the request-body bv-dl link).
	sv := scriptFnBody(t, js, "renderSSEView")
	if strings.Contains(sv, ".innerHTML") {
		t.Fatalf("renderSSEView must build via textContent only, never innerHTML")
	}
	mustContainAllWeb(t, sv, "bv-dl", "data:text/plain;charset=utf-8,", "response.sse")
}

// TestReassembleSSEBehavioral runs the REAL parseSSE + reassembleSSE from the
// rendered script under node against a captured Anthropic message stream, and
// asserts the streamed text_delta chunks fold back into the final assistant
// text plus the message metadata (model / stop_reason / output tokens).
func TestReassembleSSEBehavioral(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	js := inlineScript(t)

	const driver = `
var SSE = [
 'event: message_start',
 'data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-opus-4-8","content":[],"stop_reason":null,"usage":{"input_tokens":42,"output_tokens":1}}}',
 '',
 'event: content_block_start',
 'data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}',
 '',
 'event: content_block_delta',
 'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}',
 '',
 'event: content_block_delta',
 'data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}',
 '',
 'event: content_block_stop',
 'data: {"type":"content_block_stop","index":0}',
 '',
 'event: message_delta',
 'data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":12}}',
 '',
 'event: message_stop',
 'data: {"type":"message_stop"}'
].join('\n');
process.stdout.write(JSON.stringify(reassembleSSE(SSE)));
`
	prog := reconstructFn(t, js, "parseSSE") + "\n" + reconstructFn(t, js, "reassembleSSE") + "\n" + driver
	cmd := exec.Command("node")
	cmd.Stdin = strings.NewReader(prog)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node run failed: %v\n%s", err, out)
	}
	var r struct {
		Text   string `json:"text"`
		Model  string `json:"model"`
		Stop   string `json:"stop"`
		OutTok int    `json:"outTok"`
		Events int    `json:"events"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("reassembleSSE output not JSON: %v\n%s", err, out)
	}
	if r.Text != "Hello" {
		t.Fatalf("reassembled assistant text = %q, want %q", r.Text, "Hello")
	}
	if r.Stop != "end_turn" {
		t.Fatalf("stop_reason = %q, want end_turn", r.Stop)
	}
	if r.Model != "claude-opus-4-8" {
		t.Fatalf("model = %q, want claude-opus-4-8", r.Model)
	}
	if r.OutTok != 12 {
		t.Fatalf("output tokens = %d, want 12", r.OutTok)
	}
	if r.Events != 7 {
		t.Fatalf("parsed SSE events = %d, want 7", r.Events)
	}
}

// TestIsSSEBodyBehavioral locks the routing predicate that decides SSE-reassembly
// vs JSON-pretty-print vs raw: only a real event-stream is SSE; a non-streaming
// Message, an OAuth body, and a ccwrap fail-closed sentinel are NOT.
func TestIsSSEBodyBehavioral(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	js := inlineScript(t)
	const driver = `
var cases = {
  json_message: '{"type":"message","role":"assistant","content":[{"type":"text","text":"hi"}]}',
  sse: 'event: message_start\ndata: {"type":"message_start"}\n\nevent: message_stop\ndata: {"type":"message_stop"}',
  sentinel: '‹ccwrap: body on a credential host was not JSON; withheld from capture (fail-closed redaction)›',
  oauth: '{"access_token":"x","token_type":"Bearer"}'
};
var out = {};
Object.keys(cases).forEach(function(k){ out[k] = isSSEBody(cases[k]); });
process.stdout.write(JSON.stringify(out));
`
	prog := reconstructFn(t, js, "isSSEBody") + "\n" + driver
	cmd := exec.Command("node")
	cmd.Stdin = strings.NewReader(prog)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("node run failed: %v\n%s", err, out)
	}
	var r struct {
		JSONMessage bool `json:"json_message"`
		SSE         bool `json:"sse"`
		Sentinel    bool `json:"sentinel"`
		OAuth       bool `json:"oauth"`
	}
	if err := json.Unmarshal(out, &r); err != nil {
		t.Fatalf("isSSEBody output not JSON: %v\n%s", err, out)
	}
	if !r.SSE {
		t.Fatalf("isSSEBody(event-stream) = false, want true")
	}
	if r.JSONMessage || r.OAuth || r.Sentinel {
		t.Fatalf("isSSEBody must be false for non-SSE bodies: message=%v oauth=%v sentinel=%v", r.JSONMessage, r.OAuth, r.Sentinel)
	}
}
