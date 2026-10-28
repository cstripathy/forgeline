import { h, icon, mount, pill, relTime, absTime, pageHeader, errorBox, notice, spinner, toast, modal, confirmDialog, field, copyButton } from '../lib/dom.js';
import { get, post, put, del, atLeast } from '../lib/api.js';
import { navigate } from '../lib/router.js';
import { dag } from '../lib/dag.js';
import { session } from '../app.js';

const STARTER = `version: v1alpha1
name: my-pipeline
steps:
  - id: build
    image: alpine:3.24
    run: |
      echo "building..."
      sleep 2
      echo "done"
  - id: test
    image: alpine:3.24
    needs: [build]
    run: echo "running tests" && sleep 1
`;

export async function pipelineEditorPage({ params }) {
  const id = params.id;
  const isNew = id === null || id === undefined;
  const canEdit = atLeast(session.role, 'developer');
  const el = h('div', {});
  mount(el, spinner());

  let saved = null; // the stored pipeline, null for a new one
  if (!isNew) {
    try { saved = await get('/v1/pipelines/' + encodeURIComponent(id)); } catch (e) { return mount(el, errorBox(e.status === 404 ? 'There is no pipeline with this name.' : e.message)); }
  }
  let secretNames = [];
  if (canEdit) { try { secretNames = (await get('/v1/secrets')).secrets.map(s => s.name); } catch { /* optional */ } }

  let text = saved ? saved.yaml : STARTER;
  let desc = saved ? saved.description || '' : '';
  let baseVersion = saved ? saved.version : 0;
  let result = null, timer = null, seq = 0, busy = false;

  const area = h('textarea', { id: 'yaml', class: 'block h-[34rem] w-full resize-none whitespace-pre bg-transparent py-3 pl-2 pr-3 font-mono text-[13px] leading-6 outline-none', spellcheck: 'false', wrap: 'off',
    autocapitalize: 'off', autocomplete: 'off', 'aria-label': 'Pipeline YAML', readOnly: !canEdit });
  area.value = text;
  const gutter = h('pre', { class: 'h-[34rem] select-none overflow-hidden border-r border-slate-200 bg-slate-50 px-3 py-3 text-right font-mono text-[13px] leading-6 text-slate-400 dark:border-slate-800 dark:bg-slate-900/60', 'aria-hidden': 'true' });
  const lines = () => { gutter.textContent = Array.from({ length: Math.max(1, area.value.split('\n').length) }, (_, i) => i + 1).join('\n'); };
  area.addEventListener('scroll', () => { gutter.scrollTop = area.scrollTop; });
  area.addEventListener('keydown', (e) => {
    if (e.key === 'Tab' && !e.shiftKey && canEdit) { // a tab key that indents, as an editor should
      e.preventDefault();
      const s = area.selectionStart;
      area.setRangeText('  ', s, area.selectionEnd, 'end');
      changed();
    }
  });
  area.addEventListener('input', () => changed());
  lines();

  const descIn = h('input', { id: 'desc', class: 'input', value: desc, maxlength: '300', placeholder: 'What this pipeline is for', readOnly: !canEdit });
  descIn.addEventListener('input', () => { desc = descIn.value; paint(); });

  const side = h('div', { class: 'space-y-4' });
  const actions = h('div', { class: 'flex flex-wrap items-center gap-2' });

  function dirty() { return isNew || text !== saved.yaml || desc !== (saved.description || ''); }

  function changed() { text = area.value; lines(); paint(); clearTimeout(timer); timer = setTimeout(validate, 350); }

  async function validate() {
    const mine = ++seq;
    try { const r = await post('/v1/pipelines/validate', { yaml: text }); if (mine === seq) { result = r; paint(); } }
    catch (e) { if (mine === seq) { result = { valid: false, problems: [{ path: '', msg: e.message }], warnings: [], missing_secrets: [] }; paint(); } }
  }

  async function save(thenRun) {
    if (busy) return;
    busy = true; paint();
    try {
      let p;
      if (isNew) p = await post('/v1/pipelines', { yaml: text, description: desc });
      else p = await put('/v1/pipelines/' + encodeURIComponent(id), { yaml: text, description: desc, base_version: baseVersion });
      toast(isNew ? 'Pipeline created' : 'Saved as version ' + p.version, 'ok');
      if (thenRun) { const r = await post('/v1/pipelines/' + encodeURIComponent(p.id) + '/run'); navigate('/runs/' + r.id); return; }
      if (isNew) { navigate('/pipelines/' + encodeURIComponent(p.id)); return; }
      saved = p; baseVersion = p.version;
    } catch (e) {
      if (e.status === 409 && e.body.current) await conflict(e.body.current, e.message);
      else toast(e.message, 'error');
    }
    busy = false; paint();
  }

  async function conflict(current, msg) {
    const choice = await modal('This pipeline changed', (close) => h('div', {},
      h('p', { class: 'text-sm' }, msg + '.'), h('p', { class: 'muted mt-2 text-sm' }, 'Load their version to see what changed (your edits stay on screen until you do), or overwrite it with yours.'),
      h('div', { class: 'mt-5 flex justify-end gap-2' },
        h('button', { class: 'btn-secondary', type: 'button', onclick: () => close('cancel') }, 'Keep editing'),
        h('button', { class: 'btn-secondary', type: 'button', onclick: () => close('load') }, 'Load their version'),
        h('button', { class: 'btn-danger', type: 'button', onclick: () => close('overwrite') }, 'Overwrite'))));
    if (choice === 'load') { saved = current; baseVersion = current.version; text = current.yaml; desc = current.description || ''; area.value = text; descIn.value = desc; lines(); validate(); }
    if (choice === 'overwrite') { baseVersion = current.version; saved = current; busy = false; await save(false); }
  }

  async function runSaved() {
    try { const r = await post('/v1/pipelines/' + encodeURIComponent(id) + '/run'); navigate('/runs/' + r.id); } catch (e) { toast(e.message, 'error'); }
  }

  async function remove() {
    if (!await confirmDialog({ title: 'Delete this pipeline?', body: 'The definition and its version history are removed. Past runs stay in the run list.', confirmLabel: 'Delete', danger: true, typeToConfirm: id })) return;
    try { await del('/v1/pipelines/' + encodeURIComponent(id)); toast('Deleted', 'ok'); navigate('/pipelines'); } catch (e) { toast(e.message, 'error'); }
  }

  async function history() {
    const fresh = await get('/v1/pipelines/' + encodeURIComponent(id));
    await modal('Version history', (close) => {
      const rows = [{ version: fresh.version, by: fresh.updated_by, at: fresh.updated_at, yaml: fresh.yaml, current: true }, ...(fresh.history || [])];
      const preview = h('pre', { class: 'mt-3 max-h-64 overflow-auto rounded-lg bg-slate-950 p-3 font-mono text-xs text-slate-100' }, 'Select a version to preview it.');
      return h('div', {}, h('ul', { class: 'divide-y divide-slate-200 dark:divide-slate-800' }, rows.map(v => h('li', { class: 'flex items-center justify-between gap-3 py-2.5' },
        h('div', {}, h('div', { class: 'text-sm font-medium' }, 'Version ' + v.version, v.current && h('span', { class: 'pill-ok ml-2' }, 'current')), h('div', { class: 'muted text-xs', title: absTime(v.at) }, relTime(v.at) + ' by ' + v.by)),
        h('div', { class: 'flex gap-2' },
          h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: () => { preview.textContent = v.yaml; } }, icon('eye', 'size-3.5'), 'View'),
          !v.current && canEdit && h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: async () => {
            try { const p = await post('/v1/pipelines/' + encodeURIComponent(id) + '/versions/' + v.version + '/restore'); toast('Restored as version ' + p.version, 'ok'); close(); navigate('/pipelines'); setTimeout(() => navigate('/pipelines/' + encodeURIComponent(id)), 0); } catch (e) { toast(e.message, 'error'); }
          } }, icon('history', 'size-3.5'), 'Restore'))))), preview);
    }, { wide: true });
  }

  function addStep() {
    const steps = result && result.graph ? result.graph.steps.map(s => s.id) : [];
    return modal('Add a step', (close) => {
      const idIn = h('input', { id: 'sid', class: 'input', placeholder: 'build', pattern: '[a-z][a-z0-9-]*' });
      const imgIn = h('input', { id: 'simg', class: 'input', value: 'alpine:3.24' });
      const cmd = h('textarea', { id: 'scmd', class: 'input h-28 font-mono text-[13px]', placeholder: 'echo hello', spellcheck: 'false' });
      const net = h('select', { id: 'snet', class: 'input' }, h('option', { value: '' }, 'Allowed (default)'), h('option', { value: 'none' }, 'None: no network at all'));
      const to = h('input', { id: 'sto', class: 'input', placeholder: '10m (optional)' });
      const needs = steps.map(s => ({ s, box: h('input', { type: 'checkbox', value: s, class: 'size-4 rounded' }) }));
      const secs = secretNames.map(s => ({ s, box: h('input', { type: 'checkbox', value: s, class: 'size-4 rounded' }) }));
      const err = h('div', { class: 'mt-3 hidden' });
      const checks = (items, empty) => h('div', { class: 'flex flex-wrap gap-x-4 gap-y-1.5' }, items.length ? items.map(i => h('label', { class: 'flex items-center gap-1.5 text-sm' }, i.box, i.s)) : h('span', { class: 'muted text-sm' }, empty));
      return h('div', { class: 'space-y-4' },
        h('div', { class: 'grid gap-4 sm:grid-cols-2' }, field('Step id', idIn, 'Lowercase letters, digits and hyphens.'), field('Container image', imgIn, 'Pin a version or digest.')),
        field('Command', cmd, 'A shell script, one command per line.'),
        field('Runs after', checks(needs, 'No other steps yet.'), 'Steps that must succeed first.'),
        h('div', { class: 'grid gap-4 sm:grid-cols-2' }, field('Network access', net), field('Time limit', to)),
        field('Secrets this step may read', checks(secs, 'No secrets exist yet. Add them under Secrets.'), 'The step receives only what you tick.'), err,
        h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'),
          h('button', { class: 'btn-primary', type: 'button', onclick: () => {
            const sid = idIn.value.trim();
            const fail = (m) => { mount(err, errorBox(m)); err.classList.remove('hidden'); };
            if (!/^[a-z][a-z0-9-]{0,62}$/.test(sid)) return fail('The step id must be lowercase letters, digits and hyphens, starting with a letter.');
            if (!cmd.value.trim()) return fail('Give the step something to run.');
            if (!imgIn.value.trim()) return fail('Give a container image.');
            if (steps.includes(sid)) return fail('A step called ' + sid + ' already exists.');
            const idx = text.search(/^steps:\s*$/m);
            if (idx < 0 || /^[A-Za-z]/m.test(text.slice(idx + 6))) return fail('The steps list is not at the end of the file, so the step cannot be added automatically. Add it by hand.');
            const pick = (items) => items.filter(i => i.box.checked).map(i => i.s);
            let b = `  - id: ${sid}\n    image: ${imgIn.value.trim()}\n    run: |\n` + cmd.value.replace(/\s+$/, '').split('\n').map(l => '      ' + l).join('\n') + '\n';
            if (pick(needs).length) b += `    needs: [${pick(needs).join(', ')}]\n`;
            const ps = pick(secs);
            if (net.value || ps.length) { b += '    permissions:\n'; if (net.value) b += `      network: ${net.value}\n`; if (ps.length) b += `      secrets: [${ps.join(', ')}]\n`; }
            if (to.value.trim()) b += `    timeout: ${to.value.trim()}\n`;
            area.value = text.replace(/\s*$/, '\n') + b; changed(); area.scrollTop = area.scrollHeight; close(); toast('Step added at the end', 'ok');
          } }, 'Add step')));
    });
  }

  function paint() {
    const r = result;
    const invalid = r && !r.valid;
    const can = canEdit && !busy;
    mount(actions,
      canEdit && h('button', { class: 'btn-secondary', type: 'button', onclick: addStep }, icon('plus', 'size-3.5'), 'Add step'),
      !isNew && h('button', { class: 'btn-ghost', type: 'button', onclick: history }, icon('history', 'size-4'), 'History'),
      canEdit && h('button', { class: 'btn-secondary', type: 'button', disabled: !can || !dirty() || invalid, onclick: () => save(false) }, icon('check', 'size-4'), isNew ? 'Create' : 'Save'),
      canEdit && h('button', { class: 'btn-primary', type: 'button', disabled: !can || invalid || (isNew ? false : false), onclick: () => (dirty() ? save(true) : runSaved()) }, icon('play', 'size-3.5'), dirty() ? (isNew ? 'Create & run' : 'Save & run') : 'Run'),
      !isNew && atLeast(session.role, 'admin') && h('button', { class: 'btn-ghost text-rose-600 dark:text-rose-400', type: 'button', onclick: remove, 'aria-label': 'Delete pipeline' }, icon('trash', 'size-4')));

    const panels = [];
    if (!r) panels.push(h('div', { class: 'card card-pad muted text-sm' }, 'Checking…'));
    else if (r.valid) panels.push(h('div', { class: 'card' },
      h('div', { class: 'flex items-center justify-between border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('h2', { class: 'h-section' }, 'Graph'), h('span', { class: 'pill-ok' }, icon('check', 'size-3'), 'Valid')),
      h('div', { class: 'p-3' }, dag(r.graph.steps.map(s => ({ id: s.id, needs: s.needs, status: 'pending', sub: s.network === 'none' ? 'no network' : s.secrets.length ? s.secrets.length + ' secret' + (s.secrets.length > 1 ? 's' : '') : s.image })), { fit: true })))
    );
    else panels.push(h('div', { class: 'card' },
      h('div', { class: 'flex items-center justify-between border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('h2', { class: 'h-section' }, 'Problems'), h('span', { class: 'pill-bad' }, r.problems.length + (r.problems.length === 1 ? ' problem' : ' problems'))),
      h('ul', { class: 'divide-y divide-slate-100 dark:divide-slate-800' }, r.problems.map(p => h('li', { class: 'px-5 py-3 text-sm' }, p.path && h('code', { class: 'mr-2 rounded bg-slate-100 px-1.5 py-0.5 font-mono text-xs dark:bg-slate-800' }, p.path), p.msg)))));
    if (r && r.valid && r.missing_secrets.length) panels.push(notice('warn', 'These secrets are requested but not set: ', h('strong', {}, r.missing_secrets.join(', ')), '. A run will fail at that step. ', atLeast(session.role, 'admin') && h('a', { href: '#/secrets', class: 'underline' }, 'Add them')));
    if (r && r.warnings.length) panels.push(h('div', { class: 'card card-pad' }, h('h2', { class: 'h-section mb-2' }, 'Suggestions'), h('ul', { class: 'space-y-2 text-sm' }, r.warnings.map(w => h('li', { class: 'flex gap-2' }, icon('info', 'mt-0.5 size-4 text-amber-500'), w)))));
    mount(side, panels);
  }

  mount(el,
    h('nav', { class: 'mb-3 flex items-center gap-1.5 text-sm muted', 'aria-label': 'Breadcrumb' }, h('a', { href: '#/pipelines', class: 'hover:underline' }, 'Pipelines'), icon('chevron', 'size-3'), h('span', {}, isNew ? 'New pipeline' : id)),
    pageHeader(isNew ? 'New pipeline' : id, isNew ? 'Describe the steps; the name inside the YAML becomes the pipeline name.' : `Version ${saved.version} · changed ${relTime(saved.updated_at)} by ${saved.updated_by}`, actions),
    !canEdit && h('div', { class: 'mb-4' }, notice('info', 'You can read this pipeline but not change it. Ask a developer or administrator for access.')),
    h('div', { class: 'mb-4' }, field('Description', descIn)),
    h('div', { class: 'grid gap-4 xl:grid-cols-2' },
      h('div', { class: 'card overflow-hidden' }, h('div', { class: 'flex' }, gutter, h('div', { class: 'min-w-0 flex-1' }, area))),
      h('div', {}, side)));
  paint();
  validate();
  el.cleanup = () => clearTimeout(timer);
  return el;
}
