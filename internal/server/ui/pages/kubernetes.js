import { h, icon, mount, pageHeader, emptyState, spinner, errorBox, notice, toast, modal, confirmDialog, field } from '../lib/dom.js';
import { get, post, del, atLeast } from '../lib/api.js';
import { navigate } from '../lib/router.js';
import { session } from '../app.js';
import { explorer } from './aws_explorer.js';

// Kubernetes clusters. Reading is open to everyone; a change (scale, restart, set an image) is a proposal
// that a different administrator approves after seeing the cluster's own dry run. An EKS cluster signs in through a connected AWS account and stores
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
    atLeast(session.role, 'developer') && h('button', { class: 'btn-primary btn-sm', id: 'propose-k8s', type: 'button', onclick: () => changeForm(c) }, icon('plus', 'size-3.5'), 'Propose a change'),
    admin && h('button', { class: 'btn-ghost btn-sm text-rose-600 dark:text-rose-400', id: 'remove-cluster', type: 'button', onclick: async () => {
      if (!await confirmDialog({ title: 'Remove ' + c.name + '?', body: 'Forgeline forgets the cluster and its stored token. Nothing in the cluster changes.', confirmLabel: 'Remove', danger: true })) return;
      try { await del('/v1/k8s/clusters/' + encodeURIComponent(id)); toast('Removed', 'ok'); navigate('/kubernetes'); } catch (e) { toast(e.message, 'error'); }
    } }, icon('trash', 'size-3.5'), 'Remove')), ex);
  return el;
}

function changeForm(c) {
  modal('Propose a change to ' + c.name, (close) => {
    const KINDS = [['k8s-scale', 'Scale'], ['k8s-restart', 'Restart (rolling)'], ['k8s-image', 'Set a container image']];
    let kind = 'k8s-scale';
    const ns = h('input', { id: 'kc-ns', class: 'input', placeholder: 'shop' });
    const wk = h('select', { id: 'kc-kind', class: 'input w-auto' }, [['deployment', 'Deployment'], ['statefulset', 'StatefulSet']].map(([v, l]) => h('option', { value: v }, l)));
    const name = h('input', { id: 'kc-name', class: 'input', placeholder: 'web' });
    const replicas = h('input', { id: 'kc-replicas', class: 'input', type: 'number', min: '0', max: '100', value: '3' });
    const container = h('input', { id: 'kc-container', class: 'input', placeholder: 'web' });
    const image = h('input', { id: 'kc-image', class: 'input font-mono', placeholder: 'registry/app@sha256:…' });
    const reason = h('textarea', { id: 'kc-reason', class: 'input h-20', maxlength: '500', placeholder: 'Why? The approver reads this.' });
    const what = h('div', { class: 'grid gap-4 sm:grid-cols-2' });
    const err = h('div', { class: 'hidden' });
    const draw = () => mount(what, kind === 'k8s-scale' ? field('Replicas', replicas, 'Zero stops the workload.', 'kc-replicas') : kind === 'k8s-image' ? [field('Container', container, null, 'kc-container'), field('Image', image, 'Pin by digest where you can.', 'kc-image')] : null);
    draw();
    const go = h('button', { class: 'btn-primary', type: 'button', id: 'kc-go', onclick: async () => {
      err.classList.add('hidden'); go.disabled = true;
      const body = { kind, cluster: c.id, namespace: ns.value.trim(), workload: wk.value + '/' + name.value.trim(), reason: reason.value };
      if (kind === 'k8s-scale') body.replicas = Number(replicas.value);
      if (kind === 'k8s-image') Object.assign(body, { container: container.value.trim(), image: image.value.trim() });
      try { await post('/v1/cloud/changes', body); toast('Proposed. The cluster is dry-running it; someone else must approve.', 'ok'); close(); navigate('/infrastructure?tab=changes'); }
      catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; }
    } }, 'Propose change');
    return h('div', { class: 'space-y-4' }, notice('info', 'This only proposes it. The cluster dry-runs the change so the approver sees exactly what would differ, and nothing is applied until a different administrator approves. The cluster\'s own namespaces can\'t be changed here.'),
      field('What to do', h('select', { id: 'kc-op', class: 'input w-auto', onchange: (e) => { kind = e.target.value; draw(); } }, KINDS.map(([v, l]) => h('option', { value: v }, l))), null, 'kc-op'),
      h('div', { class: 'grid gap-4 sm:grid-cols-3' }, field('Namespace', ns, null, 'kc-ns'), field('Kind', wk, null, 'kc-kind'), field('Name', name, null, 'kc-name')), what,
      field('Reason', reason, null, 'kc-reason'), err, h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go));
  }, { wide: true });
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
    return h('div', { class: 'space-y-4' }, notice('info', 'Forgeline proves it can read the cluster before saving anything. Reading needs only get, list and watch. To let people propose changes, the account also needs the patch verb on deployments and statefulsets; leave it out and the cluster stays read-only.'),
      h('div', { class: 'grid gap-4 sm:grid-cols-3' }, field('Name', name, null, 'k-name'), field('Environment', env, null, 'k-env'), field('Connect with', kindSel, null, 'k-kind')), fields, err,
      h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => close() }, 'Cancel'), go));
  }, { wide: true });
}
