package modelalias

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"sort"
	"strings"
)

const MaxRewriteBytes int64 = 32 * 1024 * 1024

const (
	ModeDisabled = "disabled"
	ModeRewrite  = "rewrite"

	EnvAliasesFile = "CCWRAP_MODEL_ALIASES_FILE"
	EnvAliasesJSON = "CCWRAP_MODEL_ALIASES_JSON"
)

type Config struct {
	Forward                  map[string]string
	Strict                   bool
	Source                   string
	Fingerprint              string
	ProviderModelPassthrough bool
}

type Context struct {
	Enabled           bool
	Rewritten         bool
	NormalizeResponse bool
	LogicalModel      string
	UpstreamModel     string
	Reverse           map[string]string
}

type ResolveOptions struct {
	ExplicitFile             string
	ExplicitFileContent      []byte // if non-empty, used in place of LoadFile(ExplicitFile); takes normative precedence over reading from disk
	ExplicitPairs            []string
	ParentEnv                []string
	EnvFileContent           []byte // if non-empty, used in place of LoadFile(env-path); takes normative precedence over reading from disk
	FlagSettings             map[string]any
	Strict                   bool
	ProviderModelPassthrough bool
}

func New(forward map[string]string, source string, strict bool) (Config, error) {
	cfg := Config{Strict: strict, Source: strings.TrimSpace(source)}
	if len(forward) == 0 {
		return cfg, nil
	}
	clean := make(map[string]string, len(forward))
	seenProvider := map[string]string{}
	for logical, provider := range forward {
		logical = strings.TrimSpace(logical)
		provider = strings.TrimSpace(provider)
		if logical == "" || provider == "" {
			return Config{}, fmt.Errorf("model alias entries require non-empty logical and provider model IDs")
		}
		if LooksProviderSpecific(logical) {
			return Config{}, fmt.Errorf("model alias logical model %q looks provider-specific; use a Claude logical model as the alias key", logical)
		}
		if prev, ok := seenProvider[provider]; ok && prev != logical {
			return Config{}, fmt.Errorf("provider model %q is mapped from both %q and %q; duplicate provider aliases are not supported", provider, prev, logical)
		}
		clean[logical] = provider
		seenProvider[provider] = logical
	}
	cfg.Forward = clean
	cfg.Fingerprint = Fingerprint(clean)
	return cfg, nil
}

func Resolve(opts ResolveOptions) (Config, []string, error) {
	merged := map[string]string{}
	var sources []string
	if opts.FlagSettings != nil {
		aliases, err := ExtractFromFlagSettings(opts.FlagSettings)
		if err != nil {
			return Config{}, nil, err
		}
		if len(aliases) > 0 {
			merge(merged, aliases)
			sources = append(sources, "flagSettings:modelAliases")
		}
	}
	env := envSliceToMap(opts.ParentEnv)
	if raw := strings.TrimSpace(env[EnvAliasesJSON]); raw != "" {
		aliases, err := ParseJSON([]byte(raw), EnvAliasesJSON)
		if err != nil {
			return Config{}, nil, err
		}
		merge(merged, aliases)
		sources = append(sources, EnvAliasesJSON)
	}
	file := strings.TrimSpace(opts.ExplicitFile)
	fileSource := "explicit:model-alias-file"
	contentSnapshot := opts.ExplicitFileContent
	if file == "" {
		file = strings.TrimSpace(env[EnvAliasesFile])
		fileSource = EnvAliasesFile
		contentSnapshot = opts.EnvFileContent
	}
	if file != "" || len(contentSnapshot) > 0 {
		var aliases map[string]string
		if len(contentSnapshot) > 0 {
			// Normative precedence: provided content wins; disk MUST NOT be read.
			parsed, err := ParseJSON(contentSnapshot, fileSource)
			if err != nil {
				return Config{}, nil, err
			}
			aliases = parsed
		} else {
			loaded, err := LoadFile(file)
			if err != nil {
				return Config{}, nil, err
			}
			aliases = loaded
		}
		merge(merged, aliases)
		sources = append(sources, fileSource)
	}
	if len(opts.ExplicitPairs) > 0 {
		aliases, err := ParsePairs(opts.ExplicitPairs)
		if err != nil {
			return Config{}, nil, err
		}
		merge(merged, aliases)
		sources = append(sources, "explicit:model-alias")
	}
	sources = dedupe(sources)
	cfg, err := New(merged, strings.Join(sources, ", "), opts.Strict && !opts.ProviderModelPassthrough)
	if err != nil {
		return Config{}, nil, err
	}
	cfg.ProviderModelPassthrough = opts.ProviderModelPassthrough
	if cfg.ProviderModelPassthrough {
		cfg.Strict = false
	}
	return cfg, sources, nil
}

func (c Config) Enabled() bool { return len(c.Forward) > 0 }
func (c Config) Count() int    { return len(c.Forward) }
func (c Config) Mode() string {
	if c.Enabled() {
		return ModeRewrite
	}
	return ModeDisabled
}

func (c Config) LogicalForProvider(provider string) (string, bool) {
	provider = strings.TrimSpace(provider)
	if provider == "" || len(c.Forward) == 0 {
		return "", false
	}
	for logical, upstream := range c.Forward {
		if upstream == provider {
			return logical, true
		}
	}
	return "", false
}

type Normalization struct {
	Source        string
	LogicalModel  string
	ProviderModel string
}

func NormalizeModelID(value string, cfg Config) (logical string, changed bool, unresolved bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return value, false, false
	}
	if logical, ok := cfg.LogicalForProvider(value); ok {
		return logical, true, false
	}
	if LooksProviderSpecific(value) && !cfg.ProviderModelPassthrough {
		return value, false, true
	}
	return value, false, false
}

func NormalizeClaudeModelArgs(args []string, cfg Config) (out []string, normalized []Normalization, unresolved []string) {
	out = make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out); i++ {
		arg := out[i]
		if arg == "--" {
			break
		}
		if strings.HasPrefix(arg, "--model=") {
			value := strings.TrimSpace(arg[len("--model="):])
			logical, changed, missing := NormalizeModelID(value, cfg)
			if changed {
				out[i] = "--model=" + logical
				normalized = append(normalized, Normalization{Source: "--model", LogicalModel: logical, ProviderModel: value})
			} else if missing {
				unresolved = append(unresolved, value)
			}
			continue
		}
		if arg == "--model" && i+1 < len(out) {
			value := strings.TrimSpace(out[i+1])
			logical, changed, missing := NormalizeModelID(value, cfg)
			if changed {
				out[i+1] = logical
				normalized = append(normalized, Normalization{Source: "--model", LogicalModel: logical, ProviderModel: value})
			} else if missing {
				unresolved = append(unresolved, value)
			}
			i++
		}
	}
	return out, normalized, dedupe(unresolved)
}

func NormalizeModelEnv(env map[string]string, cfg Config, isModelKey func(string) bool) (out map[string]string, normalized []Normalization, unresolved map[string]string) {
	out = make(map[string]string, len(env))
	for k, v := range env {
		out[k] = v
	}
	unresolved = map[string]string{}
	if isModelKey == nil {
		isModelKey = func(string) bool { return true }
	}
	for key, value := range out {
		value = strings.TrimSpace(value)
		if value == "" || !isModelKey(key) {
			continue
		}
		logical, changed, missing := NormalizeModelID(value, cfg)
		if changed {
			out[key] = logical
			normalized = append(normalized, Normalization{Source: key, LogicalModel: logical, ProviderModel: value})
		} else if missing {
			unresolved[key] = value
		}
	}
	if len(unresolved) == 0 {
		return out, normalized, nil
	}
	return out, normalized, unresolved
}

func LoadFile(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read model alias file %s: %w", path, err)
	}
	return ParseJSON(data, path)
}

func ParseJSON(data []byte, source string) (map[string]string, error) {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse model aliases %s: %w", source, err)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("model aliases %s must be a JSON object", source)
	}
	if nested, ok := obj["modelAliases"]; ok {
		return coerceAliasMap(nested, source+".modelAliases")
	}
	if nested, ok := obj["model_aliases"]; ok {
		return coerceAliasMap(nested, source+".model_aliases")
	}
	if nested, ok := obj["ccwrap"]; ok {
		ccwrap, ok := nested.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s.ccwrap must be an object", source)
		}
		if aliases, ok := ccwrap["modelAliases"]; ok {
			return coerceAliasMap(aliases, source+".ccwrap.modelAliases")
		}
		if aliases, ok := ccwrap["model_aliases"]; ok {
			return coerceAliasMap(aliases, source+".ccwrap.model_aliases")
		}
	}
	return coerceAliasMap(obj, source)
}

func ExtractFromFlagSettings(settings map[string]any) (map[string]string, error) {
	out := map[string]string{}
	if raw, ok := settings["ccwrap"]; ok {
		ccwrap, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("settings ccwrap must be an object")
		}
		aliases, ok := ccwrap["modelAliases"]
		if !ok {
			aliases, ok = ccwrap["model_aliases"]
		}
		if ok {
			m, err := coerceAliasMap(aliases, "settings.ccwrap.modelAliases")
			if err != nil {
				return nil, err
			}
			merge(out, m)
		}
	}
	if raw, ok := settings["modelOverrides"]; ok {
		m, err := coerceAliasMap(raw, "settings.modelOverrides")
		if err != nil {
			return nil, err
		}
		merge(out, m)
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func ParsePairs(pairs []string) (map[string]string, error) {
	out := map[string]string{}
	for _, pair := range pairs {
		left, right, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("model alias %q must use logical=provider", pair)
		}
		logical := strings.TrimSpace(left)
		provider := strings.TrimSpace(right)
		if logical == "" || provider == "" {
			return nil, fmt.Errorf("model alias %q must include non-empty logical and provider model IDs", pair)
		}
		out[logical] = provider
	}
	return out, nil
}

func coerceAliasMap(raw any, source string) (map[string]string, error) {
	obj, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s must be an object mapping logical model to provider model", source)
	}
	out := map[string]string{}
	for k, v := range obj {
		logical := strings.TrimSpace(k)
		provider, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("%s[%s] must be a string", source, k)
		}
		provider = strings.TrimSpace(provider)
		if logical == "" || provider == "" {
			return nil, fmt.Errorf("%s contains empty logical/provider model", source)
		}
		out[logical] = provider
	}
	return out, nil
}

func Fingerprint(forward map[string]string) string {
	if len(forward) == 0 {
		return ""
	}
	keys := make([]string, 0, len(forward))
	for k := range forward {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = h.Write([]byte(k))
		_, _ = h.Write([]byte("\x00"))
		_, _ = h.Write([]byte(forward[k]))
		_, _ = h.Write([]byte("\x00"))
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))[:16]
}

func newContextForRequest(method, path string, cfg Config) *Context {
	ctx := &Context{Enabled: cfg.Enabled() || cfg.Strict, Reverse: reverseMap(cfg.Forward)}
	if cfg.Enabled() && isBatchResultsPath(method, path) {
		ctx.NormalizeResponse = true
	}
	return ctx
}

func RewriteRequest(r *http.Request, cfg Config) (*Context, error) {
	ctx := newContextForRequest("", "", cfg)
	if r != nil && r.URL != nil {
		ctx = newContextForRequest(r.Method, r.URL.Path, cfg)
	}
	if (!cfg.Enabled() && !cfg.Strict) || r == nil || r.URL == nil || !isRewriteEndpoint(r.Method, r.URL.Path) {
		return ctx, nil
	}
	if enc := strings.TrimSpace(r.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return nil, fmt.Errorf("model alias rewrite requires uncompressed request body; got Content-Encoding=%s", enc)
	}
	if ct := strings.TrimSpace(r.Header.Get("Content-Type")); ct != "" && !isJSONContentType(ct) {
		return nil, fmt.Errorf("model alias rewrite requires JSON request body; got Content-Type=%s", ct)
	}
	body, err := readLimitedBody(r.Body, MaxRewriteBytes)
	if err != nil {
		return nil, err
	}
	newBody, rewriteCtx, err := RewriteJSONRequestBody(r.URL.Path, body, cfg)
	if err != nil {
		return nil, err
	}
	replaceRequestBody(r, newBody)
	if r.Header.Get("Content-Type") == "" {
		r.Header.Set("Content-Type", "application/json")
	}
	return rewriteCtx, nil
}

// claudeBillingHeaderPrefix is the leading text of Claude Code's attribution
// pixel — embedded as a text block in the request body's `system` array
// (claude-code/src/constants/system.ts → splitSysPromptPrefix in api.ts).
// Format:
//
//	x-anthropic-billing-header: cc_version=<ver>; cc_entrypoint=<e>; cch=<hash>; ...
//
// Anthropic's server lifts this from system[0] for billing/attestation; for
// non-Anthropic upstreams it is wasted tokens at best, model-confusion at
// worst (the GPT model sees it as a literal system instruction).
const claudeBillingHeaderPrefix = "x-anthropic-billing-header"

// envKeepClaudeMetadata opts out of stripping Claude Code identity envelopes
// when forwarding to a non-claude-* upstream. Default is to strip; set this
// to "1" / "true" / "yes" to forward verbatim.
const envKeepClaudeMetadata = "CCWRAP_KEEP_CLAUDE_METADATA"

// claudeCodeIdentityPrefixes mirrors CLI_SYSPROMPT_PREFIX_VALUES from
// claude-code/src/constants/system.ts. Exact-match set; update when Claude
// Code adds new variants. Exact match (not prefix) so we never accidentally
// strip a user's deliberately-crafted "You are Claude Code, but ..." override.
var claudeCodeIdentityPrefixes = map[string]struct{}{
	"You are Claude Code, Anthropic's official CLI for Claude.":                                      {},
	"You are Claude Code, Anthropic's official CLI for Claude, running within the Claude Agent SDK.": {},
	"You are a Claude agent, built on Anthropic's Claude Agent SDK.":                                 {},
}

func keepClaudeMetadata() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(envKeepClaudeMetadata)))
	return v == "1" || v == "true" || v == "yes"
}

func shouldDropClaudeCodeSystemBlock(text string) bool {
	if strings.HasPrefix(text, claudeBillingHeaderPrefix) {
		return true
	}
	_, ok := claudeCodeIdentityPrefixes[text]
	return ok
}

// stripClaudeCodeSystemBlocks rewrites obj["system"] in place when it is an
// array, dropping every element whose text content is either the billing
// header pixel or one of the known Claude Code identity prefixes. No-op when
// "system" is missing, not an array, or yields no drops. If every element is
// dropped, the field is removed from obj entirely (an empty array would be
// accepted by Anthropic's API but rejected by some third-party gateways).
func stripClaudeCodeSystemBlocks(obj map[string]json.RawMessage) {
	rawSystem, ok := obj["system"]
	if !ok {
		return
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(rawSystem, &arr); err != nil {
		return // string or other shape — Claude Code never emits these for /v1/messages
	}
	kept := make([]json.RawMessage, 0, len(arr))
	dropped := false
	for _, elem := range arr {
		var asObj struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(elem, &asObj); err == nil && asObj.Type == "text" {
			if shouldDropClaudeCodeSystemBlock(asObj.Text) {
				dropped = true
				continue
			}
			kept = append(kept, elem)
			continue
		}
		var asStr string
		if err := json.Unmarshal(elem, &asStr); err == nil {
			if shouldDropClaudeCodeSystemBlock(asStr) {
				dropped = true
				continue
			}
			kept = append(kept, elem)
			continue
		}
		kept = append(kept, elem)
	}
	if !dropped {
		return
	}
	if len(kept) == 0 {
		delete(obj, "system")
		return
	}
	out, err := json.Marshal(kept)
	if err != nil {
		return
	}
	obj["system"] = out
}

func RewriteJSONRequestBody(path string, body []byte, cfg Config) ([]byte, *Context, error) {
	ctx := &Context{Enabled: cfg.Enabled() || cfg.Strict, Reverse: reverseMap(cfg.Forward)}
	if (!cfg.Enabled() && !cfg.Strict) || !isModelPath(path) || len(bytes.TrimSpace(body)) == 0 {
		return body, ctx, nil
	}
	if isBatchCreatePath(path) {
		return RewriteJSONBatchCreateBody(body, cfg, ctx)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, nil, fmt.Errorf("parse model-bearing request JSON: %w", err)
	}
	changed, logical, provider, err := rewriteRequestObjectModel(obj, cfg)
	if err != nil {
		return nil, nil, err
	}
	if !changed {
		return body, ctx, nil
	}
	// Strip Claude Code identity envelope when forwarding to non-claude-* upstream.
	// Default-on; CCWRAP_KEEP_CLAUDE_METADATA=1 disables. Skips batch path on purpose:
	// Claude Code's source never invokes /v1/messages/batches, so the envelope
	// only ever appears on the single-message path.
	if !strings.HasPrefix(strings.ToLower(provider), "claude-") && !keepClaudeMetadata() {
		stripClaudeCodeSystemBlocks(obj)
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, nil, fmt.Errorf("encode rewritten request JSON: %w", err)
	}
	ctx.Rewritten = true
	ctx.NormalizeResponse = true
	ctx.LogicalModel = logical
	ctx.UpstreamModel = provider
	return out, ctx, nil
}

func RewriteJSONBatchCreateBody(body []byte, cfg Config, ctx *Context) ([]byte, *Context, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, nil, fmt.Errorf("parse message batch request JSON: %w", err)
	}
	rawRequests, ok := obj["requests"]
	if !ok {
		return body, ctx, nil
	}
	var requests []map[string]json.RawMessage
	if err := json.Unmarshal(rawRequests, &requests); err != nil {
		return nil, nil, fmt.Errorf("message batch requests must be an array")
	}
	changed := false
	for i := range requests {
		rawParams, ok := requests[i]["params"]
		if !ok {
			continue
		}
		var params map[string]json.RawMessage
		if err := json.Unmarshal(rawParams, &params); err != nil {
			return nil, nil, fmt.Errorf("message batch requests[%d].params must be an object", i)
		}
		itemChanged, logical, provider, err := rewriteRequestObjectModel(params, cfg)
		if err != nil {
			return nil, nil, fmt.Errorf("message batch requests[%d].params: %w", i, err)
		}
		if itemChanged {
			encoded, _ := json.Marshal(params)
			requests[i]["params"] = encoded
			changed = true
			ctx.Rewritten = true
			ctx.NormalizeResponse = true
			if ctx.LogicalModel == "" {
				ctx.LogicalModel = logical
				ctx.UpstreamModel = provider
			}
		}
	}
	if !changed {
		return body, ctx, nil
	}
	encodedRequests, _ := json.Marshal(requests)
	obj["requests"] = encodedRequests
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, nil, fmt.Errorf("encode rewritten message batch request JSON: %w", err)
	}
	return out, ctx, nil
}

func rewriteRequestObjectModel(obj map[string]json.RawMessage, cfg Config) (changed bool, logical string, provider string, err error) {
	rawModel, ok := obj["model"]
	if !ok {
		return false, "", "", nil
	}
	var modelID string
	if err := json.Unmarshal(rawModel, &modelID); err != nil {
		return false, "", "", fmt.Errorf("request model field must be a string")
	}
	if provider, ok := cfg.Forward[modelID]; ok {
		encoded, _ := json.Marshal(provider)
		obj["model"] = encoded
		return true, modelID, provider, nil
	}
	if cfg.Strict && LooksProviderSpecific(modelID) {
		return false, "", "", fmt.Errorf("provider-specific model %q reached Claude request path; use a Claude logical model plus CCWRAP modelAliases", modelID)
	}
	return false, "", "", nil
}

func RewriteResponse(resp *http.Response, ctx *Context) error {
	if resp == nil || ctx == nil || !ctx.NormalizeResponse || len(ctx.Reverse) == 0 {
		return nil
	}
	if enc := strings.TrimSpace(resp.Header.Get("Content-Encoding")); enc != "" && !strings.EqualFold(enc, "identity") {
		return fmt.Errorf("model alias response normalization requires uncompressed upstream response; got Content-Encoding=%s", enc)
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(ct, "text/event-stream") {
		resp.Body = WrapSSEResponse(resp.Body, ctx)
		resp.Header.Del("Content-Length")
		resp.ContentLength = -1
		return nil
	}
	if strings.Contains(ct, "jsonl") || strings.Contains(ct, "ndjson") {
		body, err := readLimitedBody(resp.Body, MaxRewriteBytes)
		if err != nil {
			return err
		}
		out, err := RewriteJSONLResponseBody(body, ctx)
		if err != nil {
			return err
		}
		replaceResponseBody(resp, out)
		return nil
	}
	if ct == "" || isJSONContentType(ct) {
		body, err := readLimitedBody(resp.Body, MaxRewriteBytes)
		if err != nil {
			return err
		}
		out, err := RewriteJSONResponseBody(body, ctx)
		if err != nil {
			return err
		}
		replaceResponseBody(resp, out)
	}
	return nil
}

func RewriteJSONResponseBody(body []byte, ctx *Context) ([]byte, error) {
	if ctx == nil || len(ctx.Reverse) == 0 || len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, nil
	}
	changed := rewriteResponseObjectModels(obj, ctx.Reverse)
	if !changed {
		return body, nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode normalized response JSON: %w", err)
	}
	return out, nil
}

func RewriteJSONLResponseBody(body []byte, ctx *Context) ([]byte, error) {
	if ctx == nil || len(ctx.Reverse) == 0 || len(bytes.TrimSpace(body)) == 0 {
		return body, nil
	}
	lines := bytes.Split(body, []byte("\n"))
	changed := false
	for i, line := range lines {
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) == 0 || !bytes.HasPrefix(trimmed, []byte("{")) {
			continue
		}
		out, err := RewriteJSONResponseBody(trimmed, ctx)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(out, trimmed) {
			prefixLen := len(line) - len(bytes.TrimLeft(line, " \t\r"))
			suffixLen := len(line) - len(bytes.TrimRight(line, " \t\r"))
			var rebuilt []byte
			rebuilt = append(rebuilt, line[:prefixLen]...)
			rebuilt = append(rebuilt, out...)
			if suffixLen > 0 {
				rebuilt = append(rebuilt, line[len(line)-suffixLen:]...)
			}
			lines[i] = rebuilt
			changed = true
		}
	}
	if !changed {
		return body, nil
	}
	return bytes.Join(lines, []byte("\n")), nil
}

func rewriteResponseObjectModels(obj map[string]json.RawMessage, reverse map[string]string) bool {
	changed := rewriteRawModelField(obj, "model", reverse)
	if raw, ok := obj["message"]; ok {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(raw, &msg); err == nil {
			if rewriteResponseObjectModels(msg, reverse) {
				encoded, _ := json.Marshal(msg)
				obj["message"] = encoded
				changed = true
			}
		}
	}
	if raw, ok := obj["result"]; ok {
		var result map[string]json.RawMessage
		if err := json.Unmarshal(raw, &result); err == nil {
			if rewriteResponseObjectModels(result, reverse) {
				encoded, _ := json.Marshal(result)
				obj["result"] = encoded
				changed = true
			}
		}
	}
	return changed
}

func WrapSSEResponse(body io.ReadCloser, ctx *Context) io.ReadCloser {
	pr, pw := io.Pipe()
	go func() {
		defer body.Close()
		scanner := bufio.NewScanner(body)
		scanner.Buffer(make([]byte, 0, 64*1024), int(MaxRewriteBytes))
		for scanner.Scan() {
			line := strings.TrimSuffix(scanner.Text(), "\r")
			out := rewriteSSELine(line, ctx)
			if _, err := io.WriteString(pw, out+"\n"); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		if err := scanner.Err(); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
		_ = pw.Close()
	}()
	return pr
}

func rewriteSSELine(line string, ctx *Context) string {
	if ctx == nil || len(ctx.Reverse) == 0 || !strings.HasPrefix(line, "data:") {
		return line
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" || !strings.HasPrefix(payload, "{") {
		return line
	}
	out, err := RewriteJSONResponseBody([]byte(payload), ctx)
	if err != nil || bytes.Equal(out, []byte(payload)) {
		return line
	}
	return "data: " + string(out)
}

func ProviderSpecificModelArgs(args []string) []string {
	var hits []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			break
		}
		var value string
		if strings.HasPrefix(arg, "--model=") {
			value = strings.TrimSpace(arg[len("--model="):])
		} else if arg == "--model" && i+1 < len(args) {
			value = strings.TrimSpace(args[i+1])
			i++
		}
		if value != "" && LooksProviderSpecific(value) {
			hits = append(hits, value)
		}
	}
	return hits
}

// LooksProviderSpecific reports whether modelID is shaped like a
// provider-routed id (Bedrock, Vertex, Azure-style deployments, ARNs) rather
// than a Claude logical id. It backs the strict-mode fail-closed gates
// (rewriteRequestObjectModel, validateHiddenModeModels), so it must err toward
// TRUE for provider shapes: a miss here lets an unaliased provider id sail to
// the gateway unrewritten. Marker scan runs BEFORE any canonical-shape
// exemption — Vertex ids ("claude-opus-4-8@20250115") start with "claude-"
// yet are provider-specific, and Bedrock ids carry an "anthropic." segment
// ("anthropic.claude-opus-4-8", "us.anthropic.claude-...-v1:0") with none of
// the path/ARN markers. Canonical ids ("claude-opus-4-8", bare aliases like
// "sonnet") contain no marker and fall through to false. Explicitly aliased
// ids are exempt upstream of this check (cfg.Forward is consulted first).
func LooksProviderSpecific(modelID string) bool {
	s := strings.TrimSpace(modelID)
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	markers := []string{"arn:", "/", "projects/", "deployments/", "publishers/", "@", "anthropic."}
	for _, marker := range markers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return strings.Contains(s, ":")
}

func rewriteRawModelField(obj map[string]json.RawMessage, field string, reverse map[string]string) bool {
	raw, ok := obj[field]
	if !ok {
		return false
	}
	var modelID string
	if err := json.Unmarshal(raw, &modelID); err != nil {
		return false
	}
	logical, ok := reverse[modelID]
	if !ok {
		return false
	}
	encoded, _ := json.Marshal(logical)
	obj[field] = encoded
	return true
}

func readLimitedBody(rc io.ReadCloser, limit int64) ([]byte, error) {
	if rc == nil {
		return []byte{}, nil
	}
	defer rc.Close()
	lr := &io.LimitedReader{R: rc, N: limit + 1}
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("read body for model alias rewrite: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("model alias rewrite body exceeds %d bytes", limit)
	}
	return data, nil
}

func replaceRequestBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	r.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
}

func replaceResponseBody(resp *http.Response, body []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
}

func isRewriteEndpoint(method, path string) bool {
	return strings.EqualFold(method, http.MethodPost) && isModelPath(path)
}
func isModelPath(path string) bool {
	path = strings.TrimRight(path, "/")
	return path == "/v1/messages" || path == "/v1/messages/count_tokens" || path == "/v1/messages/batches"
}
func isBatchCreatePath(path string) bool {
	return strings.TrimRight(path, "/") == "/v1/messages/batches"
}
func isBatchResultsPath(method, path string) bool {
	if !strings.EqualFold(method, http.MethodGet) && !strings.EqualFold(method, http.MethodHead) {
		return false
	}
	path = strings.TrimRight(path, "/")
	return strings.HasPrefix(path, "/v1/messages/batches/") && strings.HasSuffix(path, "/results")
}
func reverseMap(forward map[string]string) map[string]string {
	if len(forward) == 0 {
		return nil
	}
	out := make(map[string]string, len(forward))
	for logical, provider := range forward {
		out[provider] = logical
	}
	return out
}
func isJSONContentType(ct string) bool {
	media, _, err := mime.ParseMediaType(ct)
	if err != nil {
		parts := strings.Fields(ct)
		if len(parts) > 0 {
			media = parts[0]
		}
	}
	media = strings.ToLower(media)
	return media == "application/json" || strings.HasSuffix(media, "+json")
}
func merge(dst, src map[string]string) {
	for k, v := range src {
		dst[k] = v
	}
}
func envSliceToMap(env []string) map[string]string {
	out := map[string]string{}
	for _, pair := range env {
		parts := strings.SplitN(pair, "=", 2)
		if len(parts) == 2 {
			out[parts[0]] = parts[1]
		}
	}
	return out
}
func dedupe(in []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}
