import { h, icon, mount, pill, sparkline, relTime, absTime, fmtSeconds, duration, pct, kpi, pageHeader, emptyState, spinner, errorBox, toast, bytes } from '../lib/dom.js';
import { get, post, atLeast } from '../lib/api.js';
import { navigate } from '../lib/router.js';
import { session } from '../app.js';

export async function overviewPage() {
  const el = h('div', {});
  let timer = null;
  let first = true;

  async function load() {
    try {
      const [ov, rt] = await Promise.all([get('/v1/overview'), get('/v1/infra/runtime').catch(() => null)]);
      render(ov, rt);
    } catch (e) {
      if (first) mount(el, errorBox(e.message));
    }
    first = false;
  }

  function render(ov, rt) {
    const t = ov.totals;
    const finished = t.succeeded + t.failed;
    const rate = finished ? t.succeeded / finished : null;
    const canRun = atLeast(session.role, 'developer');

    const runNow = async (p) => {
      try {
        const run = await post('/v1/pipelines/' + encodeURIComponent(p.name) + '/run');
        toast('Started ' + p.name, 'ok');
        navigate('/runs/' + run.id);
      } catch (e) { toast(e.message, 'error'); }
    };

    const pipelineRows = ov.pipelines.map(p => {
      const open = () => navigate(p.saved ? '/pipelines/' + encodeURIComponent(p.name) : (p.latest ? '/runs/' + p.latest.id : '/pipelines'));
      return h('tr', { class: 'row-link', onclick: open, tabindex: '0', onkeydown: (e) => { if (e.key === 'Enter') open(); } },
        h('td', {}, h('div', { class: 'font-medium' }, p.name, !p.saved && h('span', { class: 'pill-idle ml-2 align-middle', title: 'Runs exist, but there is no saved definition in the console' }, 'not saved')),
          p.description && h('div', { class: 'muted max-w-xs truncate text-xs' }, p.description)),
        h('td', {}, p.latest ? pill(p.latest.status) : h('span', { class: 'muted text-xs' }, 'never run')),
        h('td', {}, sparkline(p.recent)),
        h('td', { class: 'tabular-nums' }, pct(p.success_rate)),
        h('td', { class: 'tabular-nums' }, p.avg_seconds != null ? fmtSeconds(p.avg_seconds) : '–'),
        h('td', { class: 'whitespace-nowrap text-sm' }, p.latest ? h('span', { title: absTime(p.latest.created_at) }, relTime(p.latest.created_at)) : '–',
          p.latest?.triggered_by && h('div', { class: 'muted text-xs' }, 'by ' + p.latest.triggered_by)),
        h('td', { class: 'text-right' }, canRun && p.saved && h('button', { class: 'btn-secondary btn-sm', type: 'button', title: 'Run ' + p.name,
          onclick: (e) => { e.stopPropagation(); runNow(p); } }, icon('play', 'size-3'), 'Run')));
    });

    const recentRows = ov.recent.map(r => h('tr', { class: 'row-link', onclick: () => navigate('/runs/' + r.id), tabindex: '0', onkeydown: (e) => { if (e.key === 'Enter') navigate('/runs/' + r.id); } },
      h('td', { class: 'font-medium' }, r.pipeline), h('td', {}, pill(r.status)),
      h('td', { class: 'muted text-sm' }, r.triggered_by ? r.triggered_by + ' · ' + r.trigger : r.trigger, r.source?.sha && h('span', { class: 'ml-1 font-mono text-xs' }, r.source.sha.slice(0, 7))),
      h('td', { class: 'whitespace-nowrap text-sm tabular-nums' }, duration(r.started_at, r.finished_at) || '–'),
      h('td', { class: 'muted whitespace-nowrap text-sm', title: absTime(r.created_at) }, relTime(r.created_at))));

    mount(el,
      pageHeader('Overview', 'What is running, what is failing, and how the platform is doing.',
        canRun && h('a', { href: '#/pipelines/new', class: 'btn-primary' }, icon('plus'), 'New pipeline')),
      h('div', { class: 'grid grid-cols-2 gap-4 lg:grid-cols-4' },
        kpi('Running', t.running, 'steps executing now', t.running ? 'run' : null),
        kpi('Queued', t.queued, 'waiting for a worker'),
        kpi('Success rate', pct(rate), `${t.succeeded} passed · ${t.failed} failed · last ${t.window} runs`, rate == null ? null : rate >= 0.9 ? 'ok' : rate < 0.6 ? 'bad' : null),
        kpi('Failed', t.failed, 'in the same window', t.failed ? 'bad' : null)),
      platformStrip(rt),
      h('section', { class: 'mt-8' },
        h('h2', { class: 'h-section mb-3' }, 'Pipelines'),
        h('div', { class: 'card overflow-x-auto' }, ov.pipelines.length
          ? h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Pipeline', 'Status', 'Last 12 runs', 'Success', 'Avg time', 'Last run', ''].map(x => h('th', {}, x)))), h('tbody', {}, pipelineRows))
          : emptyState('flow', 'No pipelines yet', 'Create your first pipeline in the editor, or send one from the command line.',
              canRun && h('a', { href: '#/pipelines/new', class: 'btn-primary' }, icon('plus'), 'Create a pipeline')))),
      ov.recent.length > 0 && h('section', { class: 'mt-8' },
        h('h2', { class: 'h-section mb-3' }, 'Recent runs'),
        h('div', { class: 'card overflow-x-auto' }, h('table', { class: 'tbl' },
          h('thead', {}, h('tr', {}, ['Pipeline', 'Status', 'Started by', 'Duration', 'When'].map(x => h('th', {}, x)))), h('tbody', {}, recentRows)))));
  }

  function platformStrip(rt) {
    if (!rt) return null;
    const d = rt.docker, s = rt.server;
    const item = (ok, label, detail) => h('div', { class: 'flex items-center gap-2 text-sm' },
      h('span', { class: 'size-2 rounded-full ' + (ok ? 'bg-emerald-500' : 'bg-rose-500') }), h('span', { class: 'font-medium' }, label), h('span', { class: 'muted' }, detail));
    return h('a', { href: '#/infrastructure', class: 'card mt-4 flex flex-wrap items-center gap-x-8 gap-y-2 px-5 py-3 transition-colors hover:border-brand-300 dark:hover:border-brand-500/50' },
      item(d.available, 'Docker', d.available ? `${d.version} · ${rt.steps.length} step container${rt.steps.length === 1 ? '' : 's'} running` : (d.error || 'not reachable')),
      item(true, 'Server', `v${s.version} · ${s.running} run${s.running === 1 ? '' : 's'} active, ${s.queued} queued · ${s.max_runs} worker${s.max_runs === 1 ? '' : 's'}`),
      s.disk_total_bytes ? item(s.disk_free_bytes / s.disk_total_bytes > 0.1, 'Disk', bytes(s.disk_free_bytes) + ' free') : null,
      h('span', { class: 'muted ml-auto flex items-center gap-1 text-xs' }, 'Infrastructure', icon('chevron', 'size-3')));
  }

  mount(el, spinner());
  await load();
  timer = setInterval(() => { if (!document.hidden) load(); }, 4000);
  el.cleanup = () => clearInterval(timer);
  return el;
}
