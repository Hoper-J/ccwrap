package ui

import (
	"strings"
	"testing"
)

// syntheticBody is a structurally-faithful fixture modeled on a real
// 2.1.143 /v1/messages body. NO real captured body / NO real PII.
const syntheticBody = `{` +
	`"model":"claude-x",` +
	`"messages":[{"role":"user","content":[` +
	`{"type":"text","text":"<system-reminder>ctx</system-reminder>"},` +
	`{"type":"tool_result","tool_use_id":"t1","content":"ok"},` +
	`{"type":"text","text":"hi"}]}],` +
	`"system":[` +
	`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.x; cch=ab12;"},` +
	`{"type":"text","text":"big agent prompt here","cache_control":{"type":"ephemeral","scope":"global"}},` +
	`{"type":"text","text":"rules","cache_control":{"type":"ephemeral"}}],` +
	`"tools":[` +
	`{"name":"Bash","description":"Runs a shell command. Long markdown.","input_schema":{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"number"}},"required":["command"]}},` +
	`{"name":"Edit","description":"Edits a file.","input_schema":{"type":"object","properties":{"file_path":{"type":"string"}}}}],` +
	`"max_tokens":64000,"stream":true,"metadata":{"user_id":"u"}}`

func TestParseRequestBodyAnatomy(t *testing.T) {
	bv, err := ParseRequestBody([]byte(syntheticBody))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if bv.TotalBytes != len(syntheticBody) {
		t.Fatalf("TotalBytes=%d want %d", bv.TotalBytes, len(syntheticBody))
	}
	got := map[string]int{}
	var order []string
	for _, s := range bv.Anatomy {
		got[s.Key] = s.Bytes
		order = append(order, s.Key)
	}
	for _, k := range []string{"messages", "system", "tools"} {
		if got[k] == 0 {
			t.Fatalf("anatomy missing/zero for %q: %+v", k, bv.Anatomy)
		}
	}
	idx := func(k string) int {
		for i, v := range order {
			if v == k {
				return i
			}
		}
		return -1
	}
	if !(idx("messages") < idx("system") && idx("system") < idx("tools")) {
		t.Fatalf("anatomy not in wire order: %v", order)
	}
	if bv.Anatomy[0].Key != "model" {
		t.Fatalf("first wire key must be model, got %v", order)
	}
}

func TestParseRequestBodySystemBlocks(t *testing.T) {
	bv, err := ParseRequestBody([]byte(syntheticBody))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(bv.System) != 3 {
		t.Fatalf("want 3 system blocks, got %d", len(bv.System))
	}
	if bv.System[0].Index != 0 || !strings.Contains(bv.System[0].Text, "x-anthropic-billing-header") {
		t.Fatalf("block0 must be billing header verbatim: %+v", bv.System[0])
	}
	if bv.System[0].CacheControl != "" {
		t.Fatalf("block0 has no cache_control, got %q", bv.System[0].CacheControl)
	}
	if bv.System[1].CacheControl != "ephemeral/global" {
		t.Fatalf("block1 cache=%q want ephemeral/global", bv.System[1].CacheControl)
	}
	if bv.System[2].CacheControl != "ephemeral" {
		t.Fatalf("block2 cache=%q want ephemeral", bv.System[2].CacheControl)
	}
	if bv.System[1].Bytes == 0 || bv.System[1].Text == "" {
		t.Fatalf("blocks carry verbatim text+bytes: %+v", bv.System[1])
	}
}

func TestParseRequestBodyToolsSchemaRaw(t *testing.T) {
	bv, err := ParseRequestBody([]byte(syntheticBody))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(bv.Tools) != 2 {
		t.Fatalf("want 2 tools, got %d", len(bv.Tools))
	}
	bash := bv.Tools[0]
	if bash.Name != "Bash" {
		t.Fatalf("tool0 name=%q want Bash", bash.Name)
	}
	if bash.DescBytes == 0 || !strings.Contains(bash.Description, "shell command") {
		t.Fatalf("verbatim description expected: %+v", bash)
	}
	if len(bash.SchemaProps) != 2 || bash.SchemaProps[0] != "command" {
		t.Fatalf("schema props (declaration order) = %v want [command timeout]", bash.SchemaProps)
	}
	if !strings.Contains(bash.RawSchema, "\"required\"") || !strings.Contains(bash.RawSchema, "\"command\"") {
		t.Fatalf("RawSchema must be the literal schema JSON: %s", bash.RawSchema)
	}
}

func TestParseRequestBodyMessages(t *testing.T) {
	bv, err := ParseRequestBody([]byte(syntheticBody))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(bv.Messages) != 1 || bv.Messages[0].Role != "user" {
		t.Fatalf("want 1 user turn, got %+v", bv.Messages)
	}
	blks := bv.Messages[0].Blocks
	if len(blks) != 3 {
		t.Fatalf("want 3 content blocks, got %d", len(blks))
	}
	if blks[0].Type != "text" || blks[1].Type != "tool_result" || blks[2].Type != "text" {
		t.Fatalf("block types/order wrong: %+v", blks)
	}
	if blks[0].Index != 0 || blks[2].Index != 2 {
		t.Fatalf("indices must be wire order: %+v", blks)
	}
	if blks[1].Bytes == 0 || blks[1].Raw == "" {
		t.Fatalf("non-text block keeps raw view: %+v", blks[1])
	}
}

func TestParseRequestBodyStringContentMessage(t *testing.T) {
	body := `{"messages":[{"role":"user","content":"plain string"}]}`
	bv, err := ParseRequestBody([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(bv.Messages) != 1 || len(bv.Messages[0].Blocks) != 1 {
		t.Fatalf("want 1 turn/1 block, got %+v", bv.Messages)
	}
	b := bv.Messages[0].Blocks[0]
	if b.Type != "text" || b.Raw != "plain string" {
		t.Fatalf("string content must yield ONE unquoted text block, got %+v", b)
	}
}

func TestParseRequestBodyMalformedKeepsRaw(t *testing.T) {
	junk := []byte(`{not valid json`)
	bv, err := ParseRequestBody(junk)
	if err == nil {
		t.Fatalf("expected parse error for malformed body")
	}
	if bv.TotalBytes != len(junk) || string(bv.Raw) != string(junk) {
		t.Fatalf("Raw escape hatch must survive malformed input: %+v", bv)
	}
	if bv.System != nil || bv.Tools != nil || bv.Messages != nil {
		t.Fatalf("no projections on malformed input: %+v", bv)
	}
}

func TestParseRequestBodyNonObjectNoPanic(t *testing.T) {
	for _, in := range []string{`[1,2]`, `["a","b"]`, `[]`, `42`, `"x"`, `true`} {
		bv, err := ParseRequestBody([]byte(in))
		if err == nil {
			t.Fatalf("non-object body %q must return error, got nil", in)
		}
		if bv.TotalBytes != len(in) || string(bv.Raw) != in {
			t.Fatalf("Raw escape hatch must survive non-object %q: %+v", in, bv)
		}
		if bv.System != nil || bv.Tools != nil || bv.Messages != nil || bv.Anatomy != nil {
			t.Fatalf("no projections for non-object %q: %+v", in, bv)
		}
	}
}
