import { h, clear, bytes, modal, toast, progressBar } from '../ui.js';
import * as mainApi from '../api.js';

// Moving a stack, with its folder and named volumes, to another node. The main node runs the
// move (see internal/migrate); these are its dialogs.

const STEPS = [['check', 'Check'], ['copying', 'Copy'], ['starting', 'Start'], ['verifying', 'Verify'], ['finishing', 'Finish']];

const list = (items, cls) => items.length ? h('ul.move-notes', items.map((t) => h('li', { class: cls }, t))) : null;

// Asks where to, shows what the move takes and what's in the way, then starts it.
export async function moveStackDialog({ node, stack, onStarted }) {
  let nodes;
  try { nodes = (await mainApi.get('/api/nodes')).filter((n) => n.id !== node.id); } catch (e) { toast(e.message, true); return; }
  if (!nodes.length) { toast('There\'s no other node to move it to. Pair one under All nodes first.', true); return; }
  const online = nodes.filter((n) => n.online);
  const target = h('select', { 'aria-label': 'Move to' },
    nodes.map((n) => h('option', { value: n.id, disabled: !n.online }, n.online ? n.name : `${n.name} (offline)`)));
  if (online.length) target.value = online[0].id;
  const del = h('input', { type: 'checkbox' });
  const delLabel = h('span');
  const report = h('div.move-report', h('div.faint', 'Checking…'));
  const err = h('div.error-text');
  let check = null, gen = 0;
  const moveBtn = { label: 'Move', class: 'primary', onclick: start };

  async function runCheck() {
    const my = ++gen;
    check = null;
    setMoveEnabled(false);
    const toName = nodes.find((n) => n.id === target.value)?.name || '';
    delLabel.textContent = `Delete it and its data from ${node.name} once it runs on ${toName}`;
    clear(report, h('div.faint', 'Checking both nodes…'));
    try {
      const c = await mainApi.post('/api/move/check', { from: node.id, stack, to: target.value });
      if (my !== gen) return;
      check = c;
      const p = c.plan;
      clear(report,
        h('dl.kv',
          h('dt', 'Copies'), h('dd', `${bytes(p.size)}: ~/docker/${p.folder}${p.volumes.length ? ` and ${p.volumes.length} volume${p.volumes.length > 1 ? 's' : ''} (${p.volumes.map((v) => v.name).join(', ')})` : ''}`),
          h('dt', `Free on ${c.toName}`), h('dd', c.target.free >= 0 ? bytes(c.target.free) : 'unknown'),
          p.ports.length ? [h('dt', 'Ports'), h('dd', p.ports.join(', '))] : null,
          h('dt', 'Downtime'), h('dd', p.running ? `${stack} is stopped on ${c.fromName} while it's copied, then started on ${c.toName}` : `${stack} isn't running, so nothing stops`)),
        list(c.problems, 'crit'), list(c.warnings, 'warn'),
        c.problems.length ? null : h('div.faint', `If it doesn't start on ${c.toName}, the copy there is removed and ${stack} is started again on ${c.fromName}.`));
      setMoveEnabled(!c.problems.length);
    } catch (e) {
      if (my === gen) clear(report, h('div.notice.warn', e.message));
    }
  }

  function setMoveEnabled(on) {
    const btn = [...document.querySelectorAll('.modal .actions .btn.primary')].at(-1);
    if (btn) btn.disabled = !on;
  }

  async function start() {
    if (!check || check.problems.length) return false;
    try {
      const r = await mainApi.post('/api/move', { from: node.id, stack, to: target.value, deleteAfter: del.checked });
      onStarted?.(r.move);
      setTimeout(() => showMove(r.move.id), 0);
      return true;
    } catch (e) { err.textContent = e.message; return false; }
  }

  target.addEventListener('change', runCheck);
  const shown = modal(`Move ${stack} to another node`, h('div.stack',
    h('label.field', 'Move to', target), report,
    h('label.check-row', del, delLabel), err), [{ label: 'Cancel', value: null }, moveBtn]);
  runCheck();
  await shown;
}

// Follows a move: its steps, how much is copied, and its log.
export async function showMove(id) {
  const steps = h('ol.stepper');
  const status = h('div.move-status');
  const pre = h('pre.output', { style: { maxHeight: '36vh' } });
  let open = true, t, current = null;
  const cancelBtn = h('button.btn.small', { onclick: async () => {
    cancelBtn.disabled = true;
    try { await mainApi.post(`/api/move/${id}/cancel`); } catch (e) { toast(e.message, true); }
  } }, 'Cancel move');

  const tick = async () => {
    try {
      const m = (await mainApi.get('/api/move')).moves.find((x) => x.id === id);
      if (!m) { clear(status, h('div.notice.warn', 'This move is gone (the dashboard restarted).')); return; }
      current = m;
      const at = STEPS.findIndex(([k]) => k === m.phase);
      const failedAt = m.phase === 'failed' || m.phase === 'cancelled' ? Math.max(1, STEPS.findIndex(([k]) => k === m.failedStep)) : -1;
      clear(steps, STEPS.map(([k, label], i) => {
        let cls = '';
        if (m.phase === 'done' || i < at || (failedAt >= 0 && i < failedAt) || i === 0) cls = 'done';
        if (i === at) cls = 'active';
        if (failedAt >= 0 && i === failedAt) cls = 'failed';
        return h('li', { class: cls }, h('span.dot-step'), h('span', label));
      }));
      const pct = m.total > 0 ? Math.min(99, m.done / m.total * 100) : 0;
      clear(status,
        m.phase === 'copying' ? h('div.stack', { style: { gap: '6px' } },
          h('div.row', h('span', `Copying ${m.stack} from ${m.fromName} to ${m.toName}`), h('span.spacer'),
            h('span.faint', `${bytes(m.done)} sent${m.total ? ` of about ${bytes(m.total)} (before compression)` : ''}`), cancelBtn),
          progressBar(pct, 'Copied')) : null,
        m.phase === 'done' ? h('div.notice', `${m.stack} now runs on ${m.toName}.${m.deleteAfter ? '' : ` The old copy on ${m.fromName} is stopped and kept; remove it there when you're happy.`}`) : null,
        m.phase === 'failed' ? h('div.notice.crit', `${m.error}.${m.failedStep === 'starting' ? ` The copy on ${m.toName} was removed.` : ''}${m.rolledBack ? ` ${m.stack} runs on ${m.fromName} again.` : ` ${m.stack} is still on ${m.fromName}.`}`) : null,
        m.phase === 'cancelled' ? h('div.notice.warn', `Cancelled.${m.rolledBack ? ` ${m.stack} runs on ${m.fromName} again.` : ''}`) : null,
        m.warnings?.length && m.phase === 'done' ? list(m.warnings, 'warn') : null);
      const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
      pre.textContent = m.log.join('\n');
      if (atBottom) pre.scrollTop = pre.scrollHeight;
      if (m.finishedAt) return;
    } catch (e) { clear(status, h('div.notice.warn', e.message)); }
    if (open) t = setTimeout(tick, 1000);
  };
  tick();
  await modal('Moving a stack', h('div.stack', { 'data-wide': '1' }, steps, status, pre), [{ label: 'Close', value: true, class: 'primary' }]);
  open = false;
  clearTimeout(t);
  return current;
}

// The moves that are running, for badges on the stack list.
export async function runningMoves() {
  try { return (await mainApi.get('/api/move')).moves.filter((m) => !m.finishedAt); } catch { return []; }
}
