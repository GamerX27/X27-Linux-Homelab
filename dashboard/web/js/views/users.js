import { h, clear, busy, switchEl, confirmAction, toast, modal, menu } from '../ui.js';

const shellName = (p) => p.split('/').pop();

// Regular users (UID 1000+): groups, login shell, password, lock. wheel is what the
// dashboard login needs; docker is what running docker without sudo needs.
export function renderUsers(el, { api, node }) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  clear(el, content);

  async function load() {
    try { draw(await api.get('/users')); } catch (e) { clear(content, h('div.notice.warn', e.message)); }
  }

  const act = (name, action, body, msg) => api.post(`/users/${encodeURIComponent(name)}/${action}`, body).then((r) => { if (msg) toast(msg); return r; });

  function groupSwitch(u, group, me) {
    const sw = switchEl(u[group], async (on, input) => {
      if (!on && group === 'wheel' && !await confirmAction(`Take ${u.name} out of wheel`,
        `${u.name} can't log in to the dashboard or use sudo anymore.`, 'Take out')) { input.checked = true; return; }
      if (!on && group === 'docker' && !await confirmAction(`Take ${u.name} out of docker`,
        `${u.name} can't run docker without sudo anymore. Running containers aren't affected. It takes effect at their next login.`, 'Take out')) { input.checked = true; return; }
      input.disabled = true;
      try { await act(u.name, 'group', { group, member: on }, `${u.name} ${on ? 'added to' : 'taken out of'} ${group}.`); }
      catch (e) { toast(e.message, true); input.checked = !on; }
      input.disabled = false;
      load();
    }, `${u.name} in ${group}`);
    if (group === 'wheel' && u.name === me && u.wheel) {
      sw.querySelector('input').disabled = true;
      sw.title = 'You can\'t take yourself out of wheel: you\'d lose access to this dashboard.';
    }
    return sw;
  }

  async function passwordDialog(u) {
    const pw = h('input', { type: 'password', autocomplete: 'new-password' });
    const pw2 = h('input', { type: 'password', autocomplete: 'new-password' });
    const err = h('div.error-text');
    await modal(`Password for ${u.name}`, h('div.stack', h('label.field', 'New password', pw), h('label.field', 'Again', pw2),
      h('p.faint', { style: { margin: 0 } }, 'Used for SSH, sudo and this dashboard. At least 8 characters.'), err), [
      { label: 'Cancel', value: null },
      { label: 'Set password', class: 'primary', onclick: async () => {
        if (pw.value !== pw2.value) { err.textContent = 'The passwords don\'t match.'; return false; }
        try { await act(u.name, 'password', { password: pw.value }, 'Password changed.'); return true; }
        catch (e) { err.textContent = e.message; return false; }
      } }]);
  }

  async function deleteDialog(u) {
    const home = h('input', { type: 'checkbox' });
    const v = await modal(`Delete ${u.name}`, h('div.stack',
      h('p.muted', { style: { margin: 0 } }, `${u.name} can't log in anymore. Their containers keep running; apps in `, h('code', `${u.home}/docker`),
        ' stay unless you delete the home folder too.'),
      h('label.check-row', home, h('span', 'Also delete ', h('code', u.home), ' (files, apps, backups). This can\'t be undone.'))), [
      { label: 'Cancel', value: null },
      { label: 'Delete user', class: 'danger solid', onclick: async () => {
        try { await act(u.name, 'delete', { removeHome: home.checked }, `${u.name} deleted.`); return true; }
        catch (e) { toast(e.message, true); return false; }
      } }]);
    if (v) load();
  }

  async function addDialog(shells) {
    const name = h('input', { type: 'text', autocomplete: 'off', spellcheck: false, autocapitalize: 'off' });
    const full = h('input', { type: 'text', autocomplete: 'off' });
    const pw = h('input', { type: 'password', autocomplete: 'new-password' });
    const shell = h('select', shells.map((s) => h('option', { value: s }, shellName(s))));
    shell.value = shells.find((s) => s.endsWith('/fish')) || shells[0] || '';
    const wheel = h('input', { type: 'checkbox' });
    const docker = h('input', { type: 'checkbox', checked: true });
    const err = h('div.error-text');
    const v = await modal('Add user', h('div.stack',
      h('div.form-grid', h('label.field', 'Username', name), h('label.field', 'Full name (optional)', full)),
      h('div.form-grid', h('label.field', 'Password', pw), h('label.field', 'Login shell', shell)),
      h('label.check-row', docker, h('span', h('strong', 'docker'), h('span.muted', ' · can run docker without sudo (effectively root)'))),
      h('label.check-row', wheel, h('span', h('strong', 'wheel'), h('span.muted', ' · administrator: sudo and this dashboard'))),
      err), [
      { label: 'Cancel', value: null },
      { label: 'Add user', class: 'primary', onclick: async () => {
        try {
          await api.post('/users', { name: name.value.trim(), full: full.value.trim(), password: pw.value, shell: shell.value, wheel: wheel.checked, docker: docker.checked });
          toast(`${name.value.trim()} added.`);
          return true;
        } catch (e) { err.textContent = e.message; return false; }
      } }]);
    if (v) load();
  }

  function draw({ users, shells, me }) {
    const rows = users.map((u) => {
      const shell = h('select', { 'aria-label': `Login shell of ${u.name}`, style: { width: 'auto' },
        onchange: async () => {
          shell.disabled = true;
          try { await act(u.name, 'shell', { shell: shell.value }, `${u.name} now logs in with ${shellName(shell.value)}.`); }
          catch (e) { toast(e.message, true); }
          load();
        } },
      [...new Set([...shells, u.shell])].map((s) => h('option', { value: s }, shellName(s))));
      shell.value = u.shell;
      const self = u.name === me;
      return h('tr',
        h('td', h('div.row', h('strong', u.name), self ? h('span.badge', 'you') : null, u.locked ? h('span.badge.warn', 'locked') : null),
          h('div.sub', [u.full, u.home].filter(Boolean).join(' · '))),
        h('td.num-cell.muted', u.uid),
        h('td', groupSwitch(u, 'wheel', me)),
        h('td', groupSwitch(u, 'docker', me)),
        h('td', shell),
        h('td.actions', menu([
          { label: 'Set password…', onclick: () => passwordDialog(u) },
          { label: u.locked ? 'Unlock' : 'Lock (no logins)', hidden: self, onclick: async () => {
            if (!u.locked && !await confirmAction(`Lock ${u.name}`, `${u.name} can't log in with a password anymore (SSH keys may still work). Unlock to undo.`, 'Lock')) return;
            try { await act(u.name, 'lock', { lock: !u.locked }, u.locked ? `${u.name} unlocked.` : `${u.name} locked.`); } catch (e) { toast(e.message, true); }
            load();
          } },
          { label: 'Delete…', danger: true, hidden: self, onclick: () => deleteDialog(u) },
        ], `Actions for ${u.name}`)));
    });
    clear(content,
      h('div.card',
        h('div.card-head', h('h2', 'Users'), h('button.btn.primary', { onclick: () => addDialog(shells) }, 'Add user')),
        h('p.muted', { style: { marginTop: 0 } }, `Regular users on ${node.name}. `, h('strong', 'wheel'), ' can use sudo and log in here; ',
          h('strong', 'docker'), ' can run docker without sudo. Changes you make here stay: the boot services don\'t add users back.'),
        users.length ? h('div.table-wrap', h('table',
          h('thead', h('tr', h('th', 'User'), h('th', 'UID'), h('th', 'wheel'), h('th', 'docker'), h('th', 'Shell'), h('th', ''))),
          h('tbody', rows))) : h('div.faint', 'No regular users.')));
  }

  load();
  return () => {};
}
