import { h, icon, mount, spinner, errorBox, notice, toast, field } from '../lib/dom.js';
import { get, put, post } from '../lib/api.js';

/** The assistant's settings card on the Integrations page. */
export async function assistantSettings(box, admin) {
  mount(box, spinner());
  let cfg;
  try { cfg = await get('/v1/assistant'); } catch (e) { return mount(box, errorBox(e.message)); }

  const backend = h('select', { id: 'as-backend', class: 'input', disabled: !admin },
    h('option', { value: 'off', selected: cfg.backend === 'off' }, 'Off'),
    h('option', { value: 'api', selected: cfg.backend === 'api' }, 'Anthropic API key (recommended for teams)'),
    h('option', { value: 'claude-token', selected: cfg.backend === 'claude-token' }, 'Claude Code login token (from claude setup-token)'),
    h('option', { value: 'cli', selected: cfg.backend === 'cli' }, 'The claude program on this server (one person only)'));
  const model = h('input', { id: 'as-model', class: 'input font-mono', value: cfg.model, disabled: !admin });
  const key = h('input', { id: 'as-key', class: 'input font-mono', type: 'password', autocomplete: 'new-password', placeholder: cfg.has_key ? 'A key is saved. Type to replace it.' : 'sk-ant-…', disabled: !admin });
  const keyField = field('API key', key, 'Stored encrypted and never shown again. Calls are billed to this key.');
  const tok = h('input', { id: 'as-token', class: 'input font-mono', type: 'password', autocomplete: 'new-password', placeholder: cfg.has_token ? 'A token is saved. Type to replace it.' : 'Paste the output of: claude setup-token', disabled: !admin });
  const tokField = field('Claude Code token', tok, 'Stored encrypted and never shown again. It is the same secret (CLAUDE_CODE_OAUTH_TOKEN) that Claude Code agent steps use. The program runs in a scratch home directory, so the server user\'s own login is not touched.');
  const cliWarn = notice('warn', 'This runs the ', h('code', { class: 'font-mono text-xs' }, 'claude'), ' program on the server as the server\'s own user, with that user\'s login, for everyone who can use the assistant. Use it only on a server for one person (a laptop). For a team, use an API key: sharing one personal subscription is not what it is licensed for.');
  const err = h('div', { class: 'mt-4 hidden' });
  const out = h('div', { class: 'mt-4' });
  const sync = () => { keyField.classList.toggle('hidden', backend.value !== 'api'); tokField.classList.toggle('hidden', backend.value !== 'claude-token'); cliWarn.classList.toggle('hidden', backend.value !== 'cli'); };
  backend.addEventListener('change', sync); sync();

  const save = h('button', { class: 'btn-primary', type: 'submit', disabled: !admin }, icon('check', 'size-4'), 'Save');
  const test = h('button', { class: 'btn-secondary', type: 'button', disabled: !cfg.ready, onclick: async () => {
    test.disabled = true; mount(out, spinner());
    try { const r = await post('/v1/integrations/assistant/test'); mount(out, notice('ok', 'It answered: ', r.reply)); }
    catch (e) { mount(out, errorBox(e.message)); }
    test.disabled = false;
  } }, icon('bolt', 'size-4'), 'Test');

  mount(box, h('form', { class: 'card card-pad', novalidate: true, onsubmit: async (ev) => {
    ev.preventDefault(); err.classList.add('hidden'); save.disabled = true;
    try {
      await put('/v1/integrations/assistant', { backend: backend.value, model: model.value, api_key: key.value, claude_token: tok.value });
      toast('Assistant settings saved', 'ok');
      assistantSettings(box, admin);
    } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); save.disabled = false; }
  } },
    h('div', { class: 'mb-5 flex items-center justify-between' }, h('h2', { class: 'flex items-center gap-2 text-base font-semibold' }, icon('sparkle'), 'Assistant'),
      cfg.ready ? h('span', { class: 'pill-ok' }, 'Ready') : h('span', { class: 'pill-idle' }, cfg.backend === 'off' ? 'Off' : 'Needs a key or token')),
    h('p', { class: 'muted mb-5 text-sm' }, 'A helper in the top bar that answers questions about the page you are on: why a run failed, what a pipeline does, what is running in AWS. It can only read, never sees secret values, and anything it drafts is text until you save it.'),
    h('div', { class: 'space-y-5' }, field('How it reaches Claude', backend), cliWarn, keyField, tokField, field('Model', model, 'Leave as is unless you know you need another.')),
    err, admin && h('div', { class: 'mt-6 flex items-center gap-2' }, save, test), out));
}
