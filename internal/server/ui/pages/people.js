import { h, icon, mount, pageHeader, relTime, absTime, spinner, errorBox, toast, modal, confirmDialog, field, notice } from '../lib/dom.js';
import { get, post, patch, del } from '../lib/api.js';
import { session } from '../app.js';

const ROLES = { viewer: 'Viewer: can look, cannot change anything', developer: 'Developer: runs and edits pipelines, proposes cloud changes', admin: 'Admin: everything, including people, secrets and approvals' };

export async function peoplePage() {
  const el = h('div', {});
  mount(el, spinner());

  async function load() {
    let users;
    try { users = (await get('/v1/users')).users; } catch (e) { return mount(el, errorBox(e.message)); }
    const me = session.name.toLowerCase();
    const rows = users.map(u => {
      const self = u.id === me;
      const role = h('select', { class: 'input w-36 py-1.5', 'aria-label': 'Role of ' + u.username, onchange: async (e) => {
        try { await patch('/v1/users/' + encodeURIComponent(u.id), { role: e.target.value }); toast(u.username + ' is now ' + e.target.value, 'ok'); load(); } catch (ex) { toast(ex.message, 'error'); load(); }
      } }, Object.keys(ROLES).map(r => h('option', { value: r, selected: r === u.role }, r)));
      return h('tr', {},
        h('td', {}, h('div', { class: 'flex items-center gap-3' }, h('span', { class: 'flex size-8 items-center justify-center rounded-full bg-slate-200 text-xs font-semibold dark:bg-slate-700' }, u.username.slice(0, 1).toUpperCase()),
          h('div', {}, h('div', { class: 'font-medium' }, u.name || u.username, self && h('span', { class: 'pill-idle ml-2' }, 'you')), h('div', { class: 'muted text-xs' }, u.username)))),
        h('td', {}, role),
        h('td', {}, u.disabled ? h('span', { class: 'pill-bad' }, 'Disabled') : h('span', { class: 'pill-ok' }, 'Active')),
        h('td', { class: 'muted whitespace-nowrap text-sm', title: u.last_login_at ? absTime(u.last_login_at) : '' }, u.last_login_at ? relTime(u.last_login_at) : 'never'),
        h('td', { class: 'whitespace-nowrap text-right' },
          h('button', { class: 'btn-secondary btn-sm mr-2', type: 'button', onclick: () => resetPw(u) }, icon('key', 'size-3.5'), 'Reset password'),
          h('button', { class: 'btn-secondary btn-sm mr-2', type: 'button', onclick: async () => {
            try { await patch('/v1/users/' + encodeURIComponent(u.id), { disabled: !u.disabled }); toast(u.disabled ? 'Enabled' : 'Disabled and signed out', 'ok'); load(); } catch (ex) { toast(ex.message, 'error'); }
          } }, u.disabled ? 'Enable' : 'Disable'),
          h('button', { class: 'btn-ghost btn-sm text-rose-600 dark:text-rose-400', type: 'button', 'aria-label': 'Delete ' + u.username, onclick: async () => {
            if (!await confirmDialog({ title: 'Delete ' + u.username + '?', body: 'Their account is removed and they are signed out. Runs they started stay in the history.', confirmLabel: 'Delete', danger: true, typeToConfirm: u.username })) return;
            try { await del('/v1/users/' + encodeURIComponent(u.id)); toast('Deleted', 'ok'); load(); } catch (ex) { toast(ex.message, 'error'); }
          } }, icon('trash', 'size-4'))));
    });
    mount(el,
      pageHeader('People', 'Who can sign in, and what they can do.', h('button', { class: 'btn-primary', type: 'button', onclick: create }, icon('plus'), 'Add person')),
      h('div', { class: 'card overflow-x-auto' }, h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Person', 'Role', 'Status', 'Last sign-in', ''].map(x => h('th', {}, x)))), h('tbody', {}, rows))),
      h('div', { class: 'mt-4' }, notice('info', 'The server always keeps at least one active administrator. Disabling someone or resetting their password signs them out everywhere.')),
      h('div', { class: 'card card-pad mt-6' }, h('h2', { class: 'h-section mb-3' }, 'What each role can do'), h('dl', { class: 'space-y-2 text-sm' }, Object.entries(ROLES).map(([k, v]) => h('div', { class: 'flex gap-2' }, h('dt', { class: 'w-24 font-medium capitalize' }, k), h('dd', { class: 'muted' }, v.split(': ')[1]))))));
  }

  function create() {
    modal('Add a person', (close) => {
      const user = h('input', { id: 'nu', class: 'input', autocomplete: 'off', placeholder: 'grace' });
      const name = h('input', { id: 'nn', class: 'input', autocomplete: 'off', placeholder: 'Grace Hopper (optional)' });
      const pw = h('input', { id: 'np', class: 'input', type: 'password', autocomplete: 'new-password' });
      const role = h('select', { id: 'nr', class: 'input' }, Object.keys(ROLES).map(r => h('option', { value: r, selected: r === 'developer' }, r)));
      const err = h('div', { class: 'hidden' });
      const save = h('button', { class: 'btn-primary', type: 'button', onclick: async () => {
        err.classList.add('hidden'); save.disabled = true;
        try { await post('/v1/users', { username: user.value.trim(), name: name.value, password: pw.value, role: role.value }); toast('Added ' + user.value.trim(), 'ok'); close(); load(); }
        catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); save.disabled = false; }
      } }, 'Add person');
      return h('div', { class: 'space-y-4' }, field('Username', user, 'Letters, digits and . _ - @'), field('Name', name), field('Role', role), field('Temporary password', pw, 'At least 12 characters. Tell them to change it after signing in.'), err,
        h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), save));
    });
  }

  function resetPw(u) {
    modal('Reset password for ' + u.username, (close) => {
      const pw = h('input', { id: 'rp', class: 'input', type: 'password', autocomplete: 'new-password' });
      const err = h('div', { class: 'hidden' });
      const save = h('button', { class: 'btn-primary', type: 'button', onclick: async () => {
        err.classList.add('hidden'); save.disabled = true;
        try { await patch('/v1/users/' + encodeURIComponent(u.id), { password: pw.value }); toast('Password reset; they were signed out', 'ok'); close(); load(); }
        catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); save.disabled = false; }
      } }, 'Reset password');
      return h('div', { class: 'space-y-4' }, field('New password', pw, 'At least 12 characters.'), err, h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), save));
    });
  }

  await load();
  return el;
}
