import { h, mount, pageHeader, errorBox, toast, field } from '../lib/dom.js';
import { post } from '../lib/api.js';
import { session } from '../app.js';

export async function accountPage() {
  const cur = h('input', { id: 'cur', class: 'input', type: 'password', autocomplete: 'current-password' });
  const next = h('input', { id: 'new', class: 'input', type: 'password', autocomplete: 'new-password' });
  const again = h('input', { id: 'again', class: 'input', type: 'password', autocomplete: 'new-password' });
  const err = h('div', { class: 'hidden' });
  const btn = h('button', { class: 'btn-primary', type: 'submit' }, 'Change password');
  return h('div', { class: 'max-w-lg' }, pageHeader('Account', session.name + ' · ' + session.role),
    h('form', { class: 'card card-pad space-y-4', onsubmit: async (e) => {
      e.preventDefault(); err.classList.add('hidden');
      if (next.value !== again.value) { mount(err, errorBox('The two new passwords are different.')); err.classList.remove('hidden'); return; }
      btn.disabled = true;
      try { await post('/v1/me/password', { current: cur.value, new: next.value }); toast('Password changed. Please sign in again.', 'ok'); location.reload(); }
      catch (ex) { mount(err, errorBox(ex.message)); err.classList.remove('hidden'); btn.disabled = false; }
    } }, h('h2', { class: 'text-base font-semibold' }, 'Change password'), field('Current password', cur), field('New password', next, 'At least 12 characters. You will be signed out everywhere.'), field('Repeat the new password', again), err, btn));
}
