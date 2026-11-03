import { h, clear, bytes, busy, confirmAction, modal, toast, icon, when, menu, menuOpen, modalOpen } from '../ui.js';
import { csrfToken } from '../api.js';
import { WEEKDAYS } from './features.js';

const jobRunning = (j) => !!j && !j.finishedAt;

function ago(unix) {
  const d = (Date.now() / 1000 - unix);
  if (d < 3600) return `${Math.round(d / 60)} min ago`;
  if (d < 86400) return `${Math.round(d / 3600)} h ago`;
  return `${Math.round(d / 86400)} d ago`;
}

const table = (head, rows, empty) => rows.length
  ? h('div.table-wrap', h('table', h('thead', h('tr', head.map((x) => h('th', x)))), h('tbody', rows)))
  : h('div.empty', empty);

// Starts a backup of apps (the saved selection when empty); stopsSome warns first.
// Returns the job, or nothing when cancelled or refused.
export async function backupNow(api, apps, stopsSome = true, btn, here = false) {
  if (stopsSome && !await confirmAction('Back up now',
    `${apps.length ? apps.join(', ') : 'The selected apps'} ${apps.length === 1 ? 'is' : 'are'} stopped while the folder is packed, then started again.`, 'Back up', false)) return null;
  const run = () => api.post('/docker/backups/run', { apps });
  const r = btn ? await busy(btn, run, '') : await run().catch((e) => { toast(e.message, true); });
  if (r?.job) toast(here ? 'Backup started.' : 'Backup started. Progress shows under Backup.');
  return r?.job || null;
}

// The running or last backup or import's output, refreshed while it runs.
async function showBackupLog(api, title = 'Backup log') {
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
  await modal(title, h('div', { 'data-wide': '1' }, pre), [{ label: 'Close', value: true, class: 'primary' }]);
  open = false; clearTimeout(t);
}

const BACKUP_PHASE = { starting: 'starting', stopping: 'stopping', archiving: 'packing the folder', 'starting-again': 'starting again' };
const IMPORT_PHASE = { starting: 'starting', apps: 'unpacking apps', 'starting-app': 'starting', updates: 'updates and Gotify',
  shares: 'network shares', dashboard: 'dashboard settings', network: 'network and host' };

// What a finished job says, as a toast.
function jobToast(j) {
  if (j.kind === 'import') {
    toast(j.phase === 'failed' ? `Import finished with errors: ${j.error}.` : `${j.summary}.`, j.phase === 'failed');
  } else {
    toast(j.phase === 'failed' ? `Backup failed: ${j.error}.` : `Backup finished: ${j.done} app${j.done === 1 ? '' : 's'} backed up.`, j.phase === 'failed');
  }
}

// The running job's strip, or the last one's result; backups and imports share the runner.
function jobStatus(api, j) {
  const imp = j?.kind === 'import';
  const logBtn = j ? h('button.btn.small.ghost', { onclick: () => showBackupLog(api, imp ? 'Import log' : 'Backup log') }, 'Log') : null;
  if (jobRunning(j)) {
    const total = j.done + j.failed + (j.current ? 1 : 0) + (j.pending?.length || 0);
    const text = imp ? `Importing${j.current ? ` ${j.current}` : ''} · ${IMPORT_PHASE[j.phase] || 'working'}…`
      : `Backing up ${j.current || ''} · ${BACKUP_PHASE[j.phase] || 'working'}…${total > 1 ? ` (${j.done + j.failed + 1} of ${total})` : ''}`;
    return h('div.update-strip.running', { role: 'status' },
      h('span.status.warn.job-status', h('span.spinner.small', { 'aria-hidden': 'true' }), text),
      h('span.spacer'), logBtn);
  }
  if (!j) return null;
  const failed = j.phase === 'failed';
  const text = imp ? (failed ? `Last import: ${j.summary || 'nothing imported'}; ${j.error}` : `Last import: ${j.summary}`)
    : failed ? `Last backup failed: ${j.error}` : `Last backup finished: ${j.done} app${j.done === 1 ? '' : 's'}`;
  return h('div.notice.update-result', { class: failed ? 'failed' : 'done' },
    h('span', { class: `status ${failed ? 'crit' : 'good'}` }, text),
    h('span.faint', `· ${when(j.finishedAt).replace(/^.*\((.*)\)$/, '$1')}${j.kind === 'scheduled' ? ' · scheduled' : ''}`),
    h('span.spacer'), logBtn);
}

// Follows the shared job: polls every 2 s while one runs (not under an open menu or
// dialog) and toasts when the one it saw running ends.
function jobWatcher(api, onData) {
  let timer, stopped = false, watching = null;
  async function load() {
    clearTimeout(timer);
    let d;
    try {
      d = await api.get('/docker/backups');
    } catch (e) {
      if (!stopped) onData(null, e);
      return;
    }
    if (stopped) return;
    const j = d.backups.job;
    if (jobRunning(j)) watching = j.id;
    else if (j && j.id === watching) { watching = null; jobToast(j); }
    onData(d);
    if (jobRunning(j)) timer = setTimeout(function again() {
      if (menuOpen() || modalOpen()) timer = setTimeout(again, 2000); else load();
    }, 2000);
  }
  return { load, watch: (id) => { watching = id; }, stop: () => { stopped = true; clearTimeout(timer); } };
}

// ---- Backups: archives of stack folders, on a schedule or now ---------------------------

export function renderBackups(el, { api }) {
  const bk = {}; // the form, built once so polling doesn't wipe edits
  let data = null;
  const body = h('div.stack');
  const filter = h('input.filter-input', { type: 'search', placeholder: 'Filter…', 'aria-label': 'Filter', oninput: () => draw() });
  const refreshBtn = h('button.btn', { onclick: () => w.load(), title: 'Refresh', 'aria-label': 'Refresh' }, icon('refresh', 15));
  clear(el, h('div.stack', h('div.row', h('span.spacer'), filter, refreshBtn), body));
  const w = jobWatcher(api, (d, err) => {
    if (err) { clear(body, h('div.notice.warn', err.message)); return; }
    data = d;
    draw();
  });

  const match = (...fields) => {
    const q = filter.value.trim().toLowerCase();
    return !q || fields.some((f) => String(f ?? '').toLowerCase().includes(q));
  };

  async function run(apps, stopsSome, btn) {
    const j = await backupNow(api, apps, stopsSome, btn, true);
    if (j) w.watch(j.id);
    w.load();
  }

  function backupArchives(b) {
    const rows = (b.archives || []).filter((a) => match(a.app, a.file)).map((a) => h('tr',
      h('td', h('strong', a.app)), h('td.mono', a.file),
      h('td.num-cell', bytes(a.size)), h('td.num-cell.muted', ago(a.time)),
      h('td.actions', menu([{ label: 'Delete', danger: true, onclick: async () => {
        if (!await confirmAction('Delete backup', `Delete ${a.file}? This can't be undone.`, 'Delete')) return;
        try { await api.post('/docker/backups/delete', { app: a.app, file: a.file }); toast('Backup deleted.'); } catch (e) { toast(e.message, true); }
        w.load();
      } }], 'Backup actions'))));
    return h('div.card',
      h('div.card-head', h('h2', `Backups (${(b.archives || []).length})`), h('span.faint.mono', b.dest)),
      table(['App', 'File', 'Size', 'Taken', ''], rows, 'No backups yet.'));
  }

  function draw() {
    if (!data) return;
    const { backups: b, apps } = data;
    // While polling, refresh only the status and the list; the form keeps what's typed.
    if (bk.built && bk.status.isConnected) {
      clear(bk.status, jobStatus(api, b.job));
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
      if (r) { bk.built = null; w.load(); }
    } }, 'Save');
    bk.runBtn = h('button.btn', { disabled: jobRunning(b.job), title: 'Back up the ticked apps now (saved or not)', onclick: (e) => {
      const list = picked();
      if (!list.length) { toast('Tick at least one app.', true); return; }
      run(list, true, e.currentTarget);
    } }, 'Back up now');

    bk.status = h('div', jobStatus(api, b.job));
    bk.list = h('div', backupArchives(b));
    bk.built = true;
    const on = c.freq && c.freq !== 'off';
    clear(body,
      bk.status,
      h('div.card',
        h('div.card-head', h('h2', 'Backup settings'), on ? h('span.status.good', 'Scheduled') : h('span.status.idle', 'Manual only')),
        h('p.muted', { style: { marginTop: 0 } },
          'Each app is stopped with ', h('code', 'docker compose stop'), ', its whole folder (compose file, .env and bind-mounted data) is packed into a .tar.gz, and it is started again. Named volumes aren\'t included. A scheduled backup that fails sends a Gotify message when Gotify is set up under System → Features.'),
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

  clear(body, h('div.card.empty', 'Loading…'));
  w.load();
  return () => w.stop();
}

// ---- Export & import: every app's config and the server's own settings in one file ------

const PARTS = [
  ['dashboard', 'Dashboard settings', 'Port, compose presets, the backup schedule, paired nodes and API tokens.'],
  ['shares', 'Network shares', 'NFS and SMB mounts, with their SMB passwords.'],
  ['updates', 'Updates & Gotify', 'The automatic update schedule, Gotify server and token, and which services are on.'],
  ['network', 'Network & host', 'Hostname, time zone, NTP, and each connection\'s IPv4 address and DNS.'],
];

export function renderExport(el, { api, node }) {
  const status = h('div');
  const exportCard = h('div.card');
  const importCard = h('div.card');
  clear(el, h('div.stack', status, exportCard, importCard));
  let job = null;
  const w = jobWatcher(api, (d, err) => {
    if (err) { clear(status, h('div.notice.warn', err.message)); return; }
    job = d.backups.job;
    clear(status, job?.kind === 'import' || jobRunning(job) ? jobStatus(api, job) : null);
    if (job?.kind === 'import' && !jobRunning(job)) {
      if (job.waiting) {
        status.append(h('div.notice.warn', { style: { marginTop: '8px' } }, 'A network change is waiting: keep it under ',
          h('a', { href: `#/n/${encodeURIComponent(node.id)}/system/settings` }, 'System → Settings'), ' within 2 minutes, or it\'s undone.'));
      }
      if (job.port) {
        const url = `${location.protocol}//${location.hostname}:${job.port}/`;
        status.append(h('div.notice.warn', { style: { marginTop: '8px' } }, `The dashboard restarts on port ${job.port}: `, h('a', { href: url }, url)));
      }
    }
  });

  // ---- Export
  const partBoxes = PARTS.map(([id, label, hint]) => {
    const box = h('input', { type: 'checkbox', checked: true });
    return { id, box, el: h('label.check-row', box, h('span', h('strong', label), h('span.faint', ` · ${hint}`))) };
  });
  const dataList = h('div.stack', { style: { gap: '6px' } }, h('p.faint', 'Loading…'));
  const stop = h('input', { type: 'checkbox', checked: true });
  const stopRow = h('label.check-row.hidden', stop, h('span', 'Stop running apps while their data is copied, then start them again'));
  const total = h('p.faint', { style: { margin: '8px 0 0' } });
  let dataBoxes = [];
  const syncData = () => {
    const on = dataBoxes.filter((x) => x.box.checked);
    stopRow.classList.toggle('hidden', !on.some((x) => x.a.running));
    total.textContent = on.length ? `Data: ${bytes(on.reduce((n, x) => n + x.a.size, 0))}, plus the config files.` : 'Config files only: a few KB.';
  };
  api.get('/docker/backups/data-apps').then((r) => {
    dataBoxes = r.apps.map((a) => {
      const box = h('input', { type: 'checkbox', onchange: syncData });
      return { a, box, el: h('label.check-row', box, h('span', h('strong', a.name), h('span.faint', ` · ${bytes(a.size)}`),
        a.running ? h('span.status.good', { style: { marginLeft: '8px' } }, 'running') : null)) };
    });
    clear(dataList, dataBoxes.length ? dataBoxes.map((x) => x.el) : h('p.faint', 'No app holds data beyond its config files.'));
    syncData();
  }).catch((e) => clear(dataList, h('p.status.crit', e.message)));
  const download = h('button.btn.primary', { onclick: () => {
    if (jobRunning(job)) { toast('Wait for the running backup or import to finish.', true); return; }
    const params = new URLSearchParams();
    dataBoxes.filter((x) => x.box.checked).forEach((x) => params.append('data', x.a.name));
    if (stop.checked) params.set('stop', '1');
    params.set('parts', partBoxes.filter((x) => x.box.checked).map((x) => x.id).join(','));
    h('a', { href: api.url(`/backup/export?${params}`), download: '' }).click();
    toast(dataBoxes.some((x) => x.box.checked) ? 'Export started. With app data it can take a while.' : 'Export started.');
  } }, icon('download', 15), 'Download');
  clear(exportCard,
    h('div.card-head', h('h2', 'Export')),
    h('p.muted', { style: { marginTop: 0 } }, 'One .tar.gz with every app\'s compose file, .env and the other files at the top of its folder, plus the server settings ticked below. Import it here or on a new server to set it up the same way.'),
    h('h3', { style: { margin: '4px 0 8px' } }, 'Include'),
    h('div.stack', { style: { gap: '6px' } },
      h('label.check-row', h('input', { type: 'checkbox', checked: true, disabled: true }),
        h('span', h('strong', 'Apps'), h('span.faint', ' · every app in ~/docker: compose file, .env and the other small files at the top'))),
      partBoxes.map((x) => x.el)),
    h('h3', { style: { margin: '16px 0 8px' } }, 'App data'),
    h('p.faint', { style: { marginTop: 0 } }, 'Tick apps to include their whole folder (databases, bind-mounted config). Named volumes aren\'t included.'),
    dataList, total, stopRow,
    h('div.notice.warn', { style: { margin: '14px 0' } }, 'The file holds secrets: the apps\' .env files, SMB passwords, the Gotify token and the paired nodes\' tokens. Keep it somewhere safe.'),
    h('div.row', download));

  // ---- Import
  const fileInput = h('input', { type: 'file', accept: '.gz,.tgz,application/gzip' });
  const progress = h('span.muted');
  const preview = h('div');
  const upload = h('button.btn', { onclick: () => send() }, icon('upload', 15), 'Upload');
  function send() {
    const f = fileInput.files[0];
    if (!f) { toast('Pick an export file first.', true); return; }
    upload.disabled = true; upload.classList.add('busy');
    clear(preview);
    const xhr = new XMLHttpRequest();
    xhr.open('POST', api.url('/backup/import'));
    xhr.setRequestHeader('X-CSRF-Token', csrfToken());
    xhr.setRequestHeader('Content-Type', 'application/gzip');
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) progress.textContent = `Uploading ${Math.round(e.loaded / e.total * 100)}% of ${bytes(e.total)}…`;
    };
    const done = () => { upload.disabled = false; upload.classList.remove('busy'); };
    xhr.onerror = () => { done(); progress.textContent = ''; toast('The upload failed.', true); };
    xhr.onload = () => {
      done();
      progress.textContent = '';
      let r = {};
      try { r = JSON.parse(xhr.responseText); } catch {}
      if (xhr.status !== 200) { toast(r.error || `Upload failed (${xhr.status})`, true); return; }
      drawPreview(r);
    };
    xhr.send(f);
  }

  function drawPreview(pv) {
    const replace = h('input', { type: 'checkbox' });
    const start = h('input', { type: 'checkbox', checked: true });
    const apps = pv.apps.map((a) => {
      const box = h('input', { type: 'checkbox', checked: true });
      const note = h('span.faint');
      const el = h('label.check-row', box, h('span', h('strong', a.name),
        h('span.faint', ` · ${a.data ? `with data, ${bytes(a.size)}` : 'config'}`), note));
      return { a, box, el, note };
    });
    const syncApps = () => apps.forEach(({ a, note }) => {
      note.textContent = !a.exists ? '' : !replace.checked ? ' · already here, will be skipped'
        : a.running ? ' · running: stopped, then its files are replaced' : ' · already here: its files are replaced';
      note.className = a.exists && replace.checked ? 'status warn' : 'faint';
      if (a.exists && replace.checked) note.style.marginLeft = '8px'; else note.style.marginLeft = '';
    });
    replace.addEventListener('change', syncApps);
    syncApps();
    const netWarn = h('div.notice.warn.hidden', { style: { marginTop: '6px' } },
      'The hostname, time zone and IPv4 settings are changed to the exported ones. An IPv4 change is undone after 2 minutes unless you keep it under System → Settings, so a wrong address can\'t lock you out.');
    const parts = PARTS.filter(([id]) => pv.parts.includes(id)).map(([id, label]) => {
      const box = h('input', { type: 'checkbox', checked: id !== 'network' });
      if (id === 'network') box.addEventListener('change', () => netWarn.classList.toggle('hidden', !box.checked));
      return { id, label, box, el: h('label.check-row', box, h('span', h('strong', label), pv.details[id] ? h('span.faint', ` · ${pv.details[id]}`) : null)) };
    });
    const go = h('button.btn.primary', { onclick: async () => {
      const pickedApps = apps.filter((x) => x.box.checked).map((x) => x.a.name);
      const pickedParts = parts.filter((x) => x.box.checked);
      if (!pickedApps.length && !pickedParts.length) { toast('Tick at least one app or part.', true); return; }
      const skipped = replace.checked ? [] : apps.filter((x) => x.box.checked && x.a.exists).map((x) => x.a.name);
      const msg = [pickedApps.length ? `Apps: ${pickedApps.join(', ')}.` : 'No apps.',
        skipped.length ? `Already here and skipped: ${skipped.join(', ')}.` : '',
        pickedParts.length ? `Server settings: ${pickedParts.map((x) => x.label).join(', ')}, replacing the ones on this server.` : '']
        .filter(Boolean).join(' ');
      if (!await confirmAction('Import', msg, 'Import', false)) return;
      const r = await busy(go, () => api.post('/backup/import/apply', { id: pv.id, apps: pickedApps, replace: replace.checked,
        start: start.checked, parts: pickedParts.map((x) => x.id) }), '');
      if (r?.job) {
        toast('Import started. Progress shows above.');
        w.watch(r.job.id);
        clear(preview);
        fileInput.value = '';
        w.load();
      }
    } }, 'Import');
    clear(preview,
      h('div.notice', { style: { margin: '14px 0' } }, 'Export of ', h('strong', pv.host || 'unknown host'),
        pv.taken ? `, taken ${when(pv.taken).replace(/^.*\((.*)\)$/, '$1')}` : '', '.'),
      h('h3', { style: { margin: '4px 0 8px' } }, `Apps (${apps.length})`),
      apps.length ? h('div.stack', { style: { gap: '6px' } }, apps.map((x) => x.el)) : h('p.faint', 'No apps in this file.'),
      apps.length ? h('div.stack', { style: { gap: '6px', marginTop: '10px' } },
        h('label.check-row', replace, h('span', 'Replace apps that are already here', h('span.faint', ' · files in the export overwrite the ones there; other files are kept'))),
        h('label.check-row', start, h('span', 'Start apps after', h('span.faint', ' · docker compose up -d')))) : null,
      parts.length ? [h('h3', { style: { margin: '16px 0 8px' } }, 'Server settings'),
        h('div.stack', { style: { gap: '6px' } }, parts.map((x) => x.el)), netWarn] : null,
      h('div.row', { style: { marginTop: '14px' } }, go,
        h('button.btn.ghost', { onclick: () => { clear(preview); fileInput.value = ''; } }, 'Cancel')));
  }

  clear(importCard,
    h('div.card-head', h('h2', 'Import')),
    h('p.muted', { style: { marginTop: 0 } }, 'Upload an export to restore its apps into ~/docker and pick which server settings to apply. Nothing changes until you press Import.'),
    h('div.row', { style: { flexWrap: 'wrap' } }, fileInput, upload, progress),
    preview);

  w.load();
  return () => w.stop();
}
