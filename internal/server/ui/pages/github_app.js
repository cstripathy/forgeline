import { h, icon, mount, spinner, errorBox, notice, toast, field } from '../lib/dom.js';
import { get, post } from '../lib/api.js';

/**
 * "Connect GitHub" with a GitHub App: create the app with one click, install it, choose
 * repositories. Nothing is copied by hand; GitHub hands Forgeline the app's key and webhook secret.
 */
export function githubAppCard(gh, reload) {
  const box = h('div', { class: 'card card-pad mb-4', id: 'gh-app-card' });
  const flash = new URLSearchParams((location.hash.split('?')[1] || '')).get('app');
  if (flash === 'created') toast('GitHub App created. Now install it.', 'ok');
  else if (flash === 'failed') toast('GitHub did not finish creating the app. Try again.', 'error');
  else if (flash === 'expired') toast('That setup link expired. Start again.', 'error');
  if (flash) history.replaceState(null, '', '#/integrations');

  const title = (badge) => h('div', { class: 'mb-3 flex flex-wrap items-center gap-3' },
    h('h2', { class: 'flex items-center gap-2 text-base font-semibold' }, icon('git', 'size-5'), 'Connect with a GitHub App'), badge);

  if (!gh.app) { mount(box, create()); return box; }
  if (!gh.app.installation_id) { install(); return box; }
  mount(box, title(h('span', { class: 'pill-ok' }, 'Connected')),
    h('p', { class: 'text-sm' }, 'Using the app ', h('a', { class: 'font-medium underline', href: gh.app.url || '#', target: '_blank', rel: 'noopener noreferrer' }, gh.app.slug),
      ' (installation ', h('span', { class: 'font-mono' }, String(gh.app.installation_id)), '). Forgeline signs in with short-lived tokens; there is no personal access token to rotate. Change the repositories below.'));
  return box;

  function create() {
    const pub = h('input', { id: 'app-public', class: 'input', value: gh.public_url || '', placeholder: location.origin, autocomplete: 'off' });
    const owner = h('input', { id: 'app-owner', class: 'input', placeholder: 'leave empty for your own account' });
    const name = h('input', { id: 'app-name', class: 'input', maxlength: '34', placeholder: 'Forgeline (a short random suffix is added)' });
    const api = h('input', { id: 'app-api', class: 'input font-mono', value: gh.api_base || 'https://api.github.com' });
    const err = h('div', { class: 'hidden' });
    const go = h('button', { class: 'btn-primary', id: 'app-create', type: 'button', onclick: async () => {
      err.classList.add('hidden'); go.disabled = true;
      try {
        const r = await post('/v1/integrations/github/app/start', { public_url: pub.value, owner: owner.value, name: name.value, api_base: api.value === 'https://api.github.com' ? '' : api.value });
        location.href = r.launch_url;
      } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; }
    } }, icon('plug', 'size-4'), 'Create the GitHub App');
    return h('div', {},
      title(h('span', { class: 'pill-idle' }, 'Recommended')),
      h('p', { class: 'muted mb-4 text-sm' }, 'One click creates a private GitHub App with read access to repository contents and write access to commit statuses, and sets up the webhook. You approve once on GitHub; no token or secret is typed or copied.'),
      h('div', { class: 'grid gap-4 md:grid-cols-2' },
        field('This server\'s public address', pub, 'Where GitHub can reach this server over https, for the webhook. Leave it as is on a laptop; the webhook is then created switched off.'),
        field('Organisation (optional)', owner, 'Make an organisation own the app instead of you.')),
      h('div', { class: 'mt-4 grid gap-4 md:grid-cols-2' }, field('App name (optional)', name), field('GitHub API address', api, 'Change this only for GitHub Enterprise Server.')),
      err, h('div', { class: 'mt-5' }, go));
  }

  async function install() {
    mount(box, title(h('span', { class: 'pill-wait' }, 'Step 2 of 2: install')), spinner());
    let data;
    try { data = await get('/v1/integrations/github/app/installations'); } catch (e) { return mount(box, title(h('span', { class: 'pill-bad' }, 'Needs attention')), errorBox(e.message)); }
    const list = data.installations;
    const sel = h('select', { id: 'app-installation', class: 'input' }, list.map(i => h('option', { value: String(i.id) }, i.account + ' (' + i.repository_selection + ' repositories)')));
    const repoBox = h('div', { class: 'mt-3 space-y-2' });
    const err = h('div', { class: 'hidden mt-3' });
    let repos = [];
    const picks = new Map(); // full_name -> checkbox
    async function loadRepos() {
      mount(repoBox, spinner()); picks.clear();
      try { repos = (await get('/v1/integrations/github/app/repos?installation_id=' + encodeURIComponent(sel.value))).repositories; } catch (e) { return mount(repoBox, errorBox(e.message)); }
      if (!repos.length) return mount(repoBox, notice('warn', 'The app can see no repositories yet. Open the installation on GitHub and give it some.'));
      mount(repoBox, h('div', { class: 'muted text-sm' }, 'Repositories that may start runs (from their default branch):'),
        repos.map(r => { const box = h('input', { type: 'checkbox', class: 'size-4 rounded', value: r.full_name }); picks.set(r.full_name, box); return h('label', { class: 'flex items-center gap-2 text-sm' }, box, h('span', { class: 'font-mono' }, r.full_name), r.private && h('span', { class: 'pill-idle' }, 'private')); }));
    }
    const go = h('button', { class: 'btn-primary', id: 'app-finish', type: 'button', onclick: async () => {
      err.classList.add('hidden'); go.disabled = true;
      const rules = repos.filter(r => picks.get(r.full_name)?.checked).map(r => ({ repo: r.full_name, branches: [r.default_branch || 'main'] }));
      try { await post('/v1/integrations/github/app/install', { installation_id: Number(sel.value), rules, public_url: gh.public_url || '' }); toast(rules.length ? 'GitHub connected' : 'Installation saved. Pick repositories below.', 'ok'); reload(); }
      catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; }
    } }, 'Save and turn on');
    mount(box, title(h('span', { class: 'pill-wait' }, 'Step 2 of 2: install')),
      h('p', { class: 'muted mb-3 text-sm' }, 'The app exists. Install it on the account or organisation, choosing the repositories it may see.'),
      h('div', { class: 'mb-3 flex flex-wrap gap-2' },
        h('a', { id: 'app-install-link', class: 'btn-primary', href: data.install_url, target: '_blank', rel: 'noopener noreferrer' }, 'Open GitHub to install'),
        h('button', { class: 'btn-secondary', type: 'button', onclick: install }, icon('refresh', 'size-4'), 'I installed it: check again')),
      list.length ? h('div', {}, field('Installation', sel), repoBox, err, h('div', { class: 'mt-4' }, go))
        : notice('info', 'Not installed yet. After installing on GitHub, choose "check again".'));
    if (list.length) { sel.addEventListener('change', loadRepos); loadRepos(); }
  }
}
