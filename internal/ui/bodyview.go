package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// BodyView is the pure, render-ready projection of a captured request
// body. Storage is full-fidelity bytes on disk; this type only drives
// display. No I/O happens here.
type BodyView struct {
	TotalBytes int
	Anatomy    []AnatomySegment // top-level keys, wire order, byte share
	System     []SystemBlock
	Tools      []ToolEntry
	Messages   []MessageTurn
	// Raw is the original bytes, re-emitted unchanged for the Raw JSON
	// escape hatch. Never mutated.
	Raw []byte
}

type AnatomySegment struct {
	Key   string
	Bytes int
	Pct   int // 0..100, integer percent of TotalBytes
}

type SystemBlock struct {
	Index        int
	Type         string
	Bytes        int
	Text         string // verbatim, no redaction
	CacheControl string // "", "ephemeral", "ephemeral/global", or raw json
}

type ToolEntry struct {
	Name        string
	Description string
	DescBytes   int
	SchemaProps []string // property names, declaration order best-effort
	RawSchema   string   // literal input_schema JSON, pretty
}

type MessageTurn struct {
	Role   string
	Blocks []MessageBlock
}

type MessageBlock struct {
	Index int
	Type  string // text|tool_use|tool_result|image|<unknown>
	Bytes int
	Raw   string // type-specific raw view; unknown -> raw JSON
}

// ParseRequestBody projects raw body bytes into a BodyView.
// On malformed JSON it returns a BodyView with only TotalBytes+Raw set
// and a non-nil error, so callers can still offer the Raw escape hatch.
func ParseRequestBody(raw []byte) (BodyView, error) {
	bv := BodyView{TotalBytes: len(raw), Raw: raw}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var top orderedObject
	if err := dec.Decode(&top); err != nil {
		return bv, err
	}
	for _, kv := range top {
		seg := AnatomySegment{Key: kv.Key, Bytes: len(kv.Raw)}
		if bv.TotalBytes > 0 {
			seg.Pct = seg.Bytes * 100 / bv.TotalBytes
		}
		bv.Anatomy = append(bv.Anatomy, seg)
	}
	for _, kv := range top {
		switch kv.Key {
		case "system":
			bv.System = parseSystemBlocks(kv.Raw)
		case "tools":
			bv.Tools = parseTools(kv.Raw)
		case "messages":
			bv.Messages = parseMessages(kv.Raw)
		}
	}
	return bv, nil
}

// parseSystemBlocks projects the system[] array into ordered SystemBlocks,
// preserving array index and verbatim text with no redaction. A non-array
// system value yields nil (the Raw escape hatch still applies).
func parseSystemBlocks(raw json.RawMessage) []SystemBlock {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := make([]SystemBlock, 0, len(arr))
	for i, el := range arr {
		var b struct {
			Type         string          `json:"type"`
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		}
		_ = json.Unmarshal(el, &b)
		out = append(out, SystemBlock{
			Index:        i,
			Type:         b.Type,
			Bytes:        len(el),
			Text:         b.Text,
			CacheControl: cacheControlLabel(b.CacheControl),
		})
	}
	return out
}

// parseTools projects the tools[] array into ToolEntry, preserving the
// verbatim tool name/description and re-emitting input_schema as literal
// pretty JSON with NO synthesized signatures. A non-array tools value
// yields nil (the Raw escape hatch still applies).
func parseTools(raw json.RawMessage) []ToolEntry {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := make([]ToolEntry, 0, len(arr))
	for _, el := range arr {
		var t struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		_ = json.Unmarshal(el, &t)
		out = append(out, ToolEntry{
			Name:        t.Name,
			Description: t.Description,
			DescBytes:   len(t.Description),
			SchemaProps: schemaPropNames(t.InputSchema),
			RawSchema:   prettyJSON(t.InputSchema),
		})
	}
	return out
}

// parseMessages projects the messages[] array into turns of ordered
// content blocks, preserving array index and arbitrary block types
// verbatim, with NO transcript synthesis and NO heuristic origin tags. A
// text block keeps its text value as Raw; any non-text or type-less block
// keeps its raw JSON; an empty type renders "<unknown>".
// A non-array content (or messages) value degrades to a single raw
// block / nil so the Raw escape hatch still applies.
func parseMessages(raw json.RawMessage) []MessageTurn {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := make([]MessageTurn, 0, len(arr))
	for _, mRaw := range arr {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		_ = json.Unmarshal(mRaw, &m)
		turn := MessageTurn{Role: m.Role}
		var blocks []json.RawMessage
		if err := json.Unmarshal(m.Content, &blocks); err != nil {
			raw := string(m.Content)
			var s string
			if json.Unmarshal(m.Content, &s) == nil {
				raw = s
			}
			turn.Blocks = []MessageBlock{{Index: 0, Type: "text", Bytes: len(m.Content), Raw: raw}}
			out = append(out, turn)
			continue
		}
		for i, el := range blocks {
			var hdr struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			_ = json.Unmarshal(el, &hdr)
			typ := hdr.Type
			if typ == "" {
				typ = "<unknown>"
			}
			raw := string(el)
			if hdr.Type == "text" {
				raw = hdr.Text
			}
			turn.Blocks = append(turn.Blocks, MessageBlock{Index: i, Type: typ, Bytes: len(el), Raw: raw})
		}
		out = append(out, turn)
	}
	return out
}

// schemaPropNames returns input_schema.properties keys in declaration
// order (wire-faithful; map order would be random). It reuses
// orderedObject as the properties value: a JSON object whose first token
// is '{', which orderedObject.UnmarshalJSON handles.
func schemaPropNames(schema json.RawMessage) []string {
	var s struct {
		Properties orderedObject `json:"properties"`
	}
	if err := json.Unmarshal(schema, &s); err != nil {
		return nil
	}
	out := make([]string, 0, len(s.Properties))
	for _, kv := range s.Properties {
		out = append(out, kv.Key)
	}
	return out
}

// prettyJSON re-indents raw JSON for the Schema-raw view;
// empty input -> "", and unparseable input falls back to the raw string
// so the literal schema is never dropped.
func prettyJSON(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Indent(&buf, raw, "", "  "); err != nil {
		return string(raw)
	}
	return buf.String()
}

// cacheControlLabel renders a cache_control object as the rail label
// "type" or "type/scope" (e.g. "ephemeral", "ephemeral/global"); absent
// -> "", unparseable -> trimmed raw JSON.
func cacheControlLabel(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var cc struct {
		Type  string `json:"type"`
		Scope string `json:"scope"`
	}
	if err := json.Unmarshal(raw, &cc); err != nil || cc.Type == "" {
		return strings.TrimSpace(string(raw))
	}
	if cc.Scope != "" {
		return cc.Type + "/" + cc.Scope
	}
	return cc.Type
}

// orderedObject preserves top-level key order (json.Unmarshal into a map
// loses it; the anatomy lesson depends on wire order).
type orderedObject []orderedKV
type orderedKV struct {
	Key string
	Raw json.RawMessage
}

func (o *orderedObject) UnmarshalJSON(b []byte) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token() // consume '{'
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return fmt.Errorf("ui: request body top-level is not a JSON object")
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := keyTok.(string)
		if !ok {
			return fmt.Errorf("ui: non-string object key %v", keyTok)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		*o = append(*o, orderedKV{Key: key, Raw: raw})
	}
	return nil
}
