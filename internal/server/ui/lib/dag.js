// Forgeline web console: pipeline graph drawing.
import { h, dotClass } from './dom.js';

const NODE_W = 196, NODE_H = 58, GAP_X = 72, GAP_Y = 22, PAD = 16;

/** Orders steps into columns by dependency depth. steps: [{id, needs}] */
export function layers(steps) {
  const byId = new Map(steps.map(s => [s.id, s]));
  const depth = new Map();
  const visit = (id, seen = new Set()) => {
    if (depth.has(id)) return depth.get(id);
    if (seen.has(id)) return 0; // a cycle; the server rejects those, do not hang on one
    seen.add(id);
    const s = byId.get(id);
    const d = s && s.needs && s.needs.length ? 1 + Math.max(...s.needs.filter(n => byId.has(n)).map(n => visit(n, seen)), -1) : 0;
    depth.set(id, d);
    return d;
  };
  steps.forEach(s => visit(s.id));
  const cols = [];
  steps.forEach(s => { const d = depth.get(s.id); (cols[d] = cols[d] || []).push(s); });
  return cols.filter(Boolean);
}

const STROKE = {
  succeeded: 'stroke-emerald-500', cached: 'stroke-emerald-400', failed: 'stroke-rose-500', running: 'stroke-sky-500',
  queued: 'stroke-amber-400', pending: 'stroke-slate-300 dark:stroke-slate-600', canceled: 'stroke-slate-400', skipped: 'stroke-slate-300 dark:stroke-slate-600',
};
const BAR = {
  succeeded: 'fill-emerald-500', cached: 'fill-emerald-400', failed: 'fill-rose-500', running: 'fill-sky-500',
  queued: 'fill-amber-400', pending: 'fill-slate-300 dark:fill-slate-600', canceled: 'fill-slate-400', skipped: 'fill-slate-300 dark:fill-slate-600',
};

/**
 * Draws the graph. steps: [{id, needs, status?, sub?}]. onSelect(id) makes nodes clickable.
 * Returns an SVG element wrapped in a scroll container.
 */
export function dag(steps, { selected = null, onSelect = null, fit = false } = {}) {
  const cols = layers(steps);
  const pos = new Map();
  let maxRows = 0;
  cols.forEach((col, ci) => {
    maxRows = Math.max(maxRows, col.length);
    col.forEach((s, ri) => pos.set(s.id, { x: PAD + ci * (NODE_W + GAP_X), y: PAD + ri * (NODE_H + GAP_Y) }));
  });
  // Centre shorter columns vertically so edges look balanced.
  cols.forEach((col, ci) => {
    const offset = ((maxRows - col.length) * (NODE_H + GAP_Y)) / 2;
    col.forEach(s => { pos.get(s.id).y += offset; });
  });
  const width = PAD * 2 + cols.length * NODE_W + Math.max(0, cols.length - 1) * GAP_X;
  const height = PAD * 2 + maxRows * NODE_H + Math.max(0, maxRows - 1) * GAP_Y;

  const svg = h('svg', { viewBox: `0 0 ${width} ${height}`, width, height, role: 'group', 'aria-label': 'Pipeline graph', class: fit ? 'block h-auto min-w-[480px] max-w-full' : 'block' });
  const defs = h('defs', {});
  defs.append(h('marker', { id: 'arrow', viewBox: '0 0 10 10', refX: '8', refY: '5', markerWidth: '7', markerHeight: '7', orient: 'auto-start-reverse' },
    h('path', { d: 'M0 0 10 5 0 10z', class: 'fill-slate-400 dark:fill-slate-500' })));
  svg.append(defs);

  for (const s of steps) {
    for (const n of s.needs || []) {
      const a = pos.get(n), b = pos.get(s.id);
      if (!a || !b) continue;
      const x1 = a.x + NODE_W, y1 = a.y + NODE_H / 2, x2 = b.x - 2, y2 = b.y + NODE_H / 2, mx = (x1 + x2) / 2;
      svg.append(h('path', { d: `M${x1} ${y1} C${mx} ${y1} ${mx} ${y2} ${x2} ${y2}`, fill: 'none', class: 'stroke-slate-300 dark:stroke-slate-600', 'stroke-width': '1.8', 'marker-end': 'url(#arrow)' }));
    }
  }
  for (const s of steps) {
    const p = pos.get(s.id);
    const status = s.status || 'pending';
    const isSel = selected === s.id;
    const g = h('g', {
      transform: `translate(${p.x} ${p.y})`, tabindex: onSelect ? '0' : null, role: onSelect ? 'button' : null,
      'aria-label': `${s.id}, ${status}`, class: onSelect ? 'cursor-pointer outline-none [&:focus-visible>rect:first-child]:stroke-brand-500' : '',
      onclick: onSelect ? () => onSelect(s.id) : null,
      onkeydown: onSelect ? (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); onSelect(s.id); } } : null,
    });
    g.append(
      h('rect', { width: NODE_W, height: NODE_H, rx: '10', 'stroke-width': isSel ? '2.4' : '1.6', class: 'fill-white dark:fill-slate-900 ' + (isSel ? 'stroke-brand-500' : STROKE[status] || 'stroke-slate-300') }),
      h('rect', { x: '0', y: '0', width: '6', height: NODE_H, rx: '3', class: BAR[status] || 'fill-slate-300' }),
      h('text', { x: '18', y: '24', class: 'fill-slate-900 text-[13px] font-semibold dark:fill-slate-100' }, clip(s.id, 22)),
      h('text', { x: '18', y: '43', class: 'fill-slate-500 text-[11px] dark:fill-slate-400' }, clip(s.sub || status, 28)),
    );
    if (s.title) g.append(h('title', {}, s.title));
    svg.append(g);
  }
  const wrap = h('div', { class: 'overflow-auto' }, svg);
  return wrap;
}

function clip(s, n) { s = String(s); return s.length > n ? s.slice(0, n - 1) + '…' : s; }

export { dotClass };
