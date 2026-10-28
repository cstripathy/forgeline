import { h, icon, mount, spinner, errorBox, notice, toast, field, copyButton } from '../lib/dom.js';
import { get, post, put, del } from '../lib/api.js';

/**
 * The "Sign in with AWS" wizard: enter the SSO start address, approve in your own browser, then
 * choose an account and role. Forgeline never sees your AWS password. done() runs once connected.
 */
export function ssoWizard(box, { initialRegion, onDone, onCancel }) {
  let timer = null, stopped = false;
  const stop = () => { stopped = true; if (timer) clearTimeout(timer); };
  const show = (...kids) => mount(box, ...kids);

  async function begin() {
    show(spinner());
    let st;
    try { st = await get('/v1/integrations/aws/sso'); } catch (e) { return show(errorBox(e.message)); }
    if (stopped) return;
    if (st.signed_in) return choose(st);
    startForm(st);
  }

  function startForm(st, problem) {
    const url = h('input', { id: 'sso-url', class: 'input font-mono', value: st.start_url || '', placeholder: 'https://d-1234567890.awsapps.com/start', autocomplete: 'off' });
    const reg = h('input', { id: 'sso-region', class: 'input', value: st.sso_region || initialRegion || 'ap-south-1', placeholder: 'ap-south-1' });
    const err = h('div', { class: problem ? '' : 'hidden' }, problem ? errorBox(problem) : null);
    const go = h('button', { class: 'btn-primary', type: 'button', onclick: async () => {
      err.classList.add('hidden'); go.disabled = true;
      try { const r = await post('/v1/integrations/aws/sso/start', { start_url: url.value, sso_region: reg.value }); wait(r, url.value, reg.value); }
      catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; }
    } }, icon('plug', 'size-4'), 'Continue to AWS');
    show(h('div', { class: 'space-y-4' },
      h('p', { class: 'muted text-sm' }, 'Forgeline will ask AWS to sign you in. You approve in your own browser, so no password or key is typed here.'),
      field('AWS access portal URL', url, 'From IAM Identity Center → Settings: "AWS access portal URL".'),
      field('Identity Center region', reg, 'The region where IAM Identity Center is set up.'), err,
      h('div', { class: 'flex justify-end gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => { stop(); onCancel(); } }, 'Cancel'), go)));
  }

  function wait(r, startUrl, region) {
    const status = h('div', { class: 'muted text-sm', role: 'status' }, 'Waiting for you to approve in AWS…');
    const tries = { n: 0 };
    show(h('div', { class: 'space-y-4' },
      notice('info', 'Open the AWS page, check that the code matches, and choose Allow access. This page continues by itself.'),
      h('div', { class: 'text-center' }, h('div', { class: 'muted text-xs uppercase tracking-wide' }, 'Your code'),
        h('div', { id: 'sso-code', class: 'mt-1 select-all font-mono text-3xl font-semibold tracking-widest' }, r.user_code)),
      h('div', { class: 'flex flex-wrap items-center justify-center gap-2' },
        h('a', { id: 'sso-open', class: 'btn-primary', href: r.verification_url, target: '_blank', rel: 'noopener noreferrer' }, 'Open the AWS sign-in page'),
        copyButton(r.verification_url, 'Copy link')),
      status,
      h('div', { class: 'flex justify-end' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => { stop(); startForm({ start_url: startUrl, sso_region: region }); } }, 'Start over'))));
    const tick = async () => {
      if (stopped) return;
      try {
        const p = await post('/v1/integrations/aws/sso/poll', { login_id: r.login_id });
        if (stopped) return;
        if (p.status === 'authorized') return choose(await get('/v1/integrations/aws/sso'));
        if (p.status === 'denied') return startForm({ start_url: startUrl, sso_region: region }, 'The sign-in was denied in AWS. You can try again.');
        if (p.status === 'expired') return startForm({ start_url: startUrl, sso_region: region }, 'The code expired before it was approved. Start again.');
      } catch (e) { if (++tries.n > 3) return startForm({ start_url: startUrl, sso_region: region }, e.message); }
      timer = setTimeout(tick, Math.max(2, r.interval || 5) * 1000);
    };
    timer = setTimeout(tick, Math.max(2, r.interval || 5) * 1000);
  }

  async function choose(st) {
    show(spinner());
    let accounts;
    try { accounts = (await get('/v1/integrations/aws/sso/accounts')).accounts; }
    catch (e) { return startForm(st, e.message); }
    if (stopped) return;
    if (!accounts.length) return startForm(st, 'Your sign-in has no AWS accounts assigned. Ask your Identity Center administrator.');
    const acct = h('select', { id: 'sso-account', class: 'input' }, accounts.map(a => h('option', { value: a.id, selected: a.id === st.account_id }, a.name + ' (' + a.id + ')')));
    const role = h('select', { id: 'sso-role', class: 'input' });
    const reg = h('input', { id: 'sso-aws-region', class: 'input', value: initialRegion || 'ap-south-1', placeholder: 'ap-south-1' });
    const err = h('div', { class: 'hidden' });
    const go = h('button', { class: 'btn-primary', type: 'button', disabled: true, onclick: async () => {
      err.classList.add('hidden'); go.disabled = true; go.textContent = 'Testing…';
      const a = accounts.find(x => x.id === acct.value);
      try { await post('/v1/integrations/aws/sso/connect', { account_id: acct.value, account_name: a ? a.name : '', role_name: role.value, region: reg.value }); toast('Connected to AWS', 'ok'); stop(); onDone(); }
      catch (e) { mount(err, errorBox(e.message)); err.classList.remove('hidden'); go.disabled = false; go.textContent = 'Test and connect'; }
    } }, 'Test and connect');
    async function loadRoles() {
      mount(role, h('option', {}, 'Loading…')); go.disabled = true;
      try {
        const roles = (await get('/v1/integrations/aws/sso/roles?account_id=' + encodeURIComponent(acct.value))).roles;
        mount(role, roles.map(n => h('option', { value: n, selected: n === st.role_name }, n)));
        go.disabled = !roles.length;
        if (!roles.length) mount(role, h('option', {}, 'No roles in this account'));
      } catch (e) { mount(role, h('option', {}, 'Could not load roles')); mount(err, errorBox(e.message)); err.classList.remove('hidden'); }
    }
    acct.addEventListener('change', loadRoles);
    show(h('div', { class: 'space-y-4' },
      notice('ok', 'Signed in to AWS' + (st.expires_at ? ' until ' + new Date(st.expires_at).toLocaleString() : '') + '.'),
      field('Account', acct), field('Role', role, 'Forgeline can do whatever this role allows. For viewing, pick a read-only role; changes always need a second administrator\'s approval.'),
      field('Region', reg, 'The region Forgeline looks at and deploys into.'), err,
      h('div', { class: 'flex flex-wrap justify-between gap-2' },
        h('button', { class: 'btn-ghost text-rose-600', type: 'button', onclick: async () => { try { await del('/v1/integrations/aws/sso'); toast('Signed out of AWS', 'ok'); stop(); startForm({}); } catch (e) { toast(e.message, 'error'); } } }, 'Sign out of AWS'),
        h('div', { class: 'flex gap-2' }, h('button', { class: 'btn-secondary', type: 'button', onclick: () => { stop(); onCancel(); } }, 'Cancel'), go))));
    loadRoles();
  }

  begin();
  return stop;
}
