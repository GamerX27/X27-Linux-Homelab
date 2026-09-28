import { h, clear, bytes, modal, confirmAction, toast, icon, menu, codeArea } from '../ui.js';
import { csrfToken } from '../api.js';

// The logged-in user's home folder. Everything runs on the node as that user, so the same
// files are readable and writable as over SSH.

const join = (dir, name) => (dir ? `${dir}/${name}` : name);
const parent = (p) => p.split('/').slice(0, -1).join('/');

function fileIcon(e) {
  if (e.type === 'dir' || e.linkDir) return '📁';
  if (e.type === 'link') return '🔗';
  if (/\.(ya?ml|json|toml|conf|env|ini|txt|md|sh|log|cfg)$/i.test(e.name) || e.name.startsWith('.')) return '📝';
  return '📄';
}

function fmtTime(unix) {
  return unix ? new Date(unix * 1000).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }) : '';
}

// Remember where the user was per node while the page is open.
const lastDir = new Map();

export function renderFiles(el, { api, node }) {
  let dir = lastDir.get(node.id) || '';
  let entries = [];
  let showHidden = false;
  let loading = 0;

  const crumbs = h('nav.crumbs', { 'aria-label': 'Folder' });
  const filter = h('input.filter-input', { type: 'search', placeholder: 'Filter…', 'aria-label': 'Filter', oninput: () => drawList() });
  const hiddenToggle = h('label.check-row.muted', { style: { fontSize: '13px' } },
    h('input', { type: 'checkbox', onchange: (e) => { showHidden = e.target.checked; drawList(); } }), 'Hidden files');
  const fileInput = h('input', { type: 'file', multiple: true, class: 'hidden', onchange: () => { upload([...fileInput.files]); fileInput.value = ''; } });
  const listBox = h('div.card.files-card');
  const uploads = h('div.uploads');

  clear(el, h('div.stack',
    h('div.row', crumbs, h('span.spacer'), filter, hiddenToggle,
      h('button.btn', { onclick: () => load(), title: 'Refresh', 'aria-label': 'Refresh' }, icon('refresh', 15)),
      h('button.btn', { onclick: newFolder }, 'New folder'),
      h('button.btn', { onclick: newFile }, 'New file'),
      h('button.btn.primary', { onclick: () => fileInput.click() }, 'Upload'), fileInput),
    uploads,
    listBox));

  // Drag and drop files anywhere on the list to upload them into the current folder.
  listBox.addEventListener('dragover', (e) => { if (e.dataTransfer?.types.includes('Files')) { e.preventDefault(); listBox.classList.add('drop'); } });
  listBox.addEventListener('dragleave', (e) => { if (!listBox.contains(e.relatedTarget)) listBox.classList.remove('drop'); });
  listBox.addEventListener('drop', (e) => {
    e.preventDefault();
    listBox.classList.remove('drop');
    const fs = [...(e.dataTransfer?.files || [])];
    if (fs.length) upload(fs);
  });

  async function load(to = dir) {
    const n = ++loading;
    try {
      const r = await api.get(`/files/list?path=${encodeURIComponent(to)}`);
      if (n !== loading) return;
      dir = to;
      lastDir.set(node.id, dir);
      entries = r.entries || [];
      filter.value = '';
      drawCrumbs();
      drawList();
    } catch (e) {
      if (n !== loading) return;
      if (to !== dir) { toast(e.message, true); return; }
      drawCrumbs();
      clear(listBox, h('div.notice.warn', e.message));
    }
  }

  function drawCrumbs() {
    const parts = dir ? dir.split('/') : [];
    clear(crumbs,
      h('button.crumb', { onclick: () => load('') }, '🏠 Home'),
      parts.map((p, i) => [h('span.faint', ' › '),
        i === parts.length - 1 ? h('strong.crumb-here', p) : h('button.crumb', { onclick: () => load(parts.slice(0, i + 1).join('/')) }, p)]));
  }

  function drawList() {
    const q = filter.value.trim().toLowerCase();
    const shown = entries.filter((e) => (showHidden || !e.name.startsWith('.')) && (!q || e.name.toLowerCase().includes(q)));
    const hiddenCount = entries.filter((e) => e.name.startsWith('.')).length;
    const rows = shown.map((e) => {
      const isDir = e.type === 'dir' || e.linkDir;
      const path = join(dir, e.name);
      const open = () => (isDir ? load(path) : edit(path));
      return h('tr',
        h('td', h('button.link-btn.file-name', { onclick: open, title: e.type === 'link' ? `→ ${e.target}` : '' },
          h('span.file-icon', { 'aria-hidden': 'true' }, fileIcon(e)), e.name),
        e.type === 'link' ? h('span.faint.mono', { style: { fontSize: '12px', marginLeft: '8px' } }, `→ ${e.target}`) : null),
        h('td.num-cell.muted', isDir ? '' : bytes(e.size)),
        h('td.num-cell.muted', fmtTime(e.modTime)),
        h('td.mono.faint', { style: { fontSize: '12px' } }, e.mode),
        h('td.actions', menu([
          { label: isDir ? 'Open' : 'Edit', onclick: open },
          { label: isDir ? 'Download as .tar.gz' : 'Download', onclick: () => download(path) },
          { label: 'Rename', onclick: () => rename(e) },
          { label: 'Move to…', onclick: () => move(e) },
          { label: 'Delete', danger: true, onclick: () => del(e) },
        ], `Actions for ${e.name}`)));
    });
    clear(listBox,
      dir ? h('button.link-btn.up-row', { onclick: () => load(parent(dir)) }, '↑ Up to ', parent(dir) ? parent(dir).split('/').pop() : 'Home') : null,
      rows.length ? h('div.table-wrap', h('table',
        h('thead', h('tr', h('th', 'Name'), h('th', 'Size'), h('th', 'Modified'), h('th', 'Permissions'), h('th', ''))),
        h('tbody', rows)))
        : h('div.empty', q ? 'Nothing matches the filter.' : entries.length ? `Only hidden files here (${hiddenCount}).` : 'This folder is empty. Drop files here to upload them.'),
      h('div.faint.files-foot', `${shown.length} item${shown.length === 1 ? '' : 's'}`,
        !showHidden && hiddenCount ? ` · ${hiddenCount} hidden` : '', ' · drop files here to upload'));
  }

  function download(path) {
    const a = h('a', { href: api.url(`/files/download?path=${encodeURIComponent(path)}`), download: '' });
    document.body.append(a);
    a.click();
    a.remove();
  }

  async function ask(title, label, value, okLabel) {
    const input = h('input', { type: 'text', value, spellcheck: false, autocapitalize: 'off' });
    const err = h('div.error-text');
    let result = null;
    const v = await modal(title, h('div.stack', h('label.field', label, input), err), [
      { label: 'Cancel', value: null },
      { label: okLabel, class: 'primary', onclick: async () => {
        const s = input.value.trim();
        if (!s) { err.textContent = 'Enter a name.'; return false; }
        result = s;
        return true;
      } },
    ]);
    return v ? result : null;
  }

  const op = async (path, body, msg) => {
    try { await api.post(path, body); if (msg) toast(msg); return true; } catch (e) { toast(e.message, true); return false; }
  };

  async function newFolder() {
    const name = await ask('New folder', 'Name', '', 'Create');
    if (name && await op('/files/mkdir', { path: join(dir, name) }, `Folder ${name} created.`)) load();
  }

  async function newFile() {
    const name = await ask('New file', 'Name', '', 'Create');
    if (name && await op('/files/write', { path: join(dir, name), content: '', create: true })) { await load(); edit(join(dir, name)); }
  }

  async function rename(e) {
    const name = await ask(`Rename ${e.name}`, 'New name', e.name, 'Rename');
    if (name && name !== e.name && await op('/files/move', { path: join(dir, e.name), to: join(dir, name) }, 'Renamed.')) load();
  }

  async function move(e) {
    const to = await ask(`Move ${e.name}`, 'Move to folder (relative to your home, empty = home)', dir, 'Move');
    if (to === null) return;
    const target = to.replace(/^~\/?/, '').replace(/^\/+|\/+$/g, '');
    if (await op('/files/move', { path: join(dir, e.name), to: join(target, e.name) }, `Moved to ~/${target}.`)) load();
  }

  async function del(e) {
    const isDir = e.type === 'dir';
    if (!await confirmAction(`Delete ${e.name}`, isDir ? `Delete the folder ${e.name} and everything in it? This can't be undone.`
      : `Delete ${e.name}? This can't be undone.`, 'Delete')) return;
    if (await op('/files/delete', { path: join(dir, e.name) }, `${e.name} deleted.`)) load();
  }

  async function edit(path) {
    let content;
    try { content = (await api.get(`/files/read?path=${encodeURIComponent(path)}`)).content; }
    catch (e) {
      toast(e.message, true);
      return;
    }
    const ta = codeArea(content, 24);
    const err = h('div.error-text');
    await modal(`~/${path}`, h('div.stack', { 'data-wide': '1' }, ta, err), [
      { label: 'Close', value: false },
      { label: 'Download', onclick: () => { download(path); return false; } },
      { label: 'Save', class: 'primary', onclick: async () => {
        try { await api.post('/files/write', { path, content: ta.value }); toast('Saved.'); load(); return true; }
        catch (e) { err.textContent = e.message; return false; }
      } },
    ]);
  }

  // Uploads go straight to the node through XHR so progress can be shown.
  function upload(files) {
    const target = dir;
    const next = (i) => {
      if (i >= files.length) { load(); return; }
      const f = files[i];
      const bar = h('div.meter', h('span', { style: { width: '0%' } }));
      const label = h('span.muted', '0%');
      const row = h('div.upload-row', h('span.mono', f.name), bar, label);
      uploads.append(row);
      const send = (overwrite) => {
        const xhr = new XMLHttpRequest();
        xhr.open('POST', api.url(`/files/upload?path=${encodeURIComponent(target)}&name=${encodeURIComponent(f.name)}${overwrite ? '&overwrite=1' : ''}`));
        xhr.setRequestHeader('X-CSRF-Token', csrfToken());
        xhr.setRequestHeader('Content-Type', 'application/octet-stream');
        xhr.upload.onprogress = (e) => {
          if (!e.lengthComputable) return;
          const p = Math.round(e.loaded / e.total * 100);
          bar.firstChild.style.width = p + '%';
          label.textContent = `${p}% of ${bytes(e.total)}`;
        };
        xhr.onload = async () => {
          let msg = '';
          try { msg = JSON.parse(xhr.responseText).error || ''; } catch {}
          if (xhr.status === 200) {
            label.textContent = 'Done';
            setTimeout(() => row.remove(), 2500);
            next(i + 1);
          } else if (msg.includes('already exists') && !overwrite
            && await confirmAction('Replace file', `${f.name} already exists in this folder. Replace it?`, 'Replace')) {
            send(true);
          } else {
            label.textContent = msg || `Failed (${xhr.status})`;
            label.className = 'error-text';
            setTimeout(() => row.remove(), 8000);
            next(i + 1);
          }
        };
        xhr.onerror = () => { label.textContent = 'Upload failed'; label.className = 'error-text'; next(i + 1); };
        xhr.send(f);
      };
      send(false);
    };
    next(0);
  }

  load();
  return () => {};
}
