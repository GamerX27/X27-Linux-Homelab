import { h, clear, when, busy, confirmAction, modal, icon } from '../ui.js';

const STATE = {
  update: ['warn', 'Update available'],
  uptodate: ['good', 'Up to date'],
  local: ['idle', 'Built locally'],
  unreachable: ['crit', "Couldn't check"],
};

export function renderUpdates(el, { api, node, refreshNodes }) {
  let timer, stopped = false;
  const osCard = h('div.card', h('div.empty', 'Loading…'));
  const ctCard = h('div.card', h('div.empty', 'Loading…'));
  clear(el, h('div.stack', osCard, ctCard));

  async function load() {
    const [os, ct] = await Promise.allSettled([api.get('/os'), api.get('/docker/updates')]);
    if (stopped) return;
    drawOS(os.status === 'fulfilled' ? os.value : { error: os.reason.message });
    drawCT(ct.status === 'fulfilled' ? ct.value : { error: ct.reason.message });
    // Poll faster while a check or update runs.
    const running = os.value?.check?.checking || os.value?.updating || ct.value?.checking;
    timer = setTimeout(load, running ? 3000 : 30000);
  }

  function drawOS(s) {
    const head = h('div.card-head', h('h2', 'Operating system image'));
    if (!s.supported) {
      clear(osCard, head, h('div.notice', s.error || 'Not available on this node.'));
      return;
    }
    const c = s.check || {};
    let status;
    if (s.updating) status = h('span.status.warn', 'Updating… the node reboots when the update is installed');
    else if (c.checking) status = h('span.status.idle', 'Checking…');
    else if (c.error) status = h('span.status.crit', 'Check failed');
    else if (s.staged) status = h('span.status.warn', 'Update downloaded, reboot to apply');
    else if (c.available) status = h('span.status.warn', `Update available${c.version ? ': ' + c.version : ''}`);
    else if (c.checkedAt) status = h('span.status.good', 'Up to date');
    else status = h('span.status.idle', 'Not checked yet');

    const checkBtn = h('button.btn', { disabled: c.checking, onclick: () =>
      busy(checkBtn, () => api.post('/os/check')).then(() => setTimeout(load, 1500)) }, icon('refresh', 15), 'Check now');
    const updateBtn = h('button.btn.primary', { disabled: s.updating || !c.available, onclick: async () => {
      if (!await confirmAction('Install update', `${node.name} downloads the new image and reboots. Containers restart with it.`, 'Update and reboot', false)) return;
      await busy(updateBtn, () => api.post('/os/update'));
      load();
    } }, 'Update now');
    const rebootBtn = h('button.btn', { onclick: async () => {
      if (!await confirmAction('Reboot', `Reboot ${node.name} now?`, 'Reboot')) return;
      busy(rebootBtn, () => api.post('/power/reboot'));
    } }, s.staged ? 'Reboot to apply' : 'Reboot');
    const rollbackBtn = s.canRollback ? h('button.btn.danger', { onclick: async () => {
      if (!await confirmAction('Roll back', `Boot ${node.name} into the previous image? It reboots right away.`, 'Roll back and reboot')) return;
      busy(rollbackBtn, () => api.post('/os/rollback'));
    } }, 'Roll back') : null;

    head.append(h('div.row', checkBtn, updateBtn, rebootBtn, rollbackBtn));
    clear(osCard, head,
      h('div.row', { style: { marginBottom: '12px' } }, status,
        h('span.faint', `· last checked ${when(c.checkedAt)} · checks every 6 hours`)),
      c.error ? h('pre.output', { style: { marginBottom: '12px' } }, c.error) : null,
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

  function drawCT(u) {
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
    } }, `Update all (${avail.length})`) : null;

    clear(ctCard,
      h('div.card-head', h('h2', 'Container images'), h('div.row', checkBtn, allBtn)),
      h('div.row', { style: { marginBottom: '12px' } },
        u.checking ? h('span.status.idle', 'Checking registries…')
          : !u.checkedAt ? h('span.status.idle', 'Not checked yet')
          : avail.length ? h('span.status.warn', `${avail.length} update${avail.length > 1 ? 's' : ''} available`)
          : h('span.status.good', 'All images up to date'),
        h('span.faint', `· last checked ${when(u.checkedAt)} · compared by digest, nothing is pulled until you update`)),
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
        })))) : u.checkedAt ? h('div.faint', 'No containers.') : null);
  }

  function showLog(title, text) {
    modal(title, h('pre.output', text || 'Done.'), [{ label: 'Close', value: true, class: 'primary' }]);
  }

  load().catch(() => {});
  return () => { stopped = true; clearTimeout(timer); };
}
