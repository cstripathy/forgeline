import { h, icon, mount, pageHeader, relTime, absTime, emptyState, spinner, errorBox, notice, toast, modal, confirmDialog, field } from '../lib/dom.js';
import { get, post, put, del, atLeast } from '../lib/api.js';
import { session } from '../app.js';

// Guard: security and hygiene findings across the connected AWS accounts. Scans only read. An
// administrator can accept a finding for a while, with a reason that everyone can see.

const SEVERITIES = ['critical', 'high', 'medium', 'low', 'info'];
const SEV_CLASS = { critical: 'pill-bad', high: 'pill-bad', medium: 'pill-wait', low: 'pill-idle', info: 'pill-idle' };
const SERVICE = { sg: 'Security groups', ebs: 'EBS volumes', ec2: 'EC2', rds: 'RDS', cfn: 'CloudFormation', ecs: 'ECS', logs: 'CloudWatch Logs', ecr: 'ECR', lambda: 'Lambda', 's3-access': 'S3', iam: 'IAM', 'pipe-pipelines': 'Pipelines', 'pipe-logs': 'Run logs' };
const svcName = (id) => SERVICE[id] || (id.startsWith('k8s-') ? 'Kubernetes ' + id.slice(4) : id);
const sevPill = (s) => h('span', { class: SEV_CLASS[s] || 'pill-idle', 'data-severity': s }, s);
const SCHEDULE = [[0, 'Off'], [6, 'Every 6 hours'], [12, 'Every 12 hours'], [24, 'Every day'], [168, 'Every week']];

export async function guardPage() {
  const admin = atLeast(session.role, 'admin'), dev = atLeast(session.role, 'developer');
  const el = h('div', {});
  let view = 'open', severity = '', account = '', timer;
  let data, scans, accounts = [], clusters = [];

  async function load() {
    const qs = `?status=${view === 'resolved' ? 'resolved' : 'open'}` + (account ? '&account=' + encodeURIComponent(account) : '');
    try {
      [data, scans, accounts, clusters] = await Promise.all([get('/v1/guard/findings' + qs), get('/v1/guard/scans'), get('/v1/aws/accounts').then(r => r.accounts).catch(() => []), get('/v1/k8s/clusters').then(r => r.clusters).catch(() => [])]);
    } catch (e) { return mount(el, errorBox(e.message)); }
    draw();
    clearTimeout(timer);
    if (scans.scans[0]?.status === 'running') timer = setTimeout(() => { if (document.body.contains(el)) load(); }, 3000);
  }

  function draw() {
    const last = scans.scans[0];
    const running = last?.status === 'running';
    const rows = data.findings.filter(f => (!severity || f.severity === severity) && (view === 'accepted' ? !!f.accepted : view === 'open' ? !f.accepted : true));
    const total = SEVERITIES.reduce((n, s) => n + (data.counts[s] || 0), 0);

    const scanBtn = h('button', { class: 'btn-primary', id: 'scan-now', type: 'button', disabled: !dev || running, onclick: async () => {
      try { await post('/v1/guard/scan', account ? { account } : {}); toast('Scanning. This takes under a minute for each account.', 'ok'); load(); } catch (e) { toast(e.message, 'error'); }
    } }, icon('refresh', 'size-4'), running ? 'Scanning…' : 'Scan now');
    const auto = admin && h('label', { class: 'flex items-center gap-2 text-sm' }, h('span', { class: 'muted' }, 'Automatic'),
      h('select', { id: 'auto-scan', class: 'input w-auto', onchange: async (e) => { try { await put('/v1/guard/settings', { every_hours: Number(e.target.value) }); toast('Saved', 'ok'); } catch (err) { toast(err.message, 'error'); } } },
        SCHEDULE.map(([v, l]) => h('option', { value: String(v), selected: v === scans.every_hours }, l))));

    mount(el, pageHeader('Guard', 'Security and hygiene checks across your AWS accounts, Kubernetes clusters and pipelines. Scans only read; fixing something is a proposal a person approves.', auto, scanBtn),
      last && h('p', { class: 'muted mb-4 text-sm', id: 'scan-status' }, running ? 'A scan is running…' : `Last scan ${relTime(last.finished_at || last.started_at)}`,
        last.accounts?.some(a => a.error) ? ' · ' + last.accounts.filter(a => a.error).map(a => `${a.name}: ${a.error}`).join('; ') : '',
        last.accounts?.some(a => a.skipped?.length) ? ' · could not check: ' + [...new Set(last.accounts.flatMap(a => a.skipped))].join(', ') : ''),
      h('div', { class: 'mb-4 grid grid-cols-2 gap-3 sm:grid-cols-5' }, SEVERITIES.map(s => h('button', { type: 'button', 'data-sev-tile': s, 'aria-pressed': String(severity === s),
        class: 'rounded-xl border p-3 text-left transition hover:border-brand-400 ' + (severity === s ? 'border-brand-500 ring-2 ring-brand-500/30' : 'border-slate-200 dark:border-slate-800'),
        onclick: () => { severity = severity === s ? '' : s; draw(); } },
        h('div', { class: 'text-xs uppercase tracking-wide muted' }, s), h('div', { class: 'mt-1 text-2xl font-semibold tabular-nums ' + (s === 'critical' && data.counts[s] ? 'text-rose-600 dark:text-rose-400' : '') }, data.counts[s] || 0)))),
      h('div', { class: 'mb-3 flex flex-wrap items-center justify-between gap-2' },
        h('div', { class: 'flex gap-1', role: 'tablist' }, [['open', `Open (${total})`], ['accepted', `Accepted (${data.accepted})`], ['resolved', 'Resolved']].map(([id, label]) => h('button', { type: 'button', role: 'tab', id: 'view-' + id, 'aria-selected': String(view === id),
          class: 'rounded-lg px-3 py-1.5 text-sm font-medium ' + (view === id ? 'bg-brand-50 text-brand-700 dark:bg-brand-950/40 dark:text-brand-200' : 'muted hover:bg-slate-100 dark:hover:bg-slate-800'),
          onclick: () => { view = id; load(); } }, label))),
        h('select', { id: 'guard-account', class: 'input w-auto', 'aria-label': 'Account', onchange: (e) => { account = e.target.value; load(); } },
          h('option', { value: '' }, 'Everything'), h('option', { value: 'pipelines', selected: account === 'pipelines' }, 'Pipelines and run logs'), accounts.map(a => h('option', { value: a.id, selected: a.id === account }, a.name)), clusters.map(k => h('option', { value: 'k8s:' + k.id, selected: 'k8s:' + k.id === account }, k.name + ' (Kubernetes)')))),
      rows.length ? h('div', { class: 'card overflow-x-auto' }, h('table', { class: 'tbl', id: 'findings' }, h('thead', {}, h('tr', {}, ['Severity', 'Finding', 'Resource', 'Account', 'First seen'].map(x => h('th', {}, x)))),
        h('tbody', {}, rows.map(f => h('tr', { class: 'row-link', tabindex: '0', 'data-finding': f.id, 'data-rule': f.rule, onclick: () => detail(f), onkeydown: (e) => { if (e.key === 'Enter') detail(f); } },
          h('td', {}, sevPill(f.severity)), h('td', {}, h('div', { class: 'font-medium' }, f.title), h('div', { class: 'muted max-w-md truncate text-xs' }, f.detail)),
          h('td', {}, h('div', { class: 'text-sm' }, f.name), h('div', { class: 'muted text-xs' }, svcName(f.service))),
          h('td', { class: 'text-sm' }, f.account_name), h('td', { class: 'muted whitespace-nowrap text-sm', title: absTime(f.first_seen) }, relTime(f.first_seen)))))))
        : emptyState('shield', last ? (view === 'open' ? 'Nothing open here' : 'Nothing to show') : 'No scan has run yet', last ? null : 'Press Scan now to check your pipelines, accounts and clusters.'));
  }

  function detail(f) {
    modal(f.title, (close) => {
      const reason = h('input', { id: 'accept-reason', class: 'input', maxlength: '500', placeholder: 'Why this is acceptable for now (shown to everyone)' });
      const days = h('select', { id: 'accept-days', class: 'input w-auto' }, [7, 14, 30, 60, 90].map(d => h('option', { value: String(d) }, d + ' days')));
      const err = h('div', { class: 'hidden' });
      const accept = h('button', { class: 'btn-secondary', type: 'button', id: 'accept-finding', onclick: async () => {
        err.classList.add('hidden');
        try { await post('/v1/guard/findings/' + f.id + '/accept', { reason: reason.value, days: Number(days.value) }); toast('Accepted until it expires', 'ok'); close(); load(); }
        catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); }
      } }, 'Accept this finding');
      return h('div', { class: 'space-y-4', id: 'finding-detail' },
        h('div', { class: 'flex flex-wrap items-center gap-3' }, sevPill(f.severity), h('span', { class: 'muted text-sm' }, `${f.account_name}${f.aws_account ? ' · ' + f.aws_account : ''} · ${f.region} · ${svcName(f.service)}`)),
        h('dl', { class: 'grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm' }, h('dt', { class: 'muted' }, 'Resource'), h('dd', { class: 'break-all font-mono text-xs' }, f.name + (f.resource !== f.name ? ' (' + f.resource + ')' : '')),
          h('dt', { class: 'muted' }, 'What we saw'), h('dd', {}, f.detail), h('dt', { class: 'muted' }, 'First seen'), h('dd', { title: absTime(f.first_seen) }, relTime(f.first_seen)),
          h('dt', { class: 'muted' }, f.status === 'resolved' ? 'Resolved' : 'Last seen'), h('dd', { title: absTime(f.resolved_at || f.last_seen) }, relTime(f.resolved_at || f.last_seen))),
        h('div', {}, h('div', { class: 'h-section mb-1' }, 'How to fix it'), h('p', { class: 'text-sm' }, f.fix)),
        f.accepted ? notice('info', h('div', {}, h('strong', {}, 'Accepted'), ` by ${f.accepted.by} until ${absTime(f.accepted.expires)}. `, f.accepted.reason),
          admin && h('button', { class: 'btn-ghost btn-sm mt-2', id: 'unaccept', type: 'button', onclick: async () => {
            try { await del('/v1/guard/findings/' + f.id + '/accept'); toast('Back in the open list', 'ok'); close(); load(); } catch (e) { toast(e.message, 'error'); }
          } }, 'Stop accepting')) : null,
        admin && f.status === 'open' && !f.accepted ? h('div', { class: 'space-y-2 rounded-lg border border-slate-200 p-3 dark:border-slate-800' },
          h('div', { class: 'h-section' }, 'Accept the risk for a while'), reason, h('div', { class: 'flex items-center justify-between gap-2' }, days, accept), err,
          h('p', { class: 'muted text-xs' }, 'It leaves the open count and comes back for review when the time is up. The reason and your name are in the audit log.')) : null,
        dev && h('div', { class: 'flex justify-end' }, h('button', { class: 'btn-secondary btn-sm', type: 'button', id: 'ask-fix', onclick: () => {
          close();
          const input = document.querySelector('#assistant-input') || (document.querySelector('#assistant-open')?.click(), document.querySelector('#assistant-input'));
          if (input) { input.value = `Guard found: "${f.title}" on ${f.name} (${f.resource}) in account ${f.account_name}. Explain the risk, tell me the safest fix step by step, and say which parts you can propose and which I must do by hand.`; input.focus(); }
        } }, icon('sparkle', 'size-3.5'), 'Ask the assistant how to fix it')));
    }, { wide: true });
  }

  mount(el, spinner());
  await load();
  el.cleanup = () => clearTimeout(timer);
  return el;
}
