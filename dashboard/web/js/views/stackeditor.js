import { h, clear, modal, toast, codeArea } from '../ui.js';
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

// Create (no name) or edit a stack in ~/docker/<name>/. Resolves true when something was saved.
export async function openStackEditor({ api, root, name, onDone }) {
  const create = !name;
  let files = { compose: BLANK, env: '' };
  if (!create) {
    try { files = await api.get(`/docker/stacks/${encodeURIComponent(name)}`); }
    catch (e) { toast(e.message, true); return false; }
  }

  const err = h('pre.output.error-output.hidden');
  const note = h('div.notice.warn.hidden', { style: { whiteSpace: 'pre-wrap' } });
  const compose = codeArea(files.compose, 20);
  const env = codeArea(files.env || '', 5);
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
      h('code', './data'), ' are inside the stack folder.' + (create ? '' : ' The previous version is kept as .bak.')),
    err);

  const save = (start) => async () => {
    err.classList.add('hidden');
    const stackName = create ? projectName(nameInput.value) : name;
    if (create && !stackName) { err.textContent = 'Enter a name.'; err.classList.remove('hidden'); return false; }
    try {
      const r = await api.post(create ? '/docker/stacks' : `/docker/stacks/${encodeURIComponent(name)}`,
        { name: stackName, compose: compose.value, env: env.value, start });
      toast(start ? `${stackName}: saved and ${create ? 'started' : 'recreated'}.` : `${stackName}: saved.`);
      if (start && r.output) modal(`Stack ${stackName}`, h('pre.output', r.output), [{ label: 'Close', value: true, class: 'primary' }]);
      onDone?.();
      return true;
    } catch (e) {
      err.textContent = e.message;
      err.classList.remove('hidden');
      err.scrollIntoView({ block: 'nearest' });
      return false;
    }
  };

  return modal(create ? 'New stack' : `Edit ${name}`, body, [
    { label: 'Cancel', value: false },
    { label: 'Save', onclick: save(false) },
    { label: create ? 'Save & start' : 'Save & recreate', class: 'primary', onclick: save(true) },
  ]);
}
