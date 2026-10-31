import { h, icon, mount, clear, pageHeader, emptyState, spinner, errorBox, notice, toast, modal, confirmDialog } from '../lib/dom.js';
import { get, post, del, atLeast } from '../lib/api.js';
import { session } from '../app.js';

// Marketplace: packages of skills and agents. Each is shown with what it can do before anyone
// installs it, and the install is bound to the fingerprint that was shown.

const tierText = (t) => t === 'T1' ? 'T1 · may file change proposals (a person still approves)' : 'T0 · reads and advises only';

export async function marketplacePage() {
  const admin = atLeast(session.role, 'admin');
  const el = h('div', {});
  let packages = [], tag = '', q = '';

  async function load() {
    try { packages = (await get('/v1/market/packages')).packages; } catch (e) { return mount(el, errorBox(e.message)); }
    draw();
  }

  function draw() {
    const tags = [...new Set(packages.flatMap(p => p.tags || []))].sort();
    const shown = packages.filter(p => (!tag || (p.tags || []).includes(tag)) && (!q || (p.title + ' ' + p.description).toLowerCase().includes(q.toLowerCase())));
    const search = h('input', { class: 'input w-56', id: 'market-search', type: 'search', placeholder: 'Search packages', 'aria-label': 'Search packages', value: q,
      oninput: (e) => { q = e.target.value; const pos = e.target.selectionStart; draw(); const n = document.querySelector('#market-search'); n.focus(); n.setSelectionRange(pos, pos); } });
    mount(el, pageHeader('Marketplace', 'Ready-made skills and agents. You see what each one can do before it is installed, and an install is pinned to the fingerprint you reviewed.', search),
      h('div', { class: 'mb-4 flex flex-wrap gap-2' }, [['', 'All'], ...tags.map(t => [t, t])].map(([v, l]) => h('button', { type: 'button', 'data-tag': v, 'aria-pressed': String(tag === v),
        class: 'rounded-full border px-3 py-1 text-sm ' + (tag === v ? 'border-brand-500 bg-brand-50 text-brand-700 dark:bg-brand-950/40 dark:text-brand-200' : 'border-slate-200 muted hover:border-brand-400 dark:border-slate-800'),
        onclick: () => { tag = v; draw(); } }, l))),
      shown.length ? h('div', { class: 'grid gap-4 sm:grid-cols-2 xl:grid-cols-3', id: 'market-grid' }, shown.map(card)) : emptyState('plug', 'Nothing matches', 'Try another word or clear the tag.'));
  }

  function card(p) {
    const inst = p.installed;
    return h('div', { class: 'card flex flex-col p-4', 'data-package': p.id },
      h('div', { class: 'flex items-start justify-between gap-2' }, h('h3', { class: 'font-semibold' }, p.title),
        p.problem ? h('span', { class: 'pill-bad' }, 'damaged') : p.update ? h('span', { class: 'pill-wait' }, 'update') : inst ? h('span', { class: 'pill-ok' }, 'installed') : null),
      h('p', { class: 'muted mt-1 flex-1 text-sm' }, p.description),
      h('div', { class: 'muted mt-3 text-xs' }, `${p.agents.length} agent${p.agents.length === 1 ? '' : 's'} · ${p.skills.length} skill${p.skills.length === 1 ? '' : 's'} · v${p.version} · ${p.publisher}`),
      h('div', { class: 'mt-3 flex justify-end' }, h('button', { class: 'btn-secondary btn-sm', type: 'button', 'data-open': p.id, onclick: () => detail(p) }, 'Review')));
  }

  function detail(p) {
    modal(p.title, (close) => {
      const inst = p.installed;
      const tier = h('select', { id: 'install-tier', class: 'input w-auto', disabled: !!inst }, h('option', { value: 'T0' }, 'T0 · advise only'), p.writes ? h('option', { value: 'T1' }, 'T1 · may file proposals') : null);
      const err = h('div', { class: 'hidden' });
      const act = async (fn, done) => {
        err.classList.add('hidden');
        try { await fn(); toast(done, 'ok'); close(); load(); } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); }
      };
      return h('div', { class: 'space-y-4', id: 'package-detail' },
        h('p', { class: 'text-sm' }, p.description),
        h('dl', { class: 'grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm' }, h('dt', { class: 'muted' }, 'Publisher'), h('dd', {}, p.publisher), h('dt', { class: 'muted' }, 'Version'), h('dd', {}, 'v' + p.version + (inst ? ` (installed: v${inst.version})` : '')),
          h('dt', { class: 'muted' }, 'Fingerprint'), h('dd', { class: 'break-all font-mono text-xs', id: 'package-hash' }, p.sha256)),
        p.problem ? notice('error', p.problem) : null,
        h('div', {}, h('div', { class: 'h-section mb-2' }, 'What it can do'),
          p.agents.map(a => h('div', { class: 'mb-3 rounded-lg border border-slate-200 p-3 dark:border-slate-800', 'data-agent': a.key },
            h('div', { class: 'font-medium' }, a.name), h('div', { class: 'muted text-sm' }, a.description),
            h('div', { class: 'mt-2 text-xs' }, h('span', { class: 'muted' }, 'Allowed to read: '), a.tools.map(t => h('code', { class: 'mr-1.5' }, t))),
            h('div', { class: 'mt-1 text-xs muted' }, tierText(a.tier))))),
        p.skills.length ? h('div', {}, h('div', { class: 'h-section mb-2' }, 'Skills it adds'), p.skills.map(s => h('details', { class: 'mb-2' }, h('summary', { class: 'cursor-pointer text-sm font-medium' }, s.name),
          h('pre', { class: 'mt-1 whitespace-pre-wrap rounded-lg border border-slate-200 p-3 text-xs dark:border-slate-800' }, s.body)))) : null,
        notice('info', 'Nothing here can change your cloud by itself. At most it can propose a change, and a different administrator approves it.'),
        admin && !p.problem ? h('div', { class: 'space-y-2' }, err,
          h('div', { class: 'flex flex-wrap items-center justify-between gap-2' },
            inst ? h('button', { class: 'btn-ghost btn-sm', type: 'button', id: 'uninstall', onclick: async () => {
              if (await confirmDialog({ title: 'Uninstall ' + p.title, body: 'Its agents are disabled. The skills and agents stay, with their history, so you can delete them yourself.', confirmLabel: 'Uninstall' }))
                act(() => del('/v1/market/install/' + p.id), 'Uninstalled');
            } }, 'Uninstall') : h('label', { class: 'flex items-center gap-2 text-sm' }, h('span', { class: 'muted' }, 'Allow up to'), tier),
            !inst || p.update ? h('button', { class: 'btn-primary', type: 'button', id: 'install', onclick: () => act(() => post('/v1/market/install', { id: p.id, hash: p.sha256, tier: tier.value }), inst ? 'Updated' : 'Installed') }, inst ? 'Update to v' + p.version : 'Install') : null),
          inst ? h('p', { class: 'muted text-xs' }, `Installed by ${inst.by}. Tier stays at ${inst.tier}; an update cannot raise it.`) : null) : null);
    }, { wide: true });
  }

  mount(el, spinner());
  await load();
  return el;
}
