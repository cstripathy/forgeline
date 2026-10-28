import { h, icon, mount, pageHeader, relTime, absTime, emptyState, spinner, errorBox, toast, pill } from '../lib/dom.js';
import { get, post, atLeast } from '../lib/api.js';
import { navigate } from '../lib/router.js';
import { session } from '../app.js';

export async function pipelinesPage() {
  const el = h('div', {});
  mount(el, spinner());
  let list, ov;
  try { [list, ov] = await Promise.all([get('/v1/pipelines'), get('/v1/overview')]); } catch (e) { return mount(el, errorBox(e.message)); }
  const latest = Object.fromEntries(ov.pipelines.map(p => [p.name, p.latest]));
  const canEdit = atLeast(session.role, 'developer');

  const run = async (p) => {
    try { const r = await post('/v1/pipelines/' + encodeURIComponent(p.id) + '/run'); toast('Started ' + p.name, 'ok'); navigate('/runs/' + r.id); } catch (e) { toast(e.message, 'error'); }
  };
  const rows = list.pipelines.map(p => {
    const open = () => navigate('/pipelines/' + encodeURIComponent(p.id));
    return h('tr', { class: 'row-link', tabindex: '0', onclick: open, onkeydown: (e) => { if (e.key === 'Enter') open(); } },
      h('td', {}, h('div', { class: 'font-medium' }, p.name), p.description && h('div', { class: 'muted max-w-md truncate text-xs' }, p.description)),
      h('td', { class: 'tabular-nums' }, p.steps), h('td', { class: 'tabular-nums' }, 'v' + p.version),
      h('td', {}, latest[p.name] ? pill(latest[p.name].status) : h('span', { class: 'muted text-xs' }, 'never run')),
      h('td', { class: 'muted whitespace-nowrap text-sm', title: absTime(p.updated_at) }, relTime(p.updated_at) + ' by ' + p.updated_by),
      h('td', { class: 'text-right' }, canEdit && h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: (e) => { e.stopPropagation(); run(p); } }, icon('play', 'size-3'), 'Run')));
  });
  mount(el,
    pageHeader('Pipelines', 'Saved pipeline definitions. Each save makes a new version you can restore.',
      canEdit && h('a', { href: '#/pipelines/new', class: 'btn-primary' }, icon('plus'), 'New pipeline')),
    h('div', { class: 'card overflow-x-auto' }, list.pipelines.length
      ? h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Pipeline', 'Steps', 'Version', 'Latest run', 'Last changed', ''].map(x => h('th', {}, x)))), h('tbody', {}, rows))
      : emptyState('flow', 'No saved pipelines', 'A pipeline is a set of steps that run in containers, with dependencies between them.',
          canEdit && h('a', { href: '#/pipelines/new', class: 'btn-primary' }, icon('plus'), 'Create your first pipeline'))));
  return el;
}
