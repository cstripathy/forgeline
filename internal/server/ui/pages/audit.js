import { h, icon, mount, pageHeader, relTime, absTime, emptyState, spinner, errorBox } from '../lib/dom.js';
import { get } from '../lib/api.js';

export async function auditPage() {
  const el = h('div', {});
  const actor = h('input', { id: 'f-actor', class: 'input w-40', placeholder: 'Person', 'aria-label': 'Filter by person' });
  const action = h('input', { id: 'f-action', class: 'input w-48', placeholder: 'Action, e.g. secret', 'aria-label': 'Filter by action' });
  const denied = h('input', { id: 'f-denied', type: 'checkbox', class: 'size-4 rounded' });
  const body = h('div', {});

  async function load() {
    const q = new URLSearchParams({ limit: '200' });
    if (actor.value.trim()) q.set('actor', actor.value.trim());
    if (action.value.trim()) q.set('action', action.value.trim());
    if (denied.checked) q.set('denied', '1');
    let events;
    try { events = (await get('/v1/audit?' + q)).events; } catch (e) { return mount(body, errorBox(e.message)); }
    mount(body, h('div', { class: 'card overflow-x-auto' }, events.length ? h('table', { class: 'tbl' },
      h('thead', {}, h('tr', {}, ['When', 'Who', 'What', 'On', 'Detail', 'From'].map(x => h('th', {}, x)))),
      h('tbody', {}, events.map(e => h('tr', {},
        h('td', { class: 'muted whitespace-nowrap text-sm', title: absTime(e.time) }, relTime(e.time)),
        h('td', { class: 'whitespace-nowrap font-medium' }, e.actor, e.role && h('span', { class: 'muted ml-1 text-xs' }, e.role)),
        h('td', {}, h('span', { class: (e.allowed ? 'pill-idle' : 'pill-bad') + ' font-mono' }, e.allowed ? e.action : e.action + ' · refused')),
        h('td', { class: 'font-mono text-xs' }, e.target || ''), h('td', { class: 'max-w-md text-sm' }, e.detail || ''), h('td', { class: 'muted font-mono text-xs' }, e.ip || ''))))
    ) : emptyState('shield', 'Nothing matches', 'Try fewer filters.')));
  }

  let t;
  for (const i of [actor, action]) i.addEventListener('input', () => { clearTimeout(t); t = setTimeout(load, 300); });
  denied.addEventListener('change', load);
  mount(el, pageHeader('Audit log', 'Who did what, including everything that was refused. It can be read here and never edited.',
    h('button', { class: 'btn-secondary', type: 'button', onclick: load }, icon('refresh', 'size-3.5'), 'Refresh')),
    h('div', { class: 'mb-4 flex flex-wrap items-center gap-3' }, actor, action, h('label', { class: 'flex items-center gap-2 text-sm' }, denied, 'Only refused actions')), body);
  await load();
  return el;
}
