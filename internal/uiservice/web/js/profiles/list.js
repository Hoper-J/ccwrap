// The profile list: every profile with its provider, host, auth, aliases,
// the running sessions on it and its last test. Rows open the profile page.

import { h, icon, show } from '../dom.js';
import { ago } from '../fmt.js';

// authLabel is a profile's auth in a few words: "bearer · env",
// "x-api-key · inline", "passthrough".
export function authLabel(auth) {
  if (!auth) return 'passthrough';
  const mode = auth.mode === 'ccwrap_x_api_key' ? 'x-api-key' : auth.mode === 'ccwrap_bearer' ? 'bearer' : auth.mode;
  const src = auth.has_key_env ? 'env' : auth.has_inline_key ? 'inline' : '';
  return src ? mode + ' · ' + src : mode;
}

// aliasCount counts the aliases, the "other Claude models" rule included.
export function aliasCount(p) {
  return Object.keys(p.model_aliases || {}).length;
}

// testChip is a last test's status chip; its title says the whole result.
export function testChip(t) {
  return h('span', { class: 'test-chip mono', 'data-status': t.status, title: t.text + (t.at ? ' · tested ' + ago(t.at) : '') }, t.status);
}

// mountProfiles shows the list. deps: {api, toast, go, sessions()}.
export function mountProfiles(main, deps) {
  let data = null;
  const running = {}; // name → testing
  const root = h('div', { class: 'main-inner' });
  show(main, root);

  async function load() {
    try {
      data = await deps.api.get('/profiles');
      draw();
    } catch (err) {
      show(root, h('section', { class: 'placeholder' }, h('h1', { class: 'page-title' }, 'Profiles'), h('p', { class: 'lede' }, "Couldn't load profiles: " + err.message)));
    }
  }
  load();

  async function test(name) {
    running[name] = true;
    draw();
    try {
      const t = await deps.api.post('/profiles/' + encodeURIComponent(name) + '/test', {});
      const p = data.profiles.find((x) => x.name === name);
      if (p) p.last_test = t;
    } catch (err) {
      deps.toast("Couldn't test " + name + ': ' + err.message, 'error');
    }
    delete running[name];
    draw();
  }

  // testAll tests every profile, four at a time.
  async function testAll() {
    const queue = data.profiles.map((p) => p.name);
    const worker = async () => { while (queue.length) await test(queue.shift()); };
    await Promise.all([worker(), worker(), worker(), worker()]);
  }

  function draw() {
    const sessions = deps.sessions();
    const health = (id) => (sessions.find((s) => s.id === id) || {}).health || 'ok';
    show(root, h('section', { class: 'profiles', 'aria-labelledby': 'prof-title' },
      h('div', { class: 'page-head' },
        h('div', { class: 'page-head-text' },
          h('h1', { class: 'page-title', id: 'prof-title' }, 'Profiles'),
          h('p', { class: 'page-sub' }, 'Saved in ', h('span', { class: 'mono' }, data.display_path || data.path), '.')),
        h('div', { class: 'page-head-acts' },
          h('button', { type: 'button', class: 'btn', disabled: !data.profiles.length || Object.keys(running).length > 0, onclick: testAll }, icon('activity'), 'Test all'),
          h('button', { type: 'button', class: 'btn btn-primary', onclick: () => deps.go('#/profiles/new') }, icon('plus'), 'New profile'))),
      data.profiles.length ? h('div', { class: 'prof-table', role: 'table', 'aria-label': 'Profiles' },
        h('div', { class: 'prof-row prof-head', role: 'row' },
          ['Name', 'Provider', 'Base URL', 'Auth', 'Aliases', 'Sessions', 'Last test', ''].map((c, i) => h('div', { role: 'columnheader', class: i >= 1 && i <= 5 ? 'prof-wide' : null }, c))),
        data.profiles.map((p) => {
          const open = () => deps.go('#/profiles/' + encodeURIComponent(p.name));
          const live = (p.sessions || []).filter((id) => sessions.some((s) => s.id === id && s.state !== 'ended'));
          return h('div', { class: 'prof-row', role: 'row', onclick: (e) => { if (!e.target.closest('button')) open(); } },
            h('div', { role: 'cell', class: 'prof-name-cell' },
              h('button', { type: 'button', class: 'prof-name', onclick: open }, p.name),
              data.default === p.name ? h('span', { class: 'default-pill' }, 'default') : null,
              h('div', { class: 'prof-narrow mono' }, [p.provider, p.base_url_host, authLabel(p.auth), live.length ? live.length + ' running' : ''].filter(Boolean).join(' · '))),
            h('div', { role: 'cell', class: 'prof-wide' }, p.provider || '—'),
            h('div', { role: 'cell', class: 'prof-wide mono prof-host' }, p.base_url_host || '—'),
            h('div', { role: 'cell', class: 'prof-wide mono' }, authLabel(p.auth)),
            h('div', { role: 'cell', class: 'prof-wide mono' }, String(aliasCount(p))),
            h('div', { role: 'cell', class: 'prof-wide prof-sessions' }, live.length ? [live.map((id) => h('span', { class: 'dot', 'data-health': health(id) })), h('span', null, live.length + ' running')] : h('span', { class: 'muted' }, '—')),
            h('div', { role: 'cell', class: 'prof-test' },
              running[p.name] ? h('span', { class: 'status-line' }, icon('loader', 'spin'), 'testing…')
                : p.last_test ? testChip(p.last_test) : h('span', { class: 'muted' }, 'not tested')),
            h('div', { role: 'cell', class: 'prof-acts' },
              h('button', { type: 'button', class: 'icon-btn icon-btn-xs', 'aria-label': 'Test ' + p.name, title: 'Test ' + p.name, disabled: !!running[p.name], onclick: () => test(p.name) }, icon('activity')),
              h('button', { type: 'button', class: 'icon-btn icon-btn-xs', 'aria-label': 'Edit ' + p.name, title: 'Edit ' + p.name, onclick: open }, icon('edit'))));
        })) : h('div', { class: 'proj-empty' },
        h('span', { class: 'proj-empty-icon' }, icon('server')),
        h('h2', null, 'No profiles yet'),
        h('p', null, 'A profile names an upstream — its base URL, key and model aliases — so a session can run on it. Sessions without one use the launch environment.'),
        h('p', { class: 'proj-empty-src' }, h('button', { type: 'button', class: 'btn btn-primary', onclick: () => deps.go('#/profiles/new') }, icon('plus'), 'New profile')))));
  }

  return {
    // update redraws for a new session list (the running column).
    update() { if (data) draw(); },
    unmount() {},
  };
}
