// Building the page without HTML strings: elements come from createElement
// and every piece of text goes in as a text node, so nothing the API sends
// can become markup.

const SVG_NS = 'http://www.w3.org/2000/svg';

// h makes an element. props: class, text, on<Event> handlers, and any
// attribute (true sets it empty, false/null leaves it out). Children are
// nodes, strings or (nested) arrays of them; null and false are skipped.
export function h(tag, props, ...children) {
  const el = document.createElement(tag);
  setProps(el, props);
  append(el, children);
  return el;
}

function setProps(el, props) {
  if (!props) return;
  for (const [k, v] of Object.entries(props)) {
    if (v == null || v === false) continue;
    if (k === 'class') el.className = v;
    else if (k === 'text') el.textContent = String(v);
    else if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
    else el.setAttribute(k, v === true ? '' : String(v));
  }
}

export function append(el, children) {
  for (const c of [children].flat(Infinity)) {
    if (c == null || c === false) continue;
    el.appendChild(typeof c === 'object' ? c : document.createTextNode(String(c)));
  }
  return el;
}

// icon is a sprite icon (#i-<name> in the page's inline sprite).
export function icon(name, cls) {
  const svg = document.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('class', cls ? 'icon ' + cls : 'icon');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  const use = document.createElementNS(SVG_NS, 'use');
  use.setAttribute('href', '#i-' + name);
  svg.appendChild(use);
  return svg;
}

// show puts children into el in place of what was there.
export function show(el, ...children) {
  while (el.firstChild) el.removeChild(el.firstChild);
  return append(el, children);
}

// rovingFocus makes the [data-nav] items inside container one tab stop:
// ↑/↓ move, Home/End jump.
export function rovingFocus(container) {
  container.addEventListener('keydown', (e) => {
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(e.key)) return;
    const items = Array.from(container.querySelectorAll('[data-nav]'));
    const i = items.indexOf(document.activeElement);
    if (i < 0) return;
    e.preventDefault();
    const j = e.key === 'ArrowDown' ? Math.min(items.length - 1, i + 1)
      : e.key === 'ArrowUp' ? Math.max(0, i - 1)
        : e.key === 'Home' ? 0 : items.length - 1;
    for (const it of items) it.setAttribute('tabindex', '-1');
    items[j].setAttribute('tabindex', '0');
    items[j].focus();
  });
}

// copyText copies text through a hidden textarea and execCommand: unlike
// the Clipboard API it also works on plain HTTP (a forwarded port).
export function copyText(text) {
  const ta = document.createElement('textarea');
  ta.value = text;
  ta.setAttribute('readonly', '');
  ta.className = 'visually-hidden';
  document.body.appendChild(ta);
  ta.select();
  let ok = false;
  try { ok = document.execCommand('copy'); } catch (_) { ok = false; }
  ta.remove();
  return ok;
}
