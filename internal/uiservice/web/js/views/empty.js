// No running sessions: where sessions come from, and the three most recent
// projects to continue in.

import { h, icon, show } from '../dom.js';
import { ago } from '../fmt.js';

// recentProjects is the three most recently active projects that have a
// conversation to continue.
export function recentProjects(projects) {
  return projects
    .filter((p) => p.has_conversation)
    .slice()
    .sort((a, b) => Date.parse(b.last_active_at) - Date.parse(a.last_active_at))
    .slice(0, 3);
}

// launchProfile is the profile a one-click launch in a folder uses: the
// one ccwrap last used there if it still exists, else the default.
export function launchProfile(project, profileNames, defaultName) {
  const last = project.last_profile;
  if (last === '') return '';
  if (last != null && profileNames.includes(last)) return last;
  return profileNames.includes(defaultName) ? defaultName : '';
}

// renderEmpty draws the empty state. m: {projects, opening: {path: launch},
// onProjects(), onContinue(project)}.
export function renderEmpty(main, m) {
  const recent = recentProjects(m.projects);
  show(main, h('div', { class: 'main-inner' },
    h('section', { class: 'empty', 'aria-labelledby': 'empty-title' },
      h('div', { class: 'kicker' }, 'Sessions'),
      h('h1', { class: 'page-title', id: 'empty-title' }, 'No running sessions.'),
      h('p', { class: 'lede' }, 'Start one from Projects. CCWRAP opens your terminal in the folder and runs Claude Code there; the session appears here as soon as it connects.'),
      h('div', { class: 'empty-actions' }, h('button', { type: 'button', class: 'btn btn-primary', onclick: m.onProjects }, icon('folder'), 'Open Projects')),
      recent.length ? [
        h('div', { class: 'section-label' }, 'Recent projects'),
        h('div', { class: 'recent' }, recent.map((p) => h('div', { class: 'recent-row' },
          h('div', { class: 'recent-text' },
            h('div', { class: 'recent-name' }, p.name),
            h('div', { class: 'recent-sub' }, [p.display_path, p.last_title || p.last_prompt, ago(p.last_active_at)].filter(Boolean).join(' · '))),
          m.opening[p.path] && m.opening[p.path].state === 'opening'
            ? h('span', { class: 'status-line', role: 'status' }, icon('loader', 'spin'), 'Opening' + (m.opening[p.path].terminal ? ' in ' + m.opening[p.path].terminal : '') + '…')
            : h('button', { type: 'button', class: 'btn btn-sm', onclick: () => m.onContinue(p) }, 'Continue')))),
      ] : null)));
}
