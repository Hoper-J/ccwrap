// Toasts: top right, at most four, each gone after four seconds.

import { h } from '../dom.js';

const MAX = 4;

export function createToasts(stack) {
  return function toast(text, kind) {
    const el = h('div', { class: 'toast', role: 'status', 'data-kind': kind || null }, text);
    stack.appendChild(el);
    while (stack.children.length > MAX) stack.removeChild(stack.firstChild);
    setTimeout(() => el.setAttribute('data-leaving', ''), 3600);
    setTimeout(() => el.remove(), 4000);
  };
}
