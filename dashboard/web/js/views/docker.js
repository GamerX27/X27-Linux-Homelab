import { h, clear, bytes, busy, confirmAction, modal, toast, icon, pct } from '../ui.js';
import { renderImageUpdates } from './imageupdates.js';
import { openStackEditor } from './stackeditor.js';

const SECTIONS = ['Containers', 'Stacks', 'Images', 'Image updates', 'Volumes', 'Networks'];

// Host to open published ports on: the page's own host for the main node, the node's
// address for paired nodes.
function portHost(node) {
  if (!node || node.local || node.id === 'local' || !node.address) return location.hostname;
  const a = node.address;
  const m = a.match(/^\[([^\]]+)\]/);
  return m ? `[${m[1]}]` : a.replace(/:\d+$/, '');
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
    if (p.Type === 'tcp') {
      const scheme = [443, 8443, 9443].includes(p.PrivatePort) ? 'https' : 'http';
      out.push(h('a.port', { href: `${scheme}://${host}:${p.PublicPort}`, target: '_blank', rel: 'noopener noreferrer',
        title: `Open ${scheme}://${host}:${p.PublicPort}` }, label));
    } else {
      out.push(h('span.port.udp', { title: 'UDP' }, `${label}/udp`));
    }
  }
  return out.length ? h('div.ports', out) : h('span.faint', '—');
}

function stateStatus(s) {
  return s === 'running' ? 'good' : s === 'restarting' || s === 'paused' ? 'warn' : s === 'dead' ? 'crit' : 'idle';
}

function ago(unix) {
  const d = (Date.now() / 1000 - unix);
  if (d < 3600) return `${Math.round(d / 60)} min ago`;
  if (d < 86400) return `${Math.round(d / 3600)} h ago`;
  return `${Math.round(d / 86400)} d ago`;
}

export function renderDocker(el, { api, node, refreshNodes }) {
  let section = 'Containers', timer, stopped = false, sub = null;
  let pending = node?.summary?.containerUpdates;
  const body = h('div');
  const filter = h('input.filter-input', { type: 'search', placeholder: 'Filter…', 'aria-label': 'Filter', oninput: () => draw() });
  const nav = h('div.btn-group');
  let data = {};

  const pruneBtn = h('button.btn.danger', { onclick: async () => {
    const what = section.toLowerCase();
    const msg = {
      containers: 'Remove all stopped containers?',
      images: 'Remove every image no container uses? They are pulled again when needed.',
      volumes: 'Remove every volume no container uses? Data in them is deleted for good.',
      networks: 'Remove all networks no container uses?',
    }[what];
    if (!msg || !await confirmAction(`Prune ${what}`, msg, 'Prune')) return;
    await busy(pruneBtn, () => api.post(`/docker/prune/${what}`));
    load();
  } }, 'Prune unused');

  const refreshBtn = h('button.btn', { onclick: () => load(), title: 'Refresh' }, icon('refresh', 15));
  const newStackBtn = h('button.btn.primary', { onclick: () =>
    openStackEditor({ api, root: data.Stacks?.root || '~/docker', onDone: () => load() }) }, icon('plus', 15), 'New stack');
  const host = portHost(node);
  clear(el, h('div.card',
    h('div.card-head', nav, h('div.row', filter, refreshBtn, pruneBtn, newStackBtn)),
    body));

  function drawNav() {
    clear(nav, SECTIONS.map((s) => h('button.btn.small', { class: s === section ? 'primary' : '',
      onclick: () => { section = s; filter.value = ''; drawNav(); load(); } },
      s, s === 'Image updates' && pending ? h('span.badge.update', { style: { marginLeft: '2px' } }, String(pending)) : null)));
    pruneBtn.classList.toggle('hidden', section === 'Stacks' || section === 'Image updates');
    newStackBtn.classList.toggle('hidden', section !== 'Stacks');
    filter.classList.toggle('hidden', section === 'Image updates');
    refreshBtn.classList.toggle('hidden', section === 'Image updates');
  }

  async function load() {
    clearTimeout(timer);
    sub?.();
    sub = null;
    if (section === 'Image updates') {
      sub = renderImageUpdates(body, { api, refreshNodes });
      return;
    }
    const path = { Containers: '/docker/containers', Stacks: '/docker/stacks', Images: '/docker/images',
      Volumes: '/docker/volumes', Networks: '/docker/networks' }[section];
    const sec = section;
    try {
      const d = await api.get(path);
      if (sec === 'Images' && !data.Containers) data.Containers = await api.get('/docker/containers');
      if (sec !== section || stopped) return; // switched sections while loading
      data[sec] = d;
      draw();
    } catch (e) {
      if (sec !== section || stopped) return;
      clear(body, h('div.notice.warn', e.message));
    }
    if (!stopped && (section === 'Containers' || section === 'Stacks')) timer = setTimeout(load, 5000);
  }

  const match = (...fields) => {
    const q = filter.value.trim().toLowerCase();
    return !q || fields.some((f) => String(f ?? '').toLowerCase().includes(q));
  };

  function draw() {
    const d = data[section] || [];
    const table = (head, rows, empty) => rows.length
      ? h('div.table-wrap', h('table', h('thead', h('tr', head.map((x) => h('th', x)))), h('tbody', rows)))
      : h('div.empty', empty);

    if (section === 'Containers') {
      const rows = d.filter((c) => match(c.Names[0], c.Image, c.State, c.Labels?.['com.docker.compose.project']))
        .sort((a, b) => (a.Names[0] || '').localeCompare(b.Names[0] || ''))
        .map(containerRow);
      clear(body, table(['Name', 'Image', 'State', 'Ports', ''], rows, 'No containers.'));
    } else if (section === 'Stacks') {
      const st = data.Stacks || {};
      const rows = (st.stacks || []).filter((p) => match(p.name, p.dir, p.containers?.join(' '))).map(stackRow);
      clear(body,
        h('div.faint', { style: { marginBottom: '8px', fontSize: '12.5px' } }, 'Stacks folder: ', h('span.mono', st.root || '~/docker'),
          ' · one folder per stack with its compose.yml'),
        rows.length ? table(['Stack', 'Containers', 'Ports', 'Status', ''], rows, '')
          : h('div.empty', 'No stacks yet. ', h('button.btn.small.primary', { onclick: () => newStackBtn.click() }, 'Create one')));
    } else if (section === 'Images') {
      const used = new Set((data.Containers || []).map((c) => c.ImageID));
      const rows = d.filter((i) => match(i.RepoTags?.join(' '), i.Id))
        .sort((a, b) => b.Created - a.Created)
        .map((i) => {
          const btn = h('button.btn.small.danger', { onclick: async () => {
            if (!await confirmAction('Remove image', `Remove ${i.RepoTags?.[0] || i.Id.slice(7, 19)}?`, 'Remove')) return;
            await busy(btn, () => api.post('/docker/images/remove', { id: i.Id }), 'Image removed.');
            load();
          } }, 'Remove');
          return h('tr',
            h('td', (i.RepoTags?.length && i.RepoTags[0] !== '<none>:<none>' ? i.RepoTags : ['<untagged>']).map((t) => h('div.mono', t)),
              h('div.sub.mono', i.Id.slice(7, 19))),
            h('td.num-cell', bytes(i.Size)),
            h('td.num-cell.muted', ago(i.Created)),
            h('td', used.has(i.Id) ? h('span.status.good', 'In use') : h('span.status.idle', 'Unused')),
            h('td.actions', btn));
        });
      clear(body, table(['Image', 'Size', 'Created', 'Status', ''], rows, 'No images.'));
    } else if (section === 'Volumes') {
      const rows = d.filter((v) => match(v.Name)).sort((a, b) => a.Name.localeCompare(b.Name))
        .map((v) => h('tr', h('td.mono', v.Name), h('td', v.Driver), h('td.mono.muted', v.Mountpoint)));
      clear(body, table(['Name', 'Driver', 'Mount point'], rows, 'No volumes.'));
    } else {
      const rows = d.filter((n) => match(n.Name, n.Driver)).sort((a, b) => a.Name.localeCompare(b.Name))
        .map((n) => h('tr', h('td.mono', n.Name), h('td', n.Driver), h('td', n.Scope), h('td.mono.muted', n.Id.slice(0, 12))));
      clear(body, table(['Name', 'Driver', 'Scope', 'ID'], rows, 'No networks.'));
    }
  }

  function containerRow(c) {
    const name = c.Names[0]?.replace(/^\//, '') || c.Id.slice(0, 12);
    const running = c.State === 'running';
    const act = (label, action, cls = '') => {
      const b = h('button.btn.small', { class: cls, onclick: async () => {
        if (action === 'remove' && !await confirmAction('Remove container', `Remove ${name}? Its volumes are kept.`, 'Remove')) return;
        await busy(b, () => api.post(`/docker/containers/${c.Id}/${action}`), `${name}: ${action} done.`);
        load();
      } }, label);
      return b;
    };
    const project = c.Labels?.['com.docker.compose.project'];
    const recreate = h('button.btn.small', { title: 'Pull the newest image and recreate this container, keeping its data', onclick: async () => {
      const how = project
        ? `Pulls the newest ${c.Image} and recreates ${name} through its compose stack (${project}).`
        : `Pulls the newest ${c.Image} and recreates ${name} with the same settings: ports, environment, networks, restart policy.`;
      if (!await confirmAction('Pull & recreate', `${how} Volumes and bind mounts are kept, so its data stays.`, 'Pull & recreate', false)) return;
      const r = await busy(recreate, () => api.post(`/docker/containers/${c.Id}/recreate`), '');
      if (r) {
        toast(`${name} recreated.`);
        if (r.output) modal(`Pull & recreate: ${name}`, h('pre.output', r.output), [{ label: 'Close', value: true, class: 'primary' }]);
      }
      load();
    } }, 'Pull & recreate');
    return h('tr',
      h('td', h('strong', name), c.Labels?.['com.docker.compose.project'] ? h('div.sub', 'compose: ' + c.Labels['com.docker.compose.project']) : null),
      h('td.mono', { style: { maxWidth: '280px', overflowWrap: 'anywhere' } }, c.Image),
      h('td', h('span.status', { class: stateStatus(c.State) }, c.State), h('div.sub', c.Status)),
      h('td', portLinks(c.Ports, host)),
      h('td.actions', h('div.btn-group',
        h('button.btn.small', { onclick: () => showLogs(name, c.Id) }, 'Logs'),
        running ? h('button.btn.small', { onclick: () => showStats(name, c.Id) }, 'Stats') : null,
        running ? act('Restart', 'restart') : null,
        running ? act('Stop', 'stop') : act('Start', 'start'),
        recreate,
        act('Remove', 'remove', 'danger'))));
  }

  function stackRow(p) {
    const act = (label, action, opts = {}) => {
      const b = h('button.btn.small', { class: opts.cls || '', title: opts.title || '', onclick: async () => {
        if (opts.confirm && !await confirmAction(`${label}: ${p.name}`, opts.confirm, label, !!opts.danger)) return;
        const r = await busy(b, () => api.post(`/docker/stacks/${encodeURIComponent(p.name)}/${action}`), '');
        if (r) {
          toast(`${p.name}: ${label.toLowerCase()} done.`);
          if (r.output && opts.showOutput) modal(`${label}: ${p.name}`, h('pre.output', r.output), [{ label: 'Close', value: true, class: 'primary' }]);
        }
        load();
      } }, label);
      return b;
    };
    const running = p.running > 0;
    const cls = !p.total ? 'idle' : p.running === p.total ? 'good' : p.running ? 'warn' : 'idle';
    const status = !p.total ? 'Not started' : `${p.running}/${p.total} running`;
    return h('tr',
      h('td', h('strong', p.name), h('div.sub.mono', p.file || p.dir || ''),
        !p.filesExist ? h('div.sub', 'compose files not on this host (managed elsewhere)')
          : !p.inRoot ? h('div.sub', 'outside the stacks folder') : null),
      h('td', p.containers?.length ? p.containers.join(', ') : h('span.faint', '—')),
      h('td', portLinks(p.ports, host)),
      h('td', h('span.status', { class: cls }, status)),
      h('td.actions', h('div.btn-group',
        running ? act('Stop', 'stop') : p.filesExist ? act('Start', 'start') : null,
        running ? act('Restart', 'restart') : null,
        p.filesExist ? act('Pull & recreate', 'recreate', { cls: 'primary', showOutput: true,
          title: 'Pull newer images and recreate the containers; volumes and bind mounts are kept',
          confirm: `Pull the newest images for ${p.name} and recreate its containers? Data in volumes and bind mounts is kept.` }) : null,
        p.inRoot ? h('button.btn.small', { onclick: () =>
          openStackEditor({ api, root: data.Stacks?.root, name: p.name, onDone: () => load() }) }, 'Edit') : null,
        p.total ? act('Remove', 'remove', { cls: 'danger', danger: true,
          confirm: `Stop and remove ${p.name}'s containers? The stack folder, its files and named volumes are kept, so Start brings it back.` }) : null)));
  }

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
    const b = h('div.stack', { 'data-wide': '1' }, h('label.row.muted', follow, 'Refresh every 3 seconds'), pre);
    await modal(`Logs: ${name}`, b, [{ label: 'Close', value: true, class: 'primary' }]);
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
  load();
  // The node list refreshes every 15 s; keep the pending-updates count current.
  const onNodes = (e) => {
    const n = e.detail.find((x) => x.id === node?.id);
    if (n && n.summary?.containerUpdates !== pending) { pending = n.summary?.containerUpdates; drawNav(); }
  };
  document.addEventListener('nodes', onNodes);
  return () => { stopped = true; clearTimeout(timer); sub?.(); document.removeEventListener('nodes', onNodes); };
}
