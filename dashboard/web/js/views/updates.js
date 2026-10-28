import { h, clear, when, busy, confirmAction, icon, modal, toast, release } from '../ui.js';
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
export function renderUpdates(el, { api, node, refreshNodes }) {
  let timer, chTimer, stopped = false;
  const osCard = h('div.card', h('div.empty', 'Loading…'));
  const chCard = h('div.card', h('div.empty', 'Loading…'));
  clear(el, h('div.stack', osCard, chCard));
  let channelState = null;

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
    else if (s.staged && switchTarget(s)) status = h('span.status.warn', `Switch to ${switchTarget(s)} downloaded, reboot to apply`);
    else if (s.staged) status = h('span.status.warn', 'Update downloaded, reboot to apply');
    else if (c.available) status = h('span.status.warn', `Update available${c.version ? ': ' + release(c.version) : ''}`);
    else if (c.checkedAt) status = h('span.status.good', 'Up to date');
    else status = h('span.status.idle', 'Not checked yet');

    const checkBtn = h('button.btn', { disabled: c.checking, onclick: () =>
      busy(checkBtn, () => api.post('/os/check')).then(() => setTimeout(load, 1500)) }, icon('refresh', 15), 'Check now');
    const updateBtn = inRollout ? h('a.btn.primary', { href: '#/update' }, 'View progress')
      : h('button.btn.primary', { disabled: s.updating || !(c.available || s.staged), onclick: async () => {
        if (!await confirmAction('Install update', `${node.name} downloads the new image and reboots into it. Its containers restart with it. `
          + 'You can follow it on the update page.', s.staged ? 'Reboot to apply' : 'Update and reboot', false)) return;
        busy(updateBtn, () => startUpdate([node.id]));
      } }, s.staged && !c.available ? 'Reboot to apply' : 'Update now');
    // Only while it downloads: once staged, there's nothing left to stop (and no reboot yet).
    const cancelBtn = s.updating && ['checking', 'downloading'].includes(job?.phase) ? h('button.btn', { onclick: async () => {
      if (!await confirmAction('Cancel update', `Stop downloading the update on ${node.name}? Nothing is installed; the next update starts over.`, 'Stop the download')) return;
      busy(cancelBtn, () => api.post('/os/cancel')).then(() => setTimeout(load, 1000));
    } }, 'Cancel') : null;
    const rebootBtn = h('button.btn', { onclick: async () => {
      if (!await confirmAction('Reboot', `Reboot ${node.name} now?`, 'Reboot')) return;
      busy(rebootBtn, () => api.post('/power/reboot'));
    } }, 'Reboot');
    const rollbackBtn = s.canRollback ? h('button.btn.danger', { onclick: async () => {
      if (!await confirmAction('Roll back', `Boot ${node.name} into the previous image? It reboots right away.`, 'Roll back and reboot')) return;
      busy(rollbackBtn, () => api.post('/os/rollback'));
    } }, 'Roll back') : null;

    head.append(h('div.row', checkBtn, updateBtn, cancelBtn, rebootBtn, rollbackBtn));
    clear(osCard, head,
      h('div.row', { style: { marginBottom: '12px' } }, status,
        h('span.faint', `· last checked ${when(c.checkedAt)} · checks every 6 hours`)),
      c.error ? h('pre.output', { style: { marginBottom: '12px' } }, c.error) : null,
      lastJob(job),
      h('div.table-wrap', h('table',
        h('thead', h('tr', h('th', 'State'), h('th', 'Version'), h('th', 'Image'), h('th', 'Built'))),
        h('tbody', (s.deployments || []).map((d) => h('tr',
          h('td', d.booted ? h('span.badge.good', 'running') : d.staged ? h('span.badge.warn', 'staged') : h('span.badge', 'previous'),
            d.pinned ? h('span.badge', { style: { marginLeft: '4px' } }, 'pinned') : null),
          h('td.mono', d.version || '—'),
          h('td', h('div.mono', d.image || '—'), d.digest ? h('div.sub.mono', d.digest.slice(0, 19) + '…') : null),
          h('td.num-cell.muted', d.timestamp ? new Date(d.timestamp * 1000).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' }) : '—')))))));
  }

  // The latest update run (from the dashboard, the schedule or `autoupdate-run`) once it's over.
  function lastJob(j) {
    if (!j || !['done', 'failed', 'cancelled'].includes(j.phase)) return null;
    const at = when(j.finishedAt || j.updatedAt);
    const line = j.phase === 'done'
      ? h('span.status.good', `Updated ${j.fromName || j.from || '?'} → ${j.toName || j.to || 'new image'}, checked after the reboot`)
      : j.phase === 'cancelled' ? h('span.status.idle', 'Last update cancelled')
      : h('span.status.crit', 'Last update failed');
    return h('div.notice.last-update', { class: j.phase },
      h('div.row', line, h('span.faint', `· ${at}`), h('button.btn.small.ghost', { onclick: () => showLog(api, node.name) }, 'Log')),
      j.phase !== 'done' && j.error ? h('div.muted', { style: { marginTop: '4px' } }, j.error) : null);
  }

  // The channel a staged deployment switches to, when it isn't the one running.
  function switchTarget(s) {
    const c = channelState;
    return c?.stagedChannel && c.stagedChannel !== c.current && s.staged ? c.stagedChannel : '';
  }

  async function loadChannel() {
    let c;
    try { c = await api.get('/os/channel'); } catch (e) { c = { error: e.message }; }
    if (stopped) return;
    const was = channelState?.switching;
    channelState = c;
    drawChannel(c);
    if (was && !c.switching) { load(); refreshNodes?.(); }
    chTimer = setTimeout(loadChannel, c.switching ? 2000 : 60000);
  }

  function drawChannel(c) {
    const head = h('div.card-head', h('h2', 'Release channel'));
    if (!c.available) {
      clear(chCard, head, h('div.notice', c.error || 'Not available on this node.'));
      return;
    }
    head.append(c.current ? h('span.badge.good', c.current) : h('span.badge', 'unknown image'));
    let pick = c.current;
    const rows = (c.channels || []).map((ch) => {
      const input = h('input', { type: 'radio', name: 'channel', value: ch.name, checked: ch.name === c.current,
        disabled: c.switching, onchange: () => { pick = ch.name; sync(); } });
      return h('label.channel-row', input,
        h('div.text', h('div.row', h('strong', ch.name),
          ch.name === c.current ? h('span.badge.good', 'running') : null,
          ch.name === c.stagedChannel && ch.name !== c.current ? h('span.badge.warn', 'after reboot') : null,
          /may break/.test(ch.about) ? h('span.badge.warn', 'may break') : null),
        h('div.desc', ch.about), h('div.sub.mono', ch.image.replace(/^.*docker:\/\//, ''))));
    });
    const switchBtn = h('button.btn.primary', { onclick: async () => {
      const ch = c.channels.find((x) => x.name === pick);
      if (!await confirmAction(`Switch to ${pick}`, `${node.name} downloads the ${pick} image (${ch.about}). It applies after a reboot; `
        + 'your data, containers and settings stay. Until the next update, Roll back on this page goes back to the image you run now.',
        `Switch to ${pick}`, /may break/.test(ch.about))) return;
      if (await busy(switchBtn, () => api.post('/os/channel', { channel: pick }))) loadChannel();
    } }, 'Switch');
    const sync = () => { switchBtn.disabled = c.switching || pick === (c.stagedChannel || c.current); };
    sync();
    const log = h('pre.output.rollout-log.hidden');
    if (c.switching || c.stagedChannel) {
      api.get('/os/channel/log').then((r) => {
        if (!r.text) return;
        log.textContent = r.text;
        log.classList.remove('hidden');
        log.scrollTop = log.scrollHeight;
      }).catch(() => {});
    }
    clear(chCard, head,
      h('p.muted', { style: { marginTop: 0 } }, 'Which image this node follows. Updates only come from its channel. ',
        'On the node itself, ', h('code', 'channel'), ' does the same.'),
      c.switching ? h('div.notice.warn', { style: { marginBottom: '12px' } }, 'Downloading the new channel\'s image…') : null,
      c.stagedChannel && c.stagedChannel !== c.current && !c.switching
        ? h('div.notice.warn', { style: { marginBottom: '12px' } }, `Switch to ${c.stagedChannel} is ready. Reboot ${node.name} to apply it.`) : null,
      h('div.stack', { style: { gap: '0' } }, rows),
      h('div.row', { style: { marginTop: '12px' } }, switchBtn),
      log);
  }

  load().catch(() => {});
  loadChannel();
  return () => { stopped = true; clearTimeout(timer); clearTimeout(chTimer); };
}
