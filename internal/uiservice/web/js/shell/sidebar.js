// The sidebar: running sessions first (oldest first, as the server sorts,
// so a new one appends and nothing jumps), then Projects and Profiles. Full
// width at ≥ 1024, else a 56px rail of monograms (or the full list in a
// drawer).

import { h, icon, show, rovingFocus } from '../dom.js';
import { ago, since } from '../fmt.js';

// sessionItems is what each session item shows. current is the id being
// viewed.
export function sessionItems(sessions, current, now) {
  const titles = {};
  for (const s of sessions) {
    const t = s.display.title;
    titles[t] = (titles[t] || 0) + 1;
  }
  return sessions.map((s) => {
    const ended = s.state === 'ended';
    const title = s.display.title;
    const badge = ended ? '' : s.display.badge || '';
    const sub = ended ? 'ended ' + ago(s.ended_at, now) : s.display.subtitle + ' · ' + since(s.started_at, now);
    const health = ended ? 'ended' : s.health || 'ok';
    return {
      id: s.id,
      title,
      dup: titles[title] > 1 ? ' · ' + s.id.slice(0, 4) : '',
      sub,
      badge,
      health,
      ended,
      current: s.id === current,
      mono: title.slice(0, 2),
      tip: title + ' — ' + sub + (badge ? ' · ' + badge : ''),
    };
  });
}

// renderSidebar draws the sidebar. m: {items, rail, loading, route,
// counts: {projects, profiles}, address, onSession(id), onNav(view)}.
export function renderSidebar(nav, m) {
  const activeId = m.route.view === 'session' ? m.currentId : '';
  const navItems = [
    { view: 'projects', label: 'Projects', icon: 'folder', count: m.counts.projects },
    { view: 'profiles', label: 'Profiles', icon: 'server', count: m.counts.profiles },
  ];
  const navCurrent = (v) => (m.route.view === v || (v === 'profiles' && m.route.view === 'profile'));
  // One tab stop: the current item, else the first.
  let tabbed = false;
  const tab = (isCurrent) => {
    if (isCurrent && !tabbed) { tabbed = true; return '0'; }
    return '-1';
  };
  if (m.rail) {
    const list = h('div', { class: 'rail' },
      m.loading ? [1, 2, 3].map(() => h('span', { class: 'rail-item', 'aria-hidden': 'true' })) : null,
      m.items.map((it) => h('button', {
        type: 'button', class: 'rail-item', 'data-nav': true, 'data-ended': it.ended,
        'aria-current': it.id === activeId ? 'page' : null, 'aria-label': it.tip, title: it.tip,
        tabindex: tab(it.id === activeId), onclick: () => m.onSession(it.id),
      }, it.mono, h('span', { class: 'rail-dot', 'data-health': it.health }))),
      h('span', { class: 'rail-sep' }),
      navItems.map((n) => h('button', {
        type: 'button', class: 'rail-item', 'data-nav': true, 'aria-current': navCurrent(n.view) ? 'page' : null,
        'aria-label': n.label, title: n.label, tabindex: tab(navCurrent(n.view)), onclick: () => m.onNav(n.view),
      }, icon(n.icon))));
    fixTabStop(list, tabbed);
    show(nav, list);
    return;
  }
  const live = m.items.filter((it) => !it.ended).length;
  const scroll = h('div', { class: 'sb-scroll' },
    h('div', { class: 'sb-head' },
      h('span', { class: 'sb-label', id: 'sb-sessions' }, 'Sessions'),
      h('span', { class: 'sb-count' }, m.loading ? '' : String(live)),
      h('span', { class: 'spacer' }),
      h('button', { type: 'button', class: 'sb-add', 'aria-label': 'Start a session from Projects', title: 'Start a session from Projects', onclick: () => m.onNav('projects') }, icon('plus'))),
    m.loading ? [1, 2, 3].map(() => h('div', { class: 'sb-skel', 'aria-hidden': 'true' }, h('span'), h('span'), h('span'), h('span'))) : null,
    !m.loading && m.items.length === 0
      ? h('p', { class: 'sb-empty' }, 'No running sessions. Start one from ', h('a', { href: '#/projects' }, 'Projects'), '.')
      : null,
    h('div', { class: 'sb-list', role: 'list', 'aria-labelledby': 'sb-sessions' },
      m.items.map((it) => h('div', { role: 'listitem' },
        h('button', {
          type: 'button', class: 'sb-item', 'data-nav': true, 'data-ended': it.ended,
          'aria-current': it.id === activeId ? 'page' : null, title: it.tip,
          tabindex: tab(it.id === activeId), onclick: () => m.onSession(it.id),
        },
        h('span', { class: 'dot', 'data-health': it.health }),
        h('span', { class: 'sb-title' }, it.title, it.dup ? h('span', { class: 'sb-dup' }, it.dup) : null),
        it.badge ? h('span', { class: 'badge', 'data-health': it.health }, it.badge) : h('span'),
        h('span'),
        h('span', { class: 'sb-sub' }, it.sub))))),
    h('div', { class: 'sb-divider' }),
    navItems.map((n) => h('button', {
      type: 'button', class: 'sb-nav', 'data-nav': true, 'aria-current': navCurrent(n.view) ? 'page' : null,
      tabindex: tab(navCurrent(n.view)), onclick: () => m.onNav(n.view),
    }, icon(n.icon), h('span', { class: 'sb-nav-label' }, n.label), h('span', { class: 'sb-nav-count' }, n.count == null ? '' : String(n.count)))));
  fixTabStop(scroll, tabbed);
  show(nav, scroll, h('div', { class: 'sb-foot' }, h('span', { class: 'sb-foot-dot' }), h('span', null, 'ccwrap ui'), h('span', { class: 'spacer' }), h('span', null, m.address)));
}

function fixTabStop(container, tabbed) {
  if (tabbed) return;
  const first = container.querySelector('[data-nav]');
  if (first) first.setAttribute('tabindex', '0');
}

export function initSidebar(nav) {
  rovingFocus(nav);
}
