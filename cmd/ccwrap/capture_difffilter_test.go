package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"testing"
)

// diffCapture builds a capture document the way `ccwrap capture --full` emits
// it (canonicalized request header names); run varies the per-run fields.
func diffCapture(run, model string) map[string]any {
	return map[string]any{
		"meta": map[string]any{"claude_bin": "/x/" + run, "schema_version": 1},
		"tls":  map[string]any{"ja3": "j3", "ja4": "j4", "peetprint": "p", "clienthello_hex": "1603" + run},
		"request": map[string]any{
			"method": "POST", "host": "api.anthropic.com", "path": "/v1/messages",
			"headers": map[string][]string{
				"Anthropic-Version":        {"2023-06-01"},
				"Content-Length":           {"10" + run},
				"X-Client-Request-Id":      {"req-" + run},
				"X-Claude-Code-Session-Id": {"sess-" + run},
				"X-Claude-Code-Prompt-Id":  {"prompt-" + run},
			},
			"body": map[string]any{
				"model":    model,
				"metadata": map[string]any{"user_id": `{"session_id":"` + run + `"}`},
				"system": []any{
					map[string]any{"type": "text", "text": "x-anthropic-billing-header: cc_version=2.1.296.a" + run + "; cc_entrypoint=cli; cch=c" + run + "; cc_prompt_id=" + run + ";"},
					map[string]any{"type": "text", "text": "You are an agent."},
				},
			},
			"body_encoding": "json",
		},
		"response": map[string]any{"status": 200, "body": "reply " + run, "body_encoding": "sse"},
	}
}

func applyDiffFilter(t *testing.T, doc map[string]any) string {
	t.Helper()
	in, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("jq", "-S", captureDiffFilter)
	cmd.Stdin = bytes.NewReader(in)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("jq rejected the diff filter: %v\n%s", err, out)
	}
	return string(out)
}

// The printed filter must reduce two captures of the same Claude Code to the
// same document — header names compared case-insensitively, as capture emits
// them canonicalized — while a real change (here: the model) survives.
func TestCaptureDiffFilterStripsPerRunNoise(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	a, b := applyDiffFilter(t, diffCapture("1", "claude-x")), applyDiffFilter(t, diffCapture("2", "claude-x"))
	if a != b {
		t.Fatalf("per-run noise survived the filter:\n--- run 1\n%s\n--- run 2\n%s", a, b)
	}
	if c := applyDiffFilter(t, diffCapture("1", "claude-y")); c == a {
		t.Fatal("the filter also erased a real difference (the model)")
	}
	// --tls-only output has no request or response block.
	_ = applyDiffFilter(t, map[string]any{"meta": map[string]any{}, "tls": map[string]any{"ja3": "j3"}})
}
