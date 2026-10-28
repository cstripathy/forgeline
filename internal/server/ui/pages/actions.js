import { h, icon, mount, pageHeader, relTime, absTime, emptyState, spinner, errorBox, notice, toast, modal, confirmDialog, field, pill } from '../lib/dom.js';
import { get, post } from '../lib/api.js';

// GitHub Actions through the API (design note 011). Everything shown here comes from GitHub, so
// it is inserted as text only, and links are only followed when the server has checked them.

/** Maps GitHub's status and conclusion onto the console's pill vocabulary. */
function runState(r) {
  if (r.status !== 'completed') {
    if (r.status === 'in_progress') return ['running', 'Running'];
    return ['queued', r.status === 'waiting' ? 'Waiting' : 'Queued'];
  }
  switch (r.conclusion) {
    case 'success': return ['succeeded', 'Succeeded'];
    case 'failure': case 'timed_out': case 'startup_failure': return ['failed', r.conclusion === 'failure' ? 'Failed' : 'Timed out'];
    case 'cancelled': return ['canceled', 'Canceled'];
    case 'skipped': return ['skipped', 'Skipped'];
    default: return ['pending', r.conclusion || 'Done'];
  }
}

export async function actionsPage() {
  const el = h('div', {});
  mount(el, spinner());
  let repos, repo = '', timer = null, dead = false, workflows = [], runs = [], filter = '';
  const body = h('div', {});

  try { repos = await get('/v1/actions/repos'); } catch (e) { return mount(el, errorBox(e.message)); }
  if (!repos.connected || !repos.repos.length) {
    return mount(el, pageHeader('GitHub Actions', 'Start, watch and re-run workflows of your repositories.'),
      h('div', { class: 'card' }, emptyState('plug', 'Connect GitHub first', 'Actions are limited to the repositories on the GitHub allow list.',
        h('a', { class: 'btn-primary', href: '#/integrations' }, 'Open Integrations'))));
  }
  repo = repos.repos[0].repo;
  const rule = () => repos.repos.find(r => r.repo === repo);

  const repoSel = h('select', { class: 'input w-auto', 'aria-label': 'Repository', onchange: (e) => { repo = e.target.value; filter = ''; load(); } },
    repos.repos.map(r => h('option', { value: r.repo }, r.repo)));
  const refreshBtn = h('button', { class: 'btn-secondary', type: 'button', onclick: () => load() }, icon('refresh'), 'Refresh');

  async function load(quiet) {
    if (!quiet) mount(body, spinner());
    try {
      const q = '?repo=' + encodeURIComponent(repo);
      [workflows, runs] = await Promise.all([
        get('/v1/actions/workflows' + q).then(r => r.workflows),
        get('/v1/actions/runs' + q + (filter ? '&workflow=' + encodeURIComponent(filter) : '')).then(r => r.runs),
      ]);
    } catch (e) { return mount(body, errorBox(e.message)); }
    if (dead) return;
    draw();
    clearInterval(timer);
    if (runs.some(r => r.status !== 'completed')) timer = setInterval(() => { if (!document.hidden) load(true); }, 10000);
  }

  function draw() {
    const wfCard = h('div', { class: 'card' },
      h('div', { class: 'border-b border-slate-200 px-4 py-3 font-medium dark:border-slate-800' }, 'Workflows'),
      workflows.length
        ? h('ul', { class: 'divide-y divide-slate-200 dark:divide-slate-800' }, workflows.map(w => h('li', { class: 'flex items-center justify-between gap-3 px-4 py-2.5' },
          h('button', { class: 'min-w-0 text-left', type: 'button', title: 'Show only this workflow\'s runs', onclick: () => { filter = filter === String(w.id) ? '' : String(w.id); load(true); } },
            h('div', { class: 'truncate text-sm font-medium ' + (filter === String(w.id) ? 'text-brand-600 dark:text-brand-300' : '') }, w.name),
            h('div', { class: 'muted truncate font-mono text-xs' }, w.path)),
          w.state === 'active'
            ? h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: () => startDialog(w) }, icon('play', 'size-3.5'), 'Run')
            : h('span', { class: 'pill-idle' }, w.state))))
        : emptyState('flow', 'No workflows', 'This repository has no files under .github/workflows.'));

    const runsCard = h('div', { class: 'card overflow-x-auto' },
      h('div', { class: 'flex items-center justify-between border-b border-slate-200 px-4 py-3 dark:border-slate-800' },
        h('span', { class: 'font-medium' }, 'Runs', filter && h('span', { class: 'muted ml-2 text-sm' }, '(one workflow)')),
        filter && h('button', { class: 'btn-ghost btn-sm', type: 'button', onclick: () => { filter = ''; load(true); } }, 'Show all')),
      runs.length
        ? h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Status', 'Run', 'Branch', 'Event', 'By', 'When'].map(x => h('th', {}, x)))),
          h('tbody', {}, runs.map(r => {
            const [k, label] = runState(r);
            return h('tr', { class: 'cursor-pointer', tabindex: '0', onclick: () => detail(r), onkeydown: (e) => { if (e.key === 'Enter') detail(r); } },
              h('td', {}, pill(k, label)),
              h('td', { class: 'max-w-xs truncate text-sm' }, h('span', { class: 'font-medium' }, r.name), h('span', { class: 'muted ml-2 text-xs' }, '#' + r.run_number),
                r.fork && h('span', { class: 'pill-wait ml-2', title: 'The code of this run comes from another repository' }, 'fork')),
              h('td', { class: 'font-mono text-xs' }, r.branch), h('td', { class: 'text-sm' }, r.event), h('td', { class: 'text-sm' }, r.actor || '–'),
              h('td', { class: 'muted whitespace-nowrap text-sm', title: absTime(r.created_at) }, relTime(r.created_at)));
          })))
        : emptyState('clock', 'No runs yet', 'Start a workflow, or push to a branch that has one.'));

    mount(body, h('div', { class: 'grid gap-4 lg:grid-cols-[minmax(0,20rem)_minmax(0,1fr)]' }, wfCard, runsCard));
  }

  function startDialog(w) {
    const exact = (rule().branches || []).filter(b => !b.includes('*'));
    modal('Run ' + w.name, (close) => {
      const ref = h('input', { id: 'ref', class: 'input font-mono', value: exact[0] || '', list: 'refs', autocomplete: 'off' });
      const list = h('datalist', { id: 'refs' }, exact.map(b => h('option', { value: b })));
      const rows = h('div', { class: 'space-y-2' });
      const addRow = () => {
        const k = h('input', { class: 'input font-mono', placeholder: 'name', 'aria-label': 'Input name', maxlength: '64' });
        const v = h('input', { class: 'input', placeholder: 'value', 'aria-label': 'Input value', maxlength: '1000' });
        const row = h('div', { class: 'grid grid-cols-[1fr_2fr_auto] gap-2' }, k, v,
          h('button', { class: 'btn-ghost btn-sm', type: 'button', 'aria-label': 'Remove input', onclick: () => row.remove() }, icon('x')));
        row._get = () => [k.value.trim(), v.value];
        rows.append(row);
      };
      const err = h('div', { class: 'hidden' });
      const go = h('button', { class: 'btn-primary', type: 'button', onclick: async () => {
        err.classList.add('hidden'); go.disabled = true;
        const inputs = {};
        for (const row of rows.children) { const [k, v] = row._get(); if (k) inputs[k] = v; }
        try {
          await post('/v1/actions/dispatch', { repo, workflow: String(w.id), ref: ref.value.trim(), inputs });
          toast('Started. It appears in the list in a few seconds.', 'ok'); close(); setTimeout(() => load(true), 3000);
        } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; }
      } }, icon('play', 'size-4'), 'Run workflow');
      return h('div', { class: 'space-y-4' },
        notice('info', 'The workflow file needs ', h('code', { class: 'font-mono text-xs' }, 'on: workflow_dispatch'), '. Only branches allowed for this repository can be used: ', h('span', { class: 'font-mono text-xs' }, (rule().branches || []).join(', ')), '.'),
        field('Branch', ref, 'The workflow file of this branch is the one that runs.'), list,
        h('div', {}, h('div', { class: 'label' }, 'Inputs (optional)'), rows, h('button', { class: 'btn-ghost btn-sm mt-2', type: 'button', onclick: addRow }, icon('plus', 'size-3.5'), 'Add input')),
        err, h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go));
    });
  }

  async function detail(r0) {
    modal('Run #' + r0.run_number + ' · ' + r0.name, (close) => {
      const box = h('div', {}, spinner());
      (async function render() {
        let d;
        try { d = await get('/v1/actions/runs/' + r0.id + '?repo=' + encodeURIComponent(repo)); } catch (e) { return mount(box, errorBox(e.message)); }
        const r = d.run, [k, label] = runState(r), done = r.status === 'completed';
        const act = (what, text, extra, danger) => h('button', { class: danger ? 'btn-danger btn-sm' : 'btn-secondary btn-sm', type: 'button', onclick: async (ev) => {
          if (danger && !await confirmDialog({ title: text + '?', body: 'This changes the run on GitHub.', confirmLabel: text, danger: true })) return;
          ev.target.disabled = true;
          try { await post('/v1/actions/runs/' + r.id + '/' + what, { repo, ...extra }); toast(text + ': done', 'ok'); close(); setTimeout(() => load(true), 2000); }
          catch (e) { toast(e.message, 'error'); ev.target.disabled = false; }
        } }, text);
        const logs = h('pre', { class: 'mt-2 hidden max-h-96 overflow-auto rounded-lg bg-slate-950 p-3 font-mono text-xs leading-relaxed text-slate-100' });
        const jobs = d.jobs.map(j => {
          const [jk, jl] = runState(j);
          return h('div', { class: 'rounded-lg border border-slate-200 p-3 dark:border-slate-800' },
            h('div', { class: 'flex items-center justify-between gap-2' }, h('div', { class: 'flex min-w-0 items-center gap-2' }, pill(jk, jl), h('span', { class: 'truncate text-sm font-medium' }, j.name)),
              h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: async (ev) => {
                ev.target.disabled = true; logs.classList.remove('hidden'); logs.textContent = 'Loading…';
                try { const l = await get('/v1/actions/jobs/' + j.id + '/logs?repo=' + encodeURIComponent(repo)); logs.textContent = (l.truncated ? '… (only the end of the log is shown)\n' : '') + l.text; logs.scrollTop = logs.scrollHeight; }
                catch (e) { logs.textContent = e.message; }
                ev.target.disabled = false;
              } }, 'View log')),
            j.runner_name && h('div', { class: 'muted mt-1 text-xs' }, 'Runner ', h('span', { class: 'font-mono' }, j.runner_name), j.labels && j.labels.length ? ' · ' + j.labels.join(', ') : ''),
            h('ul', { class: 'mt-2 space-y-0.5 text-xs' }, (j.steps || []).map(s => { const [sk] = runState(s); return h('li', { class: 'flex items-center gap-2' }, h('span', { class: 'size-1.5 rounded-full ' + ({ succeeded: 'bg-emerald-500', failed: 'bg-rose-500', running: 'bg-sky-500', queued: 'bg-amber-400' }[sk] || 'bg-slate-300') }), s.name); })));
        });
        mount(box, h('div', { class: 'space-y-3' },
          h('div', { class: 'flex flex-wrap items-center gap-2 text-sm' }, pill(k, label), h('span', { class: 'font-mono text-xs' }, r.branch), h('span', { class: 'muted' }, 'by ' + (r.actor || 'unknown') + ' · ' + r.event + ' · ' + relTime(r.created_at))),
          r.fork && notice('warn', 'The code of this run comes from a fork. Review it before re-running, and never let a self-hosted runner execute it.'),
          jobs.length ? h('div', { class: 'space-y-2' }, jobs) : h('p', { class: 'muted text-sm' }, 'No jobs yet.'), logs,
          h('div', { class: 'flex flex-wrap items-center justify-between gap-2 pt-1' },
            h('div', { class: 'flex flex-wrap gap-2' }, !done && act('cancel', 'Cancel run', {}, true), done && act('rerun', 'Re-run all jobs', {}), done && r.conclusion === 'failure' && act('rerun', 'Re-run failed jobs', { failed_only: true })),
            r.html_url && h('a', { class: 'btn-ghost btn-sm', href: r.html_url, target: '_blank', rel: 'noopener noreferrer' }, 'Open on GitHub'))));
      })();
      return box;
    }, { wide: true });
  }

  mount(el,
    pageHeader('GitHub Actions', 'Start, watch and re-run workflows of your allowed repositories. Nothing runs on this server: GitHub runs the jobs.', repoSel, refreshBtn),
    body);
  await load();
  el.cleanup = () => { dead = true; clearInterval(timer); };
  return el;
}
