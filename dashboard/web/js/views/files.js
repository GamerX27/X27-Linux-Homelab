import { h, clear, bytes, modal, confirmAction, toast, icon, menu, codeArea, modalOpen, menuOpen } from '../ui.js';
import { csrfToken } from '../api.js';

// The logged-in user's home folder, or a network share's folder (?root=<share>, from the
// Storage page). Everything runs on the node as that user, so the same files are readable and
// writable as over SSH.

const join = (dir, name) => (dir ? `${dir}/${name}` : name);
const parent = (p) => p.split('/').slice(0, -1).join('/');
const isDirEntry = (e) => e.type === 'dir' || e.linkDir;

const PREVIEW = [
  [/\.(png|jpe?g|gif|webp|avif|bmp|ico)$/i, 'image'],
  [/\.(mp4|webm)$/i, 'video'],
  [/\.(mp3|ogg|wav|flac|m4a)$/i, 'audio'],
  [/\.pdf$/i, 'pdf'],
];
const previewKind = (name) => PREVIEW.find(([re]) => re.test(name))?.[1] || null;
// Same list as files.ArchiveExts; a plain .gz/.bz2 unpacks to one file, not a folder.
const ARCHIVE = /\.(tar\.(gz|bz2|xz|zst)|tgz|tbz2?|txz|tzst|tar|zip|7z|rar|gz|bz2)$/i;
const SINGLE = /(?<!\.tar)\.(gz|bz2)$/i;

function fileIcon(e) {
  if (isDirEntry(e)) return '📁';
  if (e.type === 'link') return '🔗';
  if (ARCHIVE.test(e.name)) return '📦';
  const kind = previewKind(e.name);
  if (kind === 'image') return '🖼️';
  if (kind === 'video') return '🎞️';
  if (kind === 'audio') return '🎵';
  if (/\.(ya?ml|json|toml|conf|env|ini|txt|md|sh|log|cfg)$/i.test(e.name) || e.name.startsWith('.')) return '📝';
  return '📄';
}

function fmtTime(unix) {
  return unix ? new Date(unix * 1000).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }) : '';
}

// "notes.txt" -> ["notes", ".txt"]; folders and dotfiles have no extension.
function splitExt(name, dir) {
  const i = name.lastIndexOf('.');
  return dir || i <= 0 ? [name, ''] : [name.slice(0, i), name.slice(i)];
}

// "-rwxr-xr-x" -> 0o755
function modeBits(s) {
  const p = (s || '').slice(-9);
  let m = 0;
  for (let i = 0; i < 9; i++) if (p[i] && p[i] !== '-' && p[i] !== 'S' && p[i] !== 'T') m |= 1 << (8 - i);
  return m;
}

const plural = (n, w) => `${n} ${w}${n === 1 ? '' : 's'}`;

// Remember where the user was per node while the page is open.
const lastDir = new Map();

// The node's API with ?root=<share> on every call, so the whole view works in that share.
function withRoot(api, root) {
  if (!root) return api;
  const add = (p) => `${p}${p.includes('?') ? '&' : '?'}root=${encodeURIComponent(root)}`;
  return { ...api, get: (p) => api.get(add(p)), post: (p, b) => api.post(add(p), b), url: (p) => api.url(add(p)) };
}

export function renderFiles(el, { api: nodeApi, node, query }) {
  const root = query?.get('root') || '';
  const api = withRoot(nodeApi, root);
  const place = `${node.id}:${root}`;
  const top = root ? `🗄 ${root}` : '🏠 Home';
  const prefix = root ? `/var/mnt/${root}/` : '~/';
  let dir = query?.get('path') ?? lastDir.get(place) ?? '';
  let entries = [];
  let showHidden = false;
  let loading = 0;
  let stopped = false;
  const selected = new Set();
  let sort = { key: 'name', desc: false };
  try { sort = { ...sort, ...JSON.parse(localStorage.getItem('files-sort') || '{}') }; } catch {}

  const crumbs = h('nav.crumbs', { 'aria-label': 'Folder' });
  const filter = h('input.filter-input', { type: 'search', placeholder: 'Filter…', 'aria-label': 'Filter', oninput: () => {
    const q = filter.value.trim().toLowerCase();
    for (const n of selected) if (q && !n.toLowerCase().includes(q)) selected.delete(n);
    drawList();
  } });
  const hiddenToggle = h('label.check-row.muted', { style: { fontSize: '13px' } },
    h('input', { type: 'checkbox', onchange: (e) => { showHidden = e.target.checked; pruneSelection(); drawList(); } }), 'Hidden files');
  const fileInput = h('input', { type: 'file', multiple: true, class: 'hidden',
    onchange: () => { upload([...fileInput.files].map((file) => ({ file, sub: '' }))); fileInput.value = ''; } });
  const tools = h('div.row', { style: { gap: '8px', flexWrap: 'wrap' } });
  const listBox = h('div.card.files-card');
  const uploads = h('div.uploads');

  // Home or one of the node's network shares; only shown once there are shares.
  const places = h('select.places', { 'aria-label': 'Folder to browse', class: 'hidden', onchange: () => {
    location.hash = `#/n/${encodeURIComponent(node.id)}/files` + (places.value ? `?root=${encodeURIComponent(places.value)}` : '');
  } });
  nodeApi.get('/storage/mounts').then((r) => {
    const names = (r.mounts || []).map((m) => m.name);
    if (!names.length || stopped) return;
    clear(places, h('option', { value: '' }, 'Home'), names.map((n) => h('option', { value: n }, n)));
    places.value = root;
    places.classList.remove('hidden');
  }).catch(() => {});

  clear(el, h('div.stack',
    h('div.row', places, crumbs, h('span.spacer'), tools, fileInput),
    uploads,
    listBox));

  // Drag and drop files or folders anywhere on the list to upload them into the current folder.
  listBox.addEventListener('dragover', (e) => { if (e.dataTransfer?.types.includes('Files')) { e.preventDefault(); listBox.classList.add('drop'); } });
  listBox.addEventListener('dragleave', (e) => { if (!listBox.contains(e.relatedTarget)) listBox.classList.remove('drop'); });
  listBox.addEventListener('drop', async (e) => {
    e.preventDefault();
    listBox.classList.remove('drop');
    // Entries have to be taken during the event; reading them can happen later.
    const items = [...(e.dataTransfer?.items || [])].map((i) => i.webkitGetAsEntry?.()).filter(Boolean);
    if (!items.length) {
      const fs = [...(e.dataTransfer?.files || [])];
      if (fs.length) upload(fs.map((file) => ({ file, sub: '' })));
      return;
    }
    const out = [], emptyDirs = [];
    try {
      for (const it of items) await walkEntry(it, '', out, emptyDirs);
    } catch (err) { toast(`Couldn't read the dropped files: ${err.message || err}`, true); return; }
    if (out.length || emptyDirs.length) upload(out, emptyDirs);
  });

  async function walkEntry(entry, prefix, out, emptyDirs) {
    if (entry.isFile) {
      out.push({ file: await new Promise((res, rej) => entry.file(res, rej)), sub: prefix });
      return;
    }
    if (!entry.isDirectory) return;
    const sub = join(prefix, entry.name);
    const reader = entry.createReader();
    let any = false, batch;
    do {
      batch = await new Promise((res, rej) => reader.readEntries(res, rej));
      for (const c of batch) { any = true; await walkEntry(c, sub, out, emptyDirs); }
    } while (batch.length);
    if (!any) emptyDirs.push(sub);
  }

  // Escape clears the selection, Delete deletes it (outside inputs, menus and dialogs).
  const onKey = (e) => {
    if (!selected.size || modalOpen() || menuOpen() || document.activeElement?.matches?.('textarea, select, input:not([type=checkbox])')) return;
    if (e.key === 'Escape') { selected.clear(); drawList(); }
    else if (e.key === 'Delete') { e.preventDefault(); delSelected(); }
  };
  document.addEventListener('keydown', onKey);

  async function load(to = dir) {
    const n = ++loading;
    try {
      const r = await api.get(`/files/list?path=${encodeURIComponent(to)}`);
      if (n !== loading || stopped) return;
      if (to !== dir) { selected.clear(); filter.value = ''; }
      dir = to;
      lastDir.set(place, dir);
      entries = r.entries || [];
      pruneSelection();
      syncURL();
      drawCrumbs();
      drawList();
    } catch (e) {
      if (n !== loading || stopped) return;
      if (to !== dir) { toast(e.message, true); return; }
      drawCrumbs();
      drawTools();
      clear(listBox, h('div.notice.warn', e.message, dir ? [' ', h('button.link-btn', { onclick: () => load('') }, root ? 'Go to the top' : 'Go home')] : null));
    }
  }

  // Keep the folder in the address so a reload or a shared link lands here. replaceState
  // doesn't fire hashchange, so the view isn't rebuilt.
  function syncURL() {
    const base = `#/n/${encodeURIComponent(node.id)}/files`;
    if (!location.hash.startsWith(base)) return;
    const q = new URLSearchParams();
    if (root) q.set('root', root);
    if (dir) q.set('path', dir);
    history.replaceState(null, '', base + (q.size ? `?${q}` : ''));
  }

  function pruneSelection() {
    const names = new Set(entries.filter((e) => showHidden || !e.name.startsWith('.')).map((e) => e.name));
    for (const n of selected) if (!names.has(n)) selected.delete(n);
  }

  function drawCrumbs() {
    const parts = dir ? dir.split('/') : [];
    clear(crumbs,
      h('button.crumb', { onclick: () => load('') }, top),
      parts.map((p, i) => [h('span.faint', ' › '),
        i === parts.length - 1 ? h('strong.crumb-here', p) : h('button.crumb', { onclick: () => load(parts.slice(0, i + 1).join('/')) }, p)]));
  }

  function drawTools() {
    if (selected.size) {
      clear(tools,
        h('strong.sel-count', `${selected.size} selected`),
        h('button.btn', { onclick: () => download([...selected].map((n) => join(dir, n))) }, selected.size > 1 ? 'Download .zip' : 'Download'),
        h('button.btn', { onclick: () => copyTo([...selected]) }, 'Copy to…'),
        h('button.btn', { onclick: () => moveTo([...selected]) }, 'Move to…'),
        h('button.btn', { onclick: () => compress([...selected]) }, 'Compress'),
        h('button.btn.danger', { onclick: delSelected }, 'Delete'),
        h('button.btn', { onclick: () => { selected.clear(); drawList(); }, title: 'Clear selection (Esc)' }, 'Clear'));
      return;
    }
    clear(tools, filter, hiddenToggle,
      h('button.btn', { onclick: () => load(), title: 'Refresh', 'aria-label': 'Refresh' }, icon('refresh', 15)),
      h('button.btn', { onclick: newFolder }, 'New folder'),
      h('button.btn', { onclick: newFile }, 'New file'),
      h('button.btn.primary', { onclick: () => fileInput.click() }, 'Upload'));
  }

  function visible() {
    const q = filter.value.trim().toLowerCase();
    const shown = entries.filter((e) => (showHidden || !e.name.startsWith('.')) && (!q || e.name.toLowerCase().includes(q)));
    const cmpName = (a, b) => a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' });
    const cmp = { name: cmpName, size: (a, b) => a.size - b.size || cmpName(a, b), modTime: (a, b) => a.modTime - b.modTime || cmpName(a, b) }[sort.key] || cmpName;
    return shown.sort((a, b) => {
      const da = isDirEntry(a), db = isDirEntry(b);
      if (da !== db) return da ? -1 : 1;
      const c = da && sort.key === 'size' ? cmpName(a, b) : cmp(a, b);
      return sort.desc ? -c : c;
    });
  }

  function sortHead(key, label, cls = '') {
    const on = sort.key === key;
    return h('th', { class: cls, 'aria-sort': on ? (sort.desc ? 'descending' : 'ascending') : 'none' },
      h('button.sort-btn', { class: on ? 'on' : '', onclick: () => {
        sort = { key, desc: on ? !sort.desc : false };
        localStorage.setItem('files-sort', JSON.stringify(sort));
        drawList();
      } }, label, h('span.sort-arrow', { 'aria-hidden': 'true' }, on ? (sort.desc ? ' ↓' : ' ↑') : '')));
  }

  function drawList() {
    drawTools();
    const q = filter.value.trim();
    const shown = visible();
    const hiddenCount = entries.filter((e) => e.name.startsWith('.')).length;
    const all = h('input', { type: 'checkbox', 'aria-label': 'Select all',
      onchange: (ev) => { for (const e of shown) ev.target.checked ? selected.add(e.name) : selected.delete(e.name); drawList(); } });
    const nSel = shown.filter((e) => selected.has(e.name)).length;
    all.checked = shown.length > 0 && nSel === shown.length;
    all.indeterminate = nSel > 0 && nSel < shown.length;

    const rows = shown.map((e) => {
      const isDir = isDirEntry(e);
      const path = join(dir, e.name);
      const kind = !isDir && previewKind(e.name);
      const open = () => (isDir ? load(path) : kind ? preview(e) : edit(path));
      const box = h('input', { type: 'checkbox', checked: selected.has(e.name), 'aria-label': `Select ${e.name}`,
        onchange: (ev) => { ev.target.checked ? selected.add(e.name) : selected.delete(e.name); drawList(); } });
      return h('tr', { class: selected.has(e.name) ? 'selected' : '' },
        h('td.check-cell', box),
        h('td', h('button.link-btn.file-name', { onclick: open, title: e.type === 'link' ? `→ ${e.target}` : '' },
          h('span.file-icon', { 'aria-hidden': 'true' }, fileIcon(e)), e.name),
        e.type === 'link' ? h('span.faint.mono', { style: { fontSize: '12px', marginLeft: '8px' } }, `→ ${e.target}`) : null),
        h('td.num-cell.muted', isDir ? '' : bytes(e.size)),
        h('td.num-cell.muted.col-mod', fmtTime(e.modTime)),
        h('td.mono.faint.col-perm', { style: { fontSize: '12px' } }, e.mode),
        h('td.actions', menu([
          { label: isDir ? 'Open' : kind ? 'Preview' : 'Edit', onclick: open },
          { label: 'Edit as text', onclick: () => edit(path), hidden: isDir || !kind },
          { label: isDir ? 'Download as .tar.gz' : 'Download', onclick: () => download([path]) },
          { label: 'Extract here', onclick: () => extract(e), hidden: isDir || !ARCHIVE.test(e.name) },
          { label: 'Compress to .zip', onclick: () => compress([e.name]) },
          { label: 'Duplicate', onclick: () => duplicate(e) },
          { label: 'Rename', onclick: () => rename(e) },
          { label: 'Copy to…', onclick: () => copyTo([e.name]) },
          { label: 'Move to…', onclick: () => moveTo([e.name]) },
          { label: 'Permissions…', onclick: () => perms(e), hidden: e.type === 'link' },
          { label: 'Delete', danger: true, onclick: () => del(e) },
        ], `Actions for ${e.name}`)));
    });
    clear(listBox,
      dir ? h('button.link-btn.up-row', { onclick: () => load(parent(dir)) }, '↑ Up to ', parent(dir) ? parent(dir).split('/').pop() : 'Home') : null,
      rows.length ? h('div.table-wrap', h('table.files-table',
        h('thead', h('tr', h('th.check-cell', all), sortHead('name', 'Name'), sortHead('size', 'Size'), sortHead('modTime', 'Modified', 'col-mod'),
          h('th.col-perm', 'Permissions'), h('th', ''))),
        h('tbody', rows)))
        : h('div.empty', q ? 'Nothing matches the filter.' : entries.length ? `Only hidden files here (${hiddenCount}).` : 'This folder is empty. Drop files here to upload them.'),
      h('div.faint.files-foot', plural(shown.length, 'item'),
        !showHidden && hiddenCount ? ` · ${hiddenCount} hidden` : '', ' · drop files or folders here to upload'));
  }

  function download(paths) {
    const qs = paths.map((p) => `path=${encodeURIComponent(p)}`).join('&');
    const a = h('a', { href: api.url(`/files/download?${qs}`), download: '' });
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
    try { const r = await api.post(path, body); if (msg) toast(msg); return r || {}; } catch (e) { toast(e.message, true); return null; }
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

  async function duplicate(e) {
    const taken = new Set(entries.map((x) => x.name));
    const [stem, ext] = splitExt(e.name, isDirEntry(e));
    let name = `${stem} copy${ext}`;
    for (let i = 2; taken.has(name); i++) name = `${stem} copy ${i}${ext}`;
    if (await op('/files/copy', { path: join(dir, e.name), to: join(dir, name) }, `Copied to ${name}.`)) load();
  }

  const where = (p) => (p ? prefix + p : root ? `the top of ${root}` : 'your home folder');

  async function copyTo(names) {
    const to = await pickFolder(names.length === 1 ? `Copy ${names[0]}` : `Copy ${plural(names.length, 'item')}`, 'Copy here');
    if (to === null) return;
    const busyMsg = names.length > 1 || entries.some((e) => names.includes(e.name) && isDirEntry(e));
    if (busyMsg) toast('Copying…');
    if (await op('/files/copy', { paths: names.map((n) => join(dir, n)), to }, `Copied to ${where(to)}.`)) { selected.clear(); load(); }
  }

  async function moveTo(names) {
    const to = await pickFolder(names.length === 1 ? `Move ${names[0]}` : `Move ${plural(names.length, 'item')}`, 'Move here');
    if (to === null) return;
    if (to === dir) { toast('They are already in that folder.', true); return; }
    if (await op('/files/move', { paths: names.map((n) => join(dir, n)), to }, `Moved to ${where(to)}.`)) { selected.clear(); load(); }
  }

  async function compress(names) {
    toast('Compressing…');
    const r = await op('/files/compress', { paths: names.map((n) => join(dir, n)) });
    if (!r) return;
    toast(`Created ${r.output.split('/').pop()}.`);
    selected.clear();
    load();
  }

  async function extract(e) {
    toast(`Extracting ${e.name}…`);
    const r = await op('/files/extract', { path: join(dir, e.name) });
    if (!r) return;
    const name = r.output.split('/').pop();
    if (SINGLE.test(e.name)) { toast(`Extracted ${name}.`); load(); return; }
    toast(`Extracted to ${name}.`);
    load(r.output);
  }

  async function del(e) {
    const isDir = e.type === 'dir';
    const msg = e.type === 'link' ? `Delete the link ${e.name}? Only the link is removed; what it points to stays.`
      : isDir ? `Delete the folder ${e.name} and everything in it? This can't be undone.`
        : `Delete ${e.name}? This can't be undone.`;
    if (!await confirmAction(`Delete ${e.name}`, msg, 'Delete')) return;
    if (await op('/files/delete', { path: join(dir, e.name) }, `${e.name} deleted.`)) { selected.delete(e.name); load(); }
  }

  async function delSelected() {
    const names = [...selected];
    if (!names.length) return;
    if (names.length === 1) { const e = entries.find((x) => x.name === names[0]); if (e) del(e); return; }
    const dirs = entries.filter((e) => names.includes(e.name) && e.type === 'dir').length;
    if (!await confirmAction(`Delete ${plural(names.length, 'item')}`,
      `Delete ${plural(names.length, 'item')}${dirs ? `, including ${plural(dirs, 'folder')} with everything in them` : ''}? This can't be undone.`, 'Delete')) return;
    if (await op('/files/delete', { paths: names.map((n) => join(dir, n)) }, `${plural(names.length, 'item')} deleted.`)) { selected.clear(); load(); }
  }

  // A small folder browser for Copy/Move. Resolves to the chosen folder, or null.
  async function pickFolder(title, okLabel) {
    let at = dir;
    const crumbsEl = h('nav.crumbs', { 'aria-label': 'Destination' });
    const listEl = h('div.picker-list');
    const err = h('div.error-text');
    const name = h('input', { type: 'text', placeholder: 'New folder name', spellcheck: false, autocapitalize: 'off' });
    const go = async (p) => {
      try {
        const r = await api.get(`/files/list?path=${encodeURIComponent(p)}`);
        at = p;
        err.textContent = '';
        const parts = at ? at.split('/') : [];
        clear(crumbsEl, h('button.crumb', { type: 'button', onclick: () => go('') }, top),
          parts.map((x, i) => [h('span.faint', ' › '), i === parts.length - 1 ? h('strong.crumb-here', x)
            : h('button.crumb', { type: 'button', onclick: () => go(parts.slice(0, i + 1).join('/')) }, x)]));
        const dirs = (r.entries || []).filter((e) => isDirEntry(e) && (showHidden || !e.name.startsWith('.')))
          .sort((a, b) => a.name.localeCompare(b.name, undefined, { numeric: true, sensitivity: 'base' }));
        clear(listEl,
          at ? h('button.picker-row', { type: 'button', onclick: () => go(parent(at)) }, '↑ ..') : null,
          dirs.length ? dirs.map((e) => h('button.picker-row', { type: 'button', onclick: () => go(join(at, e.name)) }, `📁 ${e.name}`))
            : h('div.faint.picker-empty', 'No folders inside.'));
      } catch (e) {
        if (p !== '' && at === p) return go('');
        err.textContent = e.message;
      }
    };
    const mk = async () => {
      const n = name.value.trim();
      if (!n) { name.focus(); return; }
      try {
        await api.post('/files/mkdir', { path: join(at, n) });
        name.value = '';
        await go(join(at, n));
      } catch (e) { err.textContent = e.message; }
    };
    name.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); mk(); } });
    await go(at);
    const v = await modal(title, h('div.stack', crumbsEl, listEl,
      h('div.row', { style: { gap: '8px' } }, name, h('button.btn', { type: 'button', onclick: mk }, 'Create folder')), err), [
      { label: 'Cancel', value: null },
      { label: okLabel, class: 'primary', onclick: () => at },
    ]);
    return typeof v === 'string' ? v : null;
  }

  async function perms(e) {
    const isDir = e.type === 'dir';
    let mode = modeBits(e.mode);
    const octal = h('input.mono', { type: 'text', value: mode.toString(8).padStart(3, '0'), maxLength: 3, size: 4,
      'aria-label': 'Octal', inputmode: 'numeric' });
    const boxes = [];
    const who = ['Owner', 'Group', 'Others'], what = ['Read', 'Write', isDir ? 'Enter' : 'Execute'];
    const grid = h('table.perm-grid',
      h('thead', h('tr', h('th', ''), what.map((w) => h('th', w)))),
      h('tbody', who.map((w, r) => h('tr', h('th', w), what.map((x, c) => {
        const bit = 1 << (8 - (r * 3 + c));
        const b = h('input', { type: 'checkbox', checked: !!(mode & bit), 'aria-label': `${w} ${x}`,
          onchange: () => { mode = b.checked ? mode | bit : mode & ~bit; octal.value = mode.toString(8).padStart(3, '0'); } });
        boxes.push([b, bit]);
        return h('td', b);
      })))));
    octal.addEventListener('input', () => {
      if (!/^[0-7]{3}$/.test(octal.value)) return;
      mode = parseInt(octal.value, 8);
      for (const [b, bit] of boxes) b.checked = !!(mode & bit);
    });
    const rec = h('input', { type: 'checkbox' });
    const err = h('div.error-text');
    await modal(`Permissions for ${e.name}`, h('div.stack', grid,
      h('label.field', 'Octal', octal),
      isDir ? h('label.check-row', rec, h('span', 'Apply to everything inside',
        h('div.faint', { style: { fontSize: '12.5px' } }, 'Files only keep execute if they already had it.'))) : null,
      err), [
      { label: 'Cancel', value: null },
      { label: 'Apply', class: 'primary', onclick: async () => {
        if (!/^[0-7]{3}$/.test(octal.value)) { err.textContent = 'Use three octal digits, like 644.'; return false; }
        try {
          await api.post('/files/chmod', { path: join(dir, e.name), mode: octal.value, recursive: isDir && rec.checked });
          toast('Permissions changed.');
          load();
          return true;
        } catch (x) { err.textContent = x.message; return false; }
      } },
    ]);
  }

  // Images, video, audio and PDFs, with ←/→ through the others in this folder.
  async function preview(e) {
    const items = visible().filter((x) => !isDirEntry(x) && previewKind(x.name));
    let i = Math.max(0, items.findIndex((x) => x.name === e.name));
    const titleEl = h('span');
    const stage = h('div.preview-stage');
    const counter = h('span.faint');
    const prev = h('button.btn', { type: 'button', onclick: () => step(-1), 'aria-label': 'Previous' }, '←');
    const next = h('button.btn', { type: 'button', onclick: () => step(1), 'aria-label': 'Next' }, '→');
    const show = () => {
      const x = items[i];
      const url = api.url(`/files/download?inline=1&path=${encodeURIComponent(join(dir, x.name))}`);
      const kind = previewKind(x.name);
      titleEl.textContent = x.name;
      counter.textContent = items.length > 1 ? `${i + 1} of ${items.length} · ${bytes(x.size)}` : bytes(x.size);
      prev.disabled = next.disabled = items.length < 2;
      clear(stage,
        kind === 'image' ? h('img', { src: url, alt: x.name })
          : kind === 'video' ? h('video', { src: url, controls: true, autoplay: true })
            : kind === 'audio' ? h('audio', { src: url, controls: true, autoplay: true })
              : h('iframe', { src: url, title: x.name }));
    };
    const step = (d) => { if (items.length > 1) { i = (i + d + items.length) % items.length; show(); } };
    const keys = (ev) => {
      if (ev.target.closest?.('video, audio')) return;
      if (ev.key === 'ArrowLeft') step(-1);
      else if (ev.key === 'ArrowRight') step(1);
    };
    show();
    document.addEventListener('keydown', keys);
    await modal(titleEl, h('div.stack', { 'data-wide': '1' }, stage, h('div.row', prev, next, counter)), [
      { label: 'Download', onclick: () => { download([join(dir, items[i].name)]); return false; } },
      { label: 'Close', class: 'primary', value: null },
    ]);
    document.removeEventListener('keydown', keys);
    clear(stage);
  }

  async function edit(path) {
    let content;
    try { content = (await api.get(`/files/read?path=${encodeURIComponent(path)}`)).content; }
    catch (e) {
      if (/text file|too large/.test(e.message)) {
        if (await confirmAction("Can't open this here", e.message.replace(/; download it instead$/, '.') + ' Download it instead?', 'Download', false)) download([path]);
      } else toast(e.message, true);
      return;
    }
    const ta = codeArea(content, 24);
    const err = h('div.error-text');
    let saved = content;
    const save = async () => {
      const text = ta.value;
      try { await api.post('/files/write', { path, content: text }); saved = text; err.textContent = ''; toast('Saved.'); load(); return true; }
      catch (e) { err.textContent = e.message; return false; }
    };
    ta.addEventListener('keydown', (e) => {
      if (e.key === 's' && (e.ctrlKey || e.metaKey) && !e.altKey) { e.preventDefault(); save(); }
    });
    await modal(prefix + path, h('div.stack', { 'data-wide': '1' }, ta, h('div.faint', { style: { fontSize: '12px' } }, 'Ctrl+S saves without closing.'), err), [
      { label: 'Close', value: false },
      { label: 'Download', onclick: () => { download([path]); return false; } },
      { label: 'Save', class: 'primary', onclick: save },
    ], { dirty: () => ta.value !== saved });
  }

  // Uploads go straight to the node through XHR so progress can be shown. Each item is a
  // File and the subfolder it goes in (for dropped folders); emptyDirs are made afterwards.
  function upload(items, emptyDirs = []) {
    const target = dir;
    let replaceAll = false;
    const finish = async () => {
      for (const d of [...emptyDirs].sort((a, b) => a.split('/').length - b.split('/').length)) {
        try { await api.post('/files/mkdir', { path: join(target, d) }); }
        catch (e) { if (!e.message.includes('already exists')) toast(e.message, true); }
      }
      load();
    };
    const next = (i) => {
      if (i >= items.length) { finish(); return; }
      const { file: f, sub } = items[i];
      const bar = h('div.meter', h('span', { style: { width: '0%' } }));
      const label = h('span.muted', '0%');
      const row = h('div.upload-row', h('span.mono', join(sub, f.name)), bar, label);
      uploads.append(row);
      const send = (overwrite) => {
        const xhr = new XMLHttpRequest();
        const qs = `path=${encodeURIComponent(join(target, sub))}&name=${encodeURIComponent(f.name)}${overwrite ? '&overwrite=1' : ''}${sub ? '&parents=1' : ''}`;
        xhr.open('POST', api.url(`/files/upload?${qs}`));
        xhr.setRequestHeader('X-CSRF-Token', csrfToken());
        xhr.setRequestHeader('Content-Type', 'application/octet-stream');
        xhr.upload.onprogress = (e) => {
          if (!e.lengthComputable) return;
          const p = Math.round(e.loaded / e.total * 100);
          bar.firstChild.style.width = p + '%';
          label.textContent = `${p}% of ${bytes(e.total)}`;
        };
        const failed = (msg) => {
          label.textContent = msg;
          label.className = 'error-text';
          setTimeout(() => row.remove(), 8000);
          next(i + 1);
        };
        xhr.onload = async () => {
          let msg = '';
          try { msg = JSON.parse(xhr.responseText).error || ''; } catch {}
          if (xhr.status === 200) {
            label.textContent = 'Done';
            setTimeout(() => row.remove(), 2500);
            next(i + 1);
            return;
          }
          if (!msg.includes('already exists') || overwrite) { failed(msg || `Failed (${xhr.status})`); return; }
          if (replaceAll) { send(true); return; }
          const more = items.length - i > 1;
          const choice = await modal('Replace file', h('p.muted', { style: { margin: 0 } },
            `${join(sub, f.name)} already exists in this folder. Replace it?`), [
            { label: 'Skip', value: 'skip' },
            more ? { label: 'Replace all', value: 'all' } : null,
            { label: 'Replace', value: 'one', class: 'danger solid' },
          ].filter(Boolean));
          if (choice === 'all') replaceAll = true;
          if (choice === 'one' || choice === 'all') send(true);
          else { label.textContent = 'Skipped'; setTimeout(() => row.remove(), 2500); next(i + 1); }
        };
        xhr.onerror = () => failed('Upload failed');
        xhr.send(f);
      };
      send(false);
    };
    next(0);
  }

  drawCrumbs();
  drawTools();
  load();
  return () => { stopped = true; document.removeEventListener('keydown', onKey); };
}
