import { h, clear, when, busy, confirmAction, icon, modal, toast } from '../ui.js';
import * as mainApi from '../api.js';
import { startUpdate } from './rollout.js';

const PHASE = {
  checking: 'Checking for an update…',
  downloading: 'Downloading the update…',
  staged: 'Update downloaded, reboot to apply',
  rebooting: 'Rebooting into the update…',
  verifying: 'Back up, checking the new image…',
};

async function showLog(api, name) {
  let text;
  try { text = (await api.get('/os/log?offset=0')).text; } catch (e) { toast(e.message, true); return; }
  const pre = h('pre.output.rollout-log', text || 'No output.');
  modal(`Update log: ${name}`, pre, [{ label: 'Close', value: true, class: 'primary' }]);
  pre.scrollTop = pre.scrollHeight;
}

// The OS image only; container image updates live in the Docker tab.
export function renderUpdates(el, { api, node }) {
  let timer, stopped = false;
  const osCard = h('div.card', h('div.empty', 'Loading…'));
  clear(el, osCard);

  async function load() {
    let os, job = null;
    try {
      os = await api.get('/os');
    } catch (e) {
      os = { error: e.message };
    }
    try { job = (await mainApi.get('/api/rollout')).job; } catch { /* only the main node knows */ }
    if (stopped) return;
    drawOS(os, job?.state === 'running' && job.nodes?.some((n) => n.id === node.id));
    // Poll faster while a check or update runs.
    const running = os.check?.checking || os.updating;
    timer = setTimeout(load, running ? 3000 : 30000);
  }

  function drawOS(s, inRollout) {
    const head = h('div.card-head', h('h2', 'Operating system image'));
    if (!s.supported) {
      clear(osCard, head, h('div.notice', s.error || 'Not available on this node.'));
      return;
    }
    const c = s.check || {};
    const job = s.job;
    let status;
    if (s.updating) status = h('span.status.warn', PHASE[job?.phase] || 'Updating…', s.progress ? h('span.muted', ` · ${s.progress}`) : null);
    else if (c.checking) status = h('span.status.idle', 'Checking…');
    else if (c.error) status = h('span.status.crit', 'Check failed');
    else if (s.staged) status = h('span.status.warn', 'Update downloaded, reboot to apply');
    else if (c.available) status = h('span.status.warn', `Update available${c.version ? ': ' + c.version : ''}`);
    else if (c.checkedAt) status = h('span.status.good', 'Up to date');
    else status = h('span.status.idle', 'Not checked yet');

    const checkBtn = h('button.btn', { disabled: c.checking, onclick: () =>
      busy(checkBtn, () => api.post('/os/check')).then(() => setTimeout(load, 1500)) }, icon('refresh', 15), 'Check now');
    const updateBtn = inRollout ? h('a.btn.primary', { href: '#/update' }, 'View progress')
      : h('button.btn.primary', { disabled: s.updating || !(c.available || s.staged), onclick: async () => {
        if (!await confirmAction('Install update', `${node.name} downloads the new image and reboots into it. Containers restart with it. `
          + 'You follow it on the update page.', s.staged ? 'Reboot to apply' : 'Update and reboot', false)) return;
        busy(updateBtn, () => startUpdate([node.id]));
      } }, s.staged && !c.available ? 'Reboot to apply' : 'Update now');
    const rebootBtn = h('button.btn', { onclick: async () => {
      if (!await confirmAction('Reboot', `Reboot ${node.name} now?`, 'Reboot')) return;
      busy(rebootBtn, () => api.post('/power/reboot'));
    } }, 'Reboot');
    const rollbackBtn = s.canRollback ? h('button.btn.danger', { onclick: async () => {
      if (!await confirmAction('Roll back', `Boot ${node.name} into the previous image? It reboots right away.`, 'Roll back and reboot')) return;
      busy(rollbackBtn, () => api.post('/os/rollback'));
    } }, 'Roll back') : null;

    head.append(h('div.row', checkBtn, updateBtn, rebootBtn, rollbackBtn));
    clear(osCard, head,
      h('div.row', { style: { marginBottom: '12px' } }, status,
        h('span.faint', `· last checked ${when(c.checkedAt)} · checks every 6 hours`)),
      c.error ? h('pre.output', { style: { marginBottom: '12px' } }, c.error) : null,
      lastJob(job),
      h('div.table-wrap', h('table',
        h('thead', h('tr', h('th', 'Deployment'), h('th', 'Version'), h('th', 'Image'), h('th', 'Built'))),
        h('tbody', (s.deployments || []).map((d) => h('tr',
          h('td', d.booted ? h('span.badge.good', 'running') : d.staged ? h('span.badge.warn', 'staged') : h('span.badge', 'rollback'),
            d.pinned ? h('span.badge', { style: { marginLeft: '4px' } }, 'pinned') : null),
          h('td.mono', d.version || '—'),
          h('td', h('div.mono', d.image || '—'), d.digest ? h('div.sub.mono', d.digest.slice(0, 19) + '…') : null),
          h('td.num-cell.muted', d.timestamp ? new Date(d.timestamp * 1000).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }) : '—')))))),
      c.available && c.version ? h('p.muted', { style: { marginBottom: 0 } }, `Newer image: `, h('span.mono', c.version)) : null);
  }

  // The latest update run (from the dashboard, the schedule or `autoupdate-run`) once it's over.
  function lastJob(j) {
    if (!j || !['done', 'failed'].includes(j.phase)) return null;
    const at = when(j.finishedAt || j.updatedAt);
    const line = j.phase === 'done'
      ? h('span.status.good', `Updated ${j.fromName || j.from || '?'} → ${j.toName || j.to || 'new image'}, checked after the reboot`)
      : h('span.status.crit', 'Last update failed');
    return h('div.notice.last-update', { class: j.phase },
      h('div.row', line, h('span.faint', `· ${at}`), h('button.btn.small.ghost', { onclick: () => showLog(api, node.name) }, 'Log')),
      j.phase === 'failed' && j.error ? h('div.muted', { style: { marginTop: '4px' } }, j.error) : null);
  }

  load().catch(() => {});
  return () => { stopped = true; clearTimeout(timer); };
}
