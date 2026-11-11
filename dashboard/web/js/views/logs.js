import { h, clear } from '../ui.js';
import { logView } from '../logview.js';

const QUICK = [
  ['', 'Everything'], ['kernel', 'Kernel'], ['dashboard.service', 'Dashboard'], ['autoupdate.service', 'Automatic updates'],
  ['docker.service', 'Docker'], ['netbird.service', 'NetBird'], ['sshd.service', 'SSH'], ['smartd.service', 'Disk health (smartd)'],
];

// The systemd journal: pick a source, filter, and follow it live.
export function renderLogs(el, { api, node, query }) {
  let stopped = false;
  const unitSel = h('select.log-source', { 'aria-label': 'Source' },
    h('optgroup', { label: 'Common' }, QUICK.map(([v, l]) => h('option', { value: v }, l))));
  const prio = h('select.log-select', { 'aria-label': 'Priority' },
    h('option', { value: '7' }, 'All priorities'), h('option', { value: '6' }, 'Info and up'),
    h('option', { value: '4' }, 'Warnings and up'), h('option', { value: '3' }, 'Errors only'));
  const since = h('select.log-select', { 'aria-label': 'Since' },
    [['boot', 'This boot'], ['15m', 'Last 15 min'], ['1h', 'Last hour'], ['24h', 'Last 24 h'], ['7d', 'Last 7 days'], ['', 'All']].map(([v, l]) => h('option', { value: v }, l)));

  const want = query?.get('unit');
  if (want && !QUICK.some(([v]) => v === want)) unitSel.append(h('option', { value: want }, want));
  unitSel.value = want || '';
  if (query?.get('priority')) prio.value = query.get('priority');

  const view = logView({
    tools: [unitSel, prio, since],
    serverSearch: true,
    fileName: () => `${node.name}-${unitSel.value || 'journal'}`,
    fetch: async ({ cursor, grep }) => {
      const p = new URLSearchParams();
      if (unitSel.value === 'kernel') p.set('kernel', '1');
      else if (unitSel.value) p.set('unit', unitSel.value);
      p.set('priority', prio.value);
      if (since.value) p.set('since', since.value);
      if (grep) p.set('grep', grep);
      if (cursor) p.set('cursor', cursor);
      p.set('limit', '1000');
      const r = await api.get('/logs?' + p);
      return {
        cursor: r.cursor,
        entries: (r.entries || []).map((e) => ({ time: e.time, src: e.ident || e.unit || '', pid: e.pid, level: e.priority, message: e.message })),
      };
    },
  });
  clear(el, h('div.card', view.el));

  // The rest of the units, for the picker.
  api.get('/logs/units').then((us) => {
    if (stopped || !us?.length) return;
    const known = new Set([...unitSel.options].map((o) => o.value));
    unitSel.append(h('optgroup', { label: 'All services' }, us.filter((u) => !known.has(u.unit))
      .sort((a, b) => a.unit.localeCompare(b.unit))
      .map((u) => h('option', { value: u.unit, title: u.description }, `${u.unit}${u.active === 'failed' ? ' (failed)' : ''}`))));
  }).catch(() => {});

  for (const c of [unitSel, prio, since]) c.addEventListener('change', view.reload);
  view.reload();
  return () => { stopped = true; view.stop(); };
}
