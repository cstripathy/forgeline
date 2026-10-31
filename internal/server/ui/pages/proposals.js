import { h, icon, mount, pageHeader, relTime, absTime, pill, emptyState, spinner, errorBox, notice, toast, confirmDialog } from '../lib/dom.js';
import { get, post, atLeast } from '../lib/api.js';
import { session } from '../app.js';

// Changes the assistant drafted. Nothing here runs until a different administrator approves the
// exact text shown.
export async function proposalsPage({ params }) {
  const el = h('div', {});
  let timer;
  async function list() {
    let r;
    try { r = await get('/v1/ai/proposals'); } catch (e) { return mount(el, errorBox(e.message)); }
    mount(el, pageHeader('Proposals', 'Changes the assistant drafted for review. Nothing runs until a different administrator approves the exact text.'),
      r.proposals.length ? h('div', { class: 'card overflow-x-auto' }, h('table', { class: 'tbl' },
        h('thead', {}, h('tr', {}, ['Change', 'Status', 'Risk', 'Calls', 'Proposed by', 'When'].map(x => h('th', {}, x)))),
        h('tbody', {}, r.proposals.map(p => h('tr', {},
          h('td', {}, h('a', { href: '#/proposals/' + p.id, class: 'font-medium text-brand-700 hover:underline dark:text-brand-200' }, p.title)),
          h('td', {}, pill(p.status)),
          h('td', {}, h('span', { class: p.risk === 'high' ? 'pill-bad' : 'pill-idle' }, p.risk)),
          h('td', { class: 'text-sm' }, String(p.calls.length)),
          h('td', { class: 'text-sm' }, p.proposed_by, p.via_ai && h('span', { class: 'muted ml-1 text-xs' }, '· drafted by the assistant')),
          h('td', { class: 'muted text-sm', title: absTime(p.proposed_at) }, relTime(p.proposed_at)))))))
        : emptyState('check', 'No proposals yet', 'Ask the assistant to change something in AWS. It files a proposal here instead of acting.'));
  }

  async function detail(id) {
    let p;
    try { p = await get('/v1/ai/proposals/' + encodeURIComponent(id)); } catch (e) { return mount(el, errorBox(e.message)); }
    const admin = atLeast(session.role, 'admin');
    const mine = p.proposed_by === session.name;
    const waiting = p.status === 'proposed';
    const note = h('input', { id: 'proposal-note', class: 'input', placeholder: 'Optional note for the record', 'aria-label': 'Note' });
    const act = async (what, body) => {
      try { await post('/v1/ai/proposals/' + p.id + '/' + what, body); toast(what === 'approve' ? 'Approved. Applying…' : 'Done', 'ok'); } catch (e) { toast(e.message, 'error'); }
      detail(id);
    };
    const approve = h('button', { class: 'btn-primary', type: 'button', id: 'approve-proposal', onclick: async () => {
      const ok = await confirmDialog({ title: 'Approve and apply this change?', body: 'Forgeline will run exactly the script shown, against ' + targetLabel(p.target, p) + '. This changes your AWS account.', confirmLabel: 'Approve and apply' });
      if (ok) act('approve', { hash: p.hash, note: note.value });
    } }, icon('check', 'size-4'), 'Approve and apply');
    mount(el,
      h('nav', { class: 'mb-2 text-sm', 'aria-label': 'Breadcrumb' }, h('a', { href: '#/proposals', class: 'muted hover:underline' }, 'Proposals'), h('span', { class: 'muted' }, ' › '), h('span', {}, p.id)),
      pageHeader(p.title, 'Drafted by ' + p.proposed_by + (p.via_ai ? ' with the assistant' : '') + ' · ' + relTime(p.proposed_at), pill(p.status)),
      p.status === 'failed' && h('div', { class: 'mb-4' }, errorBox('The change did not complete cleanly. Check the account by hand.')),
      h('section', { class: 'card card-pad mb-4' }, h('h2', { class: 'h-section mb-2' }, 'Why'), h('p', { class: 'whitespace-pre-wrap text-sm' }, p.reason)),
      h('section', { class: 'card mb-4' }, h('div', { class: 'border-b border-slate-200 px-5 py-3 dark:border-slate-800' }, h('h2', { class: 'h-section' }, 'What it will do'),
          h('p', { class: 'muted text-xs' }, 'Account connection: ' + targetLabel(p.target, p) + ' · risk ' + p.risk)),
        h('table', { class: 'tbl' }, h('thead', {}, h('tr', {}, ['Service', 'Operation', 'Arguments'].map(x => h('th', {}, x)))),
          h('tbody', {}, p.calls.map(c => h('tr', {}, h('td', { class: 'font-mono text-sm' }, c.service), h('td', { class: 'font-mono text-sm font-semibold' }, c.operation), h('td', { class: 'font-mono text-xs break-all' }, c.args || '')))))),
      h('section', { class: 'card card-pad mb-4' }, h('h2', { class: 'h-section mb-2' }, 'The exact script'), h('pre', { class: 'overflow-x-auto rounded-lg bg-slate-950 p-3 font-mono text-xs text-slate-100', id: 'proposal-code' }, p.code),
        h('p', { class: 'muted mt-2 break-all font-mono text-[11px]' }, 'sha256 ' + p.hash)),
      waiting && (admin
        ? (mine ? notice('info', 'You proposed this, so someone else must approve it.')
          : h('section', { class: 'card card-pad mb-4' }, h('div', { class: 'flex flex-wrap items-end gap-3' }, h('div', { class: 'min-w-64 flex-1' }, note), approve,
            h('button', { class: 'btn-secondary', type: 'button', id: 'reject-proposal', onclick: () => act('reject', {}) }, 'Reject'))))
        : notice('info', 'An administrator will review this.')),
      waiting && (mine || admin) && h('button', { class: 'btn-ghost btn-sm', type: 'button', onclick: () => act('cancel', {}) }, 'Withdraw this proposal'),
      p.decided_by && h('p', { class: 'muted mb-4 text-sm' }, (p.status === 'rejected' ? 'Rejected by ' : p.status === 'cancelled' ? 'Withdrawn by ' : 'Approved by ') + p.decided_by + (p.note ? ' — ' + p.note : '')),
      !!(p.result || (p.ran_calls || []).length) && h('section', { class: 'card card-pad' }, h('h2', { class: 'h-section mb-2' }, 'What ran'),
        (p.ran_calls || []).length ? h('p', { class: 'mb-2 text-sm' }, 'AWS calls the sandbox reported: ', h('span', { class: 'font-mono' }, p.ran_calls.map(c => c.service + ' ' + c.operation).join(', '))) : null,
        h('pre', { class: 'max-h-64 overflow-auto rounded-lg bg-slate-100 p-3 font-mono text-xs dark:bg-slate-800' }, p.result || '')));
    clearInterval(timer);
    if (p.status === 'applying') timer = setInterval(() => { if (!document.hidden) detail(id); }, 2000);
  }

  mount(el, spinner());
  if (params && params.id) await detail(params.id); else await list();
  el.cleanup = () => clearInterval(timer);
  return el;
}

function targetLabel(t, p) {
  // An added account's target is acct/<id>/<AWS account number>/<profile>/<region>.
  const parts = (t || '/').split('/');
  if (parts[0] === 'acct' && parts.length === 5) return (p && p.account_name ? p.account_name + ' ' : '') + 'AWS account ' + parts[2] + ', profile ' + parts[3] + ', region ' + parts[4];
  const [profile, region] = parts;
  return (profile ? 'profile ' + profile : 'the server\'s own AWS identity') + ', region ' + region;
}
