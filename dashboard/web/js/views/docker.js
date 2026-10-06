import { h, clear, bytes, busy, confirmAction, modal, toast, icon, pct, when, menu, menuOpen, modalOpen } from '../ui.js';
import { openStackEditor } from './stackeditor.js';
import { WEEKDAYS } from './features.js';

const SECTIONS = ['Apps', 'Images', 'Storage', 'Backups'];
const PROJECT = 'com.docker.compose.project';

function stateClass(s) {
  return s === 'running' ? 'good' : s === 'restarting' || s === 'paused' ? 'warn' : s === 'dead' ? 'crit' : 'idle';
}

function ago(unix) {
  const d = (Date.now() / 1000 - unix);
  if (d < 3600) return `${Math.round(d / 60)} min ago`;
  if (d < 86400) return `${Math.round(d / 3600)} h ago`;
  return `${Math.round(d / 86400)} d ago`;
}

const cname = (c) => c.Names?.[0]?.replace(/^\//, '') || c.Id.slice(0, 12);

// Host to open published ports on: the page's own host for the main node, the node's
// address for paired nodes.
function portHost(node) {
  if (!node || node.local || node.id === 'local') return location.hostname;
  if (!node.address) return null; // not known yet: show ports without links
  const m = node.address.match(/^\[([^\]]+)\]/);
  return m ? `[${m[1]}]` : node.address.replace(/:\d+$/, '');
}

// Published ports as links (tcp) or plain text (udp); one entry per host port.
function portLinks(ports, host) {
  const seen = new Set();
  const out = [];
  for (const p of (ports || []).filter((x) => x.PublicPort && x.IP !== '127.0.0.1' && x.IP !== '::1')
    .sort((a, b) => a.PublicPort - b.PublicPort)) {
    const key = `${p.PublicPort}/${p.Type}`;
    if (seen.has(key)) continue;
    seen.add(key);
    const label = p.PublicPort === p.PrivatePort ? `${p.PublicPort}` : `${p.PublicPort}→${p.PrivatePort}`;
    if (p.Type === 'tcp' && host) {
      const scheme = [443, 8443, 9443].includes(p.PrivatePort) ? 'https' : 'http';
      out.push(h('a.port', { href: `${scheme}://${host}:${p.PublicPort}`, target: '_blank', rel: 'noopener noreferrer',
        title: `Open ${scheme}://${host}:${p.PublicPort}`, onclick: (e) => e.stopPropagation() }, label));
    } else {
      out.push(h('span.port.udp', { title: p.Type === 'udp' ? 'UDP' : '' }, p.Type === 'udp' ? `${label}/udp` : label));
    }
  }
  return out.length ? h('div.ports', out) : null;
}

// Names of the containers whose image has an update, sorted.
const outdated = (cs, upd) => cs.filter((c) => upd.get(c.Image) === 'update').map(cname).sort();

// "a, b, c +2" for a badge; the full list goes in its title.
const shortList = (names, max = 3) => names.length > max ? `${names.slice(0, max).join(', ')} +${names.length - max}` : names.join(', ');

function showOutput(title, text) {
  if (text) modal(title, h('pre.output', text), [{ label: 'Close', value: true, class: 'primary' }]);
}

// ---- Container update jobs (GET /docker/updates → job) ---------------------------------

const PHASE = { starting: 'starting', checking: 'checking registries', pulling: 'pulling images', recreating: 'recreating containers' };

export const jobRunning = (j) => !!j && !j.finishedAt;

// "Updating web · pulling images (2 of 3)" while a job runs.
export function jobLine(j) {
  const done = j.updated + j.failed;
  const total = done + (j.current ? 1 : 0) + (j.pending?.length || 0);
  const step = PHASE[j.phase] || 'updating';
  if (!j.current) return `Updating · ${step}…`;
  return `Updating ${j.current} · ${step}…${total > 1 ? ` (${done + 1} of ${total})` : ''}`;
}

// How a finished job ended, in a few words.
export function jobSummary(j) {
  if (j.phase === 'failed') return j.updated + j.failed ? `${j.updated} updated, ${j.failed} failed` : `Update failed: ${j.error}`;
  return j.updated ? `${j.updated} updated` : 'Already up to date';
}

// The running or last update's output, refreshed while it runs.
export async function showUpdateLog(api, title = 'Update log') {
  const pre = h('pre.output', { style: { maxHeight: '60vh' } }, 'Loading…');
  let t, open = true;
  const tick = async () => {
    let running = false;
    try {
      const [l, st] = await Promise.all([api.get('/docker/updates/log'), api.get('/docker/updates')]);
      const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
      pre.textContent = l.text || (jobRunning(st.job) ? 'Waiting for output…' : 'No output.');
      if (atBottom) pre.scrollTop = pre.scrollHeight;
      running = jobRunning(st.job);
    } catch (e) { pre.textContent = e.message; }
    if (open && running) t = setTimeout(tick, 2000);
  };
  tick();
  await modal(title, h('div', { 'data-wide': '1' }, pre), [{ label: 'Close', value: true, class: 'primary' }]);
  open = false; clearTimeout(t);
}

export function renderDocker(el, { api, node, getNode, refreshNodes }) {
  let section = 'Apps', timer, stopped = false;
  let watching = null, dismissed = null; // job IDs: the one to toast when it ends, the hidden result
  let backupWatching = null; // backup job to toast when it ends
  const bk = {}; // the Backups section's form, built once so polling doesn't wipe edits
  const data = {};
  const expanded = new Map(); // group key → open? (only once the user toggled it)
  const body = h('div.stack');
  const nav = h('div.seg', { role: 'tablist' });
  const filter = h('input.filter-input', { type: 'search', placeholder: 'Filter…', 'aria-label': 'Filter', oninput: () => draw() });
  const newStackBtn = h('button.btn.primary', { onclick: () =>
    openStackEditor({ api, root: data.stacks?.root || '~/docker', onDone: () => load() }) }, icon('plus', 15), 'New stack');
  const refreshBtn = h('button.btn', { onclick: () => load(), title: 'Refresh', 'aria-label': 'Refresh' }, icon('refresh', 15));
  clear(el, h('div.stack',
    h('div.row', nav, h('span.spacer'), filter, refreshBtn, newStackBtn),
    body));

  function drawNav() {
    clear(nav, SECTIONS.map((s) => h('button.seg-btn', { role: 'tab', 'aria-selected': String(s === section), class: s === section ? 'active' : '',
      onclick: () => { section = s; filter.value = ''; bk.built = null; drawNav(); clear(body, h('div.card.empty', 'Loading…')); load(); } }, s)));
    newStackBtn.classList.toggle('hidden', section !== 'Apps');
  }

  async function load() {
    clearTimeout(timer);
    const sec = section;
    try {
      if (sec === 'Apps') {
        const [stacks, containers, updates] = await Promise.all([
          api.get('/docker/stacks'), api.get('/docker/containers'), api.get('/docker/updates')]);
        Object.assign(data, { stacks, containers, updates });
        const j = updates.job;
        if (jobRunning(j)) watching = j.id;
        else if (j && j.id === watching) {
          watching = null;
          toast(`Update ${j.phase === 'failed' ? 'failed' : 'finished'}: ${jobSummary(j)}.`, j.phase === 'failed');
          refreshNodes?.();
        }
      } else if (sec === 'Backups') {
        data.backups = await api.get('/docker/backups');
        const j = data.backups.backups.job;
        if (jobRunning(j)) backupWatching = j.id;
        else if (j && j.id === backupWatching) {
          backupWatching = null;
          toast(j.phase === 'failed' ? `Backup failed: ${j.error}.` : `Backup finished: ${j.done} app${j.done === 1 ? '' : 's'} backed up.`, j.phase === 'failed');
        }
      } else if (sec === 'Images') {
        const [images, containers] = await Promise.all([api.get('/docker/images'), api.get('/docker/containers')]);
        Object.assign(data, { images, containers });
      } else {
        const [volumes, networks] = await Promise.all([api.get('/docker/volumes'), api.get('/docker/networks')]);
        Object.assign(data, { volumes, networks });
      }
      if (sec !== section || stopped) return;
      draw();
    } catch (e) {
      if (sec !== section || stopped) return;
      clear(body, h('div.notice.warn', e.message));
    }
    schedule();
  }

  // Refresh Apps every 5 s (2 s while an update runs, 3 s while a check runs), and Backups
  // every 2 s while one runs, but not under an open menu or dialog.
  function schedule() {
    clearTimeout(timer);
    if (stopped) return;
    if (section === 'Backups') {
      if (!jobRunning(data.backups?.backups.job)) return;
      timer = setTimeout(() => { if (menuOpen() || modalOpen()) schedule(); else load(); }, 2000);
      return;
    }
    if (section !== 'Apps') return;
    timer = setTimeout(() => {
      if (menuOpen() || modalOpen()) schedule();
      else load();
    }, jobRunning(data.updates?.job) ? 2000 : data.updates?.checking ? 3000 : 5000);
  }

  const match = (...fields) => {
    const q = filter.value.trim().toLowerCase();
    return !q || fields.some((f) => String(f ?? '').toLowerCase().includes(q));
  };

  // Looked up on every draw: the node list may not have loaded when the tab opened, and the
  // links must point at the node the containers run on, not the main node.
  let host = portHost(node);
  function draw() {
    host = portHost(getNode?.() || node);
    if (section === 'Apps') drawApps();
    else if (section === 'Images') drawImages();
    else if (section === 'Backups') drawBackups();
    else drawStorage();
  }

  // ---- Apps: stacks with their containers, and image updates --------------------------

  // Starts an update job; its progress then shows in the strip and on the rows.
  async function startUpdate(btn, path, body) {
    const r = await busy(btn, () => api.post(path, body), '');
    if (r?.job) { watching = r.job.id; toast('Update started. Progress shows above.'); }
    load();
  }

  // pending: [{ label, names }] per stack / standalone group with containers to update.
  function updateStrip(pending) {
    const u = data.updates || {};
    const job = u.job, running = jobRunning(job);
    const avail = (u.images || []).filter((i) => i.state === 'update');
    const summary = pending.map((p) => `${p.label} (${p.names.join(', ')})`).join('; ');
    const check = h('button.btn.small', { disabled: u.checking || running, onclick: () =>
      busy(check, () => api.post('/docker/updates/check'), '').then(() => setTimeout(load, 1000)) },
      icon('refresh', 14), u.checking ? 'Checking…' : 'Check now');
    const all = avail.length && !running ? h('button.btn.small.primary', { onclick: async () => {
      const list = h('ul', { style: { margin: '8px 0', paddingLeft: '20px' } },
        pending.map((p) => h('li', h('strong', p.label), ': ', p.names.join(', '))));
      const msg = h('span', 'Pull the newer images and recreate these containers, one pass per stack? Containers that depend on them may restart too. Data is kept.', list);
      if (!await confirmAction('Update all', msg, 'Update all', false)) return;
      startUpdate(all, '/docker/updates/apply-all');
    } }, `Update all (${avail.length})`) : null;
    const logBtn = job ? h('button.btn.small.ghost', { onclick: () => showUpdateLog(api) }, 'Log') : null;
    const status = running ? h('span.status.warn.job-status', h('span.spinner.small', { 'aria-hidden': 'true' }), jobLine(job))
      : u.checking ? h('span.status.idle', 'Checking registries…')
      : !u.checkedAt ? h('span.status.idle', 'Images not checked yet')
      : avail.length ? h('span.status.warn', { title: summary }, `${avail.length} image update${avail.length > 1 ? 's' : ''} available`)
      : h('span.status.good', 'All images up to date');
    const checked = u.checkedAt ? when(u.checkedAt).replace(/^.*\((.*)\)$/, '$1') : '';
    const strip = h('div.update-strip', { class: running ? 'running' : '', role: 'status' }, status,
      !running && !u.checking && pending.length ? h('span.muted.update-list', { title: summary }, pending.map((p) => `${p.label}: ${shortList(p.names)}`).join(' · ')) : null,
      !running && checked ? h('span.faint', `· checked ${checked}`) : null,
      u.error ? h('span.faint', `· ${u.error}`) : null, h('span.spacer'), running ? logBtn : null, check, all);
    // The last run's result, until dismissed.
    const last = job && !running && job.id !== dismissed ? h('div.notice.update-result', { class: job.phase === 'failed' ? 'failed' : 'done' },
      h('span', { class: `status ${job.phase === 'failed' ? 'crit' : 'good'}` },
        job.phase === 'failed' ? `Last update failed: ${jobSummary(job)}` : `Last update finished: ${jobSummary(job)}`),
      h('span.faint', `· ${when(job.finishedAt).replace(/^.*\((.*)\)$/, '$1')}`),
      h('span.spacer'), logBtn,
      h('button.btn.small.ghost', { 'aria-label': 'Dismiss', title: 'Dismiss', onclick: () => { dismissed = job.id; draw(); } }, '✕')) : null;
    return [strip, last];
  }

  function drawApps() {
    const stacks = data.stacks?.stacks || [];
    const cs = data.containers || [];
    const upd = new Map((data.updates?.images || []).map((i) => [i.image, i.state]));
    const known = new Set(stacks.map((s) => s.name));
    const groups = stacks.map((s) => ({ key: 's:' + s.name, stack: s, containers: cs.filter((c) => c.Labels?.[PROJECT] === s.name) }));
    const standalone = cs.filter((c) => !known.has(c.Labels?.[PROJECT] || ''));
    if (standalone.length) groups.push({ key: 'standalone', containers: standalone });

    const pending = groups.map((g) => ({ label: g.stack?.name || 'standalone', names: outdated(g.containers, upd) }))
      .filter((p) => p.names.length);
    const visible = groups.filter((g) => match(g.stack?.name, g.stack?.dir, ...g.containers.map(cname), ...g.containers.map((c) => c.Image)));
    clear(body,
      updateStrip(pending),
      visible.length ? h('div.apps', visible.map((g) => groupEl(g, upd)))
        : h('div.card.empty', stacks.length || cs.length ? 'Nothing matches the filter.'
          : ['No apps yet. ', h('button.btn.small.primary', { onclick: () => newStackBtn.click() }, 'Create a stack')]),
      h('div.faint', { style: { fontSize: '12.5px' } }, 'New stacks go in ', h('span.mono', data.stacks?.root || '~/docker'), '/<name>/compose.yml'));
  }

  function groupEl(g, upd) {
    const s = g.stack;
    const job = data.updates?.job, jr = jobRunning(job);
    const key = s?.name;
    const updating = jr && key && job.current === key;
    const queued = jr && key && job.pending?.includes(key);
    const names = outdated(g.containers, upd);
    const updates = names.length;
    const running = g.containers.filter((c) => c.State === 'running').length;
    const total = s ? s.total : g.containers.length;
    const troubled = total > 0 && running < total;
    const open = expanded.has(g.key) ? expanded.get(g.key) : (!s || troubled || updates > 0);
    const toggle = () => { expanded.set(g.key, !open); draw(); };

    let title, sub, status, actions = [];
    if (s) {
      title = s.name;
      sub = s.file || s.dir || '';
      const cls = !total ? 'idle' : running === total ? 'good' : running ? 'warn' : 'crit';
      status = h('span.status', { class: cls }, !total ? 'Not started' : `${running}/${total} running`);
      const act = (label, action, opts = {}) => async (btn) => {
        if (opts.confirm && !await confirmAction(`${label}: ${s.name}`, opts.confirm, label, !!opts.danger)) return;
        const run = () => api.post(`/docker/stacks/${encodeURIComponent(s.name)}/${action}`);
        const r = btn ? await busy(btn, run, '') : await run().catch((e) => { toast(e.message, true); });
        if (r) {
          toast(`${s.name}: ${label.toLowerCase()} done.`);
          if (opts.output) showOutput(`${label}: ${s.name}`, r.output);
          if (action === 'recreate') refreshNodes?.();
        }
        load();
      };
      let primary = null;
      if (updating || queued) {
        primary = null; // the badge says it
      } else if (updates && s.filesExist) {
        primary = h('button.btn.small.primary', { title: jr ? 'Another update is running' : `Update ${names.join(', ')}`, disabled: jr, onclick: async (e) => {
          const btn = e.currentTarget;
          if (!await confirmAction(`Update: ${s.name}`, `Pull the newer images for ${names.join(', ')} and recreate ${updates > 1 ? 'them' : 'it'}? Containers that depend on ${updates > 1 ? 'them' : 'it'} (like ones sharing its network) may restart too. Data in volumes and bind mounts is kept.`, 'Update', false)) return;
          startUpdate(btn, `/docker/stacks/${encodeURIComponent(s.name)}/update`);
        } }, `Update ${updates}`);
      } else if (running === 0 && s.filesExist) {
        primary = h('button.btn.small', { onclick: (e) => act('Start', 'start')(e.currentTarget) }, 'Start');
      }
      actions = [primary, menu([
        { label: 'Start', onclick: () => act('Start', 'start')(), hidden: running > 0 || !s.filesExist },
        { label: 'Stop', onclick: () => act('Stop', 'stop')(), hidden: running === 0 },
        { label: 'Restart', onclick: () => act('Restart', 'restart')(), hidden: running === 0 },
        { label: 'Pull & recreate all', hidden: !s.filesExist, onclick: () => act('Pull & recreate', 'recreate', { output: true,
          confirm: `Pull the newest images for every service in ${s.name} and recreate all its containers? Data in volumes and bind mounts is kept.` })() },
        { label: 'Back up now', hidden: !s.inRoot, onclick: () => backupNow([s.name], running > 0) },
        { label: 'Edit compose file', hidden: !s.inRoot, onclick: () => openStackEditor({ api, root: data.stacks?.root, name: s.name, onDone: () => load() }) },
        { label: 'Remove…', danger: true, hidden: !total && !s.inRoot, onclick: () => removeStack(s) },
      ], `Actions for ${s.name}`)];
    } else {
      title = 'Standalone containers';
      sub = 'started with docker run';
      status = h('span.status', { class: running === total ? 'good' : running ? 'warn' : 'idle' }, `${running}/${total} running`);
    }

    const head = h('div.app-head', { onclick: (e) => { if (!e.target.closest('button, a')) toggle(); } },
      h('button.chev', { class: open ? 'open' : '', 'aria-expanded': String(open), 'aria-label': `${open ? 'Collapse' : 'Expand'} ${title}`, onclick: toggle }, '›'),
      h('div.app-title',
        h('div.row', { style: { gap: '8px' } }, h('strong', title),
          updating ? h('span.badge.updating', { title: jobLine(job) }, h('span.spinner.small', { 'aria-hidden': 'true' }), `Updating · ${PHASE[job.phase] || 'working'}…`)
          : queued ? h('span.badge', { title: 'Updates after what is updating now' }, 'Queued')
          : updates ? h('span.badge.update', { title: `Updates for ${names.join(', ')}` }, `${updates > 1 ? 'Updates' : 'Update'}: ${shortList(names)}`) : null,
          s && !s.filesExist ? h('span.badge', 'managed elsewhere') : s && !s.inRoot ? h('span.badge', 'outside ~/docker') : null),
        h('div.sub.mono', sub)),
      h('div.app-status', status),
      h('div.app-ports', portLinks(s ? s.ports : [], host)),
      h('div.app-actions', actions));
    return h('div.app-group', { class: `${open ? 'open' : ''}${updating ? ' updating' : ''}` }, head,
      open ? h('div.app-body', g.containers.length
        ? g.containers.sort((a, b) => cname(a).localeCompare(cname(b))).map((c) => containerEl(c, upd, updating))
        : h('div.faint', { style: { padding: '10px 14px' } }, 'No containers. Start the stack to create them.')) : null);
  }

  // stackUpdating: this container's stack is being updated right now.
  function containerEl(c, upd, stackUpdating) {
    const name = cname(c);
    const running = c.State === 'running';
    const hasUpdate = upd.get(c.Image) === 'update';
    const job = data.updates?.job, jr = jobRunning(job);
    const updating = (stackUpdating && hasUpdate) || (jr && job.current === name);
    const queued = !updating && jr && !c.Labels?.[PROJECT] && job.pending?.includes(name);
    const act = (label, action, confirmMsg) => async () => {
      if (confirmMsg && !await confirmAction(`${label}: ${name}`, confirmMsg, label, action === 'remove')) return;
      try {
        const r = await api.post(`/docker/containers/${c.Id}/${action}`);
        toast(`${name}: ${label.toLowerCase()} done.`);
        if (action === 'recreate') { showOutput(`Pull & recreate: ${name}`, r.output); refreshNodes?.(); }
      } catch (e) { toast(e.message, true); }
      load();
    };
    const recreateMsg = c.Labels?.[PROJECT]
      ? `Pulls the newest ${c.Image} and recreates ${name} through its stack; containers that depend on it are recreated too. Volumes and bind mounts are kept.`
      : `Pulls the newest ${c.Image} and recreates ${name} with the same ports, environment, volumes, networks and restart policy. Its data is kept.`;
    const updateBtn = hasUpdate && !updating && !queued ? h('button.btn.small.primary', { disabled: jr,
      title: jr ? 'Another update is running' : `Pull the newer ${c.Image} and recreate what uses it`,
      onclick: (e) => startUpdate(e.currentTarget, '/docker/updates/apply', { image: c.Image }) }, 'Update') : null;
    const badge = updating ? h('span.badge.updating', { style: { marginLeft: '6px' } }, h('span.spinner.small', { 'aria-hidden': 'true' }), 'updating')
      : queued ? h('span.badge', { style: { marginLeft: '6px' } }, 'queued')
      : hasUpdate ? h('span.badge.update', { style: { marginLeft: '6px' } }, 'update') : null;
    return h('div.ct-row', { class: updating ? 'has-update updating' : hasUpdate ? 'has-update' : '' },
      h('div.ct-name', h('span.dot', { class: stateClass(c.State), 'aria-hidden': 'true' }),
        h('button.link-btn', { onclick: () => showLogs(name, c.Id), title: 'Show logs' }, name)),
      h('div.ct-image.mono', { title: c.Image }, c.Image, badge),
      h('div.ct-state', h('span', { class: `status ${stateClass(c.State)}` }, c.State), h('span.faint', ` · ${c.Status}`)),
      h('div.app-ports', portLinks(c.Ports, host)),
      h('div.app-actions', updateBtn, menu([
        { label: 'Logs', onclick: () => showLogs(name, c.Id) },
        { label: 'Stats', onclick: () => showStats(name, c.Id), hidden: !running },
        { label: 'Start', onclick: act('Start', 'start'), hidden: running },
        { label: 'Stop', onclick: act('Stop', 'stop'), hidden: !running },
        { label: 'Restart', onclick: act('Restart', 'restart'), hidden: !running },
        { label: 'Pull & recreate', onclick: act('Pull & recreate', 'recreate', recreateMsg) },
        { label: 'Remove', danger: true, onclick: act('Remove', 'remove', `Remove ${name}? Its volumes are kept.`) },
      ], `Actions for ${name}`)));
  }

  async function removeStack(s) {
    const del = h('input', { type: 'checkbox' });
    const warn = h('div.notice.warn.hidden', 'Everything in the folder is deleted, including bind-mounted data like ./config. This can\'t be undone.');
    del.addEventListener('change', () => warn.classList.toggle('hidden', !del.checked));
    const bodyEl = h('div.stack',
      h('p.muted', { style: { margin: 0 } }, `Stops and removes ${s.name}'s containers and network. Named volumes are kept.`),
      s.inRoot ? h('label.check-row', del, h('span', 'Also delete the folder ', h('code', s.dir), ' and everything in it')) : null,
      warn);
    const ok = await modal(`Remove ${s.name}`, bodyEl, [
      { label: 'Cancel', value: false },
      { label: 'Remove', class: 'danger solid', onclick: async () => {
        try {
          const r = await api.post(`/docker/stacks/${encodeURIComponent(s.name)}/remove`, { deleteFolder: del.checked });
          toast(del.checked ? `${s.name} removed and its folder deleted.` : `${s.name} removed; its folder is kept.`);
          return r || true;
        } catch (e) { toast(e.message, true); return false; }
      } },
    ]);
    if (ok) load();
  }

  // ---- Backups: archives of stack folders, on a schedule or now -------------------------

  // Starts a backup of apps (the saved selection when empty); stopsSome warns first.
  async function backupNow(apps, stopsSome = true, btn) {
    if (stopsSome && !await confirmAction('Back up now',
      `${apps.length ? apps.join(', ') : 'The selected apps'} ${apps.length === 1 ? 'is' : 'are'} stopped while the folder is packed, then started again.`, 'Back up', false)) return;
    const run = () => api.post('/docker/backups/run', { apps });
    const r = btn ? await busy(btn, run, '') : await run().catch((e) => { toast(e.message, true); });
    if (r?.job) {
      backupWatching = r.job.id;
      toast(section === 'Backups' ? 'Backup started.' : 'Backup started. Progress shows under Backups.');
    }
    if (section === 'Backups') load();
  }

  async function showBackupLog() {
    const pre = h('pre.output', { style: { maxHeight: '60vh' } }, 'Loading…');
    let t, open = true;
    const tick = async () => {
      let running = false;
      try {
        const [l, st] = await Promise.all([api.get('/docker/backups/log'), api.get('/docker/backups')]);
        pre.textContent = l.text || 'No output.';
        pre.scrollTop = pre.scrollHeight;
        running = jobRunning(st.backups.job);
      } catch (e) { pre.textContent = e.message; }
      if (open && running) t = setTimeout(tick, 2000);
    };
    tick();
    await modal('Backup log', h('div', { 'data-wide': '1' }, pre), [{ label: 'Close', value: true, class: 'primary' }]);
    open = false; clearTimeout(t);
  }

  const BACKUP_PHASE = { starting: 'starting', stopping: 'stopping', archiving: 'packing the folder', 'starting-again': 'starting again' };

  function backupStatus(b) {
    const j = b.job;
    const logBtn = j ? h('button.btn.small.ghost', { onclick: () => showBackupLog() }, 'Log') : null;
    if (jobRunning(j)) {
      const total = j.done + j.failed + (j.current ? 1 : 0) + (j.pending?.length || 0);
      return h('div.update-strip.running', { role: 'status' },
        h('span.status.warn.job-status', h('span.spinner.small', { 'aria-hidden': 'true' }),
          `Backing up ${j.current || ''} · ${BACKUP_PHASE[j.phase] || 'working'}…${total > 1 ? ` (${j.done + j.failed + 1} of ${total})` : ''}`),
        h('span.spacer'), logBtn);
    }
    if (!j) return null;
    const failed = j.phase === 'failed';
    return h('div.notice.update-result', { class: failed ? 'failed' : 'done' },
      h('span', { class: `status ${failed ? 'crit' : 'good'}` },
        failed ? `Last backup failed: ${j.error}` : `Last backup finished: ${j.done} app${j.done === 1 ? '' : 's'}`),
      h('span.faint', `· ${when(j.finishedAt).replace(/^.*\((.*)\)$/, '$1')}${j.kind === 'scheduled' ? ' · scheduled' : ''}`),
      h('span.spacer'), logBtn);
  }

  function backupArchives(b) {
    const rows = (b.archives || []).filter((a) => match(a.app, a.file)).map((a) => h('tr',
      h('td', h('strong', a.app)), h('td.mono', a.file),
      h('td.num-cell', bytes(a.size)), h('td.num-cell.muted', ago(a.time)),
      h('td.actions', menu([{ label: 'Delete', danger: true, onclick: async () => {
        if (!await confirmAction('Delete backup', `Delete ${a.file}? This can't be undone.`, 'Delete')) return;
        try { await api.post('/docker/backups/delete', { app: a.app, file: a.file }); toast('Backup deleted.'); } catch (e) { toast(e.message, true); }
        load();
      } }], 'Backup actions'))));
    return h('div.card',
      h('div.card-head', h('h2', `Backups (${(b.archives || []).length})`), h('span.faint.mono', b.dest)),
      table(['App', 'File', 'Size', 'Taken', ''], rows, 'No backups yet.'));
  }

  function drawBackups() {
    const { backups: b, apps } = data.backups;
    // While polling, refresh only the status and the list; the form keeps what's typed.
    if (bk.built && bk.status.isConnected) {
      clear(bk.status, backupStatus(b));
      clear(bk.list, backupArchives(b));
      bk.runBtn.disabled = jobRunning(b.job);
      return;
    }
    const c = b.config;
    const selected = new Set(c.apps || []);
    const known = new Set(apps);
    const freq = h('select', [['off', 'Off'], ['daily', 'Daily'], ['weekly', 'Weekly'], ['monthly', 'Monthly']].map(([v, l]) => h('option', { value: v }, l)));
    const weekday = h('select', WEEKDAYS.map(([v, l]) => h('option', { value: v }, l)));
    const day = h('select', Array.from({ length: 31 }, (_, i) => h('option', { value: String(i + 1) }, String(i + 1))));
    const time = h('input', { type: 'time', required: true });
    freq.value = c.freq || 'off'; weekday.value = c.weekday || 'sun'; day.value = c.day || '1'; time.value = c.time || '03:00';
    const wdField = h('label.field', 'Weekday', weekday);
    const dayField = h('label.field', 'Day of month', day);
    const timeField = h('label.field', 'Time (24-hour)', time);
    const sync = () => {
      wdField.classList.toggle('hidden', freq.value !== 'weekly');
      dayField.classList.toggle('hidden', freq.value !== 'monthly');
      timeField.classList.toggle('hidden', freq.value === 'off');
    };
    freq.addEventListener('change', sync);
    sync();
    const dest = h('input', { type: 'text', value: c.dest || '', placeholder: b.dest, spellcheck: false });
    const keep = h('input', { type: 'number', min: 1, max: 365, value: c.keep || 7 });

    // Saved apps that no longer exist stay listed, so they can be unticked.
    const names = [...new Set([...apps, ...selected])].sort();
    const boxes = names.map((n) => {
      const box = h('input', { type: 'checkbox', checked: selected.has(n), value: n });
      return { n, box, el: h('label.check-row', box, h('span', n, known.has(n) ? null : h('span.faint', ' (not in ~/docker anymore)'))) };
    });
    const picked = () => boxes.filter((x) => x.box.checked).map((x) => x.n);
    const all = h('button.btn.small.ghost', { type: 'button', onclick: () => {
      const on = picked().length !== boxes.length;
      boxes.forEach((x) => { x.box.checked = on; });
    } }, 'Select all / none');

    const save = h('button.btn.primary', { onclick: async () => {
      const r = await busy(save, () => api.post('/docker/backups/config', { apps: picked(), dest: dest.value.trim(),
        keep: Number(keep.value), freq: freq.value, weekday: weekday.value, day: day.value, time: time.value }), 'Backup settings saved.');
      if (r) { bk.built = null; load(); }
    } }, 'Save');
    bk.runBtn = h('button.btn', { disabled: jobRunning(b.job), title: 'Back up the ticked apps now (saved or not)', onclick: (e) => {
      const list = picked();
      if (!list.length) { toast('Tick at least one app.', true); return; }
      backupNow(list, true, e.currentTarget);
    } }, 'Back up now');

    bk.status = h('div', backupStatus(b));
    bk.list = h('div', backupArchives(b));
    bk.built = true;
    const on = c.freq && c.freq !== 'off';
    clear(body,
      bk.status,
      h('div.card',
        h('div.card-head', h('h2', 'Backup settings'), on ? h('span.status.good', 'Scheduled') : h('span.status.idle', 'Manual only')),
        h('p.muted', { style: { marginTop: 0 } },
          'Each app is stopped with ', h('code', 'docker compose stop'), ', its whole folder (compose file, .env and bind-mounted data) is packed into a .tar.gz, and it is started again. Named volumes aren\'t included. A scheduled backup that fails sends a Gotify message when Gotify is set up under Features.'),
        on ? h('dl.kv', { style: { marginBottom: '14px' } },
          h('dt', 'Next run'), h('dd', when(b.nextRun)),
          h('dt', 'Last scheduled run'), h('dd', when(c.lastRun))) : null,
        h('h3', { style: { margin: '4px 0 8px' } }, 'Apps'),
        names.length ? h('div.stack', { style: { gap: '6px', marginBottom: '8px' } }, boxes.map((x) => x.el), h('div', all))
          : h('p.faint', 'No stacks in ~/docker yet.'),
        h('h3', { style: { margin: '16px 0 8px' } }, 'Schedule and storage'),
        h('div.form-grid',
          h('label.field', 'How often', freq), wdField, dayField, timeField,
          h('label.field', 'Keep last (per app)', keep)),
        h('div.form-grid', { style: { marginTop: '12px' } },
          h('label.field', { style: { gridColumn: '1 / -1' } }, 'Backup folder', dest)),
        h('p.faint', { style: { fontSize: '12.5px' } }, 'Leave empty for ', h('span.mono', '~/backups/docker'), '. Any absolute path works, like a mounted NAS or USB disk, as long as it\'s outside ~/docker.'),
        h('div.row', save, bk.runBtn)),
      bk.list);
  }

  // ---- Images and Storage -------------------------------------------------------------

  const table = (head, rows, empty) => rows.length
    ? h('div.table-wrap', h('table', h('thead', h('tr', head.map((x) => h('th', x)))), h('tbody', rows)))
    : h('div.empty', empty);

  function pruneBtn(what, msg) {
    const b = h('button.btn.small.danger', { onclick: async () => {
      if (!await confirmAction(`Prune ${what}`, msg, 'Prune')) return;
      await busy(b, () => api.post(`/docker/prune/${what}`));
      load();
    } }, 'Prune unused');
    return b;
  }

  function drawImages() {
    const used = new Set((data.containers || []).map((c) => c.ImageID));
    const rows = (data.images || []).filter((i) => match(i.RepoTags?.join(' '), i.Id))
      .sort((a, b) => b.Created - a.Created)
      .map((i) => h('tr',
        h('td', (i.RepoTags?.length && i.RepoTags[0] !== '<none>:<none>' ? i.RepoTags : ['<untagged>']).map((t) => h('div.mono', t)),
          h('div.sub.mono', i.Id.slice(7, 19))),
        h('td.num-cell', bytes(i.Size)),
        h('td.num-cell.muted', ago(i.Created)),
        h('td', used.has(i.Id) ? h('span.status.good', 'In use') : h('span.status.idle', 'Unused')),
        h('td.actions', menu([{ label: 'Remove', danger: true, onclick: async () => {
          if (!await confirmAction('Remove image', `Remove ${i.RepoTags?.[0] || i.Id.slice(7, 19)}?`, 'Remove')) return;
          try { await api.post('/docker/images/remove', { id: i.Id }); toast('Image removed.'); } catch (e) { toast(e.message, true); }
          load();
        } }], 'Image actions'))));
    clear(body, h('div.card',
      h('div.card-head', h('h2', `Images (${(data.images || []).length})`),
        pruneBtn('images', 'Remove every image no container uses? They are pulled again when needed.')),
      table(['Image', 'Size', 'Created', 'Status', ''], rows, 'No images.')));
  }

  function drawStorage() {
    const vols = (data.volumes || []).filter((v) => match(v.Name)).sort((a, b) => a.Name.localeCompare(b.Name))
      .map((v) => h('tr', h('td.mono', v.Name), h('td', v.Driver), h('td.mono.muted', v.Mountpoint)));
    const nets = (data.networks || []).filter((n) => match(n.Name, n.Driver)).sort((a, b) => a.Name.localeCompare(b.Name))
      .map((n) => h('tr', h('td.mono', n.Name), h('td', n.Driver), h('td', n.Scope), h('td.mono.muted', n.Id.slice(0, 12))));
    clear(body,
      h('div.card', h('div.card-head', h('h2', `Volumes (${(data.volumes || []).length})`),
        pruneBtn('volumes', 'Remove every volume no container uses? Data in them is deleted for good.')),
      table(['Name', 'Driver', 'Mount point'], vols, 'No volumes.')),
      h('div.card', h('div.card-head', h('h2', `Networks (${(data.networks || []).length})`),
        pruneBtn('networks', 'Remove all networks no container uses?')),
      table(['Name', 'Driver', 'Scope', 'ID'], nets, 'No networks.')));
  }

  // ---- Logs and stats -------------------------------------------------------------------

  async function showLogs(name, id) {
    const pre = h('pre.output', { style: { maxHeight: '60vh' } }, 'Loading…');
    const follow = h('input', { type: 'checkbox', checked: true });
    let t, open = true;
    const fetchLogs = async () => {
      try {
        const r = await api.get(`/docker/containers/${id}/logs?tail=500`);
        const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
        pre.textContent = r.logs || '(no output)';
        if (atBottom || pre.dataset.first !== '1') { pre.scrollTop = pre.scrollHeight; pre.dataset.first = '1'; }
      } catch (e) { pre.textContent = e.message; }
      if (open && follow.checked) t = setTimeout(fetchLogs, 3000);
    };
    follow.addEventListener('change', () => { if (follow.checked) fetchLogs(); else clearTimeout(t); });
    fetchLogs();
    await modal(`Logs: ${name}`, h('div.stack', { 'data-wide': '1' }, h('label.row.muted', follow, 'Refresh every 3 seconds'), pre),
      [{ label: 'Close', value: true, class: 'primary' }]);
    open = false; clearTimeout(t);
  }

  async function showStats(name, id) {
    const box = h('dl.kv', 'Loading…');
    let t, open = true;
    const tick = async () => {
      try {
        const s = await api.get(`/docker/containers/${id}/stats`);
        clear(box,
          h('dt', 'CPU'), h('dd', pct(s.cpuPercent)),
          h('dt', 'Memory'), h('dd', `${bytes(s.memUsed)}${s.memLimit ? ' of ' + bytes(s.memLimit) : ''}`),
          h('dt', 'Network in / out'), h('dd', `${bytes(s.netRx)} / ${bytes(s.netTx)}`));
      } catch (e) { clear(box, e.message); }
      if (open) t = setTimeout(tick, 3000);
    };
    tick();
    await modal(`Stats: ${name}`, box, [{ label: 'Close', value: true, class: 'primary' }]);
    open = false; clearTimeout(t);
  }

  drawNav();
  clear(body, h('div.card.empty', 'Loading…'));
  load();
  // Redraw when the node list (and so this node's address) arrives or changes.
  const onNodes = () => { if (!menuOpen() && !modalOpen() && data.containers) draw(); };
  document.addEventListener('nodes', onNodes);
  return () => { stopped = true; clearTimeout(timer); document.removeEventListener('nodes', onNodes); };
}
