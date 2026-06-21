package supervisor

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// TestWebEgressTestScriptSymbols smokes the inline JS to make sure the
// new popover egress-test functions are referenced. Full DOM behavior
// is exercised by manual / Playwright runs; this guards against the
// script block being accidentally truncated or imports drifting.
//
// Mirrors the rendering harness used by web_profile_test_test.go: the
// inline script is gated on {{if .LiveEnabled}}, so we set it true.
func TestWebEgressTestScriptSymbols(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	html := buf.String()
	required := []string{
		// Popover edit-panel button (T12) — tests draft egress:
		"appendEgressTestButton",
		"onEgressTestClick",
		"renderEgressTestResult",
		"/profile/test-egress",
		"egress_override",
		// Posture-cell enhancement (T13 D′) — tests active session live:
		"enhancePostureEgressCell",
		"renderEgressCellResult",
		"<active-session>",
		"egress-probe-btn",
	}
	for _, sym := range required {
		if !strings.Contains(html, sym) {
			t.Errorf("dashboard HTML missing symbol %q", sym)
		}
	}
}

// TestSP3InlineScript_CountryFlag runs the ACTUAL rendered countryFlag helper
// via node (pure function, no DOM). A 2-letter ISO country code is prefixed
// with its regional-indicator flag emoji ("US" -> "🇺🇸 US"); anything that is
// not exactly two A-Z letters passes through unchanged (graceful degrade).
func TestSP3InlineScript_CountryFlag(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	m := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(m) != 2 {
		t.Fatalf("inline script not found")
	}
	body := scriptFnBody(t, m[1], "countryFlag")
	driver := "function countryFlag(code)" + body + `
var cases = [
  {in:'US', want:'🇺🇸 US'},
  {in:'DE', want:'🇩🇪 DE'},
  {in:'us', want:'🇺🇸 US'},
  {in:'USA', want:'USA'},
  {in:'United States', want:'United States'},
  {in:'', want:''},
  {in:'1A', want:'1A'},
];
var out = cases.map(function(c){ return {in:c.in, got:countryFlag(c.in), want:c.want}; });
process.stdout.write(JSON.stringify(out));
`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v\n%s", err, out)
	}
	var res []struct {
		In   string `json:"in"`
		Got  string `json:"got"`
		Want string `json:"want"`
	}
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		t.Fatalf("parse node output: %v\nraw: %s", jerr, out)
	}
	for _, c := range res {
		if c.Got != c.Want {
			t.Errorf("countryFlag(%q) = %q, want %q", c.In, c.Got, c.Want)
		}
	}
}

// TestSP3InlineScript_EgressModeHintAndValidity runs the ACTUAL rendered
// egressModeHint + egressRowValid helpers. egressModeHint makes "inherit"
// self-explanatory (the egress analog of the old "inline" jargon) with one
// plain line per mode, keeping the glued mode <select> narrow. egressRowValid
// reports whether a proxy mode (http/socks5/socks5h) has the url it requires;
// inherit/direct need none. egressRowValid calls egressIsProxyMode, so the
// driver lifts that self-contained predicate too (DRY, not re-inlined).
func TestSP3InlineScript_EgressModeHintAndValidity(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{
		Title: "x", LiveEnabled: true,
		BootstrapB64: bootstrapB64(pageBootstrap{EventsURL: "/events"}),
	})
	js := regexp.MustCompile(`(?s)<script>(.*)</script>`).FindStringSubmatch(buf.String())
	if len(js) != 2 {
		t.Fatalf("inline script not found")
	}
	driver := "function egressIsProxyMode(mode)" + scriptFnBody(t, js[1], "egressIsProxyMode") +
		"function egressRowValid(mode, url)" + scriptFnBody(t, js[1], "egressRowValid") +
		"function egressModeHint(mode)" + scriptFnBody(t, js[1], "egressModeHint") + `
var out = {
  hint: { inherit: egressModeHint('inherit'), direct: egressModeHint('direct'),
    http: egressModeHint('http'), socks5: egressModeHint('socks5'),
    socks5h: egressModeHint('socks5h'), blank: egressModeHint('') },
  valid: { inheritEmpty: egressRowValid('inherit',''), directEmpty: egressRowValid('direct',''),
    httpEmpty: egressRowValid('http',''), httpUrl: egressRowValid('http','http://p:8080'),
    socks5hEmpty: egressRowValid('socks5h',''), socks5hUrl: egressRowValid('socks5h','socks5h://p:1080'),
    httpWhitespace: egressRowValid('http','   ') }
};
process.stdout.write(JSON.stringify(out));
`
	cmd := exec.Command("node", "-")
	cmd.Stdin = strings.NewReader(driver)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node driver failed: %v\n%s", err, out)
	}
	var res struct {
		Hint  map[string]string `json:"hint"`
		Valid map[string]bool   `json:"valid"`
	}
	if jerr := json.Unmarshal(out, &res); jerr != nil {
		t.Fatalf("parse node output: %v\nraw: %s", jerr, out)
	}
	// "inherit" must be self-explanatory (no bare jargon); each mode distinct.
	if !strings.Contains(res.Hint["inherit"], "session") {
		t.Errorf("inherit hint must explain it follows the session egress, got %q", res.Hint["inherit"])
	}
	if !strings.Contains(res.Hint["direct"], "no proxy") {
		t.Errorf("direct hint wrong: %q", res.Hint["direct"])
	}
	if !strings.Contains(res.Hint["http"], "HTTP") {
		t.Errorf("http hint wrong: %q", res.Hint["http"])
	}
	if !strings.Contains(res.Hint["socks5"], "locally") {
		t.Errorf("socks5 hint wrong: %q", res.Hint["socks5"])
	}
	if !strings.Contains(res.Hint["socks5h"], "at the proxy") {
		t.Errorf("socks5h hint wrong: %q", res.Hint["socks5h"])
	}
	if res.Hint["blank"] != "" {
		t.Errorf("unknown-mode hint must be empty, got %q", res.Hint["blank"])
	}
	// validity: proxy modes require a non-empty url; inherit/direct don't.
	want := map[string]bool{
		"inheritEmpty": true, "directEmpty": true,
		"httpEmpty": false, "httpUrl": true,
		"socks5hEmpty": false, "socks5hUrl": true, "httpWhitespace": false,
	}
	for k, w := range want {
		if res.Valid[k] != w {
			t.Errorf("egressRowValid %s = %v, want %v", k, res.Valid[k], w)
		}
	}
}
