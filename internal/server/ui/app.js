// Forgeline web console. See docs/design/006-console.md.
import { h, icon, mount, toast, spinner, errorBox } from './lib/dom.js';
import { get, post, atLeast, onSignedOut } from './lib/api.js';
import { route, start, stop, currentPath } from './lib/router.js';
import { loginPage, setupPage } from './pages/auth.js';
import { overviewPage } from './pages/overview.js';
import { runPage } from './pages/run.js';
import { pipelinesPage } from './pages/pipelines.js';
import { pipelineEditorPage } from './pages/editor.js';
import { actionsPage } from './pages/actions.js';
import { proposalsPage } from './pages/proposals.js';
import { agentsPage, agentPage, skillPage } from './pages/agents.js';
import { workflowPage, workflowRunPage } from './pages/workflows.js';
import { infraPage } from './pages/infra.js';
import { integrationsPage } from './pages/integrations.js';
import { secretsPage } from './pages/secrets.js';
import { peoplePage } from './pages/people.js';
import { auditPage } from './pages/audit.js';
import { accountPage } from './pages/account.js';
import { assistantButton } from './lib/assistant.js';

const root = document.getElementById('root');
export const session = { role: 'viewer', name: '', user: null };

const NAV = [
  { path: '/', label: 'Overview', icon: 'home', min: 'viewer' },
  { path: '/pipelines', label: 'Pipelines', icon: 'flow', min: 'viewer', also: ['/pipelines/', '/runs/'] },
  { path: '/actions', label: 'Actions', icon: 'bolt', min: 'developer' },
  { path: '/infrastructure', label: 'Infrastructure', icon: 'server', min: 'viewer' },
  { path: '/agents', label: 'Agents', icon: 'sparkle', min: 'developer', also: ['/agents/', '/skills/', '/workflows/', '/workflow-runs/'] },
  { path: '/proposals', label: 'Proposals', icon: 'check', min: 'viewer' },
  { path: '/integrations', label: 'Integrations', icon: 'plug', min: 'developer' },
  { path: '/secrets', label: 'Secrets', icon: 'key', min: 'developer' },
  { path: '/people', label: 'People', icon: 'users', min: 'admin' },
  { path: '/audit', label: 'Audit log', icon: 'shield', min: 'admin' },
];

function setTheme(dark) {
  document.documentElement.classList.toggle('dark', dark);
  try { localStorage.setItem('forgeline-theme', dark ? 'dark' : 'light'); } catch { /* storage blocked */ }
}

function isActive(item) {
  const p = currentPath().split('?')[0];
  if (item.path === '/') return p === '/';
  return p === item.path || p.startsWith(item.path + '/') || (item.also || []).some(a => p.startsWith(a));
}

function shell() {
  const outlet = h('main', { id: 'main', tabindex: '-1', class: 'mx-auto w-full max-w-7xl flex-1 px-4 py-6 outline-none sm:px-6 lg:px-8' });
  const nav = h('nav', { class: 'flex flex-col gap-0.5 px-3', 'aria-label': 'Main' });
  const renderNav = () => mount(nav, NAV.filter(i => atLeast(session.role, i.min)).map(i =>
    h('a', {
      href: '#' + i.path, onclick: () => sidebar.classList.add('hidden'),
      class: 'flex items-center gap-3 rounded-lg px-3 py-2 text-sm font-medium transition-colors ' + (isActive(i)
        ? 'bg-brand-50 text-brand-700 dark:bg-brand-500/15 dark:text-brand-200'
        : 'text-slate-600 hover:bg-slate-100 dark:text-slate-300 dark:hover:bg-slate-800'),
      'aria-current': isActive(i) ? 'page' : null,
    }, icon(i.icon, 'size-[18px]'), i.label)));

  const sidebar = h('aside', { class: 'fixed inset-y-0 left-0 z-40 hidden w-64 flex-col border-r border-slate-200 bg-white pt-4 lg:flex dark:border-slate-800 dark:bg-slate-900' },
    h('a', { href: '#/', class: 'mb-5 flex items-center gap-2.5 px-6' },
      h('img', { src: '/ui/favicon.svg', alt: '', class: 'size-8' }), h('span', { class: 'text-lg font-semibold tracking-tight' }, 'Forgeline')),
    nav,
    h('div', { class: 'mt-auto border-t border-slate-200 p-4 text-xs text-slate-500 dark:border-slate-800 dark:text-slate-400' }, h('div', { id: 'build-info' }, 'Delivery where AI agents run safely')));

  const dark = () => document.documentElement.classList.contains('dark');
  const themeBtn = h('button', { class: 'btn-ghost btn-sm', type: 'button', 'aria-label': 'Toggle dark mode', onclick: () => { setTheme(!dark()); themeBtn.replaceChildren(icon(dark() ? 'sun' : 'moon')); } }, icon(dark() ? 'sun' : 'moon'));

  const userMenu = h('div', { class: 'relative' });
  const renderUser = () => {
    const open = { v: false };
    const menu = h('div', { class: 'absolute right-0 top-full z-30 mt-2 hidden w-56 rounded-xl border border-slate-200 bg-white p-1.5 shadow-xl dark:border-slate-700 dark:bg-slate-900' },
      h('div', { class: 'px-3 py-2' }, h('div', { class: 'truncate text-sm font-medium' }, session.user?.name || session.name), h('div', { class: 'muted truncate text-xs' }, session.name + ' · ' + session.role)),
      h('hr', { class: 'my-1 border-slate-200 dark:border-slate-800' }),
      session.user && h('a', { href: '#/account', class: 'flex items-center gap-2 rounded-lg px-3 py-2 text-sm hover:bg-slate-100 dark:hover:bg-slate-800', onclick: () => menu.classList.add('hidden') }, icon('lock'), 'Account & password'),
      h('button', { type: 'button', class: 'flex w-full items-center gap-2 rounded-lg px-3 py-2 text-sm hover:bg-slate-100 dark:hover:bg-slate-800', onclick: signOut }, icon('logout'), 'Sign out'));
    const btn = h('button', { type: 'button', class: 'flex items-center gap-2 rounded-lg px-2 py-1.5 text-sm hover:bg-slate-100 dark:hover:bg-slate-800', 'aria-haspopup': 'menu', onclick: (e) => { e.stopPropagation(); menu.classList.toggle('hidden'); } },
      h('span', { class: 'flex size-7 items-center justify-center rounded-full bg-brand-600 text-xs font-semibold text-white' }, (session.name || '?').slice(0, 1).toUpperCase()),
      h('span', { class: 'hidden max-w-32 truncate sm:block' }, session.name), icon('chevron', 'size-3.5 rotate-90'));
    document.addEventListener('click', () => menu.classList.add('hidden'));
    mount(userMenu, btn, menu);
  };

  const top = h('header', { class: 'sticky top-0 z-30 flex h-14 items-center gap-3 border-b border-slate-200 bg-white/85 px-4 backdrop-blur lg:pl-72 dark:border-slate-800 dark:bg-slate-900/85' },
    h('button', { class: 'btn-ghost btn-sm lg:hidden', type: 'button', 'aria-label': 'Open menu', onclick: () => { sidebar.classList.toggle('hidden'); sidebar.classList.toggle('flex'); } }, icon('menu', 'size-5')),
    h('div', { class: 'flex-1' }), assistantButton(session.role), themeBtn, userMenu);

  renderUser(); renderNav();
  addEventListener('hashchange', renderNav);
  mount(root, sidebar, top, h('div', { class: 'flex min-h-[calc(100vh-3.5rem)] flex-col lg:pl-64' }, outlet));
  fetch('/healthz').catch(() => {});
  return outlet;
}

async function signOut() {
  stop(); // end this page's polling first, or it would hit the server with a dead session
  try { await post('/v1/auth/logout'); } catch { /* already signed out */ }
  boot();
}

function guard(min, page) {
  return (ctx) => atLeast(session.role, min) ? page(ctx) : forbidden();
}

function forbidden() {
  return h('div', { class: 'card card-pad mx-auto mt-10 max-w-md text-center' }, icon('lock', 'mx-auto size-8 text-slate-400'),
    h('h1', { class: 'mt-3 text-lg font-semibold' }, 'You do not have access to this page'),
    h('p', { class: 'muted mt-1 text-sm' }, 'Your role is ' + session.role + '. Ask an administrator if you need more.'),
    h('a', { href: '#/', class: 'btn-secondary mt-4' }, 'Back to the overview'));
}

function notFound(err) {
  return h('div', { class: 'card card-pad mx-auto mt-10 max-w-md text-center' }, icon('alert', 'mx-auto size-8 text-slate-400'),
    h('h1', { class: 'mt-3 text-lg font-semibold' }, err ? 'Something went wrong' : 'Page not found'),
    err && h('div', { class: 'mt-3 text-left' }, errorBox(err.message || String(err))),
    h('a', { href: '#/', class: 'btn-secondary mt-4' }, 'Back to the overview'));
}

let routesRegistered = false;
let signedOutHandled = false;

async function boot() {
  stop();
  mount(root, spinner());
  let state;
  try { state = await get('/v1/auth/state'); } catch (e) {
    mount(root, h('div', { class: 'mx-auto mt-24 max-w-md p-4' }, errorBox(e.message))); return;
  }
  if (state.setup_needed) { setupPage(root, boot); return; }
  if (!state.authenticated) { loginPage(root, boot); return; }
  session.role = state.role; session.name = state.actor; session.user = state.user;
  signedOutHandled = false;

  const outlet = shell();
  if (!routesRegistered) {
    routesRegistered = true;
    route('/', guard('viewer', overviewPage));
    route('/pipelines', guard('viewer', pipelinesPage));
    route('/pipelines/new', guard('developer', (c) => pipelineEditorPage({ ...c, params: { id: null } })));
    route('/pipelines/:id', guard('viewer', pipelineEditorPage));
    route('/runs/:id', guard('viewer', runPage));
    route('/actions', guard('developer', actionsPage));
    route('/agents', guard('developer', agentsPage));
    route('/agents/:id', guard('developer', agentPage));
    route('/skills/:id', guard('developer', skillPage));
    route('/workflows/:id', guard('developer', workflowPage));
    route('/workflow-runs/:id', guard('developer', workflowRunPage));
    route('/proposals', guard('viewer', proposalsPage));
    route('/proposals/:id', guard('viewer', proposalsPage));
    route('/infrastructure', guard('viewer', infraPage));
    route('/integrations', guard('developer', integrationsPage));
    route('/secrets', guard('developer', secretsPage));
    route('/people', guard('admin', peoplePage));
    route('/audit', guard('admin', auditPage));
    route('/account', guard('viewer', accountPage));
  }
  start(outlet, notFound);
}

onSignedOut(() => {
  if (signedOutHandled) return;
  signedOutHandled = true;
  toast('Your session ended. Please sign in again.', 'error');
  boot();
});

boot();
