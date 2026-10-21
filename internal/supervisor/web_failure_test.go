package supervisor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

func failedUnmappedRecord() model.RequestRecord {
	return model.RequestRecord{
		Method: "POST", Path: "/v1/messages?beta=true", StatusCode: 404, LatencyMS: 12,
		StreamState: model.StreamStateHTTP, ActualUpstreamHost: "gw.example",
		RequestHeaders: http.Header{"Anthropic-Version": []string{"2023-06-01"}},
		Model:          "claude-fable-5-1", UpstreamModel: "claude-fable-5-1", ModelUnmapped: true,
		Failure: &model.RequestFailure{
			Source: "upstream", Type: "not_found_error", Message: "model not found: claude-fable-5-1",
			Body: `{"error":"<script>alert(1)</script>"}`, Hint: "map it",
		},
	}
}

// The first paint shows the model on the row, the failure reason in the
// details, and the failure panel (escaped) in the drawer.
func TestRequestRowFirstPaintShowsModelAndFailure(t *testing.T) {
	rec := failedUnmappedRecord()
	rows := unifiedActivityRows([]model.RequestRecord{rec}, nil, nil, 50, false, false)
	if len(rows) != 1 {
		t.Fatalf("rows = %d", len(rows))
	}
	row := rows[0]
	// ?beta=true is dropped from the displayed path.
	if row.Main != "POST /v1/messages · claude-fable-5-1 (no alias)" {
		t.Fatalf("Main = %q", row.Main)
	}
	if row.Right != "404 model not found: claude-fable-5-1 · 12 ms · http · gw.example" {
		t.Fatalf("Right = %q", row.Right)
	}
	if row.Failure == nil || row.Failure.Head != "upstream returned 404 · not_found_error" {
		t.Fatalf("Failure = %+v", row.Failure)
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", ActivityRows: rows})
	html := buf.String()
	mustContainAllWeb(t, html,
		`<div class="req-fail" data-source="upstream"><div class="rf-head">upstream returned 404 · not_found_error</div>`,
		`<div class="rf-msg">model not found: claude-fable-5-1</div><div class="rf-hint">map it</div>`,
		`<details class="req-sub rf-raw"><summary>error body</summary>`,
		`&lt;script&gt;alert(1)&lt;/script&gt;`)
	if strings.Contains(html, "<script>alert(1)") {
		t.Fatal("error body must be escaped")
	}

	errRows := unifiedActivityRows(nil, []model.ErrorRecord{{Summary: "boom", Count: 3}}, nil, 50, false, false)
	if errRows[0].Main != "boom ×3" {
		t.Fatalf("error Main = %q", errRows[0].Main)
	}
}

// The live rows label models and failures exactly like the first paint,
// and count the same requests as failed.
func TestRequestLabelsLiveParity(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	long := strings.Repeat("é", 70)
	recs := []model.RequestRecord{
		{Method: "POST", Path: "/v1/messages", StatusCode: 200, Model: "claude-opus-5-5", UpstreamModel: "gpt-6"},
		{Method: "POST", Path: "/v1/messages?beta=true", StatusCode: 200, Model: "claude-opus-5-5", UpstreamModel: "claude-opus-5-5"},
		failedUnmappedRecord(),
		{Method: "POST", Path: "/v1/messages/", StatusCode: 502, Failure: &model.RequestFailure{Source: "ccwrap", Message: long}},
		{Method: "POST", Path: "/v1/messages/count_tokens", StatusCode: 404, Failure: &model.RequestFailure{Source: "upstream"}},
		{Method: "GET", Path: "/v1/messages", StatusCode: 500},
		{Method: "POST", Path: "/v1/messages", StatusCode: 404, Synthetic: true},
	}
	recs = append(recs, model.RequestRecord{Method: "GET", Path: "/v1/models?beta=true&limit=20", StatusCode: 200})
	type labels struct {
		Model  string `json:"model"`
		Head   string `json:"head"`
		Brief  string `json:"brief"`
		Failed bool   `json:"failed"`
		Path   string `json:"path"`
	}
	want := make([]labels, len(recs))
	for i, r := range recs {
		want[i] = labels{ui.RequestModelLabel(r), ui.RequestFailureHead(r), ui.RequestFailureBrief(r, failureBriefMax), r.FailedModelRequest(), ui.DisplayPath(r.Path)}
	}
	wire, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	js := liftScriptFns(t, renderedInlineScript(t), "requestModelLabel", "failureHead", "failureBrief", "isFailedModelRequest", "displayPath")
	driver := js + `
var recs = ` + string(wire) + `;
process.stdout.write(JSON.stringify(recs.map(function(r){
  return {model: requestModelLabel(r), head: failureHead(r), brief: failureBrief(r, ` + "60" + `), failed: isFailedModelRequest(r), path: displayPath(r.path)};
})));`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	var got []labels
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse %s: %v", out, err)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("record %d: live %+v, first paint %+v", i, got[i], want[i])
		}
	}
}

// failurePanelEl builds the same panel as the Go template.
func TestFailurePanelLiveMatchesTemplate(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	js := liftScriptFns(t, renderedInlineScript(t), "failurePanelEl")
	driver := domShimJS + "\n" + js + `
var el = failurePanelEl({failure: {source: 'upstream', head: 'upstream returned 404 · not_found_error', message: 'model not found', hint: 'map it', body: '{"e":1}', truncated: true}});
function walk(n){ return {cls: n.className, src: n.getAttribute('data-source'), text: n.children.length ? null : n.textContent, kids: n.children.map(walk)}; }
process.stdout.write(JSON.stringify([walk(el), failurePanelEl({}) === null]));`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	const want = `[{"cls":"req-fail","src":"upstream","text":null,"kids":[` +
		`{"cls":"rf-head","src":null,"text":"upstream returned 404 · not_found_error","kids":[]},` +
		`{"cls":"rf-msg","src":null,"text":"model not found","kids":[]},` +
		`{"cls":"rf-hint","src":null,"text":"map it","kids":[]},` +
		`{"cls":"req-sub rf-raw","src":null,"text":null,"kids":[` +
		`{"cls":"","src":null,"text":"error body (truncated)","kids":[]},` +
		`{"cls":"sub-body","src":null,"text":null,"kids":[{"cls":"rf-pre","src":null,"text":"{\"e\":1}","kids":[]}]}]}]},true]`
	if string(out) != want {
		t.Fatalf("live panel:\n got %s\nwant %s", out, want)
	}
	// The template renders the same structure.
	rows := []webRow{{Main: "x", Class: "forwarded-api", HeaderGroups: []webHeaderGroup{{Name: "g"}}, Failure: &webFailure{Source: "upstream", Head: "upstream returned 404 · not_found_error", Message: "model not found", Hint: "map it", Body: `{"e":1}`, Truncated: true}}}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", ActivityRows: rows})
	mustContainAllWeb(t, buf.String(), `<div class="req-fail" data-source="upstream"><div class="rf-head">upstream returned 404 · not_found_error</div><div class="rf-msg">model not found</div><div class="rf-hint">map it</div><details class="req-sub rf-raw"><summary>error body (truncated)</summary><div class="sub-body"><pre class="rf-pre">{&#34;e&#34;:1}</pre></div></details></div>`)
}
