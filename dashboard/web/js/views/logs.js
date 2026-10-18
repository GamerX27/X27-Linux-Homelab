import { h, clear, icon, toast, askAI } from '../ui.js';

const PRIORITY = ['emerg', 'alert', 'crit', 'err', 'warning', 'notice', 'info', 'debug'];
const QUICK = [
  ['', 'Everything'], ['kernel', 'Kernel'], ['dashboard.service', 'Dashboard'], ['autoupdate.service', 'Automatic updates'],
  ['docker.service', 'Docker'], ['netbird.service', 'NetBird'], ['sshd.service', 'SSH'], ['smartd.service', 'Disk health (smartd)'],
];
const MAX_LINES = 5000;

const fmtTime = (ms) => {
  const d = new Date(ms);
  return d.toLocaleString([], { month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' });
};

// The systemd journal: pick a source, filter, and follow it live.
export function renderLogs(el, { api, node, query }) {
  let timer, stopped = false, cursor = '', lines = [], following = false, gen = 0;

  const unitSel = h('select', { 'aria-label': 'Source', style: { width: 'auto', maxWidth: '260px' } },
    h('optgroup', { label: 'Common' }, QUICK.map(([v, l]) => h('option', { value: v }, l))));
  const prio = h('select', { 'aria-label': 'Priority', style: { width: 'auto' } },
    h('option', { value: '7' }, 'All priorities'), h('option', { value: '6' }, 'Info and up'),
    h('option', { value: '4' }, 'Warnings and up'), h('option', { value: '3' }, 'Errors only'));
  const since = h('select', { 'aria-label': 'Since', style: { width: 'auto' } },
    [['boot', 'This boot'], ['15m', 'Last 15 min'], ['1h', 'Last hour'], ['24h', 'Last 24 h'], ['7d', 'Last 7 days'], ['', 'All']].map(([v, l]) => h('option', { value: v }, l)));
  const grep = h('input.filter-input', { type: 'search', placeholder: 'Search…', spellcheck: false });
  const followBtn = h('button.btn', { onclick: () => setFollow(!following) }, 'Follow');
  const dl = h('button.btn', { title: 'Download what\'s shown', onclick: download }, icon('download', 15), 'Download');
  const ai = h('span');
  const syncAI = () => {
    const src = unitSel.value === 'kernel' ? 'the kernel log' : unitSel.value ? `the journal of ${unitSel.value}` : 'the system journal';
    clear(ai, askAI(`Read ${src} on node ${node.name} (since ${since.selectedOptions[0]?.textContent.toLowerCase()}) and explain the errors and warnings: what they mean and whether I need to do something.`,
      `the Logs tab of node ${node.name} (id ${node.id})`));
  };
  const info = h('span.faint');
  const list = h('div.log-view', { role: 'log', 'aria-live': 'off' });

  const want = query?.get('unit');
  if (want && !QUICK.some(([v]) => v === want)) unitSel.append(h('option', { value: want }, want));
  unitSel.value = want || '';

  clear(el, h('div.card',
    h('div.row', { style: { marginBottom: '12px' } }, unitSel, prio, since, grep, h('span.spacer'), info, ai, followBtn, dl),
    list));

  // The rest of the units, for the picker.
  api.get('/logs/units').then((us) => {
    if (stopped || !us?.length) return;
    const known = new Set([...unitSel.options].map((o) => o.value));
    unitSel.append(h('optgroup', { label: 'All services' }, us.filter((u) => !known.has(u.unit))
      .sort((a, b) => a.unit.localeCompare(b.unit))
      .map((u) => h('option', { value: u.unit, title: u.description }, `${u.unit}${u.active === 'failed' ? ' (failed)' : ''}`))));
  }).catch(() => {});

  function params(after) {
    const p = new URLSearchParams();
    if (unitSel.value === 'kernel') p.set('kernel', '1');
    else if (unitSel.value) p.set('unit', unitSel.value);
    p.set('priority', prio.value);
    if (since.value) p.set('since', since.value);
    if (grep.value.trim()) p.set('grep', grep.value.trim());
    if (after) p.set('cursor', after);
    p.set('limit', '1000');
    return p;
  }

  const row = (e) => h('div.log-line', { class: `p${e.priority}` },
    h('span.t', fmtTime(e.time)),
    h('span.src', e.ident || e.unit || ''),
    h('span.msg', e.message));

  function render(entries, append) {
    const atBottom = list.scrollHeight - list.scrollTop - list.clientHeight < 40;
    if (!append) { lines = []; clear(list); }
    lines.push(...entries);
    const frag = document.createDocumentFragment();
    entries.forEach((e) => frag.append(row(e)));
    list.append(frag);
    while (lines.length > MAX_LINES) { lines.shift(); list.firstChild?.remove(); }
    if (!lines.length) clear(list, h('div.empty', 'No log entries match.'));
    info.textContent = `${lines.length} line${lines.length === 1 ? '' : 's'}`;
    if (!append || atBottom) list.scrollTop = list.scrollHeight;
  }

  async function load() {
    const my = ++gen;
    clearTimeout(timer);
    try {
      const r = await api.get('/logs?' + params());
      if (stopped || my !== gen) return;
      cursor = r.cursor;
      render(r.entries, false);
    } catch (e) { if (my === gen) clear(list, h('div.notice.warn', e.message)); }
    if (following) schedule();
  }

  async function poll() {
    const my = gen;
    try {
      const r = await api.get('/logs?' + params(cursor));
      if (stopped || my !== gen) return;
      if (r.cursor) cursor = r.cursor;
      if (r.entries.length) {
        if (list.querySelector('.empty')) clear(list);
        render(r.entries, true);
      }
    } catch { /* keep trying */ }
    if (following && my === gen) schedule();
  }

  function schedule() { clearTimeout(timer); if (!stopped) timer = setTimeout(poll, 2000); }

  function setFollow(on) {
    following = on;
    followBtn.classList.toggle('primary', on);
    followBtn.textContent = on ? 'Following' : 'Follow';
    if (on) { list.scrollTop = list.scrollHeight; schedule(); } else clearTimeout(timer);
  }

  function download() {
    const text = lines.map((e) => `${new Date(e.time).toISOString()} ${e.ident || e.unit || '-'}[${e.pid || ''}] ${PRIORITY[e.priority] || ''}: ${e.message}`).join('\n');
    const a = h('a', { href: URL.createObjectURL(new Blob([text + '\n'], { type: 'text/plain' })),
      download: `${node.name}-${unitSel.value || 'journal'}-${new Date().toISOString().slice(0, 19)}.log` });
    document.body.append(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
    if (!lines.length) toast('Nothing to download.', true);
  }

  let debounce;
  for (const c of [unitSel, prio, since]) c.addEventListener('change', () => { syncAI(); load(); });
  syncAI();
  grep.addEventListener('input', () => { clearTimeout(debounce); debounce = setTimeout(load, 350); });

  load();
  return () => { stopped = true; clearTimeout(timer); clearTimeout(debounce); };
}
