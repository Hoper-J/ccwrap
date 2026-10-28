// One state object for the page; views subscribe and redraw from it.

export function createStore(initial) {
  let state = initial;
  const subscribers = new Set();
  return {
    get: () => state,
    // set merges a patch (or the patch a function returns) into the state.
    set(patch) {
      const next = typeof patch === 'function' ? patch(state) : patch;
      state = { ...state, ...next };
      for (const fn of subscribers) fn(state);
    },
    subscribe(fn) {
      subscribers.add(fn);
      return () => subscribers.delete(fn);
    },
  };
}

// mergeSessions is the sidebar's session list: the server's, plus the
// ended session the user is viewing if the server already dropped it, so
// the viewed one stays until they leave.
export function mergeSessions(server, previous, viewedId) {
  const list = server.slice();
  if (viewedId && !list.some((s) => s.id === viewedId)) {
    const kept = previous.find((s) => s.id === viewedId);
    if (kept) {
      const ended = { ...kept, state: 'ended', display: { ...kept.display, badge: '' } };
      if (!ended.ended_at) ended.ended_at = new Date().toISOString();
      list.push(ended);
      list.sort((a, b) => Date.parse(a.started_at) - Date.parse(b.started_at) || (a.id < b.id ? -1 : 1));
    }
  }
  return list;
}
