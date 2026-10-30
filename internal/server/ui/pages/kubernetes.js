import { h, icon, mount, pageHeader, emptyState, spinner, errorBox, notice, toast, modal, confirmDialog, field } from '../lib/dom.js';
import { get, post, del, atLeast } from '../lib/api.js';
import { navigate } from '../lib/router.js';
import { session } from '../app.js';
import { explorer } from './aws_explorer.js';

// Kubernetes clusters, read-only. An EKS cluster signs in through a connected AWS account and stores
// nothing; any other cluster uses a service-account token kept in the vault.

const ENVS = [['', 'No label'], ['production', 'Production'], ['staging', 'Staging'], ['development', 'Development'], ['sandbox', 'Sandbox']];
const host = (s) => { try { return new URL(s).host; } catch { return s; } };

export async function kubernetesPage({ params }) {
  return params.id ? clusterPage(params.id) : listPage();
}

async function listPage() {
  const admin = atLeast(session.role, 'admin');
  const el = h('div', {});
  let clusters, accounts = [];
  try { [clusters, accounts] = await Promise.all([get('/v1/k8s/clusters').then(r => r.clusters), get('/v1/aws/accounts').then(r => r.accounts).catch(() => [])]); } catch (e) { return mount(el, errorBox(e.message)); }
  mount(el, pageHeader('Kubernetes', 'Clusters Forgeline can read: what runs in them, and what Guard finds. Reading only; nothing here changes a cluster.',
    admin && h('button', { class: 'btn-primary', id: 'add-cluster', type: 'button', onclick: () => addForm(accounts) }, icon('plus', 'size-4'), 'Add cluster')),
    clusters.length ? h('div', { class: 'grid gap-4 sm:grid-cols-2 lg:grid-cols-3' }, clusters.map(c => h('a', { href: '#/kubernetes/' + c.id, class: 'card card-pad block transition hover:border-brand-400', 'data-cluster': c.id },
      h('div', { class: 'flex items-start justify-between gap-2' }, h('h3', { class: 'font-medium' }, c.name), c.env && h('span', { class: 'pill-idle' }, c.env)),
      h('p', { class: 'muted mt-1 truncate text-sm' }, host(c.server)),
      h('div', { class: 'muted mt-3 flex flex-wrap gap-x-4 text-xs' }, h('span', {}, c.version || 'unknown version'), h('span', {}, c.kind === 'eks' ? 'EKS · ' + c.cluster_name : 'service-account token')))))
      : emptyState('box', 'No clusters yet', admin ? 'Add an EKS cluster through a connected AWS account, or any cluster with a read-only service-account token.' : 'An administrator can add clusters.'));
  return el;
}

async function clusterPage(id) {
  const admin = atLeast(session.role, 'admin');
  const el = h('div', {});
  let c;
  try { c = (await get('/v1/k8s/clusters')).clusters.find(x => x.id === id); } catch (e) { return mount(el, errorBox(e.message)); }
  if (!c) return mount(el, errorBox('No such cluster.'));
  const ex = explorer({ dev: atLeast(session.role, 'developer'), url: '/v1/k8s/clusters/' + encodeURIComponent(id) + '/resources', scope: 'cluster' });
  mount(el, pageHeader(c.name, `${c.version || ''} · ${host(c.server)}${c.kind === 'eks' ? ' · EKS ' + c.cluster_name : ''}`,
    h('a', { class: 'btn-secondary btn-sm', href: '#/kubernetes' }, 'All clusters'), h('a', { class: 'btn-secondary btn-sm', href: '#/guard' }, icon('shield', 'size-3.5'), 'Guard findings'),
    admin && h('button', { class: 'btn-ghost btn-sm text-rose-600 dark:text-rose-400', id: 'remove-cluster', type: 'button', onclick: async () => {
      if (!await confirmDialog({ title: 'Remove ' + c.name + '?', body: 'Forgeline forgets the cluster and its stored token. Nothing in the cluster changes.', confirmLabel: 'Remove', danger: true })) return;
      try { await del('/v1/k8s/clusters/' + encodeURIComponent(id)); toast('Removed', 'ok'); navigate('/kubernetes'); } catch (e) { toast(e.message, 'error'); }
    } }, icon('trash', 'size-3.5'), 'Remove')), ex);
  return el;
}

function addForm(accounts) {
  modal('Add a Kubernetes cluster', (close) => {
    let kind = 'token';
    const name = h('input', { id: 'k-name', class: 'input', maxlength: '40', placeholder: 'Shop staging' });
    const env = h('select', { id: 'k-env', class: 'input' }, ENVS.map(([v, l]) => h('option', { value: v }, l)));
    const kindSel = h('select', { id: 'k-kind', class: 'input', onchange: (e) => { kind = e.target.value; draw(); } }, [['token', 'A service-account token'], ['eks', 'EKS, through an AWS account']].map(([v, l]) => h('option', { value: v }, l)));
    const server = h('input', { id: 'k-server', class: 'input font-mono', placeholder: 'https://ABC123.gr7.ap-south-1.eks.amazonaws.com' });
    const token = h('textarea', { id: 'k-token', class: 'input h-20 font-mono text-xs', placeholder: 'A token for a service account that can only get, list and watch. It is stored in the vault.' });
    const ca = h('textarea', { id: 'k-ca', class: 'input h-20 font-mono text-xs', placeholder: '-----BEGIN CERTIFICATE-----  (leave empty if the server certificate is publicly trusted)' });
    const acct = h('select', { id: 'k-account', class: 'input' }, accounts.map(a => h('option', { value: a.id }, a.name)));
    const cname = h('input', { id: 'k-cluster', class: 'input', placeholder: 'prod-eks' });
    const region = h('input', { id: 'k-region', class: 'input', value: 'ap-south-1' });
    const fields = h('div', { class: 'space-y-4' });
    const err = h('div', { class: 'hidden' });
    function draw() {
      mount(fields, kind === 'token' ? [field('API server', server, 'Must be https (http only for a cluster on this machine).', 'k-server'), field('Token', token, null, 'k-token'), field('Certificate authority', ca, null, 'k-ca')]
        : [accounts.length ? h('div', { class: 'grid gap-4 sm:grid-cols-3' }, field('AWS account', acct, null, 'k-account'), field('EKS cluster name', cname, null, 'k-cluster'), field('Region', region, null, 'k-region')) : notice('warn', 'Connect an AWS account first (Infrastructure → AWS account).')]);
    }
    draw();
    const go = h('button', { class: 'btn-primary', type: 'button', id: 'k-save', onclick: async () => {
      err.classList.add('hidden'); go.disabled = true; go.textContent = 'Checking the cluster…';
      try {
        const body = { name: name.value, env: env.value, kind };
        if (kind === 'token') Object.assign(body, { server: server.value, token: token.value, ca: ca.value });
        else Object.assign(body, { account: acct.value, cluster_name: cname.value, region: region.value });
        const c = await post('/v1/k8s/clusters', body);
        toast('Added ' + c.name, 'ok'); close(); navigate('/kubernetes/' + c.id);
      } catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; go.textContent = 'Add cluster'; }
    } }, 'Add cluster');
    return h('div', { class: 'space-y-4' }, notice('info', 'Read-only. Forgeline proves it can read the cluster before saving anything. Use a role that can only get, list and watch.'),
      h('div', { class: 'grid gap-4 sm:grid-cols-3' }, field('Name', name, null, 'k-name'), field('Environment', env, null, 'k-env'), field('Connect with', kindSel, null, 'k-kind')), fields, err,
      h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go));
  }, { wide: true });
}
