import { h, clear, busy, confirmAction, toast, modal, menu, modalOpen, menuOpen, when } from '../ui.js';
import * as api from '../api.js';
import { shareDialog } from './storage.js';

// Settings for the whole fleet, not one node: the virtual IP and its members, shares mounted
// on several nodes, and apps that run wherever the virtual IP is.

const SHARE_STATES = {
  mounted: ['good', 'mounted'], waiting: ['idle', 'mounts on use'], mounting: ['warn', 'mounting'],
  failed: ['crit', 'failed'], off: ['idle', 'not mounted'], offline: ['crit', 'offline'],
};
const APP_STATES = {
  running: ['good', 'running'], starting: ['warn', 'starting'], stopping: ['warn', 'stopping'],
  waiting: ['warn', 'waiting'], failed: ['crit', 'failed'], standby: ['idle', 'standby'], missing: ['crit', 'no copy'],
};
const source = (m) => m.type === 'smb' ? `//${m.server}/${m.path}` : `${m.server.includes(':') ? `[${m.server}]` : m.server}:${m.path}`;

function ago(unix) {
  if (!unix) return 'never';
  const d = Date.now() / 1000 - unix;
  if (d < 90) return `${Math.max(0, Math.round(d))} s ago`;
  if (d < 3600) return `${Math.round(d / 60)} min ago`;
  if (d < 172800) return `${Math.round(d / 3600)} h ago`;
  return `${Math.round(d / 86400)} days ago`;
}

const minutes = (sec) => sec < 60 ? `${sec} s` : sec % 3600 === 0 && sec >= 3600 ? `${sec / 3600} h` : `${Math.round(sec / 60)} min`;

const MODES = [['shared', 'Shared'], ['replicated', 'Replicated'], ['synced', 'Synced']];
const copies = (mode) => mode === 'replicated' || mode === 'synced';
// How often, per mode, in seconds.
const INTERVALS = {
  replicated: [[300, '5 minutes'], [900, '15 minutes'], [1800, '30 minutes'], [3600, '1 hour'], [21600, '6 hours'], [86400, '1 day']],
  synced: [[10, '10 seconds'], [30, '30 seconds'], [60, '1 minute'], [300, '5 minutes'], [900, '15 minutes']],
};
const DEFAULT_INTERVAL = { replicated: 900, synced: 30 };

// Fills the interval list for a mode, keeping the value when it fits.
function setIntervals(sel, mode, value) {
  const list = INTERVALS[mode] || [];
  clear(sel, list.map(([v, l]) => h('option', { value: v }, l)));
  sel.value = String(list.some(([v]) => v === value) ? value : DEFAULT_INTERVAL[mode] || '');
}

// The dialog that makes a stack highly available, from the Cluster page or a node's Docker tab.
export async function makeHADialog({ stack = '', node = '', onDone } = {}) {
  let cl;
  try { cl = await api.get('/api/cluster'); } catch (e) { toast(e.message, true); return; }
  if (!cl.state.vip.enabled) { toast('Set up the virtual IP first (Cluster → Virtual IP).', true); return; }
  if (!cl.holder) { toast('No member holds the virtual IP right now.', true); return; }
  const holderName = cl.members.find((m) => m.nodeId === cl.holder)?.name || cl.holder;
  if (node && node !== cl.holder) {
    toast(`${holderName} holds the virtual IP; an HA app starts from the stack there. Move the stack there first, or move the virtual IP.`, true);
    return;
  }
  const taken = new Set(cl.state.apps.map((a) => a.name));
  const pick = h('select', { 'aria-label': 'Stack' }, h('option', { value: '' }, 'Loading…'));
  const data = h('div.seg', { role: 'tablist', 'aria-label': 'Data' });
  let mode = 'shared';
  const interval = h('select', { 'aria-label': 'How often' });
  const vipOnly = h('input', { type: 'checkbox' });
  const ivLabel = h('span', 'Copy every');
  const ivField = h('label.field', ivLabel, interval);
  const report = h('div.stack');
  const err = h('div.error-text');
  let check = null;

  const drawMode = () => {
    const sharedOK = check && check.check.problems.length === 0;
    clear(data, MODES.map(([v, l]) => h('button.seg-btn', { type: 'button', role: 'tab',
      class: v === mode ? 'active' : '', 'aria-selected': String(v === mode), disabled: v === 'shared' && check && !sharedOK,
      title: v === 'shared' && check && !sharedOK ? `Not all of its data is on cluster shares:\n${check.check.problems.join('\n')}` : null,
      onclick: () => { mode = v; drawMode(); drawReport(); } }, l)));
    if (copies(mode)) setIntervals(interval, mode, Number(interval.value));
    ivLabel.textContent = mode === 'synced' ? 'Send changes every' : 'Copy every';
    ivField.classList.toggle('hidden', !copies(mode));
  };
  const drawReport = () => {
    if (!check) { clear(report); return; }
    const c = check.check;
    clear(report,
      mode === 'shared'
        ? (c.problems.length
          ? h('div.notice.warn', h('strong', 'Not all of its data is on cluster shares on every member:'), h('ul', c.problems.map((p) => h('li', p))),
            'Choose Synced to keep that data on each node\'s own disk, the same on every member, or move it to a cluster share.')
          : h('div.notice', `All of its data is on ${c.shares.length ? `the cluster share${c.shares.length > 1 ? 's' : ''} ${c.shares.join(', ')}` : 'cluster shares'}. A failover starts it on the next member at once.`))
        : mode === 'synced'
          ? h('div.notice', 'Every ', h('strong', minutes(Number(interval.value))), ', the files of its folder and named volumes that changed are sent to ',
            check.members.filter((n) => n !== check.holderName).join(', ') || 'the other members',
            '. Only changes travel, so it suits bigger apps too; the first sync sends everything. A failover can lose up to that much. ',
            'The app is paused for a moment while small changes are sent.',
            c.warnings.length ? h('ul', c.warnings.map((w) => h('li', w))) : null)
          : h('div.notice', 'Its whole folder and named volumes are copied to ', check.members.filter((n) => n !== check.holderName).join(', ') || 'the other members',
            ' every ', h('strong', minutes(Number(interval.value))), '. A failover can lose up to that much. The app is paused while each copy is taken, so keep big data on a cluster share.',
            c.warnings.length ? h('ul', c.warnings.map((w) => h('li', w))) : null),
      mode !== 'shared' && c.problems.length ? h('details',
        h('summary.muted', 'Why not shared?'),
        h('ul', c.problems.map((p) => h('li', p))),
        h('p.faint', { style: { margin: 0 } }, 'Shared needs every bind on a NAS share (/var/mnt/<share>/…). To keep that data on the nodes\' own disks instead, choose Synced: the same folder on every member, kept in step.')) : null,
      check.ports.length ? h('p.faint', { style: { margin: 0 } }, `Published ports: ${check.ports.join(', ')}.`) : null);
  };
  interval.addEventListener('change', drawReport);

  const runCheck = async () => {
    check = null; err.textContent = ''; clear(report, h('div.faint', 'Checking…'));
    if (!pick.value) { clear(report); drawMode(); return; }
    try {
      check = await api.post('/api/cluster/apps/check', { name: pick.value, node: cl.holder });
      if (check.check.problems.length && mode === 'shared') mode = 'synced';
    } catch (e) { err.textContent = e.message; clear(report); }
    drawMode(); drawReport();
  };
  pick.addEventListener('change', runCheck);

  api.get(`/api/n/${encodeURIComponent(cl.holder)}/docker/stacks`).then((r) => {
    const list = (r.stacks || []).filter((s) => s.inRoot && !taken.has(s.name));
    clear(pick, h('option', { value: '' }, list.length ? 'Pick a stack…' : 'No stacks left to pick'), list.map((s) => h('option', { value: s.name }, s.name)));
    if (stack) { pick.value = stack; runCheck(); }
  }).catch((e) => { err.textContent = e.message; });
  drawMode();

  const body = h('div.stack', { 'data-wide': '1' },
    h('p.muted', { style: { margin: 0 } }, `It runs on whichever member holds the virtual IP, now ${holderName}. If that node goes down, the next member starts it.`),
    h('label.field', `Stack on ${holderName}`, pick),
    h('div.field', h('span', 'Its data'), data),
    ivField,
    report,
    h('label.check-row', vipOnly, h('span', h('strong', 'Ports only on the virtual IP'),
      h('span.muted', ` · ${cl.state.vip.address}:port reaches it; the nodes' own addresses don't. Nothing is blocked by a firewall: the ports just aren't published there.`))),
    err);
  const ok = await modal('Make an app highly available', body, [
    { label: 'Cancel', value: null },
    { label: 'Make it highly available', class: 'primary', onclick: async () => {
      err.textContent = '';
      if (!pick.value) { err.textContent = 'Pick a stack.'; return false; }
      try {
        const r = await api.post('/api/cluster/apps', { name: pick.value, node: cl.holder, data: mode, interval: Number(interval.value), vipOnly: vipOnly.checked });
        toast(`${pick.value} is highly available. ${r.output || ''}`);
        return true;
      } catch (e) { err.textContent = e.message; return false; }
    } }]);
  if (ok) onDone?.();
}

export function renderCluster(el, { menuButton }) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  clear(el,
    h('div.page-head', h('div.row', menuButton(), h('div', h('h1', 'Cluster'),
      h('div.sub', 'Settings for all your nodes together: one address that always reaches the dashboard and your apps, shares on every node, and apps that move when a node goes down.')))),
    content);
  let timer, stopped = false, cl = null;

  async function load() {
    clearTimeout(timer);
    try {
      const r = await api.get('/api/cluster');
      if (stopped) return;
      cl = r;
      if (!modalOpen() && !menuOpen()) draw();
    } catch (e) { clear(content, h('div.notice.warn', e.message)); }
    if (!stopped) timer = setTimeout(load, 10000);
  }

  const nodeName = (id) => cl.nodes.find((n) => n.id === id)?.name || cl.members.find((m) => m.nodeId === id)?.name || id;
  const ro = () => cl.readOnly;

  // ---- Virtual IP ----

  async function vipDialog() {
    const v = cl.state.vip;
    const on = h('input', { type: 'checkbox', checked: v.enabled || !v.address });
    const addr = h('input', { type: 'text', value: v.address || '', placeholder: '192.168.1.50', spellcheck: false, style: { width: '170px' } });
    const prefix = h('input', { type: 'number', min: 8, max: 30, value: v.prefix || 24, style: { width: '80px' } });
    const failback = h('input', { type: 'checkbox', checked: v.address ? !!v.failback : cl.state.apps.length === 0 });
    let members = (v.members || []).map((m) => m.nodeId);
    if (!members.length) members = ['local'];
    const list = h('ol.member-list');
    const add = h('select', { 'aria-label': 'Add a member' });
    const drawList = () => {
      clear(list, members.map((id, i) => h('li',
        h('span.member-name', nodeName(id), id === 'local' ? h('span.badge', { style: { marginLeft: '6px' } }, 'main node') : null),
        h('span.faint', `priority ${Math.max(150 - 10 * i, 1)}`),
        h('span.spacer'),
        h('button.btn.small.ghost', { type: 'button', disabled: i === 0, 'aria-label': `Move ${nodeName(id)} up`, onclick: () => { [members[i - 1], members[i]] = [members[i], members[i - 1]]; drawList(); } }, '↑'),
        h('button.btn.small.ghost', { type: 'button', disabled: i === members.length - 1, 'aria-label': `Move ${nodeName(id)} down`, onclick: () => { [members[i + 1], members[i]] = [members[i], members[i + 1]]; drawList(); } }, '↓'),
        h('button.btn.small.ghost', { type: 'button', 'aria-label': `Remove ${nodeName(id)}`, onclick: () => { members.splice(i, 1); drawList(); } }, '✕'))));
      const rest = cl.nodes.filter((n) => !members.includes(n.id));
      clear(add, h('option', { value: '' }, rest.length ? 'Add a member…' : 'Every node is a member'), rest.map((n) => h('option', { value: n.id }, n.name)));
      add.disabled = !rest.length;
    };
    add.addEventListener('change', () => { if (add.value) { members.push(add.value); drawList(); } });
    drawList();
    const err = h('div.error-text');
    const body = h('div.stack', { 'data-wide': '1' },
      h('label.check-row', on, h('span', h('strong', 'Use a virtual IP'),
        h('div.faint', { style: { fontSize: '12.5px' } }, 'An extra address on your LAN. Pick a free one in the same subnet as every member.'))),
      h('div.form-grid', h('label.field', 'Address', addr), h('label.field', 'Prefix', prefix)),
      h('div.field', h('span', 'Members, highest priority first'), list, add),
      h('p.faint', { style: { margin: 0 } }, 'The first member that\'s up holds the address. The main node doesn\'t have to be one: then the address, the dashboard on it and the HA apps live only on the other members.'),
      h('label.check-row', failback, h('span', h('strong', 'Move back to the first member by itself'),
        h('div.faint', { style: { fontSize: '12.5px' } }, cl.state.apps.length
          ? 'Leave this off with HA apps: moving back on its own would restart a replicated app on an out-of-date copy. Use "Move virtual IP here" instead.'
          : 'Off: the address stays where it is until you move it ("Move virtual IP here"). Turn it off before adding HA apps.'))),
      err);
    const r = await modal(v.address ? 'Virtual IP' : 'Set up the virtual IP', body, [
      { label: 'Cancel', value: null },
      { label: 'Save', class: 'primary', onclick: async () => {
        err.textContent = '';
        const b = { enabled: on.checked, address: addr.value.trim(), prefix: Number(prefix.value), members, failback: failback.checked };
        if (b.enabled && (!b.address || members.length < 2)) { err.textContent = 'Enter an address and pick at least two members.'; return false; }
        try { return await api.post('/api/cluster/vip', b); } catch (e) { err.textContent = e.message; return false; }
      } }]);
    if (!r) return;
    toast(on.checked ? 'Virtual IP set up. The dashboard\'s certificate may have changed: browsers and MCP clients warn once more.' : 'Virtual IP turned off.');
    if (r.output === 'restart') setTimeout(() => location.reload(), 5000);
    else setTimeout(load, 1500);
  }

  async function moveVIP(m) {
    const replicated = cl.state.apps.filter((a) => copies(a.data)).map((a) => a.name);
    if (!await confirmAction(`Move the virtual IP to ${m.name}`,
      `The HA apps stop where they run now${replicated.length ? `, ${replicated.join(', ')} send${replicated.length === 1 ? 's' : ''} a final copy to ${m.name}` : ''}, and they start on ${m.name} once it holds the address. `
      + 'The dashboard on the virtual IP is away for a few seconds.', 'Move it', false)) return;
    toast(`Moving the virtual IP to ${m.name}…`);
    try { const r = await api.post('/api/cluster/switchover', { to: m.nodeId }); toast(r.output || 'Moved.'); }
    catch (e) { toast(e.message, true); }
    load();
  }

  function vipCard() {
    const v = cl.state.vip;
    const holder = cl.members.find((m) => m.nodeId === cl.holder);
    const status = !v.address ? h('span.badge', 'not set up') : !v.enabled ? h('span.badge', 'off')
      : h('span.status', { class: holder ? 'good' : 'crit' }, holder ? `held by ${holder.name}` : 'nobody holds it');
    const rows = cl.members.map((m) => {
      const vs = m.vip;
      let st;
      if (!m.online) st = h('span.status.crit', { title: m.error || '' }, 'offline');
      else if (vs?.holds) st = h('span.status.good', 'holds it');
      else if (vs?.releasing) st = h('span.status.warn', 'giving it up');
      else if (vs && !vs.active) st = h('span.status.crit', 'keepalived isn\'t running');
      else if (vs?.error) st = h('span.status.warn', { title: vs.error }, 'problem');
      else st = h('span.status.idle', 'standby');
      const copied = m.nodeId === 'local' ? h('span.faint', 'main node') : !m.sync ? h('span.faint', '—')
        : m.sync.error ? h('span.error-text', { title: m.sync.error }, 'copy failed') : h('span.faint', ago(m.sync.at));
      return h('tr',
        h('td', h('strong', m.name), m.nodeId === 'local' ? h('span.badge', { style: { marginLeft: '6px' } }, 'main node') : null,
          vs?.error && m.online ? h('div.sub.error-text', vs.error) : null),
        h('td.num-cell', m.priority),
        h('td', st),
        h('td', copied),
        h('td.actions', v.enabled && !vs?.holds && m.online && !ro()
          ? h('button.btn.small', { onclick: () => moveVIP(m), title: `Move the virtual IP and the HA apps to ${m.name}` }, 'Move virtual IP here') : null));
    });
    return h('div.card',
      h('div.card-head', h('h2', 'Virtual IP'), status, h('span.spacer'),
        h('button.btn', { class: v.address ? '' : 'primary', disabled: ro(), onclick: vipDialog }, v.address ? 'Change…' : 'Set up')),
      v.address ? h('dl.kv', { style: { marginBottom: '14px' } },
        h('dt', 'Address'), h('dd.mono', `${v.address}/${v.prefix}`),
        h('dt', 'Failback'), h('dd', v.failback ? 'Moves back to the first member by itself' : 'Stays where it is until moved')) : null,
      cl.members.length ? h('div.table-wrap', h('table',
        h('thead', h('tr', h('th', 'Member'), h('th', 'Priority'), h('th', 'Status'), h('th', 'Copy of the main node\'s state'), h('th', ''))),
        h('tbody', rows))) : null,
      h('p.faint', { style: { marginBottom: 0 } },
        'One address on your LAN for the dashboard and the HA apps. If the member holding it, or its dashboard, stops, the next member takes it within a few seconds ',
        'and serves the dashboard there with the paired nodes, API tokens and cluster settings it was last sent. Changes made there aren\'t kept.'));
  }

  // ---- Shared storage ----

  function nodesPicker(sel = []) {
    const all = h('input', { type: 'radio', name: 'share-nodes', checked: !sel.length });
    const some = h('input', { type: 'radio', name: 'share-nodes', checked: !!sel.length });
    const boxes = cl.nodes.map((n) => h('label.check-row', h('input', { type: 'checkbox', value: n.id, checked: sel.includes(n.id) }), h('span', n.name)));
    const list = h('div.share-node-list', { class: sel.length ? '' : 'hidden' }, boxes);
    const sync = () => list.classList.toggle('hidden', !some.checked);
    all.addEventListener('change', sync);
    some.addEventListener('change', sync);
    return {
      el: h('div.field', h('span', 'Mount it on'),
        h('label.check-row', all, h('span', h('strong', 'Every node'), h('span.muted', ' · nodes paired later get it too'))),
        h('label.check-row', some, h('span', h('strong', 'Chosen nodes'))), list),
      body: () => ({ nodes: some.checked ? boxes.map((b) => b.querySelector('input')).filter((i) => i.checked).map((i) => i.value) : [] }),
    };
  }

  async function shareEdit(sh) {
    const picker = nodesPicker(sh?.nodes || []);
    let people = [];
    try { people = (await api.get('/api/n/local/users')).users || []; } catch { /* the main node's users only help pick an owner */ }
    const v = await shareDialog({ m: sh, title: sh ? `Edit cluster share ${sh.name}` : 'Add cluster share', people,
      exports: (server) => api.get(`/api/n/local/storage/exports?server=${encodeURIComponent(server)}`),
      test: (b) => api.post('/api/n/local/storage/test-mount', b),
      saveLabel: sh ? 'Save' : 'Add and mount everywhere',
      extra: picker,
      save: async (b) => {
        if (b.nodes && b.nodes.length === 0 && picker.el.querySelectorAll('input[type=radio]')[1].checked) throw new Error('Pick at least one node.');
        const r = await api.post(sh ? `/api/cluster/shares/${encodeURIComponent(sh.name)}` : '/api/cluster/shares', b);
        const bad = (r.nodes || []).filter((n) => n.state === 'failed' || n.state === 'offline');
        toast(bad.length ? `${b.name} saved; it couldn't be mounted on ${bad.map((n) => n.name).join(', ')} yet.` : `${b.name} is mounted on ${(r.nodes || []).length} node(s).`, bad.length > 0);
      } });
    if (v) load();
  }

  async function shareRemove(sh) {
    if (!await confirmAction(`Remove ${sh.name}`, `/var/mnt/${sh.name} is unmounted on every node and the share is deleted from the cluster, with its stored password. The files on ${sh.server} aren't touched. Containers that use the folder see an empty folder afterwards.`, 'Remove')) return;
    try { await api.del(`/api/cluster/shares/${encodeURIComponent(sh.name)}`); toast(`${sh.name} removed.`); } catch (e) { toast(e.message, true); }
    load();
  }

  function sharesCard() {
    const rows = cl.state.shares.map((sh) => {
      const per = cl.shares[sh.name] || [];
      return h('tr',
        h('td', h('strong', sh.name), h('div.sub.mono', `/var/mnt/${sh.name}`)),
        h('td', h('span.mono', source(sh)), h('div.sub', [sh.type.toUpperCase(), sh.readOnly ? 'read-only' : null, sh.dockerWaits ? 'Docker waits' : null].filter(Boolean).join(' · '))),
        h('td', sh.nodes.length ? sh.nodes.map(nodeName).join(', ') : 'every node'),
        h('td', per.length ? h('div.node-chips', per.map((n) => {
          const [cls, label] = SHARE_STATES[n.state] || ['idle', n.state || 'unknown'];
          return h('span.status', { class: cls, title: n.error || '' }, `${n.name}: ${label}`);
        })) : h('span.faint', 'checking…'),
        per.filter((n) => n.error).map((n) => h('div.sub.error-text', { style: { whiteSpace: 'pre-wrap' } }, `${n.name}: ${n.error}`))),
        h('td.actions', menu([
          { label: 'Edit…', disabled: ro(), onclick: () => shareEdit(sh) },
          { label: 'Try again on every node', disabled: ro(), onclick: async () => {
            try { await api.post(`/api/cluster/shares/${encodeURIComponent(sh.name)}/retry`); toast('Checked every node.'); } catch (e) { toast(e.message, true); }
            load();
          } },
          { label: 'Copy compose line', onclick: async () => {
            try { await navigator.clipboard.writeText(`- /var/mnt/${sh.name}:/${sh.name}`); toast('Copied.'); } catch { toast('Couldn\'t copy', true); }
          } },
          { label: 'Remove…', danger: true, disabled: ro(), onclick: () => shareRemove(sh) },
        ], `Actions for ${sh.name}`)));
    });
    return h('div.card',
      h('div.card-head', h('h2', 'Shared storage'), h('span.spacer'), h('button.btn.primary', { disabled: ro(), onclick: () => shareEdit(null) }, 'Add share')),
      h('p.muted', { style: { marginTop: 0 } }, 'An NFS or SMB share mounted at the same ', h('code', '/var/mnt/<name>'),
        ' on every node, or on the ones you pick. A compose file that uses it, like ', h('code', '- /var/mnt/media:/media'),
        ', then works on any of them, so an app can move or fail over without its data. Each node tests the share before mounting it, and a node that\'s off gets it when it\'s back.'),
      rows.length ? h('div.table-wrap', h('table',
        h('thead', h('tr', h('th', 'Name'), h('th', 'Share'), h('th', 'Nodes'), h('th', 'Status'), h('th', ''))),
        h('tbody', rows))) : h('div.empty', 'No cluster shares yet.'));
  }

  // ---- HA apps ----

  async function appEdit(a) {
    let mode = a.data;
    const seg = h('div.seg', { role: 'tablist' });
    const interval = h('select', { 'aria-label': 'How often' });
    let wanted = a.interval;
    interval.addEventListener('change', () => { wanted = Number(interval.value); });
    const ivLabel = h('span', 'Copy every');
    const ivField = h('label.field', ivLabel, interval);
    const drawSeg = () => {
      clear(seg, MODES.map(([v, l]) => h('button.seg-btn', { type: 'button', class: v === mode ? 'active' : '',
        'aria-selected': String(v === mode), onclick: () => { mode = v; drawSeg(); } }, l)));
      if (copies(mode)) setIntervals(interval, mode, wanted);
      ivLabel.textContent = mode === 'synced' ? 'Send changes every' : 'Copy every';
      ivField.classList.toggle('hidden', !copies(mode));
    };
    drawSeg();
    const vipOnly = h('input', { type: 'checkbox', checked: a.vipOnly });
    const err = h('div.error-text');
    const ok = await modal(`High-availability app ${a.name}`, h('div.stack',
      h('div.field', h('span', 'Its data'), seg), ivField,
      h('label.check-row', vipOnly, h('span', h('strong', 'Ports only on the virtual IP'))),
      h('p.faint', { style: { margin: 0 } }, 'Saving copies its compose files from the node that runs it to the other members again, and recreates it there.'),
      err), [
      { label: 'Cancel', value: null },
      { label: 'Save', class: 'primary', onclick: async () => {
        try { await api.post(`/api/cluster/apps/${encodeURIComponent(a.name)}`, { data: mode, interval: Number(interval.value), vipOnly: vipOnly.checked }); return true; }
        catch (e) { err.textContent = e.message; return false; }
      } }]);
    if (ok) { toast(`${a.name} saved.`); load(); }
  }

  async function appRemove(a) {
    if (!await confirmAction(`Make ${a.name} an ordinary app`, `It keeps running where it is now, and doesn't move when that node goes down. The copies on the other members stay, stopped; remove them there when you don't need them.`, 'Make ordinary')) return;
    try { const r = await api.del(`/api/cluster/apps/${encodeURIComponent(a.name)}`); toast(r.output || 'Done.'); } catch (e) { toast(e.message, true); }
    load();
  }

  function appsCard() {
    const rows = cl.state.apps.map((a) => {
      const per = cl.members.map((m) => ({ m, s: m.local?.apps?.find((x) => x.name === a.name) }));
      const last = per.filter((p) => p.s?.activeFrom).sort((x, y) => y.s.activeFrom - x.s.activeFrom)[0];
      const lock = per.find((p) => p.s?.lock)?.s.lock;
      return h('tr',
        h('td', h('strong', a.name), h('div.sub', `by ${a.owner}`)),
        h('td', a.data === 'shared' ? h('span', 'Shared', h('div.sub', a.shares?.length ? `on ${a.shares.join(', ')}` : ''))
          : h('span', a.data === 'synced' ? 'Synced' : 'Replicated', h('div.sub', `every ${minutes(a.interval)}`))),
        h('td', a.vipOnly ? h('span.badge', 'virtual IP only') : h('span.faint', 'every address')),
        h('td', h('div.node-chips', per.map(({ m, s }) => {
          if (!m.online || !s) return h('span.status.crit', { title: m.error || '' }, `${m.name}: ${m.online ? 'unknown' : 'offline'}`);
          const [cls, label] = APP_STATES[s.state] || ['idle', s.state];
          const copy = copies(a.data) && !s.running && s.replicaAt ? ` · ${a.data === 'synced' ? 'synced' : 'copy'} ${ago(s.replicaAt)}` : '';
          return h('span.status', { class: cls, title: [s.detail, s.lock].filter(Boolean).join('\n') }, `${m.name}: ${label}${copy}`);
        })),
        per.map(({ s }) => s?.detail ? h('div.sub', s.detail) : null),
        lock ? h('div.sub', `Lock: ${lock}`) : null,
        per.flatMap(({ s }) => (s?.pushes || []).filter((p) => !p.ok).map((p) => h('div.sub.error-text', `Copy to ${p.name} failed: ${p.error}`)))),
        h('td', last ? h('span', { title: when(last.s.activeFrom) }, `${last.m.name}, ${ago(last.s.activeFrom)}`) : h('span.faint', '—')),
        h('td.actions', menu([
          { label: 'Change…', disabled: ro(), onclick: () => appEdit(a) },
          { label: a.data === 'synced' ? 'Send the changes now' : 'Copy to the other members now', hidden: !copies(a.data), onclick: async () => {
            toast(`Copying ${a.name}…`);
            try { await api.post(`/api/cluster/apps/${encodeURIComponent(a.name)}/replicate`); toast(`${a.name} copied.`); } catch (e) { toast(e.message, true); }
            load();
          } },
          { label: 'Copy its compose files to every member', disabled: ro(), onclick: async () => {
            try { const r = await api.post(`/api/cluster/apps/${encodeURIComponent(a.name)}/sync`); toast(r.output || 'Copied.'); } catch (e) { toast(e.message, true); }
          } },
          { label: 'Open on the node that runs it', hidden: !cl.holder, onclick: () => { location.hash = `#/n/${encodeURIComponent(cl.holder)}/docker`; } },
          { label: 'Make ordinary…', danger: true, disabled: ro(), onclick: () => appRemove(a) },
        ], `Actions for ${a.name}`)));
    });
    const can = cl.state.vip.enabled && !ro();
    return h('div.card',
      h('div.card-head', h('h2', 'High-availability apps'), h('span.spacer'),
        h('button.btn.primary', { disabled: !can, title: can ? '' : 'Set up the virtual IP first', onclick: () => makeHADialog({ onDone: load }) }, 'Make an app highly available')),
      h('p.muted', { style: { marginTop: 0 } }, 'Apps that run on whichever member holds the virtual IP. If that node goes offline, the next member starts them. ',
        h('strong', 'Shared'), ': all their data is on cluster shares, so the next member starts at once. ',
        h('strong', 'Synced'), ': the files that changed are sent to the other members every few seconds. ',
        h('strong', 'Replicated'), ': their whole folder and volumes are copied on a timer. With both, a failover can lose up to one interval of changes. ',
        'Reach them on the virtual IP, like ', h('code', `${cl.state.vip.address || '192.168.1.50'}:8096`), '.'),
      cl.state.apps.length ? h('div.table-wrap', h('table',
        h('thead', h('tr', h('th', 'App'), h('th', 'Data'), h('th', 'Ports'), h('th', 'On each member'), h('th', 'Last started'), h('th', ''))),
        h('tbody', rows))) : h('div.empty', cl.state.vip.enabled ? 'No HA apps yet.' : 'Set up the virtual IP first.'));
  }

  function draw() {
    clear(content,
      ro() ? h('div.notice.warn', 'This node is standing in for the main node. You can look at the cluster here; change it on the main node.') : null,
      vipCard(), sharesCard(), appsCard());
  }

  load();
  return () => { stopped = true; clearTimeout(timer); };
}
