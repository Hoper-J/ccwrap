// The app bar: brand, sidebar toggle, connection pill, overflow menu. Only
// global things live here.

import { h, icon, show } from '../dom.js';

const CONN_TITLE = {
  connecting: 'Connecting to ccwrap ui…',
  connected: 'Live: updates arrive as they happen.',
  reconnecting: 'The connection dropped; retrying. What is shown stays live once it is back.',
  disconnected: "ccwrap ui can't be reached.",
};

// renderAppbar draws the bar. m: {conn, toggleLabel, toggleExpanded,
// onToggle, menu: [{label, icon, href?, onSelect?}]}.
export function renderAppbar(bar, m) {
  const menuId = 'appbar-menu';
  const menu = h('div', { class: 'menu', role: 'menu', id: menuId, hidden: true },
    m.menu.map((it) => it.href
      ? h('a', { class: 'menu-item', role: 'menuitem', href: it.href, target: '_blank', rel: 'noopener noreferrer', onclick: () => close() }, icon(it.icon), it.label)
      : h('button', { type: 'button', class: 'menu-item', role: 'menuitem', onclick: () => { close(); it.onSelect(); } }, icon(it.icon), it.label)));
  const more = h('button', { type: 'button', class: 'icon-btn', 'aria-haspopup': 'menu', 'aria-expanded': 'false', 'aria-controls': menuId, 'aria-label': 'More', title: 'More' }, icon('more'));
  const wrap = h('div', { class: 'menu-wrap' }, more, menu);
  function close() {
    menu.hidden = true;
    more.setAttribute('aria-expanded', 'false');
  }
  more.addEventListener('click', () => {
    const open = menu.hidden;
    menu.hidden = !open;
    more.setAttribute('aria-expanded', String(open));
    if (open) {
      const first = menu.querySelector('.menu-item');
      if (first) first.focus();
    }
  });
  menu.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') { close(); more.focus(); }
  });
  document.addEventListener('mousedown', (e) => { if (!wrap.contains(e.target)) close(); });
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && !menu.hidden) { close(); more.focus(); } });

  const pill = h('span', { class: 'conn-pill', role: 'status', id: 'conn-pill' });
  const toggle = h('button', { type: 'button', class: 'icon-btn', id: 'sidebar-toggle', onclick: m.onToggle }, icon('panel-left'));
  show(bar,
    h('div', { class: 'brand' }, h('span', { class: 'brand-mark', 'aria-hidden': 'true' }), h('span', { class: 'brand-word' }, 'CCWRAP')),
    h('span', { class: 'appbar-divider', 'aria-hidden': 'true' }),
    toggle,
    h('span', { class: 'spacer' }),
    pill,
    wrap);
  return {
    // conn shows a connection state on the pill.
    conn(state) {
      pill.dataset.state = state;
      pill.textContent = state;
      pill.title = CONN_TITLE[state] || state;
    },
    toggle(label, expanded) {
      toggle.setAttribute('aria-label', label);
      toggle.title = label;
      toggle.setAttribute('aria-expanded', String(expanded));
    },
  };
}
