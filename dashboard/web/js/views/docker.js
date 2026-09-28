import { h, clear, bytes, busy, confirmAction, modal, toast, icon, pct } from '../ui.js';
import { renderImageUpdates } from './imageupdates.js';

const SECTIONS = ['Containers', 'Compose', 'Images', 'Image updates', 'Volumes', 'Networks'];

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
  clear(el, h('div.card',
    h('div.card-head', nav, h('div.row', filter, refreshBtn, pruneBtn)),
    body));

  function drawNav() {
    clear(nav, SECTIONS.map((s) => h('button.btn.small', { class: s === section ? 'primary' : '',
      onclick: () => { section = s; filter.value = ''; drawNav(); load(); } },
      s, s === 'Image updates' && pending ? h('span.badge.update', { style: { marginLeft: '2px' } }, String(pending)) : null)));
    pruneBtn.classList.toggle('hidden', section === 'Compose' || section === 'Image updates');
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
    const path = { Containers: '/docker/containers', Compose: '/docker/compose', Images: '/docker/images',
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
    if (!stopped && (section === 'Containers' || section === 'Compose')) timer = setTimeout(load, 5000);
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
    } else if (section === 'Compose') {
      const rows = d.filter((p) => match(p.name, p.dir)).map(projectRow);
      clear(body, table(['Project', 'Containers', 'Status', ''], rows, 'No compose projects running.'));
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
    const ports = [...new Set((c.Ports || []).filter((p) => p.PublicPort).map((p) => `${p.PublicPort}→${p.PrivatePort}/${p.Type}`))];
    return h('tr',
      h('td', h('strong', name), c.Labels?.['com.docker.compose.project'] ? h('div.sub', 'compose: ' + c.Labels['com.docker.compose.project']) : null),
      h('td.mono', { style: { maxWidth: '280px', overflowWrap: 'anywhere' } }, c.Image),
      h('td', h('span.status', { class: stateStatus(c.State) }, c.State), h('div.sub', c.Status)),
      h('td.mono', ports.join(', ') || h('span.faint', '—')),
      h('td.actions', h('div.btn-group',
        h('button.btn.small', { onclick: () => showLogs(name, c.Id) }, 'Logs'),
        running ? h('button.btn.small', { onclick: () => showStats(name, c.Id) }, 'Stats') : null,
        running ? act('Restart', 'restart') : null,
        running ? act('Stop', 'stop') : act('Start', 'start'),
        act('Remove', 'remove', 'danger'))));
  }

  function projectRow(p) {
    const act = (label, action, confirmMsg, cls = '') => {
      const b = h('button.btn.small', { class: cls, onclick: async () => {
        if (confirmMsg && !await confirmAction(`${label} ${p.name}`, confirmMsg, label)) return;
        const r = await busy(b, () => api.post(`/docker/compose/${encodeURIComponent(p.name)}/${action}`), '');
        if (r) {
          toast(`${p.name}: ${action} done.`);
          if (r.output) modal(`docker compose ${action}: ${p.name}`, h('pre.output', r.output), [{ label: 'Close', value: true, class: 'primary' }]);
        }
        load();
      } }, label);
      return b;
    };
    const cls = p.running === p.total ? 'good' : p.running ? 'warn' : 'idle';
    return h('tr',
      h('td', h('strong', p.name), h('div.sub.mono', p.files?.[0] || p.dir || ''),
        !p.filesExist ? h('div.sub', 'compose files not on this host (managed elsewhere)') : null),
      h('td', p.containers.join(', ')),
      h('td', h('span.status', { class: cls }, `${p.running}/${p.total} running`)),
      h('td.actions', h('div.btn-group',
        p.filesExist ? act('Pull', 'pull') : null,
        p.filesExist ? act('Up', 'up') : null,
        act('Restart', 'restart'),
        act('Down', 'down', `Stop and remove ${p.name}'s containers? Named volumes are kept.`, 'danger'))));
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
