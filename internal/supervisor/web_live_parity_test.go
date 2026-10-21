package supervisor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/ui"
)

func renderedInlineScript(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events", HeaderDenyList: ui.CredentialDenyList()}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("single bare <script> not found")
	}
	return m[1]
}

// liftScriptFns returns the named inline-script functions verbatim.
func liftScriptFns(t *testing.T, js string, names ...string) string {
	t.Helper()
	var b strings.Builder
	for _, name := range names {
		body := scriptFnBody(t, js, name)
		sig := "function " + name + "("
		i := strings.Index(js, sig)
		params := js[i+len(sig):]
		params = params[:strings.IndexByte(params, ')')]
		b.WriteString("function " + name + "(" + params + ")" + body + "\n")
	}
	return b.String()
}

func runNode(t *testing.T, script string) []byte {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, out)
	}
	return out
}

// The bootstrap survives non-ASCII text (CJK profile names, em dashes).
func TestBootstrapDecodesUTF8(t *testing.T) {
	b64 := bootstrapB64(pageBootstrap{EventsURL: "/events", ProfileToken: "默认 — profile → ✓"})
	quoted, _ := json.Marshal(b64)
	js := liftScriptFns(t, renderedInlineScript(t), "decodeBootstrap")
	out := runNode(t, js+"\nprocess.stdout.write(JSON.parse(decodeBootstrap("+string(quoted)+")).profile_token);")
	if string(out) != "默认 — profile → ✓" {
		t.Fatalf("decoded %q", out)
	}
}

// Error rows show the class, host and suggested action live, as on first
// paint.
func TestErrorRowLiveParity(t *testing.T) {
	recs := []model.ErrorRecord{
		{ErrorClass: "upstream_unreachable", UpstreamHost: "gw", SuggestedAction: "check upstream reachability", Summary: "x"},
		{Severity: "error", Summary: "y"},
		// A folded repeat shows its count.
		{ErrorClass: "upstream_http_404", Severity: "warn", UpstreamHost: "gw", SuggestedAction: "add an alias", Summary: "z", Count: 3},
	}
	wire, _ := json.Marshal(recs)
	js := liftScriptFns(t, renderedInlineScript(t), "joinParts", "activityRowData")
	out := runNode(t, js+"\nfunction formatClock(){return ''}\nprocess.stdout.write(JSON.stringify("+string(wire)+".map(function(r){var d=activityRowData(r,'error');return [d.main,d.right,d.label,d.severity];})));")
	var got [][4]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parse %s: %v", out, err)
	}
	rows := unifiedActivityRows(nil, recs, nil, 50, false, false)
	for i := range recs {
		first := [4]string{rows[i].Main, rows[i].Right, rows[i].Label, rows[i].Severity}
		if got[i] != first {
			t.Errorf("error %d: live %q, first paint %q", i, got[i], first)
		}
	}
}

// A warn-level entry reads "warning" and carries data-severity on first
// paint (the CSS colors it amber, errors red).
func TestWarnErrorRowFirstPaint(t *testing.T) {
	rows := unifiedActivityRows(nil, []model.ErrorRecord{{Severity: "warn", Summary: "w"}, {Severity: "error", Summary: "e"}}, nil, 50, false, false)
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", ActivityRows: rows})
	html := buf.String()
	if !regexp.MustCompile(`data-kind="error" data-class="error" data-severity="warn"[^>]*>.*?<div class="cell-label" title="warning">warning</div>`).MatchString(html) {
		t.Fatalf("warn row not rendered as a warning:\n%s", html)
	}
	if strings.Count(html, ` data-severity="`) != 1 || !strings.Contains(html, `data-kind="error" data-class="error"><span class="rowchev" aria-hidden="true"></span><div class="cell-time">`) {
		t.Fatal("only the warn row carries data-severity")
	}
	mustContainAllWeb(t, html, `<div class="cell-label" title="error">error</div>`)
	mustContainAllWeb(t, html, `.row[data-kind="error"] .cell-label{color:var(--danger)}`, `.row[data-kind="error"][data-severity="warn"] .cell-label{color:var(--warn)}`)
}

// Sessions on the event stream carry the server-rendered cell text.
func TestEventForWireAddsSessionDisplay(t *testing.T) {
	ev := eventForWire(model.Event{Type: "session_updated", Data: model.Session{RouteClass: model.RouteClassThirdPartyHidden, AuthPolicy: model.AuthPolicyCCWRAPOverrideFailClosed}})
	w, ok := ev.Data.(sessionForWire)
	if !ok || w.Display.RouteValue != "Gateway" || w.Display.AuthValue != "CCWRAP-owned · fail-closed" {
		t.Fatalf("event data = %#v", ev.Data)
	}
}
