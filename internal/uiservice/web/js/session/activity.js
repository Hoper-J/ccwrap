// Activity: the session's requests, errors and trace, newest first, behind
// filter tabs. A forwarded request opens into its inspector: the failure,
// the request headers by role, the captured bodies.

import { h, show } from '../dom.js';
import { clock } from '../fmt.js';
import { FILTERS, activityRow, counts, visibleEntries, defaultFilter, addRecord, headerSummary } from './model.js';
import { bodyDrawer, requestBodyDrawer, responseView, jsonView } from './bodies.js';

const PAGE = 50;
const EMPTY = 'No recent traffic for this session yet — requests appear here live as Claude Code sends them.';

// createActivity draws Activity into container. deps: {deny: Set,
// unmask(): bool, loadBody(id): Promise<string>, ended(): bool}.
export function createActivity(container, deps) {
  let act = { requests: [], errors: [], trace: [] };
  let filter = '';
  let limit = PAGE;
  const open = new Set();      // keys of rows the user opened
  const rowCache = new Map();  // key → row element, so open inspectors survive redraws

  const tabs = h('div', { class: 'filter-bar', role: 'tablist', 'aria-label': 'Activity filter' });
  const list = h('div', { class: 'act-list', role: 'tabpanel', id: 'act-panel' });
  const more = h('button', { type: 'button', class: 'show-more-btn', onclick: () => { limit += PAGE; draw(); } }, 'Show more');
  const moreWrap = h('div', { class: 'show-more', hidden: true }, more);
  show(container, h('section', { class: 'activity', 'aria-labelledby': 'act-title' },
    h('div', { class: 'section-head' }, h('h2', { class: 'section-title', id: 'act-title' }, 'Activity'), h('span', { class: 'section-aside' }, 'newest first')),
    tabs, list, moreWrap));

  tabs.addEventListener('keydown', (e) => {
    if (!['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) return;
    const btns = Array.from(tabs.querySelectorAll('[role="tab"]'));
    const i = btns.indexOf(document.activeElement);
    if (i < 0) return;
    e.preventDefault();
    let j = e.key === 'ArrowLeft' ? i - 1 : e.key === 'ArrowRight' ? i + 1 : e.key === 'Home' ? 0 : btns.length - 1;
    j = (j + btns.length) % btns.length;
    select(btns[j].dataset.filter, true);
  });

  function select(f, focus) {
    filter = f;
    limit = PAGE;
    draw();
    if (focus) {
      const t = tabs.querySelector('[data-filter="' + f + '"]');
      if (t) t.focus();
    }
  }

  function key(rec, cls) {
    return cls + '|' + rec.timestamp + '|' + (rec.count || '');
  }

  function drawTabs() {
    const c = counts(act);
    show(tabs, FILTERS.map((f) => h('button', {
      type: 'button', role: 'tab', class: 'filter-btn' + (f.key === filter ? ' on' : ''), id: 'act-tab-' + f.key,
      'data-filter': f.key, 'aria-selected': String(f.key === filter), 'aria-controls': 'act-panel',
      tabindex: f.key === filter ? '0' : '-1', onclick: () => select(f.key, false),
    }, f.label + ' ', h('span', { class: 'filter-n' }, String(c[f.key] || 0)))));
    list.setAttribute('aria-labelledby', 'act-tab-' + filter);
  }

  function draw() {
    if (!filter) filter = defaultFilter(act);
    drawTabs();
    const entries = visibleEntries(act, filter, limit);
    const total = counts(act)[filter] || 0;
    moreWrap.hidden = total <= entries.length;
    if (!entries.length) {
      rowCache.clear();
      show(list, h('p', { class: 'act-empty' }, deps.ended() ? 'No traffic was recorded for this session.' : EMPTY));
      return;
    }
    const keep = new Set();
    const els = entries.map(([rec, cls]) => {
      const k = key(rec, cls);
      keep.add(k);
      let el = rowCache.get(k);
      if (!el) {
        el = rowEl(activityRow(rec, cls, { deny: deps.deny, unmask: deps.unmask() }), k);
        rowCache.set(k, el);
      }
      return el;
    });
    for (const k of rowCache.keys()) if (!keep.has(k)) rowCache.delete(k);
    show(list, h('div', { class: 'act-head', 'aria-hidden': 'true' }, h('span'), h('span', null, 'Time'), h('span', { class: 'act-kind-col' }, 'Kind'), h('span', null, 'Summary'), h('span', null, 'Details')), els);
  }

  function rowEl(row, k) {
    const expandable = !!(row.forwarded || row.telemetry);
    const summary = h('div', {
      class: 'act-sum', role: expandable ? 'button' : null, tabindex: expandable ? '0' : null,
      'aria-expanded': expandable ? String(open.has(k)) : null,
    },
    h('span', { class: 'act-chev', 'aria-hidden': 'true' }),
    h('span', { class: 'act-time' }, clock(row.ts) || '—'),
    h('span', { class: 'act-kind', title: row.label }, row.label),
    h('span', { class: 'act-main' + (row.mono ? ' mono' : '') }, row.switch ? switchText(row) : row.main, h('span', { class: 'act-kind-inline' }, row.label)),
    h('span', { class: 'act-right' }, row.right));
    const el = h('div', {
      class: 'act-row', role: 'listitem', 'data-kind': row.kind, 'data-class': row.cls, 'data-st': row.st || null,
      'data-severity': row.severity || null, 'data-forwarded': row.forwarded ? 'true' : null, 'data-ts': row.ts,
    }, summary);
    if (row.note) el.appendChild(h('div', { class: 'act-note' }, row.note));
    if (!expandable) return el;
    let inspector = null;
    const toggle = () => {
      const isOpen = summary.getAttribute('aria-expanded') !== 'true';
      summary.setAttribute('aria-expanded', String(isOpen));
      el.toggleAttribute('data-open', isOpen);
      if (isOpen) {
        open.add(k);
        if (!inspector) {
          inspector = inspectorEl(row);
          el.appendChild(inspector);
        }
        inspector.hidden = false;
      } else {
        open.delete(k);
        if (inspector) inspector.hidden = true;
      }
    };
    summary.addEventListener('click', toggle);
    summary.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); }
    });
    if (open.has(k)) { summary.setAttribute('aria-expanded', 'false'); toggle(); }
    return el;
  }

  function inspectorEl(row) {
    const parts = [];
    if (row.failure) parts.push(failureEl(row.failure));
    if (row.headers) parts.push(headersEl(row.headers, row.headersCompared));
    if (row.telemetry) {
      if (row.bodyId) parts.push(bodyDrawer('request body', row.bodyId, jsonView, deps.loadBody));
      if (row.responseBodyId) parts.push(bodyDrawer('response body', row.responseBodyId, jsonView, deps.loadBody));
    } else {
      if (row.bodyId) parts.push(requestBodyDrawer(row.bodyId, row.upstreamBodyId, deps.loadBody));
      if (row.responseBodyId) parts.push(bodyDrawer('response body', row.responseBodyId, responseView, deps.loadBody));
    }
    if (!parts.length) parts.push(h('p', { class: 'act-note' }, 'Nothing captured for this request. Turn on Bodies to record request and response bodies.'));
    return h('div', { class: 'inspector' }, parts);
  }

  return {
    // set replaces the activity (the first load).
    set(next) { act = next; draw(); },
    // add folds in a live record: type request | error | trace.
    add(type, rec) { act = addRecord(act, type, rec); draw(); },
    // reveal shows a filter and opens the row recorded at ts.
    reveal(f, ts) {
      filter = f;
      limit = PAGE;
      const entry = visibleEntries(act, f, Infinity).findIndex(([rec]) => rec.timestamp === ts);
      if (entry >= limit) limit = entry + 1;
      draw();
      const el = entry >= 0 ? list.querySelector('[data-ts="' + CSS.escape(ts) + '"]') : null;
      if (el) {
        const s = el.querySelector('.act-sum');
        if (s && s.getAttribute('aria-expanded') === 'false') s.click();
        el.scrollIntoView({ block: 'center' });
        if (s) s.focus();
        el.classList.add('act-flash');
        setTimeout(() => el.classList.remove('act-flash'), 1600);
      } else {
        const t = tabs.querySelector('[data-filter="' + f + '"]');
        if (t) t.scrollIntoView({ block: 'center' });
      }
    },
    redraw() { draw(); },
  };
}

function switchText(row) {
  const s = row.switch;
  const to = s.to + (s.toProvider ? ' (' + s.toProvider + ')' : '');
  return (row.main === 'refused' ? 'switch refused: ' : 'profile ') + s.from + ' → ' + to;
}

function failureEl(f) {
  return h('div', { class: 'req-fail', 'data-source': f.source },
    h('div', { class: 'rf-head' }, f.head),
    f.message ? h('div', { class: 'rf-msg' }, f.message) : null,
    f.hint ? h('div', { class: 'rf-hint' }, f.hint) : null,
    f.body ? h('details', { class: 'req-sub rf-raw' }, h('summary', null, 'error body' + (f.truncated ? ' (truncated)' : '')),
      h('div', { class: 'sub-body' }, h('pre', { class: 'rf-pre' }, f.body))) : null);
}

function headersEl(groups, compared) {
  return h('details', { class: 'req-sub' }, h('summary', null, 'request headers'),
    h('div', { class: 'sub-body' },
      h('div', { class: 'hdr-sum' }, headerSummary(groups, compared).map(([k, n]) => h('span', { 'data-k': k }, h('b', null, String(n)), ' ' + k))),
      groups.map((g) => h('div', { class: 'hdr-group' },
        h('div', { class: 'hdr-group-name' }, g.name),
        g.rows.map((r) => h('div', { class: 'hdr-row' },
          h('span', { class: 'hdr-k mono' }, r.name, r.tag ? h('span', { class: 'hdr-tag', 'data-tag': r.tag }, r.tag) : null),
          h('span', { class: 'hdr-v mono' + (r.redacted ? ' redv' : '') + (r.struck ? ' struck' : '') },
            r.was ? [h('span', { class: 'visually-hidden' }, 'was '), h('span', { class: 'hdr-was' }, r.was), h('span', { class: 'hdr-arrow', 'aria-hidden': 'true' }, '→'), h('span', { class: 'visually-hidden' }, ', now ')] : null,
            r.set ? h('span', { class: 'hdr-set' }, r.value) : r.value,
            r.redacted ? [' ', h('span', { class: 'hdr-redpill' }, 'redacted')] : null)))))));
}
