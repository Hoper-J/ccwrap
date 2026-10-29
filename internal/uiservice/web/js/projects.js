// Projects: every folder Claude Code has run in, most recent first, with a
// profile picker and the launch actions. Launches open the user's terminal,
// so their progress (opening, timed out, rejected) shows in the row.

import { h, icon, show } from './dom.js';
import { ago, since } from './fmt.js';
import { launchProfile } from './views/empty.js';

// How long a launch may take before the row says no session appeared.
export const LAUNCH_TIMEOUT_MS = 30000;

// matches reports whether a project matches a search: name, path, branch or
// conversation title, case-insensitive.
export function matches(p, query) {
  const q = query.trim().toLowerCase();
  if (!q) return true;
  return [p.name, p.path, p.display_path, p.git_branch, p.last_title, p.last_prompt].some((v) => v && String(v).toLowerCase().includes(q));
}

// running is a project's running sessions, from the live session list.
export function running(p, sessions) {
  return sessions.filter((s) => s.state !== 'ended' && s.project_dir === p.path);
}

// runningText is the row's live status: "running · deepseek · 12m", or
// "2 sessions running".
export function runningText(list, now) {
  if (list.length === 1) return 'running · ' + (list[0].profile || 'no profile') + ' · ' + since(list[0].started_at, now);
  return list.length + ' sessions running';
}

// worstHealth is the worst health among sessions.
export function worstHealth(list) {
  const rank = { ok: 0, warn: 1, error: 2 };
  let w = 'ok';
  for (const s of list) if ((rank[s.health] || 0) > rank[w]) w = s.health;
  return w;
}

// settleLaunches ends the launches newly listed sessions answer: a session
// started in the launch's folder after it began (two seconds' slack for
// clocks). It returns the launches left and the sessions that started.
export function settleLaunches(launches, sessions) {
  const left = { ...launches };
  const started = [];
  for (const s of sessions) {
    const l = left[s.project_dir];
    if (!l || l.state === 'rejected' || s.state === 'ended' || Date.parse(s.started_at) < l.since - 2000) continue;
    delete left[s.project_dir];
    started.push(s);
  }
  return { launches: left, started };
}

// conversationLabel names the folder's latest conversation: its title, else
// its last prompt. none marks the fallback text, which shows dimmed.
export function conversationLabel(p) {
  const text = p.last_title || p.last_prompt;
  if (text) return { text, none: false };
  return { text: p.has_conversation ? 'Untitled conversation' : 'No conversations yet', none: true };
}

// continueLaunch is how an ended session continues: it resumes its own
// conversation when it reported one, and otherwise the folder's latest.
export function continueLaunch(conversation) {
  return conversation ? { mode: 'resume', session: conversation } : { mode: 'continue' };
}

// conversationItem is one entry of the Resume list: the conversation's
// title (else its last prompt), and its branch and age — or the running
// session it is open in, which opens instead of a second Claude Code on
// the same conversation.
export function conversationItem(c, sessions, now) {
  const text = c.title || c.last_prompt || '';
  const open = (c.open_in || [])[0] || '';
  const s = open ? sessions.find((x) => x.id === open) : null;
  let meta;
  if (s) meta = 'open · ' + (s.profile || 'no profile') + ' · ' + since(s.started_at, now);
  else if (open) meta = 'open in another session';
  else meta = [c.git_branch, ago(c.last_active_at, now)].filter(Boolean).join(' · ');
  return { text: text || 'Untitled conversation', none: !text, meta, open };
}

// mountProjects shows the Projects page in main. deps: {api, toast, go,
// openSession(id), launch(project, profile, mode, session), store}. It
// returns {update(state), unmount()}.
export function mountProjects(main, deps) {
  let st = null;
  let query = '';
  let termOpen = false;
  let terminals = null; // GET /settings/terminal
  const picks = {};     // path → chosen profile ('' = no profile)
  let menuFor = '';     // path whose row menu is open
  let convFor = '';     // path whose Resume list is open
  let convs = null;     // its conversations; null while they load
  let convErr = '';

  const title = h('h1', { class: 'page-title' }, 'Projects');
  const sub = h('p', { class: 'page-sub' });
  const termSlot = h('div', { class: 'term-wrap' });
  const search = h('input', { type: 'search', class: 'search-input', 'aria-label': 'Search projects', spellcheck: 'false', autocomplete: 'off' });
  search.addEventListener('input', () => { query = search.value; drawList(); });
  const listSlot = h('div', { class: 'proj-slot' });
  show(main, h('div', { class: 'main-inner' }, h('section', { class: 'projects', 'aria-labelledby': 'proj-title' },
    h('div', { class: 'page-head' }, h('div', { class: 'page-head-text' }, title, sub), termSlot),
    h('div', { class: 'search' }, icon('search'), search, h('kbd', null, '/')),
    listSlot)));
  title.id = 'proj-title';

  const onKey = (e) => {
    if (e.key === '/' && !['INPUT', 'TEXTAREA', 'SELECT'].includes(document.activeElement.tagName)) {
      e.preventDefault();
      search.focus();
    }
    if (e.key === 'Escape' && convFor) { closeResume(true); return; }
    if (e.key === 'Escape' && (termOpen || menuFor)) { termOpen = false; menuFor = ''; drawTerm(); drawList(); }
  };
  const onDown = (e) => {
    const inside = (sel) => e.target.closest && e.target.closest(sel);
    if (termOpen && !termSlot.contains(e.target)) { termOpen = false; drawTerm(); }
    if (menuFor && !inside('.row-menu-wrap')) { menuFor = ''; drawList(); }
    if (convFor && !inside('.conv-menu') && !inside('[data-conv-trigger]')) closeResume(false);
  };
  document.addEventListener('keydown', onKey);
  document.addEventListener('mousedown', onDown);

  deps.api.get('/settings/terminal').then((t) => { terminals = t; drawTerm(); drawList(); }).catch(() => {});

  function termName() {
    if (!terminals || !terminals.current) return '';
    const t = (terminals.installed || []).find((x) => x.id === terminals.current);
    return t ? t.name : terminals.current;
  }

  function drawTerm() {
    if (!terminals) { show(termSlot); return; }
    const installed = terminals.installed || [];
    const trigger = h('button', {
      type: 'button', class: 'term-btn', 'aria-haspopup': 'listbox', 'aria-expanded': String(termOpen),
      onclick: () => { termOpen = !termOpen; drawTerm(); if (termOpen) focusOption(); },
    }, h('span', { class: 'term-label' }, 'Opens in'), icon('terminal'), h('span', { class: 'term-name' }, termName() || 'no terminal'), h('span', { class: 'caret', 'aria-hidden': 'true' }, '▾'));
    const list = termOpen ? h('div', { class: 'term-pop', role: 'listbox', 'aria-label': 'Terminal for new sessions', tabindex: '-1' },
      h('div', { class: 'term-pop-head' }, 'installed terminals'),
      installed.length ? installed.map((t) => h('div', {
        role: 'option', class: 'term-opt', tabindex: '-1', 'aria-selected': String(t.id === terminals.current), 'data-id': t.id,
        onclick: () => pick(t),
        onkeydown: (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); pick(t); } },
      },
      h('span', { class: 'term-radio', 'aria-hidden': 'true' }),
      h('span', null, t.name),
      h('span', { class: 'term-tags' },
        terminals.claude_choice === t.id ? h('span', { class: 'tag' }, 'Claude Code uses this') : null,
        t.verified ? null : h('span', { class: 'tag warn' }, 'untested')))) : h('p', { class: 'pop-note' }, 'No supported terminal app is installed (Terminal, iTerm, Ghostty).'),
      h('div', { class: 'term-pop-foot' }, 'Only installed terminals are listed. ccwrap opens a new window there and runs ccwrap in the project folder.')) : null;
    if (list) {
      list.addEventListener('keydown', (e) => {
        if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(e.key)) return;
        e.preventDefault();
        const opts = Array.from(list.querySelectorAll('[role="option"]'));
        const i = opts.indexOf(document.activeElement);
        const j = e.key === 'ArrowDown' ? Math.min(opts.length - 1, i + 1) : e.key === 'ArrowUp' ? Math.max(0, i - 1) : e.key === 'Home' ? 0 : opts.length - 1;
        if (opts[j]) opts[j].focus();
      });
    }
    show(termSlot, trigger, list);
  }

  function focusOption() {
    const opt = termSlot.querySelector('[role="option"][aria-selected="true"]') || termSlot.querySelector('[role="option"]');
    if (opt) opt.focus();
  }

  async function pick(t) {
    termOpen = false;
    try {
      terminals = await deps.api.put('/settings/terminal', { terminal: t.id });
      deps.toast('New sessions open in ' + t.name + '.');
    } catch (err) {
      deps.toast("Couldn't change the terminal: " + err.message, 'error');
    }
    drawTerm();
    const b = termSlot.querySelector('.term-btn');
    if (b) b.focus();
  }

  function profileFor(p) {
    if (Object.prototype.hasOwnProperty.call(picks, p.path)) return picks[p.path];
    return launchProfile(p, st.profiles.names, st.profiles.default);
  }

  function drawList() {
    if (!st) return;
    const all = st.projects;
    sub.textContent = all.length ? all.length + ' folders Claude Code has run in, most recent first.' : '';
    search.placeholder = 'Search ' + all.length + ' projects by name, path, branch or title';
    if (!st.projectsLoaded) { show(listSlot); return; }
    if (!all.length) {
      show(listSlot, h('div', { class: 'proj-empty' },
        h('span', { class: 'proj-empty-icon' }, icon('folder')),
        h('h2', null, 'No projects found'),
        h('p', null, 'Projects come from Claude Code’s own history: every folder you have run Claude Code in shows up here, most recent first. Run ', h('code', null, 'ccwrap'), ' or ', h('code', null, 'claude'), ' in a folder once and it appears.'),
        h('p', { class: 'mono proj-empty-src' }, 'reads ~/.claude.json and ~/.claude/projects/')));
      return;
    }
    const rows = all.filter((p) => matches(p, query));
    if (!rows.length) {
      show(listSlot, h('div', { class: 'proj-nomatch' }, 'No projects match “' + query + '”. ',
        h('button', { type: 'button', class: 'link-btn', onclick: () => { query = ''; search.value = ''; drawList(); search.focus(); } }, 'Clear search')));
      return;
    }
    const now = Date.now();
    const keep = focusKey();
    show(listSlot, h('div', { class: 'proj-list', role: 'list', 'aria-label': 'Projects' },
      h('div', { class: 'proj-head', 'aria-hidden': 'true' }, h('span', null, 'Project'), h('span', { class: 'proj-wide' }, 'Last conversation'), h('span', null, 'Profile'), h('span')),
      rows.map((p) => rowEl(p, now))));
    refocus(keep);
  }

  // The list redraws as sessions and times change; focusKey names the
  // control that had focus (its row's folder and its data-focus), and
  // refocus gives focus back to the control that replaces it.
  function focusKey() {
    const a = document.activeElement;
    const row = a && listSlot.contains(a) && a.dataset.focus ? a.closest('[data-path]') : null;
    return row ? { path: row.dataset.path, key: a.dataset.focus } : null;
  }

  function refocus(k) {
    const row = k && listSlot.querySelector('[data-path="' + CSS.escape(k.path) + '"]');
    const el = row && row.querySelector('[data-focus="' + CSS.escape(k.key) + '"]');
    if (!el) return false;
    el.focus({ preventScroll: true });
    return true;
  }

  function rowEl(p, now) {
    const live = running(p, st.sessions);
    const launch = st.launches[p.path];
    const opening = launch && launch.state === 'opening';
    const term = (launch && launch.terminal) || termName() || 'your terminal';
    const label = conversationLabel(p);
    const conv = h('span', { class: label.none ? 'proj-title none' : 'proj-title' }, label.text);
    const meta = h('span', { class: 'proj-meta' }, p.git_branch ? [icon('git-branch'), h('span', { class: 'mono' }, p.git_branch), h('span', null, '·')] : null, h('span', { class: 'mono proj-ago' }, ago(p.last_active_at, now)));
    const profile = profileFor(p);
    const names = st.profiles.names;
    // "no profile" is short enough for the column; what it means (the
    // launch environment) is in the hover text.
    const select = h('select', { class: 'proj-select', 'aria-label': 'Profile for ' + p.name, title: 'Profile for new sessions here; no profile uses the launch environment', disabled: !!opening, onchange: (e) => { picks[p.path] = e.target.value; } },
      names.map((n) => h('option', { value: n, selected: n === profile }, n)),
      h('option', { value: '', selected: profile === '' }, 'no profile'));
    let actions;
    if (opening) {
      actions = h('span', { class: 'status-line', role: 'status' }, icon('loader', 'spin'), 'Opening in ' + term + '…');
    } else if (live.length) {
      const newest = live.slice().sort((a, b) => Date.parse(b.started_at) - Date.parse(a.started_at))[0];
      // Continue would most likely land in the running conversation;
      // the Resume list marks it and opens its session instead.
      actions = [
        h('button', { type: 'button', class: 'btn btn-sm btn-primary', 'data-focus': 'open', onclick: () => deps.openSession(newest.id) }, 'Open session'),
        h('div', { class: 'row-menu-wrap' },
          h('button', { type: 'button', class: 'icon-btn icon-btn-sm', 'data-focus': 'more', 'aria-haspopup': 'menu', 'aria-expanded': String(menuFor === p.path), 'aria-label': 'More for ' + p.name, title: 'More for ' + p.name, onclick: () => { menuFor = menuFor === p.path ? '' : p.path; convFor = ''; drawList(); revealMenu(); } }, icon('more')),
          menuFor === p.path ? h('div', { class: 'menu', role: 'menu' },
            live.map((s) => h('button', { type: 'button', class: 'menu-item', role: 'menuitem', 'data-focus': 'open:' + s.id, onclick: () => { menuFor = ''; deps.openSession(s.id); } },
              h('span', { class: 'dot', 'data-health': s.health || 'ok' }), 'Open ' + (s.profile || 'no profile') + ' · ' + since(s.started_at, now))),
            h('button', { type: 'button', class: 'menu-item', role: 'menuitem', 'data-focus': 'another', onclick: () => { menuFor = ''; deps.launch(p, profileFor(p), 'new'); } }, icon('plus'), 'Start another session'),
            h('button', { type: 'button', class: 'menu-item', role: 'menuitem', 'data-focus': 'resume-more', 'data-conv-trigger': true, 'aria-haspopup': 'menu', onclick: () => openResume(p) }, icon('clock'), 'Resume a conversation…')) : null),
      ];
    } else {
      const canContinue = !!p.has_conversation;
      actions = [
        h('button', {
          type: 'button', class: 'btn btn-sm', 'data-focus': 'continue', 'aria-disabled': canContinue ? null : 'true',
          title: canContinue ? 'Continue the latest conversation here' : 'No conversation to resume yet',
          onclick: () => { if (canContinue) deps.launch(p, profileFor(p), 'continue'); },
        }, 'Continue'),
        h('button', {
          type: 'button', class: 'btn btn-sm', 'data-focus': 'resume', 'data-conv-trigger': true, 'aria-haspopup': 'menu', 'aria-expanded': String(convFor === p.path),
          'aria-disabled': canContinue ? null : 'true',
          title: canContinue ? 'Pick a conversation here to resume' : 'No conversation to resume yet',
          onclick: () => { if (!canContinue) return; if (convFor === p.path) closeResume(false); else openResume(p); },
        }, 'Resume…'),
        h('button', { type: 'button', class: 'btn btn-sm', 'data-focus': 'new', title: 'Start a new conversation here', onclick: () => deps.launch(p, profileFor(p), 'new') }, 'New'),
      ];
    }
    if (convFor === p.path && !opening) actions = [actions, resumeList(p, now)];
    let note = null;
    if (launch && launch.state === 'timeout') {
      note = noteEl('warn', 'alert', 'No session appeared after 30s — check the ' + term + ' window.', [
        ['Try again', () => deps.launch(p, profileFor(p), launch.mode, launch.session)], ['Dismiss', () => deps.dismiss(p.path)]]);
    } else if (launch && launch.state === 'rejected') {
      note = noteEl('danger', 'alert', launch.message, [['Dismiss', () => deps.dismiss(p.path)]]);
    } else if (!p.trusted && !live.length) {
      note = noteEl('', null, 'Not trusted yet — Claude Code will ask you to trust this folder.', []);
    }
    const health = worstHealth(live);
    const status = live.length ? runningText(live, now) : '';
    return h('div', { class: 'proj-row', role: 'listitem', 'data-path': p.path },
      h('div', { class: 'proj-grid' },
        h('div', { class: 'proj-main' },
          h('div', { class: 'proj-name-line' },
            h('span', { class: 'proj-name' }, p.name),
            status ? h('span', { class: 'proj-running', 'data-health': health, title: status }, h('span', null, status)) : null),
          h('div', { class: 'proj-path mono' }, p.display_path || p.path),
          h('div', { class: 'proj-conv-narrow' }, conv.cloneNode(true), h('span', null, '·'), meta.cloneNode(true))),
        h('div', { class: 'proj-conv proj-wide' }, conv, meta),
        h('span', { class: 'select-wrap' }, select, h('span', { class: 'caret', 'aria-hidden': 'true' }, '▾')),
        h('div', { class: 'proj-actions' }, actions)),
      note);
  }

  // revealMenu scrolls an opened row menu into view: on the last rows it
  // opens below the fold.
  function revealMenu() {
    const menu = menuFor ? listSlot.querySelector('.row-menu-wrap .menu') : null;
    if (menu) menu.scrollIntoView({ block: 'nearest' });
  }

  // openResume opens the Resume list under p's actions and loads the
  // folder's conversations; an answer for a list closed meanwhile is dropped.
  async function openResume(p) {
    menuFor = '';
    convFor = p.path;
    convs = null;
    convErr = '';
    drawList();
    try {
      const out = await deps.api.get('/projects/conversations?path=' + encodeURIComponent(p.path));
      if (convFor !== p.path) return;
      convs = out.conversations || [];
    } catch (err) {
      if (convFor !== p.path) return;
      convErr = err.message;
    }
    drawList();
    const menu = listSlot.querySelector('.conv-menu');
    if (!menu) return;
    menu.scrollIntoView({ block: 'nearest' });
    const first = menu.querySelector('[role="menuitem"]');
    if (first) first.focus();
  }

  // closeResume closes the Resume list; from the keyboard, focus goes back
  // to what opened it.
  function closeResume(focusTrigger) {
    const path = convFor;
    convFor = '';
    drawList();
    if (focusTrigger) refocus({ path, key: 'resume' }) || refocus({ path, key: 'more' });
  }

  // resumeList is the folder's conversations, newest first. One a running
  // session is in opens that session; another resumes in a new terminal;
  // the last item opens Claude Code's own picker, which has them all.
  function resumeList(p, now) {
    const choose = (fn) => () => { convFor = ''; fn(); drawList(); };
    let items;
    if (convErr) items = h('p', { class: 'conv-note', role: 'none' }, "Couldn't list the conversations: " + convErr);
    else if (convs === null) items = h('p', { class: 'conv-note', role: 'none' }, 'Loading conversations…');
    else if (!convs.length) items = h('p', { class: 'conv-note', role: 'none' }, 'No conversations to resume here yet.');
    else {
      items = convs.map((c) => {
        const it = conversationItem(c, st.sessions, now);
        return h('button', {
          type: 'button', class: 'menu-item conv-item', role: 'menuitem', 'data-focus': 'conv:' + c.id, title: it.text,
          onclick: choose(() => (it.open ? deps.openSession(it.open) : deps.launch(p, profileFor(p), 'resume', c.id))),
        }, h('span', { class: it.none ? 'conv-text none' : 'conv-text' }, it.text), h('span', { class: it.open ? 'conv-meta open' : 'conv-meta' }, it.meta));
      });
    }
    const menu = h('div', { class: 'menu conv-menu', role: 'menu', 'aria-label': 'Conversations in ' + p.name },
      h('div', { class: 'conv-head', role: 'none' }, 'Resume in ' + (termName() || 'your terminal')),
      items,
      h('div', { class: 'menu-sep', role: 'separator' }),
      h('button', {
        type: 'button', class: 'menu-item', role: 'menuitem', 'data-focus': 'conv-more', title: 'Pick from every conversation here in Claude Code',
        onclick: choose(() => deps.launch(p, profileFor(p), 'resume')),
      }, icon('terminal'), 'More in Claude Code…'));
    menu.addEventListener('keydown', (e) => {
      if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(e.key)) return;
      e.preventDefault();
      const opts = Array.from(menu.querySelectorAll('[role="menuitem"]'));
      const i = opts.indexOf(document.activeElement);
      const j = e.key === 'ArrowDown' ? Math.min(opts.length - 1, i + 1) : e.key === 'ArrowUp' ? Math.max(0, i - 1) : e.key === 'Home' ? 0 : opts.length - 1;
      if (opts[j]) opts[j].focus();
    });
    return menu;
  }

  function noteEl(kind, role, text, acts) {
    return h('div', { class: 'note-strip', 'data-kind': kind || null, role: role || null },
      icon(kind ? 'alert-triangle' : 'info'), h('span', { class: 'note-text' }, text),
      acts.length ? h('span', { class: 'note-acts' }, acts.map(([label, fn]) => h('button', { type: 'button', class: 'btn btn-xs', 'data-focus': 'note:' + label, onclick: fn }, label))) : null);
  }

  return {
    update(next) {
      st = next;
      // Leave a row's open profile list alone while the user is in it.
      if (document.activeElement && document.activeElement.classList.contains('proj-select') && listSlot.contains(document.activeElement)) return;
      drawList();
    },
    unmount() {
      document.removeEventListener('keydown', onKey);
      document.removeEventListener('mousedown', onDown);
    },
  };
}
