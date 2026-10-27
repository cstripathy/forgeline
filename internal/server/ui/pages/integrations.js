import { h, icon, mount, pageHeader, relTime, absTime, spinner, errorBox, notice, toast, modal, confirmDialog, field, copyButton, emptyState } from '../lib/dom.js';
import { get, put, post, del, atLeast } from '../lib/api.js';
import { session } from '../app.js';

const OUTCOME = { ok: 'pill-ok', 'run started': 'pill-ok', accepted: 'pill-run', ignored: 'pill-idle', rejected: 'pill-bad', failed: 'pill-bad', busy: 'pill-wait' };

export async function integrationsPage() {
  const el = h('div', {});
  const admin = atLeast(session.role, 'admin');
  mount(el, spinner());
  let rules = [];

  async function load() {
    let gh;
    try { gh = await get('/v1/integrations/github'); } catch (e) { return mount(el, errorBox(e.message)); }
    rules = gh.rules.map(r => ({ repo: r.repo, branches: r.branches.join(', ') }));
    if (!rules.length) rules.push({ repo: '', branches: 'main' });
    render(gh);
  }

  function render(gh) {
    const enabled = h('input', { id: 'gh-on', type: 'checkbox', class: 'size-4 rounded', checked: gh.configured ? gh.enabled : true, disabled: !admin });
    const api = h('input', { id: 'gh-api', class: 'input', value: gh.api_base, disabled: !admin });
    const pub = h('input', { id: 'gh-pub', class: 'input', value: gh.public_url, placeholder: 'https://ci.example.com', disabled: !admin });
    const file = h('input', { id: 'gh-file', class: 'input', value: gh.pipeline_file, disabled: !admin });
    const token = h('input', { id: 'gh-token', class: 'input font-mono', type: 'password', autocomplete: 'new-password', placeholder: gh.token_set ? 'A token is saved. Type to replace it.' : 'github_pat_…', disabled: !admin });
    const secretMode = h('select', { id: 'gh-secret-mode', class: 'input', disabled: !admin },
      gh.webhook_secret_set && h('option', { value: 'keep' }, 'Keep the saved secret'),
      h('option', { value: 'generate', selected: !gh.webhook_secret_set }, 'Generate a new one (shown once)'), h('option', { value: 'own' }, 'I will type my own'));
    const secretIn = h('input', { id: 'gh-secret', class: 'input hidden font-mono', type: 'password', autocomplete: 'new-password', placeholder: 'At least 16 characters' });
    secretMode.addEventListener('change', () => secretIn.classList.toggle('hidden', secretMode.value !== 'own'));
    const err = h('div', { class: 'hidden mt-4' });
    const rulesBox = h('div', { class: 'space-y-2' });
    const paintRules = () => mount(rulesBox, rules.map((r, i) => h('div', { class: 'flex flex-wrap items-center gap-2' },
      h('input', { class: 'input min-w-48 flex-1 font-mono', placeholder: 'owner/repository', 'aria-label': 'Repository ' + (i + 1), value: r.repo, disabled: !admin, oninput: (e) => { r.repo = e.target.value; } }),
      h('input', { class: 'input min-w-48 flex-1 font-mono', placeholder: 'main, release/*', 'aria-label': 'Branches for repository ' + (i + 1), value: r.branches, disabled: !admin, oninput: (e) => { r.branches = e.target.value; } }),
      admin && h('button', { class: 'btn-ghost btn-sm', type: 'button', 'aria-label': 'Remove repository ' + (i + 1), onclick: () => { rules.splice(i, 1); paintRules(); } }, icon('x')))));
    paintRules();

    const save = h('button', { class: 'btn-primary', type: 'submit', disabled: !admin }, icon('check', 'size-4'), 'Save');
    const test = h('button', { class: 'btn-secondary', type: 'button', disabled: !gh.active, onclick: runTest }, icon('bolt', 'size-4'), 'Test connection');
    const testOut = h('div', { class: 'mt-4' });

    async function runTest() {
      test.disabled = true; mount(testOut, spinner());
      try {
        const r = await post('/v1/integrations/github/test');
        mount(testOut, h('div', { class: 'card divide-y divide-slate-100 dark:divide-slate-800' }, r.results.map(c => h('div', { class: 'flex items-start gap-3 px-4 py-3 text-sm' },
          h('span', { class: c.ok ? 'pill-ok' : 'pill-bad' }, icon(c.ok ? 'check' : 'x', 'size-3'), c.ok ? 'OK' : 'Failed'), h('div', {}, h('div', { class: 'font-mono font-medium' }, c.repo), h('div', { class: 'muted' }, c.message))))));
      } catch (e) { mount(testOut, errorBox(e.message)); }
      test.disabled = false;
    }

    const form = h('form', { class: 'card card-pad', novalidate: true, onsubmit: async (ev) => {
      ev.preventDefault(); err.classList.add('hidden'); save.disabled = true;
      const body = {
        enabled: enabled.checked, api_base: api.value, public_url: pub.value, pipeline_file: file.value,
        rules: rules.filter(r => r.repo.trim()).map(r => ({ repo: r.repo.trim(), branches: r.branches.split(',').map(b => b.trim()).filter(Boolean) })),
        token: token.value, generate_webhook_secret: secretMode.value === 'generate', webhook_secret: secretMode.value === 'own' ? secretIn.value : '',
      };
      try {
        const out = await put('/v1/integrations/github', body);
        toast('GitHub settings saved', 'ok');
        if (out.generated_webhook_secret) await showSecret(out);
        load();
      } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); save.disabled = false; }
    } },
      h('div', { class: 'mb-5 flex items-center justify-between' }, h('h2', { class: 'text-base font-semibold' }, 'Connection'),
        h('label', { class: 'flex items-center gap-2 text-sm font-medium', for: 'gh-on' }, enabled, 'Enabled')),
      h('div', { class: 'space-y-5' },
        field('Repositories and branches that may start runs', h('div', {}, rulesBox, admin && h('button', { class: 'btn-secondary btn-sm mt-2', type: 'button', onclick: () => { rules.push({ repo: '', branches: 'main' }); paintRules(); } }, icon('plus', 'size-3.5'), 'Add repository')),
          'Only these can trigger a run. Branch names may use * (it does not cross a /). Pull requests from forks are always ignored.'),
        h('div', { class: 'grid gap-5 md:grid-cols-2' },
          field('Access token', token, 'A fine-grained token with Contents: read and Commit statuses: write on those repositories. It is stored encrypted and cannot be read back.'),
          field('Webhook secret', h('div', { class: 'space-y-2' }, secretMode, secretIn), 'GitHub signs every delivery with it; Forgeline refuses unsigned ones.')),
        h('div', { class: 'grid gap-5 md:grid-cols-2' },
          field('This server\'s public address', pub, 'Where GitHub reaches this server (over HTTPS). Used for the webhook URL and the links on commit statuses.'),
          field('Pipeline file in each repository', file)),
        field('GitHub API address', api, 'Change this only for GitHub Enterprise Server.')),
      err, admin && h('div', { class: 'mt-6 flex flex-wrap items-center gap-2' }, save, test, gh.configured && gh.source === 'console' && h('button', { class: 'btn-ghost ml-auto text-rose-600 dark:text-rose-400', type: 'button', onclick: remove }, icon('trash', 'size-4'), 'Remove integration')), testOut);

    const hookUrl = gh.webhook_url;
    mount(el,
      pageHeader('Integrations', 'Connect Forgeline to the systems around it.'),
      h('div', { class: 'mb-4 flex flex-wrap items-center gap-3' },
        h('span', { class: 'flex items-center gap-2 text-lg font-semibold' }, icon('git', 'size-5'), 'GitHub'),
        gh.active ? h('span', { class: 'pill-ok' }, 'Active') : gh.problem ? h('span', { class: 'pill-bad' }, 'Needs attention') : h('span', { class: 'pill-idle' }, 'Not active'),
        gh.source === 'flags' && h('span', { class: 'pill-wait' }, 'Set by command-line flags')),
      gh.problem && h('div', { class: 'mb-4' }, errorBox(gh.problem)),
      gh.source === 'flags' && h('div', { class: 'mb-4' }, notice('warn', 'This is configured by ', h('code', { class: 'font-mono text-xs' }, '--github-allow'), ' and environment variables. Saving here replaces that configuration, and the flags are then ignored.')),
      !admin && h('div', { class: 'mb-4' }, notice('info', 'Only administrators can change integrations.')),
      h('div', { class: 'grid gap-4 xl:grid-cols-3' },
        h('div', { class: 'xl:col-span-2' }, form),
        h('div', { class: 'space-y-4' },
          h('div', { class: 'card card-pad' }, h('h2', { class: 'h-section mb-3' }, 'Set up the webhook in GitHub'),
            h('ol', { class: 'list-decimal space-y-2 pl-5 text-sm' }, h('li', {}, 'Repository → Settings → Webhooks → Add webhook.'),
              h('li', {}, 'Payload URL: ', h('code', { class: 'break-all font-mono text-xs' }, hookUrl)), h('li', {}, 'Content type: ', h('code', { class: 'font-mono text-xs' }, 'application/json')),
              h('li', {}, 'Secret: the one shown when you save.'), h('li', {}, 'Events: ', h('em', {}, 'Pushes'), ' and ', h('em', {}, 'Pull requests'), '.')),
            h('div', { class: 'mt-3' }, copyButton(hookUrl, 'Copy URL'))),
          h('div', { class: 'card' }, h('div', { class: 'border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('h2', { class: 'h-section' }, 'Recent deliveries')),
            gh.deliveries.length ? h('ul', { class: 'divide-y divide-slate-100 dark:divide-slate-800' }, gh.deliveries.slice(0, 12).map(d => h('li', { class: 'px-5 py-2.5 text-sm' },
              h('div', { class: 'flex items-center justify-between gap-2' }, h('span', { class: OUTCOME[d.outcome] || 'pill-idle' }, d.outcome), h('span', { class: 'muted text-xs', title: absTime(d.time) }, relTime(d.time))),
              h('div', { class: 'mt-1 text-xs' }, h('span', { class: 'font-medium' }, d.event), d.repo && h('span', { class: 'font-mono' }, ' ' + d.repo), d.sha && h('span', { class: 'font-mono muted' }, ' @' + d.sha.slice(0, 7))),
              d.detail && h('div', { class: 'muted text-xs' }, d.detail), d.run_id && h('a', { href: '#/runs/' + d.run_id, class: 'text-xs text-brand-600 hover:underline dark:text-brand-300' }, 'Open run')))
            ) : emptyState('plug', 'No deliveries yet', 'They appear here once GitHub starts sending events.')))));
  }

  function showSecret(out) {
    return modal('Your webhook secret', (close) => h('div', { class: 'space-y-4' },
      notice('warn', 'This is the only time it is shown. Paste it into the webhook\'s Secret field in GitHub now.'),
      h('pre', { class: 'break-all whitespace-pre-wrap rounded-lg bg-slate-950 p-3 font-mono text-xs text-emerald-300' }, out.generated_webhook_secret),
      h('div', { class: 'flex justify-between' }, copyButton(out.generated_webhook_secret, 'Copy secret'), h('button', { class: 'btn-primary', type: 'button', onclick: () => close() }, 'I have saved it'))));
  }

  async function remove() {
    if (!await confirmDialog({ title: 'Remove the GitHub integration?', body: 'Webhooks stop working at once and the stored token and webhook secret are deleted.', confirmLabel: 'Remove', danger: true })) return;
    try { await del('/v1/integrations/github'); toast('Removed', 'ok'); load(); } catch (e) { toast(e.message, 'error'); }
  }

  await load();
  return el;
}
