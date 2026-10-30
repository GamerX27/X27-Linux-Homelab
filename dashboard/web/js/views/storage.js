import { h, clear, busy, switchEl, confirmAction, toast, modal, menu, meter, bytes, modalOpen, menuOpen } from '../ui.js';

const VERSIONS = { nfs: ['4.2', '4.1', '4', '3'], smb: ['3.1.1', '3.0', '2.1'] };
const STATES = {
  mounted: ['good', 'Mounted'], waiting: ['idle', 'Mounts on use'], mounting: ['warn', 'Mounting…'],
  unmounting: ['warn', 'Unmounting…'], failed: ['crit', 'Failed'], off: ['idle', 'Not mounted'],
};
const source = (m) => m.type === 'smb' ? `//${m.server}/${m.path}` : `${m.server.includes(':') ? `[${m.server}]` : m.server}:${m.path}`;

// A mount's name as the node accepts it (1-32 lowercase letters, digits and dashes, no dash
// at either end): lowercased, with spaces, underscores and dots turned into dashes and
// anything else left out.
const mountName = (s) => s.trim().toLowerCase().replace(/[\s_.]+/g, '-').replace(/[^a-z0-9-]/g, '')
  .replace(/-+/g, '-').slice(0, 32).replace(/^-+|-+$/g, '');

// NFS and SMB shares mounted on the node, under /var/mnt/<name>, for containers to use.
export function renderStorage(el, { api, node }) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  let timer, stopped = false;
  clear(el, content);

  async function load() {
    clearTimeout(timer);
    try {
      const r = await api.get('/storage/mounts');
      if (stopped) return;
      if (!modalOpen() && !menuOpen()) draw(r.mounts || []);
    } catch (e) {
      clear(content, h('div.notice.warn', e.message));
    }
    if (!stopped) timer = setTimeout(load, 15000);
  }

  const browse = (m) => `#/n/${encodeURIComponent(node.id)}/files/files?root=${encodeURIComponent(m.name)}`;
  const act = (m, action, msg) => api.post(`/storage/mounts/${encodeURIComponent(m.name)}/${action}`).then((r) => { if (msg) toast(msg); return r; });

  async function users() {
    try { return (await api.get('/users')).users || []; } catch { return []; }
  }

  async function mountDialog(m) {
    const editing = !!m;
    m ||= { type: 'nfs', dockerWaits: true };
    const people = await users();
    let type = m.type;

    const name = h('input', { type: 'text', autocomplete: 'off', spellcheck: false, autocapitalize: 'off', value: m.name || '', placeholder: 'e.g. nas-media', disabled: editing });
    const server = h('input', { type: 'text', autocomplete: 'off', spellcheck: false, autocapitalize: 'off', value: m.server || '', placeholder: 'e.g. 192.168.1.5 or nas.lan' });
    const path = h('input', { type: 'text', autocomplete: 'off', spellcheck: false, autocapitalize: 'off', value: m.path || '' });
    const exportsList = h('datalist', { id: 'nfs-exports' });
    path.setAttribute('list', 'nfs-exports');
    const browse = h('button.btn.small', { type: 'button', onclick: async () => {
      const r = await busy(browse, () => api.get(`/storage/exports?server=${encodeURIComponent(server.value.trim())}`), '');
      if (!r) return;
      clear(exportsList, r.exports.map((x) => h('option', { value: x })));
      toast(r.exports.length ? `${r.exports.length} export(s) found. Pick one in the path field.` : 'The server lists no exports.');
      path.focus();
    } }, 'Browse');

    const guest = h('input', { type: 'checkbox', checked: !!m.guest });
    const username = h('input', { type: 'text', autocomplete: 'off', spellcheck: false, autocapitalize: 'off', value: m.username || '' });
    const password = h('input', { type: 'password', autocomplete: 'new-password',
      placeholder: editing && m.hasPassword ? 'Leave empty to keep the current password' : '' });
    const peek = h('button.btn.ghost.small', { type: 'button', onclick: () => {
      const show = password.type === 'password';
      password.type = show ? 'text' : 'password';
      peek.textContent = show ? 'Hide' : 'Show';
    } }, 'Show');
    const domain = h('input', { type: 'text', autocomplete: 'off', spellcheck: false, value: m.domain || '', placeholder: 'Usually empty' });

    const version = h('select');
    const readOnly = h('input', { type: 'checkbox', checked: !!m.readOnly });
    const automount = h('input', { type: 'checkbox', checked: !!m.automount });
    const dockerWaits = h('input', { type: 'checkbox', checked: !!m.dockerWaits });
    const owners = [...new Set([...people.map((u) => u.name), m.owner].filter(Boolean))];
    const owner = h('select', h('option', { value: '' }, 'root'), owners.map((u) => h('option', { value: u }, u)));
    owner.value = m.owner || '';
    const fileMode = h('input', { type: 'text', autocomplete: 'off', value: (m.fileMode || '0664').replace(/^0(?=\d{3}$)/, ''), inputmode: 'numeric', maxlength: 4 });
    const dirMode = h('input', { type: 'text', autocomplete: 'off', value: (m.dirMode || '0775').replace(/^0(?=\d{3}$)/, ''), inputmode: 'numeric', maxlength: 4 });

    const where = h('code');
    const compose = h('code');
    const result = h('div');
    const err = h('div.error-text');

    const seg = h('div.seg', { role: 'tablist', 'aria-label': 'Protocol', style: { alignSelf: 'flex-start' } });
    const pathLabel = h('span');
    const smbFields = h('div.stack',
      h('label.check-row', guest, h('span', h('strong', 'Guest access'), h('span.muted', ' · the share needs no login'))),
      h('div.form-grid.login-fields',
        h('label.field', 'Username', username),
        h('label.field', 'Password', h('div.input-row', password, peek)),
        h('label.field', 'Domain or workgroup', domain)));
    const smbAdvanced = [h('label.field', 'Files owned by', owner),
      h('label.field', 'File permissions', fileMode),
      h('label.field', 'Folder permissions', dirMode)];

    function sync() {
      clear(seg, ['nfs', 'smb'].map((t) => h('button.seg-btn', { type: 'button', role: 'tab', disabled: editing && t !== type,
        'aria-selected': String(t === type), class: t === type ? 'active' : '', onclick: () => { type = t; sync(); } }, t === 'nfs' ? 'NFS' : 'SMB (Samba, Windows)')));
      const nfs = type === 'nfs';
      pathLabel.textContent = nfs ? 'Export path' : 'Share';
      path.placeholder = nfs ? 'e.g. /export/media' : 'e.g. media, or media/films';
      browse.classList.toggle('hidden', !nfs);
      smbFields.classList.toggle('hidden', nfs);
      smbAdvanced.forEach((f) => f.classList.toggle('hidden', nfs));
      smbFields.querySelector('.login-fields').classList.toggle('hidden', guest.checked);
      const cur = version.value || m.version || '';
      clear(version, h('option', { value: '' }, 'Automatic'), VERSIONS[type].map((v) => h('option', { value: v }, v)));
      version.value = VERSIONS[type].includes(cur) ? cur : '';
      const p = `/var/mnt/${mountName(name.value) || '<name>'}`;
      where.textContent = p;
      compose.textContent = `- ${p}:/data`;
    }
    guest.addEventListener('change', sync);
    name.addEventListener('input', sync);
    // Show the name it gets once the field is left: "My NAS" becomes my-nas.
    name.addEventListener('change', () => { if (!editing) name.value = mountName(name.value); });
    sync();

    const body = () => ({
      name: editing ? m.name : mountName(name.value), type, server: server.value.trim(), path: path.value.trim(), version: version.value,
      readOnly: readOnly.checked, automount: automount.checked, dockerWaits: dockerWaits.checked,
      guest: guest.checked, username: username.value.trim(), password: password.value, domain: domain.value.trim(),
      owner: owner.value, fileMode: fileMode.value.trim(), dirMode: dirMode.value.trim(),
    });

    const form = h('div.stack', { 'data-wide': '1' },
      h('label.field', 'Name', name),
      seg,
      h('div.form-grid',
        h('label.field', 'Server', server),
        h('label.field', pathLabel, h('div.input-row', path, browse), exportsList)),
      smbFields,
      h('details',
        h('summary', 'More options'),
        h('div.stack',
          h('div.form-grid', h('label.field', 'Protocol version', version), smbAdvanced),
          h('label.check-row', readOnly, h('span', h('strong', 'Read-only'), h('span.muted', ' · nothing on this node can change the files'))),
          h('label.check-row', automount, h('span', h('strong', 'Mount on first access'),
            h('span.muted', ' · instead of at boot. The node doesn\'t wait for the server when it starts.'))))),
      h('label.check-row', dockerWaits, h('span', h('strong', 'Docker waits for this share at boot'),
        h('span.muted', ' · so containers never see the empty folder underneath. If the server is down, Docker starts up to 45 s later, without it. Applies from the next boot.'))),
      h('p.faint', { style: { margin: 0 } }, 'Mounted at ', where, '. In a compose file: ', h('code', 'volumes:'), ' ', compose),
      result, err);

    const v = await modal(editing ? `Edit ${m.name}` : 'Add network share', form, [
      { label: 'Cancel', value: null },
      { label: 'Test', onclick: async () => {
        err.textContent = '';
        clear(result);
        try {
          const r = await api.post('/storage/test-mount', body());
          clear(result, h('div.notice', r.output));
        } catch (e) { err.textContent = e.message; }
        return false;
      } },
      { label: editing ? 'Save' : 'Add and mount', class: 'primary', onclick: async () => {
        err.textContent = '';
        clear(result);
        try {
          await api.post(editing ? `/storage/mounts/${encodeURIComponent(m.name)}` : '/storage/mounts', body());
          toast(editing ? `${m.name} saved.` : `${body().name} added and mounted.`);
          return true;
        } catch (e) { err.textContent = e.message; return false; }
      } }]);
    if (v) load();
  }

  async function remove(m) {
    if (!await confirmAction(`Remove ${m.name}`,
      `${m.mountPoint} is unmounted and the mount is deleted from this node, with its stored password. The files on ${m.server} aren't touched. Containers that use the folder see an empty folder afterwards.`,
      'Remove')) return;
    try { await act(m, 'remove', `${m.name} removed.`); } catch (e) { toast(e.message, true); }
    load();
  }

  function row(m) {
    const [cls, label] = STATES[m.state] || ['idle', m.state];
    const used = m.total ? h('div', { style: { minWidth: '120px' } }, meter(m.used / m.total * 100, `${m.name} used`),
      h('div.sub', `${bytes(m.used)} of ${bytes(m.total)}`)) : h('span.faint', '—');
    const boot = switchEl(m.enabled, async (on, input) => {
      input.disabled = true;
      try { await act(m, on ? 'enable' : 'disable', on ? `${m.name} mounts at boot.` : `${m.name} no longer mounts at boot.`); }
      catch (e) { toast(e.message, true); input.checked = !on; }
      input.disabled = false;
      load();
    }, `Mount ${m.name} at boot`);
    const up = m.state === 'mounted' || m.state === 'mounting';
    return h('tr',
      h('td', h('a.share-name', { href: browse(m), title: `Browse ${m.mountPoint}` }, m.name), h('div.sub.mono.mount-point', m.mountPoint), h('div.sub.mono.phone-share', source(m))),
      h('td.col-share', h('span.mono', source(m)),
        h('div.sub', [m.type.toUpperCase() + (m.version ? ` ${m.version}` : ''), m.readOnly ? 'read-only' : null,
          m.type === 'smb' ? (m.guest ? 'guest' : m.username) : null, m.dockerWaits ? 'Docker waits' : null].filter(Boolean).join(' · '))),
      h('td', h('span.status', { class: cls }, label)),
      h('td.col-usage', used),
      h('td', boot),
      h('td.actions', menu([
        { label: up ? 'Unmount' : 'Mount', onclick: async () => {
          try { await act(m, up ? 'unmount' : 'mount', up ? `${m.name} unmounted.` : `${m.name} mounted.`); } catch (e) { toast(e.message, true); }
          load();
        } },
        { label: 'Browse files', onclick: () => { location.hash = browse(m); } },
        { label: 'Edit…', onclick: () => mountDialog(m) },
        { label: 'Copy path', onclick: async () => {
          try { await navigator.clipboard.writeText(m.mountPoint); toast('Path copied.'); } catch { toast('Couldn\'t copy', true); }
        } },
        { label: 'Logs', onclick: () => { location.hash = `#/n/${encodeURIComponent(node.id)}/system/logs?unit=${encodeURIComponent(m.unit)}`; } },
        { label: 'Remove…', danger: true, onclick: () => remove(m) },
      ], `Actions for ${m.name}`)));
  }

  function draw(mounts) {
    clear(content,
      h('div.card',
        h('div.card-head', h('h2', 'Network shares'), h('button.btn.primary', { onclick: () => mountDialog(null) }, 'Add share')),
        h('p.muted', { style: { marginTop: 0 } }, `NFS and SMB shares from a NAS or another computer, mounted on ${node.name} under `,
          h('code', '/var/mnt'), '. Give containers the folder as a volume. Mounts stay across reboots and OS updates; a server that\'s down never stops the node from booting.'),
        mounts.length ? h('div.table-wrap', h('table.mounts-table',
          h('thead', h('tr', h('th', 'Name'), h('th.col-share', 'Share'), h('th', 'Status'), h('th.col-usage', 'Space'), h('th', 'At boot'), h('th', ''))),
          h('tbody', mounts.map(row))))
          : h('div.empty', 'No shares yet. ', h('button.btn.small', { onclick: () => mountDialog(null) }, 'Add one'))));
  }

  load();
  return () => { stopped = true; clearTimeout(timer); };
}
