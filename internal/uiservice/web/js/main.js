// ccwrap ui's page: boot, routing, the shell, and the live session list.

import { bootstrap, createApi, connectEvents } from './api.js';
import { createStore, mergeSessions } from './store.js';
import { parseRoute, sessionHash, landingSession } from './router.js';
import { h, show, copyText } from './dom.js';
import { renderAppbar } from './shell/appbar.js';
import { renderSidebar, sessionItems, initSidebar } from './shell/sidebar.js';
import { renderBanner } from './shell/banner.js';
import { createToasts } from './shell/toast.js';
import { renderEmpty, launchProfile } from './views/empty.js';
import { mountSession } from './session/view.js';
import { mountProjects, settleLaunches, LAUNCH_TIMEOUT_MS } from './projects.js';

const boot = bootstrap(document.body.dataset);
const api = createApi(boot.token);
const toast = createToasts(document.getElementById('toasts'));
const app = document.getElementById('app');
const shell = document.getElementById('shell');
const sidebar = document.getElementById('sidebar');
const main = document.getElementById('main');
const bannerSlot = document.getElementById('banner');
const narrowQuery = window.matchMedia('(max-width: 1023px)');
const COLLAPSED_KEY = 'ccwrap.ui.sidebarCollapsed';

const store = createStore({
  conn: 'connecting',
  lastContact: null,
  everConnected: false,
  route: parseRoute(location.hash),
  serverSessions: [],
  sessions: [],
  loaded: false,
  projects: [],
  profiles: { names: [], default: '' },
  collapsed: localStorage.getItem(COLLAPSED_KEY) === '1',
  narrow: narrowQuery.matches,
  drawer: false,
  projectsLoaded: false,
  // launches: project path → {state: opening | timeout | rejected, since,
  // mode, terminal, message}.
  launches: {},
});

// currentSessionId is the session the session route shows: the one named,
// else the most recently started running one.
function currentSessionId(st) {
  if (st.route.view !== 'session') return '';
  if (st.route.id) return st.route.id;
  const s = landingSession(st.sessions);
  return s ? s.id : '';
}

function setSessions(server) {
  const st = store.get();
  store.set({ serverSessions: server, sessions: mergeSessions(server, st.sessions, currentSessionId(st)), loaded: true });
}

// patchSession folds a session's live update into its list entry: health
// and badge change with session.updated, not sessions.changed.
function patchSession(id, sess) {
  if (!sess) return;
  const st = store.get();
  const fold = (list) => list.map((s) => {
    if (s.id !== id) return s;
    const ended = sess.state === 'ended';
    return {
      ...s,
      state: sess.state || s.state,
      health: sess.session_health || s.health,
      ended_at: sess.ended_at || s.ended_at,
      profile: sess.active_profile_name ?? s.profile,
      display: { ...s.display, badge: ended ? '' : ((sess.display && sess.display.badge) || '') },
    };
  });
  store.set({ serverSessions: fold(st.serverSessions), sessions: fold(st.sessions) });
}

// ---- Shell ----------------------------------------------------------------

const appbar = renderAppbar(document.getElementById('appbar'), {
  onToggle() {
    const st = store.get();
    if (st.narrow) {
      store.set({ drawer: !st.drawer });
    } else {
      localStorage.setItem(COLLAPSED_KEY, st.collapsed ? '0' : '1');
      store.set({ collapsed: !st.collapsed });
    }
  },
  menu: [
    { label: 'Sessions JSON', icon: 'external-link', href: '/api/v1/sessions' },
    { label: 'Projects JSON', icon: 'external-link', href: '/api/v1/projects' },
    { label: 'Copy app address', icon: 'copy', onSelect: () => toast(copyText(location.origin + '/') ? 'App address copied.' : "Couldn't copy the address.") },
  ],
});
initSidebar(sidebar);
narrowQuery.addEventListener('change', (e) => store.set({ narrow: e.matches, drawer: false }));
window.addEventListener('hashchange', () => store.set({ route: parseRoute(location.hash), drawer: false }));
document.addEventListener('keydown', (e) => { if (e.key === 'Escape' && store.get().drawer) store.set({ drawer: false }); });

function go(hash) {
  if (location.hash === hash) store.set({ route: parseRoute(hash), drawer: false });
  else location.hash = hash;
}

let scrim = null;
function renderShell(st) {
  const rail = st.narrow || st.collapsed;
  app.dataset.layout = rail ? 'rail' : 'full';
  app.toggleAttribute('data-drawer', st.narrow && st.drawer);
  app.dataset.conn = st.conn;
  shell.inert = st.conn === 'disconnected';
  appbar.conn(st.conn);
  if (st.narrow) appbar.toggle(st.drawer ? 'Close navigation' : 'Open navigation', st.drawer);
  else appbar.toggle(st.collapsed ? 'Expand sidebar' : 'Collapse sidebar', !st.collapsed);
  if (st.narrow && st.drawer) {
    if (!scrim) {
      scrim = h('button', { type: 'button', class: 'scrim', 'aria-label': 'Close navigation', tabindex: '-1', onclick: () => store.set({ drawer: false }) });
      shell.appendChild(scrim);
    }
  } else if (scrim) {
    scrim.remove();
    scrim = null;
  }
  renderSidebar(sidebar, {
    rail: rail && !(st.narrow && st.drawer),
    loading: !st.loaded,
    items: sessionItems(st.sessions, currentSessionId(st), Date.now()),
    route: st.route,
    currentId: currentSessionId(st),
    counts: { projects: st.projects.length || null, profiles: st.profiles.names.length || null },
    address: boot.address,
    onSession: (id) => go(sessionHash(id)),
    onNav: (view) => go('#/' + view),
  });
  renderBanner(bannerSlot, {
    shown: st.conn === 'disconnected',
    lastContact: st.lastContact,
    onReconnect: () => events.reconnect(),
  });
}

// ---- Views ----------------------------------------------------------------

// mounted is the live view in main: the session view and the Projects page
// keep themselves current, so they are mounted once, not redrawn.
let mounted = null;
// deny is the credential headers the inspector shows masked.
const deny = new Set(boot.deny);

function unmount() {
  if (mounted && mounted.handle) mounted.handle.unmount();
  mounted = null;
}

function mount(kind, id, make) {
  if (mounted && mounted.kind === kind && mounted.id === id) return mounted.handle;
  unmount();
  main.scrollTop = 0;
  mounted = { kind, id, handle: make() };
  return mounted.handle;
}

function renderMain(st) {
  const r = st.route;
  if (r.view === 'session') {
    const id = currentSessionId(st);
    if (id) {
      mount('session', id, () => mountSession(main, {
        api, id, deny, toast, go,
        summary: () => store.get().sessions.find((x) => x.id === id),
        profiles: async () => (await api.get('/profiles')).profiles || [],
        continueIn: (path, profile) => launch({ path, name: path.split('/').pop() }, profile, 'continue'),
      }));
      return;
    }
    unmount();
    if (!st.loaded) return show(main);
    return renderEmpty(main, {
      projects: st.projects, opening: st.launches, onProjects: () => go('#/projects'),
      onContinue: (p) => launch(p, launchProfile(p, st.profiles.names, st.profiles.default), 'continue'),
    });
  }
  if (r.view === 'projects') {
    mount('projects', '', () => mountProjects(main, {
      api, toast, launch, dismiss,
      openSession: (id) => go(sessionHash(id)),
    })).update(st);
    return;
  }
  unmount();
  if (r.view === 'profiles') return placeholder('Profiles', 'The Profiles page comes next.');
  return placeholder(r.isNew ? 'New profile' : r.name, 'The profile page comes next.');
}

function placeholder(title, text) {
  show(main, h('div', { class: 'main-inner' }, h('section', { class: 'placeholder' },
    h('h1', { class: 'page-title' }, title), h('p', { class: 'lede' }, text))));
}

// ---- Launches ---------------------------------------------------------------

function setLaunch(path, value) {
  const launches = { ...store.get().launches };
  if (value) launches[path] = value;
  else delete launches[path];
  store.set({ launches });
}

function dismiss(path) { setLaunch(path, null); }

// launch opens the user's terminal in a project and runs ccwrap there
// (mode new or continue). The row shows opening until a session from that
// folder appears; after LAUNCH_TIMEOUT_MS it says none did.
async function launch(p, profile, mode) {
  const since = Date.now();
  setLaunch(p.path, { state: 'opening', since, mode, terminal: '' });
  const onProjects = store.get().route.view === 'projects';
  try {
    const out = await api.post('/projects/launch', { path: p.path, profile, mode });
    const cur = store.get().launches[p.path];
    if (cur && cur.since === since && cur.state === 'opening') setLaunch(p.path, { ...cur, terminal: out.terminal });
    setTimeout(() => {
      const c = store.get().launches[p.path];
      if (c && c.since === since && c.state === 'opening') {
        setLaunch(p.path, { ...c, state: 'timeout' });
        if (store.get().route.view !== 'projects') toast('No session appeared after 30s — check the ' + (c.terminal || 'terminal') + ' window.', 'warning');
      }
    }, LAUNCH_TIMEOUT_MS);
  } catch (err) {
    const message = 'Not opened: ' + err.message;
    if (onProjects) setLaunch(p.path, { state: 'rejected', since, mode, message });
    else { setLaunch(p.path, null); toast(message, 'error'); }
  }
}

// launchesSettled ends the launches newly listed sessions answer.
function launchesSettled(sessions) {
  const st = store.get();
  const { launches, started } = settleLaunches(st.launches, sessions);
  if (!started.length) return;
  for (const s of started) {
    const name = (st.projects.find((x) => x.path === s.project_dir) || {}).name || s.display.title;
    toast('Session started in ' + name + ' · ' + (s.profile || 'no profile') + '.');
  }
  store.set({ launches });
}

let lastKey = '';
store.subscribe((st) => {
  renderShell(st);
  if (mounted && mounted.kind === 'projects' && st.route.view === 'projects') {
    mounted.handle.update(st);
    return;
  }
  // The session view follows the list too: a session that ended shows so
  // even if its session.ended never arrived.
  if (mounted && mounted.kind === 'session' && st.route.view === 'session') mounted.handle.update();
  // Redraw the page only when what it shows changed, so focus and scroll
  // stay put while the list ticks.
  const key = JSON.stringify([st.route, currentSessionId(st), st.loaded,
    st.route.view === 'session' && !currentSessionId(st) ? [st.projects, st.launches] : null]);
  if (key !== lastKey) {
    lastKey = key;
    renderMain(st);
  }
});

// ---- Data -----------------------------------------------------------------

async function loadLists() {
  const [sessions, projects, profiles] = await Promise.allSettled([api.get('/sessions'), api.get('/projects'), api.get('/profiles')]);
  if (sessions.status === 'fulfilled') setSessions(sessions.value.sessions || []);
  const patch = { projectsLoaded: true };
  if (projects.status === 'fulfilled') patch.projects = projects.value.projects || [];
  if (profiles.status === 'fulfilled') {
    patch.profiles = { names: (profiles.value.profiles || []).map((p) => p.name), default: profiles.value.default || '' };
  }
  store.set(patch);
}

const events = connectEvents(boot.instance, {
  state(conn, lastContact) {
    const st = store.get();
    if (conn === 'connected' && st.everConnected && st.conn !== 'connected') toast('Reconnected to ccwrap ui.');
    store.set({ conn, lastContact, everConnected: st.everConnected || conn === 'connected' });
  },
  hello(data) {
    if (Array.isArray(data.sessions)) setSessions(data.sessions);
  },
  replaced() { location.reload(); },
  event(type, payload) {
    if (type === 'sessions.changed') {
      setSessions(payload.sessions || []);
      launchesSettled(payload.sessions || []);
      return;
    }
    if (type === 'session.updated' || type === 'session.ended') patchSession(payload.session_id, payload.data);
    if (mounted && mounted.kind === 'session' && mounted.id === payload.session_id) mounted.handle.event(type, payload);
  },
});

loadLists();
// Relative times ("12m", "ended 40s ago") move on.
setInterval(() => store.set({}), 15000);
