import { h, clear, bytes, busy, confirmAction, modal, toast, icon, pct, when, menu, menuOpen, modalOpen } from '../ui.js';
import { openStackEditor } from './stackeditor.js';

const SECTIONS = ['Apps', 'Images', 'Storage'];
const PROJECT = 'com.docker.compose.project';

function stateClass(s) {
  return s === 'running' ? 'good' : s === 'restarting' || s === 'paused' ? 'warn' : s === 'dead' ? 'crit' : 'idle';
}

function ago(unix) {
  const d = (Date.now() / 1000 - unix);
  if (d < 3600) return `${Math.round(d / 60)} min ago`;
  if (d < 86400) return `${Math.round(d / 3600)} h ago`;
  return `${Math.round(d / 86400)} d ago`;
}

const cname = (c) => c.Names?.[0]?.replace(/^\//, '') || c.Id.slice(0, 12);

// Host to open published ports on: the page's own host for the main node, the node's
// address for paired nodes.
function portHost(node) {
  if (!node || node.local || node.id === 'local') return location.hostname;
  if (!node.address) return null; // not known yet: show ports without links
  const m = node.address.match(/^\[([^\]]+)\]/);
  return m ? `[${m[1]}]` : node.address.replace(/:\d+$/, '');
}

// Published ports as links (tcp) or plain text (udp); one entry per host port.
function portLinks(ports, host) {
  const seen = new Set();
  const out = [];
  for (const p of (ports || []).filter((x) => x.PublicPort && x.IP !== '127.0.0.1' && x.IP !== '::1')
    .sort((a, b) => a.PublicPort - b.PublicPort)) {
    const key = `${p.PublicPort}/${p.Type}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const label = p.PublicPort === p.PrivatePort ? `${p.PublicPort}` : `${p.PublicPort}→${p.PrivatePort}`;
    if (p.Type === 'tcp' && host) {
      const scheme = [443, 8443, 9443].includes(p.PrivatePort) ? 'https' : 'http';
      out.push(h('a.port', { href: `${scheme}://${host}:${p.PublicPort}`, target: '_blank', rel: 'noopener noreferrer',
        title: `Open ${scheme}://${host}:${p.PublicPort}`, onclick: (e) => e.stopPropagation() }, label));
    } else {
      out.push(h('span.port.udp', { title: p.Type === 'udp' ? 'UDP' : '' }, p.Type === 'udp' ? `${label}/udp` : label));
    }
  }
  return out.length ? h('div.ports', out) : null;
}

function showOutput(title, text) {
  if (text) modal(title, h('pre.output', text), [{ label: 'Close', value: true, class: 'primary' }]);
}

export function renderDocker(el, { api, node, getNode, refreshNodes }) {
  let section = 'Apps', timer, stopped = false;
  const data = {};
  const expanded = new Map(); // group key → open? (only once the user toggled it)
  const body = h('div.stack');
  const nav = h('div.seg', { role: 'tablist' });
  const filter = h('input.filter-input', { type: 'search', placeholder: 'Filter…', 'aria-label': 'Filter', oninput: () => draw() });
  const newStackBtn = h('button.btn.primary', { onclick: () =>
    openStackEditor({ api, root: data.stacks?.root || '~/docker', onDone: () => load() }) }, icon('plus', 15), 'New stack');
  const refreshBtn = h('button.btn', { onclick: () => load(), title: 'Refresh', 'aria-label': 'Refresh' }, icon('refresh', 15));
  clear(el, h('div.stack',
    h('div.row', nav, h('span.spacer'), filter, refreshBtn, newStackBtn),
    body));

  function drawNav() {
    clear(nav, SECTIONS.map((s) => h('button.seg-btn', { role: 'tab', 'aria-selected': String(s === section), class: s === section ? 'active' : '',
      onclick: () => { section = s; filter.value = ''; drawNav(); clear(body, h('div.card.empty', 'Loading…')); load(); } }, s)));
    newStackBtn.classList.toggle('hidden', section !== 'Apps');
  }

  async function load() {
    clearTimeout(timer);
    const sec = section;
    try {
      if (sec === 'Apps') {
        const [stacks, containers, updates] = await Promise.all([
          api.get('/docker/stacks'), api.get('/docker/containers'), api.get('/docker/updates')]);
        Object.assign(data, { stacks, containers, updates });
      } else if (sec === 'Images') {
        const [images, containers] = await Promise.all([api.get('/docker/images'), api.get('/docker/containers')]);
        Object.assign(data, { images, containers });
      } else {
        const [volumes, networks] = await Promise.all([api.get('/docker/volumes'), api.get('/docker/networks')]);
        Object.assign(data, { volumes, networks });
      }
      if (sec !== section || stopped) return;
      draw();
    } catch (e) {
      if (sec !== section || stopped) return;
      clear(body, h('div.notice.warn', e.message));
    }
    schedule();
  }

  // Refresh Apps every 5 s (3 s while a check runs), but not under an open menu or dialog.
  function schedule() {
    clearTimeout(timer);
    if (stopped || section !== 'Apps') return;
    timer = setTimeout(() => {
      if (menuOpen() || modalOpen()) schedule();
      else load();
    }, data.updates?.checking ? 3000 : 5000);
  }

  const match = (...fields) => {
    const q = filter.value.trim().toLowerCase();
    return !q || fields.some((f) => String(f ?? '').toLowerCase().includes(q));
  };

  // Looked up on every draw: the node list may not have loaded when the tab opened, and the
  // links must point at the node the containers run on, not the main node.
  let host = portHost(node);
  function draw() {
    host = portHost(getNode?.() || node);
    if (section === 'Apps') drawApps();
    else if (section === 'Images') drawImages();
    else drawStorage();
  }

  // ---- Apps: stacks with their containers, and image updates --------------------------

  function updateStrip() {
    const u = data.updates || {};
    const avail = (u.images || []).filter((i) => i.state === 'update');
    const check = h('button.btn.small', { disabled: u.checking, onclick: () =>
      busy(check, () => api.post('/docker/updates/check'), '').then(() => setTimeout(load, 1000)) },
      icon('refresh', 14), u.checking ? 'Checking…' : 'Check now');
    const all = avail.length ? h('button.btn.small.primary', { onclick: async () => {
      if (!await confirmAction('Update all', `Pull the newer images and recreate what uses them? ${avail.map((i) => i.image).join(', ')}. Data is kept.`, 'Update all', false)) return;
      const r = await busy(all, () => api.post('/docker/updates/apply-all'), '');
      if (r) {
        toast(`${r.updated} updated${r.failed ? `, ${r.failed} failed` : ''}.`, r.failed > 0);
        showOutput('Update all', r.output);
        refreshNodes?.();
      }
      load();
    } }, `Update all (${avail.length})`) : null;
    const status = u.checking ? h('span.status.idle', 'Checking registries…')
      : !u.checkedAt ? h('span.status.idle', 'Images not checked yet')
      : avail.length ? h('span.status.warn', `${avail.length} image update${avail.length > 1 ? 's' : ''} available`)
      : h('span.status.good', 'All images up to date');
    const checked = u.checkedAt ? when(u.checkedAt).replace(/^.*\((.*)\)$/, '$1') : '';
    return h('div.update-strip', status, checked ? h('span.faint', `· checked ${checked}`) : null,
      u.error ? h('span.faint', `· ${u.error}`) : null, h('span.spacer'), check, all);
  }

  function drawApps() {
    const stacks = data.stacks?.stacks || [];
    const cs = data.containers || [];
    const upd = new Map((data.updates?.images || []).map((i) => [i.image, i.state]));
    const known = new Set(stacks.map((s) => s.name));
    const groups = stacks.map((s) => ({ key: 's:' + s.name, stack: s, containers: cs.filter((c) => c.Labels?.[PROJECT] === s.name) }));
    const standalone = cs.filter((c) => !known.has(c.Labels?.[PROJECT] || ''));
    if (standalone.length) groups.push({ key: 'standalone', containers: standalone });

    const visible = groups.filter((g) => match(g.stack?.name, g.stack?.dir, ...g.containers.map(cname), ...g.containers.map((c) => c.Image)));
    clear(body,
      updateStrip(),
      visible.length ? h('div.apps', visible.map((g) => groupEl(g, upd)))
        : h('div.card.empty', stacks.length || cs.length ? 'Nothing matches the filter.'
          : ['No apps yet. ', h('button.btn.small.primary', { onclick: () => newStackBtn.click() }, 'Create a stack')]),
      h('div.faint', { style: { fontSize: '12.5px' } }, 'New stacks go in ', h('span.mono', data.stacks?.root || '~/docker'), '/<name>/compose.yml'));
  }

  function groupEl(g, upd) {
    const s = g.stack;
    const updates = new Set(g.containers.filter((c) => upd.get(c.Image) === 'update').map((c) => c.Image)).size;
    const running = g.containers.filter((c) => c.State === 'running').length;
    const total = s ? s.total : g.containers.length;
    const troubled = total > 0 && running < total;
    const open = expanded.has(g.key) ? expanded.get(g.key) : (!s || troubled || updates > 0);
    const toggle = () => { expanded.set(g.key, !open); draw(); };

    let title, sub, status, actions = [];
    if (s) {
      title = s.name;
      sub = s.file || s.dir || '';
      const cls = !total ? 'idle' : running === total ? 'good' : running ? 'warn' : 'crit';
      status = h('span.status', { class: cls }, !total ? 'Not started' : `${running}/${total} running`);
      const act = (label, action, opts = {}) => async (btn) => {
        if (opts.confirm && !await confirmAction(`${label}: ${s.name}`, opts.confirm, label, !!opts.danger)) return;
        const run = () => api.post(`/docker/stacks/${encodeURIComponent(s.name)}/${action}`);
        const r = btn ? await busy(btn, run, '') : await run().catch((e) => { toast(e.message, true); });
        if (r) {
          toast(`${s.name}: ${label.toLowerCase()} done.`);
          if (opts.output) showOutput(`${label}: ${s.name}`, r.output);
          if (action === 'recreate') refreshNodes?.();
        }
        load();
      };
      let primary = null;
      if (updates && s.filesExist) {
        primary = h('button.btn.small.primary', { onclick: (e) => act('Update', 'recreate', { output: true })(e.currentTarget) }, 'Update');
      } else if (running === 0 && s.filesExist) {
        primary = h('button.btn.small', { onclick: (e) => act('Start', 'start')(e.currentTarget) }, 'Start');
      }
      actions = [primary, menu([
        { label: 'Start', onclick: () => act('Start', 'start')(), hidden: running > 0 || !s.filesExist },
        { label: 'Stop', onclick: () => act('Stop', 'stop')(), hidden: running === 0 },
        { label: 'Restart', onclick: () => act('Restart', 'restart')(), hidden: running === 0 },
        { label: 'Pull & recreate', hidden: !s.filesExist, onclick: () => act('Pull & recreate', 'recreate', { output: true,
          confirm: `Pull the newest images for ${s.name} and recreate its containers? Data in volumes and bind mounts is kept.` })() },
        { label: 'Edit compose file', hidden: !s.inRoot, onclick: () => openStackEditor({ api, root: data.stacks?.root, name: s.name, onDone: () => load() }) },
        { label: 'Remove…', danger: true, hidden: !total && !s.inRoot, onclick: () => removeStack(s) },
      ], `Actions for ${s.name}`)];
    } else {
      title = 'Standalone containers';
      sub = 'started with docker run';
      status = h('span.status', { class: running === total ? 'good' : running ? 'warn' : 'idle' }, `${running}/${total} running`);
    }

    const head = h('div.app-head', { onclick: (e) => { if (!e.target.closest('button, a')) toggle(); } },
      h('button.chev', { class: open ? 'open' : '', 'aria-expanded': String(open), 'aria-label': `${open ? 'Collapse' : 'Expand'} ${title}`, onclick: toggle }, '›'),
      h('div.app-title',
        h('div.row', { style: { gap: '8px' } }, h('strong', title),
          updates ? h('span.badge.update', `${updates} update${updates > 1 ? 's' : ''}`) : null,
          s && !s.filesExist ? h('span.badge', 'managed elsewhere') : s && !s.inRoot ? h('span.badge', 'outside ~/docker') : null),
        h('div.sub.mono', sub)),
      h('div.app-status', status),
      h('div.app-ports', portLinks(s ? s.ports : [], host)),
      h('div.app-actions', actions));
    return h('div.app-group', { class: open ? 'open' : '' }, head,
      open ? h('div.app-body', g.containers.length
        ? g.containers.sort((a, b) => cname(a).localeCompare(cname(b))).map((c) => containerEl(c, upd, !s))
        : h('div.faint', { style: { padding: '10px 14px' } }, 'No containers. Start the stack to create them.')) : null);
  }

  function containerEl(c, upd, standalone) {
    const name = cname(c);
    const running = c.State === 'running';
    const hasUpdate = upd.get(c.Image) === 'update';
    const act = (label, action, confirmMsg) => async () => {
      if (confirmMsg && !await confirmAction(`${label}: ${name}`, confirmMsg, label, action === 'remove')) return;
      try {
        const r = await api.post(`/docker/containers/${c.Id}/${action}`);
        toast(`${name}: ${label.toLowerCase()} done.`);
        if (action === 'recreate') { showOutput(`Pull & recreate: ${name}`, r.output); refreshNodes?.(); }
      } catch (e) { toast(e.message, true); }
      load();
    };
    const recreateMsg = c.Labels?.[PROJECT]
      ? `Pulls the newest ${c.Image} and recreates ${name} through its stack. Volumes and bind mounts are kept.`
      : `Pulls the newest ${c.Image} and recreates ${name} with the same ports, environment, volumes, networks and restart policy. Its data is kept.`;
    const updateBtn = hasUpdate && standalone ? h('button.btn.small.primary', { onclick: async (e) => {
      const r = await busy(e.currentTarget, () => api.post(`/docker/containers/${c.Id}/recreate`), '');
      if (r) { toast(`${name} updated.`); showOutput(`Update: ${name}`, r.output); refreshNodes?.(); }
      load();
    } }, 'Update') : null;
    return h('div.ct-row',
      h('div.ct-name', h('span.dot', { class: stateClass(c.State), 'aria-hidden': 'true' }),
        h('button.link-btn', { onclick: () => showLogs(name, c.Id), title: 'Show logs' }, name)),
      h('div.ct-image.mono', { title: c.Image }, c.Image, hasUpdate ? h('span.badge.update', { style: { marginLeft: '6px' } }, 'update') : null),
      h('div.ct-state', h('span', { class: `status ${stateClass(c.State)}` }, c.State), h('span.faint', ` · ${c.Status}`)),
      h('div.app-ports', portLinks(c.Ports, host)),
      h('div.app-actions', updateBtn, menu([
        { label: 'Logs', onclick: () => showLogs(name, c.Id) },
        { label: 'Stats', onclick: () => showStats(name, c.Id), hidden: !running },
        { label: 'Start', onclick: act('Start', 'start'), hidden: running },
        { label: 'Stop', onclick: act('Stop', 'stop'), hidden: !running },
        { label: 'Restart', onclick: act('Restart', 'restart'), hidden: !running },
        { label: 'Pull & recreate', onclick: act('Pull & recreate', 'recreate', recreateMsg) },
        { label: 'Remove', danger: true, onclick: act('Remove', 'remove', `Remove ${name}? Its volumes are kept.`) },
      ], `Actions for ${name}`)));
  }

  async function removeStack(s) {
    const del = h('input', { type: 'checkbox' });
    const warn = h('div.notice.warn.hidden', 'Everything in the folder is deleted, including bind-mounted data like ./config. This can\'t be undone.');
    del.addEventListener('change', () => warn.classList.toggle('hidden', !del.checked));
    const bodyEl = h('div.stack',
      h('p.muted', { style: { margin: 0 } }, `Stops and removes ${s.name}'s containers and network. Named volumes are kept.`),
      s.inRoot ? h('label.check-row', del, h('span', 'Also delete the folder ', h('code', s.dir), ' and everything in it')) : null,
      warn);
    const ok = await modal(`Remove ${s.name}`, bodyEl, [
      { label: 'Cancel', value: false },
      { label: 'Remove', class: 'danger solid', onclick: async () => {
        try {
          const r = await api.post(`/docker/stacks/${encodeURIComponent(s.name)}/remove`, { deleteFolder: del.checked });
          toast(del.checked ? `${s.name} removed and its folder deleted.` : `${s.name} removed; its folder is kept.`);
          return r || true;
        } catch (e) { toast(e.message, true); return false; }
      } },
    ]);
    if (ok) load();
  }

  // ---- Images and Storage -------------------------------------------------------------

  const table = (head, rows, empty) => rows.length
    ? h('div.table-wrap', h('table', h('thead', h('tr', head.map((x) => h('th', x)))), h('tbody', rows)))
    : h('div.empty', empty);

  function pruneBtn(what, msg) {
    const b = h('button.btn.small.danger', { onclick: async () => {
      if (!await confirmAction(`Prune ${what}`, msg, 'Prune')) return;
      await busy(b, () => api.post(`/docker/prune/${what}`));
      load();
    } }, 'Prune unused');
    return b;
  }

  function drawImages() {
    const used = new Set((data.containers || []).map((c) => c.ImageID));
    const rows = (data.images || []).filter((i) => match(i.RepoTags?.join(' '), i.Id))
      .sort((a, b) => b.Created - a.Created)
      .map((i) => h('tr',
        h('td', (i.RepoTags?.length && i.RepoTags[0] !== '<none>:<none>' ? i.RepoTags : ['<untagged>']).map((t) => h('div.mono', t)),
          h('div.sub.mono', i.Id.slice(7, 19))),
        h('td.num-cell', bytes(i.Size)),
        h('td.num-cell.muted', ago(i.Created)),
        h('td', used.has(i.Id) ? h('span.status.good', 'In use') : h('span.status.idle', 'Unused')),
        h('td.actions', menu([{ label: 'Remove', danger: true, onclick: async () => {
          if (!await confirmAction('Remove image', `Remove ${i.RepoTags?.[0] || i.Id.slice(7, 19)}?`, 'Remove')) return;
          try { await api.post('/docker/images/remove', { id: i.Id }); toast('Image removed.'); } catch (e) { toast(e.message, true); }
          load();
        } }], 'Image actions'))));
    clear(body, h('div.card',
      h('div.card-head', h('h2', `Images (${(data.images || []).length})`),
        pruneBtn('images', 'Remove every image no container uses? They are pulled again when needed.')),
      table(['Image', 'Size', 'Created', 'Status', ''], rows, 'No images.')));
  }

  function drawStorage() {
    const vols = (data.volumes || []).filter((v) => match(v.Name)).sort((a, b) => a.Name.localeCompare(b.Name))
      .map((v) => h('tr', h('td.mono', v.Name), h('td', v.Driver), h('td.mono.muted', v.Mountpoint)));
    const nets = (data.networks || []).filter((n) => match(n.Name, n.Driver)).sort((a, b) => a.Name.localeCompare(b.Name))
      .map((n) => h('tr', h('td.mono', n.Name), h('td', n.Driver), h('td', n.Scope), h('td.mono.muted', n.Id.slice(0, 12))));
    clear(body,
      h('div.card', h('div.card-head', h('h2', `Volumes (${(data.volumes || []).length})`),
        pruneBtn('volumes', 'Remove every volume no container uses? Data in them is deleted for good.')),
      table(['Name', 'Driver', 'Mount point'], vols, 'No volumes.')),
      h('div.card', h('div.card-head', h('h2', `Networks (${(data.networks || []).length})`),
        pruneBtn('networks', 'Remove all networks no container uses?')),
      table(['Name', 'Driver', 'Scope', 'ID'], nets, 'No networks.')));
  }

  // ---- Logs and stats -------------------------------------------------------------------

  async function showLogs(name, id) {
    const pre = h('pre.output', { style: { maxHeight: '60vh' } }, 'Loading…');
    const follow = h('input', { type: 'checkbox', checked: true });
    let t, open = true;
    const fetchLogs = async () => {
      try {
        const r = await api.get(`/docker/containers/${id}/logs?tail=500`);
        const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
        pre.textContent = r.logs || '(no output)';
        if (atBottom || pre.dataset.first !== '1') { pre.scrollTop = pre.scrollHeight; pre.dataset.first = '1'; }
      } catch (e) { pre.textContent = e.message; }
      if (open && follow.checked) t = setTimeout(fetchLogs, 3000);
    };
    follow.addEventListener('change', () => { if (follow.checked) fetchLogs(); else clearTimeout(t); });
    fetchLogs();
    await modal(`Logs: ${name}`, h('div.stack', { 'data-wide': '1' }, h('label.row.muted', follow, 'Refresh every 3 seconds'), pre),
      [{ label: 'Close', value: true, class: 'primary' }]);
    open = false; clearTimeout(t);
  }

  async function showStats(name, id) {
    const box = h('dl.kv', 'Loading…');
    let t, open = true;
    const tick = async () => {
      try {
        const s = await api.get(`/docker/containers/${id}/stats`);
        clear(box,
          h('dt', 'CPU'), h('dd', pct(s.cpuPercent)),
          h('dt', 'Memory'), h('dd', `${bytes(s.memUsed)}${s.memLimit ? ' of ' + bytes(s.memLimit) : ''}`),
          h('dt', 'Network in / out'), h('dd', `${bytes(s.netRx)} / ${bytes(s.netTx)}`));
      } catch (e) { clear(box, e.message); }
      if (open) t = setTimeout(tick, 3000);
    };
    tick();
    await modal(`Stats: ${name}`, box, [{ label: 'Close', value: true, class: 'primary' }]);
    open = false; clearTimeout(t);
  }

  drawNav();
  clear(body, h('div.card.empty', 'Loading…'));
  load();
  // Redraw when the node list (and so this node's address) arrives or changes.
  const onNodes = () => { if (!menuOpen() && !modalOpen() && data.containers) draw(); };
  document.addEventListener('nodes', onNodes);
  return () => { stopped = true; clearTimeout(timer); document.removeEventListener('nodes', onNodes); };
}
