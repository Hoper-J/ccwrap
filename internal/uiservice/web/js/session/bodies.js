// Captured bodies in the inspector: request bodies laid out by part
// (system, tools, messages), response streams reassembled, anything else
// pretty-printed. Ported from the session page's body viewer.

import { h, show } from '../dom.js';

// isSSEBody reports a captured response that is a text/event-stream: it
// does not parse as JSON and has event: and data: lines.
export function isSSEBody(txt) {
  try { JSON.parse(txt); return false; } catch (_) { /* not JSON */ }
  const s = String(txt == null ? '' : txt);
  return s.includes('event:') && s.includes('data:');
}

// parseSSE splits a stream into {event, data} records (an event's data
// lines joined). Tolerates CRLF.
export function parseSSE(txt) {
  const events = [];
  for (const block of String(txt == null ? '' : txt).split(/\r?\n\r?\n/)) {
    const ev = { event: '', data: '' };
    for (const line of block.split(/\r?\n/)) {
      if (line.startsWith('event:')) ev.event = line.slice(6).trim();
      else if (line.startsWith('data:')) ev.data += line.slice(5).trim();
    }
    if (ev.event || ev.data) events.push(ev);
  }
  return events;
}

// reassembleSSE folds an Anthropic message stream into its final shape: the
// assistant text (and tool input), thinking, model, stop reason and output
// tokens.
export function reassembleSSE(txt) {
  const events = parseSSE(txt);
  const out = { text: '', thinking: '', model: '', stop: '', outTok: 0, events: events.length };
  for (const ev of events) {
    let d;
    try { d = JSON.parse(ev.data); } catch (_) { continue; }
    if (!d || typeof d !== 'object') continue;
    if (d.type === 'message_start' && d.message) {
      out.model = d.message.model || out.model;
      if (d.message.usage && d.message.usage.output_tokens != null) out.outTok = d.message.usage.output_tokens;
    } else if (d.type === 'content_block_delta' && d.delta) {
      if (typeof d.delta.text === 'string') out.text += d.delta.text;
      else if (typeof d.delta.thinking === 'string') out.thinking += d.delta.thinking;
      else if (typeof d.delta.partial_json === 'string') out.text += d.delta.partial_json;
    } else if (d.type === 'message_delta') {
      if (d.delta && d.delta.stop_reason) out.stop = d.delta.stop_reason;
      if (d.usage && d.usage.output_tokens != null) out.outTok = d.usage.output_tokens;
    }
  }
  return out;
}

// anatomy is a request body's top-level keys by share of its size, largest
// first: [{key, pct}] ("<1%" for a tiny non-empty part).
export function anatomy(doc, total) {
  return Object.keys(doc).map((k) => {
    const seg = JSON.stringify(doc[k]).length;
    return { key: k, seg, pct: total ? Math.round((seg * 100) / total) : 0 };
  }).sort((a, b) => b.seg - a.seg).map((e) => ({ key: e.key, pct: e.pct === 0 && e.seg > 0 ? '<1%' : e.pct + '%' }));
}

function pre(text) {
  return h('pre', null, String(text == null ? '' : text));
}

function accordion(title, children, open) {
  return h('details', { open }, h('summary', null, title), h('div', { class: 'bv-acc' }, children));
}

// raw is one block's text: collapsed when long.
function raw(text) {
  const s = String(text == null ? '' : text);
  if (s.length > 500) return h('details', null, h('summary', null, 'raw (' + s.length + ' chars)'), pre(s));
  return pre(s);
}

function cacheControl(b) {
  return b && b.cache_control ? b.cache_control.type + (b.cache_control.scope ? '/' + b.cache_control.scope : '') : '';
}

function block(i, type, text, cc) {
  const global = cc.includes('/');
  return h('div', { class: 'bv-block' + (cc ? (global ? ' ccg' : ' cc') : '') },
    h('div', { class: 'bh' }, '[' + i + '] ' + (type || '') + ' · ' + String(text == null ? '' : text).length + ' B',
      cc ? h('span', { class: 'bv-chip' + (global ? ' g' : '') }, cc) : null),
    raw(text));
}

function turn(role, blocks) {
  return h('div', { class: 'bv-turn' },
    h('div', { class: 'bv-role' }, '◆ ' + (role || '')),
    blocks.map((b, i) => block(i, (b && b.type) || '', b && typeof b.text === 'string' ? b.text : JSON.stringify(b, null, 2), cacheControl(b))));
}

function tool(t) {
  const props = t.input_schema && t.input_schema.properties ? Object.keys(t.input_schema.properties) : [];
  const desc = String(t.description == null ? '' : t.description);
  return h('details', { class: 'bv-tool' },
    h('summary', null, (t.name || '?') + ' ', h('span', { class: 'tm' }, JSON.stringify(t == null ? {} : t).length + ' B · ' + props.length + ' props')),
    h('div', { class: 'tb' },
      h('details', null, h('summary', null, 'description (' + desc.length + ' chars)'), pre(desc)),
      h('div', { class: 'bh' }, 'input_schema'),
      pre(JSON.stringify(t.input_schema || {}, null, 2))));
}

function anatomyStrip(items) {
  return h('div', { class: 'body-anatomy' }, items.map(([k, v]) => h('span', null, k + ' ', h('b', null, String(v)))));
}

function secLabel(t) { return h('div', { class: 'bv-seclabel' }, t); }

// download is a link that saves text as a file; the blob exists only for
// the click.
function download(label, text, name, type) {
  return h('button', {
    type: 'button',
    class: 'bv-dl',
    onclick: () => {
      const url = URL.createObjectURL(new Blob([text], { type }));
      const a = h('a', { href: url, download: name });
      document.body.appendChild(a);
      a.click();
      a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 0);
    },
  }, label);
}

// requestView lays a request body out in reading order: anatomy, scalar
// config, system, tools, messages, other parts, raw.
export function requestView(txt) {
  let doc;
  try { doc = JSON.parse(txt); } catch (_) { return [pre(txt)]; }
  if (!doc || typeof doc !== 'object' || Array.isArray(doc)) return [pre(JSON.stringify(doc, null, 2))];
  const keys = Object.keys(doc);
  const scalars = keys.filter((k) => doc[k] === null || typeof doc[k] !== 'object');
  const parts = [anatomyStrip(anatomy(doc, txt.length).map((a) => [a.key, a.pct]))];
  if (scalars.length) {
    parts.push(secLabel('config'), h('div', { class: 'bv-config' }, scalars.map((k) => [h('div', { class: 'ck' }, k), h('div', { class: 'cv' }, String(doc[k]))])));
  }
  if (Array.isArray(doc.system)) {
    parts.push(accordion('system [' + doc.system.length + ']', doc.system.map((b, i) => block(i, (b && b.type) || '', b && typeof b.text === 'string' ? b.text : JSON.stringify(b), cacheControl(b))), true));
  }
  if (Array.isArray(doc.tools)) parts.push(accordion('tools [' + doc.tools.length + ']', doc.tools.map(tool)));
  if (Array.isArray(doc.messages)) {
    parts.push(accordion('messages [' + doc.messages.length + ']', doc.messages.map((m) => {
      const c = m.content;
      return turn(m.role || '', Array.isArray(c) ? c : [{ type: 'text', text: String(c) }]);
    })));
  }
  for (const k of keys) {
    if (k === 'system' || k === 'tools' || k === 'messages') continue;
    const v = doc[k];
    if (v === null || typeof v !== 'object') continue;
    parts.push(accordion(k, [pre(JSON.stringify(v, null, 2))]));
  }
  parts.push(h('details', null, h('summary', null, 'Raw JSON'),
    h('div', { class: 'bv-acc' }, download('download request-body.json', txt, 'request-body.json', 'application/json'), pre(txt))));
  return parts;
}

// jsonView pretty-prints a body when it is JSON, else shows it as is.
export function jsonView(txt) {
  try { return [pre(JSON.stringify(JSON.parse(txt), null, 2))]; } catch (_) { return [pre(txt)]; }
}

// responseView reassembles a stream, else pretty-prints.
export function responseView(txt) {
  if (!isSSEBody(txt)) return jsonView(txt);
  const r = reassembleSSE(txt);
  const chips = [['events', r.events]];
  if (r.model) chips.push(['model', r.model]);
  if (r.stop) chips.push(['stop', r.stop]);
  if (r.outTok) chips.push(['out tok', r.outTok]);
  return [
    anatomyStrip(chips),
    r.thinking ? [secLabel('thinking'), pre(r.thinking)] : null,
    secLabel('assistant text'),
    pre(r.text || '(no text content)'),
    h('details', null, h('summary', null, 'Raw SSE (' + String(txt).length + ' chars)'),
      download('download response.sse', String(txt), 'response.sse', 'text/plain'), pre(txt)),
  ];
}

const GONE = 'body not retained (evicted, capture off, or write failed)';

// bodyDrawer is a lazily loaded body: fetched and laid out by view on first
// open. load(id) resolves to the body text.
export function bodyDrawer(label, id, view, load) {
  const panel = h('div', { class: 'sub-body body-panel' }, label + ' — loading…');
  const d = h('details', { class: 'req-sub body-drawer' }, h('summary', null, label), panel);
  d.addEventListener('toggle', () => {
    if (!d.open || d.dataset.loaded) return;
    d.dataset.loaded = '1';
    load(id).then((txt) => show(panel, view(txt))).catch(() => show(panel, GONE));
  });
  return d;
}

// requestBodyDrawer shows the request as received and, when ccwrap changed
// it (alias rewrite, stripped system blocks), as forwarded.
export function requestBodyDrawer(id, upstreamId, load) {
  if (!upstreamId) return bodyDrawer('request body', id, requestView, load);
  const panel = h('div', { class: 'sub-body body-panel' });
  const d = h('details', { class: 'req-sub body-drawer' }, h('summary', null, 'request body'), panel);
  d.addEventListener('toggle', () => {
    if (!d.open || d.dataset.loaded) return;
    d.dataset.loaded = '1';
    const paneC = h('div', { class: 'body-view-pane on' }, 'loading…');
    const paneU = h('div', { class: 'body-view-pane' }, 'loading…');
    const btnC = h('button', { type: 'button', class: 'body-view-btn on', 'aria-pressed': 'true' }, 'received');
    const btnU = h('button', { type: 'button', class: 'body-view-btn', 'aria-pressed': 'false' }, 'forwarded');
    const pick = (client) => {
      btnC.classList.toggle('on', client);
      btnU.classList.toggle('on', !client);
      btnC.setAttribute('aria-pressed', String(client));
      btnU.setAttribute('aria-pressed', String(!client));
      paneC.classList.toggle('on', client);
      paneU.classList.toggle('on', !client);
    };
    btnC.addEventListener('click', () => pick(true));
    btnU.addEventListener('click', () => pick(false));
    show(panel, h('div', { class: 'body-view-toggle' }, btnC, btnU, h('span', { class: 'body-view-hint' }, 'after alias + strip')), paneC, paneU);
    load(id).then((txt) => show(paneC, requestView(txt))).catch(() => show(paneC, GONE));
    load(upstreamId).then((txt) => show(paneU, requestView(txt))).catch(() => show(paneU, GONE));
  });
  return d;
}
