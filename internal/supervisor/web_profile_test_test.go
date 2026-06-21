package supervisor

import (
	"bytes"
	"net/http"
	"strings"
	"testing"
)

// TestWebInlineJS_HasPerRowTestButton.
//
// The profile-popover catalog must render a per-row [test] button next to
// the existing radio/name/host/meta cells. The click handler wires to
// onProfileTestClick, which runs the single-fetch state machine.
// ev.stopPropagation() prevents the click from cascading to the row's
// switch handler.
//
// We assert on the rendered HTML (which embeds the inline JS+CSS via
// webTpl) since the JS is not exported as a separate `inlineJS` const.
func TestWebInlineJS_HasPerRowTestButton(t *testing.T) {
	var buf bytes.Buffer
	// The inline script block (containing renderCatalog, onProfileTestClick,
	// etc.) is gated on {{if .LiveEnabled}} — set it true so the JS renders.
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	if !strings.Contains(js, "pop-test-btn") {
		t.Errorf("inline JS must contain a per-row 'pop-test-btn' class for the [test] button")
	}
	if !strings.Contains(js, "createElement('button')") {
		t.Errorf("expected a <button> DOM element for [test]")
	}
	if !strings.Contains(js, "onProfileTestClick") {
		t.Errorf("inline JS must wire onProfileTestClick handler")
	}
}

// TestWebInlineJS_HasTestAllFooter.
//
// The popover must render a footer at the bottom containing a divider line
// followed by a full-width [Test all] button. The click handler wires to
// onProfileTestAllClick, which runs the fan-out logic with an
// AbortController. The footer appears after all catalog rows (including
// the inherit-env row when present).
func TestWebInlineJS_HasTestAllFooter(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	if !strings.Contains(js, "pop-test-all-btn") {
		t.Errorf("inline JS must contain 'pop-test-all-btn' for footer button")
	}
	if !strings.Contains(js, "pop-footer-divider") {
		t.Errorf("inline JS must contain 'pop-footer-divider' before the [Test all] button")
	}
	if !strings.Contains(js, "onProfileTestAllClick") {
		t.Errorf("inline JS must wire onProfileTestAllClick handler")
	}
}

func TestWebInlineJS_TestFetchUsesPostJSON(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	if !strings.Contains(js, "fetch('/profile/test'") {
		t.Errorf("inline JS must fetch /profile/test")
	}
	idx := strings.Index(js, "fetch('/profile/test'")
	if idx < 0 {
		t.Fatal("no /profile/test fetch found")
	}
	// Inspect 500 chars after the fetch site for the required headers + body shape.
	end := idx + 500
	if end > len(js) {
		end = len(js)
	}
	tail := js[idx:end]
	if !strings.Contains(tail, "X-CCWRAP-Profile-Token") {
		t.Errorf("/profile/test fetch must include X-CCWRAP-Profile-Token header; got:\n%s", tail)
	}
	if !strings.Contains(tail, "method: 'POST'") {
		t.Errorf("/profile/test fetch must be POST; got:\n%s", tail)
	}
	if !strings.Contains(tail, "name:") {
		t.Errorf("/profile/test fetch must include name in body; got:\n%s", tail)
	}
}

func TestWebInlineJS_HasChipRenderer(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	if !strings.Contains(js, "pop-test-chip") {
		t.Errorf("inline JS must use 'pop-test-chip' class for result chip")
	}
	if !strings.Contains(js, "renderProfileTestChip") {
		t.Errorf("inline JS must define renderProfileTestChip function")
	}
}

func TestWebInlineJS_ChipPaletteCoversAllStatuses(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	required := []string{"OK", "SKIPPED", "AUTH_FAIL", "MODEL_404", "HTTP_4XX", "HTTP_5XX", "TIMEOUT", "NET_FAIL"}
	for _, status := range required {
		if !strings.Contains(js, `data-status="`+status+`"`) {
			t.Errorf("chip palette missing CSS rule for status %s", status)
		}
	}
}

func TestWebInlineJS_ChipTooltipUsesErrField(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	// The chip is icon-only; the tooltip incorporates the error/skipped
	// reason via concatenation instead of direct assignment. Assert the
	// SEMANTIC (error/skipped_reason flow into chip.title) not the exact
	// assignment syntax.
	if !strings.Contains(js, "String(result.error)") {
		t.Errorf("chip tooltip must source from result.error")
	}
	if !strings.Contains(js, "String(result.skipped_reason)") {
		t.Errorf("chip tooltip must source from result.skipped_reason for SKIPPED")
	}
	if !strings.Contains(js, "chip.title") {
		t.Errorf("chip.title must be set somewhere in renderProfileTestChip")
	}
}

func TestWebInlineJS_TestAllUsesParallelFetches(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	if !strings.Contains(js, "querySelectorAll('.sp3-pop-row')") {
		t.Errorf("test-all must enumerate .sp3-pop-row elements")
	}
	// The runner uses runPromisesWithLimit (bounded) rather than
	// Promise.all (unbounded). Either is "parallel" — verify a bounded
	// runner exists.
	if !strings.Contains(js, "runPromisesWithLimit") {
		t.Errorf("test-all must use runPromisesWithLimit for bounded parallel fetches")
	}
	if !strings.Contains(js, "testAllMaxConcurrency") {
		t.Errorf("test-all must declare a concurrency cap (testAllMaxConcurrency)")
	}
}

func TestWebInlineJS_TestAllUsesAbortController(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	if !strings.Contains(js, "testAllAbort") {
		t.Errorf("test-all must track its own AbortController via testAllAbort")
	}
}

// TestWebInlineJS_TestButtonAppliesToAllRowTypes.
//
// The named-profile row construction produces buttons for active and
// inactive named rows alike (the same code path). The footer [Test all]
// is a separate button.
//
// Verified via substring counts on the rendered inline JS. There is only
// one row type (named profile), so the test-button assertion collapses to
// "the per-row builder emits the button at least once".
//
//   - >= 2 createElement('button') sites for the per-row [test] +
//     [Test all] footer (renderCatalog also creates an unrelated
//     button elsewhere in the template; we use >= for safety).
//   - >= 1 onProfileTestClick(...) callsite — invocation from the
//     per-row [test] button (the definition uses 'function
//     onProfileTestClick(' and is subtracted out).
func TestWebInlineJS_TestButtonAppliesToAllRowTypes(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	btnCount := strings.Count(js, "createElement('button')")
	if btnCount < 2 {
		t.Errorf("expected at least 2 createElement('button') instances (per-row [test] + [Test all]); got %d", btnCount)
	}
	callTotal := strings.Count(js, "onProfileTestClick(")
	defCount := strings.Count(js, "function onProfileTestClick(")
	callSites := callTotal - defCount
	if callSites < 1 {
		t.Errorf("expected at least 1 onProfileTestClick(...) callsite; got %d (total=%d defs=%d)", callSites, callTotal, defCount)
	}
}

// TestWebInlineJS_4XXRendersHTTPChip.
//
// 4xx HTTP responses from /profile/test are pre-probe failures (CSRF, format
// validation, profile-not-found, inherit-env credentials missing). Their
// body is plain text from http.Error, not the standard JSON ProfileTestResult.
// The frontend must surface them as HTTP_4XX chips (with the body text in
// tooltip) — NOT as the catch-all NET_FAIL chip, which is reserved for actual
// network/transport failures.
//
// Verified by substring assertions on the rendered inline JS: the fetch chain
// must branch on !r.ok and synthesize an HTTP_4XX / HTTP_5XX chip result.
// Branching only on `r.status >= 400 && r.status < 500` would let 5xx fall
// through to r.json() — throwing SyntaxError on plain-text http.Error bodies
// and masking the actual server diagnostic.
func TestWebInlineJS_4XXRendersHTTPChip(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	// The fetch chain should treat any non-2xx as plain text.
	if !strings.Contains(js, "'HTTP_4XX'") {
		t.Errorf("4xx-class response should render as HTTP_4XX chip")
	}
	if !strings.Contains(js, "'HTTP_5XX'") {
		t.Errorf("5xx-class response should render as HTTP_5XX chip (Batch 3 fix)")
	}
	if !strings.Contains(js, "r.status >= 500") {
		t.Errorf("chip-status selector must distinguish 5xx from 4xx")
	}
	// The 4xx-only band must stay gone — it would re-introduce the 5xx
	// SyntaxError bug if it crept back in.
	if strings.Contains(js, "r.status < 500") {
		t.Errorf("the `r.status < 500` upper bound is the pre-Batch-3 bug shape — reverted regression?")
	}
}

// TestWebInlineJS_NoEnvHasCredentialsGate — the env_has_credentials gate
// is removed. The official profile (which replaced the inherit-env
// sentinel row) has Auth=nil, so probe.go SKIPs without hitting the
// network — no 4xx to avoid.
func TestWebInlineJS_NoEnvHasCredentialsGate(t *testing.T) {
	var buf bytes.Buffer
	renderWebPage(noopResponseWriter{header: http.Header{}, b: &buf}, webPageData{Title: "x", LiveEnabled: true})
	js := buf.String()
	if strings.Contains(js, "resp.env_has_credentials") {
		t.Errorf("inline JS must not gate on resp.env_has_credentials post-Slice #2")
	}
}
