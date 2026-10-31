import { h, icon, mount, pageHeader, pill, relTime, absTime, emptyState, spinner, errorBox, notice, toast, modal, confirmDialog, field } from '../lib/dom.js';
import { get, post, put, atLeast } from '../lib/api.js';
import { session } from '../app.js';

// Environments: temporary infrastructure and developer boxes. Ask for one, see what it costs and
// whether it starts at once or waits for an administrator, and it is deleted when its time is up.

const LABEL = { requested: 'Requested', provisioning: 'Creating', active: 'Active', destroying: 'Deleting', destroyed: 'Deleted', failed: 'Failed', rejected: 'Not created' };
const TTLS = [[1, '1 hour'], [2, '2 hours'], [4, '4 hours'], [8, '8 hours'], [24, '1 day'], [72, '3 days'], [168, '1 week']];
const usd = (n) => '$' + (n < 10 ? n.toFixed(2) : n.toFixed(0));
const statusPill = (s) => pill(s, LABEL[s] || s);

function timeLeft(iso) {
  const ms = new Date(iso) - Date.now();
  if (ms <= 0) return 'now';
  const m = Math.round(ms / 60000), hrs = Math.floor(m / 60);
  return hrs >= 48 ? Math.round(hrs / 24) + ' days' : hrs >= 1 ? `${hrs}h ${m % 60}m` : m + ' min';
}

export async function environmentsPage() {
  const admin = atLeast(session.role, 'admin');
  const el = h('div', {});
  let envs, policy, templates, timer;

  async function load() {
    try { [envs, policy, templates] = await Promise.all([get('/v1/envs').then(r => r.environments), get('/v1/envs/policy'), get('/v1/envs/templates').then(r => r.templates)]); } catch (e) { return mount(el, errorBox(e.message)); }
    draw();
    clearTimeout(timer);
    if (envs.some(e => ['requested', 'provisioning', 'destroying'].includes(e.status))) timer = setTimeout(() => { if (document.body.contains(el)) load(); }, 4000);
  }

  function draw() {
    const live = envs.filter(e => ['requested', 'provisioning', 'active', 'destroying'].includes(e.status));
    const spend = live.reduce((n, e) => n + e.monthly_usd, 0);
    mount(el, pageHeader('Environments', 'Temporary infrastructure and developer boxes. Each has an owner and a time limit, and is deleted by Forgeline when the time is up.',
      admin && h('button', { class: 'btn-secondary', id: 'env-policy', type: 'button', onclick: () => policyForm() }, 'Self-service policy'),
      h('button', { class: 'btn-primary', id: 'new-env', type: 'button', onclick: () => requestForm() }, icon('plus', 'size-4'), 'New environment')),
      h('p', { class: 'muted mb-4 text-sm', id: 'env-summary' }, `${live.length} running or on the way, about ${usd(spend)} a month while they last. `,
        policy.self_service ? `Self-service is on for ${policy.templates.length ? policy.templates.join(', ') : 'no templates'} (up to ${policy.max_ttl_hours} hours and ${usd(policy.max_monthly_usd)} a month).` : 'Self-service is off: an administrator approves each one under Infrastructure → Cloud changes.'),
      envs.length ? h('div', { class: 'card overflow-x-auto' }, h('table', { class: 'tbl', id: 'env-table' }, h('thead', {}, h('tr', {}, ['Environment', 'Template', 'Owner', 'Status', 'Time left', 'Cost'].map(x => h('th', {}, x)))),
        h('tbody', {}, envs.map(e => h('tr', { class: 'row-link', tabindex: '0', 'data-env': e.id, 'data-status': e.status, onclick: () => detail(e), onkeydown: (ev) => { if (ev.key === 'Enter') detail(e); } },
          h('td', {}, h('div', { class: 'font-medium' }, e.name, e.via_ai && h('span', { class: 'pill-idle ml-2', title: 'The assistant asked for this; it waits for an administrator' }, 'asked by the assistant')), h('div', { class: 'muted max-w-sm truncate text-xs' }, e.purpose)),
          h('td', { class: 'text-sm' }, templates.find(t => t.id === e.template)?.title || e.template), h('td', { class: 'text-sm' }, e.owner), h('td', {}, statusPill(e.status)),
          h('td', { class: 'whitespace-nowrap text-sm', title: e.expires_at ? absTime(e.expires_at) : '' }, e.status === 'active' && e.expires_at ? timeLeft(e.expires_at) : e.status === 'requested' || e.status === 'provisioning' ? h('span', { class: 'muted' }, e.ttl_hours + ' h once ready') : h('span', { class: 'muted' }, '–')),
          h('td', { class: 'whitespace-nowrap text-sm' }, '~' + usd(e.monthly_usd) + '/mo'))))))
        : emptyState('clock', 'No environments yet', 'Ask for a developer box or a QA sandbox. You will see the cost and whether it starts at once before you submit.'));
  }

  function detail(e) {
    const mine = e.owner === session.name, canAct = mine || admin;
    modal(e.name, (close) => {
      const hours = h('select', { id: 'extend-hours', class: 'input w-auto' }, [1, 2, 4, 8, 24].map(n => h('option', { value: String(n) }, '+' + n + (n === 1 ? ' hour' : ' hours'))));
      const note = e.note && h('p', { class: 'text-sm' }, e.note);
      return h('div', { class: 'space-y-4', id: 'env-detail' },
        h('div', { class: 'flex flex-wrap items-center gap-3' }, statusPill(e.status), h('span', { class: 'muted text-sm' }, `${templates.find(t => t.id === e.template)?.title || e.template} · owner ${e.owner} · requested ${relTime(e.requested_at)}`)),
        h('p', { class: 'text-sm' }, e.purpose),
        h('dl', { class: 'grid grid-cols-[auto_1fr] gap-x-6 gap-y-1.5 text-sm' },
          h('dt', { class: 'muted' }, 'Stack'), h('dd', { class: 'font-mono text-xs' }, e.stack),
          h('dt', { class: 'muted' }, 'Cost'), h('dd', {}, `about ${usd(e.monthly_usd)} a month, ${usd(e.monthly_usd * e.ttl_hours / 730)} for ${e.ttl_hours} hours`),
          e.expires_at && [h('dt', { class: 'muted' }, 'Deleted'), h('dd', { title: absTime(e.expires_at) }, e.status === 'active' ? 'in ' + timeLeft(e.expires_at) : absTime(e.expires_at))],
          e.extensions ? [h('dt', { class: 'muted' }, 'Extended'), h('dd', {}, e.extensions + ' time' + (e.extensions === 1 ? '' : 's'))] : null,
          e.params?.InstanceType && [h('dt', { class: 'muted' }, 'Machine'), h('dd', {}, `${e.params.InstanceType}, ${e.params.DiskGiB} GiB disk`)]),
        e.status === 'requested' ? notice('info', e.auto_approve ? 'Within the self-service policy: it starts as soon as its preview is ready.' : (e.via_ai ? 'The assistant asked for this on ' + e.owner + '\'s behalf and no person has read it yet, so it always waits for an administrator. ' : '') + 'Waiting for an administrator. They approve it under Infrastructure → Cloud changes, where the preview shows exactly what would be created.') : null,
        e.status === 'active' && e.template === 'dev-box' ? notice('info', 'Connect with Session Manager: ', h('code', { class: 'text-xs' }, 'aws ssm start-session --target <instance id>'), '. The instance id is in the stack outputs. It has no inbound ports.') : null,
        note ? (e.status === 'failed' || e.status === 'rejected' ? notice('warn', note) : note) : null,
        canAct && e.status === 'active' ? h('div', { class: 'flex flex-wrap items-center gap-2' }, hours, h('button', { class: 'btn-secondary btn-sm', id: 'extend-env', type: 'button', onclick: async () => {
          try { await post('/v1/envs/' + e.id + '/extend', { hours: Number(hours.value) }); toast('Extended', 'ok'); close(); load(); } catch (err) { toast(err.message, 'error'); }
        } }, 'Extend')) : null,
        canAct && (e.status === 'active' || e.status === 'failed') ? h('div', { class: 'flex justify-end' }, h('button', { class: 'btn-ghost btn-sm text-rose-600 dark:text-rose-400', id: 'destroy-env', type: 'button', onclick: async () => {
          if (!await confirmDialog({ title: 'Delete ' + e.name + ' now?', body: 'Everything in it is deleted, including data on its disk.', confirmLabel: 'Delete it', danger: true })) return;
          try { await post('/v1/envs/' + e.id + '/destroy'); toast('Deleting', 'ok'); close(); load(); } catch (err) { toast(err.message, 'error'); }
        } }, icon('trash', 'size-3.5'), 'Delete now')) : null);
    }, { wide: true });
  }

  function requestForm() {
    modal('New environment', (close) => {
      let tpl = templates[0];
      const name = h('input', { id: 'env-name', class: 'input', maxlength: '40', placeholder: 'Checkout branch testing' });
      const purpose = h('textarea', { id: 'env-purpose', class: 'input h-16', maxlength: '300', placeholder: 'What it is for. Whoever approves it reads this.' });
      const ttl = h('select', { id: 'env-ttl', class: 'input', onchange: quote }, TTLS.filter(t => t[0] <= policy.max_lifetime_hours).map(([v, l]) => h('option', { value: String(v), selected: v === 8 }, l)));
      const params = h('div', { class: 'grid gap-4 sm:grid-cols-2' });
      const inputs = {};
      const choices = h('div', { class: 'grid gap-3 sm:grid-cols-2' });
      const q = h('div', { id: 'env-quote' });
      const err = h('div', { class: 'hidden' });
      function drawChoices() {
        mount(choices, templates.map(t => h('button', { type: 'button', 'data-template': t.id, 'aria-pressed': String(t.id === tpl.id),
          class: 'rounded-xl border p-3 text-left transition hover:border-brand-400 ' + (t.id === tpl.id ? 'border-brand-500 ring-2 ring-brand-500/30' : 'border-slate-200 dark:border-slate-800'),
          onclick: () => { tpl = t; ttl.value = String(t.default_ttl_hours); drawChoices(); drawParams(); quote(); } },
          h('div', { class: 'font-medium' }, t.title), h('div', { class: 'muted mt-1 text-xs' }, t.description), h('div', { class: 'mt-2 text-xs' }, 'about ' + usd(t.monthly_usd) + ' a month'))));
      }
      function drawParams() {
        for (const k of Object.keys(inputs)) delete inputs[k];
        mount(params, (tpl.params || []).map(p => {
          const c = p.options ? h('select', { class: 'input', id: 'env-' + p.name, onchange: quote }, p.options.map(o => h('option', { value: o, selected: o === p.default }, o)))
            : h('input', { class: 'input', id: 'env-' + p.name, value: p.default || '', oninput: quote });
          inputs[p.name] = c;
          return field(p.label, c, null, 'env-' + p.name);
        }));
      }
      let seq = 0;
      async function quote() {
        const n = ++seq;
        try {
          const r = await post('/v1/envs/estimate', { template: tpl.id, ttl_hours: Number(ttl.value), params: Object.fromEntries(Object.entries(inputs).map(([k, c]) => [k, c.value])) });
          if (n !== seq) return;
          mount(q, notice(r.self_service ? 'ok' : 'info', h('div', {}, h('strong', {}, `About ${usd(r.for_ttl_usd)} for ${ttl.selectedOptions[0].text}`), ` (${usd(r.monthly_usd)} a month). `, r.self_service ? 'It starts as soon as its preview is ready: it is within the self-service policy.' : 'It needs an administrator\'s approval first. ' + r.why)));
        } catch (e) { if (n === seq) mount(q, errorBox(e.message)); }
      }
      const go = h('button', { class: 'btn-primary', type: 'button', id: 'env-save', onclick: async () => {
        err.classList.add('hidden'); go.disabled = true;
        try {
          await post('/v1/envs', { template: tpl.id, name: name.value, purpose: purpose.value, ttl_hours: Number(ttl.value), params: Object.fromEntries(Object.entries(inputs).map(([k, c]) => [k, c.value])) });
          toast('Requested', 'ok'); close(); load();
        } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; }
      } }, 'Request it');
      drawChoices(); drawParams(); ttl.value = String(tpl.default_ttl_hours); quote();
      return h('div', { class: 'space-y-4' }, choices, h('div', { class: 'grid gap-4 sm:grid-cols-2' }, field('Name', name, null, 'env-name'), field('Keep it for', ttl, 'It is deleted then, with everything in it. You can extend it.', 'env-ttl')),
        params, field('What it is for', purpose, null, 'env-purpose'), q, err,
        h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go));
    }, { wide: true });
  }

  function policyForm() {
    modal('Self-service policy', (close) => {
      const on = h('input', { type: 'checkbox', id: 'pol-on', checked: policy.self_service });
      const boxes = templates.map(t => h('label', { class: 'flex items-center gap-2 text-sm' }, h('input', { type: 'checkbox', 'data-pol-template': t.id, checked: policy.templates.includes(t.id) }), t.title, h('span', { class: 'muted text-xs' }, '~' + usd(t.monthly_usd) + '/mo')));
      const num = (id, v, min, max) => h('input', { id, class: 'input', type: 'number', min: String(min), max: String(max), value: String(v) });
      const ttl = num('pol-ttl', policy.max_ttl_hours, 1, 168), life = num('pol-life', policy.max_lifetime_hours, 1, 720), per = num('pol-per', policy.max_active_per_user, 1, 10), cost = num('pol-cost', policy.max_monthly_usd, 1, 500);
      const err = h('div', { class: 'hidden' });
      const go = h('button', { class: 'btn-primary', type: 'button', id: 'pol-save', onclick: async () => {
        err.classList.add('hidden'); go.disabled = true;
        try {
          await put('/v1/envs/policy', { self_service: on.checked, templates: [...document.querySelectorAll('[data-pol-template]:checked')].map(b => b.dataset.polTemplate),
            max_ttl_hours: Number(ttl.value), max_lifetime_hours: Number(life.value), max_active_per_user: Number(per.value), max_monthly_usd: Number(cost.value) });
          toast('Saved', 'ok'); close(); load();
        } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; }
      } }, 'Save policy');
      return h('div', { class: 'space-y-4' },
        notice('info', 'Self-service is the one way an environment starts without a second person. Requests inside these limits start on their own, in the name of the policy, and are audited. Everything else waits for an administrator as usual. Deleting when time is up is always automatic.'),
        h('label', { class: 'flex items-center gap-2 font-medium' }, on, 'Let developers start these without waiting'),
        h('div', {}, h('div', { class: 'h-section mb-1' }, 'Templates'), h('div', { class: 'space-y-1' }, boxes)),
        h('div', { class: 'grid gap-4 sm:grid-cols-2' }, field('Longest self-service request (hours)', ttl, null, 'pol-ttl'), field('Longest any environment may live (hours)', life, 'Extensions included.', 'pol-life'),
          field('Environments per person', per, null, 'pol-per'), field('Largest self-service cost ($ a month)', cost, null, 'pol-cost')), err,
        h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go));
    }, { wide: true });
  }

  mount(el, spinner());
  await load();
  el.cleanup = () => clearTimeout(timer);
  return el;
}
