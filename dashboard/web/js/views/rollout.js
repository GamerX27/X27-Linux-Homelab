import { h, clear, icon, busy, confirmAction, toast, duration } from '../ui.js';
import * as api from '../api.js';

// The OS update page (#/update). The main node runs the update (internal/rollout) and this
// page only shows it, so it can be closed, reloaded, or lose its connection while the main
// node reboots, and pick up where the update is.

// Starts an OS update on these nodes and opens the update page.
export async function startUpdate(ids) {
  await api.post('/api/rollout', { nodes: ids });
  location.hash = '#/update';
}

const STEPS = ['Download', 'Staged', 'Reboot', 'Back online', 'Verified'];

// Where a node is on STEPS: [index of the step it's on, whether that step is finished].
function position(n) {
  const at = (step) => ({ waiting: 0, staging: 0, staged: 1, rebooting: n.wentDown ? 3 : 2, verifying: 4 }[step] ?? 0);
  switch (n.step) {
    case 'waiting': return [0, false];
    case 'staging': return [0, false];
    case 'staged': return [1, true];
    case 'rebooting': return [at('rebooting'), false];
    case 'verifying': return [4, false];
    case 'done': return [4, true];
    case 'failed': return [at(n.failedStep), false];
    default: return [-1, false];
  }
}

function stepper(n) {
  if (n.step === 'uptodate') return h('div.stepper.muted', 'Already up to date, nothing to do.');
  const [cur, finished] = position(n);
  return h('ol.stepper', STEPS.map((label, i) => {
    let cls = 'todo';
    if (i < cur || (i === cur && finished)) cls = 'done';
    else if (i === cur) cls = n.step === 'failed' ? 'failed' : n.step === 'waiting' ? 'todo' : 'active';
    return h('li', { class: cls, 'aria-current': cls === 'active' ? 'step' : null }, h('span.dot-step'), h('span', label));
  }));
}

// A span of seconds: "42s", "3m 05s", then duration()'s "1h 4m".
function span(sec) {
  const s = Math.max(0, Math.floor(sec));
  if (s < 60) return `${s}s`;
  if (s < 3600) return `${Math.floor(s / 60)}m ${String(s % 60).padStart(2, '0')}s`;
  return duration(s);
}

const since = (unix) => unix ? span(Date.now() / 1000 - unix) : '';

const STATE = {
  running: ['warn', 'Updating'],
  stopped: ['crit', 'Stopped'],
  done: ['good', 'Finished'],
  failed: ['crit', 'Finished with errors'],
};

export function renderRollout(el, { menuButton, refreshNodes }) {
  let timer, stopped = false, job = null, lostSince = 0;
  const logs = new Map(); // node id -> { box, pre, offset, open }
  const sub = h('div.sub');
  const banner = h('div');
  const body = h('div.stack');
  clear(el,
    h('div.page-head', h('div.row', menuButton(), h('div', h('h1', 'OS update'), sub))),
    h('div.stack', banner, body));

  async function load() {
    let next = 3000;
    try {
      const r = await api.get('/api/rollout');
      // Refresh the sidebar and node list when the update moves on (or the node came back).
      if (lostSince || r.job?.state !== job?.state) refreshNodes();
      job = r.job;
      lostSince = 0;
      if (!job || job.state !== 'running') next = 15000;
    } catch (e) {
      if (e.status === 401) return; // the login form takes over and brings us back here
      if (!lostSince) lostSince = Date.now();
    }
    if (stopped) return;
    draw();
    await pollLogs();
    timer = setTimeout(load, next);
  }

  function draw() {
    drawBanner();
    if (!job) {
      if (lostSince) { sub.textContent = ''; clear(body); return; }
      sub.textContent = 'No update running';
      clear(body, h('div.card.empty', h('p', { style: { margin: '0 0 12px' } }, 'No OS update is running.'),
        h('a.btn', { href: '#/' }, 'Go to all nodes')));
      return;
    }
    const [cls, label] = STATE[job.state] || ['idle', job.state];
    const ns = job.nodes || [];
    const count = (s) => ns.filter((n) => n.step === s).length;
    sub.textContent = `${ns.length} node${ns.length === 1 ? '' : 's'} · started by ${job.user || 'someone'} · `
      + (job.finishedAt ? `took ${span(job.finishedAt - job.startedAt)}` : `running for ${since(job.startedAt)}`);

    const actions = [];
    if (job.state === 'running') {
      const stopBtn = h('button.btn', { disabled: job.stopRequested, onclick: () => busy(stopBtn, () => api.post('/api/rollout/stop')).then(load) },
        job.stopRequested ? 'Stopping after this node…' : 'Stop after current node');
      actions.push(stopBtn);
    }
    if (job.state === 'stopped' && count('staged')) {
      const contBtn = h('button.btn.primary', { onclick: async () => {
        if (!await confirmAction('Reboot the rest', `Reboot ${ns.filter((n) => n.step === 'staged').map((n) => n.name).join(', ')} into the update, one at a time?`, 'Continue', false)) return;
        busy(contBtn, () => api.post('/api/rollout/continue')).then(load);
      } }, 'Reboot the rest');
      actions.push(contBtn);
    }
    if (job.state !== 'running') {
      const doneBtn = h('button.btn', { class: job.state === 'stopped' && count('staged') ? '' : 'primary', onclick: async () => {
        await busy(doneBtn, () => api.del('/api/rollout'));
        refreshNodes();
        location.hash = '#/';
      } }, job.state === 'stopped' && count('staged') ? 'Leave them staged' : 'Done');
      actions.push(doneBtn);
    }

    const updated = count('done'), failed = count('failed'), current = count('uptodate');
    const summary = job.state === 'running'
      ? (ns.some((n) => n.step === 'staging' || n.step === 'waiting')
        ? 'Every node downloads the new image at the same time. Then they reboot one at a time, and the main node goes last.'
        : 'Rebooting one node at a time. Each one has to come back on the new image before the next one goes.')
      : [updated && `${updated} of ${ns.length} updated and verified`, current && `${current} already up to date`,
        failed && `${failed} failed`, count('staged') && `${count('staged')} staged, not rebooted`].filter(Boolean).join(' · ');

    clear(body,
      h('div.card',
        h('div.card-head', h('div.row', h('span', { class: `status ${cls}` }, label),
          job.stopRequested && job.state === 'running' ? h('span.badge.warn', 'stopping') : null), h('div.row', actions)),
        h('p.muted', { style: { margin: 0 } }, summary),
        job.error ? h('div.notice', { style: { marginTop: '12px' } }, job.error) : null),
      ns.map(nodeCard));
  }

  function nodeCard(n) {
    const msgCls = n.step === 'failed' ? 'crit' : n.step === 'done' ? 'good' : n.step === 'uptodate' ? 'good' : 'idle';
    let lg = logs.get(n.id);
    if (!lg) {
      const pre = h('pre.output.rollout-log');
      lg = { pre, offset: 0, open: false, box: h('div', { hidden: true }, pre) };
      logs.set(n.id, lg);
    }
    const logBtn = h('button.btn.small.ghost', { onclick: () => {
      lg.open = !lg.open; lg.box.hidden = !lg.open; logBtn.textContent = lg.open ? 'Hide log' : 'Log';
      if (lg.open) pollLogs();
    } }, lg.open ? 'Hide log' : 'Log');
    const active = !['done', 'failed', 'uptodate', 'staged', 'waiting'].includes(n.step);
    return h('div.card.rollout-node', { class: n.step },
      h('div.card-head',
        h('div',
          h('div.row', { style: { gap: '8px' } }, h('strong', n.name), n.local ? h('span.badge', 'main') : null),
          h('div.sub.mono', n.from || '?', ' → ', n.toName || n.to || 'newest image')),
        logBtn),
      stepper(n),
      h('div.row.rollout-msg',
        n.error ? h('span.status.crit', n.error)
          : n.msg ? h('span', { class: `status ${msgCls}` }, n.msg) : null,
        active && n.stepAt ? h('span.faint', `· ${since(n.stepAt)}`) : null),
      lg.box);
  }

  // Follows each open log from where it left off; a node that's rebooting just doesn't answer.
  async function pollLogs() {
    await Promise.all([...logs].filter(([, lg]) => lg.open).map(async ([id, lg]) => {
      try {
        const r = await api.get(`/api/n/${encodeURIComponent(id)}/os/log?offset=${lg.offset}`);
        if (r.offset < lg.offset || (lg.offset === 0 && r.offset > 0)) lg.pre.textContent = '';
        const atEnd = lg.pre.scrollTop + lg.pre.clientHeight >= lg.pre.scrollHeight - 4;
        if (r.text) lg.pre.append(r.text);
        lg.offset = r.offset;
        if (!lg.pre.textContent) lg.pre.textContent = 'No output yet.';
        if (atEnd) lg.pre.scrollTop = lg.pre.scrollHeight;
      } catch { /* offline while it reboots */ }
    }));
  }

  function drawBanner() {
    if (!lostSince) { clear(banner); return; }
    const main = job?.nodes?.find((n) => n.local);
    const rebooting = main && ['rebooting', 'verifying'].includes(main.step);
    clear(banner, h('div.card.reconnect',
      h('span.spinner', { 'aria-hidden': 'true' }),
      h('div',
        h('strong', rebooting ? `${main.name} is rebooting into the update` : 'Lost the connection to the main node'),
        h('div.muted', `Reconnecting… ${since(lostSince / 1000)}. This page carries on by itself when it's back.`))));
  }

  // Keep the "running for" and reconnect timers moving between polls.
  const tick = setInterval(() => { if (!stopped && (lostSince || job?.state === 'running')) draw(); }, 1000);
  load().catch((e) => toast(e.message, true));
  return () => { stopped = true; clearTimeout(timer); clearInterval(tick); };
}
