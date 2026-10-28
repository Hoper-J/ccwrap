// Hash routes, so the page works on a forwarded port with no server
// routing: #/sessions/<id> · #/projects · #/profiles · #/profiles/<name> ·
// #/profiles/new · #/profiles/<name>/<field>.

export function parseRoute(hash) {
  const parts = String(hash || '').replace(/^#\/?/, '').split('/').filter(Boolean).map((p) => {
    try { return decodeURIComponent(p); } catch (_) { return p; }
  });
  switch (parts[0]) {
    case 'sessions': return { view: 'session', id: parts[1] || '' };
    case 'projects': return { view: 'projects' };
    case 'profiles':
      if (parts[1] === 'new') return { view: 'profile', isNew: true, name: '' };
      // #/profiles/<name>/<field> opens the profile at a field to fix
      // (key, base_url).
      if (parts[1]) return { view: 'profile', isNew: false, name: parts[1], field: parts[2] || '' };
      return { view: 'profiles' };
  }
  return { view: 'session', id: '' };
}

export function sessionHash(id) { return '#/sessions/' + encodeURIComponent(id); }
export function profileHash(name) { return '#/profiles/' + encodeURIComponent(name); }

// landingSession is the session the bare session route shows: the most
// recently started one still running, or null for the empty state.
export function landingSession(sessions) {
  let best = null;
  for (const s of sessions) {
    if (s.state === 'ended') continue;
    if (!best || Date.parse(s.started_at) > Date.parse(best.started_at)) best = s;
  }
  return best;
}
