// The AWS resource explorer: everything in the connected account by service, what Forgeline has
// changed there, and a detail view per resource. Read-only; changes go through Proposals.
import { h, icon, mount, pill, relTime, absTime, spinner, errorBox, notice, modal, emptyState, copyButton } from '../lib/dom.js';
import { get } from '../lib/api.js';

const CATEGORIES = ['Compute', 'Containers', 'Storage', 'Databases', 'Network', 'Messaging', 'Management'];
const DOT = { Compute: 'bg-sky-500', Containers: 'bg-violet-500', Storage: 'bg-emerald-500', Databases: 'bg-amber-500', Network: 'bg-cyan-500', Messaging: 'bg-pink-500', Management: 'bg-slate-500' };
const PAGE = 50;

/** A resource state as a coloured pill. AWS names states differently per service, so match by meaning. */
function stateBadge(state) {
  if (!state) return h('span', { class: 'muted' }, '–');
  const s = state.toLowerCase();
  const cls = /^(running|active|available|in-use|in_use|enabled|healthy|.*_complete|create_complete|update_complete)$/.test(s) && !/rollback|delete/.test(s) ? 'pill-ok'
    : /fail|error|rollback|impaired|deleted|inactive|incompatible/.test(s) ? 'pill-bad'
    : /pending|creating|provisioning|modifying|starting|updating|stopping|in_progress|in-progress|backing/.test(s) ? 'pill-wait' : 'pill-idle';
  return h('span', { class: cls }, h('span', { class: 'size-1.5 rounded-full bg-current' }), state);
}

function stateSummary(svc) {
  const counts = {};
  for (const r of svc.resources) if (r.state) counts[r.state] = (counts[r.state] || 0) + 1;
  return Object.entries(counts).sort((a, b) => b[1] - a[1]).slice(0, 2).map(([s, n]) => `${n} ${s.toLowerCase()}`).join(' · ');
}

/** Which applied proposals mention this resource by name or id. Short names would match anything, so ignore them. */
function touchedBy(res, proposals) {
  const keys = [res.name, res.id].filter(k => k && k.length >= 4);
  return proposals.filter(p => p.status === 'applied' && keys.some(k => p.code.includes(k)));
}

export function explorer({ dev, account = 'primary' }) {
  const root = h('section', { class: 'card', id: 'aws-explorer' }, spinner());
  let inv = null, proposals = [], selected = '', query = '', shown = PAGE;

  async function load(refresh) {
    mount(root, spinner());
    try {
      [inv, proposals] = await Promise.all([
        get('/v1/infra/aws/resources?account=' + encodeURIComponent(account) + (refresh ? '&refresh=1' : '')),
        // Proposals are for the primary account, so only it can show "changed through Forgeline".
        account === 'primary' ? get('/v1/ai/proposals').then(r => r.proposals).catch(() => []) : [],
      ]);
    } catch (e) { return mount(root, h('div', { class: 'p-5' }, errorBox(e.message))); }
    draw();
  }

  function draw() {
    if (!inv.enabled) return mount(root, '');
    if (inv.error) return mount(root, h('div', { class: 'p-5' }, errorBox(inv.error)));
    const withStuff = inv.services.filter(s => s.count > 0);
    const empty = inv.services.filter(s => s.count === 0 && !s.error);
    const denied = inv.services.filter(s => s.error);
    const total = withStuff.reduce((n, s) => n + s.count, 0);
    const applied = proposals.filter(p => p.status === 'applied');

    const search = h('input', { id: 'explorer-search', class: 'input w-56', type: 'search', placeholder: 'Search name, id, tag…', value: query, 'aria-label': 'Search resources',
      oninput: (e) => { query = e.target.value; shown = PAGE; drawTable(); } });
    const ordered = CATEGORIES.flatMap(cat => withStuff.filter(s => s.category === cat));
    const tiles = h('div', { class: 'space-y-3 px-5 py-4' },
      ordered.length ? h('div', { class: 'grid gap-3 sm:grid-cols-2 lg:grid-cols-4' }, ordered.map(tile)) : emptyState('cloud', 'Nothing found in this region', 'Global services such as S3 are listed too. Check the region in Change connection.'),
      empty.length || denied.length ? h('div', { class: 'space-y-1 border-t border-slate-200 pt-3 text-xs dark:border-slate-800' },
        empty.length ? h('p', { class: 'muted' }, 'Nothing in: ' + empty.map(s => s.label).join(', ')) : null,
        denied.length ? h('p', { class: 'text-amber-700 dark:text-amber-400' }, 'Could not list: ' + denied.map(s => `${s.label} (${s.error})`).join('; ')) : null) : null);

    const tableBox = h('div', { id: 'explorer-table' });
    mount(root,
      h('div', { class: 'flex flex-wrap items-center justify-between gap-3 border-b border-slate-200 px-5 py-3 dark:border-slate-800' },
        h('div', {}, h('h2', { class: 'h-section' }, 'Resources'),
          h('p', { class: 'muted mt-0.5 text-xs', title: absTime(inv.fetched_at) }, `${total} in ${withStuff.length} service${withStuff.length === 1 ? '' : 's'} · region ${inv.region} · read ${relTime(inv.fetched_at)}`)),
        h('div', { class: 'flex items-center gap-2' }, search,
          h('button', { class: 'btn-secondary btn-sm', type: 'button', id: 'explorer-refresh', onclick: () => load(true) }, icon('refresh', 'size-3.5'), 'Refresh'))),
      tiles, tableBox, changesPanel(applied));
    drawTable();
  }

  function tile(svc) {
    const on = selected === svc.id;
    return h('button', { type: 'button', 'data-service': svc.id, 'aria-pressed': String(on),
      class: 'rounded-xl border p-3 text-left transition hover:border-brand-400 ' + (on ? 'border-brand-500 bg-brand-50 ring-2 ring-brand-500/30 dark:bg-brand-950/30' : 'border-slate-200 dark:border-slate-800'),
      onclick: () => { selected = on ? '' : svc.id; shown = PAGE; draw(); } },
      h('div', { class: 'flex items-center justify-between gap-2' }, h('span', { class: 'flex min-w-0 items-center gap-2' }, h('span', { class: 'size-2 shrink-0 rounded-full ' + DOT[svc.category], title: svc.category }), h('span', { class: 'truncate text-sm font-medium' }, svc.label)), svc.global ? h('span', { class: 'muted text-[10px] uppercase' }, 'global') : null),
      h('div', { class: 'mt-1 text-2xl font-semibold tabular-nums' }, svc.count),
      h('div', { class: 'muted mt-0.5 truncate text-xs' }, stateSummary(svc) || '\u00a0'));
  }

  function matches(r) {
    if (!query) return true;
    const q = query.toLowerCase();
    return [r.name, r.id, r.state, r.arn, ...(r.fields || []).map(f => f.value), ...Object.entries(r.tags || {}).flat()].some(v => v && String(v).toLowerCase().includes(q));
  }

  function drawTable() {
    const box = root.querySelector('#explorer-table');
    if (!box) return;
    const rows = [];
    for (const s of inv.services) if (!selected || s.id === selected) for (const r of s.resources) if (matches(r)) rows.push({ s, r });
    const label = selected ? inv.services.find(s => s.id === selected)?.label : 'All resources';
    const capped = inv.services.some(s => s.truncated && (!selected || s.id === selected));
    mount(box, h('div', { class: 'border-t border-slate-200 dark:border-slate-800' },
      h('div', { class: 'flex items-center justify-between px-5 py-2.5 text-sm' }, h('span', { class: 'font-medium' }, `${label} · ${rows.length}`),
        selected && h('button', { class: 'btn-ghost btn-sm', type: 'button', onclick: () => { selected = ''; draw(); } }, 'Show all')),
      rows.length ? h('div', { class: 'overflow-x-auto' }, h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Resource', 'Service', 'State', 'Created', 'Forgeline'].map(x => h('th', {}, x)))),
        h('tbody', {}, rows.slice(0, shown).map(({ s, r }) => {
          const by = touchedBy(r, proposals);
          return h('tr', { class: 'row-link', tabindex: '0', 'data-resource': r.id, onclick: () => detail(s, r), onkeydown: (e) => { if (e.key === 'Enter') detail(s, r); } },
            h('td', {}, h('div', { class: 'font-medium' }, r.name), r.id !== r.name && h('div', { class: 'muted max-w-xs truncate font-mono text-xs' }, r.id)),
            h('td', { class: 'whitespace-nowrap text-sm' }, s.label), h('td', {}, stateBadge(r.state)),
            h('td', { class: 'muted whitespace-nowrap text-sm', title: absTime(r.created) }, r.created ? relTime(r.created) : '–'),
            h('td', {}, by.length ? h('span', { class: 'pill-run', title: by.map(p => p.title).join('\n') }, icon('sparkle', 'size-3'), `changed · ${by.length}`) : h('span', { class: 'muted' }, '–')));
        })))) : h('p', { class: 'muted px-5 pb-5 text-sm' }, query ? 'Nothing matches that search.' : 'Nothing here.'),
      rows.length > shown && h('div', { class: 'flex justify-center pb-4' }, h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: () => { shown += PAGE; drawTable(); } }, `Show ${Math.min(PAGE, rows.length - shown)} more`)),
      capped && h('p', { class: 'muted px-5 pb-4 text-xs' }, `Showing the first 200 of the largest services. Use search or the assistant to find the rest.`)));
  }

  function changesPanel(applied) {
    if (!applied.length) return h('div', { class: 'border-t border-slate-200 px-5 py-4 text-sm dark:border-slate-800' },
      h('div', { class: 'h-section mb-1' }, 'Changed through Forgeline'), h('p', { class: 'muted' }, 'Nothing yet. Changes the assistant proposes and an administrator approves will be listed here.'));
    return h('div', { class: 'border-t border-slate-200 px-5 py-4 dark:border-slate-800', id: 'forgeline-changes' },
      h('div', { class: 'h-section mb-2' }, 'Changed through Forgeline'),
      h('ul', { class: 'space-y-2' }, applied.slice(0, 6).map(p => h('li', { class: 'flex flex-wrap items-center gap-x-3 gap-y-1 text-sm' },
        pill('applied'), h('a', { href: '#/proposals/' + encodeURIComponent(p.id), class: 'font-medium hover:underline' }, p.title),
        h('span', { class: 'muted text-xs' }, `${p.proposed_by} proposed, ${p.decided_by || '?'} approved · ${relTime(p.finished_at || p.decided_at)}`),
        h('span', { class: 'flex flex-wrap gap-1' }, (p.ran_calls?.length ? p.ran_calls : p.calls).map(c => h('code', { class: 'rounded bg-slate-100 px-1.5 py-0.5 text-xs dark:bg-slate-800' }, `${c.service}.${c.operation}`)))))));
  }

  function detail(svc, r) {
    const by = touchedBy(r, proposals);
    modal(r.name, (close) => h('div', { class: 'space-y-4', id: 'resource-detail' },
      h('div', { class: 'flex flex-wrap items-center gap-3' }, stateBadge(r.state), h('span', { class: 'muted text-sm' }, svc.label + (r.created ? ' · created ' + relTime(r.created) : ''))),
      r.fields?.length ? h('dl', { class: 'grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm' }, r.fields.map(f => [h('dt', { class: 'muted' }, f.label), h('dd', { class: 'break-all font-mono text-xs leading-5' }, f.value)])) : null,
      h('div', {}, h('div', { class: 'h-section mb-1' }, 'Identifier'), h('div', { class: 'flex items-center gap-2' }, h('code', { class: 'break-all text-xs' }, r.arn || r.id), copyButton(r.arn || r.id, 'Copy'))),
      r.tags ? h('div', {}, h('div', { class: 'h-section mb-1' }, 'Tags'), h('div', { class: 'flex flex-wrap gap-1.5' }, Object.entries(r.tags).map(([k, v]) => h('span', { class: 'rounded-md bg-slate-100 px-2 py-0.5 text-xs dark:bg-slate-800' }, h('span', { class: 'muted' }, k + ': '), v)))) : null,
      by.length ? h('div', {}, h('div', { class: 'h-section mb-1' }, 'Changed through Forgeline'),
        h('ul', { class: 'space-y-1 text-sm' }, by.map(p => h('li', {}, h('a', { href: '#/proposals/' + encodeURIComponent(p.id), class: 'font-medium hover:underline', onclick: () => close() }, p.title), h('span', { class: 'muted text-xs' }, ` · ${relTime(p.finished_at || p.decided_at)}`))))) : null,
      dev && h('div', { class: 'flex justify-end' }, h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: () => { close(); ask(svc, r); } }, icon('sparkle', 'size-3.5'), 'Ask the assistant about this'))), { wide: true });
  }

  /** Open the assistant with a question about this resource filled in, for the person to send. */
  function ask(svc, r) {
    const panel = document.querySelector('#assistant-input') || (document.querySelector('#assistant-open')?.click(), document.querySelector('#assistant-input'));
    if (panel) { panel.value = `Review the ${svc.label.replace(/s$/, '')} "${r.name}" (${r.arn || r.id}) in ${inv.region}: how is it configured, is anything risky or wasteful, and what would you improve?`; panel.focus(); }
  }

  load(false);
  return root;
}
