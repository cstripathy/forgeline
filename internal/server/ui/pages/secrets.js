import { h, icon, mount, pageHeader, relTime, absTime, emptyState, spinner, errorBox, notice, toast, modal, confirmDialog, field } from '../lib/dom.js';
import { get, put, del, atLeast } from '../lib/api.js';
import { session } from '../app.js';

export async function secretsPage() {
  const el = h('div', {});
  const admin = atLeast(session.role, 'admin');
  mount(el, spinner());

  async function load() {
    let list;
    try { list = (await get('/v1/secrets')).secrets; } catch (e) { return mount(el, errorBox(e.message)); }
    const rows = list.map(s => h('tr', {},
      h('td', {}, h('div', { class: 'flex items-center gap-2 font-mono text-sm font-medium' }, icon('key', 'size-4 text-slate-400'), s.name),
        s.description && h('div', { class: 'muted ml-6 text-xs' }, s.description), s.problem && h('div', { class: 'ml-6 mt-1 text-xs text-rose-600 dark:text-rose-400' }, s.problem)),
      h('td', {}, s.source === 'vault' ? h('span', { class: 'pill-idle' }, 'Vault (encrypted)') : h('span', { class: 'pill-wait', title: 'Set by FORGELINE_SECRET_' + s.name + ' on the server; change it there' }, 'Environment')),
      h('td', { class: 'text-sm' }, s.used_by.length ? s.used_by.map((p, i) => h('span', {}, i ? ', ' : '', h('a', { href: '#/pipelines/' + encodeURIComponent(p), class: 'text-brand-600 hover:underline dark:text-brand-300' }, p))) : h('span', { class: 'muted' }, 'not used by a saved pipeline')),
      h('td', { class: 'muted whitespace-nowrap text-sm', title: s.updated_at ? absTime(s.updated_at) : '' }, s.updated_at ? `v${s.version} · ${relTime(s.updated_at)} by ${s.updated_by}` : 'set at server start'),
      h('td', { class: 'whitespace-nowrap text-right' }, admin && s.source === 'vault' && [
        h('button', { class: 'btn-secondary btn-sm mr-2', type: 'button', onclick: () => edit(s) }, icon('refresh', 'size-3.5'), 'Rotate'),
        h('button', { class: 'btn-ghost btn-sm text-rose-600 dark:text-rose-400', type: 'button', 'aria-label': 'Delete ' + s.name, onclick: () => remove(s) }, icon('trash', 'size-4'))])));
    mount(el,
      pageHeader('Secrets', 'Values steps can read, only when a step asks for them by name. Values are encrypted at rest and never shown again.',
        admin && h('button', { class: 'btn-primary', type: 'button', onclick: () => edit(null) }, icon('plus'), 'Add secret')),
      h('div', { class: 'mb-4' }, notice('info', 'A step receives only the secrets listed in its ', h('code', { class: 'font-mono text-xs' }, 'permissions.secrets'), '. Output that contains a secret is masked as ***. Runs started by GitHub receive no secrets yet.')),
      h('div', { class: 'card overflow-x-auto' }, list.length
        ? h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Name', 'Stored in', 'Used by', 'Last changed', ''].map(x => h('th', {}, x)))), h('tbody', {}, rows))
        : emptyState('key', 'No secrets yet', admin ? 'Add the first one, then list its name in a step.' : 'An administrator can add secrets.', admin && h('button', { class: 'btn-primary', type: 'button', onclick: () => edit(null) }, icon('plus'), 'Add secret'))));
  }

  function edit(existing) {
    modal(existing ? 'Rotate ' + existing.name : 'Add a secret', (close) => {
      const name = h('input', { id: 'sname', class: 'input font-mono', value: existing ? existing.name : '', readOnly: !!existing, placeholder: 'DEPLOY_KEY', autocomplete: 'off' });
      const value = h('input', { id: 'svalue', class: 'input font-mono', type: 'password', autocomplete: 'new-password', placeholder: existing ? 'New value' : 'Value' });
      const descr = h('input', { id: 'sdesc', class: 'input', value: existing ? existing.description || '' : '', maxlength: '200', placeholder: 'What it is for (optional)' });
      const err = h('div', { class: 'hidden' });
      const save = h('button', { class: 'btn-primary', type: 'button', onclick: async () => {
        err.classList.add('hidden'); save.disabled = true;
        try {
          await put('/v1/secrets/' + encodeURIComponent(name.value.trim()), { value: value.value, description: descr.value });
          toast(existing ? 'Rotated. New runs use the new value.' : 'Saved', 'ok'); close(); load();
        } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); save.disabled = false; }
      } }, existing ? 'Rotate' : 'Save secret');
      return h('div', { class: 'space-y-4' },
        field('Name', name, 'Letters, digits and underscores. Steps list this name; it becomes the environment variable.'),
        field('Value', value, 'At least 8 characters, one line. It cannot be read back after you save.'), field('Description', descr), err,
        h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), save));
    });
  }

  async function remove(s) {
    const used = s.used_by.length ? ` It is still requested by ${s.used_by.join(', ')}, whose runs will fail until it is set again.` : '';
    if (!await confirmDialog({ title: 'Delete ' + s.name + '?', body: 'The encrypted value is deleted for good.' + used, confirmLabel: 'Delete secret', danger: true, typeToConfirm: s.name })) return;
    try { await del('/v1/secrets/' + encodeURIComponent(s.name)); toast('Deleted', 'ok'); load(); } catch (e) { toast(e.message, 'error'); }
  }

  await load();
  return el;
}
