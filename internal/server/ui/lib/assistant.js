// The assistant panel: a read-only helper that answers questions about the page you are on.
// It cannot change anything; a pipeline it drafts is text until you save it yourself.
import { h, icon, mount, errorBox, notice, copyButton, toast } from './dom.js';
import { get, post, atLeast } from './api.js';
import { currentPath, navigate } from './router.js';

const DRAFT_KEY = 'forgeline-draft';

/** What the page is about, so the server can attach the right data to the question. */
function pageContext() {
  const p = currentPath().split('?')[0];
  let m = p.match(/^\/runs\/([^/]+)$/);
  if (m) return { kind: 'run', id: decodeURIComponent(m[1]), label: 'this run' };
  m = p.match(/^\/pipelines\/([^/]+)$/);
  if (m && m[1] !== 'new') return { kind: 'pipeline', id: decodeURIComponent(m[1]), label: 'this pipeline' };
  if (p === '/infrastructure') return { kind: 'infra', id: '', label: 'the infrastructure' };
  return { kind: 'none', id: '', label: 'the overview' };
}

const SUGGESTIONS = {
  run: ['Why did this run fail?', 'Summarise what happened in this run.'],
  pipeline: ['Review this pipeline for problems.', 'How could this run faster?'],
  infra: ['Summarise what is running in AWS.', 'Is anything here worth cleaning up?'],
  none: ['What needs attention right now?', 'Draft a pipeline that builds and tests a Go project.'],
};

/** Splits a reply into text and fenced code blocks. Everything is later inserted as text. */
function parseReply(text) {
  const parts = [];
  const re = /```([a-zA-Z0-9_-]*)\n([\s\S]*?)```/g;
  let last = 0, m;
  while ((m = re.exec(text))) {
    if (m.index > last) parts.push({ kind: 'text', text: text.slice(last, m.index) });
    parts.push({ kind: 'code', lang: m[1].toLowerCase(), text: m[2].replace(/\n$/, '') });
    last = re.lastIndex;
  }
  if (last < text.length) parts.push({ kind: 'text', text: text.slice(last) });
  return parts;
}

export function assistantButton(role) {
  if (!atLeast(role, 'developer')) return null;
  let panel = null;
  const btn = h('button', { class: 'btn-secondary btn-sm', type: 'button', id: 'assistant-open', 'aria-haspopup': 'dialog', onclick: () => toggle() }, icon('sparkle'), h('span', { class: 'hidden sm:inline' }, 'Assistant'));

  function toggle() {
    if (panel) { panel.remove(); panel = null; return; }
    panel = build(() => { panel.remove(); panel = null; btn.focus(); }, role);
    document.body.append(panel);
    panel.querySelector('textarea')?.focus();
  }
  return btn;
}

function build(close, role) {
  const history = []; // {role, content}
  const list = h('div', { class: 'flex-1 space-y-4 overflow-y-auto px-4 py-4', 'aria-live': 'polite' });
  const ctxLabel = h('span', { class: 'font-medium' });
  const input = h('textarea', { id: 'assistant-input', class: 'input min-h-[2.75rem] flex-1 resize-none', rows: '2', maxlength: '4000', placeholder: 'Ask about this page…', 'aria-label': 'Your question' });
  const sendBtn = h('button', { class: 'btn-primary', type: 'submit', 'aria-label': 'Send' }, icon('send'));
  let busy = false, ready = null;

  const refreshCtx = () => { ctxLabel.textContent = pageContext().label; };
  refreshCtx();
  addEventListener('hashchange', refreshCtx);

  function bubble(who, ...kids) {
    const mine = who === 'user';
    return h('div', { class: 'flex ' + (mine ? 'justify-end' : 'justify-start') },
      h('div', { class: 'max-w-[92%] space-y-2 rounded-xl px-3.5 py-2.5 text-sm ' + (mine ? 'bg-brand-600 text-white' : 'border border-slate-200 bg-slate-50 dark:border-slate-700 dark:bg-slate-800') }, ...kids));
  }

  function replyNodes(text) {
    return parseReply(text).map(part => {
      if (part.kind === 'text') return h('p', { class: 'whitespace-pre-wrap break-words' }, part.text.trim());
      const isYaml = part.lang === 'yaml' || part.lang === 'yml';
      return h('div', { class: 'overflow-hidden rounded-lg bg-slate-950' },
        h('pre', { class: 'max-h-72 overflow-auto p-3 font-mono text-xs text-slate-100' }, part.text),
        h('div', { class: 'flex flex-wrap gap-2 border-t border-slate-800 p-2' }, copyButton(part.text, 'Copy'),
          isYaml && h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: () => {
            try { sessionStorage.setItem(DRAFT_KEY, part.text); } catch { toast('Your browser blocked saving the draft. Copy it instead.', 'error'); return; }
            close(); navigate('/pipelines'); setTimeout(() => navigate('/pipelines/new'), 0);
          } }, icon('plus', 'size-3.5'), 'Open as a new pipeline')));
    });
  }

  function empty() {
    const c = pageContext();
    mount(list, h('div', { class: 'space-y-3 text-sm' },
      h('p', { class: 'muted' }, 'I can explain failures, review pipelines and draft new ones. I can only read: I cannot change anything, and I never see secret values.'),
      h('div', { class: 'flex flex-col gap-2' }, (SUGGESTIONS[c.kind] || SUGGESTIONS.none).map(q =>
        h('button', { class: 'btn-secondary justify-start text-left', type: 'button', onclick: () => ask(q) }, q)))));
  }

  async function ask(question) {
    const q = (question ?? input.value).trim();
    if (!q || busy) return;
    if (!history.length) list.replaceChildren();
    busy = true; sendBtn.disabled = true; input.value = '';
    list.append(bubble('user', h('p', { class: 'whitespace-pre-wrap break-words' }, q)));
    const wait = h('div', { class: 'flex justify-start' }, h('div', { class: 'rounded-xl border border-slate-200 px-3.5 py-2.5 text-sm muted dark:border-slate-700', role: 'status' }, 'Thinking…'));
    list.append(wait); list.scrollTop = list.scrollHeight;
    const c = pageContext();
    try {
      const r = await post('/v1/assistant/chat', { message: q, history, context: { kind: c.kind, id: c.id } });
      history.push({ role: 'user', content: q }, { role: 'assistant', content: r.reply });
      wait.replaceWith(bubble('assistant', ...replyNodes(r.reply), h('div', { class: 'muted text-[11px]' }, 'Based on: ' + r.context + ((r.lookups || []).length ? ' · looked up: ' + r.lookups.join(', ') : ''))));
    } catch (e) {
      wait.replaceWith(h('div', {}, errorBox(e.message)));
    }
    busy = false; sendBtn.disabled = false; list.scrollTop = list.scrollHeight; input.focus();
  }

  const form = h('form', { class: 'flex items-end gap-2 border-t border-slate-200 p-3 dark:border-slate-800', onsubmit: (e) => { e.preventDefault(); ask(); } }, input, sendBtn);
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); form.requestSubmit(); } });

  const panel = h('aside', { class: 'fixed inset-y-0 right-0 z-40 flex w-full max-w-md flex-col border-l border-slate-200 bg-white shadow-2xl dark:border-slate-700 dark:bg-slate-900', role: 'dialog', 'aria-label': 'Assistant', id: 'assistant-panel' },
    h('div', { class: 'flex items-center justify-between border-b border-slate-200 px-4 py-3 dark:border-slate-800' },
      h('div', {}, h('div', { class: 'flex items-center gap-2 text-sm font-semibold' }, icon('sparkle'), 'Assistant'), h('div', { class: 'muted text-xs' }, 'Looking at ', ctxLabel)),
      h('div', { class: 'flex gap-1' },
        h('button', { class: 'btn-ghost btn-sm', type: 'button', 'aria-label': 'Clear the conversation', onclick: () => { history.length = 0; empty(); } }, icon('refresh')),
        h('button', { class: 'btn-ghost btn-sm', type: 'button', 'aria-label': 'Close the assistant', onclick: close }, icon('x')))),
    list, form);
  panel.addEventListener('keydown', (e) => { if (e.key === 'Escape') close(); });

  // Say so up front if it is not set up, instead of failing on the first question.
  get('/v1/assistant').then(cfg => {
    ready = cfg.ready;
    if (!cfg.ready) {
      form.classList.add('hidden');
      mount(list, notice('info', 'The assistant is not set up yet. ', atLeast(role, 'admin') ? h('a', { href: '#/integrations', class: 'underline', onclick: close }, 'Turn it on under Integrations.') : 'Ask an administrator to turn it on.'));
    } else empty();
  }).catch(e => mount(list, errorBox(e.message)));
  return panel;
}

/** A draft handed over by the assistant, once. */
export function takeDraft() {
  try { const d = sessionStorage.getItem(DRAFT_KEY); if (d) sessionStorage.removeItem(DRAFT_KEY); return d; } catch { return null; }
}
