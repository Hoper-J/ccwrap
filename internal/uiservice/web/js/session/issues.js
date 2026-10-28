// Issue cards: what went wrong, the evidence, and fixes as buttons. The
// session words every card; this lays them out and runs the fixes.

import { h, icon, show } from '../dom.js';
import { ago } from '../fmt.js';

// issueTitle splits a title into its code part (a model, host or env var,
// shown mono) and the rest.
export function issueTitle(issue) {
  const code = issue.title_code || '';
  if (code && issue.title.startsWith(code)) return { code, rest: issue.title.slice(code.length) };
  return { code: '', rest: issue.title };
}

// issueEvidence is the evidence line with how long ago it last happened.
export function issueEvidence(issue, now) {
  const when = issue.last_seen_at ? 'last ' + ago(issue.last_seen_at, now) : '';
  return [issue.evidence, when].filter(Boolean).join(' · ');
}

// renderIssues draws the cards into section. m: {issues, ended, pending:
// {id, value} | null, onFix(issue, fix)}.
export function renderIssues(section, m) {
  const issues = m.issues || [];
  if (!issues.length) {
    show(section, m.ended ? null : h('p', { class: 'no-issues' }, icon('check-circle'), 'No issues recorded since launch.'));
    return;
  }
  const open = issues.filter((i) => i.state === 'open').length;
  show(section,
    h('div', { class: 'section-head' },
      h('h2', { class: 'section-title', id: 'issues-title' }, 'Issues'),
      h('span', { class: 'section-count' }, open ? open + ' open' : 'all fixed'),
      h('span', { class: 'section-aside' }, 'since launch')),
    h('div', { class: 'issue-list', role: 'list', 'aria-labelledby': 'issues-title' }, issues.map((is) => card(is, m))));
}

function card(is, m) {
  const resolved = is.state !== 'open';
  const t = issueTitle(is);
  const pending = m.pending && m.pending.id === is.id ? m.pending : null;
  const actions = [];
  if (!resolved && !m.ended) {
    for (const fix of is.fixes || []) {
      const spinning = pending && pending.value === fix.value && pending.action === fix.action;
      actions.push(h('button', {
        type: 'button', class: 'btn btn-sm' + (fix.primary ? ' btn-primary' : ''), disabled: !!pending,
        onclick: () => m.onFix(is, fix),
      }, spinning ? icon('loader', 'spin') : null, fix.label));
    }
  }
  let note = is.note;
  if (pending && pending.note) note = pending.note;
  return h('article', { class: 'issue', role: 'listitem', 'data-state': is.state, 'data-severity': is.severity },
    icon(resolved ? 'check-circle' : 'alert-triangle', 'issue-icon'),
    h('div', { class: 'issue-body' },
      h('h3', { class: 'issue-title' }, t.code ? h('code', null, t.code) : null, t.rest),
      h('p', { class: 'issue-detail' }, is.detail),
      is.evidence || is.last_seen_at ? h('p', { class: 'issue-evidence', 'data-last-seen': is.last_seen_at || null }, issueEvidence(is, Date.now())) : null,
      resolved
        ? h('p', { class: 'issue-resolution' }, is.resolution || 'Resolved.')
        : (actions.length || (note && !m.ended))
          ? h('div', { class: 'issue-actions' }, actions, note && !m.ended ? h('span', { class: 'issue-note' }, note) : null)
          : null));
}

// refreshEvidence rewords the "last … ago" of every card in section.
export function refreshEvidence(section, issues) {
  for (const el of section.querySelectorAll('.issue-evidence[data-last-seen]')) {
    const is = issues.find((i) => i.last_seen_at === el.dataset.lastSeen);
    if (is) el.textContent = issueEvidence(is, Date.now());
  }
}
