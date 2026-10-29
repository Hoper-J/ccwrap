package uiservice

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/Hoper-J/ccwrap/internal/preflight"
)

// Activity rows read as the session page reads them: method, path without
// ?beta=true, the model view, the failure; credential headers masked; the
// host ccwrap sent the request to, against the one Claude Code asked for.
func TestSessionActivityRows(t *testing.T) {
	out := runModule(t, `
import { activityRow, headerGroups, healthWord } from '$JS/session/model.js';
const deny = new Set(['authorization', 'x-api-key']);
const req = {
  timestamp: '2026-10-28T10:00:00Z', method: 'POST', path: '/v1/messages?beta=true', status_code: 404, latency_ms: 12,
  stream_state: 'http', logical_target_host: 'api.anthropic.com', actual_upstream_host: 'gw.example', model: 'claude-opus-5-5', model_unmapped: true,
  request_headers: { 'Authorization': ['Bearer sk-x'], 'Anthropic-Version': ['2023-06-01'], 'User-Agent': ['claude-cli/2'], 'X-Odd': ['1'] },
  body_ref: { id: 'b1' }, response_body_ref: { id: 'r1' },
  failure: { source: 'upstream', type: 'not_found_error', message: 'model not found: claude-opus-5-5', hint: 'No alias…' },
};
const r = activityRow(req, 'forwarded-api', { deny, unmask: false });
const u = activityRow(req, 'forwarded-api', { deny, unmask: true });
const same = activityRow({ ...req, actual_upstream_host: 'api.anthropic.com' }, 'forwarded-api', { deny, unmask: false });
const e = activityRow({ timestamp: 't', severity: 'warn', summary: 'boom', count: 3, error_class: 'upstream_http_429', upstream_host: 'gw', suggested_action: 'wait' }, 'error', {});
const tr = activityRow({ timestamp: 't', category: 'profile_switch', summary: 'switched', detail: '{"from":"a","to":"b","to_provider":"B","class":"live"}' }, 'trace', {});
const tunnel = activityRow({ timestamp: 't', method: 'CONNECT', path: 'github.com:443', status_code: 200 }, 'tunnel', { deny, unmask: false });
console.log(JSON.stringify({ r, u: u.headers, same: same.headers[0], e, tr, tunnel,
  words: [healthWord({ state: 'active', session_health: 'ok' }), healthWord({ state: 'active', session_health: 'warn' }), healthWord({ state: 'attached', session_health: 'error' }), healthWord({ state: 'ended', session_health: 'error' })] }));
`)
	var got struct {
		R struct {
			Main, Right, St, BodyID, ResponseBodyID string
			Forwarded                               bool
			Headers                                 []headerGroup
			Failure                                 struct{ Head, Message string }
		}
		U    []headerGroup
		Same headerGroup
		E    struct{ Label, Severity, Main, Right string }
		Tr   struct {
			Switch struct{ From, To, ToProvider string }
		}
		Tunnel struct {
			Note    string
			Headers any
		}
		Words []string
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	r := got.R
	if r.Main != "POST /v1/messages · claude-opus-5-5 (no alias)" || r.Right != "404 model not found: claude-opus-5-5 · 12 ms · http · gw.example" || r.St != "warn" || !r.Forwarded {
		t.Errorf("row = %+v", r)
	}
	if r.BodyID != "b1" || r.ResponseBodyID != "r1" || r.Failure.Head != "upstream returned 404 · not_found_error" {
		t.Errorf("inspector = %+v", r)
	}
	var groups []string
	for _, g := range r.Headers {
		groups = append(groups, g.Name)
	}
	if strings.Join(groups, ",") != "Routing,Protocol & versioning,Client identity,Credentials,Other" {
		t.Errorf("groups = %v", groups)
	}
	if host := r.Headers[0].Rows[0]; host.Name != "host" || host.Value != "gw.example" || host.Was != "api.anthropic.com" || host.Tag != "rewritten" {
		t.Errorf("rewritten host row = %+v", host)
	}
	if host := got.Same.Rows[0]; got.Same.Name != "Routing" || host.Value != "api.anthropic.com" || host.Was != "" || host.Tag != "" {
		t.Errorf("unchanged host row = %+v", got.Same)
	}
	if cred := groupRows(r.Headers, "Credentials")[0]; cred.Value != "‹redacted by ccwrap›" || !cred.Redacted {
		t.Errorf("credential row = %+v", cred)
	}
	if cred := groupRows(got.U, "Credentials")[0]; cred.Value != "Bearer sk-x" || cred.Redacted {
		t.Errorf("unmasked credential row = %+v", cred)
	}
	if got.E.Label != "warning" || got.E.Severity != "warn" || got.E.Main != "boom ×3" || got.E.Right != "upstream_http_429 · gw · wait" {
		t.Errorf("error row = %+v", got.E)
	}
	if s := got.Tr.Switch; s.From != "a" || s.To != "b" || s.ToProvider != "B" {
		t.Errorf("switch = %+v", s)
	}
	if got.Tunnel.Headers != nil || got.Tunnel.Note != "" {
		t.Errorf("a tunnel row has no inspector: %+v", got.Tunnel)
	}
	if strings.Join(got.Words, ",") != "active,degraded,error,ended" {
		t.Errorf("health words = %v", got.Words)
	}
}

// The inspector tags what ccwrap did to each header on the way upstream —
// rewritten, stripped, or injected (a header Claude Code did not send) —
// and never shows a value ccwrap set; the summary counts each kind.
func TestSessionHeaderDiff(t *testing.T) {
	out := runModule(t, `
import { headerGroups, headerSummary } from '$JS/session/model.js';
const deny = new Set(['authorization', 'x-api-key']);
const headers = { 'Authorization': ['Bearer sk-ph‹redacted 9 chars›'], 'Anthropic-Beta': ['a,b'], 'User-Agent': ['claude-cli/2'], 'Accept-Encoding': ['gzip'] };
const diff = { changes: [
  { name: 'Accept-Encoding', action: 'rewritten' }, { name: 'Anthropic-Beta', action: 'rewritten' },
  { name: 'Authorization', action: 'stripped' }, { name: 'X-Api-Key', action: 'injected' }, { name: 'X-Team', action: 'injected' },
] };
const groups = headerGroups(headers, deny, false, { host: 'gw.example', was: 'api.anthropic.com' }, diff);
const rows = {};
for (const g of groups) for (const r of g.rows) rows[r.name] = { group: g.name, ...r };
console.log(JSON.stringify({ rows, compared: headerSummary(groups, true), plain: headerSummary(groups, false) }));
`)
	var got struct {
		Rows map[string]struct {
			Group, Value, Was, Tag string
			Set, Struck, Redacted  bool
		}
		Compared, Plain [][]any
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	r := got.Rows
	if a := r["Authorization"]; a.Tag != "stripped" || !a.Struck || !a.Redacted || a.Group != "Credentials" {
		t.Errorf("stripped credential = %+v", a)
	}
	if k := r["X-Api-Key"]; k.Tag != "injected" || !k.Set || k.Value != "set by ccwrap" || k.Group != "Credentials" {
		t.Errorf("injected key = %+v", k)
	}
	if x := r["X-Team"]; x.Tag != "injected" || x.Value != "set by ccwrap" || x.Group != "Other" {
		t.Errorf("injected upstream header = %+v", x)
	}
	if b := r["Anthropic-Beta"]; b.Tag != "rewritten" || b.Was != "a,b" || b.Value != "set by ccwrap" || b.Group != "Protocol & versioning" {
		t.Errorf("rewritten header = %+v", b)
	}
	if u := r["User-Agent"]; u.Tag != "" || u.Value != "claude-cli/2" {
		t.Errorf("passthrough header = %+v", u)
	}
	if fmt.Sprint(got.Compared) != "[[rewritten 3] [injected 2] [stripped 1] [redacted 1] [passthrough 1]]" {
		t.Errorf("compared summary = %v", got.Compared)
	}
	if fmt.Sprint(got.Plain) != "[[headers 7] [groups 6] [rewritten 3] [redacted 1]]" {
		t.Errorf("plain summary = %v", got.Plain)
	}
}

// headerGroup is one group of an inspector's request headers.
type headerGroup struct {
	Name string
	Rows []headerRow
}

type headerRow struct {
	Name, Value, Was, Tag string
	Redacted              bool
}

func groupRows(groups []headerGroup, name string) []headerRow {
	for _, g := range groups {
		if g.Name == name {
			return g.Rows
		}
	}
	return nil
}

// Filters count and cap the retained activity, newest first; live records
// fold the way the session's rings do.
func TestSessionActivityFilters(t *testing.T) {
	out := runModule(t, `
import { counts, visibleEntries, defaultFilter, addRecord, RETAIN } from '$JS/session/model.js';
let act = { requests: [
  { timestamp: '2026-10-28T10:00:01Z', class: 'forwarded-api' },
  { timestamp: '2026-10-28T10:00:03Z', class: 'tunnel' },
], errors: [{ timestamp: '2026-10-28T10:00:02Z', error_class: 'x', summary: 's', upstream_host: 'h', count: 1 }], trace: [] };
const before = counts(act);
const all = visibleEntries(act, 'all', 10).map(([r, c]) => c);
const limited = visibleEntries(act, 'all', 2).length;
act = addRecord(act, 'error', { timestamp: '2026-10-28T10:00:04Z', error_class: 'x', summary: 's', upstream_host: 'h', count: 2 });
const folded = act.errors.length;
for (let i = 0; i < RETAIN.requests + 5; i++) act = addRecord(act, 'request', { timestamp: 'x', class: 'forwarded-api' });
console.log(JSON.stringify({ before, all, limited, folded, capped: act.requests.length,
  def: [defaultFilter({ requests: [{ class: 'tunnel' }], errors: [], trace: [] }), defaultFilter({ requests: [{ class: 'forwarded-api' }], errors: [], trace: [] })] }));
`)
	want := `{"before":{"all":3,"error":1,"trace":0,"forwarded-api":1,"tunnel":1},"all":["tunnel","error","forwarded-api"],"limited":2,"folded":1,"capped":250,"def":["all","forwarded-api"]}`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got  %s\nwant %s", strings.TrimSpace(out), want)
	}
}

// A captured stream is put back together: text, thinking, model, stop,
// output tokens.
func TestSessionBodyReassembly(t *testing.T) {
	out := runModule(t, `
import { isSSEBody, reassembleSSE, anatomy } from '$JS/session/bodies.js';
const sse = 'event: message_start\r\ndata: {"type":"message_start","message":{"model":"m1","usage":{"output_tokens":1}}}\r\n\r\n' +
  'event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}\n\n' +
  'event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"text_delta","text":"Hel"}}\n\n' +
  'event: content_block_delta\ndata: {"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}\n\n' +
  'event: message_delta\ndata: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}\n\n';
const body = JSON.stringify({ model: 'x', messages: [{ role: 'user', content: 'hi' }], system: 'y'.repeat(300) });
console.log(JSON.stringify({ sse: isSSEBody(sse), json: isSSEBody('{"a":1}'), r: reassembleSSE(sse), a: anatomy(JSON.parse(body), body.length).map((x) => x.key + ' ' + x.pct) }));
`)
	want := `{"sse":true,"json":false,"r":{"text":"Hello","thinking":"hmm","model":"m1","stop":"end_turn","outTok":9,"events":5},"a":["system 82%","messages 9%","model 1%"]}`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got  %s\nwant %s", strings.TrimSpace(out), want)
	}
}

// The ribbon's own words: the egress probe line, the alias table, issue
// titles and evidence, the header meta line.
func TestSessionWords(t *testing.T) {
	out := runModule(t, `
import { egressLine, aliasRows } from '$JS/session/ribbon.js';
import { issueTitle, issueEvidence } from '$JS/session/issues.js';
import { metaLine } from '$JS/session/header.js';
const now = Date.parse('2026-10-28T10:00:00Z');
console.log(JSON.stringify({
  ok: egressLine({ status: 'OK', public_ip: '203.0.113.9', city: 'Tokyo', country: 'jp', org: 'AS1', latency_ms: 120 }),
  fail: egressLine({ status: 'NET_FAIL', latency_ms: 0, err: 'refused' }),
  aliases: aliasRows({ 'claude-opus-5-5': 'gpt-6', '*': '*', 'claude-haiku-5': 'mini' }),
  title: [issueTitle({ title: 'claude-opus-5-5 has no alias', title_code: 'claude-opus-5-5' }), issueTitle({ title: 'Rate limited', title_code: '' })],
  evidence: [issueEvidence({ evidence: '429 slow · 2 requests', last_seen_at: '2026-10-28T09:58:00Z' }, now), issueEvidence({ evidence: '', last_seen_at: '' }, now)],
  meta: [metaLine({ active_profile_name: 'deepseek', state: 'active', claude_pid: 42, created_at: '2026-10-28T09:48:00Z', last_activity_at: '2026-10-28T09:59:52Z' }, now),
    metaLine({ state: 'ended', ended_at: '2026-10-28T09:59:20Z' }, now)],
}));
`)
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	b, _ := json.Marshal(got)
	for _, want := range []string{
		`"ok":{"ok":true,"text":"✓ 203.0.113.9 · Tokyo, 🇯🇵 JP · AS1 · 120ms"}`,
		`"fail":{"ok":false,"text":"✗ NET_FAIL · 0ms · refused"}`,
		`"aliases":[{"from":"claude-haiku-5","to":"mini"},{"from":"claude-opus-5-5","to":"gpt-6"},{"from":"other Claude models","other":true,"to":"keep their Claude names"}]`,
		`"title":[{"code":"claude-opus-5-5","rest":" has no alias"},{"code":"","rest":"Rate limited"}]`,
		`"evidence":["429 slow · 2 requests · last 2m ago",""]`,
		`"meta":["deepseek · active · claude pid 42 · up 12m · last activity 8s ago","no profile · ended 40s ago"]`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s\nin %s", want, b)
		}
	}
}

// Projects: search, live running status, and which launches a new session
// answers.
func TestProjectsPage(t *testing.T) {
	out := runModule(t, `
import { matches, running, runningText, worstHealth, settleLaunches } from '$JS/projects.js';
const now = Date.parse('2026-10-28T10:00:00Z');
const p = { name: 'sub2api', path: '/Users/me/work/sub2api', display_path: '~/work/sub2api', git_branch: 'feat/tls', last_title: 'Port native TLS replay' };
const sessions = [
  { id: 'a', project_dir: '/Users/me/work/sub2api', state: 'active', profile: 'official', health: 'warn', started_at: '2026-10-28T09:48:00Z' },
  { id: 'b', project_dir: '/Users/me/work/sub2api', state: 'ended', health: 'error', started_at: '2026-10-28T09:00:00Z' },
  { id: 'c', project_dir: '/Users/me/other', state: 'active', health: 'error', started_at: '2026-10-28T09:59:00Z' },
];
const live = running(p, sessions);
const launches = {
  '/Users/me/work/sub2api': { state: 'opening', since: Date.parse('2026-10-28T09:47:59Z') },
  '/Users/me/other': { state: 'opening', since: Date.parse('2026-10-28T09:59:30Z') },
  '/Users/me/x': { state: 'rejected', since: 0, message: 'no' },
};
const settled = settleLaunches(launches, sessions);
console.log(JSON.stringify({
  m: ['SUB2', 'feat/tls', 'native tls', '~/work', 'nope', ''].map((q) => matches(p, q)),
  live: live.map((s) => s.id),
  one: runningText(live, now),
  two: runningText([live[0], sessions[2]], now),
  worst: [worstHealth(live), worstHealth([live[0], sessions[2]]), worstHealth([])],
  left: Object.keys(settled.launches).sort(), started: settled.started.map((s) => s.id),
}));
`)
	want := `{"m":[true,true,true,true,false,true],"live":["a"],"one":"running · official · 12m","two":"2 sessions running","worst":["warn","error","ok"],"left":["/Users/me/other","/Users/me/x"],"started":["a"]}`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got  %s\nwant %s", strings.TrimSpace(out), want)
	}
}

// The profile form saves only what changed, keeps stored keys when the key
// is left blank, and gates saving the way the session page's editor did.
func TestProfileForm(t *testing.T) {
	out := runModule(t, `
import { aliasDefaultChoices, fallbackDefault, draftFrom, aliasMap, problems, editPayload, addPayload } from '$JS/profiles/form.js';
import { authLabel, aliasCount } from '$JS/profiles/list.js';
const orig = { name: 'ds', provider: 'DeepSeek', base_url: 'https://api.deepseek.com/anthropic', auth: { mode: 'ccwrap_bearer', has_key_env: true },
  model_aliases: { 'claude-sonnet-5-5': 'v4', '*': 'v4' }, egress_mode: 'inherit', egress_url: '' };
const d = draftFrom(orig);
const same = editPayload(orig, d);
const e1 = { ...draftFrom(orig), provider: 'DS', key: 'NEW_ENV' };
const e2 = { ...draftFrom(orig), auth_mode: 'passthrough' };
const e3 = { ...draftFrom(orig), key_source: 'inline' };
const e4 = { ...draftFrom(orig), egress_mode: 'http', egress_url: 'http://p:8080', other: '*' };
const fresh = { ...draftFrom(null), name: 'glm', base_url: 'https://x.example/v1', key: 'sk-1', aliases: [{ from: 'claude-opus-5-5', to: 'glm-5' }, { from: '', to: '' }], other: 'glm-5' };
console.log(JSON.stringify({
  choicesNew: aliasDefaultChoices(['glm-5'], '', true, false),
  choicesEdit: aliasDefaultChoices(['a', 'b'], 'gone', false, false).options.map((o) => o[0]),
  suggested: aliasDefaultChoices(['a'], '', false, false).options[1][1],
  fallback: [fallbackDefault(['official', 'zeta', 'ds'], 'ds'), fallbackDefault(['zeta', 'ds'], 'ds'), fallbackDefault(['ds'], 'ds')],
  draft: { auth: d.auth_mode, src: d.key_source, aliases: d.aliases, other: d.other },
  same, e1: editPayload(orig, e1), e2: editPayload(orig, e2), e3: problems(e3, orig, []).errs, e4: editPayload(orig, e4),
  fresh: problems(fresh, null, ['glm']), add: addPayload({ ...fresh, name: 'glm2' }),
  aliasMap: aliasMap({ aliases: [{ from: ' a ', to: ' b ' }, { from: 'c', to: '' }], other: '__custom', otherCustom: ' m ' }),
  auth: [authLabel(null), authLabel({ mode: 'ccwrap_bearer', has_key_env: true }), authLabel({ mode: 'ccwrap_x_api_key', has_inline_key: true })],
  count: aliasCount(orig),
}));
`)
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%s: %v", out, err)
	}
	want := map[string]string{
		"choicesNew":  `{"options":[["","not set"],["glm-5","glm-5"],["*","keep their Claude names"],["__custom","other model…"]],"value":"glm-5"}`,
		"choicesEdit": `["","a","b","gone","*","__custom"]`,
		"suggested":   `"a (suggested)"`,
		"fallback":    `["official","zeta","inherit-env"]`,
		"draft":       `{"auth":"ccwrap_bearer","src":"env_var","aliases":[{"from":"claude-sonnet-5-5","to":"v4"}],"other":"v4"}`,
		"same":        `{}`,
		"e1":          `{"provider":"DS","auth":{"key_source":"env_var","key_env":"NEW_ENV"}}`,
		"e2":          `{"auth":null}`,
		"e3":          `{"key":"Paste the key."}`,
		"e4":          `{"model_aliases":{"claude-sonnet-5-5":"v4","*":"*"},"egress":{"mode":"http","url":"http://p:8080"}}`,
		"fresh":       `{"errs":{"name":"A profile with this name exists."},"warns":{"base_url":"Claude Code adds /v1 itself, so this would send requests to /v1/v1/messages. Drop the /v1."},"fixes":{}}`,
		"add":         `{"name":"glm2","provider":"","base_url":"https://x.example/v1","auth":{"mode":"ccwrap_bearer","key":"sk-1"},"egress":{"mode":"inherit","url":""},"model_aliases":{"claude-opus-5-5":"glm-5","*":"glm-5"},"set_default":false}`,
		"aliasMap":    `{"a":"b","*":"m"}`,
		"auth":        `["passthrough","bearer · env","x-api-key · inline"]`,
		"count":       `2`,
	}
	for k, w := range want {
		if string(got[k]) != w {
			t.Errorf("%s:\n got  %s\n want %s", k, got[k], w)
		}
	}
}

// The posture sentence bolds the upstream host and the alias count.
func TestSessionPostureEmphasis(t *testing.T) {
	out := runModule(t, `
import { emphasisParts } from '$JS/session/header.js';
console.log(JSON.stringify(emphasisParts('Routing Claude Code through api.deepseek.com. CCWRAP holds your gateway credentials and applies 2 model aliases; Claude Code only sees logical names.', ['api.deepseek.com', ''])));
`)
	want := `[{"text":"Routing Claude Code through ","bold":false},{"text":"api.deepseek.com","bold":true},{"text":". CCWRAP holds your gateway credentials and applies ","bold":false},{"text":"2 model aliases","bold":true},{"text":"; Claude Code only sees logical names.","bold":false}]`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got  %s\nwant %s", strings.TrimSpace(out), want)
	}
}

// The profile form flags OpenAI-compatible base URLs as the server does,
// from the table the page is served with, and offers the fix it knows.
func TestProfileFormOpenAIBase(t *testing.T) {
	urls := []string{
		"https://dashscope.aliyuncs.com/compatible-mode/v1",
		"https://dashscope.aliyuncs.com/apps/anthropic",
		"https://ark.cn-beijing.volces.com/api/v3/",
		"https://ark.cn-beijing.volces.com/api/coding/v3",
		"https://API.DeepSeek.com",
		"https://api.deepseek.com/anthropic",
		"https://api.siliconflow.cn/v1",
		"https://api.siliconflow.cn",
		"https://gw.example/openai/v1/chat/completions",
		"https://gw.example/compatible-mode/v1",
		"https://gw.example/v1",
	}
	table, _ := json.Marshal(preflight.OpenAIEndpoints())
	in, _ := json.Marshal(urls)
	out := runModule(t, `
import { openAIBase, problems, draftFrom } from '$JS/profiles/form.js';
import { bootstrap } from '$JS/api.js';
const table = bootstrap({ openai: JSON.stringify(`+string(table)+`) }).openai;
const urls = `+string(in)+`;
const d = { ...draftFrom(null), name: 'x', base_url: 'https://dashscope.aliyuncs.com/compatible-mode/v1', key: 'k' };
const p = problems(d, null, [], table);
console.log(JSON.stringify({ got: urls.map((u) => openAIBase(u, table)), warn: p.warns.base_url, fix: p.fixes.base_url, bad: bootstrap({ openai: '{' }).openai }));
`)
	var res struct {
		Got       []*struct{ Provider, Anthropic string }
		Warn, Fix string
		Bad       []any
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	for i, raw := range urls {
		u, _ := url.Parse(raw)
		b, ok := preflight.CheckBaseURL(u)
		want := ok && b.OpenAI
		got := res.Got[i]
		if (got != nil) != want || (got != nil && (got.Provider != b.Provider || got.Anthropic != b.Fixed)) {
			t.Errorf("%s: page %+v, server %+v %v", raw, got, b, ok)
		}
	}
	if res.Fix != "https://dashscope.aliyuncs.com/apps/anthropic" || !strings.Contains(res.Warn, "Alibaba Cloud Model Studio\u2019s OpenAI-compatible URL") {
		t.Fatalf("warn %q fix %q", res.Warn, res.Fix)
	}
	if res.Bad == nil || len(res.Bad) != 0 {
		t.Fatalf("a bad table must read as none: %v", res.Bad)
	}
}

// The egress test sends nothing for a saved profile whose egress is
// unchanged, so the server tests the saved egress with any proxy
// credentials the page never sees; otherwise it sends the draft. A saved
// socks profile opens with its proxy URL and can be saved again.
func TestProfileFormEgressTest(t *testing.T) {
	out := runModule(t, `
import { draftFrom, problems, egressTestBody } from '$JS/profiles/form.js';
const socks = { name: 'p', provider: '', base_url: 'https://x.example', auth: { mode: 'ccwrap_bearer', has_inline_key: true },
  model_aliases: {}, egress_mode: 'socks5', egress_url: 'socks5://proxy:1080' };
const inherit = { ...socks, egress_mode: 'inherit', egress_url: '' };
const d = draftFrom(socks);
console.log(JSON.stringify({
  unchanged: egressTestBody(socks, d),
  inherit: egressTestBody(inherit, draftFrom(inherit)),
  changed: egressTestBody(socks, { ...d, egress_url: ' socks5://other:1080 ' }),
  mode: egressTestBody(socks, { ...d, egress_mode: 'direct' }),
  fresh: egressTestBody(null, { ...draftFrom(null), egress_mode: 'http', egress_url: 'http://p:8080' }),
  errs: problems(d, socks, []).errs,
}));
`)
	want := `{"unchanged":{},"inherit":{},"changed":{"egress":{"mode":"socks5","url":"socks5://other:1080"}},"mode":{"egress":{"mode":"direct","url":""}},"fresh":{"egress":{"mode":"http","url":"http://p:8080"}},"errs":{}}`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got  %s\nwant %s", strings.TrimSpace(out), want)
	}
}

// Test connectivity tests the saved profile, so it waits until a new
// profile or unsaved changes are saved instead of testing what was there.
func TestProfileFormTestButton(t *testing.T) {
	out := runModule(t, `
import { testButton } from '$JS/profiles/form.js';
console.log(JSON.stringify([testButton(true, false, false), testButton(false, true, false), testButton(false, false, true), testButton(false, false, false)]));
`)
	want := `[{"disabled":true,"title":"Save the profile to test it"},{"disabled":true,"title":"Save your changes to test them"},{"disabled":true,"title":"Send one small model request with this profile"},{"disabled":false,"title":"Send one small model request with this profile"}]`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got  %s\nwant %s", strings.TrimSpace(out), want)
	}
}

// An ended session resumes its own conversation when it reported one;
// otherwise it continues the folder's latest.
func TestContinueLaunch(t *testing.T) {
	out := runModule(t, `
import { continueLaunch } from '$JS/projects.js';
console.log(JSON.stringify([continueLaunch('0b6c2a4e-5f1d-4c3b-9a8e-7d6f5e4c3b2a'), continueLaunch(''), continueLaunch(undefined)]));
`)
	want := `[{"mode":"resume","session":"0b6c2a4e-5f1d-4c3b-9a8e-7d6f5e4c3b2a"},{"mode":"continue"},{"mode":"continue"}]`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got  %s\nwant %s", strings.TrimSpace(out), want)
	}
}

// A folder's conversation is named by its title, else its last prompt; a
// conversation without either is still one to continue.
func TestConversationLabel(t *testing.T) {
	out := runModule(t, `
import { conversationLabel } from '$JS/projects.js';
console.log(JSON.stringify([
  conversationLabel({ last_title: 'Fix login', last_prompt: 'hi', has_conversation: true }),
  conversationLabel({ last_prompt: 'hello there', has_conversation: true }),
  conversationLabel({ has_conversation: true }),
  conversationLabel({}),
]));
`)
	want := `[{"text":"Fix login","none":false},{"text":"hello there","none":false},{"text":"Untitled conversation","none":true},{"text":"No conversations yet","none":true}]`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got  %s\nwant %s", strings.TrimSpace(out), want)
	}
}

// A Resume list entry names the conversation like a project row does, and
// says where it stands: the running session it is open in (which opens
// instead of a second Claude Code), or its branch and age.
func TestConversationItem(t *testing.T) {
	out := runModule(t, `
import { conversationItem } from '$JS/projects.js';
const now = Date.parse('2026-10-30T10:00:00Z');
const sessions = [{ id: 'sess1', profile: 'deepseek', started_at: '2026-10-30T09:48:00Z', state: 'active' }];
console.log(JSON.stringify([
  conversationItem({ id: 'a', title: 'Fix login', last_prompt: 'hi', git_branch: 'main', last_active_at: '2026-10-30T09:57:00Z' }, sessions, now),
  conversationItem({ id: 'b', last_prompt: 'why does it fail', last_active_at: '2026-10-28T10:00:00Z' }, sessions, now),
  conversationItem({ id: 'c', title: 'Open work', open_in: ['sess1'], last_active_at: '2026-10-30T09:59:00Z' }, sessions, now),
  conversationItem({ id: 'd', open_in: ['gone'], last_active_at: '2026-10-30T09:59:00Z' }, sessions, now),
]));
`)
	want := `[{"text":"Fix login","none":false,"meta":"main · 3m ago","open":""},` +
		`{"text":"why does it fail","none":false,"meta":"2d ago","open":""},` +
		`{"text":"Open work","none":false,"meta":"open · deepseek · 12m","open":"sess1"},` +
		`{"text":"Untitled conversation","none":true,"meta":"open in another session","open":"gone"}]`
	if strings.TrimSpace(out) != want {
		t.Fatalf("got  %s\nwant %s", strings.TrimSpace(out), want)
	}
}
