// Forgeline web console: DOM helpers.
//
// Everything that reaches the page goes through h(), which only ever creates elements and text
// nodes. There is no innerHTML anywhere in the console: pipeline names, step names, log output,
// GitHub data and error messages are all untrusted, and this makes inserting them as markup
// impossible rather than merely careful.

const SVG_NS = 'http://www.w3.org/2000/svg';

/** h('div', {class: 'x', onclick: fn}, 'text', [child, child]) */
export function h(tag, props, ...children) {
  const el = SVG_TAGS.has(tag) ? document.createElementNS(SVG_NS, tag) : document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (v === false || v == null) continue;
    if (k.startsWith('on') && typeof v === 'function') el.addEventListener(k.slice(2).toLowerCase(), v);
    else if (k === 'class') el.setAttribute('class', v);
    else if (k === 'value' && 'value' in el) el.value = v;
    else if (k === 'checked' || k === 'disabled' || k === 'selected' || k === 'readOnly') el[k] = !!v;
    else if (k === 'style') throw new Error('inline styles are blocked by the CSP; use a class or el.style from script');
    else el.setAttribute(k, v === true ? '' : String(v));
  }
  append(el, children);
  return el;
}

const SVG_TAGS = new Set(['svg', 'path', 'g', 'rect', 'circle', 'line', 'text', 'title', 'defs', 'marker', 'polygon', 'tspan']);

export function append(el, children) {
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

export function clear(el) { while (el.firstChild) el.removeChild(el.firstChild); return el; }

export function mount(el, ...children) { clear(el); append(el, children); return el; }

// --- icons (fixed path data; nothing here comes from the server) ---

const ICONS = {
  home: 'M3 10.5 12 3l9 7.5V20a1 1 0 0 1-1 1h-5v-6H9v6H4a1 1 0 0 1-1-1z',
  flow: 'M4 6h6v4H4zM14 14h6v4h-6zM14 4h6v4h-6zM10 8h2a2 2 0 0 1 2 2v4M10 8h2a2 2 0 0 0 2-2',
  server: 'M4 5h16v6H4zM4 13h16v6H4zM8 8h.01M8 16h.01',
  plug: 'M9 2v5M15 2v5M6 7h12v4a6 6 0 0 1-12 0zM12 17v5',
  key: 'M15 7a4 4 0 1 1-3.9 5H4v-3h2v-2h3.1A4 4 0 0 1 15 7zM15 8h.01',
  users: 'M16 19v-1a4 4 0 0 0-4-4H7a4 4 0 0 0-4 4v1M9.5 10a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM21 19v-1a4 4 0 0 0-3-3.9M16 4.1a3 3 0 0 1 0 5.8',
  shield: 'M12 3 5 6v5c0 4.5 3 8.3 7 10 4-1.7 7-5.5 7-10V6z',
  list: 'M8 6h12M8 12h12M8 18h12M4 6h.01M4 12h.01M4 18h.01',
  cloud: 'M7 18a4 4 0 0 1-.5-8A6 6 0 0 1 18 9a4.5 4.5 0 0 1-.5 9z',
  play: 'M7 4v16l13-8z',
  stop: 'M6 6h12v12H6z',
  plus: 'M12 5v14M5 12h14',
  check: 'M5 12.5 10 17.5 19 7',
  x: 'M6 6l12 12M18 6 6 18',
  alert: 'M12 9v4M12 17h.01M10.3 3.9 2.4 18a2 2 0 0 0 1.7 3h15.8a2 2 0 0 0 1.7-3L13.7 3.9a2 2 0 0 0-3.4 0z',
  info: 'M12 8h.01M11 12h1v5h1M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0z',
  refresh: 'M20 11a8 8 0 0 0-14.9-3M4 5v4h4M4 13a8 8 0 0 0 14.9 3M20 19v-4h-4',
  trash: 'M4 7h16M10 11v6M14 11v6M6 7l1 12a1 1 0 0 0 1 1h8a1 1 0 0 0 1-1l1-12M9 7V4h6v3',
  edit: 'M4 20h4L19 9l-4-4L4 16zM14 6l4 4',
  copy: 'M9 9h10v10H9zM5 15V5h10',
  sun: 'M12 16a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM12 2v2M12 20v2M4.9 4.9l1.4 1.4M17.7 17.7l1.4 1.4M2 12h2M20 12h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4',
  moon: 'M20 14.5A8 8 0 0 1 9.5 4a8 8 0 1 0 10.5 10.5z',
  menu: 'M4 6h16M4 12h16M4 18h16',
  logout: 'M15 4h3a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2h-3M10 8l-4 4 4 4M6 12h10',
  chevron: 'M9 6l6 6-6 6',
  git: 'M6 3v12M18 9a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM6 21a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM18 9a9 9 0 0 1-9 9',
  history: 'M3 12a9 9 0 1 0 3-6.7L3 8M3 3v5h5M12 7v5l3 2',
  eye: 'M2 12s4-7 10-7 10 7 10 7-4 7-10 7S2 12 2 12zM12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6z',
  lock: 'M6 11h12v9H6zM8 11V8a4 4 0 0 1 8 0v3',
  bolt: 'M13 2 4 14h7l-1 8 9-12h-7z',
  box: 'M21 8 12 3 3 8v8l9 5 9-5zM3 8l9 5 9-5M12 13v8',
  clock: 'M12 7v5l3 2M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0z',
};

export function icon(name, cls = 'size-4') {
  const d = ICONS[name];
  if (!d) throw new Error('unknown icon ' + name);
  const svg = h('svg', { viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor', 'stroke-width': '1.8', 'stroke-linecap': 'round', 'stroke-linejoin': 'round', class: cls + ' shrink-0', 'aria-hidden': 'true' });
  svg.append(h('path', { d }));
  return svg;
}

// --- formatting ---

export function relTime(iso) {
  if (!iso) return '';
  const s = Math.round((Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 5) return 'just now';
  if (s < 60) return s + 's ago';
  if (s < 3600) return Math.floor(s / 60) + 'm ago';
  if (s < 86400) return Math.floor(s / 3600) + 'h ago';
  if (s < 86400 * 30) return Math.floor(s / 86400) + 'd ago';
  return new Date(iso).toLocaleDateString();
}

export function absTime(iso) { return iso ? new Date(iso).toLocaleString() : ''; }

export function duration(startIso, endIso) {
  if (!startIso) return '';
  const end = endIso ? new Date(endIso).getTime() : Date.now();
  return fmtSeconds((end - new Date(startIso).getTime()) / 1000);
}

export function fmtSeconds(s) {
  if (s == null || isNaN(s)) return '';
  if (s < 1) return '<1s';
  if (s < 60) return Math.round(s) + 's';
  if (s < 3600) return Math.floor(s / 60) + 'm ' + Math.round(s % 60) + 's';
  return Math.floor(s / 3600) + 'h ' + Math.floor((s % 3600) / 60) + 'm';
}

export function bytes(n) {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  return (n / Math.pow(1024, i)).toFixed(i ? 1 : 0) + ' ' + u[i];
}

export function pct(x) { return x == null ? '–' : Math.round(x * 100) + '%'; }

// --- status ---

const STATUS_PILL = {
  succeeded: ['pill-ok', 'Succeeded'], success: ['pill-ok', 'Success'], cached: ['pill-ok', 'Cached'],
  failed: ['pill-bad', 'Failed'], rejected: ['pill-bad', 'Rejected'],
  running: ['pill-run', 'Running'], executing: ['pill-run', 'Executing'], preparing: ['pill-run', 'Preparing'],
  queued: ['pill-wait', 'Queued'], pending: ['pill-wait', 'Pending'], proposed: ['pill-wait', 'Awaiting approval'],
  canceled: ['pill-idle', 'Canceled'], skipped: ['pill-idle', 'Skipped'], expired: ['pill-idle', 'Expired'],
};

export function pill(status, label) {
  const [cls, text] = STATUS_PILL[status] || ['pill-idle', status];
  const live = status === 'running' || status === 'executing' || status === 'preparing';
  return h('span', { class: cls }, h('span', { class: 'size-1.5 rounded-full bg-current ' + (live ? 'animate-pulse-dot' : '') }), label || text);
}

export function dotClass(status) {
  return ({
    succeeded: 'bg-emerald-500', cached: 'bg-emerald-400', failed: 'bg-rose-500', running: 'bg-sky-500',
    queued: 'bg-amber-400', pending: 'bg-amber-300', canceled: 'bg-slate-400', skipped: 'bg-slate-300',
  })[status] || 'bg-slate-300';
}

export function sparkline(statuses) {
  const slots = Array.from({ length: 12 }, (_, i) => statuses[statuses.length - 12 + i]);
  return h('div', { class: 'flex items-end gap-0.5', role: 'img', 'aria-label': 'Last runs: ' + (statuses.join(', ') || 'none') },
    slots.map(s => h('span', { class: 'h-5 w-1.5 rounded-sm ' + (s ? dotClass(s) : 'bg-slate-200 dark:bg-slate-800'), title: s || '' })));
}

// --- toast and dialogs ---

let toastHost;
export function toast(message, kind = 'info') {
  if (!toastHost) {
    toastHost = h('div', { class: 'pointer-events-none fixed bottom-4 right-4 z-[60] flex w-80 max-w-[calc(100vw-2rem)] flex-col gap-2', 'aria-live': 'polite' });
    document.body.append(toastHost);
  }
  const tone = kind === 'error' ? 'border-rose-300 bg-rose-50 text-rose-900 dark:border-rose-500/40 dark:bg-rose-950 dark:text-rose-100'
    : kind === 'ok' ? 'border-emerald-300 bg-emerald-50 text-emerald-900 dark:border-emerald-500/40 dark:bg-emerald-950 dark:text-emerald-100'
    : 'border-slate-200 bg-white text-slate-800 dark:border-slate-700 dark:bg-slate-900 dark:text-slate-100';
  const el = h('div', { class: 'pointer-events-auto flex items-start gap-2 rounded-lg border px-3 py-2.5 text-sm shadow-lg ' + tone, role: kind === 'error' ? 'alert' : 'status' },
    icon(kind === 'error' ? 'alert' : kind === 'ok' ? 'check' : 'info', 'mt-0.5 size-4'), h('div', { class: 'min-w-0 break-words' }, message));
  toastHost.append(el);
  setTimeout(() => el.remove(), kind === 'error' ? 8000 : 4000);
}

/** Opens a modal. build(close) returns the dialog body; resolves when closed. */
export function modal(title, build, { wide = false } = {}) {
  return new Promise(resolve => {
    const prev = document.activeElement;
    const close = (value) => { overlay.remove(); document.removeEventListener('keydown', onKey); prev && prev.focus && prev.focus(); resolve(value); };
    const onKey = (e) => { if (e.key === 'Escape') close(undefined); };
    const panel = h('div', { class: 'w-full ' + (wide ? 'max-w-3xl' : 'max-w-lg') + ' max-h-[90vh] overflow-y-auto rounded-xl border border-slate-200 bg-white shadow-2xl dark:border-slate-700 dark:bg-slate-900', role: 'dialog', 'aria-modal': 'true', 'aria-label': title },
      h('div', { class: 'flex items-center justify-between border-b border-slate-200 px-5 py-3.5 dark:border-slate-800' },
        h('h2', { class: 'text-base font-semibold' }, title),
        h('button', { class: 'btn-ghost btn-sm', type: 'button', 'aria-label': 'Close', onclick: () => close(undefined) }, icon('x'))),
      // A form, so Enter submits (it presses the primary button) and password managers behave.
      h('form', { class: 'p-5', novalidate: true, onsubmit: (e) => { e.preventDefault(); const go = e.target.querySelector('button.btn-primary, button.btn-danger'); if (go && !go.disabled) go.click(); } }, build(close)));
    const overlay = h('div', { class: 'fixed inset-0 z-50 flex items-start justify-center overflow-y-auto bg-slate-900/50 p-4 pt-[8vh] backdrop-blur-sm', onmousedown: (e) => { if (e.target === overlay) close(undefined); } }, panel);
    document.body.append(overlay);
    document.addEventListener('keydown', onKey);
    const first = panel.querySelector('input,textarea,select,button.btn-primary');
    first && first.focus();
  });
}

/** Asks for confirmation. If typeToConfirm is set the person must type it. Resolves true or false. */
export function confirmDialog({ title, body, confirmLabel = 'Confirm', danger = false, typeToConfirm = null }) {
  return modal(title, (close) => {
    const input = typeToConfirm ? h('input', { class: 'input mt-3', autocomplete: 'off', 'aria-label': 'Type ' + typeToConfirm + ' to confirm' }) : null;
    const ok = h('button', { class: danger ? 'btn-danger' : 'btn-primary', type: 'button', disabled: !!typeToConfirm, onclick: () => close(true) }, confirmLabel);
    if (input) input.addEventListener('input', () => { ok.disabled = input.value !== typeToConfirm; });
    return h('div', {}, h('div', { class: 'text-sm text-slate-600 dark:text-slate-300' }, body),
      typeToConfirm && h('div', { class: 'mt-3 text-sm' }, 'Type ', h('code', { class: 'rounded bg-slate-100 px-1.5 py-0.5 font-mono text-xs dark:bg-slate-800' }, typeToConfirm), ' to confirm.'), input,
      h('div', { class: 'mt-5 flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close(false) }, 'Cancel'), ok));
  }).then(v => v === true);
}

// --- small building blocks ---

export function pageHeader(title, subtitle, ...actions) {
  return h('div', { class: 'mb-6 flex flex-wrap items-start justify-between gap-3' },
    h('div', { class: 'min-w-0' }, h('h1', { class: 'h-page' }, title), subtitle && h('p', { class: 'muted mt-1 text-sm' }, subtitle)),
    h('div', { class: 'flex flex-wrap items-center gap-2' }, actions));
}

export function field(label, control, hint, id) {
  return h('div', {}, h('label', { class: 'label', for: id || control.id }, label), control, hint && h('p', { class: 'hint' }, hint));
}

export function emptyState(iconName, title, text, action) {
  return h('div', { class: 'flex flex-col items-center px-6 py-14 text-center' },
    h('div', { class: 'mb-3 rounded-full bg-slate-100 p-3 text-slate-500 dark:bg-slate-800' }, icon(iconName, 'size-6')),
    h('h3', { class: 'font-medium' }, title), text && h('p', { class: 'muted mt-1 max-w-sm text-sm' }, text), action && h('div', { class: 'mt-4' }, action));
}

export function errorBox(message) {
  return h('div', { class: 'flex items-start gap-2 rounded-lg border border-rose-200 bg-rose-50 px-3 py-2.5 text-sm text-rose-800 dark:border-rose-500/30 dark:bg-rose-950/50 dark:text-rose-200', role: 'alert' }, icon('alert', 'mt-0.5 size-4'), h('div', { class: 'min-w-0 break-words' }, message));
}

export function notice(kind, ...children) {
  const tone = kind === 'warn' ? 'border-amber-200 bg-amber-50 text-amber-900 dark:border-amber-500/30 dark:bg-amber-950/40 dark:text-amber-200'
    : kind === 'ok' ? 'border-emerald-200 bg-emerald-50 text-emerald-900 dark:border-emerald-500/30 dark:bg-emerald-950/40 dark:text-emerald-200'
    : 'border-sky-200 bg-sky-50 text-sky-900 dark:border-sky-500/30 dark:bg-sky-950/40 dark:text-sky-200';
  return h('div', { class: 'flex items-start gap-2 rounded-lg border px-3 py-2.5 text-sm ' + tone }, icon(kind === 'warn' ? 'alert' : kind === 'ok' ? 'check' : 'info', 'mt-0.5 size-4'), h('div', { class: 'min-w-0 break-words' }, children));
}

export function spinner() { return h('div', { class: 'flex justify-center py-16', role: 'status', 'aria-label': 'Loading' }, h('div', { class: 'size-6 animate-spin rounded-full border-2 border-slate-300 border-t-brand-600' })); }

export function copyButton(text, label = 'Copy') {
  const b = h('button', { class: 'btn-secondary btn-sm', type: 'button', onclick: async () => {
    try { await navigator.clipboard.writeText(text); toast('Copied', 'ok'); } catch { toast('Copy is blocked in this browser; select the text instead', 'error'); }
  } }, icon('copy', 'size-3.5'), label);
  return b;
}

export function kpi(label, value, sub, tone) {
  const toneCls = tone === 'bad' ? 'text-rose-600 dark:text-rose-400' : tone === 'ok' ? 'text-emerald-600 dark:text-emerald-400' : tone === 'run' ? 'text-sky-600 dark:text-sky-400' : '';
  return h('div', { class: 'card card-pad' }, h('div', { class: 'h-section' }, label),
    h('div', { class: 'mt-2 text-3xl font-semibold tabular-nums ' + toneCls }, value), sub && h('div', { class: 'muted mt-1 text-xs' }, sub));
}
