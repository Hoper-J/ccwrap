// The profile page: one profile's fields, with the gating the session
// page's editor had (renderEditOrAddPanel), testing, and delete. Saving
// sends only what changed; a blank key keeps the stored one.

import { h, icon, show } from '../dom.js';
import { since } from '../fmt.js';
import { testChip } from './list.js';

export const AUTH_MODES = [
  ['passthrough', 'passthrough'],
  ['ccwrap_bearer', 'Bearer token'],
  ['ccwrap_x_api_key', 'x-api-key'],
];
export const EGRESS_MODES = ['inherit', 'direct', 'http', 'socks5', 'socks5h'];

export function authModeHint(mode) {
  switch (mode) {
    case 'ccwrap_bearer': return 'sent as Authorization: Bearer … · env ANTHROPIC_AUTH_TOKEN';
    case 'ccwrap_x_api_key': return 'sent as x-api-key: … · env ANTHROPIC_API_KEY';
    case 'passthrough': return 'no injection — Claude credentials pass straight through';
  }
  return '';
}

export function egressModeHint(mode) {
  switch (mode) {
    case 'inherit': return 'follows the session egress — env proxy or direct';
    case 'direct': return 'no proxy — connects straight to the API';
    case 'http': return 'routes through an HTTP CONNECT proxy';
    case 'socks5': return 'SOCKS5 proxy — DNS resolved locally';
    case 'socks5h': return 'SOCKS5 proxy — DNS resolved at the proxy';
  }
  return '';
}

export function egressPlaceholder(mode) {
  switch (mode) {
    case 'http': return 'http://proxy:8080';
    case 'socks5': return 'socks5://proxy:1080';
    case 'socks5h': return 'socks5h://proxy:1080';
  }
  return '';
}

export function isProxyMode(mode) {
  return mode === 'http' || mode === 'socks5' || mode === 'socks5h';
}

// testButton is the Test connectivity button's state. The test runs against
// the saved profile, so a new profile or unsaved changes are saved first.
export function testButton(isNew, dirty, running) {
  if (isNew) return { disabled: true, title: 'Save the profile to test it' };
  if (dirty) return { disabled: true, title: 'Save your changes to test them' };
  return { disabled: !!running, title: 'Send one small model request with this profile' };
}

// aliasDefaultChoices is what "other Claude models →" offers: not set, each
// alias target (one marked suggested when every alias maps to it), the
// stored value, keep their Claude names, other model…. A new profile
// preselects the suggestion until the user picks.
export function aliasDefaultChoices(targets, current, isNew, picked) {
  const suggested = targets.length === 1 ? targets[0] : '';
  if (isNew && !picked) current = suggested;
  const options = [['', 'not set']];
  for (const t of targets) options.push([t, t + (!isNew && current === '' && t === suggested ? ' (suggested)' : '')]);
  if (current && current !== '*' && current !== '__custom' && !targets.includes(current)) options.push([current, current]);
  options.push(['*', 'keep their Claude names']);
  options.push(['__custom', 'other model…']);
  return { options, value: current };
}

// fallbackDefault is the default the service picks after deleting the
// default profile: official when it remains, else the first name, else
// inherit-env (profileapi.ChooseFallbackDefault).
export function fallbackDefault(names, removing) {
  if (names.includes('official') && removing !== 'official') return 'official';
  const rest = names.filter((n) => n !== removing).sort();
  return rest.length ? rest[0] : 'inherit-env';
}

// draftFrom is the form's starting state for a profile (null = new).
export function draftFrom(p) {
  if (!p) {
    return { name: '', provider: '', base_url: '', auth_mode: 'ccwrap_bearer', key_source: 'inline', key: '', aliases: [], other: '', otherCustom: '', otherPicked: false, egress_mode: 'inherit', egress_url: '', set_default: false };
  }
  const aliases = Object.keys(p.model_aliases || {}).filter((k) => k !== '*').sort().map((k) => ({ from: k, to: p.model_aliases[k] }));
  return {
    name: p.name, provider: p.provider || '', base_url: p.base_url || '',
    auth_mode: p.auth ? p.auth.mode : 'passthrough',
    key_source: p.auth && p.auth.has_key_env ? 'env_var' : 'inline',
    key: '', aliases, other: (p.model_aliases || {})['*'] || '', otherCustom: '', otherPicked: false,
    egress_mode: p.egress_mode || 'inherit', egress_url: p.egress_url || '', set_default: false,
  };
}

// aliasMap is the draft's aliases as profiles.json stores them, "*" for
// other Claude models; rows missing either side are left out.
export function aliasMap(d) {
  const out = {};
  for (const r of d.aliases) if (r.from.trim() && r.to.trim()) out[r.from.trim()] = r.to.trim();
  const other = d.other === '__custom' ? d.otherCustom.trim() : d.other;
  if (other) out['*'] = other;
  return out;
}

const NAME_RE = /^[A-Za-z0-9._-]+$/;

// problems is what keeps the draft from saving, by field, worded for the
// user; warnings are advisory. orig is the saved profile (null = new),
// names the existing profile names.
export function problems(d, orig, names) {
  const errs = {};
  const warns = {};
  const isNew = !orig;
  if (isNew) {
    if (!d.name.trim()) errs.name = 'A name is required.';
    else if (!NAME_RE.test(d.name.trim())) errs.name = 'Use letters, digits, dot, dash or underscore.';
    else if (names.includes(d.name.trim())) errs.name = 'A profile with this name exists.';
  }
  const base = d.base_url.trim();
  if (!base) errs.base_url = 'A base URL is required.';
  else if (!/^https?:\/\/\S+$/.test(base)) errs.base_url = 'Use an http or https URL.';
  else if (/\/v1\/?$/.test(base)) warns.base_url = 'Claude Code adds /v1 itself, so this would send requests to /v1/v1/messages. Drop the /v1.';
  if (d.auth_mode !== 'passthrough') {
    const stored = orig && orig.auth ? (d.key_source === 'env_var' ? orig.auth.has_key_env : orig.auth.has_inline_key) : false;
    if (!d.key.trim() && !stored) errs.key = d.key_source === 'env_var' ? 'Name the environment variable that holds the key.' : 'Paste the key.';
  }
  if (isProxyMode(d.egress_mode) && !/^(https?|socks5h?):\/\/\S+$/.test(d.egress_url.trim())) errs.egress_url = 'A proxy URL is required for ' + d.egress_mode + '.';
  const froms = new Set();
  for (const r of d.aliases) {
    const f = r.from.trim();
    const t = r.to.trim();
    if (!f && !t) continue;
    if (!f || !t) { errs.aliases = 'Fill both sides of each alias, or remove the row.'; break; }
    if (froms.has(f)) { errs.aliases = f + ' has two aliases.'; break; }
    froms.add(f);
  }
  if (d.other === '__custom' && !d.otherCustom.trim()) errs.other = 'Name the model other Claude models go to.';
  return { errs, warns };
}

function sameMap(a, b) {
  const ka = Object.keys(a).sort();
  const kb = Object.keys(b).sort();
  return ka.length === kb.length && ka.every((k, i) => k === kb[i] && a[k] === b[k]);
}

// editPayload is the PATCH body: only what changed. A blank key keeps the
// stored one; a new key or variable replaces it.
export function editPayload(orig, d) {
  const out = {};
  if (d.provider !== (orig.provider || '')) out.provider = d.provider;
  if (d.base_url.trim() !== (orig.base_url || '')) out.base_url = d.base_url.trim();
  const wasOn = !!orig.auth;
  const on = d.auth_mode !== 'passthrough';
  const key = d.key.trim();
  if (on && !wasOn) {
    out.auth = { mode: d.auth_mode };
    if (d.key_source === 'env_var') out.auth.key_env = key; else out.auth.key = key;
  } else if (!on && wasOn) {
    out.auth = null;
  } else if (on && wasOn) {
    const delta = {};
    if (d.auth_mode !== orig.auth.mode) delta.mode = d.auth_mode;
    if (key) {
      delta.key_source = d.key_source;
      if (d.key_source === 'env_var') delta.key_env = key; else delta.key = key;
    }
    if (Object.keys(delta).length) out.auth = delta;
  }
  const map = aliasMap(d);
  if (!sameMap(map, orig.model_aliases || {})) out.model_aliases = map;
  const egress = {};
  if (d.egress_mode !== (orig.egress_mode || 'inherit')) egress.mode = d.egress_mode;
  const url = isProxyMode(d.egress_mode) ? d.egress_url.trim() : '';
  if (url !== (orig.egress_url || '')) egress.url = url;
  if (Object.keys(egress).length) out.egress = egress;
  if (d.set_default) out.set_default = true;
  return out;
}

// addPayload is the POST body for a new profile.
export function addPayload(d) {
  const out = { name: d.name.trim(), provider: d.provider.trim(), base_url: d.base_url.trim(), auth: null, egress: { mode: d.egress_mode, url: isProxyMode(d.egress_mode) ? d.egress_url.trim() : '' }, model_aliases: aliasMap(d), set_default: d.set_default };
  if (d.auth_mode !== 'passthrough') {
    out.auth = { mode: d.auth_mode };
    if (d.key_source === 'env_var') out.auth.key_env = d.key.trim(); else out.auth.key = d.key.trim();
  }
  return out;
}

// mountProfileForm shows the profile page. deps: {api, toast, go, name
// ('' = new), field (flagged by an issue), sessions(): list,
// onSaved()}. It returns {unmount()}.
export function mountProfileForm(main, deps) {
  const isNew = !deps.name;
  let orig = null;
  let names = [];
  let draft = draftFrom(null);
  let initial = JSON.stringify(draft);
  let saving = false;
  let confirmDelete = false;
  let test = null;       // {running} | lastTest
  let egressTest = null; // {running} | {result}
  let reveal = false;
  let unaliased = [];
  let flagged = deps.field || '';
  // composing is true while an input method composes text (Chinese, say):
  // redrawing then would break the composition.
  let composing = false;

  const root = h('div', { class: 'main-inner profile-page' });
  // The footer sits after the page, outside its width cap, so it spans the
  // main column; draw adds it once the profile has loaded.
  const foot = h('div', { class: 'pf-footer' });
  show(main, root);

  async function load() {
    try {
      const list = await deps.api.get('/profiles');
      names = (list.profiles || []).map((p) => p.name);
      if (!isNew) {
        orig = await deps.api.get('/profiles/' + encodeURIComponent(deps.name));
        draft = draftFrom(orig);
        initial = JSON.stringify(draft);
        test = orig.last_test || null;
        unaliased = orig.unaliased || [];
      }
      draw();
    } catch (err) {
      show(root, h('section', { class: 'placeholder' }, h('h1', { class: 'page-title' }, isNew ? 'New profile' : deps.name),
        h('p', { class: 'lede' }, err.status === 404 ? 'No profile has this name.' : "Couldn't load the profile: " + err.message),
        h('a', { href: '#/profiles' }, '← Profiles')));
    }
  }
  load();

  const set = (patch) => { Object.assign(draft, patch); draw(); };

  function field(label, control, opts = {}) {
    return h('div', { class: 'pf-field' + (opts.flag ? ' flagged' : '') },
      h('label', { class: 'pf-label mono', for: opts.id || null }, label),
      h('div', { class: 'pf-control' }, control,
        opts.hint ? h('div', { class: 'pf-hint' }, opts.hint) : null,
        opts.error ? h('div', { class: 'pf-error', role: 'alert' }, opts.error) : null,
        opts.warn ? h('div', { class: 'pf-warn' }, opts.warn) : null,
        opts.flag ? h('div', { class: 'pf-warn' }, opts.flag) : null,
        opts.result));
  }

  function input(id, value, oninput, extra = {}) {
    const el = h('input', { id, class: 'pf-input' + (extra.cls ? ' ' + extra.cls : ''), value, spellcheck: 'false', autocomplete: 'off', ...extra.attrs });
    el.value = value;
    el.addEventListener('compositionstart', () => { composing = true; });
    el.addEventListener('compositionend', () => { composing = false; oninput(el.value); });
    el.addEventListener('input', (e) => { if (!e.isComposing) oninput(el.value); });
    return el;
  }

  function select(id, options, value, onchange, cls) {
    const el = h('select', { id, class: 'pf-select' + (cls ? ' ' + cls : '') }, options.map(([v, label]) => h('option', { value: v, selected: v === value }, label)));
    el.addEventListener('change', () => onchange(el.value));
    return el;
  }

  // draw redraws the form, keeping focus on the field being typed in.
  function draw() {
    if (composing) return;
    const active = document.activeElement;
    const focused = active && (root.contains(active) || foot.contains(active)) ? active.id : '';
    const caret = focused && active.selectionStart != null ? [active.selectionStart, active.selectionEnd] : null;
    const { errs, warns } = problems(draft, orig, names);
    const dirty = JSON.stringify(draft) !== initial;
    const blocking = Object.values(errs)[0] || '';
    const canSave = !saving && !blocking && (isNew || dirty);
    const sessions = orig ? deps.sessions().filter((s) => (orig.sessions || []).includes(s.id)) : [];
    const form = h('form', { class: 'pf-form', id: 'pf-form', novalidate: true, autocomplete: 'off' },
      identity(errs, warns), auth(errs), aliases(errs), egress(errs), confirmDelete ? deleteCard(sessions) : null);
    // Enter saves; the page never navigates (its CSP allows no form posts).
    form.addEventListener('submit', (e) => { e.preventDefault(); if (canSave) save(); });
    show(root, header(sessions, dirty), form);
    show(foot, footer(dirty, blocking, canSave));
    // root is gone when the user left before the profile loaded.
    if (!foot.isConnected && root.isConnected) main.appendChild(foot);
    if (focused) {
      const sel = '#' + CSS.escape(focused);
      const el = root.querySelector(sel) || foot.querySelector(sel);
      if (el) {
        el.focus();
        if (caret && el.setSelectionRange) { try { el.setSelectionRange(caret[0], caret[1]); } catch (_) { /* not a text field */ } }
      }
    }
  }

  function header(sessions, dirty) {
    const host = orig && orig.base_url_host;
    const used = sessions.length ? 'used by ' + sessions.length + ' running session' + (sessions.length === 1 ? '' : 's') : '';
    return h('div', { class: 'pf-head' },
      h('a', { class: 'back-link', href: '#/profiles' }, icon('back'), 'Profiles'),
      h('div', { class: 'pf-title-row' },
        h('div', { class: 'pf-title-text' },
          h('h1', { class: 'page-title' }, isNew ? 'New profile' : orig.name, orig && orig.is_default ? h('span', { class: 'default-pill' }, 'default') : null),
          orig ? h('p', { class: 'pf-meta mono' }, [orig.provider, host, used].filter(Boolean).join(' · ')) : null),
        h('div', { class: 'pf-test' },
          test && !test.running ? testChip(test) : null,
          test && !test.running ? h('span', { class: 'pf-test-text mono' }, test.text) : null,
          h('button', {
            type: 'button', class: 'btn', ...testButton(isNew, dirty, test && test.running),
            onclick: runTest,
          }, test && test.running ? icon('loader', 'spin') : icon('activity'), test && test.running ? 'Testing…' : 'Test connectivity'))));
  }

  async function runTest() {
    test = { running: true };
    draw();
    try {
      test = await deps.api.post('/profiles/' + encodeURIComponent(orig.name) + '/test', {});
    } catch (err) {
      test = null;
      deps.toast("Couldn't test: " + err.message, 'error');
    }
    draw();
  }

  function card(label, ...children) {
    return h('section', { class: 'pf-card' }, h('div', { class: 'pf-card-label' }, label), h('div', { class: 'pf-card-body' }, children));
  }

  function identity(errs, warns) {
    return card('Identity',
      isNew ? field('name *', input('pf-name', draft.name, (v) => set({ name: v }), { cls: errs.name ? 'invalid' : '', attrs: { placeholder: 'e.g. deepseek' } }), { id: 'pf-name', error: draft.name && errs.name }) : null,
      field('provider', input('pf-provider', draft.provider, (v) => set({ provider: v }), { attrs: { placeholder: 'optional group label' } }), { id: 'pf-provider' }),
      field('base_url *', input('pf-base', draft.base_url, (v) => set({ base_url: v }), { cls: errs.base_url && draft.base_url ? 'invalid' : warns.base_url || flagged === 'base_url' ? 'advisory' : '', attrs: { placeholder: 'https://api.example.com' } }),
        { id: 'pf-base', error: draft.base_url && errs.base_url, warn: warns.base_url, flag: flagged === 'base_url' && !warns.base_url ? 'Requests on this route went to the wrong path; check the base URL.' : '' }));
  }

  function auth(errs) {
    const on = draft.auth_mode !== 'passthrough';
    const stored = orig && orig.auth ? (draft.key_source === 'env_var' ? orig.auth.has_key_env : orig.auth.has_inline_key) : false;
    const placeholder = draft.key_source === 'env_var'
      ? (stored ? 'set — blank keeps the current variable' : 'env var name (e.g. DEEPSEEK_API_KEY)')
      : (stored ? 'stored — blank keeps it' : 'paste the key');
    const keyInput = input('pf-key', draft.key, (v) => set({ key: v }), {
      cls: (errs.key && dirtyKey() ? 'invalid' : flagged === 'key' ? 'advisory' : ''),
      attrs: { type: draft.key_source === 'inline' && !reveal ? 'password' : 'text', placeholder, autocomplete: 'new-password' },
    });
    return card('Auth',
      field('mode', select('pf-mode', AUTH_MODES, draft.auth_mode, (v) => set({ auth_mode: v })), { id: 'pf-mode', hint: authModeHint(draft.auth_mode) }),
      on ? field('key *', h('div', { class: 'pf-joined' },
        select('pf-keysrc', [['inline', 'inline'], ['env_var', 'env var']], draft.key_source, (v) => set({ key_source: v, key: '' }), 'joined-left'),
        keyInput,
        draft.key_source === 'inline' ? h('button', { type: 'button', class: 'pf-reveal', 'aria-label': reveal ? 'Hide the key' : 'Show the key', title: reveal ? 'Hide the key' : 'Show the key', onclick: () => { reveal = !reveal; draw(); } }, icon(reveal ? 'eye-off' : 'eye')) : null),
      { id: 'pf-key', error: dirtyKey() ? errs.key : '', flag: flagged === 'key' ? 'The upstream rejected the key this profile sends. Paste a new key or point to another variable.' : '' }) : null);
  }

  // dirtyKey reports whether the key's error should show yet: after the
  // user touched auth, or right away for a new profile being saved.
  function dirtyKey() {
    const init = JSON.parse(initial);
    return isNew ? draft.key !== '' || draft.auth_mode !== init.auth_mode : draft.auth_mode !== init.auth_mode || draft.key_source !== init.key_source;
  }

  function aliases(errs) {
    const targets = [...new Set(draft.aliases.map((r) => r.to.trim()).filter((t) => t && t !== '*'))].sort();
    const choice = aliasDefaultChoices(targets, draft.other, isNew, draft.otherPicked);
    if (isNew && !draft.otherPicked) draft.other = choice.value;
    const listId = 'pf-claude-models';
    const suggestions = [...new Set([...unaliased, ...draft.aliases.map((r) => r.from), 'opus', 'sonnet', 'haiku'].filter(Boolean))];
    return card('Model aliases',
      h('datalist', { id: listId }, suggestions.map((m) => h('option', { value: m }))),
      draft.aliases.length ? h('div', { class: 'alias-head mono' }, h('span', null, 'claude sends'), h('span'), h('span', null, 'forward to'), h('span')) : h('p', { class: 'pf-hint' }, 'No per-model aliases.'),
      draft.aliases.map((r, i) => h('div', { class: 'alias-edit-row' },
        input('pf-alias-from-' + i, r.from, (v) => { draft.aliases[i].from = v; draw(); }, { attrs: { list: listId, placeholder: 'claude-sonnet-5-5' } }),
        h('span', { class: 'alias-arrow', 'aria-hidden': 'true' }, '→'),
        input('pf-alias-to-' + i, r.to, (v) => { draft.aliases[i].to = v; draw(); }, { cls: 'alias-target', attrs: { placeholder: 'upstream model' } }),
        h('button', { type: 'button', class: 'alias-remove', 'aria-label': 'Remove alias ' + (r.from || i + 1), onclick: () => { draft.aliases.splice(i, 1); draw(); } }, icon('x')))),
      errs.aliases ? h('div', { class: 'pf-error', role: 'alert' }, errs.aliases) : null,
      h('button', { type: 'button', class: 'alias-add', onclick: () => { draft.aliases.push({ from: '', to: '' }); draw(); const el = root.querySelector('#pf-alias-from-' + (draft.aliases.length - 1)); if (el) el.focus(); } }, '+ add alias'),
      field('other Claude models\u00a0→', h('div', { class: 'pf-joined' },
        select('pf-other', choice.options, choice.value, (v) => set({ other: v, otherPicked: true })),
        draft.other === '__custom' ? input('pf-other-custom', draft.otherCustom, (v) => set({ otherCustom: v }), { attrs: { placeholder: 'upstream model' } }) : null),
      { id: 'pf-other', error: errs.other, hint: 'Where Claude models with no alias of their own go.' }));
  }

  function egress(errs) {
    const proxy = isProxyMode(draft.egress_mode);
    const result = egressTest && egressTest.result;
    return card('Egress',
      field('via', h('div', { class: 'pf-joined' },
        select('pf-egress', EGRESS_MODES.map((m) => [m, m]), draft.egress_mode, (v) => set({ egress_mode: v }), 'joined-left'),
        input('pf-egress-url', proxy ? draft.egress_url : '', (v) => set({ egress_url: v }), { cls: errs.egress_url && draft.egress_url ? 'invalid' : '', attrs: { placeholder: egressPlaceholder(draft.egress_mode), disabled: !proxy } }),
        h('button', { type: 'button', class: 'btn btn-sm', disabled: egressTest && egressTest.running, onclick: runEgressTest }, egressTest && egressTest.running ? icon('loader', 'spin') : icon('zap'), 'Test egress')),
      {
        id: 'pf-egress', hint: egressModeHint(draft.egress_mode), error: draft.egress_url ? errs.egress_url : '',
        result: result ? h('div', { class: 'pf-egress-result mono' + (result.status === 'OK' ? ' ok' : ' fail') }, egressText(result)) : null,
      }));
  }

  function egressText(r) {
    if (r.status !== 'OK') return '✗ ' + [r.status, r.err].filter(Boolean).join(' · ');
    return '✓ ' + [r.public_ip, [r.city, r.country].filter(Boolean).join(', '), r.org, (r.latency_ms || 0) + ' ms', r.egress_via ? 'via ' + r.egress_via : ''].filter(Boolean).join(' · ');
  }

  async function runEgressTest() {
    egressTest = { running: true };
    draw();
    const body = { egress: { mode: draft.egress_mode, url: isProxyMode(draft.egress_mode) ? draft.egress_url.trim() : '' } };
    try {
      const path = isNew ? '/egress-test' : '/profiles/' + encodeURIComponent(orig.name) + '/test-egress';
      egressTest = { result: await deps.api.post(path, body) };
    } catch (err) {
      egressTest = { result: { status: 'ERROR', err: err.message } };
    }
    draw();
  }

  function deleteCard(sessions) {
    const isDefault = orig.is_default;
    const lines = [];
    if (sessions.length) {
      lines.push(sessions.length + ' running session' + (sessions.length === 1 ? ' uses' : 's use') + ' it: ' + sessions.map((s) => s.display.title + ' (' + since(s.started_at) + ')').join(', ') + '. Deleting the profile doesn\u2019t stop ' + (sessions.length === 1 ? 'that session.' : 'them.'));
    }
    if (isDefault) lines.push(orig.name + ' is the default profile; ' + fallbackDefault(names, orig.name) + ' becomes the default.');
    if (!lines.length) lines.push('Nothing uses it. This can\u2019t be undone.');
    return h('div', { class: 'pf-delete' + (sessions.length ? ' danger' : ''), role: 'alertdialog', 'aria-labelledby': 'pf-del-title' },
      h('p', { class: 'pf-delete-title', id: 'pf-del-title' }, 'Delete profile ' + orig.name + '?'),
      lines.map((l) => h('p', null, l)),
      h('div', { class: 'pf-delete-acts' },
        h('button', { type: 'button', id: 'pf-del-keep', class: 'btn btn-sm', onclick: () => setDelete(false) }, 'Keep profile'),
        h('button', { type: 'button', class: 'btn btn-sm btn-danger', onclick: remove }, 'Delete profile')));
  }

  // setDelete shows or hides the delete confirmation. Opening it moves focus
  // to Keep profile, which also scrolls the card into view above the footer;
  // closing it hands focus back to the footer's Delete profile.
  function setDelete(on) {
    confirmDelete = on;
    draw();
    const el = on ? root.querySelector('#pf-del-keep') : foot.querySelector('#pf-del');
    if (el) el.focus();
  }

  async function remove() {
    try {
      const out = await deps.api.del('/profiles/' + encodeURIComponent(orig.name), { confirm_name: orig.name });
      deps.toast('Deleted profile ' + orig.name + '.' + (orig.is_default ? ' ' + out.default + ' is now the default.' : ''));
      deps.onSaved();
      deps.go('#/profiles');
    } catch (err) {
      deps.toast("Couldn't delete: " + err.message, 'error');
    }
  }

  function footer(dirty, blocking, canSave) {
    let status = '';
    if (blocking && (dirty || isNew)) status = h('span', { class: 'pf-status warn' }, 'Can\u2019t save yet: ' + blocking);
    else if (dirty) status = h('span', { class: 'pf-status' }, 'Unsaved changes');
    // Ids let a redraw keep focus on the control that had it.
    return h('div', { class: 'pf-footer-row' },
      !isNew ? h('button', { type: 'button', id: 'pf-del', class: 'btn btn-danger-outline', onclick: () => setDelete(true) }, icon('trash'), 'Delete profile') : null,
      h('span', { class: 'spacer' }),
      status,
      orig && orig.is_default ? h('span', { class: 'pf-default-static' }, 'Default profile')
        : h('label', { class: 'pf-check' }, (() => { const c = h('input', { type: 'checkbox', id: 'pf-set-default', checked: draft.set_default }); c.checked = draft.set_default; c.addEventListener('change', () => set({ set_default: c.checked })); return c; })(), 'Set as default'),
      dirty && !isNew ? h('button', { type: 'button', id: 'pf-discard', class: 'btn', onclick: () => { draft = JSON.parse(initial); draw(); } }, 'Discard changes') : null,
      isNew ? h('button', { type: 'button', class: 'btn', onclick: () => deps.go('#/profiles') }, 'Cancel') : null,
      // Outside the form, the form attribute keeps this its submit button
      // (and Enter in a field its default).
      h('button', { type: 'submit', id: 'pf-save', form: 'pf-form', class: 'btn btn-primary', disabled: !canSave }, saving ? icon('loader', 'spin') : null, isNew ? 'Create profile' : 'Save'));
  }

  async function save() {
    saving = true;
    draw();
    try {
      if (isNew) {
        const out = await deps.api.post('/profiles', addPayload(draft));
        deps.toast('Created profile ' + out.profile.name + '.');
        deps.onSaved();
        deps.go('#/profiles/' + encodeURIComponent(out.profile.name));
        return;
      }
      const out = await deps.api.patch('/profiles/' + encodeURIComponent(orig.name), editPayload(orig, draft));
      orig = { ...orig, ...out.profile, sessions: out.profile.sessions, last_test: undefined };
      test = null;
      draft = draftFrom(orig);
      initial = JSON.stringify(draft);
      flagged = '';
      const applied = out.applied || [];
      const failed = applied.filter((a) => a.result !== 'switched');
      if (!applied.length) deps.toast('Saved profile ' + orig.name + '.');
      else if (!failed.length) deps.toast('Saved profile ' + orig.name + ' and applied it to ' + applied.length + ' running session' + (applied.length === 1 ? '.' : 's.'));
      else deps.toast('Saved profile ' + orig.name + '. ' + failed.length + ' session' + (failed.length === 1 ? '' : 's') + ' did not pick it up: ' + (failed[0].message || failed[0].result), 'warning');
      deps.onSaved();
    } catch (err) {
      deps.toast("Couldn't save: " + err.message, 'error');
    }
    saving = false;
    draw();
  }

  return { unmount() {} };
}
