import { h, clear, modal, toast, codeArea, showLiveLog } from '../ui.js';
import * as mainAPI from '../api.js';

const BLANK = `services:
  app:
    image: nginx:latest
    container_name: app
    restart: unless-stopped
    ports:
      - "8080:80"
    volumes:
      - ./data:/data
`;

// Same rule as docker compose for a folder's project name.
export const projectName = (s) => s.toLowerCase().replace(/[^a-z0-9_-]/g, '').replace(/^[-_]+/, '');

// ---- Background stack actions (POST …/{action} with background: true → job) -------------

// Per action: while running, when done, and as a menu label.
export const STACK_ACTION = {
  start: { ing: 'Starting', done: 'started', label: 'Start' },
  resume: { ing: 'Starting', done: 'started', label: 'Start' },
  stop: { ing: 'Stopping', done: 'stopped', label: 'Stop' },
  restart: { ing: 'Restarting', done: 'restarted', label: 'Restart' },
  recreate: { ing: 'Pulling & recreating', done: 'pulled and recreated', label: 'Pull & recreate' },
  apply: { ing: 'Recreating', done: 'recreated', label: 'Recreate' },
  remove: { ing: 'Removing', done: 'removed', label: 'Remove' },
};
const actionOf = (j) => STACK_ACTION[j.action] || { ing: 'Working', done: 'done', label: j.action };

export const stackJobRunning = (j) => !!j && !j.finishedAt;

// "Starting…" / "Checking containers…" while a job runs.
export const stackJobLine = (j) => j.phase === 'checking' ? 'Checking containers…' : `${actionOf(j).ing}…`;

// How a finished job ended, as one sentence.
export function stackJobResult(j) {
  const a = actionOf(j);
  if (j.phase === 'failed') return `${a.label} failed: ${j.error}`;
  if (j.warning) return `${a.label[0].toUpperCase() + a.done.slice(1)}, but ${j.warning}`;
  return `${j.stack} ${a.done}.`;
}

// The status line for a job: a spinner while it runs, then how it ended.
export function stackJobStatus(j) {
  if (!j) return null;
  if (stackJobRunning(j)) return h('span.status.warn.job-status', h('span.spinner.small', { 'aria-hidden': 'true' }), stackJobLine(j));
  const cls = j.phase === 'failed' ? 'crit' : j.warning ? 'warn' : 'good';
  return h('span', { class: `status ${cls}` }, stackJobResult(j));
}

// The live output of a stack's running or last action.
export function showStackLog(api, name) {
  return showLiveLog(`Log: ${name}`, async () => {
    const r = await api.get(`/docker/stacks/${encodeURIComponent(name)}/log`);
    return { text: r.text, running: stackJobRunning(r.job), status: stackJobStatus(r.job) };
  }, { every: 1500, empty: 'Nothing has run on this stack yet.' });
}

// Create (no name) or edit a stack in ~/docker/<name>/. Resolves true when something was saved.
// onDone(job) is called after each save; job is the started action for Save & start.
export async function openStackEditor({ api, root, name, onDone }) {
  let create = !name;
  let files = { compose: BLANK, env: '' };
  if (!create) {
    try { files = await api.get(`/docker/stacks/${encodeURIComponent(name)}`); }
    catch (e) { toast(e.message, true); return false; }
  }

  const err = h('pre.output.error-output.hidden');
  const note = h('div.notice.warn.hidden', { style: { whiteSpace: 'pre-wrap' } });
  const compose = codeArea(files.compose, 20);
  const env = codeArea(files.env || '', 5);
  // What's saved; the dialog asks before closing with anything else in the editors.
  let saved = { compose: compose.value, env: env.value };
  const dirty = () => compose.value !== saved.compose || env.value !== saved.env || (create && nameInput.value !== '');
  const envBox = h('details', { open: !!files.env }, h('summary.muted', '.env (optional, kept private to the owner)'), env);
  const nameInput = h('input', { type: 'text', placeholder: 'e.g. jellyfin', spellcheck: false, autocapitalize: 'off' });
  const pathHint = h('div.faint.mono', { style: { fontSize: '12px' } });
  const showPath = () => { pathHint.textContent = `${root}/${projectName(nameInput.value) || '<name>'}/compose.yml`; };
  nameInput.addEventListener('input', showPath);
  showPath();

  // Presets from the Codeberg repository, loaded by the main node.
  const presetSel = h('select', h('option', { value: '' }, 'Blank'));
  const presetInfo = h('span.faint', { style: { fontSize: '12px' } }, 'Loading presets…');
  if (create) {
    mainAPI.get('/api/presets').then((r) => {
      presetSel.append(...r.presets.map((p) => h('option', { value: p.name }, p.name + (p.hasNote ? ' (has notes)' : ''))));
      clear(presetInfo, 'From ', h('a', { href: r.source, target: '_blank', rel: 'noopener' }, r.source.replace(/^https:\/\//, '')));
    }).catch((e) => { presetInfo.textContent = `Presets unavailable: ${e.message}`; });
    presetSel.addEventListener('change', async () => {
      note.classList.add('hidden');
      if (!presetSel.value) { compose.value = BLANK; return; }
      presetSel.disabled = true;
      try {
        const f = await mainAPI.get(`/api/presets/${encodeURIComponent(presetSel.value)}`);
        compose.value = f.compose;
        if (!nameInput.value || nameInput.dataset.auto === '1') {
          nameInput.value = projectName(f.name);
          nameInput.dataset.auto = '1';
          showPath();
        }
        if (f.note?.trim()) {
          note.textContent = `Notes for ${f.name}:\n${f.note.trim()}`;
          note.classList.remove('hidden');
        }
      } catch (e) { toast(e.message, true); }
      presetSel.disabled = false;
    });
    nameInput.addEventListener('input', () => { nameInput.dataset.auto = ''; });
  }

  const body = h('div.stack', { 'data-wide': '1' },
    create ? h('div.form-grid',
      h('label.field', 'Start from preset', presetSel, presetInfo),
      h('label.field', 'Name', nameInput, pathHint)) : h('div.faint.mono', files.file),
    note,
    h('label.field', create ? 'compose.yml' : files.file.split('/').pop(), compose),
    envBox,
    h('p.faint', { style: { margin: 0, fontSize: '12.5px' } },
      'Checked with ', h('code', 'docker compose config'), ' before saving. Relative paths like ',
      h('code', './data'), ' are inside the stack folder.' + (create ? '' : ' The previous version is kept as .bak. Save & recreate uses the images already on the node; Pull & recreate gets newer ones.'),
      ' Ctrl+S saves without closing.'),
    err);

  const save = (start) => async () => {
    err.classList.add('hidden');
    const stackName = create ? projectName(nameInput.value) : name;
    if (create && !stackName) { err.textContent = 'Enter a name.'; err.classList.remove('hidden'); return false; }
    const wasCreate = create;
    try {
      const r = await api.post(wasCreate ? '/docker/stacks' : `/docker/stacks/${encodeURIComponent(name)}`,
        { name: stackName, compose: compose.value, env: env.value, start, background: true });
      saved = { compose: compose.value, env: env.value };
      // Saved once, a new stack is edited from here on (also when starting it failed).
      if (wasCreate) { create = false; name = stackName; }
      if (r.job) {
        // Started in the background: its progress shows on the stack's row and in the log.
        toast(`${stackName}: saved. ${stackJobLine(r.job)}`);
        onDone?.(r.job);
        return { job: r.job };
      }
      if (r.startError) {
        // The files were saved, but it couldn't start (for example something else is running on it).
        err.textContent = `Saved, but couldn't ${wasCreate ? 'start' : 'recreate'} ${stackName}: ${r.startError}`;
        err.classList.remove('hidden');
        onDone?.();
        return false;
      }
      // A node that runs it while answering (older versions).
      toast(start ? `${stackName}: saved and ${wasCreate ? 'started' : 'recreated'}.` : `${stackName}: saved.`);
      if (start && r.output) modal(`Stack ${stackName}`, h('pre.output', r.output), [{ label: 'Close', value: true, class: 'primary' }]);
      onDone?.();
      return true;
    } catch (e) {
      // Nothing was saved: a failed start after a good save comes back as startError.
      err.textContent = e.message;
      err.classList.remove('hidden');
      err.scrollIntoView({ block: 'nearest' });
      return false;
    }
  };

  for (const ta of [compose, env]) {
    ta.addEventListener('keydown', (e) => {
      if (e.key === 's' && (e.ctrlKey || e.metaKey) && !e.altKey) { e.preventDefault(); save(false)(); }
    });
  }

  const v = await modal(create ? 'New stack' : `Edit ${name}`, body, [
    { label: 'Cancel', value: false },
    { label: 'Save', onclick: save(false) },
    { label: create ? 'Save & start' : 'Save & recreate', class: 'primary', onclick: save(true) },
  ], { dirty });
  // Follow what was started; the dialog can be closed and the row keeps showing it.
  if (v?.job) showStackLog(api, v.job.stack);
  return !!v;
}
