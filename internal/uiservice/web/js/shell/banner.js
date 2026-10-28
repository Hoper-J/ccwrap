// The disconnected banner: what happened, the command that recovers it, the
// last contact and a Reconnect button.

import { h, icon, show } from '../dom.js';
import { clock } from '../fmt.js';

export function renderBanner(slot, m) {
  if (!m.shown) {
    show(slot);
    return;
  }
  show(slot, h('div', { class: 'banner', role: 'alert' },
    icon('alert-triangle'),
    h('span', { class: 'banner-head' }, "ccwrap ui stopped or can't be reached."),
    h('span', { class: 'banner-text' }, 'Run ', h('code', null, 'ccwrap open'), ' in a terminal.'),
    h('span', { class: 'spacer' }),
    h('span', { class: 'banner-meta' }, (m.lastContact ? 'last contact ' + clock(m.lastContact) + ' · ' : '') + 'still retrying'),
    h('button', { type: 'button', class: 'btn', onclick: m.onReconnect }, icon('refresh-cw'), 'Reconnect')));
}
