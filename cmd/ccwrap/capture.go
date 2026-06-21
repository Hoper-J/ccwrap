package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Hoper-J/ccwrap/internal/app"
	"github.com/Hoper-J/ccwrap/internal/model"
	"github.com/Hoper-J/ccwrap/internal/preflight"
	"github.com/Hoper-J/ccwrap/internal/profiles"
	"github.com/Hoper-J/ccwrap/internal/tlsfp"
)

// captureOpts is the parsed flag set for `ccwrap capture`.
type captureOpts struct {
	Response    bool
	TLS         bool
	TLSOnly     bool
	ClientHello bool
	Headers     bool
	Unmask      bool
	Host        string
	Path        string
	Timeout     time.Duration
	ClaudeBin   string
	ClaudeArgs  []string
	// MainInference skips lightweight quota/title/warm-up calls (no tools in the
	// request body) and captures the first substantive agent inference instead.
	MainInference bool
}

// parseCaptureArgs parses capture-specific flags. Everything after the "--"
// separator, or the first unrecognized token, is treated as CLAUDE_ARGS. With
// no claude args, a default non-interactive probe (-p hello) is injected.
func parseCaptureArgs(args []string) (captureOpts, error) {
	o := captureOpts{
		Response: true,
		Host:     "api.anthropic.com",
		Path:     "/v1/messages",
		Timeout:  30 * time.Second,
	}
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			o.ClaudeArgs = append(o.ClaudeArgs, args[i+1:]...)
			break
		}
		switch a {
		case "--no-response":
			o.Response = false
		case "--with-tls":
			o.TLS = true
		case "--tls-only":
			o.TLSOnly = true
		case "--clienthello":
			o.ClientHello = true
			o.TLS = true
		case "--headers":
			o.Headers = true
		case "--unmask":
			o.Unmask = true
		case "--full":
			o.TLS, o.ClientHello, o.Headers = true, true, true
		case "--main-inference":
			o.MainInference = true
		case "--all":
			return o, fmt.Errorf("--all is not yet supported (capture grabs the first matching exchange)")
		case "--host":
			i++
			if i >= len(args) {
				return o, fmt.Errorf("--host needs a value")
			}
			o.Host = args[i]
		case "--path":
			i++
			if i >= len(args) {
				return o, fmt.Errorf("--path needs a value")
			}
			o.Path = args[i]
		case "--claude-bin":
			i++
			if i >= len(args) {
				return o, fmt.Errorf("--claude-bin needs a value")
			}
			o.ClaudeBin = args[i]
		case "--timeout":
			i++
			if i >= len(args) {
				return o, fmt.Errorf("--timeout needs a value")
			}
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return o, fmt.Errorf("--timeout: %w", err)
			}
			o.Timeout = d
		default:
			o.ClaudeArgs = append(o.ClaudeArgs, args[i:]...)
			i = len(args)
			continue
		}
		i++
	}
	if o.TLSOnly && (o.Headers || o.TLS || o.Unmask || o.MainInference) {
		return o, fmt.Errorf("--tls-only emits only the TLS block; remove --headers/--with-tls/--full/--unmask/--main-inference")
	}
	if o.TLSOnly && !o.Response {
		return o, fmt.Errorf("--tls-only emits only the TLS block; --no-response is redundant and not allowed")
	}
	if len(o.ClaudeArgs) == 0 {
		o.ClaudeArgs = []string{"-p", "hello"}
	}
	return o, nil
}

type captureInputs struct {
	record   model.RequestRecord
	reqBody  []byte
	respBody []byte
	// respAbsent marks a captured exchange whose response body was empty or
	// never spilled (a body-less 2xx, or a connection that closed before the
	// response completed). buildCaptureResult then emits a response block with a
	// nil body and body_encoding "absent" plus an explanatory meta note, instead
	// of treating the response as captured content.
	respAbsent     bool
	respStatus     int
	tls            *tlsfp.Result
	clientHelloHex string
}

type tlsBlock struct {
	JA3            string `json:"ja3"`
	JA4            string `json:"ja4"`
	Peetprint      string `json:"peetprint"`
	ClientHelloHex string `json:"clienthello_hex,omitempty"`
}

type sideBlock struct {
	Method       string      `json:"method,omitempty"`
	Host         string      `json:"host,omitempty"`
	Path         string      `json:"path,omitempty"`
	Status       int         `json:"status,omitempty"`
	Headers      http.Header `json:"headers,omitempty"`
	Body         interface{} `json:"body,omitempty"`
	BodyEncoding string      `json:"body_encoding,omitempty"`
}

type captureMeta struct {
	ClaudeBin     string   `json:"claude_bin,omitempty"`
	CapturedAt    string   `json:"captured_at,omitempty"`
	SchemaVersion int      `json:"schema_version"`
	Unmasked      bool     `json:"unmasked"`
	Notes         []string `json:"notes,omitempty"`
}

type captureResult struct {
	TLS      *tlsBlock   `json:"tls,omitempty"`
	Request  *sideBlock  `json:"request,omitempty"`
	Response *sideBlock  `json:"response,omitempty"`
	Meta     captureMeta `json:"meta"`
}

// bodyValue decodes b as a JSON value; on failure returns the raw string.
func bodyValue(b []byte) (value interface{}, enc string) {
	var v interface{}
	if json.Unmarshal(b, &v) == nil {
		return v, "json"
	}
	return string(b), "raw"
}

func buildCaptureResult(opts captureOpts, in captureInputs) captureResult {
	res := captureResult{Meta: captureMeta{
		SchemaVersion: 1,
		Unmasked:      opts.Unmask,
		ClaudeBin:     opts.ClaudeBin,
		Notes: []string{
			"TLS fingerprint is node/undici's, not Claude app logic — it changes only when the bundled runtime changes.",
			"Bodies contain the system prompt, tool definitions, your prompt, and model output — review before sharing.",
		},
	}}

	if opts.TLS || opts.TLSOnly {
		if in.tls != nil {
			res.TLS = &tlsBlock{JA3: in.tls.JA3, JA4: in.tls.JA4, Peetprint: in.tls.Peetprint}
			if opts.ClientHello {
				res.TLS.ClientHelloHex = in.clientHelloHex
			}
		}
	}
	if opts.TLSOnly {
		return res
	}

	reqVal, reqEnc := bodyValue(in.reqBody)
	req := &sideBlock{
		Method: in.record.Method, Host: in.record.LogicalTargetHost,
		Path: in.record.Path, Body: reqVal, BodyEncoding: reqEnc,
	}
	if opts.Headers {
		// Credential headers are already masked SERVER-SIDE before they reach
		// /recent (supervisor.recordRequest), unless this capture launched its
		// child with CCWRAP_UNMASK_CREDENTIALS=1 (set in captureCommand when
		// --unmask). Emit what the record carries verbatim — capture trusts the
		// wire it polled.
		req.Headers = in.record.RequestHeaders
	}
	res.Request = req
	// --main-inference promises the prompt's real agent inference, but the
	// give-up fallback (and a synthetic auth-missing record) deliberately
	// bypasses that filter. Flag the degradation in meta so a differ can't
	// mistake a quota/title/Warmup exchange for the real one.
	if opts.MainInference && !bodyIsMainInference(in.reqBody) {
		res.Meta.Notes = append(res.Meta.Notes,
			"--main-inference: the emitted request is NOT the main agent inference (give-up fallback or synthetic record) — the real inference never matched before capture stopped.")
	}

	if opts.Response {
		if in.respAbsent {
			// The request was captured but the response body was empty or never
			// spilled. Emit an explicit absent block (nil body) so the operator
			// sees the request succeeded rather than a misleading "no request"
			// timeout.
			res.Response = &sideBlock{Status: in.respStatus, Body: nil, BodyEncoding: "absent"}
			res.Meta.Notes = append(res.Meta.Notes,
				"response body not captured (empty response, or the connection closed before the response completed)")
		} else {
			respVal, respEnc := responseBodyValue(in.respBody)
			res.Response = &sideBlock{Status: in.respStatus, Body: respVal, BodyEncoding: respEnc}
		}
		if in.respStatus >= 400 {
			res.Meta.Notes = append(res.Meta.Notes,
				fmt.Sprintf("upstream returned %d; response body is an error, not assistant content.", in.respStatus))
		}
	}
	return res
}

// responseBodyValue classifies the v1 raw response body. SSE (event-stream) is
// emitted as a string with encoding "sse"; JSON errors decode to a value.
func responseBodyValue(b []byte) (interface{}, string) {
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("event:")) || bytes.Contains(b, []byte("\ndata:")) {
		return string(b), "sse"
	}
	return bodyValue(b)
}

// captureDiffFilter is the canonical jq filter that strips per-run noise.
const captureDiffFilter = `del(.meta, .response.headers["request-id","date","anthropic-ratelimit-requests-remaining","anthropic-ratelimit-tokens-remaining","cf-ray"], .request.headers["x-client-request-id","x-claude-code-session-id"], .response.body)`

// pathWithoutQuery returns p up to (but not including) the first '?'. Capture
// matches the endpoint by path while IGNORING the query string, so a real
// request like /v1/messages?beta=true matches the /v1/messages default. Path
// segments still matter: /v1/messages/count_tokens is a different path and is
// NOT a match.
func pathWithoutQuery(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		return p[:i]
	}
	return p
}

// matchesRecord reports whether rec is the exchange capture wants. The path is
// matched exactly EXCEPT for the query string, which is ignored
// (/v1/messages?beta=true matches /v1/messages); a different path segment such
// as /v1/messages/count_tokens is still NOT a match. A synthetic record (e.g.
// ccwrap auth-missing 502) satisfies it immediately so the caller never hangs.
// Otherwise it needs the request body (always) and, unless --no-response, the
// response body to have spilled.
func matchesRecord(rec model.RequestRecord, opts captureOpts) bool {
	if pathWithoutQuery(rec.Path) != pathWithoutQuery(opts.Path) {
		return false
	}
	if opts.Host != "" && rec.LogicalTargetHost != opts.Host {
		return false
	}
	if rec.Synthetic {
		return true
	}
	if rec.BodyRef == nil {
		return false
	}
	if opts.Response && rec.ResponseBodyRef == nil {
		return false
	}
	return true
}

// matchesRequestOnly is the relaxed give-up matcher: it requires only that the
// REQUEST landed (correct path/host with a spilled body, or a synthetic record),
// ignoring ResponseBodyRef entirely. It exists so a request that succeeded but
// whose response body was empty/never spilled (a body-less 2xx, or a connection
// that closed before the response completed) is still reported instead of being
// mistaken for "no request reached the API". Only consulted on the final poll
// after the strict matchesRecord never fired.
func matchesRequestOnly(rec model.RequestRecord, opts captureOpts) bool {
	if pathWithoutQuery(rec.Path) != pathWithoutQuery(opts.Path) {
		return false
	}
	if opts.Host != "" && rec.LogicalTargetHost != opts.Host {
		return false
	}
	return rec.Synthetic || rec.BodyRef != nil
}

// fetchRecentMatch GETs <base>/recent once and returns the first record
// satisfying the strict matcher for opts (strictMatcherFor). Single-shot
// convenience: the polling loop builds the matcher ONCE instead, so the
// --main-inference body-verdict memo survives across polls.
func fetchRecentMatch(base string, opts captureOpts) (model.RequestRecord, bool) {
	return fetchRecentMatchWith(base, opts, strictMatcherFor(base, opts))
}

// strictMatcherFor returns the strict record matcher for opts. Plain capture
// is the stateless matchesRecord. Under --main-inference it holds out for the
// prompt's real agent inference: skip the lightweight quota/title calls (no
// tools) AND Claude Code's startup "Warmup" probe (tools-bearing, but its sole
// user turn is the literal text "Warmup"). Synthetic (auth-missing) records
// still match so the caller never hangs. The relaxed give-up path is
// intentionally NOT filtered, so if the real inference never lands we still
// report whatever did (flagged in meta.notes by buildCaptureResult).
//
// The returned matcher is STATEFUL: body verdicts are memoized by BodyRef.ID
// for its lifetime. Spilled bodies are immutable, so a body that parsed as
// not-main can never become main — without the memo the 100ms polling loop
// re-downloads every candidate body on every tick. Fetch failures are NOT
// memoized (the body may simply not have spilled yet; best-effort false now,
// retry next poll).
func strictMatcherFor(base string, opts captureOpts) func(model.RequestRecord, captureOpts) bool {
	if !opts.MainInference {
		return matchesRecord
	}
	verdict := map[string]bool{}
	return func(rec model.RequestRecord, o captureOpts) bool {
		if !matchesRecord(rec, o) {
			return false
		}
		if rec.Synthetic {
			return true
		}
		if rec.BodyRef == nil {
			return false
		}
		if v, ok := verdict[rec.BodyRef.ID]; ok {
			return v
		}
		b, err := fetchBody(base, rec.BodyRef.ID, 3, 30*time.Millisecond)
		if err != nil {
			return false
		}
		v := bodyIsMainInference(b)
		verdict[rec.BodyRef.ID] = v
		return v
	}
}

// bodyIsMainInference reports whether a captured request body is the prompt's
// real agent inference: it must carry a non-empty tools array (vs a lightweight
// quota/title call) AND not be Claude Code's startup "Warmup" probe — a
// tools-bearing request whose last user turn is exactly the text "Warmup", fired
// before the -p prompt's inference. Pure (parses bytes only) so it unit-tests
// without a live body store.
func bodyIsMainInference(b []byte) bool {
	var body struct {
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(b, &body); err != nil {
		return false
	}
	if len(body.Tools) == 0 {
		return false
	}
	for i := len(body.Messages) - 1; i >= 0; i-- {
		if body.Messages[i].Role != "user" {
			continue
		}
		return strings.TrimSpace(lastText(body.Messages[i].Content)) != "Warmup"
	}
	return true
}

// lastText returns the trailing text of a message Content field, which is either
// a JSON string or an array of {type,text} blocks. Claude Code appends the -p
// prompt (or "Warmup") as the final text block, so the last block's text is the
// signal we fingerprint.
func lastText(content json.RawMessage) string {
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err == nil {
		for i := len(blocks) - 1; i >= 0; i-- {
			if blocks[i].Text != "" {
				return blocks[i].Text
			}
		}
	}
	return ""
}

// fetchRecentMatchRequestOnly GETs <base>/recent once and returns the first
// record satisfying the relaxed matchesRequestOnly (request landed, response
// ignored). Used only on the give-up poll. It does NOT filter on --main-inference:
// if the strict matcher never found the real agent inference, reporting whatever
// landed (including a quota/warm-up call) is more useful than failing outright —
// the auxiliary-vs-main distinction is classified downstream from the captured
// body, not enforced here.
func fetchRecentMatchRequestOnly(base string, opts captureOpts) (model.RequestRecord, bool) {
	return fetchRecentMatchWith(base, opts, matchesRequestOnly)
}

// fetchRecentMatchWith GETs <base>/recent once and returns the first record for
// which match reports true.
func fetchRecentMatchWith(base string, opts captureOpts, match func(model.RequestRecord, captureOpts) bool) (model.RequestRecord, bool) {
	var out struct {
		Requests []model.RequestRecord `json:"requests"`
	}
	if err := getJSONInto(base+"/recent", &out); err != nil {
		return model.RequestRecord{}, false
	}
	for _, rec := range out.Requests {
		if match(rec, opts) {
			return rec, true
		}
	}
	return model.RequestRecord{}, false
}

// fetchBody GETs <base>/recent/body?id=<id>, retrying on a 404/empty (the body
// store spills asynchronously, so a just-set ref may not be on disk yet).
func fetchBody(base, id string, retries int, every time.Duration) ([]byte, error) {
	u := base + "/recent/body?id=" + url.QueryEscape(id)
	var lastErr error
	for i := 0; i <= retries; i++ {
		b, code, err := getBytes(u)
		if err == nil && code == 200 && len(b) > 0 {
			return b, nil
		}
		lastErr = err
		time.Sleep(every)
	}
	return nil, fmt.Errorf("body %s not available after %d retries: %v", id, retries, lastErr)
}

func getJSONInto(u string, out interface{}) error {
	b, code, err := getBytes(u)
	if err != nil {
		return err
	}
	if code != 200 {
		return fmt.Errorf("GET %s: status %d", u, code)
	}
	return json.Unmarshal(b, out)
}

func getBytes(u string) ([]byte, int, error) {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(u)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.StatusCode, err
}

// fetchTLS GETs the captured ClientHello from the session proxy and computes
// the fingerprint. It returns a nil result (and no error) when no hello has
// been captured yet — TLS is best-effort and never fails the capture.
func fetchTLS(base string) (*tlsfp.Result, string) {
	raw, _, _ := getBytes(base + "/native-tls/clienthello.bin")
	if len(raw) == 0 {
		return nil, ""
	}
	r, err := tlsfp.Compute(raw)
	if err != nil {
		return nil, ""
	}
	return &r, hex.EncodeToString(raw)
}

// runCaptureLoop polls <base>/recent until a matching record appears
// (matchesRecord), then fetches the request/response bodies and TLS per opts
// and assembles the result. It returns a non-nil error only on a hard failure
// (no match before the deadline). childExited (may be nil) lets the loop abort
// early when the spawned child exits — after the channel fires the loop does
// one final poll then gives up. The synthetic return is true when the matched
// record was a synthetic (e.g. auth-missing 502) so the caller can choose a
// non-zero exit even though the object itself is well-formed.
func runCaptureLoop(base string, opts captureOpts, deadline time.Time, childExited <-chan struct{}) (captureResult, bool, error) {
	// --tls-only: no request record needed. Poll for the ClientHello until it
	// is captured or the deadline passes.
	if opts.TLSOnly {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			if tls, hexStr := fetchTLS(base); tls != nil {
				in := captureInputs{tls: tls, clientHelloHex: hexStr}
				return buildCaptureResult(opts, in), false, nil
			}
			if time.Now().After(deadline) {
				return captureResult{}, false, fmt.Errorf(
					"no native-TLS ClientHello captured within %s — is native-TLS enabled and a request attempted?", opts.Timeout)
			}
			<-tick.C
		}
	}

	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	childGone := false
	// Build the strict matcher once so the --main-inference body-verdict memo
	// persists across polls (see strictMatcherFor).
	match := strictMatcherFor(base, opts)
	for {
		if rec, ok := fetchRecentMatchWith(base, opts, match); ok {
			return assembleCapture(base, opts, rec), rec.Synthetic, nil
		}
		// childExited gives us one more poll (handled by the loop's top) then
		// we give up: the child has gone but no STRICT match landed. Before
		// failing, try the relaxed give-up path so a body-less successful
		// response is reported instead of timing out.
		if childGone || time.Now().After(deadline) {
			return giveUp(base, opts)
		}
		select {
		case <-tick.C:
		case <-childExited:
			// Mark the child as gone; the loop polls once more before bailing.
			childGone = true
			childExited = nil
		}
	}
}

// giveUp runs the final relaxed poll after the strict matcher never fired (the
// child exited or the deadline passed). It does ONE more /recent fetch with the
// request-only matcher so a captured request whose response body was empty or
// never spilled is still reported:
//
//   - a relaxed match whose ResponseBodyRef is non-nil → assemble normally (the
//     response simply landed between the last strict poll and this one);
//   - a relaxed match whose ResponseBodyRef is nil → assemble the request and
//     emit an absent response block + note (success for a non-synthetic record;
//     the synthetic flag is preserved so the caller keeps the non-zero exit);
//   - no relaxed match at all → the canonical no-request error.
func giveUp(base string, opts captureOpts) (captureResult, bool, error) {
	rec, ok := fetchRecentMatchRequestOnly(base, opts)
	if !ok {
		return captureResult{}, false, noMatchError(opts)
	}
	// With --no-response, or when the response actually spilled, the normal
	// assembly path is correct.
	if !opts.Response || rec.ResponseBodyRef != nil {
		return assembleCapture(base, opts, rec), rec.Synthetic, nil
	}
	// Request landed but no response body: emit it as absent.
	return assembleCaptureAbsentResponse(base, opts, rec), rec.Synthetic, nil
}

// assembleCaptureAbsentResponse fetches the request body (and TLS) for a matched
// record whose response body was never captured, then builds a result with the
// response marked absent. Mirrors assembleCapture minus the response fetch.
func assembleCaptureAbsentResponse(base string, opts captureOpts, rec model.RequestRecord) captureResult {
	in := captureInputs{record: rec, respStatus: rec.StatusCode, respAbsent: true}
	if rec.BodyRef != nil {
		if b, err := fetchBody(base, rec.BodyRef.ID, 20, 50*time.Millisecond); err == nil {
			in.reqBody = b
		}
	}
	if opts.TLS || opts.TLSOnly {
		if tls, hexStr := fetchTLS(base); tls != nil {
			in.tls = tls
			in.clientHelloHex = hexStr
		}
	}
	return buildCaptureResult(opts, in)
}

// assembleCapture fetches the request/response bodies and TLS for a matched
// record and builds the capture result. A synthetic record carries the
// upstream StatusCode (502 for auth-missing) so buildCaptureResult emits the
// >=400 note; it may have no spilled bodies.
func assembleCapture(base string, opts captureOpts, rec model.RequestRecord) captureResult {
	in := captureInputs{record: rec, respStatus: rec.StatusCode}
	if rec.BodyRef != nil {
		if b, err := fetchBody(base, rec.BodyRef.ID, 20, 50*time.Millisecond); err == nil {
			in.reqBody = b
		}
	}
	if opts.Response && !rec.Synthetic && rec.ResponseBodyRef != nil {
		if b, err := fetchBody(base, rec.ResponseBodyRef.ID, 20, 50*time.Millisecond); err == nil {
			in.respBody = b
		}
	}
	if opts.TLS || opts.TLSOnly {
		if tls, hexStr := fetchTLS(base); tls != nil {
			in.tls = tls
			in.clientHelloHex = hexStr
		}
	}
	return buildCaptureResult(opts, in)
}

// noMatchError is the canonical deadline/no-match failure. It names the host
// and path so the operator can see what capture was waiting for.
func noMatchError(opts captureOpts) error {
	return fmt.Errorf(
		"no request to %s%s within %s — is Claude authenticated and pointed at the API?",
		opts.Host, opts.Path, opts.Timeout)
}

// captureCommand implements `ccwrap capture`: it launches a one-shot Claude
// session through the proxy (mirroring runClaude's composition minus the
// interactive Attach), polls the session for the matching request, and prints
// the assembled capture object as JSON. The exit code is decoupled from
// Claude's: success when a request was captured (even a 4xx upstream), non-zero
// only for no-match/timeout/launch-failure or a synthetic auth-missing record.
func captureCommand(paths app.Paths, args []string) error {
	// --print-diff-filter is a pure local query: emit the canonical jq filter
	// and exit before any parsing or launch. Detected on the raw args (it is
	// not a parsed capture flag).
	for _, a := range args {
		if a == "--print-diff-filter" {
			fmt.Println(captureDiffFilter)
			return nil
		}
	}

	opts, err := parseCaptureArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccwrap capture: "+err.Error())
		return err
	}

	// --unmask wants raw secrets in the output. Header masking is now done
	// SERVER-SIDE — the record is masked before it ever reaches /recent (see
	// supervisor.recordRequest + ui.MaskCredentialHeaders) — so capture can no
	// longer unmask after the fact. Instead it sets the same launch-time opt-in
	// the supervisor honors, so THIS capture's child supervisor stores raw
	// credentials (request headers AND bodies) and serves them on /recent. The
	// supervisor prints its own one-time stderr warning; capture adds a second
	// below. Set before the child supervisor starts (New reads the env once).
	if opts.Unmask {
		_ = os.Setenv("CCWRAP_UNMASK_CREDENTIALS", "1")
	}

	launch := launchArgs{
		CaptureBodies: !opts.TLSOnly,
		NoInit:        true,
		Quiet:         true,
		ClaudeBin:     opts.ClaudeBin,
		ClaudeArgs:    opts.ClaudeArgs,
		NativeTLS:     nativeTLSEnabled(os.Getenv("CCWRAP_NATIVE_TLS")),
	}
	if launch.ClaudeBin == "" {
		launch.ClaudeBin = envDefault("CLAUDE_BIN", "claude")
	}

	// Mirror runClaude's native-TLS prelude: warn loudly if mirroring is
	// disabled (security downgrade) and fail-fast load + validate an externally
	// pinned ClientHello, enforcing the HELLO-set-with-NATIVE_TLS=0 mutual
	// exclusion. Stdout stays JSON-only, so the JA3 confirmation line is omitted;
	// the disabled warning goes to stderr.
	if w := nativeTLSDisabledWarning(os.Getenv("CCWRAP_NATIVE_TLS")); w != "" {
		fmt.Fprintln(os.Stderr, w)
	}
	hello, err := loadNativeTLSHello(os.Getenv("CCWRAP_NATIVE_TLS_HELLO"), launch.NativeTLS)
	if err != nil {
		return err
	}
	if hello != nil {
		launch.NativeTLSHello = hello
	}

	// Mirror runClaude's prelude. NoInit=true makes maybeMigrateFromEnv a
	// no-op; sweepOrphanSessions + EnsureOfficialProfile + resolveLaunchProfile
	// + composeLaunch are the same launch-composition path.
	sweepOrphanSessions(paths)
	cwd, _ := os.Getwd()
	if err := profiles.EnsureOfficialProfile(paths.StateDir); err != nil {
		fmt.Fprintf(os.Stderr, "ccwrap: ensure official profile: %v\n", err)
	}
	maybeMigrateFromEnv(paths.StateDir, os.Environ(), cwd, launch.ClaudeArgs, launch.NoInit)
	profileOverlay, err := resolveLaunchProfile(profiles.DefaultPath(paths.StateDir), launch.Profile)
	if err != nil {
		return err
	}
	inspect, pre, outOpts, err := composeLaunch(preflight.Options{
		Upstream:                         launch.Upstream,
		EgressProxy:                      launch.EgressProxy,
		ParentEnv:                        os.Environ(),
		WorkingDirectory:                 cwd,
		ChildArgs:                        launch.ClaudeArgs,
		ModelAliasFile:                   launch.ModelAliasFile,
		ModelAliasPairs:                  launch.ModelAliases,
		UpstreamHeaderFile:               launch.UpstreamHeadersFile,
		UpstreamHeaderPairs:              launch.UpstreamHeaders,
		AllowProviderModelPassthrough:    launch.AllowProviderModelPassthrough,
		AllowAuthPassthroughToThirdParty: launch.AllowAuthPassthroughToThirdParty,
		Profile:                          profileOverlay,
	})
	if err != nil {
		return err
	}

	l := newSessionLauncher(paths, launch, pre, inspect, outOpts, cwd, newID())
	defer l.rollback.run()
	defer l.cancelCtx()
	// Keep Claude's print output off our stdout so the only thing on stdout is
	// the capture JSON.
	l.childStdout = io.Discard

	steps := []func() error{
		l.PreparePaths,
		l.StartSupervisor,
		l.AwaitControl,
		l.CreateSession,
		l.WriteSessionSettings,
		l.SpawnChild,
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}

	base := "http://" + l.session.ProxyListenAddr

	// Wait for the child in a goroutine so the capture loop can abort early
	// when Claude exits. We never propagate Claude's exit code.
	childExited := make(chan struct{})
	go func() {
		_, _ = l.Wait()
		close(childExited)
	}()

	res, synthetic, err := runCaptureLoop(base, opts, time.Now().Add(opts.Timeout), childExited)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccwrap capture: "+err.Error())
		return err
	}
	if opts.Unmask {
		fmt.Fprintln(os.Stderr,
			"ccwrap capture: --unmask — credential VALUES (request headers AND OAuth body fields) are emitted in cleartext; do not share this output.")
	}
	if err := printJSON(res); err != nil {
		return err
	}
	if synthetic {
		// A synthetic auth-missing record is a config error: the object is
		// well-formed (and was printed) but the exit code must be non-zero.
		return fmt.Errorf(
			"captured a synthetic ccwrap response (no live request reached %s%s — is Claude authenticated?)",
			opts.Host, opts.Path)
	}
	return nil
}
