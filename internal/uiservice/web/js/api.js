// Talking to ccwrap ui: JSON requests (writes carry the page's token) and
// the merged event stream.

// bootstrap is what the page was served with: the write token, the service
// instance, its version and address, and the credential header names.
export function bootstrap(dataset) {
  return {
    token: dataset.token || '',
    instance: dataset.instance || '',
    version: dataset.version || '',
    address: dataset.address || '',
    deny: String(dataset.deny || '').split(',').filter(Boolean),
  };
}

export class ApiError extends Error {
  constructor(status, code, message) {
    super(message);
    this.status = status;
    this.code = code;
  }
}

export function createApi(token, fetchImpl = globalThis.fetch.bind(globalThis)) {
  async function request(method, path, body) {
    const headers = { Accept: 'application/json' };
    if (method !== 'GET') headers['X-CCWRAP-Token'] = token;
    let payload;
    if (body !== undefined) {
      headers['Content-Type'] = 'application/json';
      payload = JSON.stringify(body);
    }
    let res;
    try {
      res = await fetchImpl('/api/v1' + path, { method, headers, body: payload, cache: 'no-store', credentials: 'same-origin' });
    } catch (err) {
      throw new ApiError(0, 'unreachable', "ccwrap ui can't be reached.");
    }
    let data = null;
    try { data = await res.json(); } catch (_) { data = null; }
    if (!res.ok) {
      const e = (data && data.error) || {};
      throw new ApiError(res.status, e.code || 'http_' + res.status, e.message || res.statusText || 'request failed');
    }
    return data;
  }
  // text reads a body as it is (captured request and response bodies).
  async function text(path) {
    let res;
    try {
      res = await fetchImpl('/api/v1' + path, { cache: 'no-store', credentials: 'same-origin' });
    } catch (err) {
      throw new ApiError(0, 'unreachable', "ccwrap ui can't be reached.");
    }
    if (!res.ok) throw new ApiError(res.status, 'http_' + res.status, res.statusText || 'request failed');
    return res.text();
  }
  return {
    text,
    get: (path) => request('GET', path),
    post: (path, body) => request('POST', path, body),
    put: (path, body) => request('PUT', path, body),
    patch: (path, body) => request('PATCH', path, body),
    del: (path, body) => request('DELETE', path, body),
  };
}

// How long the stream may stay down before the page says it is
// disconnected (the browser keeps retrying either way).
export const GIVE_UP_MS = 5000;

// connState is the connection pill's state after an event: hello
// (connected), error (the stream dropped; the browser retries), giveup (still
// down after GIVE_UP_MS), retry (the user asked to reconnect).
export function connState(state, event) {
  switch (event) {
    case 'hello': return 'connected';
    case 'error': return state === 'connected' ? 'reconnecting' : state;
    case 'giveup': return 'disconnected';
    case 'retry': return state === 'disconnected' ? 'reconnecting' : state;
  }
  return state;
}

// connectEvents follows /api/v1/events. on.state(state) gets each pill
// state, on.hello(data) the greeting, on.event(type, payload) the rest.
// A hello from a different service instance means ccwrap ui was replaced:
// on.replaced() (the page reloads, for a fresh token).
export function connectEvents(instance, on, ES = globalThis.EventSource) {
  let es = null;
  let state = 'connecting';
  let giveUpTimer = null;
  let lastContact = null;
  const set = (event) => {
    const next = connState(state, event);
    if (next !== state) {
      state = next;
      on.state(state, lastContact);
    }
  };
  const armGiveUp = () => {
    if (giveUpTimer == null) giveUpTimer = setTimeout(() => { giveUpTimer = null; set('giveup'); }, GIVE_UP_MS);
  };
  const disarm = () => { if (giveUpTimer != null) { clearTimeout(giveUpTimer); giveUpTimer = null; } };
  const types = ['sessions.changed', 'session.updated', 'session.ended', 'request.added', 'error.added', 'trace.added'];

  function open() {
    es = new ES('/api/v1/events');
    armGiveUp();
    es.addEventListener('hello', (msg) => {
      let data = {};
      try { data = JSON.parse(msg.data); } catch (_) { data = {}; }
      if (instance && data.instance_id && data.instance_id !== instance) {
        on.replaced();
        return;
      }
      lastContact = Date.now();
      disarm();
      set('hello');
      on.hello(data);
    });
    for (const type of types) {
      es.addEventListener(type, (msg) => {
        lastContact = Date.now();
        let data = null;
        try { data = JSON.parse(msg.data); } catch (_) { return; }
        on.event(type, data);
      });
    }
    es.addEventListener('error', () => {
      set('error');
      armGiveUp();
      // A closed stream (an HTTP error, not a dropped connection) is not
      // retried by the browser.
      if (es.readyState === 2) setTimeout(reopen, 1500);
    });
  }
  function reopen() {
    if (es) es.close();
    open();
  }
  open();
  return {
    // reconnect is the banner's button: start over now.
    reconnect() { set('retry'); disarm(); reopen(); },
    lastContact: () => lastContact,
    close() { disarm(); if (es) es.close(); },
  };
}
