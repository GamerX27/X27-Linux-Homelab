import { h, clear } from '../ui.js';
import * as mainApi from '../api.js';

// Who changed what, and when: every action through the dashboard, an API token or the main
// node, plus logins. This node's, or every node's at once.

const PAGE = 100;

const fmt = (ms) => new Date(ms).toLocaleString([], { month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' });

// "token:claude" -> a badge; web and main node are plain text.
function viaEl(via) {
  if (!via) return h('span.faint', '—');
  if (via.startsWith('token:')) return h('span.badge', { title: 'An API token (MCP)' }, `🔑 ${via.slice(6)}`);
  if (via === 'main node') return h('span.faint', 'main node');
  return h('span.faint', via);
}

export function renderActivity(el, { node }) {
  let stopped = false, entries = [], more = false, loading = false;
  const scope = h('select.log-select', { 'aria-label': 'Nodes' },
    h('option', { value: 'node' }, `This node (${node.name})`), h('option', { value: 'all' }, 'All nodes'));
  const failed = h('label.check-row.muted', { style: { fontSize: '13px', alignItems: 'center' } },
    h('input', { type: 'checkbox', onchange: () => load() }), 'Only failures');
  const user = h('select.log-select', { 'aria-label': 'User' }, h('option', { value: '' }, 'Everyone'));
  const search = h('input.filter-input', { type: 'search', placeholder: 'Search actions…', spellcheck: false, 'aria-label': 'Search' });
  const info = h('span.faint.log-count');
  const body = h('div');
  const users = new Set();
  try { scope.value = localStorage.getItem('activity-scope') || 'node'; } catch {}

  clear(el, h('div.card',
    h('div.row.log-tools', scope, user, search, failed, h('span.spacer'), info),
    body));

  function url(before) {
    const p = new URLSearchParams({ limit: String(PAGE) });
    if (user.value) p.set('user', user.value);
    if (search.value.trim()) p.set('q', search.value.trim());
    if (failed.querySelector('input').checked) p.set('failed', '1');
    if (before) p.set('before', String(before));
    if (scope.value === 'node') p.set('node', node.local ? 'local' : node.id);
    return `/api/activity?${p}`;
  }

  async function load(older = false) {
    if (loading) return;
    loading = true;
    const before = older && entries.length ? entries[entries.length - 1].time : 0;
    try {
      const r = await mainApi.get(url(before));
      if (stopped) return;
      entries = older ? entries.concat(r.entries) : r.entries;
      more = r.entries.length === PAGE;
      for (const e of r.entries) if (e.user && !users.has(e.user)) {
        users.add(e.user);
        user.append(h('option', { value: e.user }, e.user));
      }
      draw(r.unreachable || []);
    } catch (e) {
      if (!stopped) clear(body, h('div.notice.warn', e.message));
    } finally { loading = false; }
  }

  function draw(unreachable) {
    const all = scope.value === 'all';
    info.textContent = `${entries.length}${more ? '+' : ''} entr${entries.length === 1 ? 'y' : 'ies'}`;
    const rows = entries.map((e) => {
      const tr = h('tr', { class: e.ok ? '' : 'failed' },
        h('td.muted.nowrap', fmt(e.time)),
        all ? h('td', e.node) : null,
        h('td', e.user || h('span.faint', '—')),
        h('td', viaEl(e.via)),
        h('td.activity-action', h('span.mono', e.action),
          e.ok ? null : h('div.error-text', e.error || 'failed')),
        h('td', h('span.status', { class: e.ok ? 'good' : 'crit' }, e.ok ? 'OK' : 'Failed')));
      return tr;
    });
    clear(body,
      unreachable.length ? h('div.notice.warn', `Couldn't reach ${unreachable.join(', ')}; their activity is missing.`) : null,
      rows.length ? h('div.table-wrap', h('table.activity-table',
        h('thead', h('tr', h('th', 'When'), all ? h('th', 'Node') : null, h('th', 'Who'), h('th', 'Through'), h('th', 'What'), h('th', ''))),
        h('tbody', rows)))
        : h('div.empty', 'Nothing recorded yet. Changes made from now on show up here.'),
      more ? h('div.row', { style: { justifyContent: 'center', marginTop: '10px' } },
        h('button.btn', { onclick: () => load(true) }, 'Load older')) : null);
  }

  let debounce;
  search.addEventListener('input', () => { clearTimeout(debounce); debounce = setTimeout(() => load(), 350); });
  user.addEventListener('change', () => load());
  scope.addEventListener('change', () => { try { localStorage.setItem('activity-scope', scope.value); } catch {} load(); });
  load();
  return () => { stopped = true; clearTimeout(debounce); };
}
