import { h, clear, icon, meter, modal, confirmAction, toast, duration, bytes, pct, menu, menuOpen } from '../ui.js';
import * as api from '../api.js';
import { startUpdate } from './rollout.js';

// Pairing passwords: 20 characters from this alphabet, shown in groups of four.
const PAIR_ALPHABET = '23456789ABCDEFGHJKMNPQRSTUVWXYZ';

function checkPairPassword(raw) {
  const s = raw.toUpperCase().replace(/[\s-]/g, '');
  const bad = [...new Set([...s].filter((c) => !PAIR_ALPHABET.includes(c)))];
  if (bad.length) {
    const hint = bad.map((c) => ({ O: 'O isn\'t used, it may be D or Q', 0: '0 isn\'t used, it may be D or Q', I: 'I isn\'t used, it may be J or 7',
      L: 'L isn\'t used, it may be 7', 1: '1 isn\'t used, it may be 7 or J' }[c] || `"${c}" can't be in a pairing password`));
    return `Check the password: ${hint.join('; ')}. Only 2-9 and A-Z without I, L and O are used.`;
  }
  if (s.length !== 20) return `The password has ${s.length} characters; it should have 20 (five groups of four).`;
  return '';
}

export function addNodeDialog(onDone) {
  const addr = h('input', { type: 'text', placeholder: '192.168.1.20 or node.lan:9090', autocomplete: 'off', spellcheck: false });
  const pw = h('input', { type: 'text', placeholder: 'XXXX-XXXX-XXXX-XXXX-XXXX', autocomplete: 'off', spellcheck: false,
    class: 'mono', style: { textTransform: 'uppercase' } });
  const name = h('input', { type: 'text', placeholder: 'Defaults to its hostname', autocomplete: 'off' });
  const err = h('div.error-text');
  pw.addEventListener('blur', () => { err.textContent = pw.value.trim() ? checkPairPassword(pw.value) : ''; });
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
      err.textContent = checkPairPassword(pw.value);
      if (err.textContent) return false;
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

// ---- Node cards ------------------------------------------------------------------------

const go = (id, tab) => (e) => { e.stopPropagation(); location.hash = `#/n/${id}/${tab}`; };

function nodeCard(n, refreshNodes) {
  const s = n.summary;
  const actions = n.local ? null : menu([
    { label: 'Rename', onclick: async () => {
      const input = h('input', { type: 'text', value: n.name });
      const v = await modal('Rename node', h('label.field', 'Name', input), [
        { label: 'Cancel', value: null },
        { label: 'Save', class: 'primary', onclick: async () => {
          try { await api.post(`/api/nodes/${n.id}/rename`, { name: input.value }); } catch (ex) { toast(ex.message, true); return false; }
          return true;
        } }]);
      if (v) refreshNodes();
    } },
    { label: 'Remove', danger: true, onclick: async () => {
      if (!await confirmAction('Remove node', `Remove ${n.name}? It forgets this main node's token; pair again with a new password from `
        + '`dashboard pair` on the node.', 'Remove')) return;
      try { await api.del(`/api/nodes/${n.id}`); toast(`${n.name} removed.`); refreshNodes(); } catch (ex) { toast(ex.message, true); }
    } },
  ], `Actions for ${n.name}`);

  const status = !n.online ? h('span.status.crit', 'Offline')
    : s?.updating ? h('span.status.warn', 'Updating OS…')
    : s?.updateStaged ? h('span.status.warn', 'Reboot to finish update')
    : s?.updateAvailable ? h('span.status.warn', 'OS update available')
    : h('span.status.good', 'Online');

  const fact = (label, value, sub) => h('div.fact', h('div.fact-label', label), h('div.fact-value', value), sub ? h('div.fact-sub', sub) : null);

  let content;
  if (n.online && s) {
    const memPct = s.memTotal ? s.memUsed / s.memTotal * 100 : 0;
    const allUp = s.running === s.containers;
    content = [
      h('div.mini',
        h('div', h('div.label', h('span', 'CPU'), h('span', pct(s.cpuPercent))), meter(s.cpuPercent, 'CPU')),
        h('div', h('div.label', h('span', 'Memory'), h('span', pct(memPct))), meter(memPct, 'Memory'),
          h('div.fact-sub', `${bytes(s.memUsed)} of ${bytes(s.memTotal)}`))),
      h('div.facts',
        fact('Containers', h('span', h('span.dot', { class: s.containers ? (allUp ? 'good' : 'warn') : '', style: { marginRight: '6px', display: 'inline-block' } }),
          `${s.running}/${s.containers}`), 'running'),
        fact('Uptime', duration(s.uptimeSec)),
        fact('Version', h('span.mono', s.version || '—'), s.version ? null : 'not an image')),
      (s.updateAvailable || s.updateStaged || s.containerUpdates) ? h('div.row', { style: { gap: '6px' } },
        s.updateAvailable ? h('a.badge.update', { href: `#/n/${n.id}/updates`, onclick: go(n.id, 'updates') }, `OS ${s.updateVersion || 'update'}`) : null,
        s.updateStaged ? h('a.badge.warn', { href: `#/n/${n.id}/updates`, onclick: go(n.id, 'updates') }, 'reboot to apply') : null,
        s.containerUpdates ? h('a.badge.update', { href: `#/n/${n.id}/docker`, onclick: go(n.id, 'docker') },
          `${s.containerUpdates} image update${s.containerUpdates > 1 ? 's' : ''}`) : null) : null,
    ];
  } else {
    content = h('div.notice', n.error || 'Not reachable.');
  }

  return h('div.card.node-card', { class: n.online ? '' : 'offline', tabindex: 0, role: 'link',
    onclick: (e) => { if (!e.target.closest('button, a, .menu')) location.hash = `#/n/${n.id}/overview`; },
    onkeydown: (e) => { if (e.key === 'Enter' && e.target === e.currentTarget) location.hash = `#/n/${n.id}/overview`; } },
  h('div.top',
    h('div', { style: { flex: 1, minWidth: 0 } },
      h('div.row', { style: { gap: '8px' } }, h('span.title', n.name), n.local ? h('span.badge', 'main') : null),
      h('div.addr', n.local ? 'this node' : n.address)),
    status, actions),
  content);
}

// ---- Global updates ----------------------------------------------------------------------

// Check progress survives redraws (the node list refreshes every 15 s) and leaving the page.
// OS updates themselves run on the main node and are followed on the update page.
const progress = { os: new Map(), ct: new Map(), busy: { os: false, ct: false } };

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function pollUntil(fn, everyMs, maxMs) {
  const end = Date.now() + maxMs;
  while (Date.now() < end) {
    try { if (await fn()) return true; } catch { /* node busy or rebooting */ }
    await sleep(everyMs);
  }
  return false;
}

function progressList(map, nodesById) {
  if (!map.size) return null;
  return h('ul.progress-list', [...map].map(([id, p]) => h('li',
    h('span', { class: `status ${p.cls}` }, nodesById.get(id)?.name || id),
    h('span.muted', p.msg),
    p.log ? h('button.link-btn', { onclick: () => modal(`Log: ${nodesById.get(id)?.name || id}`, h('pre.output', p.log),
      [{ label: 'Close', value: true, class: 'primary' }]) }, 'Log') : null)));
}

// Asks which nodes to update, then starts the update and opens its page.
async function updateDialog(candidates) {
  const boxes = candidates.map((n) => {
    const box = h('input', { type: 'checkbox', checked: true, value: n.id });
    const to = n.summary.updateStaged && !n.summary.updateAvailable ? 'staged update' : n.summary.updateVersion || 'newer image';
    return h('label.check-row', box, h('span', h('strong', n.name), n.local ? ' (main node, goes last)' : '',
      h('span.muted', ` · ${n.summary.version || '?'} → ${to}`)));
  });
  const err = h('div.error-text');
  return modal('Update OS', h('div.stack',
    h('p.muted', { style: { margin: 0 } }, 'The nodes download the new image at the same time. Then they reboot one at a time, and each '
      + 'one has to come back on the new image before the next one goes. If a node fails, the update stops there. Containers restart with their node.'),
    h('div.stack', { style: { gap: '6px' } }, boxes),
    err),
  [{ label: 'Cancel', value: false }, { label: 'Update', class: 'primary', onclick: async () => {
    const ids = boxes.map((b) => b.querySelector('input')).filter((i) => i.checked).map((i) => i.value);
    if (!ids.length) { err.textContent = 'Choose at least one node.'; return false; }
    try { await startUpdate(ids); } catch (e) { err.textContent = e.message; return false; }
    return true;
  } }]);
}

function updatesPanel(nodes, job, refreshNodes, redraw) {
  const online = nodes.filter((n) => n.online && n.summary);
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const set = (kind, id, cls, msg, log) => { progress[kind].set(id, { cls, msg, log }); redraw(); };

  // OS
  const osImage = online.filter((n) => n.summary.version);
  const osUpdating = online.filter((n) => n.summary.updating);
  const osCandidates = osImage.filter((n) => (n.summary.updateAvailable || n.summary.updateStaged) && !n.summary.updating);
  const running = job?.state === 'running';

  const osCheck = h('button.btn.small', { disabled: progress.busy.os, onclick: async () => {
    progress.busy.os = true; progress.os.clear();
    await Promise.all(osImage.map(async (n) => {
      set('os', n.id, 'idle', 'Checking…');
      try {
        await api.post(`/api/n/${n.id}/os/check`);
        let st;
        await pollUntil(async () => { st = await api.get(`/api/n/${n.id}/os`); return !st.check?.checking; }, 2000, 5 * 60000);
        const c = st?.check || {};
        if (c.error) set('os', n.id, 'crit', 'Check failed', c.error);
        else if (c.available) set('os', n.id, 'warn', `${c.version || 'Newer image'} available`);
        else set('os', n.id, 'good', 'Up to date');
      } catch (e) { set('os', n.id, 'crit', e.message); }
    }));
    progress.busy.os = false;
    refreshNodes();
  } }, icon('refresh', 14), 'Check all nodes');

  const osUpdate = running || job ? h('a.btn.small', { href: '#/update', class: running ? 'primary' : '' }, running ? 'View progress' : 'Last update')
    : null;
  const osStart = !running && osCandidates.length ? h('button.btn.small.primary', { onclick: () => updateDialog(osCandidates) },
    `Update… (${osCandidates.length})`) : null;

  const done = (job?.nodes || []).filter((n) => ['done', 'uptodate', 'failed'].includes(n.step)).length;
  const osSummary = running ? h('span.status.warn', `Updating: ${done} of ${job.nodes.length} node${job.nodes.length > 1 ? 's' : ''} finished`)
    : !osImage.length ? h('span.status.idle', 'No image-based nodes online')
    : osUpdating.length ? h('span.status.warn', `Updating ${osUpdating.map((n) => n.name).join(', ')}…`)
    : osCandidates.length ? h('span.status.warn', `${osCandidates.length} of ${osImage.length} node${osImage.length > 1 ? 's' : ''} can update`)
    : h('span.status.good', 'All nodes up to date');

  // Containers
  const ctNodes = online.filter((n) => n.summary.containerUpdates > 0);
  const ctTotal = ctNodes.reduce((a, n) => a + n.summary.containerUpdates, 0);

  const ctCheck = h('button.btn.small', { disabled: progress.busy.ct, onclick: async () => {
    progress.busy.ct = true; progress.ct.clear();
    await Promise.all(online.map(async (n) => {
      set('ct', n.id, 'idle', 'Checking registries…');
      try {
        await api.post(`/api/n/${n.id}/docker/updates/check`);
        let st;
        await sleep(1000);
        await pollUntil(async () => { st = await api.get(`/api/n/${n.id}/docker/updates`); return !st.checking; }, 2000, 10 * 60000);
        const k = (st?.images || []).filter((i) => i.state === 'update').length;
        set('ct', n.id, k ? 'warn' : 'good', k ? `${k} update${k > 1 ? 's' : ''} available` : 'All images up to date');
      } catch (e) { set('ct', n.id, 'crit', e.message); }
    }));
    progress.busy.ct = false;
    refreshNodes();
  } }, icon('refresh', 14), 'Check all nodes');

  const ctUpdate = ctNodes.length ? h('button.btn.small.primary', { disabled: progress.busy.ct, onclick: async () => {
    if (!await confirmAction('Update containers on all nodes',
      `Pull the newer images and recreate what uses them on ${ctNodes.map((n) => `${n.name} (${n.summary.containerUpdates})`).join(', ')}? Containers with unchanged images aren't touched; data is kept.`,
      `Update ${ctTotal} image${ctTotal > 1 ? 's' : ''}`, false)) return;
    progress.busy.ct = true; progress.ct.clear();
    await Promise.all(ctNodes.map(async (n) => {
      set('ct', n.id, 'idle', 'Updating…');
      try {
        const r = await api.post(`/api/n/${n.id}/docker/updates/apply-all`);
        set('ct', n.id, r.failed ? 'crit' : 'good', r.failed ? `${r.updated} updated, ${r.failed} failed` : r.updated ? `${r.updated} updated` : 'Already up to date', r.output);
      } catch (e) { set('ct', n.id, 'crit', e.message); }
    }));
    progress.busy.ct = false;
    refreshNodes();
  } }, `Update all (${ctTotal})`) : null;

  const ctSummary = ctTotal ? h('span.status.warn', `${ctTotal} image update${ctTotal > 1 ? 's' : ''} on ${ctNodes.length} of ${online.length} node${online.length > 1 ? 's' : ''}`)
    : h('span.status.good', 'All containers up to date');

  return h('div.grid.two',
    h('div.card.update-card',
      h('div.card-head', h('h2', 'OS updates'), h('div.row', osCheck, osUpdate, osStart)),
      osSummary,
      h('p.faint', { style: { margin: '6px 0 0', fontSize: '12.5px' } }, 'Downloads on all nodes at once, then reboots them one at a time. The main node goes last.'),
      progressList(progress.os, byId)),
    h('div.card.update-card',
      h('div.card-head', h('h2', 'Container updates'), h('div.row', ctCheck, ctUpdate)),
      ctSummary,
      h('p.faint', { style: { margin: '6px 0 0', fontSize: '12.5px' } }, 'Pulls newer images and recreates only what uses them. Data in volumes and bind mounts is kept.'),
      progressList(progress.ct, byId)));
}

export function renderFleet(el, { nodes, rollout, refreshNodes, menuButton }) {
  const grid = h('div.grid.nodes');
  const panel = h('div');
  const sub = h('div.sub');
  const draw = () => {
    const list = nodes();
    const offline = list.filter((n) => !n.online).length;
    sub.textContent = `${list.length} node${list.length === 1 ? '' : 's'}` + (offline ? ` · ${offline} offline` : '');
    clear(panel, list.length ? updatesPanel(list, rollout(), refreshNodes, draw) : null);
    clear(grid, list.length ? list.map((n) => nodeCard(n, refreshNodes)) : h('div.card.empty', 'Loading…'));
  };
  clear(el,
    h('div.page-head',
      h('div.row', menuButton(), h('div', h('h1', 'All nodes'), sub)),
      h('div.row',
        h('button.btn', { onclick: refreshNodes, title: 'Refresh' }, icon('refresh', 15), 'Refresh'),
        h('button.btn.primary', { onclick: () => addNodeDialog(refreshNodes) }, icon('plus', 15), 'Add node'))),
    h('div.stack', panel, grid));
  draw();
  const onNodes = () => { if (!menuOpen()) draw(); };
  document.addEventListener('nodes', onNodes);
  return () => document.removeEventListener('nodes', onNodes);
}
