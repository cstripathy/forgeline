import { h, icon, mount, pill, pageHeader, relTime, absTime, bytes, spinner, errorBox, notice, toast, modal, confirmDialog, field, emptyState, kpi } from '../lib/dom.js';
import { get, put, post, del, atLeast } from '../lib/api.js';
import { navigate } from '../lib/router.js';
import { session } from '../app.js';
import { ssoWizard } from './aws_sso.js';
import { explorer } from './aws_explorer.js';

const TABS = [['platform', 'Platform'], ['aws', 'AWS account'], ['changes', 'Cloud changes']];

export async function infraPage({ query }) {
  const tab = TABS.some(t => t[0] === query.tab) ? query.tab : 'platform';
  const el = h('div', {});
  const body = h('div', {});
  let timer = null, pending = 0;

  const tabs = h('div', { class: 'mb-6 flex gap-1 border-b border-slate-200 dark:border-slate-800', role: 'tablist' }, TABS.map(([k, label]) =>
    h('a', { href: '#/infrastructure?tab=' + k, role: 'tab', 'aria-selected': k === tab ? 'true' : 'false',
      class: '-mb-px flex items-center gap-2 border-b-2 px-4 py-2.5 text-sm font-medium ' + (k === tab ? 'border-brand-600 text-brand-700 dark:border-brand-400 dark:text-brand-200' : 'border-transparent text-slate-500 hover:text-slate-800 dark:text-slate-400 dark:hover:text-slate-200') },
      label, k === 'changes' && h('span', { id: 'pending-badge', class: 'pill-wait hidden' }))));

  mount(el, pageHeader('Infrastructure', 'The Forgeline server, the machines it runs steps on, and the cloud account under it.'), tabs, body);
  mount(body, spinner());
  const cleanup = await ({ platform, aws, changes })[tab](body);
  if (cleanup) el.cleanup = cleanup;
  refreshBadge();
  async function refreshBadge() {
    try {
      const { changes: cs } = await get('/v1/cloud/changes');
      const n = cs.filter(c => c.status === 'proposed').length;
      const b = el.querySelector('#pending-badge');
      if (b) { b.textContent = n + ' waiting'; b.classList.toggle('hidden', n === 0); }
    } catch { /* badge is optional */ }
  }
  return el;
}

// ---------------------------------------------------------------- platform

async function platform(body) {
  let timer;
  async function load() {
    let rt;
    try { rt = await get('/v1/infra/runtime'); } catch (e) { return mount(body, errorBox(e.message)); }
    const s = rt.server, d = rt.docker, sec = rt.security;
    const diskUsed = s.disk_total_bytes ? 1 - s.disk_free_bytes / s.disk_total_bytes : 0;
    const bar = (frac, bad) => { const f = h('div', { class: 'h-full rounded-full ' + (bad ? 'bg-rose-500' : 'bg-brand-500') }); f.style.width = Math.max(2, Math.round(frac * 100)) + '%'; return h('div', { class: 'h-2 w-full overflow-hidden rounded-full bg-slate-200 dark:bg-slate-800' }, f); };
    const row = (k, v) => h('div', { class: 'flex justify-between gap-4 py-1.5 text-sm' }, h('dt', { class: 'muted' }, k), h('dd', { class: 'min-w-0 break-all text-right font-medium' }, v));
    mount(body,
      h('div', { class: 'grid grid-cols-2 gap-4 lg:grid-cols-4' },
        kpi('Runs active', s.running, `of ${s.max_runs} worker${s.max_runs === 1 ? '' : 's'}`, s.running ? 'run' : null),
        kpi('Queued', s.queued, 'waiting for a worker'),
        kpi('Step containers', rt.steps.length, d.available ? 'running on this Docker host' : 'Docker is not reachable', d.available ? null : 'bad'),
        kpi('Uptime', uptime(s.uptime_seconds), 'since ' + new Date(s.started_at).toLocaleString())),
      h('div', { class: 'mt-6 grid gap-4 lg:grid-cols-2' },
        h('section', { class: 'card card-pad' }, h('h2', { class: 'h-section mb-2' }, 'Forgeline server'),
          h('dl', { class: 'divide-y divide-slate-100 dark:divide-slate-800' }, row('Version', 'v' + s.version), row('Platform', s.platform + ' · ' + s.go), row('Run history', s.store === 'postgres' ? 'PostgreSQL' : 'In memory (lost on restart)'),
            row('Steps in parallel per run', s.parallelism), row('Data directory', h('code', { class: 'font-mono text-xs' }, s.data_dir))),
          s.disk_total_bytes ? h('div', { class: 'mt-4' }, h('div', { class: 'mb-1.5 flex justify-between text-xs' }, h('span', { class: 'muted' }, 'Disk holding workspaces'), h('span', {}, `${bytes(s.disk_free_bytes)} free of ${bytes(s.disk_total_bytes)}`)), bar(diskUsed, diskUsed > 0.9)) : null),
        h('section', { class: 'card card-pad' }, h('div', { class: 'mb-2 flex items-center justify-between' }, h('h2', { class: 'h-section' }, 'Docker host'), d.available ? h('span', { class: 'pill-ok' }, 'Reachable') : h('span', { class: 'pill-bad' }, 'Not reachable')),
          d.available ? h('dl', { class: 'divide-y divide-slate-100 dark:divide-slate-800' }, row('Engine', d.version), row('Host', d.host + ' · ' + d.os), row('Resources', `${d.cpus} CPUs · ${bytes(d.memory_bytes)} memory`), row('Storage driver', d.storage_driver), row('Containers', `${d.containers_running} running of ${d.containers}`), row('Images', d.images))
            : errorBox(d.error || 'Docker did not answer. Steps cannot run until it does.'))),
      h('section', { class: 'card mt-4' }, h('div', { class: 'border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('h2', { class: 'h-section' }, 'Steps running now')),
        rt.steps.length ? h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Container', 'Image', 'State', 'Running for'].map(x => h('th', {}, x)))),
          h('tbody', {}, rt.steps.map(c => h('tr', {}, h('td', { class: 'font-mono text-xs' }, c.name), h('td', { class: 'font-mono text-xs' }, c.image), h('td', {}, c.status), h('td', {}, c.running_for)))))
          : emptyState('box', 'Nothing is running', 'Step containers appear here while a pipeline runs.')),
      h('section', { class: 'card card-pad mt-4' }, h('h2', { class: 'h-section mb-3' }, 'Security posture'),
        h('div', { class: 'grid gap-3 text-sm sm:grid-cols-4' },
          h('div', {}, h('div', { class: 'text-2xl font-semibold' }, sec.users), h('div', { class: 'muted' }, 'people (' + sec.admins + ' admin' + (sec.admins === 1 ? '' : 's') + ')')),
          h('div', {}, h('div', { class: 'text-2xl font-semibold' }, sec.vault_secrets), h('div', { class: 'muted' }, 'secrets in the vault')),
          h('div', {}, h('div', { class: 'text-2xl font-semibold' }, sec.env_secrets), h('div', { class: 'muted' }, 'secrets from the environment')),
          h('div', {}, h('div', { class: 'text-2xl font-semibold capitalize' }, sec.default_network), h('div', { class: 'muted' }, 'default step network'))),
        h('ul', { class: 'mt-4 space-y-2 text-sm' }, sec.notes.map(n => h('li', { class: 'flex gap-2' }, icon('info', 'mt-0.5 size-4 text-slate-400'), n)))));
  }
  await load();
  timer = setInterval(() => { if (!document.hidden) load(); }, 5000);
  return () => clearInterval(timer);
}

function uptime(sec) {
  if (sec < 60) return sec + 's';
  if (sec < 3600) return Math.floor(sec / 60) + 'm';
  if (sec < 86400) return Math.floor(sec / 3600) + 'h ' + Math.floor((sec % 3600) / 60) + 'm';
  return Math.floor(sec / 86400) + 'd ' + Math.floor((sec % 86400) / 3600) + 'h';
}

// ---------------------------------------------------------------- aws

const ACCOUNT_KEY = 'forgeline-aws-account';
const ENVS = [['', 'No label'], ['production', 'Production'], ['staging', 'Staging'], ['development', 'Development'], ['sandbox', 'Sandbox']];

async function aws(body) {
  const admin = atLeast(session.role, 'admin'), devRole = atLeast(session.role, 'developer');
  let account = 'primary', accounts = [];
  try { account = sessionStorage.getItem(ACCOUNT_KEY) || 'primary'; } catch { /* a blocked store only forgets the choice */ }
  // Changes are proposed for the primary account only; the other accounts are for reading.
  let dev = devRole;
  const q = (extra) => '?account=' + encodeURIComponent(account) + (extra || '');

  async function load(refresh) {
    let v;
    try { accounts = (await get('/v1/aws/accounts')).accounts; } catch { accounts = []; }
    if (account !== 'primary' && !accounts.some(a => a.id === account)) account = 'primary'; // it was removed
    dev = devRole && account === 'primary';
    try { v = await get('/v1/infra/aws' + q(refresh ? '&refresh=1' : '')); } catch (e) { return mount(body, errorBox(e.message)); }
    if (!v.enabled && account === 'primary') return mount(body, accounts.length ? h('div', { class: 'space-y-4' }, accountBar(), connectCard(null)) : connectCard(null));
    mount(body,
      accounts.length > 1 || admin ? accountBar() : null,
      account !== 'primary' && h('div', { class: 'mb-4' }, notice('info', 'This account is read-only here. The explorer, stacks, spend and the assistant can look at it; changes can be proposed for the primary account only.')),
      h('div', { class: 'mb-4 flex flex-wrap items-center justify-between gap-3' },
        v.identity ? h('div', { class: 'flex items-center gap-3 text-sm' }, h('span', { class: 'pill-ok' }, 'Connected'),
          h('span', {}, 'Account ', h('strong', { class: 'font-mono' }, v.identity.account)), h('span', { class: 'muted' }, 'region ' + v.region + (v.profile === 'forgeline-sso' ? ' · signed in from the console' : v.profile ? ' · profile ' + v.profile : '')),
          h('span', { class: 'muted hidden font-mono text-xs lg:inline' }, v.identity.arn)) : h('span', { class: 'pill-bad' }, 'Not reachable'),
        h('div', { class: 'flex gap-2' },
          h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: () => load(true) }, icon('refresh', 'size-3.5'), 'Refresh'),
          admin && account === 'primary' && h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: () => connectModal(v) }, 'Change connection'))),
      v.error && h('div', { class: 'mb-4' }, errorBox(v.error)),
      v.section_errors && Object.entries(v.section_errors).map(([k, m]) => h('div', { class: 'mb-4' }, notice('warn', h('strong', {}, k + ': '), m))),
      !v.error && h('div', { class: 'space-y-4' }, explorer({ dev: devRole, account }), stacksCard(v), instancesCard(v), costCard(), admin && account === 'primary' && mcpCard()));
  }

  function accountBar() {
    const cur = accounts.find(a => a.id === account);
    const sel = h('select', { id: 'aws-account', class: 'input w-auto min-w-48', 'aria-label': 'AWS account', onchange: (e) => {
      account = e.target.value;
      try { sessionStorage.setItem(ACCOUNT_KEY, account); } catch { /* fine */ }
      load(false);
    } }, accounts.map(a => h('option', { value: a.id, selected: a.id === account }, a.name + (a.env ? ' · ' + a.env : '') + (a.account_id ? ' · ' + a.account_id : ''))));
    return h('div', { class: 'mb-4 flex flex-wrap items-center gap-2' },
      accounts.length ? [h('label', { class: 'muted text-sm', for: 'aws-account' }, 'Account'), sel] : null,
      admin && h('button', { class: 'btn-secondary btn-sm', type: 'button', id: 'add-account', onclick: addAccountModal }, icon('plus', 'size-3.5'), 'Add account'),
      admin && cur && !cur.primary && h('button', { class: 'btn-ghost btn-sm text-rose-600 dark:text-rose-400', type: 'button', onclick: async () => {
        if (!await confirmDialog({ title: 'Remove ' + cur.name + '?', body: 'Forgeline stops showing it. Nothing in the AWS account changes.', confirmLabel: 'Remove', danger: true })) return;
        try { await del('/v1/aws/accounts/' + encodeURIComponent(cur.id)); toast('Removed', 'ok'); account = 'primary'; load(true); } catch (e) { toast(e.message, 'error'); }
      } }, icon('trash', 'size-3.5'), 'Remove'));
  }

  function addAccountModal() {
    modal('Add an AWS account', (close) => {
      const name = h('input', { id: 'acct-name', class: 'input', maxlength: '40', placeholder: 'Staging' });
      const profile = h('input', { id: 'acct-profile', class: 'input font-mono', placeholder: 'staging' });
      const region = h('input', { id: 'acct-region', class: 'input', value: 'ap-south-1' });
      const env = h('select', { id: 'acct-env', class: 'input' }, ENVS.map(([v, l]) => h('option', { value: v }, l)));
      const err = h('div', { class: 'hidden' });
      const go = h('button', { class: 'btn-primary', type: 'button', id: 'acct-save', onclick: async () => {
        err.classList.add('hidden'); go.disabled = true; go.textContent = 'Checking the profile…';
        try {
          const a = await post('/v1/aws/accounts', { name: name.value, profile: profile.value, region: region.value, env: env.value });
          account = a.id; try { sessionStorage.setItem(ACCOUNT_KEY, account); } catch { /* fine */ }
          toast('Added ' + a.name, 'ok'); close(); load(true);
        } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; go.textContent = 'Add account'; }
      } }, 'Add account');
      return h('div', { class: 'space-y-4' },
        notice('info', 'Read-only. Forgeline signs in with a profile that already exists in the server\'s AWS configuration (run aws sso login --profile <name> on the server first) and checks it before saving. Use a role with only read permissions where you can.'),
        h('div', { class: 'grid gap-4 sm:grid-cols-2' }, field('Name', name, null, 'acct-name'), field('Environment', env, null, 'acct-env'), field('Profile', profile, 'A profile from the server\'s AWS config.', 'acct-profile'), field('Region', region, null, 'acct-region')),
        err, h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go));
    }, { wide: true });
  }

  function mcpCard() {
    const box = h('section', { class: 'card card-pad', id: 'mcp-card' }, spinner());
    async function draw() {
      let m;
      try { m = await get('/v1/integrations/aws/mcp'); } catch (e) { return mount(box, errorBox(e.message)); }
      const err = h('div', { class: 'mt-3 hidden' });
      const toggle = h('button', { class: m.enabled ? 'btn-secondary btn-sm' : 'btn-primary btn-sm', type: 'button', onclick: async () => {
        toggle.disabled = true; toggle.textContent = m.enabled ? 'Turning off…' : 'Starting the proxy…';
        try { await put('/v1/integrations/aws/mcp', { enabled: !m.enabled }); toast(m.enabled ? 'Turned off' : 'AWS MCP Server is on', 'ok'); draw(); }
        catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); toggle.disabled = false; toggle.textContent = m.enabled ? 'Turn off' : 'Turn on'; }
      } }, m.enabled ? 'Turn off' : 'Turn on');
      mount(box, h('div', { class: 'flex flex-wrap items-start justify-between gap-3' },
        h('div', {}, h('div', { class: 'flex items-center gap-2' }, h('h2', { class: 'h-section' }, 'AWS MCP Server'), m.enabled ? h('span', { class: 'pill-ok' }, 'On') : h('span', { class: 'pill-idle' }, 'Off')),
          h('p', { class: 'muted mt-1 max-w-2xl text-sm' }, 'Lets the assistant read this account through AWS\'s managed MCP endpoint, and apply a change that a different administrator approved. The assistant itself can only read and file proposals (see Proposals).')),
        toggle),
        m.enabled && m.tools.length ? h('p', { class: 'muted mt-3 text-xs' }, 'Tools offered: ', h('span', { class: 'font-mono' }, m.tools.join(', '))) : null,
        h('p', { class: 'muted mt-1 text-xs' }, 'Through ', h('code', {}, m.proxy), ' to ', h('code', {}, m.endpoint)), err);
    }
    draw();
    return box;
  }

  function connectCard() {
    return h('div', { class: 'card card-pad mx-auto max-w-xl' }, h('div', { class: 'mb-3 flex items-center gap-2' }, icon('cloud', 'size-5'), h('h2', { class: 'text-base font-semibold' }, 'Connect an AWS account')),
      h('p', { class: 'muted mb-4 text-sm' }, 'Sign in with your AWS single sign-on, approving in your own browser, or use a profile or IAM role already set up on the server. Everything it shows is read-only; changes go through approval.'),
      admin ? h('button', { class: 'btn-primary', type: 'button', onclick: () => connectModal(null) }, icon('plug', 'size-4'), 'Connect') : notice('info', 'An administrator can connect an account.'));
  }

  function connectModal(cur) {
    modal('AWS connection', (close) => {
      const body = h('div', {});
      const tabBtn = (id, label) => h('button', { type: 'button', id: 'aws-tab-' + id, class: 'rounded-lg px-3 py-1.5 text-sm font-medium', onclick: () => pick(id) }, label);
      const tabs = { sso: tabBtn('sso', 'Sign in with AWS'), profile: tabBtn('profile', 'Server profile') };
      let stopWizard = null;
      function pick(which) {
        if (stopWizard) { stopWizard(); stopWizard = null; }
        for (const [k, el] of Object.entries(tabs)) el.className = 'rounded-lg px-3 py-1.5 text-sm font-medium ' + (k === which ? 'bg-brand-50 text-brand-700 dark:bg-brand-500/15 dark:text-brand-200' : 'text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800');
        if (which === 'sso') stopWizard = ssoWizard(body, { initialRegion: cur?.region, onDone: () => { close(); load(true); }, onCancel: () => close() });
        else mount(body, profileForm());
      }
      function profileForm() {
        const profile = h('input', { id: 'aws-profile', class: 'input', value: cur?.sso ? '' : cur?.profile || '', placeholder: 'default (leave empty to use the server role)' });
        const region = h('input', { id: 'aws-region', class: 'input', value: cur?.region || 'ap-south-1', placeholder: 'ap-south-1' });
        const err = h('div', { class: 'hidden' });
        const go = h('button', { class: 'btn-primary', type: 'button', onclick: async () => {
          err.classList.add('hidden'); go.disabled = true; go.textContent = 'Testing…';
          try { await put('/v1/integrations/aws', { enabled: true, profile: profile.value, region: region.value }); toast('Connected', 'ok'); close(); load(true); }
          catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; go.textContent = 'Test and save'; }
        } }, 'Test and save');
        return h('div', { class: 'space-y-4' }, field('Profile', profile, 'A profile from the server\'s AWS config. For SSO, run aws sso login --profile <name> on the server first. On a server with an IAM role, leave it empty.'), field('Region', region), err,
          h('div', { class: 'flex justify-end gap-2' }, cur && h('button', { class: 'btn-ghost mr-auto text-rose-600', type: 'button', onclick: async () => { await put('/v1/integrations/aws', { enabled: false, profile: '', region: '' }); close(); load(true); } }, 'Disconnect'),
            h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go));
      }
      queueMicrotask(() => pick('sso'));
      return h('div', { class: 'space-y-4' }, h('div', { class: 'flex gap-1', role: 'tablist' }, tabs.sso, tabs.profile), body);
    });
  }

  function stacksCard(v) {
    return h('section', { class: 'card' }, h('div', { class: 'flex items-center justify-between border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('h2', { class: 'h-section' }, 'CloudFormation stacks'),
      dev && h('button', { class: 'btn-primary btn-sm', type: 'button', onclick: () => deployModal(v) }, icon('plus', 'size-3.5'), 'Deploy the Forgeline demo')),
      v.stacks.length ? h('div', { class: 'overflow-x-auto' }, h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Stack', 'Status', 'Created', ''].map(x => h('th', {}, x)))),
        h('tbody', {}, v.stacks.map(s => h('tr', {}, h('td', {}, h('div', { class: 'font-medium' }, s.name), s.description && h('div', { class: 'muted max-w-md truncate text-xs' }, s.description)),
          h('td', {}, h('span', { class: /COMPLETE$/.test(s.status) && !/ROLLBACK|DELETE/.test(s.status) ? 'pill-ok' : /FAILED|ROLLBACK/.test(s.status) ? 'pill-bad' : 'pill-wait' }, s.status)),
          h('td', { class: 'muted whitespace-nowrap text-sm' }, relTime(s.created_at)),
          h('td', { class: 'text-right' }, dev && h('button', { class: 'btn-ghost btn-sm text-rose-600 dark:text-rose-400', type: 'button', onclick: () => proposeDelete(s) }, icon('trash', 'size-3.5'), 'Propose deletion'))))))) : emptyState('cloud', 'No stacks in this region', null));
  }

  function instancesCard(v) {
    const act = (i, action) => proposeInstance(i, action);
    return h('section', { class: 'card' }, h('div', { class: 'border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('h2', { class: 'h-section' }, 'EC2 instances')),
      v.instances.length ? h('div', { class: 'overflow-x-auto' }, h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Instance', 'State', 'Type', 'Stack', 'Address', ''].map(x => h('th', {}, x)))),
        h('tbody', {}, v.instances.map(i => h('tr', {}, h('td', {}, h('div', { class: 'font-medium' }, i.name || '(no name)'), h('div', { class: 'font-mono text-xs muted' }, i.id)),
          h('td', {}, h('span', { class: i.state === 'running' ? 'pill-ok' : i.state === 'stopped' ? 'pill-idle' : 'pill-wait' }, i.state)),
          h('td', { class: 'text-sm' }, i.type, h('span', { class: 'muted text-xs' }, ' ' + (i.architecture || ''))), h('td', { class: 'text-sm' }, i.stack || h('span', { class: 'muted' }, '–')),
          h('td', { class: 'font-mono text-xs' }, i.public_ip || i.private_ip || '–'),
          h('td', { class: 'whitespace-nowrap text-right' }, dev && [
            i.state === 'stopped' && h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: () => act(i, 'start') }, icon('play', 'size-3'), 'Start'),
            i.state === 'running' && h('button', { class: 'btn-secondary btn-sm mr-2', type: 'button', onclick: () => act(i, 'reboot') }, icon('refresh', 'size-3'), 'Reboot'),
            i.state === 'running' && h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: () => act(i, 'stop') }, icon('stop', 'size-3'), 'Stop')]))))))
        : emptyState('server', 'No instances in this region', null));
  }

  function costCard() {
    const out = h('div', {});
    const loadBtn = h('button', { class: 'btn-secondary btn-sm', type: 'button', disabled: !devRole, onclick: async () => {
      loadBtn.disabled = true; mount(out, spinner());
      try {
        const c = await get('/v1/infra/aws/cost' + q());
        if (!c.available) return mount(out, h('div', { class: 'p-5' }, errorBox(c.error || 'Cost data is not available.'), h('p', { class: 'muted mt-2 text-xs' }, 'Cost Explorer must be enabled on the account and the role allowed to call ce:GetCostAndUsage.')));
        const max = Math.max(...c.services.map(x => x.amount), 1);
        mount(out, h('div', { class: 'p-5' }, h('div', { class: 'mb-4 text-3xl font-semibold tabular-nums' }, c.unit + ' ' + c.total.toFixed(2), h('span', { class: 'muted ml-2 text-sm font-normal' }, c.from + ' to ' + c.to)),
          h('div', { class: 'space-y-2' }, c.services.slice(0, 10).map(x => { const f = h('div', { class: 'h-full rounded-full bg-brand-500' }); f.style.width = Math.max(2, Math.round(x.amount / max * 100)) + '%';
            return h('div', { class: 'grid grid-cols-[1fr_4rem] items-center gap-3 text-sm' }, h('div', {}, h('div', { class: 'mb-1 truncate' }, x.service), h('div', { class: 'h-1.5 overflow-hidden rounded-full bg-slate-200 dark:bg-slate-800' }, f)), h('div', { class: 'text-right tabular-nums' }, x.amount.toFixed(2))); }))));
      } catch (e) { mount(out, h('div', { class: 'p-5' }, errorBox(e.message))); }
      loadBtn.disabled = false;
    } }, icon('clock', 'size-3.5'), 'Load last 30 days');
    return h('section', { class: 'card' }, h('div', { class: 'flex items-center justify-between border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('div', {}, h('h2', { class: 'h-section' }, 'Spend'), h('p', { class: 'muted mt-0.5 text-xs' }, 'AWS bills each Cost Explorer request, so this loads only when you ask.')), loadBtn), out);
  }

  // ---- proposals

  function proposalForm(title, summary, build) {
    return modal(title, (close) => {
      const reason = h('textarea', { id: 'reason', class: 'input h-20', placeholder: 'Why? The approver reads this.', maxlength: '500' });
      const err = h('div', { class: 'hidden' });
      const extra = build ? build() : { el: null, get: () => ({}) };
      const go = h('button', { class: 'btn-primary', type: 'button', onclick: async () => {
        err.classList.add('hidden'); go.disabled = true;
        try { const ch = await post('/v1/cloud/changes', { ...extra.get(), reason: reason.value }); toast('Proposed. Someone else must approve it.', 'ok'); close(); navigate('/infrastructure?tab=changes'); return ch; }
        catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; }
      } }, 'Propose change');
      return h('div', { class: 'space-y-4' }, notice('info', summary), extra.el, field('Reason', reason), err, h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go));
    }, { wide: true });
  }

  function proposeInstance(i, action) {
    proposalForm(`${action[0].toUpperCase() + action.slice(1)} ${i.name || i.id}`, `This only proposes it. Nothing changes in AWS until a different administrator approves. ${action === 'stop' ? 'A stopped instance stops serving, and its disk keeps costing a little.' : ''}`,
      () => ({ el: null, get: () => ({ kind: 'instance-action', instance_id: i.id, action }) }));
  }

  function proposeDelete(s) {
    proposalForm('Delete stack ' + s.name, 'This only proposes it. The approver sees every resource that would be removed. Deleting a stack deletes what it created, including data on its disks.',
      () => ({ el: null, get: () => ({ kind: 'stack-delete', stack: s.name }) }));
  }

  async function deployModal() {
    let cat;
    try { cat = (await get('/v1/cloud/catalog')).templates[0]; } catch (e) { return toast(e.message, 'error'); }
    proposalForm('Deploy ' + cat.title, cat.description + ' You will see exactly what will be created before anyone approves it.', () => {
      const inputs = {};
      const stack = h('input', { id: 'dep-stack', class: 'input', value: cat.default_stack });
      const controls = cat.parameters.map(p => {
        const c = p.options ? h('select', { class: 'input', id: 'dep-' + p.name }, p.options.map(o => h('option', { value: o, selected: o === p.default }, o)))
          : h('input', { class: 'input font-mono', id: 'dep-' + p.name, value: p.default || '', placeholder: p.required ? 'required' : '' });
        inputs[p.name] = c;
        return field(p.label + (p.required ? ' *' : ''), c, p.help, 'dep-' + p.name);
      });
      return { el: h('div', { class: 'grid gap-4 sm:grid-cols-2' }, field('Stack name', stack), controls), get: () => ({ kind: 'stack-deploy', template: cat.id, stack: stack.value,
        params: Object.fromEntries(Object.entries(inputs).map(([k, c]) => [k, c.value])) }) };
    });
  }

  await load(false);
}

// ---------------------------------------------------------------- changes

async function changes(body) {
  const admin = atLeast(session.role, 'admin');
  let timer;
  async function load() {
    let list;
    try { list = (await get('/v1/cloud/changes')).changes; } catch (e) { return mount(body, errorBox(e.message)); }
    const waiting = list.filter(c => c.status === 'proposed').length;
    const badge = document.querySelector('#pending-badge');
    if (badge) { badge.textContent = waiting + ' waiting'; badge.classList.toggle('hidden', waiting === 0); }
    const rows = list.map(c => h('tr', { class: 'row-link', tabindex: '0', onclick: () => detail(c.id), onkeydown: (e) => { if (e.key === 'Enter') detail(c.id); } },
      h('td', {}, h('div', { class: 'font-medium' }, c.title), h('div', { class: 'muted max-w-lg truncate text-xs' }, c.reason)), h('td', {}, pill(c.status)),
      h('td', { class: 'text-sm' }, c.proposed_by), h('td', { class: 'muted whitespace-nowrap text-sm', title: absTime(c.proposed_at) }, relTime(c.proposed_at)),
      h('td', { class: 'text-sm' }, c.decided_by || h('span', { class: 'muted' }, '–'))));
    mount(body, notice('info', 'Nothing here changes AWS by itself. A person proposes a change, the preview shows what would happen, and a different administrator approves it. Every step is in the audit log.'),
      h('div', { class: 'card mt-4 overflow-x-auto' }, list.length ? h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Change', 'Status', 'Proposed by', 'When', 'Decided by'].map(x => h('th', {}, x)))), h('tbody', {}, rows))
        : emptyState('cloud', 'No changes yet', 'Propose one from the AWS account tab.')));
    return list;
  }

  async function detail(id) {
    let c;
    try { c = await get('/v1/cloud/changes/' + encodeURIComponent(id)); } catch (e) { return toast(e.message, 'error'); }
    const me = session.name;
    modal(c.title, (close) => {
      const note = h('input', { class: 'input', id: 'decide-note', placeholder: 'Note (optional)' });
      const act = async (verb, label) => {
        try { await post(`/v1/cloud/changes/${encodeURIComponent(id)}/${verb}`, { note: note.value }); toast(label, 'ok'); close(); load(); } catch (e) { toast(e.message, 'error'); }
      };
      const canDecide = c.status === 'proposed';
      return h('div', { class: 'space-y-4' },
        h('div', { class: 'flex flex-wrap items-center gap-3' }, pill(c.status), h('span', { class: 'muted text-sm' }, `Proposed by ${c.proposed_by} ${relTime(c.proposed_at)} · account ${c.account} · ${c.region}`)),
        h('div', {}, h('div', { class: 'h-section mb-1' }, 'Reason'), h('p', { class: 'text-sm' }, c.reason)),
        c.params && Object.keys(c.params).length ? h('div', {}, h('div', { class: 'h-section mb-1' }, 'Parameters'), h('dl', { class: 'grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm' }, Object.entries(c.params).map(([k, v]) => [h('dt', { class: 'muted' }, k), h('dd', { class: 'break-all font-mono text-xs' }, v)]))) : null,
        h('div', {}, h('div', { class: 'h-section mb-1' }, 'What would change'), c.preview.length ? h('div', { class: 'overflow-x-auto rounded-lg border border-slate-200 dark:border-slate-800' }, h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Action', 'Resource', 'Type', 'Detail'].map(x => h('th', {}, x)))),
          h('tbody', {}, c.preview.map(p => h('tr', {}, h('td', {}, h('span', { class: /Remove|Delete/.test(p.action) ? 'pill-bad' : /Modify|Stop|Reboot/.test(p.action) ? 'pill-wait' : 'pill-ok' }, p.action)), h('td', { class: 'font-medium' }, p.resource), h('td', { class: 'font-mono text-xs' }, p.type), h('td', { class: 'text-xs' }, [p.replacement && 'replacement: ' + p.replacement, p.detail].filter(Boolean).join(' · ')))))))
          : h('p', { class: 'muted text-sm' }, c.status === 'preparing' ? 'AWS is working out the preview…' : 'No preview is available.')),
        c.result && (c.status === 'failed' ? errorBox(c.result) : notice('ok', c.result)),
        c.decided_by && h('p', { class: 'muted text-sm' }, `${c.status === 'rejected' ? 'Rejected' : c.status === 'canceled' ? 'Canceled' : 'Approved'} by ${c.decided_by} ${relTime(c.decided_at)}${c.decision_note ? ': ' + c.decision_note : ''}`),
        canDecide && h('div', {}, note, h('div', { class: 'mt-3 flex flex-wrap justify-end gap-2' },
          c.proposed_by === me && h('span', { class: 'muted mr-auto self-center text-xs' }, 'You proposed this, so someone else must approve it.'),
          (c.proposed_by === me || admin) && h('button', { class: 'btn-secondary', type: 'button', onclick: () => act('cancel', 'Canceled') }, 'Cancel proposal'),
          admin && h('button', { class: 'btn-secondary', type: 'button', onclick: () => act('reject', 'Rejected') }, 'Reject'),
          admin && h('button', { class: 'btn-primary', type: 'button', disabled: c.proposed_by === me, onclick: async () => { if (await confirmDialog({ title: 'Approve and run this change?', body: 'It will run against AWS account ' + c.account + ' straight away.', confirmLabel: 'Approve and run' })) act('approve', 'Approved. Running now.'); } }, icon('check', 'size-4'), 'Approve and run'))));
    }, { wide: true });
  }

  await load();
  timer = setInterval(async () => { if (!document.hidden) await load(); }, 4000);
  return () => clearInterval(timer);
}
