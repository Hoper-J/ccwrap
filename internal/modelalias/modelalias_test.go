package modelalias

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestRewriteRequest_PreservesFirstPartyOnlyBodyFields is the direct guard for
// ccwrap's headline value proposition: the richer first-party request body
// (which Claude Code only emits because ccwrap keeps the first-party gates
// green) must reach the gateway with ONLY the top-level model rewritten. This
// pins that the 1P-only fields — cache_control {scope:"global"} and a tool's
// eager_input_streaming — survive an alias-HIT rewrite byte-for-byte, not just
// the no-match passthrough path. A regression in RewriteJSONRequestBody that
// incidentally dropped or altered one of these fields would fail here.
func TestRewriteRequest_PreservesFirstPartyOnlyBodyFields(t *testing.T) {
	cfg, err := New(map[string]string{"claude-sonnet-4-6": "gateway/sonnet"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	// A first-party-shaped /v1/messages body carrying the rich fields ccwrap
	// claims to let flow through unchanged.
	body := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,` +
		`"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral","scope":"global"}}]}],` +
		`"tools":[{"name":"Read","input_schema":{"type":"object"},"eager_input_streaming":true}],` +
		`"anthropic_beta":["context-management-2025-06-27"]}`)

	out, ctx, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ctx == nil || !ctx.Rewritten {
		t.Fatal("expected an alias hit (ctx.Rewritten), so this exercises the re-marshal path, not the byte-identical no-match path")
	}

	var in, got map[string]json.RawMessage
	if err := json.Unmarshal(body, &in); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("rewritten body is not valid JSON: %v", err)
	}
	// The model — and only the model — is rewritten.
	if string(got["model"]) != `"gateway/sonnet"` {
		t.Errorf("model = %s, want \"gateway/sonnet\"", got["model"])
	}
	// Every other top-level field must be byte-identical RawMessage, proving the
	// nested cache_control scope:global, the tool's eager_input_streaming, and
	// anthropic_beta all flow through untouched.
	for k, v := range in {
		if k == "model" {
			continue
		}
		if !bytes.Equal(v, got[k]) {
			t.Errorf("rewrite mutated first-party body field %q\n  was: %s\n  now: %s", k, v, got[k])
		}
	}
}

func TestRewriteMessagesRequestTopLevelModelOnly(t *testing.T) {
	cfg, err := New(map[string]string{"claude-sonnet-4-6": "gateway/sonnet-4.6-prod"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":[{"type":"tool_result","content":{"model":"do-not-touch"}}]}]}`)
	out, ctx, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.Rewritten || !ctx.NormalizeResponse || ctx.LogicalModel != "claude-sonnet-4-6" || ctx.UpstreamModel != "gateway/sonnet-4.6-prod" {
		t.Fatalf("unexpected context: %#v", ctx)
	}
	if !bytes.Contains(out, []byte(`"model":"gateway/sonnet-4.6-prod"`)) {
		t.Fatalf("top-level model not rewritten: %s", out)
	}
	if !bytes.Contains(out, []byte(`"model":"do-not-touch"`)) {
		t.Fatalf("nested payload model was changed or lost: %s", out)
	}
}

func TestRewriteCountTokensRequest(t *testing.T) {
	cfg, err := New(map[string]string{"claude-haiku-4-5": "gateway/haiku"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	out, ctx, err := RewriteJSONRequestBody("/v1/messages/count_tokens", []byte(`{"model":"claude-haiku-4-5","messages":[]}`), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.Rewritten || !ctx.NormalizeResponse || !bytes.Contains(out, []byte(`"model":"gateway/haiku"`)) {
		t.Fatalf("expected count_tokens model rewrite, ctx=%#v body=%s", ctx, out)
	}
}

func TestStrictRejectsProviderSpecificRequestModel(t *testing.T) {
	cfg, err := New(nil, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = RewriteJSONRequestBody("/v1/messages", []byte(`{"model":"gateway/sonnet","messages":[]}`), cfg)
	if err == nil {
		t.Fatal("expected provider-specific model to be rejected in strict mode")
	}
}

func TestRewriteJSONResponseRestoresLogicalModel(t *testing.T) {
	ctx := &Context{Rewritten: true, Reverse: map[string]string{"gateway/sonnet": "claude-sonnet-4-6"}}
	out, err := RewriteJSONResponseBody([]byte(`{"type":"message","model":"gateway/sonnet","content":[]}`), ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"model":"claude-sonnet-4-6"`)) {
		t.Fatalf("expected logical model in response: %s", out)
	}
}

func TestRewriteSSEResponseRestoresMessageStartModel(t *testing.T) {
	ctx := &Context{Rewritten: true, Reverse: map[string]string{"gateway/sonnet": "claude-sonnet-4-6"}}
	body := "event: message_start\n" + `data: {"type":"message_start","message":{"model":"gateway/sonnet"}}` + "\n\n" + `data: {"type":"content_block_delta","delta":{"text":"gateway/sonnet"}}` + "\n"
	rc := WrapSSEResponse(io.NopCloser(bytes.NewReader([]byte(body))), ctx)
	out, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"model":"claude-sonnet-4-6"`)) {
		t.Fatalf("expected logical model in SSE: %s", out)
	}
	if !bytes.Contains(out, []byte(`"text":"gateway/sonnet"`)) {
		t.Fatalf("content delta text should not be rewritten: %s", out)
	}
}

func TestRewriteRequestRejectsCompressedBody(t *testing.T) {
	cfg, err := New(map[string]string{"claude-sonnet-4-6": "gateway/sonnet"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader([]byte(`{"model":"claude-sonnet-4-6"}`)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Encoding", "gzip")
	if _, err := RewriteRequest(req, cfg); err == nil {
		t.Fatal("expected compressed request body to be rejected")
	}
}

func TestNormalizeClaudeModelArgsReverseAlias(t *testing.T) {
	cfg, err := New(map[string]string{"claude-sonnet-4-6": "gateway/sonnet"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	out, normalized, unresolved := NormalizeClaudeModelArgs([]string{"--model", "gateway/sonnet", "-p", "hello"}, cfg)
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved models: %#v", unresolved)
	}
	if len(normalized) != 1 || normalized[0].LogicalModel != "claude-sonnet-4-6" {
		t.Fatalf("unexpected normalizations: %#v", normalized)
	}
	want := []string{"--model", "claude-sonnet-4-6", "-p", "hello"}
	for i := range want {
		if out[i] != want[i] {
			t.Fatalf("args = %#v, want %#v", out, want)
		}
	}
}

func TestNormalizeClaudeModelArgsPassthroughAllowsUnresolvedProviderModel(t *testing.T) {
	cfg, err := New(nil, "test", false)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ProviderModelPassthrough = true
	out, normalized, unresolved := NormalizeClaudeModelArgs([]string{"--model=gateway/sonnet", "-p", "hello"}, cfg)
	if len(normalized) != 0 || len(unresolved) != 0 {
		t.Fatalf("expected passthrough without normalization/unresolved, normalized=%#v unresolved=%#v", normalized, unresolved)
	}
	if out[0] != "--model=gateway/sonnet" {
		t.Fatalf("expected provider model to pass through, got %#v", out)
	}
}

func TestNormalizeModelEnvReverseAlias(t *testing.T) {
	cfg, err := New(map[string]string{"claude-haiku-4-5": "gateway/haiku"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	out, normalized, unresolved := NormalizeModelEnv(map[string]string{"CLAUDE_CODE_SUBAGENT_MODEL": "gateway/haiku"}, cfg, func(key string) bool { return key == "CLAUDE_CODE_SUBAGENT_MODEL" })
	if len(unresolved) != 0 {
		t.Fatalf("unexpected unresolved env: %#v", unresolved)
	}
	if out["CLAUDE_CODE_SUBAGENT_MODEL"] != "claude-haiku-4-5" || len(normalized) != 1 {
		t.Fatalf("unexpected env normalization: out=%#v normalized=%#v", out, normalized)
	}
}

func TestRewriteMessageBatchCreateModels(t *testing.T) {
	cfg, err := New(map[string]string{"claude-sonnet-4-6": "gateway/sonnet", "claude-haiku-4-5": "gateway/haiku"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"requests":[{"custom_id":"a","params":{"model":"claude-sonnet-4-6","messages":[]}},{"custom_id":"b","params":{"model":"claude-haiku-4-5","metadata":{"model":"do-not-touch"}}}]}`)
	out, ctx, err := RewriteJSONRequestBody("/v1/messages/batches", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.Rewritten || !ctx.NormalizeResponse || len(ctx.Reverse) != 2 {
		t.Fatalf("expected batch rewrite context, got %#v", ctx)
	}
	if !bytes.Contains(out, []byte(`"model":"gateway/sonnet"`)) || !bytes.Contains(out, []byte(`"model":"gateway/haiku"`)) {
		t.Fatalf("expected provider models in batch request: %s", out)
	}
	if !bytes.Contains(out, []byte(`"model":"do-not-touch"`)) {
		t.Fatalf("nested payload model was changed or lost: %s", out)
	}
}

func TestRewriteMessageBatchCreateRejectsProviderSpecificModel(t *testing.T) {
	cfg, err := New(nil, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = RewriteJSONRequestBody("/v1/messages/batches", []byte(`{"requests":[{"custom_id":"a","params":{"model":"gateway/sonnet","messages":[]}}]}`), cfg)
	if err == nil {
		t.Fatal("expected provider-specific batch model to be rejected in strict mode")
	}
}

func TestRewriteJSONLBatchResultsRestoresLogicalModel(t *testing.T) {
	ctx := &Context{Reverse: map[string]string{"gateway/sonnet": "claude-sonnet-4-6"}}
	body := []byte(`{"custom_id":"a","result":{"type":"succeeded","message":{"model":"gateway/sonnet","content":[]}}}` + "\n" + `{"custom_id":"b","result":{"type":"errored","error":{"message":"gateway/sonnet"}}}` + "\n")
	out, err := RewriteJSONLResponseBody(body, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"model":"claude-sonnet-4-6"`)) {
		t.Fatalf("expected logical model in JSONL result: %s", out)
	}
	if !bytes.Contains(out, []byte(`"message":"gateway/sonnet"`)) {
		t.Fatalf("error text should not be rewritten: %s", out)
	}
}

func TestRewriteResponseSkipsWhenNormalizeResponseFalse(t *testing.T) {
	body := []byte("event: message_start\n" + `data: {"message":{"model":"gateway/sonnet"}}` + "\n")
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(bytes.NewReader(body)),
	}
	ctx := &Context{Reverse: map[string]string{"gateway/sonnet": "claude-sonnet-4-6"}, NormalizeResponse: false}
	if err := RewriteResponse(resp, ctx); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, body) {
		t.Fatalf("response body should be untouched when NormalizeResponse=false; got %q", out)
	}
}

func TestRewriteResponseNormalizesWhenRequestRewritten(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(bytes.NewReader([]byte(`{"model":"gateway/sonnet"}`))),
	}
	ctx := &Context{Rewritten: true, NormalizeResponse: true, Reverse: map[string]string{"gateway/sonnet": "claude-sonnet-4-6"}}
	if err := RewriteResponse(resp, ctx); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"model":"claude-sonnet-4-6"`)) {
		t.Fatalf("expected normalized response, got %s", out)
	}
}

func TestRewriteResponseNormalizesBatchResultsWithoutRequestRewrite(t *testing.T) {
	cfg, err := New(map[string]string{"claude-sonnet-4-6": "gateway/sonnet"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodGet, "https://api.anthropic.com/v1/messages/batches/msgbatch_123/results", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := RewriteRequest(req, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ctx.Rewritten || !ctx.NormalizeResponse {
		t.Fatalf("batch results should normalize responses without request rewrite, got %#v", ctx)
	}
	resp := &http.Response{
		Header: http.Header{"Content-Type": []string{"application/x-jsonl"}},
		Body:   io.NopCloser(bytes.NewReader([]byte(`{"result":{"message":{"model":"gateway/sonnet"}}}` + "\n"))),
	}
	if err := RewriteResponse(resp, ctx); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"model":"claude-sonnet-4-6"`)) {
		t.Fatalf("expected batch result normalization, got %s", out)
	}
}

func TestResolveExplicitFileContentWinsOverFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aliases.json")
	if err := os.WriteFile(path, []byte(`{"on-disk-key":"on-disk-value"}`), 0o644); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	snapshot := []byte(`{"snapshot-key":"snapshot-value"}`)
	cfg, _, err := Resolve(ResolveOptions{
		ExplicitFile:        path,
		ExplicitFileContent: snapshot,
	})
	if err != nil {
		t.Fatalf("Resolve returned err: %v", err)
	}
	if got, want := cfg.Forward["snapshot-key"], "snapshot-value"; got != want {
		t.Errorf("snapshot-key forward = %q, want %q", got, want)
	}
	if _, present := cfg.Forward["on-disk-key"]; present {
		t.Errorf("on-disk file was read despite snapshot present; got Forward = %v", cfg.Forward)
	}
}

func TestResolveExplicitFileContentBypassesDiskWhenFileDeleted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aliases.json")
	// Intentionally do NOT create the file on disk.
	snapshot := []byte(`{"k":"v"}`)
	cfg, _, err := Resolve(ResolveOptions{
		ExplicitFile:        path,
		ExplicitFileContent: snapshot,
	})
	if err != nil {
		t.Fatalf("Resolve with missing-file + content-snapshot returned err: %v", err)
	}
	if cfg.Forward["k"] != "v" {
		t.Errorf("Forward[k] = %q, want %q", cfg.Forward["k"], "v")
	}
}

func TestResolveEnvFileContentBypassesEnvPath(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, "env-aliases.json")
	if err := os.WriteFile(envPath, []byte(`{"on-disk-env":"on-disk-env-v"}`), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	snapshot := []byte(`{"snapshot-env":"snapshot-env-v"}`)
	cfg, _, err := Resolve(ResolveOptions{
		// No ExplicitFile; env tier kicks in:
		ParentEnv:      []string{EnvAliasesFile + "=" + envPath},
		EnvFileContent: snapshot,
	})
	if err != nil {
		t.Fatalf("Resolve returned err: %v", err)
	}
	if got, want := cfg.Forward["snapshot-env"], "snapshot-env-v"; got != want {
		t.Errorf("snapshot-env forward = %q, want %q", got, want)
	}
	if _, present := cfg.Forward["on-disk-env"]; present {
		t.Errorf("env-path file was read despite EnvFileContent set; got Forward = %v", cfg.Forward)
	}
}

// The next block of tests exercises stripClaudeCodeSystemBlocks: when the
// model alias rewrites the upstream target to a non-claude-* model, the
// request body's `system` array should have its Claude-Code-specific
// envelope (billing-header attribution + identity preamble) dropped before
// being marshaled. Default-on; CCWRAP_KEEP_CLAUDE_METADATA=1 disables.

const billingBlock = `{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.150; cc_entrypoint=cli; cch=6f0c1;"}`
const identityBlock = `{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude."}`
const userSystemBlock = `{"type":"text","text":"You are a helpful pair programmer."}`

func TestStripSystem_NonClaudeTarget_DropsBillingAndIdentity(t *testing.T) {
	cfg, err := New(map[string]string{"claude-opus-4-7": "gpt-5.5"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-opus-4-7","system":[` + billingBlock + `,` + identityBlock + `,` + userSystemBlock + `],"messages":[]}`)
	out, ctx, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !ctx.Rewritten {
		t.Fatalf("expected ctx.Rewritten=true, got %#v", ctx)
	}
	if bytes.Contains(out, []byte("x-anthropic-billing-header")) {
		t.Fatalf("billing-header block should have been stripped: %s", out)
	}
	if bytes.Contains(out, []byte("You are Claude Code, Anthropic")) {
		t.Fatalf("identity prefix block should have been stripped: %s", out)
	}
	if !bytes.Contains(out, []byte("You are a helpful pair programmer.")) {
		t.Fatalf("regular user system block must survive: %s", out)
	}
}

func TestStripSystem_ClaudeTarget_PreservesAllBlocks(t *testing.T) {
	cfg, err := New(map[string]string{"claude-opus-4-7": "claude-sonnet-4-6"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-opus-4-7","system":[` + billingBlock + `,` + identityBlock + `],"messages":[]}`)
	out, _, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("x-anthropic-billing-header")) {
		t.Fatalf("claude-* target must preserve billing-header: %s", out)
	}
	if !bytes.Contains(out, []byte("You are Claude Code")) {
		t.Fatalf("claude-* target must preserve identity prefix: %s", out)
	}
}

func TestStripSystem_NonClaudeTarget_RemovesEmptySystem(t *testing.T) {
	cfg, err := New(map[string]string{"claude-opus-4-7": "gpt-5.5"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-opus-4-7","system":[` + billingBlock + `,` + identityBlock + `],"messages":[]}`)
	out, _, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte(`"system"`)) {
		t.Fatalf("system should be removed when every block was Claude-Code metadata; got %s", out)
	}
}

func TestStripSystem_EnvOptOut_KeepsBlocks(t *testing.T) {
	t.Setenv("CCWRAP_KEEP_CLAUDE_METADATA", "1")
	cfg, err := New(map[string]string{"claude-opus-4-7": "gpt-5.5"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-opus-4-7","system":[` + billingBlock + `,` + identityBlock + `],"messages":[]}`)
	out, _, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("x-anthropic-billing-header")) {
		t.Fatalf("env opt-out should preserve billing-header: %s", out)
	}
	if !bytes.Contains(out, []byte("You are Claude Code")) {
		t.Fatalf("env opt-out should preserve identity prefix: %s", out)
	}
}

func TestStripSystem_StringArrayShape_AlsoStrips(t *testing.T) {
	// Anthropic SDK accepts system as an array of strings too — verify we
	// handle that shape (Claude Code uses object-shape but be lenient).
	cfg, err := New(map[string]string{"claude-opus-4-7": "gpt-5.5"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-opus-4-7","system":["x-anthropic-billing-header: cc_version=v1;","You are Claude Code, Anthropic's official CLI for Claude.","User prompt here"],"messages":[]}`)
	out, _, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(out, []byte("x-anthropic-billing-header")) {
		t.Fatalf("string-shape billing-header should be stripped: %s", out)
	}
	if bytes.Contains(out, []byte("You are Claude Code, Anthropic")) {
		t.Fatalf("string-shape identity prefix should be stripped: %s", out)
	}
	if !bytes.Contains(out, []byte("User prompt here")) {
		t.Fatalf("user string must survive: %s", out)
	}
}

func TestStripSystem_SystemAsStringNoOp(t *testing.T) {
	// system as a single string (legacy SDK shape) is not an array — leave
	// untouched. Claude Code does not use this shape but third-party
	// callers might.
	cfg, err := New(map[string]string{"claude-opus-4-7": "gpt-5.5"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-opus-4-7","system":"plain system prompt","messages":[]}`)
	out, _, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"system":"plain system prompt"`)) {
		t.Fatalf("string-shape system must pass through unchanged: %s", out)
	}
}

func TestStripSystem_NoSystem_NoOp(t *testing.T) {
	cfg, err := New(map[string]string{"claude-opus-4-7": "gpt-5.5"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-opus-4-7","messages":[]}`)
	out, _, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"model":"gpt-5.5"`)) {
		t.Fatalf("model rewrite must still happen when system is absent: %s", out)
	}
}

func TestStripSystem_UserPromptThatStartsWithYouAreClaudeCode_NotStripped(t *testing.T) {
	// Defensive: a user-crafted "You are Claude Code, but speak Spanish."
	// is NOT in CLI_SYSPROMPT_PREFIXES (exact-match set) so it should survive.
	cfg, err := New(map[string]string{"claude-opus-4-7": "gpt-5.5"}, "test", true)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(`{"model":"claude-opus-4-7","system":[{"type":"text","text":"You are Claude Code, but speak Spanish."}],"messages":[]}`)
	out, _, err := RewriteJSONRequestBody("/v1/messages", body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte("but speak Spanish")) {
		t.Fatalf("user override starting with 'You are Claude Code' must survive (exact-match only): %s", out)
	}
}

func TestResolveBackcompatNilContentFallsBackToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aliases.json")
	if err := os.WriteFile(path, []byte(`{"k":"v"}`), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	cfg, _, err := Resolve(ResolveOptions{
		ExplicitFile: path,
		// ExplicitFileContent: nil
	})
	if err != nil {
		t.Fatalf("Resolve returned err: %v", err)
	}
	if cfg.Forward["k"] != "v" {
		t.Errorf("Forward[k] = %q, want %q (disk-read fallback)", cfg.Forward["k"], "v")
	}
}

// TestLooksProviderSpecific pins the strict-mode gate's shape table. The
// fail-closed contract is "err toward true for provider shapes": a false
// negative lets an unaliased provider id sail to the gateway unrewritten in
// third-party hidden mode. Regression anchors: Bedrock ids
// ("anthropic.claude-*", "us.anthropic.*") carry NO path/ARN marker and were
// once missed entirely; Vertex ids start with "claude-" and were once
// exempted by prefix before the @ marker could fire.
func TestLooksProviderSpecific(t *testing.T) {
	cases := []struct {
		id   string
		want bool
	}{
		// Claude logical ids and bare aliases: not provider-specific.
		{"claude-opus-4-8", false},
		{"claude-haiku-4-5-20251001", false},
		{"claude-fable-5[1m]", false},
		{"claude-future-model-99", false}, // unknown-but-logical passes through
		{"sonnet", false},
		{"opus", false},
		{"haiku", false},
		{"default", false},
		{"best", false},
		{"glm-4-flash", false}, // non-Claude gateway alias target: no routing markers
		{"", false},
		{"   ", false},

		// Bedrock shapes: the "anthropic." segment is the only marker they carry.
		{"anthropic.claude-opus-4-8", true},
		{"anthropic.claude-3-5-sonnet-20241022-v2:0", true},
		{"us.anthropic.claude-haiku-4-5-20251001-v1:0", true},
		{"global.anthropic.claude-opus-4-8", true},
		{"ANTHROPIC.CLAUDE-OPUS-4-8", true}, // case-insensitive

		// Vertex: starts with "claude-" but the @ must win over the prefix.
		{"claude-opus-4-8@20250115", true},

		// ARN / path / deployment shapes.
		{"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-v2", true},
		{"projects/p/locations/us/publishers/anthropic/models/claude-3-haiku", true},
		{"deployments/my-claude-deployment", true},
		{"openrouter/anthropic/claude-3.5-sonnet", true},
		{"some-model:withtag", true},
	}
	for _, tc := range cases {
		if got := LooksProviderSpecific(tc.id); got != tc.want {
			t.Errorf("LooksProviderSpecific(%q) = %v, want %v", tc.id, got, tc.want)
		}
	}
}
