// The session view: header, issues, ribbon, Activity and configuration of
// one session, kept live from the service's event stream. When the session
// ends the view stays, frozen at its final state.

import { h, show } from '../dom.js';
import { createActivity } from './activity.js';
import { createRibbon } from './ribbon.js';
import { renderIssues, refreshEvidence } from './issues.js';
import { renderHeader, renderConfig, metaLine, endedText } from './header.js';

// mountSession shows session id in main. deps: {api, id, summary() (its
// sidebar entry, if any), deny: Set, toast, go(hash), profiles(),
// continueIn(path, profile, conversation)}. It returns {event(type, payload), unmount()}.
export function mountSession(main, deps) {
  const base = '/sessions/' + encodeURIComponent(deps.id);
  let sess = null;
  let pending = null;
  let gone = false;
  const headerEl = h('div', { class: 'sv-header' });
  const issuesEl = h('section', { class: 'sv-issues' });
  const ribbonEl = h('div', { class: 'sv-ribbon' });
  const activityEl = h('div', { class: 'sv-activity' });
  const configEl = h('div', { class: 'sv-config' });
  show(main, h('div', { class: 'main-inner session-view' }, headerEl, issuesEl, ribbonEl, activityEl, configEl));
  skeleton();

  const activity = createActivity(activityEl, {
    deny: deps.deny,
    unmask: () => !!(sess && sess.capture_bodies_unmasked),
    loadBody: (id) => deps.api.text(base + '/bodies/' + encodeURIComponent(id)),
    ended: () => !!(sess && sess.state === 'ended'),
  });
  const ribbon = createRibbon(ribbonEl, {
    api: deps.api, base, toast: deps.toast, go: deps.go, profiles: deps.profiles,
    onFix: fix, pending: () => pending,
  });

  function skeleton() {
    show(headerEl, h('div', { class: 'skel-head', 'aria-hidden': 'true' }, h('span'), h('span'), h('span'), h('span')));
    show(ribbonEl, h('div', { class: 'ribbon skel', 'aria-hidden': 'true' }, Array.from({ length: 8 }, () => h('div', { class: 'ribbon-cell' }))));
  }

  function render() {
    renderHeader(headerEl, {
      sess, path: summaryPath(), toast: deps.toast,
      onContinue: () => deps.continueIn(sess.project_dir, sess.active_profile_name || '', sess.claude_session_id || ''),
    });
    renderIssues(issuesEl, { issues: sess.issues, ended: sess.state === 'ended', pending, onFix: fix });
    ribbon.render(sess);
    renderConfig(configEl, sess.display.config);
  }

  function summaryPath() {
    const s = deps.summary();
    return (s && s.display && s.display.path) || sess.project_dir || '';
  }

  async function fix(issue, f) {
    if (f.action === 'open_profile') {
      deps.go('#/profiles/' + encodeURIComponent(f.value) + (f.field ? '/' + encodeURIComponent(f.field) : ''));
      return;
    }
    if (f.action === 'show_activity') {
      activity.reveal(f.value || 'all', f.at || '');
      return;
    }
    if (f.action !== 'model_default' || pending) return;
    pending = { id: issue.id, action: f.action, value: f.value, note: sess.active_profile_name ? 'Saving to profile ' + sess.active_profile_name + '…' : 'Saving…' };
    render();
    try {
      const out = await deps.api.post(base + '/model-default', { target: f.value });
      if (out && out.result && out.result !== 'switched') deps.toast(out.message || 'Saved, but the session did not pick it up: ' + out.result, 'warning');
    } catch (err) {
      deps.toast("Couldn't save the alias: " + err.message, 'error');
    }
    pending = null;
    if (sess) render();
  }

  async function load() {
    try {
      const [s, act] = await Promise.all([deps.api.get(base + '/session'), deps.api.get(base + '/activity')]);
      sess = s;
      render();
      activity.set({ requests: act.requests || [], errors: act.errors || [], trace: act.trace || [] });
    } catch (err) {
      gone = true;
      const s = deps.summary();
      const ended = s && s.state === 'ended';
      show(headerEl, h('section', { class: 'placeholder' },
        h('h1', { class: 'page-title' }, s ? s.display.title : 'Session not found'),
        h('p', { class: 'lede' }, ended ? 'This session has ended, and its details went with it.' : err.status === 404 ? 'No running session has this id.' : "Couldn't load this session: " + err.message),
        ended && s.project_dir ? h('button', { type: 'button', class: 'btn', onclick: () => deps.continueIn(s.project_dir, s.profile || '', s.claude_session_id || '') }, 'Continue in ' + s.display.title) : null));
      show(issuesEl);
      show(ribbonEl);
      show(activityEl);
      show(configEl);
    }
  }
  load();

  const tick = setInterval(() => {
    if (!sess) return;
    const meta = headerEl.querySelector('#sess-meta');
    if (meta) meta.textContent = metaLine(sess, Date.now());
    const ended = headerEl.querySelector('#sess-ended');
    if (ended) ended.textContent = endedText(sess, Date.now());
    refreshEvidence(issuesEl, sess.issues || []);
  }, 15000);

  return {
    event(type, payload) {
      if (gone || !sess) return;
      const data = payload.data;
      if (type === 'session.updated' || type === 'session.ended') {
        if (data && data.id === deps.id) {
          sess = data;
          render();
          activity.redraw();
        }
        return;
      }
      if (type === 'request.added') activity.add('request', data);
      else if (type === 'error.added') activity.add('error', data);
      else if (type === 'trace.added') activity.add('trace', data);
    },
    // update follows the session list, so a session whose session.ended
    // never reached the page (it crashed, or its stream dropped first)
    // still shows as ended.
    update() {
      if (gone || !sess || sess.state === 'ended') return;
      const s = deps.summary();
      if (!s || s.state !== 'ended') return;
      sess = { ...sess, state: 'ended', ended_at: s.ended_at || new Date().toISOString() };
      render();
      activity.redraw();
    },
    unmount() {
      clearInterval(tick);
      ribbon.close();
    },
  };
}
