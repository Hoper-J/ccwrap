// The ribbon: the session's posture in cells (the session words them in
// display.cells) and the popovers behind the interactive ones — models and
// their fix, profile hot-swap, body capture, the egress probe, the native-TLS
// fingerprints.

import { h, icon, show, copyText } from '../dom.js';

// Cells that open a popover, and when.
function interactive(cell) {
  switch (cell.key) {
    case 'models': return cell.state === 'aliases-active' || cell.state === 'aliases-warn';
    case 'profile': case 'bodies': return true;
    case 'native_tls': return cell.state === 'native-active' || cell.state === 'native-blocked';
  }
  return false;
}

// countryFlag turns a two-letter country code into its flag and code.
export function countryFlag(code) {
  const c = String(code || '').trim();
  if (!/^[A-Za-z]{2}$/.test(c)) return c;
  const up = c.toUpperCase();
  return String.fromCodePoint(0x1F1E6 + up.charCodeAt(0) - 65) + String.fromCodePoint(0x1F1E6 + up.charCodeAt(1) - 65) + ' ' + up;
}

// egressLine is a probe result in one line: "✓ 203.0.113.9 · Tokyo, 🇯🇵 JP · 120ms".
export function egressLine(r) {
  const status = String(r.status || 'UNKNOWN');
  const ms = (typeof r.latency_ms === 'number' ? r.latency_ms : 0) + 'ms';
  if (status !== 'OK') return { ok: false, text: '✗ ' + [status, ms, r.err].filter(Boolean).join(' · ') };
  const geo = [r.city, r.region, r.country ? countryFlag(r.country) : ''].filter(Boolean).join(', ');
  return { ok: true, text: '✓ ' + [r.public_ip, geo, r.org, ms].filter(Boolean).join(' · ') };
}

// aliasRows is the Models popover's table: each alias, the "*" default last
// as "other Claude models".
export function aliasRows(forward) {
  const rows = Object.keys(forward || {}).filter((k) => k !== '*').sort().map((k) => ({ from: k, to: forward[k] }));
  if (forward && forward['*']) rows.push({ from: 'other Claude models', to: forward['*'] === '*' ? 'keep their Claude names' : forward['*'], other: true });
  return rows;
}

// createRibbon draws the ribbon into container. deps: {api, base (the
// session's API path), toast, go(hash), profiles(): Promise<list>,
// onFix(issue, fix), pending(): {id,…}|null}.
export function createRibbon(container, deps) {
  let sess = null;
  let openKey = '';
  let pop = null;
  let probe = null; // {running} or {result}
  const grid = h('div', { class: 'ribbon' });
  show(container, grid);

  document.addEventListener('mousedown', (e) => {
    if (pop && !pop.contains(e.target) && !(e.target.closest && e.target.closest('.ribbon-cell[aria-expanded="true"]'))) close();
  });
  document.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && pop) {
      const k = openKey;
      close();
      const c = grid.querySelector('[data-key="' + k + '"]');
      if (c) c.focus();
    }
  });

  function close() {
    if (pop) pop.remove();
    pop = null;
    if (openKey) {
      const c = grid.querySelector('[data-key="' + openKey + '"]');
      if (c) c.setAttribute('aria-expanded', 'false');
    }
    openKey = '';
  }

  function toggle(cell) {
    if (openKey === cell.key) { close(); return; }
    close();
    openKey = cell.key;
    const el = grid.querySelector('[data-key="' + cell.key + '"]');
    el.setAttribute('aria-expanded', 'true');
    // Cells in the right half open their popover leftward.
    const right = el.getBoundingClientRect().left - grid.getBoundingClientRect().left > grid.clientWidth / 2;
    pop = h('div', { class: right ? 'pop pop-right' : 'pop', role: 'dialog', 'aria-label': cell.label });
    el.appendChild(pop);
    fill(cell.key);
    reveal();
  }

  // reveal scrolls the open popover into view: a cell near the bottom of
  // the window opens it below the fold.
  function reveal() {
    if (pop) pop.scrollIntoView({ block: 'nearest' });
  }

  function fill(key) {
    if (!pop) return;
    if (key === 'models') show(pop, modelsPop());
    else if (key === 'profile') profilePop();
    else if (key === 'bodies') show(pop, bodiesPop());
    else if (key === 'native_tls') nativeTLSPop();
  }

  function modelsPop() {
    const issue = (sess.issues || []).find((i) => i.id === 'unaliased_model' && i.state === 'open');
    const pending = deps.pending();
    return [
      issue ? h('div', { class: 'pop-fix' },
        h('p', { class: 'pop-fix-text' }, issue.detail),
        h('div', { class: 'issue-actions' }, (issue.fixes || []).map((fix) => h('button', {
          type: 'button', class: 'btn btn-sm' + (fix.primary ? ' btn-primary' : ''), disabled: !!pending,
          onclick: () => deps.onFix(issue, fix),
        }, fix.label))),
        issue.note ? h('p', { class: 'pop-note' }, issue.note) : null) : null,
      h('div', { class: 'pop-label' }, 'claude sends → forward to'),
      h('div', { class: 'alias-table' }, aliasRows(sess.model_alias_forward).map((r) => h('div', { class: 'alias-row' + (r.other ? ' other' : '') },
        h('span', { class: 'alias-from mono' }, r.from), h('span', { class: 'alias-arrow', 'aria-hidden': 'true' }, '→'), h('span', { class: 'alias-to mono' }, r.to)))),
      sess.active_profile_name ? h('div', { class: 'pop-foot' }, h('a', { href: '#/profiles/' + encodeURIComponent(sess.active_profile_name) }, 'Edit aliases in profile ' + sess.active_profile_name)) : null,
    ];
  }

  async function profilePop() {
    show(pop, h('p', { class: 'pop-note' }, 'loading…'));
    let list;
    try { list = await deps.profiles(); } catch (err) { if (pop) show(pop, h('p', { class: 'pop-note' }, "Couldn't load profiles: " + err.message)); return; }
    if (!pop || openKey !== 'profile') return;
    const current = sess.active_profile_name || 'inherit-env';
    const row = (name, meta) => h('div', { class: 'pp-row' + (name === current ? ' on' : '') },
      h('button', {
        type: 'button', class: 'pp-pick', 'aria-pressed': String(name === current), disabled: name === current,
        onclick: () => switchTo(name),
      }, h('span', { class: 'pp-radio', 'aria-hidden': 'true' }), h('span', { class: 'pp-name' }, name), h('span', { class: 'pp-meta mono' }, meta)),
      name !== 'inherit-env' ? h('button', { type: 'button', class: 'pp-edit', 'aria-label': 'Edit profile ' + name, title: 'Edit profile ' + name, onclick: () => { close(); deps.go('#/profiles/' + encodeURIComponent(name)); } }, icon('edit')) : null);
    show(pop,
      h('div', { class: 'pop-label' }, 'Switch this session to'),
      h('div', { class: 'pp-list' }, list.map((p) => row(p.name, [p.base_url_host, p.model_alias_count ? p.model_alias_count + (p.model_alias_count === 1 ? ' alias' : ' aliases') : ''].filter(Boolean).join(' · '))),
        row('inherit-env', 'no profile · the launch environment')),
      h('div', { class: 'pop-foot' }, h('a', { href: '#/profiles', onclick: () => close() }, 'Manage profiles')));
    reveal();
  }

  async function switchTo(name) {
    close();
    try {
      const out = await deps.api.post(deps.base + '/profile', { name });
      if (out.result === 'switched') deps.toast('Switched to ' + (name === 'inherit-env' ? 'inherit-env' : 'profile ' + name) + '.');
      else deps.toast(out.message || 'Not switched: ' + out.result, out.result === 'refused_needs_relaunch' ? 'warning' : 'error');
    } catch (err) {
      deps.toast('Not switched: ' + err.message, 'error');
    }
  }

  function bodiesPop() {
    const row = (label, desc, on, field) => h('button', {
      type: 'button', class: 'bodies-row', role: 'switch', 'aria-checked': String(on),
      onclick: async () => {
        try { await deps.api.post(deps.base + '/capture', { [field]: !on }); } catch (err) { deps.toast("Couldn't change capture: " + err.message, 'error'); }
      },
    }, h('span', { class: 'bodies-switch', 'aria-hidden': 'true' }), h('span', { class: 'bodies-text' }, h('span', { class: 'bodies-label' }, label), h('span', { class: 'bodies-desc' }, desc)));
    return [
      row('Request and response bodies', 'Records what Claude Code sends and receives. Credentials are masked.', !!sess.capture_bodies, 'bodies'),
      row('Telemetry bodies', 'Opens the Datadog and Sentry reports Claude Code sends.', !!sess.capture_telemetry, 'telemetry'),
      sess.capture_bodies_unmasked ? h('p', { class: 'pop-note danger' }, 'CCWRAP_UNMASK_CREDENTIALS=1: captured bodies keep their credentials.') : null,
    ];
  }

  async function nativeTLSPop() {
    show(pop, h('p', { class: 'pop-note' }, 'loading…'));
    let d;
    try { d = await deps.api.get(deps.base + '/native-tls'); } catch (err) { if (pop) show(pop, h('p', { class: 'pop-note' }, "Couldn't load: " + err.message)); return; }
    if (!pop || openKey !== 'native_tls') return;
    if (!d || !d.captured) { show(pop, h('p', { class: 'pop-note' }, 'No ClientHello captured yet.')); return; }
    const fp = (f) => [['JA3', f.ja3], ['JA4', f.ja4], ['peetprint', f.peetprint]].filter((r) => r[1]).map(([k, v]) => h('div', { class: 'fp-row' },
      h('span', { class: 'fp-k' }, k),
      h('button', { type: 'button', class: 'fp-v mono', title: 'Copy ' + k, onclick: () => deps.toast(copyText(v) ? k + ' copied.' : "Couldn't copy.") }, v)));
    const dl = (key, name) => h('a', { class: 'bv-dl', href: '/api/v1' + deps.base + '/native-tls/clienthello.bin' + (key ? '?key=' + encodeURIComponent(key) : ''), download: name }, 'download ' + name);
    const stacks = d.stacks || [];
    const used = (s) => h('p', { class: 'pop-note' }, 'used by: ' + (s.paths && s.paths.length ? s.paths.join(', ') : 'no request yet'));
    show(pop,
      stacks.length > 1 ? used(stacks[0]) : null, fp(d), dl('', 'clienthello.bin'),
      stacks.slice(1).map((s) => [used(s), fp(s), dl(s.key, 'clienthello-' + s.key + '.bin')]),
      d.note ? h('p', { class: 'pop-note' }, d.note) : null);
    reveal();
  }

  async function runProbe() {
    probe = { running: true };
    render(sess);
    try {
      probe = { result: await deps.api.post(deps.base + '/egress-test', {}) };
    } catch (err) {
      probe = { result: { status: 'ERROR', err: err.message } };
    }
    render(sess);
  }

  function cellEl(cell, ended) {
    const live = !ended && interactive(cell);
    const el = h('div', {
      class: 'ribbon-cell', 'data-key': cell.key, 'data-state': cell.state || null,
      role: live ? 'button' : null, tabindex: live ? '0' : null,
      'aria-haspopup': live ? 'dialog' : null, 'aria-expanded': live ? String(openKey === cell.key) : null,
    },
    h('div', { class: 'k' }, cell.label),
    cell.key === 'profile'
      ? h('div', { class: 'v' }, h('span', { class: 'profile-chip' + (live ? ' has-pop' : ''), title: cell.value }, h('span', { class: 'profile-chip-name' }, cell.value)))
      : h('div', { class: 'v' + (cell.mono ? ' mono' : '') + (live ? ' has-pop' : '') }, cell.value),
    cell.detail ? h('div', { class: 'd' }, cell.detail) : null);
    if (live) {
      el.addEventListener('click', (e) => { if (!pop || !pop.contains(e.target)) toggle(cell); });
      el.addEventListener('keydown', (e) => {
        if ((e.key === 'Enter' || e.key === ' ') && e.target === el) { e.preventDefault(); toggle(cell); }
      });
    }
    if (cell.key === 'egress' && !ended) {
      el.appendChild(h('button', {
        type: 'button', class: 'egress-probe', 'aria-label': 'Test where traffic exits', title: 'Test where traffic exits',
        disabled: !!(probe && probe.running), onclick: runProbe,
      }, icon(probe && probe.running ? 'loader' : 'activity', probe && probe.running ? 'spin' : '')));
      if (probe && probe.result) {
        const line = egressLine(probe.result);
        el.appendChild(h('div', { class: 'egress-result' + (line.ok ? ' ok' : ' fail') }, line.text));
      }
    }
    return el;
  }

  function render(next) {
    sess = next;
    const ended = sess.state === 'ended';
    if (ended) close();
    const cells = (sess.display && sess.display.cells) || [];
    const keepOpen = openKey;
    const popEl = pop;
    grid.dataset.count = String(cells.length);
    show(grid, cells.map((c) => cellEl(c, ended)));
    // A redraw keeps an open popover where it was, refreshed.
    if (keepOpen && popEl) {
      const el = grid.querySelector('[data-key="' + keepOpen + '"]');
      if (el) {
        el.setAttribute('aria-expanded', 'true');
        el.appendChild(popEl);
        if (keepOpen === 'models' || keepOpen === 'bodies') fill(keepOpen);
      } else {
        pop = null;
        openKey = '';
      }
    }
  }

  return { render, close };
}
