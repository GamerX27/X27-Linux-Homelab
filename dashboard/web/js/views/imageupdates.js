import { h, clear, when, busy, confirmAction, modal, icon } from '../ui.js';

const STATE = {
  update: ['warn', 'Update available'],
  uptodate: ['good', 'Up to date'],
  local: ['idle', 'Built locally'],
  unreachable: ['crit', "Couldn't check"],
};

// Container image updates, shown as a section of the Docker tab.
export function renderImageUpdates(el, { api, refreshNodes }) {
  let timer, stopped = false;
  clear(el, h('div.empty', 'Loading…'));

  async function load() {
    clearTimeout(timer);
    let u;
    try {
      u = await api.get('/docker/updates');
    } catch (e) {
      u = { error: e.message };
    }
    if (stopped) return;
    draw(u);
    // Poll faster while a check runs.
    timer = setTimeout(load, u.checking ? 3000 : 30000);
  }

  function showLog(title, text) {
    modal(title, h('pre.output', text || 'Done.'), [{ label: 'Close', value: true, class: 'primary' }]);
  }

  function draw(u) {
    const checkBtn = h('button.btn', { disabled: u.checking, onclick: () =>
      busy(checkBtn, () => api.post('/docker/updates/check')).then(() => setTimeout(load, 1500)) }, icon('refresh', 15), u.checking ? 'Checking…' : 'Check now');
    const images = u.images || [];
    const avail = images.filter((i) => i.state === 'update');
    const allBtn = avail.length > 1 ? h('button.btn.primary', { onclick: async () => {
      if (!await confirmAction('Update all', `Pull ${avail.length} images and recreate their compose projects?`, 'Update all', false)) return;
      await busy(allBtn, async () => {
        const logs = [];
        for (const i of avail) logs.push(`== ${i.image}\n` + (await api.post('/docker/updates/apply', { image: i.image })).output);
        showLog('Update all', logs.join('\n\n'));
      }, '');
      load();
      refreshNodes?.();
    } }, `Update all (${avail.length})`) : null;

    clear(el,
      h('div.row', { style: { marginBottom: '12px' } },
        u.checking ? h('span.status.idle', 'Checking registries…')
          : !u.checkedAt ? h('span.status.idle', 'Not checked yet')
          : avail.length ? h('span.status.warn', `${avail.length} update${avail.length > 1 ? 's' : ''} available`)
          : h('span.status.good', 'All images up to date'),
        h('span.faint', `· last checked ${when(u.checkedAt)} · compared by digest, nothing is pulled until you update`),
        h('span.spacer'), checkBtn, allBtn),
      u.error ? h('div.notice.warn', u.error) : null,
      images.length ? h('div.table-wrap', h('table',
        h('thead', h('tr', h('th', 'Image'), h('th', 'Used by'), h('th', 'Status'), h('th', ''))),
        h('tbody', images.map((i) => {
          const [cls, label] = STATE[i.state] || ['idle', i.state];
          const btn = i.state === 'update' ? h('button.btn.small.primary', { onclick: async () => {
            const r = await busy(btn, () => api.post('/docker/updates/apply', { image: i.image }), '');
            if (r) { showLog(`Updated ${i.image}`, r.output); load(); refreshNodes?.(); }
          } }, 'Update') : null;
          return h('tr',
            h('td.mono', i.image),
            h('td', i.containers.join(', '), i.projects?.length ? h('div.sub', 'compose: ' + i.projects.join(', ')) : null),
            h('td', h('span.status', { class: cls, title: i.error || '' }, label)),
            h('td.actions', btn));
        })))) : u.checkedAt ? h('div.empty', 'No containers.') : null);
  }

  load();
  return () => { stopped = true; clearTimeout(timer); };
}
