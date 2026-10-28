// The session header: which session (project, short id, path), how it is
// (health pill, posture sentence) and the meta line. When the session ended
// while viewed, a banner above it keeps the final state and offers Continue.

import { h, icon, show, copyText } from '../dom.js';
import { since, ago, basename } from '../fmt.js';
import { healthWord } from './model.js';

// metaLine is the header's meta: profile · state · claude pid N · up 12m ·
// last activity 8s ago (or ended 40s ago).
export function metaLine(sess, now) {
  const parts = [sess.active_profile_name || 'no profile'];
  if (sess.state === 'ended') {
    parts.push('ended' + (sess.ended_at ? ' ' + ago(sess.ended_at, now) : ''));
  } else {
    parts.push(sess.state);
    if (sess.claude_pid) parts.push('claude pid ' + sess.claude_pid);
    const up = since(sess.created_at, now);
    if (up) parts.push('up ' + up);
  }
  if (sess.last_activity_at) parts.push('last activity ' + ago(sess.last_activity_at, now));
  return parts.join(' · ');
}

// endedText is the ended banner's head: "Session ended 40s ago."
export function endedText(sess, now) {
  return 'Session ended' + (sess.ended_at ? ' ' + ago(sess.ended_at, now) : '') + '.';
}

// renderHeader draws the header. m: {sess, path, toast, onContinue}.
export function renderHeader(el, m) {
  const s = m.sess;
  const word = healthWord(s);
  const title = s.project_dir ? basename(s.project_dir) : (s.name || s.id.slice(0, 8));
  const ended = s.state === 'ended';
  show(el,
    ended ? h('div', { class: 'ended-banner', role: 'status' },
      icon('clock'),
      h('span', null, h('b', { id: 'sess-ended' }, endedText(s, Date.now())), ' Final state preserved below.'),
      h('span', { class: 'spacer' }),
      s.project_dir ? h('button', { type: 'button', class: 'btn btn-sm', onclick: m.onContinue }, 'Continue in ' + title) : null) : null,
    h('section', { class: 'sess-head', 'data-health': word, 'aria-labelledby': 'sess-title' },
      h('div', { class: 'kicker sess-kicker' }, 'Session',
        h('button', { type: 'button', class: 'sess-id mono', title: 'Copy session id ' + s.id, onclick: () => m.toast(copyText(s.id) ? 'session id copied' : "Couldn't copy the session id.") },
          s.id.slice(0, 8), icon('copy'))),
      h('div', { class: 'sess-title-row' },
        h('h1', { class: 'page-title', id: 'sess-title' }, title),
        h('span', { class: 'health-pill', 'data-health': word }, word),
        m.path ? h('span', { class: 'sess-path mono' }, m.path) : null),
      // Once ended, the banner says what the posture sentence would.
      ended ? null : h('p', { class: 'posture' }, emphasize(s.display.posture, [s.exact_upstream_host]), s.display.posture_note ? h('span', { class: 'posture-note' }, s.display.posture_note) : null),
      h('p', { class: 'sess-meta mono', id: 'sess-meta' }, metaLine(s, Date.now()))));
}

// emphasisParts splits the session's own sentence so the upstream host and
// the alias count ("1 model alias") can be bold: [{text, bold}].
export function emphasisParts(text, words) {
  const parts = [];
  const re = /\d+ model alias(?:es)?/;
  let rest = String(text || '');
  while (rest) {
    let at = -1;
    let len = 0;
    for (const w of words) {
      if (!w) continue;
      const i = rest.indexOf(w);
      if (i >= 0 && (at < 0 || i < at)) { at = i; len = w.length; }
    }
    const m = re.exec(rest);
    if (m && (at < 0 || m.index < at)) { at = m.index; len = m[0].length; }
    if (at < 0) { parts.push({ text: rest, bold: false }); break; }
    if (at > 0) parts.push({ text: rest.slice(0, at), bold: false });
    parts.push({ text: rest.slice(at, at + len), bold: true });
    rest = rest.slice(at + len);
  }
  return parts;
}

function emphasize(text, words) {
  return emphasisParts(text, words).map((p) => (p.bold ? h('b', null, p.text) : p.text));
}

// renderConfig draws the configuration details: sources, policies,
// fingerprints, for debugging and tickets.
export function renderConfig(el, rows) {
  show(el, h('details', { class: 'config' },
    h('summary', null, 'Configuration details'),
    h('dl', { class: 'config-grid' }, (rows || []).map((r) => [h('dt', null, r.label), h('dd', { class: 'mono' }, r.value)]))));
}
