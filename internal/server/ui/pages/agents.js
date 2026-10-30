import { h, icon, mount, clear, pageHeader, relTime, absTime, emptyState, spinner, errorBox, notice, toast, modal, confirmDialog, field } from '../lib/dom.js';
import { get, post, del, atLeast } from '../lib/api.js';
import { navigate } from '../lib/router.js';
import { session } from '../app.js';
import { workflowList, workflowForm } from './workflows.js';

// Agents and skills. Every save is a new version; old versions are never edited, and a rollback is
// a copy of an old version saved as the newest one.

const tierPill = (t) => h('span', { class: t === 'T1' ? 'pill-wait' : 'pill-idle', title: t === 'T1' ? 'May file change proposals, which a person approves' : 'Reads and advises only' }, t === 'T1' ? 'T1 · proposes' : 'T0 · advises');
const shortHash = (x) => h('code', { class: 'text-xs', title: x }, x.slice(0, 12));

/** A line diff: lines only in `a` are removed, only in `b` are added. Small inputs, so a plain LCS. */
function lineDiff(a, b) {
  const x = (a || '').split('\n'), y = (b || '').split('\n');
  const dp = Array.from({ length: x.length + 1 }, () => new Array(y.length + 1).fill(0));
  for (let i = x.length - 1; i >= 0; i--) for (let j = y.length - 1; j >= 0; j--) dp[i][j] = x[i] === y[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
  const out = [];
  let i = 0, j = 0;
  while (i < x.length && j < y.length) {
    if (x[i] === y[j]) { out.push([' ', x[i]]); i++; j++; }
    else if (dp[i + 1][j] >= dp[i][j + 1]) out.push(['-', x[i++]]);
    else out.push(['+', y[j++]]);
  }
  while (i < x.length) out.push(['-', x[i++]]);
  while (j < y.length) out.push(['+', y[j++]]);
  return out;
}

function diffView(a, b) {
  const rows = lineDiff(a, b);
  if (!rows.some(r => r[0] !== ' ')) return h('p', { class: 'muted text-sm' }, 'No change in this part.');
  return h('pre', { class: 'overflow-x-auto rounded-lg border border-slate-200 p-3 text-xs leading-5 dark:border-slate-800', 'data-diff': '' }, rows.map(([k, line]) =>
    h('div', { class: k === '+' ? 'bg-emerald-50 text-emerald-800 dark:bg-emerald-950/40 dark:text-emerald-300' : k === '-' ? 'bg-rose-50 text-rose-800 dark:bg-rose-950/40 dark:text-rose-300' : 'muted' }, k + ' ' + line)));
}

function versionTable(versions, current, selected, onPick, onRollback) {
  return h('div', { class: 'overflow-x-auto' }, h('table', { class: 'tbl', id: 'version-table' },
    h('thead', {}, h('tr', {}, ['Version', 'Saved by', 'When', 'Note', 'Fingerprint', ''].map(x => h('th', {}, x)))),
    h('tbody', {}, [...versions].reverse().map(v => h('tr', { class: 'row-link' + (v.n === selected ? ' bg-brand-50/60 dark:bg-brand-950/20' : ''), tabindex: '0', 'data-version': String(v.n),
      onclick: () => onPick(v.n), onkeydown: (e) => { if (e.key === 'Enter') onPick(v.n); } },
      h('td', { class: 'whitespace-nowrap font-medium' }, 'v' + v.n, v.n === current && h('span', { class: 'pill-ok ml-2' }, 'current')),
      h('td', { class: 'text-sm' }, v.by), h('td', { class: 'muted whitespace-nowrap text-sm', title: absTime(v.at) }, relTime(v.at)),
      h('td', { class: 'max-w-xs truncate text-sm' }, v.note || h('span', { class: 'muted' }, '–')), h('td', {}, shortHash(v.hash)),
      h('td', { class: 'text-right' }, onRollback && v.n !== current && h('button', { class: 'btn-ghost btn-sm', type: 'button', 'data-rollback': String(v.n), onclick: (e) => { e.stopPropagation(); onRollback(v.n); } }, icon('history', 'size-3.5'), 'Roll back to this')))))));
}

// ------------------------------------------------------------------ list

export async function agentsPage({ query }) {
  const tab = query.tab === 'skills' ? 'skills' : query.tab === 'workflows' ? 'workflows' : 'agents';
  const admin = atLeast(session.role, 'admin');
  const el = h('div', {});
  let agents, skills, tools, workflows;
  try { [agents, skills, tools, workflows] = await Promise.all([get('/v1/ai/agents'), get('/v1/ai/skills'), get('/v1/ai/tools'), get('/v1/ai/workflows')]); } catch (e) { return mount(el, errorBox(e.message)); }

  const tabBtn = (id, label) => h('a', { href: '#/agents' + (id === 'agents' ? '' : '?tab=' + id), role: 'tab', 'aria-selected': String(tab === id), id: 'tab-' + id,
    class: 'rounded-lg px-3 py-1.5 text-sm font-medium ' + (tab === id ? 'bg-brand-50 text-brand-700 dark:bg-brand-950/40 dark:text-brand-200' : 'muted hover:bg-slate-100 dark:hover:bg-slate-800') }, label);

  const agentCards = agents.agents.length ? h('div', { class: 'grid gap-4 sm:grid-cols-2 lg:grid-cols-3' }, agents.agents.map(a =>
    h('a', { href: '#/agents/' + a.id, class: 'card card-pad block transition hover:border-brand-400', 'data-agent': a.id },
      h('div', { class: 'flex items-start justify-between gap-2' }, h('h3', { class: 'font-medium' }, a.name), a.disabled ? h('span', { class: 'pill-idle' }, 'off') : tierPill(a.tier)),
      h('p', { class: 'muted mt-1 line-clamp-2 text-sm' }, a.description || 'No description.'),
      h('div', { class: 'muted mt-3 flex flex-wrap gap-x-4 text-xs' }, h('span', {}, 'v' + a.current + ' of ' + a.versions), h('span', {}, a.tools.length + ' tools'), h('span', {}, a.skills.length + ' skills')))))
    : emptyState('sparkle', 'No agents yet', admin ? 'An agent is instructions, skills and a short list of tools. Create one, then talk to it here or let the assistant hand it work.' : 'An administrator can create agents.');

  const skillRows = skills.skills.length ? h('div', { class: 'card overflow-x-auto' }, h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Skill', 'Description', 'Version'].map(x => h('th', {}, x)))),
    h('tbody', {}, skills.skills.map(s => h('tr', { class: 'row-link', tabindex: '0', 'data-skill': s.id, onclick: () => navigate('/skills/' + s.id), onkeydown: (e) => { if (e.key === 'Enter') navigate('/skills/' + s.id); } },
      h('td', { class: 'font-medium' }, s.name), h('td', { class: 'muted max-w-lg truncate text-sm' }, s.description), h('td', { class: 'text-sm' }, 'v' + s.current))))))
    : emptyState('list', 'No skills yet', admin ? 'A skill is a procedure, such as how to right-size an ECS service, that agents follow.' : 'An administrator can create skills.');

  mount(el, pageHeader('Agents', 'Named workers with instructions, skills and a short list of tools. Each save is a new version you can read back and roll back to.',
    admin && (tab === 'agents' ? h('button', { class: 'btn-primary', id: 'new-agent', type: 'button', onclick: () => agentForm(null, skills.skills, tools.tools) }, icon('plus', 'size-4'), 'New agent')
      : tab === 'skills' ? h('button', { class: 'btn-primary', id: 'new-skill', type: 'button', onclick: () => skillForm(null) }, icon('plus', 'size-4'), 'New skill')
      : h('button', { class: 'btn-primary', id: 'new-workflow', type: 'button', onclick: () => workflowForm(null, agents.agents) }, icon('plus', 'size-4'), 'New workflow'))),
    h('div', { class: 'mb-4 flex gap-1', role: 'tablist' }, tabBtn('agents', 'Agents (' + agents.agents.length + ')'), tabBtn('skills', 'Skills (' + skills.skills.length + ')'), tabBtn('workflows', 'Workflows (' + workflows.workflows.length + ')')),
    tab === 'agents' ? agentCards : tab === 'skills' ? skillRows : workflowList(workflows.workflows, admin));
  return el;
}

// ------------------------------------------------------------------ skill detail

export async function skillPage({ params }) {
  const admin = atLeast(session.role, 'admin');
  const el = h('div', {});
  let sk, selected = 0;
  async function load(keep) {
    try { sk = await get('/v1/ai/skills/' + encodeURIComponent(params.id)); } catch (e) { return mount(el, errorBox(e.message)); }
    if (!keep || !sk.versions.some(v => v.n === selected)) selected = sk.current;
    draw();
  }
  function draw() {
    const v = sk.versions.find(x => x.n === selected), prev = sk.versions.find(x => x.n === selected - 1);
    mount(el, pageHeader(sk.name, sk.description, h('a', { class: 'btn-secondary btn-sm', href: '#/agents?tab=skills' }, 'All skills'),
      admin && h('button', { class: 'btn-primary btn-sm', id: 'new-version', type: 'button', onclick: () => skillForm(sk, load) }, icon('plus', 'size-3.5'), 'New version'),
      admin && h('button', { class: 'btn-ghost btn-sm text-rose-600 dark:text-rose-400', type: 'button', onclick: async () => {
        if (!await confirmDialog({ title: 'Delete ' + sk.name + '?', body: 'Its history is deleted too. Skills an agent version uses cannot be deleted.', confirmLabel: 'Delete', danger: true })) return;
        try { await del('/v1/ai/skills/' + sk.id); toast('Deleted', 'ok'); navigate('/agents?tab=skills'); } catch (e) { toast(e.message, 'error'); }
      } }, icon('trash', 'size-3.5'), 'Delete')),
      h('div', { class: 'space-y-4' },
        h('section', { class: 'card card-pad' }, h('div', { class: 'mb-2 flex items-center justify-between' }, h('h2', { class: 'h-section' }, 'Version ' + v.n), shortHash(v.hash)),
          h('pre', { class: 'whitespace-pre-wrap rounded-lg bg-slate-50 p-3 text-sm dark:bg-slate-900', id: 'skill-body' }, v.body)),
        prev && h('section', { class: 'card card-pad' }, h('h2', { class: 'h-section mb-2' }, `Changes from v${prev.n}`), diffView(prev.body, v.body)),
        h('section', { class: 'card' }, h('div', { class: 'border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('h2', { class: 'h-section' }, 'History')),
          versionTable(sk.versions, sk.current, selected, (n) => { selected = n; draw(); }, admin && rollback))));
  }
  async function rollback(n) {
    if (!await confirmDialog({ title: `Make v${n} current again?`, body: `It is saved as a new version with the same text as v${n}. Nothing is erased.`, confirmLabel: 'Roll back' })) return;
    try { await post('/v1/ai/skills/' + sk.id + '/rollback', { to: n }); toast('Rolled back', 'ok'); load(false); } catch (e) { toast(e.message, 'error'); }
  }
  mount(el, spinner());
  await load(false);
  return el;
}

function skillForm(sk, after) {
  modal(sk ? 'New version of ' + sk.name : 'New skill', (close) => {
    const cur = sk && sk.versions.find(v => v.n === sk.current);
    const name = h('input', { id: 'skill-name', class: 'input', maxlength: '60', value: sk ? sk.name : '', placeholder: 'ECS right-sizing' });
    const desc = h('input', { id: 'skill-desc', class: 'input', maxlength: '300', value: sk ? sk.description : '', placeholder: 'One line: when to use it' });
    const body = h('textarea', { id: 'skill-body-input', class: 'input h-56 font-mono text-xs', maxlength: '8192', placeholder: 'Numbered steps the agent should follow. Say what to look at, what to compare, and when to stop and ask.' }, cur ? cur.body : '');
    const note = h('input', { id: 'skill-note', class: 'input', maxlength: '200', placeholder: sk ? 'What changed and why' : 'First version' });
    const err = h('div', { class: 'hidden' });
    const go = h('button', { class: 'btn-primary', type: 'button', id: 'skill-save', onclick: async () => {
      err.classList.add('hidden'); go.disabled = true;
      try {
        const out = await post(sk ? '/v1/ai/skills/' + sk.id + '/versions' : '/v1/ai/skills', { name: name.value, description: desc.value, body: body.value, note: note.value });
        toast(sk ? 'Saved v' + out.current : 'Skill created', 'ok'); close();
        if (sk) after(false); else navigate('/skills/' + out.id);
      } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; }
    } }, sk ? 'Save as a new version' : 'Create skill');
    return h('div', { class: 'space-y-4' }, h('div', { class: 'grid gap-4 sm:grid-cols-2' }, field('Name', name, null, 'skill-name'), field('When to use it', desc, null, 'skill-desc')),
      field('Steps', body, 'Skills are trusted text, like the assistant\'s own instructions. Write only what you would be happy for an agent to follow.', 'skill-body-input'), field('Note', note, null, 'skill-note'), err,
      h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go));
  }, { wide: true });
}

// ------------------------------------------------------------------ agent detail

export async function agentPage({ params }) {
  const admin = atLeast(session.role, 'admin');
  const el = h('div', {});
  let ag, selected = 0, skills = [], tools = [];
  async function load(keep) {
    try { [ag, skills, tools] = await Promise.all([get('/v1/ai/agents/' + encodeURIComponent(params.id)), get('/v1/ai/skills').then(r => r.skills), get('/v1/ai/tools').then(r => r.tools)]); } catch (e) { return mount(el, errorBox(e.message)); }
    if (!keep || !ag.versions.some(v => v.n === selected)) selected = ag.current;
    draw();
  }
  const skillName = (id) => skills.find(s => s.id === id)?.name || id;
  function draw() {
    const v = ag.versions.find(x => x.n === selected), prev = ag.versions.find(x => x.n === selected - 1);
    const newer = (pin) => { const s = skills.find(x => x.id === pin.id); return s && s.current > pin.n ? s.current : 0; };
    const setDiff = (a, b) => { const A = new Set(a), B = new Set(b); return [...B].filter(x => !A.has(x)).map(x => '+ ' + x).concat([...A].filter(x => !B.has(x)).map(x => '− ' + x)); };
    const pinLabel = (p) => `${skillName(p.id)} v${p.n}`;
    mount(el, pageHeader(ag.name, ag.description, h('a', { class: 'btn-secondary btn-sm', href: '#/agents' }, 'All agents'),
      admin && h('button', { class: 'btn-primary btn-sm', id: 'new-version', type: 'button', onclick: () => agentForm(ag, skills, tools, () => load(false), v) }, icon('plus', 'size-3.5'), 'New version'),
      admin && h('button', { class: 'btn-secondary btn-sm', type: 'button', id: 'toggle-agent', onclick: async () => {
        try { await post('/v1/ai/agents/' + ag.id + (ag.disabled ? '/enable' : '/disable')); load(true); } catch (e) { toast(e.message, 'error'); }
      } }, ag.disabled ? 'Turn on' : 'Turn off')),
      ag.disabled && h('div', { class: 'mb-4' }, notice('warn', 'This agent is off. It cannot run and the assistant cannot hand it work.')),
      h('div', { class: 'grid gap-4 lg:grid-cols-3' },
        h('div', { class: 'space-y-4 lg:col-span-2' },
          h('section', { class: 'card card-pad' },
            h('div', { class: 'mb-3 flex flex-wrap items-center justify-between gap-2' }, h('h2', { class: 'h-section' }, 'Version ' + v.n + (v.n === ag.current ? ' (current)' : '')), h('div', { class: 'flex items-center gap-2' }, tierPill(v.tier), shortHash(v.hash))),
            h('div', { class: 'h-section mb-1' }, 'Instructions'), h('pre', { class: 'mb-4 whitespace-pre-wrap rounded-lg bg-slate-50 p-3 text-sm dark:bg-slate-900', id: 'agent-instructions' }, v.instructions),
            h('div', { class: 'h-section mb-1' }, 'Skills (pinned)'),
            v.skills.length ? h('ul', { class: 'mb-4 space-y-1 text-sm' }, v.skills.map(p => h('li', {}, h('a', { class: 'font-medium text-brand-700 hover:underline dark:text-brand-200', href: '#/skills/' + p.id }, pinLabel(p)),
              newer(p) ? h('span', { class: 'pill-wait ml-2', title: 'A newer version exists. Save a new version of this agent to use it.' }, 'v' + newer(p) + ' available') : null))) : h('p', { class: 'muted mb-4 text-sm' }, 'None.'),
            h('div', { class: 'h-section mb-1' }, 'Tools'),
            v.tools.length ? h('div', { class: 'flex flex-wrap gap-1.5' }, v.tools.map(t => h('code', { class: 'rounded bg-slate-100 px-1.5 py-0.5 text-xs dark:bg-slate-800' }, t))) : h('p', { class: 'muted text-sm' }, 'None: it can only answer from what you tell it.'),
            h('p', { class: 'muted mt-3 text-xs' }, 'Whoever talks to the agent, every tool runs as that person and with their role. The agent can never do more than they could.')),
          prev && h('section', { class: 'card card-pad' }, h('h2', { class: 'h-section mb-2' }, `Changes from v${prev.n}`),
            h('div', { class: 'h-section mb-1 mt-1' }, 'Instructions'), diffView(prev.instructions, v.instructions),
            (() => { const ch = [...setDiff(prev.skills.map(pinLabel), v.skills.map(pinLabel)).map(x => 'Skill ' + x), ...setDiff(prev.tools, v.tools).map(x => 'Tool ' + x), ...(prev.tier !== v.tier ? [`Tier ${prev.tier} → ${v.tier}`] : [])];
              return ch.length ? h('ul', { class: 'mt-3 space-y-0.5 text-sm', id: 'agent-changes' }, ch.map(c => h('li', {}, c))) : null; })()),
          h('section', { class: 'card' }, h('div', { class: 'border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('h2', { class: 'h-section' }, 'History')),
            versionTable(ag.versions, ag.current, selected, (n) => { selected = n; draw(); }, admin && rollback))),
        chatPanel(ag, v)));
  }
  async function rollback(n) {
    if (!await confirmDialog({ title: `Make v${n} current again?`, body: `It is saved as a new version with the same instructions, skills, tools and tier as v${n}. Nothing is erased.`, confirmLabel: 'Roll back' })) return;
    try { await post('/v1/ai/agents/' + ag.id + '/rollback', { to: n }); toast('Rolled back', 'ok'); load(false); } catch (e) { toast(e.message, 'error'); }
  }
  mount(el, spinner());
  await load(false);
  return el;
}

function chatPanel(ag, v) {
  const history = [];
  const list = h('div', { class: 'max-h-96 space-y-3 overflow-y-auto text-sm', id: 'agent-chat-log', 'aria-live': 'polite' },
    h('p', { class: 'muted' }, `Ask ${ag.name} (v${v.n}) something. It answers using only its own tools.`));
  const input = h('textarea', { id: 'agent-input', class: 'input min-h-[2.75rem] flex-1 resize-none', rows: '2', maxlength: '4000', placeholder: 'Ask this agent…', 'aria-label': 'Message to the agent' });
  const send = h('button', { class: 'btn-primary', type: 'submit', id: 'agent-send', 'aria-label': 'Send' }, icon('send'));
  const bubble = (who, ...kids) => h('div', { class: who === 'you' ? 'ml-6 rounded-lg bg-brand-50 p-2.5 dark:bg-brand-950/30' : 'mr-2 rounded-lg bg-slate-50 p-2.5 dark:bg-slate-900' }, ...kids);
  const form = h('form', { class: 'mt-3 flex gap-2', onsubmit: async (e) => {
    e.preventDefault();
    const msg = input.value.trim();
    if (!msg || send.disabled) return;
    input.value = ''; send.disabled = true;
    if (list.querySelector('p.muted')) clear(list);
    list.append(bubble('you', h('div', { class: 'whitespace-pre-wrap' }, msg)));
    const wait = bubble('agent', h('span', { class: 'muted' }, 'Thinking…'));
    list.append(wait); list.scrollTop = list.scrollHeight;
    try {
      const r = await post('/v1/ai/agents/' + ag.id + '/chat', { message: msg, history, version: v.n });
      history.push({ role: 'user', content: msg }, { role: 'assistant', content: r.reply });
      wait.replaceWith(bubble('agent', h('div', { class: 'whitespace-pre-wrap', 'data-reply': '' }, r.reply),
        r.lookups?.length ? h('div', { class: 'muted mt-2 text-xs' }, 'Looked up: ' + r.lookups.join(', ')) : null,
        r.proposals?.length ? h('div', { class: 'mt-2 text-xs' }, 'Filed: ', r.proposals.map(id => h('a', { class: 'underline', href: '#/proposals/' + id }, id))) : null));
    } catch (err) { wait.replaceWith(bubble('agent', errorBox(err.message))); }
    send.disabled = false; list.scrollTop = list.scrollHeight;
  } }, input, send);
  return h('aside', { class: 'card card-pad h-fit' }, h('h2', { class: 'h-section mb-3' }, 'Talk to this agent'), list, form);
}

function agentForm(ag, skills, tools, after, base) {
  modal(ag ? 'New version of ' + ag.name : 'New agent', (close) => {
    const cur = base || (ag && ag.versions.find(v => v.n === ag.current));
    const name = h('input', { id: 'agent-name', class: 'input', maxlength: '60', value: ag ? ag.name : '', placeholder: 'Cost watcher' });
    const desc = h('input', { id: 'agent-desc', class: 'input', maxlength: '300', value: ag ? ag.description : '', placeholder: 'What it is for, in one line' });
    const ins = h('textarea', { id: 'agent-ins', class: 'input h-32 text-sm', maxlength: '4096', placeholder: 'You look for idle or oversized resources and propose the smallest safe change. Ask before assuming.' }, cur ? cur.instructions : '');
    const tier = h('select', { id: 'agent-tier', class: 'input' }, [['T0', 'T0: reads and advises'], ['T1', 'T1: may also file change proposals']].map(([v, l]) => h('option', { value: v, selected: cur?.tier === v }, l)));
    const picked = new Set((cur?.skills || []).map(p => p.id));
    const pins = Object.fromEntries((cur?.skills || []).map(p => [p.id, p.n]));
    const skillBoxes = skills.length ? skills.map(s => h('label', { class: 'flex items-center gap-2 text-sm' }, h('input', { type: 'checkbox', 'data-skill': s.id, checked: picked.has(s.id), onchange: (e) => { if (!e.target.checked) delete pins[s.id]; } }), s.name, h('span', { class: 'muted text-xs' }, picked.has(s.id) ? `pinned v${pins[s.id]}, latest v${s.current}` : `latest v${s.current}`))) : [h('p', { class: 'muted text-sm' }, 'No skills yet. Create some under Skills.')];
    const toolSet = new Set(cur?.tools || []);
    const toolBoxes = tools.map(t => h('label', { class: 'flex items-start gap-2 text-sm', title: t.help }, h('input', { type: 'checkbox', class: 'mt-1', 'data-tool': t.name, checked: toolSet.has(t.name) }), h('span', {}, h('code', { class: 'text-xs' }, t.name), t.needs === 'mcp' && h('span', { class: 'muted text-xs' }, ' needs the AWS MCP Server'), t.min !== 'viewer' && h('span', { class: 'muted text-xs' }, ' · ' + t.min + '+'))));
    const note = h('input', { id: 'agent-note', class: 'input', maxlength: '200', placeholder: ag ? 'What changed and why' : 'First version' });
    const err = h('div', { class: 'hidden' });
    const form = h('div', {});
    const go = h('button', { class: 'btn-primary', type: 'button', id: 'agent-save', onclick: async () => {
      err.classList.add('hidden'); go.disabled = true;
      const body = { name: name.value, description: desc.value, instructions: ins.value, tier: tier.value, note: note.value,
        // A skill already pinned stays at its pinned version; a newly ticked one takes the latest.
        skills: [...form.querySelectorAll('[data-skill]:checked')].map(b => ({ id: b.dataset.skill, n: pins[b.dataset.skill] || 0 })),
        tools: [...form.querySelectorAll('[data-tool]:checked')].map(b => b.dataset.tool) };
      try {
        const out = await post(ag ? '/v1/ai/agents/' + ag.id + '/versions' : '/v1/ai/agents', body);
        toast(ag ? 'Saved v' + out.current : 'Agent created', 'ok'); close();
        if (ag) after(); else navigate('/agents/' + out.id);
      } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; }
    } }, ag ? 'Save as a new version' : 'Create agent');
    form.append(h('div', { class: 'space-y-4' }, h('div', { class: 'grid gap-4 sm:grid-cols-2' }, field('Name', name, null, 'agent-name'), field('Tier', tier, 'T1 may file proposals. A person still approves every change.', 'agent-tier')),
      field('Description', desc, null, 'agent-desc'), field('Instructions', ins, null, 'agent-ins'),
      h('div', {}, h('div', { class: 'h-section mb-1' }, 'Skills'), h('div', { class: 'space-y-1' }, skillBoxes), ag && h('p', { class: 'muted mt-1 text-xs' }, 'Skills stay on the version they were pinned to. Untick and tick one to take its latest version.')),
      h('div', {}, h('div', { class: 'h-section mb-1' }, 'Tools'), h('div', { class: 'grid gap-1.5 sm:grid-cols-2' }, toolBoxes), h('p', { class: 'muted mt-1 text-xs' }, 'Pick the fewest it needs. Every tool still runs as the person asking.')),
      field('Note', note, null, 'agent-note'), err,
      h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go)));
    return form;
  }, { wide: true });
}
