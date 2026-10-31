import { h, mount, pageHeader, errorBox, toast, field, relTime, absTime, copyButton, confirmDialog } from '../lib/dom.js';
import { get, post, del, atLeast } from '../lib/api.js';
import { session } from '../app.js';

export async function accountPage() {
  const cur = h('input', { id: 'cur', class: 'input', type: 'password', autocomplete: 'current-password' });
  const next = h('input', { id: 'new', class: 'input', type: 'password', autocomplete: 'new-password' });
  const again = h('input', { id: 'again', class: 'input', type: 'password', autocomplete: 'new-password' });
  const err = h('div', { class: 'hidden' });
  const btn = h('button', { class: 'btn-primary', type: 'submit' }, 'Change password');
  return h('div', { class: 'max-w-3xl space-y-6' }, pageHeader('Account', session.name + ' · ' + session.role),
    h('form', { class: 'card card-pad max-w-lg space-y-4', onsubmit: async (e) => {
      e.preventDefault(); err.classList.add('hidden');
      if (next.value !== again.value) { mount(err, errorBox('The two new passwords are different.')); err.classList.remove('hidden'); return; }
      btn.disabled = true;
      try { await post('/v1/me/password', { current: cur.value, new: next.value }); toast('Password changed. Please sign in again.', 'ok'); location.reload(); }
      catch (ex) { mount(err, errorBox(ex.message)); err.classList.remove('hidden'); btn.disabled = false; }
    } }, h('h2', { class: 'text-base font-semibold' }, 'Change password'), field('Current password', cur), field('New password', next, 'At least 12 characters. You will be signed out everywhere.'), field('Repeat the new password', again), err, btn),
    await tokensCard());
}

// Personal API tokens, for AI tools that connect to Forgeline over MCP (note 020).
async function tokensCard() {
  const el = h('section', { class: 'card card-pad space-y-4', id: 'api-tokens', 'aria-labelledby': 'tokens-title' });
  const roles = ['viewer', 'developer', 'admin'].filter(r => atLeast(session.role, r));
  async function draw(fresh) {
    let list;
    try { list = (await get('/v1/tokens')).tokens; } catch (e) { return mount(el, errorBox(e.message)); }
    const name = h('input', { id: 'token-name', class: 'input', maxlength: '60', placeholder: 'Claude Code on my laptop' });
    const role = h('select', { id: 'token-role', class: 'input w-auto' }, roles.map(r => h('option', { value: r, selected: r === session.role }, r)));
    const days = h('select', { id: 'token-days', class: 'input w-auto' }, [[30, '30 days'], [90, '90 days'], [365, '1 year']].map(([v, l]) => h('option', { value: String(v), selected: v === 90 }, l)));
    const err = h('div', { class: 'hidden' });
    const origin = location.origin;
    mount(el,
      h('div', {}, h('h2', { id: 'tokens-title', class: 'text-base font-semibold' }, 'API tokens'),
        h('p', { class: 'muted text-sm' }, 'Let an AI tool such as Claude Code read what Forgeline shows you and file proposals for others to approve. A token acts as you, never with more than the role you give it, and works only on the MCP endpoint.')),
      fresh && h('div', { id: 'new-token', class: 'space-y-2 rounded-lg border border-emerald-300 bg-emerald-50 p-3 dark:border-emerald-800 dark:bg-emerald-950/30' },
        h('p', { class: 'text-sm font-medium' }, 'Copy it now. It is not shown again.'),
        h('div', { class: 'flex items-center gap-2' }, h('code', { id: 'new-token-value', class: 'break-all rounded bg-white px-2 py-1 font-mono text-xs dark:bg-slate-900' }, fresh.token), copyButton(fresh.token)),
        h('p', { class: 'muted text-xs' }, 'Claude Code:'),
        h('pre', { class: 'overflow-x-auto rounded bg-slate-950 p-2 font-mono text-xs text-slate-100' }, `claude mcp add --transport http forgeline ${origin}/mcp --header "Authorization: Bearer ${fresh.token}"`),
        h('p', { class: 'muted text-xs' }, 'A client that only speaks stdio:'),
        h('pre', { class: 'overflow-x-auto rounded bg-slate-950 p-2 font-mono text-xs text-slate-100' }, `FORGELINE_MCP_TOKEN=${fresh.token} forgeline mcp --server ${origin}`)),
      list.length ? h('div', { class: 'overflow-x-auto' }, h('table', { class: 'tbl', id: 'token-list' }, h('thead', {}, h('tr', {}, ['Name', 'Role', 'Expires', 'Last used', ''].map(x => h('th', {}, x)))),
        h('tbody', {}, list.map(t => h('tr', {}, h('td', { class: 'font-medium' }, t.name), h('td', {}, t.role), h('td', { class: 'muted text-sm', title: absTime(t.expires_at) }, relTime(t.expires_at)),
          h('td', { class: 'muted text-sm' }, t.last_used ? relTime(t.last_used) : 'never'),
          h('td', { class: 'text-right' }, h('button', { class: 'btn-ghost btn-sm text-rose-600 dark:text-rose-400', type: 'button', 'data-revoke': t.id, onclick: async () => {
            if (!await confirmDialog({ title: 'Revoke ' + t.name + '?', body: 'Anything using it stops working at once.', confirmLabel: 'Revoke', danger: true })) return;
            try { await del('/v1/tokens/' + encodeURIComponent(t.id)); toast('Revoked', 'ok'); draw(); } catch (e) { toast(e.message, 'error'); }
          } }, 'Revoke'))))))) : null,
      h('form', { class: 'flex flex-wrap items-end gap-3', onsubmit: async (e) => {
        e.preventDefault(); err.classList.add('hidden');
        try { const t = await post('/v1/tokens', { name: name.value, role: role.value, days: Number(days.value) }); draw(t); }
        catch (ex) { mount(err, errorBox(ex.message)); err.classList.remove('hidden'); }
      } }, h('div', { class: 'min-w-56 flex-1' }, field('Name', name, null, 'token-name')), field('Role', role, null, 'token-role'), field('Lasts', days, null, 'token-days'),
        h('button', { class: 'btn-primary', type: 'submit', id: 'token-create' }, 'Create token')), err);
  }
  await draw();
  return el;
}
