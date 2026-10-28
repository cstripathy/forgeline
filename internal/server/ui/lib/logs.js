// Forgeline web console: live step logs over Server-Sent Events.
import { h, icon } from './dom.js';

const MAX_CHARS = 2_000_000; // keep the page responsive; the server stores the full log

/** Returns {el, close}. The stream replays the stored log, follows new output, and stops at the end. */
export function logViewer(runId, stepId, onEnd) {
  const pre = h('pre', { class: 'min-h-48 max-h-[28rem] overflow-auto whitespace-pre-wrap break-words rounded-lg bg-slate-950 p-4 font-mono text-[12.5px] leading-relaxed text-slate-100' });
  const status = h('span', { class: 'text-xs text-slate-500 dark:text-slate-400' }, 'Connecting…');
  let follow = true;
  const followBox = h('input', { type: 'checkbox', checked: true, id: 'follow-' + stepId, class: 'size-3.5 rounded border-slate-300', onchange: (e) => { follow = e.target.checked; } });
  let total = 0, es = null, closed = false;

  const open = () => {
    es = new EventSource(`/v1/runs/${encodeURIComponent(runId)}/steps/${encodeURIComponent(stepId)}/logs`);
    es.onopen = () => { status.textContent = 'Live'; };
    es.onmessage = (ev) => {
      let msg; try { msg = JSON.parse(ev.data); } catch { return; }
      if (total > MAX_CHARS) return;
      total += msg.text.length;
      pre.append(document.createTextNode(msg.text));
      if (total > MAX_CHARS) pre.append(document.createTextNode('\n[the console stopped showing output here; the server still has the whole log]\n'));
      if (follow) pre.scrollTop = pre.scrollHeight;
    };
    es.addEventListener('end', (ev) => {
      let st = ''; try { st = JSON.parse(ev.data).status; } catch { /* ignore */ }
      status.textContent = 'Finished' + (st ? ' (' + st + ')' : '');
      if (!total) pre.append(document.createTextNode('(this step wrote no output)'));
      close(); onEnd && onEnd(st);
    });
    es.onerror = () => { if (!closed) status.textContent = 'Reconnecting…'; }; // EventSource retries with Last-Event-ID
  };
  const close = () => { closed = true; es && es.close(); };
  open();

  const el = h('div', {},
    h('div', { class: 'mb-2 flex items-center justify-between' },
      h('div', { class: 'flex items-center gap-2 text-sm font-medium' }, icon('list', 'size-4'), 'Output ', status),
      h('label', { class: 'flex items-center gap-1.5 text-xs text-slate-500 dark:text-slate-400', for: 'follow-' + stepId }, followBox, 'Follow')),
    pre);
  return { el, close };
}
