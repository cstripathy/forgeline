import { h, mount, icon, errorBox, field } from '../lib/dom.js';
import { post } from '../lib/api.js';

function frame(title, subtitle, body) {
  return h('div', { class: 'flex min-h-screen items-center justify-center px-4 py-10' },
    h('div', { class: 'w-full max-w-md' },
      h('div', { class: 'mb-6 flex flex-col items-center text-center' },
        h('img', { src: '/ui/favicon.svg', alt: '', class: 'mb-3 size-12' }),
        h('h1', { class: 'text-2xl font-semibold tracking-tight' }, title), h('p', { class: 'muted mt-1 text-sm' }, subtitle)),
      h('div', { class: 'card card-pad' }, body),
      h('p', { class: 'muted mt-6 text-center text-xs' }, 'Forgeline · delivery where AI agents run safely')));
}

function form(fields, submitLabel, onSubmit) {
  const err = h('div', { class: 'hidden', 'aria-live': 'polite' });
  const btn = h('button', { class: 'btn-primary w-full', type: 'submit' }, submitLabel);
  const f = h('form', { class: 'space-y-4', novalidate: true, onsubmit: async (e) => {
    e.preventDefault();
    err.classList.add('hidden'); btn.disabled = true;
    try { await onSubmit(); } catch (ex) {
      mount(err, errorBox(ex.message)); err.classList.remove('hidden'); btn.disabled = false;
    }
  } }, fields, err, btn);
  return f;
}

export function loginPage(root, done) {
  const user = h('input', { id: 'username', class: 'input', autocomplete: 'username', autofocus: true, required: true });
  const pass = h('input', { id: 'password', class: 'input', type: 'password', autocomplete: 'current-password', required: true });
  mount(root, frame('Sign in to Forgeline', 'Use your account to continue.', form([
    field('Username', user), field('Password', pass),
  ], 'Sign in', async () => {
    await post('/v1/auth/login', { username: user.value, password: pass.value });
    done();
  })));
  user.focus();
}

export function setupPage(root, done) {
  // `forgeline server` prints a link ending in #token=...; use it, then drop it from the address bar.
  const fromLink = /^#token=([A-Za-z0-9_-]+)$/.exec(location.hash);
  if (fromLink) history.replaceState(null, '', location.pathname + location.search);
  const token = h('input', { id: 'token', class: 'input font-mono', type: 'password', autocomplete: 'off', required: true, value: fromLink ? fromLink[1] : '' });
  const user = h('input', { id: 'username', class: 'input', autocomplete: 'username', required: true, value: 'admin' });
  const name = h('input', { id: 'name', class: 'input', autocomplete: 'name' });
  const pass = h('input', { id: 'password', class: 'input', type: 'password', autocomplete: 'new-password', required: true });
  const pass2 = h('input', { id: 'password2', class: 'input', type: 'password', autocomplete: 'new-password', required: true });
  mount(root, frame('Welcome to Forgeline', 'Create the first administrator to finish setting up this server.', h('div', { class: 'space-y-4' },
    h('div', { class: 'flex items-start gap-2 rounded-lg bg-brand-50 px-3 py-2.5 text-sm text-brand-900 dark:bg-brand-500/10 dark:text-brand-100' },
      icon('shield', 'mt-0.5 size-4'), h('div', {}, 'To prove you control this server, paste its token. It is the value of ', h('code', { class: 'font-mono text-xs' }, 'FORGELINE_TOKEN'), ', or the one printed when the server started.')),
    form([
      field('Server token', token), field('Username', user), field('Your name (optional)', name),
      field('Password', pass, 'At least 12 characters.'), field('Repeat the password', pass2),
    ], 'Create administrator', async () => {
      if (pass.value !== pass2.value) throw new Error('The two passwords are different.');
      await post('/v1/auth/setup', { token: token.value.trim(), username: user.value, name: name.value, password: pass.value });
      done();
    }))));
  (fromLink ? user : token).focus();
}
