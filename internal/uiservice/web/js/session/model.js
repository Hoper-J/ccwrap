// What the session view shows, as data: Activity rows, header groups, the
// health word. Ported from the session page's script (web.js), which
// mirrors the Go first paint; no DOM here.

// Activity filters, in display order.
export const FILTERS = [
  { key: 'all', label: 'All' },
  { key: 'forwarded-api', label: 'Forwarded API' },
  { key: 'synthetic', label: 'Synthetic' },
  { key: 'tunnel', label: 'Tunnel' },
  { key: 'telemetry', label: 'Telemetry' },
  { key: 'error', label: 'Errors' },
  { key: 'trace', label: 'Trace' },
];

// RETAIN mirrors the session's retention rings (server.go
// maxSession{Requests,Errors,Trace}).
export const RETAIN = { requests: 250, errors: 250, trace: 500 };

// healthWord is the session's state as its pill says it.
export function healthWord(sess) {
  if (!sess || sess.state === 'ended') return 'ended';
  if (sess.session_health === 'error') return 'error';
  if (sess.session_health === 'warn') return 'degraded';
  return 'active';
}

export function joinParts(parts) {
  return parts.filter((p) => p != null && String(p).trim() !== '').join(' · ');
}

export function methodLabel(rec) {
  if (rec.synthetic) return 'SYNTHETIC';
  return rec.method || 'REQUEST';
}

// requestModelLabel, failureHead and failureBrief mirror ui.RequestModelLabel
// / RequestFailureHead / RequestFailureBrief.
export function requestModelLabel(rec) {
  if (!rec.model) return '';
  if (rec.upstream_model && rec.upstream_model !== rec.model) return rec.model + ' → ' + rec.upstream_model;
  if (rec.model_unmapped) return rec.model + ' (no alias)';
  return rec.model;
}

export function failureHead(rec) {
  const f = rec.failure;
  if (!f) return '';
  if (f.source === 'ccwrap') return 'no upstream response';
  return 'upstream returned ' + (rec.status_code || 0) + (f.type ? ' · ' + f.type : '');
}

export function failureBrief(rec, n) {
  const m = rec.failure && rec.failure.message;
  if (!m) return '';
  const r = Array.from(m);
  return r.length <= n ? m : r.slice(0, n).join('') + '…';
}

// displayPath mirrors ui.DisplayPath: drop the ?beta=true every API call
// carries, keep any other query.
export function displayPath(path) {
  const p = String(path || '');
  const i = p.indexOf('?');
  if (i < 0) return p;
  const kept = p.slice(i + 1).split('&').filter((q) => q && q !== 'beta=true');
  return kept.length ? p.slice(0, i) + '?' + kept.join('&') : p.slice(0, i);
}

// statusTier mirrors the Go statusTier: 5xx err, 4xx warn.
export function statusTier(code) {
  const c = Number(code) || 0;
  if (c >= 500 && c < 600) return 'err';
  if (c >= 400 && c < 500) return 'warn';
  return '';
}

const HDR_GROUPS = [
  ['Protocol & versioning', (l) => l.startsWith('anthropic-')],
  ['Client identity', (l) => l === 'user-agent' || l.startsWith('x-stainless-') || l === 'x-app' || l === 'x-client-request-id' || l === 'x-claude-code-session-id'],
  ['Content negotiation', (l) => l === 'content-type' || l === 'accept' || l === 'accept-encoding'],
  ['Reliability', (l) => l === 'idempotency-key'],
  ['Credentials', null],
  ['Other', () => true],
];

export const REDACTED = '‹redacted by ccwrap›';

// SET_BY_CCWRAP stands in for a value ccwrap set on the way upstream: keys
// and configured upstream headers never leave the session.
export const SET_BY_CCWRAP = 'set by ccwrap';

function headerGroup(lower, cred) {
  if (cred) return 'Credentials';
  for (const [g, test] of HDR_GROUPS) {
    if (test && g !== 'Other' && test(lower)) return g;
  }
  return 'Other';
}

// headerGroups sorts a request's headers into role groups. Credential
// headers (deny) show the redaction sentinel unless the session runs with
// CCWRAP_UNMASK_CREDENTIALS (unmask); the session already masked them when
// it recorded the request, so this is a second layer. route ({host, was})
// leads as a Routing group: the host the request went to, and the one
// Claude Code asked for when ccwrap rewrote it. diff (the record's
// upstream_headers) tags what ccwrap did to each header on the way
// upstream — rewritten, stripped, or injected (one Claude Code did not
// send); the values ccwrap set are never shown.
export function headerGroups(headers, deny, unmask, route, diff) {
  if (!headers) return null;
  const actions = new Map(((diff && diff.changes) || []).map((c) => [c.name.toLowerCase(), c]));
  const buckets = {};
  const add = (group, row) => (buckets[group] = buckets[group] || []).push(row);
  for (const name of Object.keys(headers)) {
    const lower = name.toLowerCase();
    if (route && lower === 'host') continue;
    const vals = headers[name];
    let value = Array.isArray(vals) ? vals.join(', ') : String(vals);
    const cred = deny.has(lower);
    if (cred && !unmask) value = REDACTED;
    const row = { name, value, redacted: cred && !unmask, tag: '' };
    const change = actions.get(lower);
    actions.delete(lower);
    if (change && change.action === 'stripped') {
      row.tag = 'stripped';
      row.struck = true;
    } else if (change) {
      row.tag = 'rewritten';
      if (!row.redacted) { row.was = value; row.value = SET_BY_CCWRAP; row.set = true; }
    }
    add(headerGroup(lower, cred), row);
  }
  for (const [lower, change] of actions) {
    if (change.action !== 'injected') continue;
    add(headerGroup(lower, deny.has(lower)), { name: change.name, value: SET_BY_CCWRAP, set: true, tag: 'injected', redacted: false });
  }
  const out = [];
  if (route) out.push({ name: 'Routing', rows: [{ name: 'host', value: route.host, was: route.was, tag: route.was ? 'rewritten' : '', redacted: false }] });
  for (const [g] of HDR_GROUPS) {
    const rows = buckets[g];
    if (!rows || !rows.length) continue;
    rows.sort((a, b) => (a.name < b.name ? -1 : 1));
    out.push({ name: g, rows });
  }
  return out.length ? out : null;
}

// headerSummary counts the header rows for the inspector's summary chips:
// what ccwrap did to them when it compared them with what went upstream
// (compared), else how many headers and groups there are.
export function headerSummary(groups, compared) {
  const n = { rewritten: 0, injected: 0, stripped: 0, redacted: 0, passthrough: 0, headers: 0 };
  for (const g of groups) {
    for (const r of g.rows) {
      n.headers++;
      if (r.redacted) n.redacted++;
      if (r.tag) n[r.tag]++;
      else n.passthrough++;
    }
  }
  if (!compared) {
    return [['headers', n.headers], ['groups', groups.length], ['rewritten', n.rewritten], ['redacted', n.redacted]].filter(([k, v]) => v > 0 || k === 'headers');
  }
  return ['rewritten', 'injected', 'stripped', 'redacted', 'passthrough'].map((k) => [k, n[k]]).filter(([k, v]) => v > 0 || k === 'passthrough');
}

// activityRow is one Activity row: what its cells say, and for forwarded
// requests what its inspector holds. kind is request | error | trace.
export function activityRow(rec, cls, opts) {
  if (cls === 'error') {
    return {
      ts: rec.timestamp,
      kind: 'error',
      cls: 'error',
      label: rec.severity === 'warn' ? 'warning' : 'error',
      severity: rec.severity === 'warn' ? 'warn' : '',
      main: (rec.summary || 'proxy error') + (rec.count > 1 ? ' ×' + rec.count : ''),
      right: joinParts([rec.error_class || rec.severity, rec.upstream_host, rec.suggested_action]) || 'check ccwrap doctor',
    };
  }
  if (cls === 'trace') {
    const row = {
      ts: rec.timestamp,
      kind: 'trace',
      cls: 'trace',
      label: 'trace',
      main: rec.summary || 'activity',
      right: joinParts([rec.category || 'trace', rec.detail || '—']),
      category: rec.category || '',
    };
    if (rec.category === 'profile_switch') {
      try {
        const p = JSON.parse(rec.detail || '{}');
        row.switch = { from: String(p.from || ''), to: String(p.to || ''), toProvider: String(p.to_provider || ''), class: String(p.class || ''), reason: String(p.reason || '') };
      } catch (_) { /* keep the raw detail */ }
    }
    return row;
  }
  const modelLabel = requestModelLabel(rec);
  const brief = failureBrief(rec, 60);
  const row = {
    ts: rec.timestamp,
    kind: 'request',
    cls,
    label: cls,
    main: methodLabel(rec) + ' ' + (displayPath(rec.path) || '/') + (modelLabel ? ' · ' + modelLabel : ''),
    right: joinParts([String(rec.status_code ?? '') + (brief ? ' ' + brief : ''), (rec.latency_ms || 0) + ' ms', rec.stream_state, rec.actual_upstream_host || rec.logical_target_host]),
    st: statusTier(rec.status_code),
    mono: true,
    forwarded: cls === 'forwarded-api',
    profile: rec.active_profile_name || '',
  };
  if (cls === 'forwarded-api') {
    const rh = rec.request_headers;
    const asked = rec.logical_target_host || '';
    const went = rec.actual_upstream_host || asked;
    const route = went ? { host: went, was: asked && asked !== went ? asked : '' } : null;
    if (rh && Object.keys(rh).length) {
      row.headers = headerGroups(rh, opts.deny, opts.unmask, route, rec.upstream_headers);
      row.headersCompared = !!rec.upstream_headers;
    }
    else if (rec.synthetic) row.note = 'CCWRAP-generated, not Claude Code traffic';
    else if ((rec.method || '') === 'CONNECT') row.note = 'encrypted tunnel — not intercepted; no headers visible';
    else row.note = 'no headers recorded';
    row.bodyId = (rec.body_ref && rec.body_ref.id) || '';
    row.upstreamBodyId = (rec.upstream_body_ref && rec.upstream_body_ref.id) || '';
    row.responseBodyId = (rec.response_body_ref && rec.response_body_ref.id) || '';
    if (rec.failure) {
      row.failure = { source: rec.failure.source || 'upstream', head: failureHead(rec), message: rec.failure.message || '', hint: rec.failure.hint || '', body: rec.failure.body || '', truncated: !!rec.failure.truncated };
    }
  }
  if (cls === 'telemetry') {
    row.bodyId = (rec.body_ref && rec.body_ref.id) || '';
    row.responseBodyId = (rec.response_body_ref && rec.response_body_ref.id) || '';
    row.telemetry = !!(row.bodyId || row.responseBodyId);
  }
  return row;
}

// counts is how many entries each filter shows.
export function counts(act) {
  const c = { all: act.requests.length + act.errors.length + act.trace.length, error: act.errors.length, trace: act.trace.length };
  for (const r of act.requests) c[r.class || 'forwarded-api'] = (c[r.class || 'forwarded-api'] || 0) + 1;
  return c;
}

// visibleEntries is the newest limit entries a filter shows, newest first,
// as [record, class] pairs.
export function visibleEntries(act, filter, limit) {
  const all = [];
  if (filter === 'all' || (filter !== 'error' && filter !== 'trace')) {
    for (const r of act.requests) {
      const cls = r.class || 'forwarded-api';
      if (filter === 'all' || filter === cls) all.push([r, cls]);
    }
  }
  if (filter === 'all' || filter === 'error') for (const e of act.errors) all.push([e, 'error']);
  if (filter === 'all' || filter === 'trace') for (const t of act.trace) all.push([t, 'trace']);
  all.sort((a, b) => Date.parse(b[0].timestamp) - Date.parse(a[0].timestamp));
  return all.slice(0, limit);
}

// defaultFilter is Forwarded API when there is any, else All — so a session
// whose only activity is trace never opens on an empty tab.
export function defaultFilter(act) {
  return act.requests.some((r) => (r.class || 'forwarded-api') === 'forwarded-api') ? 'forwarded-api' : 'all';
}

// addRecord folds a live record into the activity, as the session's rings
// do: a repeated error replaces its predecessor (the session folded it), and
// each list keeps its retention cap.
export function addRecord(act, type, rec) {
  if (type === 'error') {
    const errors = act.errors.slice();
    const last = errors[errors.length - 1];
    if (rec.count > 1 && last && last.error_class === rec.error_class && last.summary === rec.summary && last.upstream_host === rec.upstream_host) errors[errors.length - 1] = rec;
    else errors.push(rec);
    return { ...act, errors: errors.slice(-RETAIN.errors) };
  }
  if (type === 'trace') return { ...act, trace: act.trace.concat([rec]).slice(-RETAIN.trace) };
  return { ...act, requests: act.requests.concat([rec]).slice(-RETAIN.requests) };
}

// issueRevealTarget is where a show_activity fix points: the filter to show
// and the row timestamp to open.
export function issueRevealTarget(fix) {
  return { filter: fix.value || 'all', at: fix.at || '' };
}
