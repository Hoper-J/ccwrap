// Times as they pass. The server sends timestamps; the page words how long
// ago and how long, so the words stay true while it sits open.

function seconds(ts, now) {
  const t = typeof ts === 'number' ? ts : Date.parse(ts);
  // Go's zero time (year 1) parses to a negative number: no time either.
  if (!Number.isFinite(t) || t <= 0) return null;
  return Math.max(0, Math.floor(((now ?? Date.now()) - t) / 1000));
}

// span is a duration in the largest whole unit: "40s", "12m", "3h", "2d" —
// as ccwrap's terminal output says uptime.
export function span(secs) {
  if (secs < 60) return secs + 's';
  if (secs < 3600) return Math.floor(secs / 60) + 'm';
  if (secs < 86400) return Math.floor(secs / 3600) + 'h';
  return Math.floor(secs / 86400) + 'd';
}

// ago is "40s ago"; "" without a time.
export function ago(ts, now) {
  const s = seconds(ts, now);
  return s == null ? '' : span(s) + ' ago';
}

// since is how long since ts: "12m"; "" without a time.
export function since(ts, now) {
  const s = seconds(ts, now);
  return s == null ? '' : span(s);
}

// clock is the local time of day: "14:45:41".
export function clock(ts) {
  const d = new Date(typeof ts === 'number' ? ts : Date.parse(ts));
  if (Number.isNaN(d.getTime())) return '';
  const p = (n) => String(n).padStart(2, '0');
  return p(d.getHours()) + ':' + p(d.getMinutes()) + ':' + p(d.getSeconds());
}

// tilde shortens a path under home to ~.
export function tilde(path, home) {
  if (home && (path === home || path.startsWith(home + '/'))) return '~' + path.slice(home.length);
  return path;
}

export function basename(path) {
  const parts = String(path || '').split('/').filter(Boolean);
  return parts.length ? parts[parts.length - 1] : String(path || '');
}
