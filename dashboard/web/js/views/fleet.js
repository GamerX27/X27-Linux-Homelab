import { h, clear, icon, meter, modal, confirmAction, toast, duration, bytes, pct } from '../ui.js';
import * as api from '../api.js';

export function addNodeDialog(onDone) {
  const addr = h('input', { type: 'text', placeholder: '192.168.1.20 or node.lan:9090', autocomplete: 'off', spellcheck: false });
  const pw = h('input', { type: 'text', placeholder: 'XXXX-XXXX-XXXX-XXXX-XXXX', autocomplete: 'off', spellcheck: false,
    class: 'mono', style: { textTransform: 'uppercase' } });
  const name = h('input', { type: 'text', placeholder: 'Defaults to its hostname', autocomplete: 'off' });
  const err = h('div.error-text');
  const body = h('div.stack',
    h('div.notice', 'On the other server, run ', h('code', 'dashboard enable node'),
      ' (or ', h('code', 'dashboard pair'), ' if it already is one). It prints a one-time pairing password.'),
    h('label.field', 'Address', addr),
    h('label.field', 'Pairing password', pw),
    h('label.field', 'Name (optional)', name),
    err);
  return modal('Add node', body, [
    { label: 'Cancel', value: null },
    { label: 'Pair', class: 'primary', onclick: async () => {
      err.textContent = '';
      try {
        const n = await api.post('/api/nodes', { address: addr.value, password: pw.value, name: name.value });
        await modal('Node paired', h('div.stack',
          h('p', { style: { margin: 0 } }, `${n.name} (${n.address}) is paired.`),
          h('p.muted', { style: { margin: 0 } }, 'Check that this certificate fingerprint matches what ', h('code', 'dashboard status'), ' shows on the node:'),
          h('code', { style: { overflowWrap: 'anywhere' } }, n.fingerprint)), [{ label: 'Done', value: true, class: 'primary' }]);
        onDone?.();
        return n;
      } catch (e) { err.textContent = e.message; return false; }
    } },
  ]);
}

function nodeCard(n, refreshNodes) {
  const s = n.summary;
  const menu = h('div.btn-group',
    !n.local ? h('button.btn.small.ghost', { title: 'Rename', onclick: async (e) => {
      e.stopPropagation();
      const input = h('input', { type: 'text', value: n.name });
      const v = await modal('Rename node', h('label.field', 'Name', input), [
        { label: 'Cancel', value: null },
        { label: 'Save', class: 'primary', onclick: async () => {
          try { await api.post(`/api/nodes/${n.id}/rename`, { name: input.value }); } catch (ex) { toast(ex.message, true); return false; }
          return true;
        } }]);
      if (v) refreshNodes();
    } }, 'Rename') : null,
    !n.local ? h('button.btn.small.ghost.danger', { title: 'Remove', onclick: async (e) => {
      e.stopPropagation();
      if (!await confirmAction('Remove node', `Remove ${n.name}? It forgets this main node's token; pair again with a new password from `
        + '`dashboard pair` on the node.', 'Remove')) return;
      try { await api.del(`/api/nodes/${n.id}`); toast(`${n.name} removed.`); refreshNodes(); } catch (ex) { toast(ex.message, true); }
    } }, 'Remove') : null);

  const status = !n.online ? h('span.status.crit', 'Offline')
    : s?.updateAvailable ? h('span.status.warn', 'Update available')
    : s?.version ? h('span.status.good', 'Up to date') : h('span.status.good', 'Online');

  return h('div.card.node-card', { class: n.online ? '' : 'offline', tabindex: 0, role: 'link',
    onclick: () => { location.hash = `#/n/${n.id}/overview`; },
    onkeydown: (e) => { if (e.key === 'Enter') location.hash = `#/n/${n.id}/overview`; } },
  h('div.top',
    h('div', { style: { flex: 1, minWidth: 0 } },
      h('div.row', h('span.title', n.name), n.local ? h('span.badge', 'main') : null),
      h('div.addr', n.local ? 'this node' : n.address)),
    menu),
  n.online && s ? [
    h('div.mini',
      h('div', h('div.label', h('span', 'CPU'), h('span', pct(s.cpuPercent))), meter(s.cpuPercent, 'CPU')),
      h('div', h('div.label', h('span', 'Memory'), h('span', pct(s.memTotal ? s.memUsed / s.memTotal * 100 : 0))),
        meter(s.memTotal ? s.memUsed / s.memTotal * 100 : 0, 'Memory'),
        h('div.faint', { style: { fontSize: '11.5px', marginTop: '3px' } }, `${bytes(s.memUsed)} of ${bytes(s.memTotal)}`))),
    h('dl.kv',
      h('dt', 'Version'), h('dd', h('span.mono', s.version || s.info?.buildId || '—')),
      s.updateAvailable ? [h('dt', 'Available'), h('dd', h('span.badge.update', s.updateVersion || 'newer image'))] : null,
      s.updateStaged ? [h('dt', 'Staged'), h('dd', h('span.badge.warn', 'reboot to apply'))] : null,
      h('dt', 'Containers'), h('dd', `${s.running} running of ${s.containers}`,
        s.containerUpdates ? h('span.badge.update', { style: { marginLeft: '6px' } }, `${s.containerUpdates} image update${s.containerUpdates > 1 ? 's' : ''}`) : null),
      h('dt', 'Uptime'), h('dd', duration(s.uptimeSec))),
  ] : h('div.notice', n.error || 'Not reachable.'),
  h('div.foot', status));
}

export function renderFleet(el, { nodes, refreshNodes, menuButton }) {
  const grid = h('div.grid.nodes');
  const draw = () => {
    const list = nodes();
    const updates = list.filter((n) => n.summary?.updateAvailable).length;
    const offline = list.filter((n) => !n.online).length;
    sub.textContent = `${list.length} node${list.length === 1 ? '' : 's'}`
      + (offline ? ` · ${offline} offline` : '') + (updates ? ` · ${updates} with an OS update available` : '');
    clear(grid, list.length ? list.map((n) => nodeCard(n, refreshNodes)) : h('div.card.empty', 'Loading…'));
  };
  const sub = h('div.sub');
  clear(el,
    h('div.page-head',
      h('div.row', menuButton(), h('div', h('h1', 'All nodes'), sub)),
      h('div.row',
        h('button.btn', { onclick: refreshNodes, title: 'Refresh' }, icon('refresh', 15), 'Refresh'),
        h('button.btn.primary', { onclick: () => addNodeDialog(refreshNodes) }, icon('plus', 15), 'Add node'))),
    grid);
  draw();
  const onNodes = () => draw();
  document.addEventListener('nodes', onNodes);
  return () => document.removeEventListener('nodes', onNodes);
}
