import { h, icon, mount, pill, relTime, absTime, duration, pageHeader, errorBox, spinner, toast, confirmDialog } from '../lib/dom.js';
import { get, post, atLeast } from '../lib/api.js';
import { navigate } from '../lib/router.js';
import { dag } from '../lib/dag.js';
import { logViewer } from '../lib/logs.js';
import { session } from '../app.js';

const TERMINAL = new Set(['succeeded', 'failed', 'canceled']);

export async function runPage({ params }) {
  const id = params.id;
  const el = h('div', {});
  let run = null, failure = null, failureFor = '', failureTries = 0, selected = null, userPicked = false, timer = null, viewer = null, shownStep = null, dead = false;
  const canAct = atLeast(session.role, 'developer');

  async function refresh() {
    try { run = await get('/v1/runs/' + encodeURIComponent(id)); } catch (e) {
      if (!run) { mount(el, errorBox(e.status === 404 ? 'There is no run with this id.' : e.message)); stop(); return; }
      return;
    }
    if (dead) return;
    if (run.status === 'failed' && failureFor !== run.id) {
      failureFor = run.id;
      try { failure = await get('/v1/runs/' + encodeURIComponent(id) + '/failure'); failureTries = 0; } catch {
        failure = null;
        // A glitch should not hide the summary for good: try again a couple of times.
        if (++failureTries < 3) { failureFor = ''; setTimeout(() => { if (!dead) refresh(); }, 2000); }
      }
    }
    if (!userPicked) selected = pickDefault(run);
    render();
    if (TERMINAL.has(run.status)) stop();
  }

  function pickDefault(r) {
    const by = (s) => r.steps.find(x => x.status === s);
    return (by('running') || by('failed') || [...r.steps].reverse().find(x => x.status === 'succeeded' || x.status === 'cached') || r.steps[0] || {}).id || null;
  }

  function stop() { clearInterval(timer); timer = null; }

  async function cancel() {
    if (!await confirmDialog({ title: 'Cancel this run?', body: 'Steps that are running will be stopped.', confirmLabel: 'Cancel run', danger: true })) return;
    try { await post('/v1/runs/' + encodeURIComponent(id) + '/cancel'); toast('Cancel requested', 'ok'); refresh(); } catch (e) { toast(e.message, 'error'); }
  }

  async function runAgain() {
    try { const r = await post('/v1/pipelines/' + encodeURIComponent(run.pipeline_id) + '/run'); navigate('/runs/' + r.id); } catch (e) { toast(e.message, 'error'); }
  }

  function failureCard() {
    if (run.status !== 'failed' || !failure || !failure.failed) return null;
    const ask = () => {
      const input = document.querySelector('#assistant-input') || (document.querySelector('#assistant-open')?.click(), document.querySelector('#assistant-input'));
      if (input) { input.value = `Run ${run.id} of ${run.pipeline} failed${failure.step ? ' at step ' + failure.step : ''}. Explain what went wrong and what I should change in the pipeline file.`; input.focus(); }
    };
    return h('section', { class: 'card card-pad mb-4', id: 'failure-summary', 'aria-labelledby': 'failure-title' },
      h('div', { class: 'mb-2 flex flex-wrap items-center justify-between gap-2' },
        h('h2', { id: 'failure-title', class: 'h-section' }, 'What went wrong'),
        h('div', { class: 'flex gap-2' },
          failure.step && h('button', { class: 'btn-secondary btn-sm', type: 'button', id: 'failure-show-step', onclick: () => { selected = failure.step; userPicked = true; render(); } }, 'Show the step'),
          canAct && h('button', { class: 'btn-secondary btn-sm', type: 'button', id: 'failure-ask', onclick: ask }, icon('sparkle', 'size-3.5'), 'Ask the assistant'))),
      h('p', { class: 'text-sm font-medium' }, failure.headline),
      failure.causes.length ? h('ul', { class: 'mt-2 list-disc space-y-1 pl-5 text-sm' }, failure.causes.map(c => h('li', {}, c))) : null,
      failure.skipped.length ? h('p', { class: 'muted mt-2 text-sm' }, 'Did not run because of it: ' + failure.skipped.join(', ')) : null,
      failure.tail ? h('details', { class: 'mt-3' }, h('summary', { class: 'cursor-pointer text-sm' }, 'End of the output'),
        h('pre', { class: 'mt-2 max-h-72 overflow-auto rounded-lg bg-slate-950 p-3 font-mono text-xs text-slate-100' }, failure.tail)) : null);
  }

  function render() {
    const live = !TERMINAL.has(run.status);
    const steps = run.steps.map(s => ({ id: s.id, needs: s.needs || [], status: s.status,
      sub: s.status + (s.started_at ? ' · ' + duration(s.started_at, s.finished_at) : ''), title: s.error || '' }));
    const step = run.steps.find(s => s.id === selected);

    const meta = (label, value) => h('div', {}, h('dt', { class: 'h-section' }, label), h('dd', { class: 'mt-1 text-sm font-medium' }, value));
    const src = run.source;

    if (viewer && shownStep !== selected) { viewer.close(); viewer = null; }
    if (step && !viewer && step.status !== 'pending' && step.status !== 'skipped') { viewer = logViewer(id, step.id); shownStep = selected; }

    mount(el,
      h('nav', { class: 'mb-3 flex items-center gap-1.5 text-sm muted', 'aria-label': 'Breadcrumb' },
        h('a', { href: '#/pipelines', class: 'hover:underline' }, 'Pipelines'), icon('chevron', 'size-3'),
        run.pipeline_id ? h('a', { href: '#/pipelines/' + encodeURIComponent(run.pipeline_id), class: 'hover:underline' }, run.pipeline) : h('span', {}, run.pipeline),
        icon('chevron', 'size-3'), h('span', { class: 'font-mono text-xs' }, run.id.slice(0, 8))),
      pageHeader(run.pipeline, null,
        pill(run.status),
        live && canAct && h('button', { class: 'btn-secondary', type: 'button', onclick: cancel }, icon('stop', 'size-3.5'), 'Cancel'),
        !live && canAct && run.pipeline_id && h('button', { class: 'btn-primary', type: 'button', onclick: runAgain }, icon('play', 'size-3.5'), 'Run again')),
      run.reason && run.status !== 'succeeded' && h('div', { class: 'mb-4' }, errorBox(run.reason)),
      failureCard(),
      h('dl', { class: 'card card-pad mb-4 grid grid-cols-2 gap-4 sm:grid-cols-3 lg:grid-cols-5' },
        meta('Started by', run.triggered_by || run.trigger),
        meta('Trigger', run.trigger + (run.pipeline_version ? ' · v' + run.pipeline_version : '')),
        meta('Created', h('span', { title: absTime(run.created_at) }, relTime(run.created_at))),
        meta('Duration', duration(run.started_at, run.finished_at) || (live ? 'waiting…' : '–')),
        meta('Source', src ? h('span', { class: 'font-mono text-xs' }, [src.repo, src.sha && '@' + src.sha.slice(0, 7), src.ref && ' (' + src.ref.replace('refs/heads/', '') + ')'].filter(Boolean).join('')) : 'none')),
      h('div', { class: 'card mb-4' },
        h('div', { class: 'border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('h2', { class: 'h-section' }, 'Steps')),
        h('div', { class: 'p-4' }, dag(steps, { selected, onSelect: (sid) => { selected = sid; userPicked = true; render(); } }))),
      step && h('div', { class: 'card card-pad' },
        h('div', { class: 'mb-4 flex flex-wrap items-center justify-between gap-2' },
          h('div', { class: 'flex items-center gap-3' }, h('h2', { class: 'text-base font-semibold' }, step.id), pill(step.status)),
          h('div', { class: 'muted text-sm' }, step.started_at ? 'Started ' + relTime(step.started_at) + (step.finished_at ? ' · took ' + duration(step.started_at, step.finished_at) : ' · running') : 'Not started')),
        step.error && h('div', { class: 'mb-4' }, errorBox(step.error)),
        step.status === 'failed' && step.exit_code ? h('p', { class: 'muted mb-3 text-sm' }, 'Exit code ' + step.exit_code) : null,
        viewer ? viewer.el : h('p', { class: 'muted text-sm' }, step.status === 'skipped' ? 'This step was skipped because a step it needs did not succeed.' : 'This step has not started yet.')));
  }

  mount(el, spinner());
  await refresh();
  if (run && !TERMINAL.has(run.status)) timer = setInterval(() => { if (!document.hidden) refresh(); }, 1500);
  el.cleanup = () => { dead = true; stop(); viewer && viewer.close(); };
  return el;
}
