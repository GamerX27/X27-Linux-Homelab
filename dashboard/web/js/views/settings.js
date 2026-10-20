import { h, clear, busy, switchEl, confirmAction, toast } from '../ui.js';
import * as mainApi from '../api.js';

export function renderSettings(el, { api, node, refreshNodes }) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  clear(el, content);

  async function load() {
    try {
      const [s, zones, d] = await Promise.all([api.get('/settings'), api.get('/settings/timezones'), api.get('/dashboard').catch(() => null)]);
      draw(s, zones || [], d);
    } catch (e) {
      clear(content, h('div.notice.warn', e.message));
    }
  }

  // Waits for the main node's dashboard on its new port, then moves the page there.
  async function follow(port) {
    const url = `${location.protocol}//${location.hostname}:${port}/`;
    const note = h('div.notice.warn', `Restarting on port ${port}… This page moves to `, h('a', { href: url }, url), ' when it\'s back.');
    content.prepend(note);
    for (let i = 0; i < 30; i++) {
      await new Promise((r) => setTimeout(r, 2000));
      try { await fetch(url, { mode: 'no-cors', cache: 'no-store' }); location.href = url; return; } catch { /* not up yet */ }
    }
    note.textContent = `The dashboard didn't answer on port ${port}. Check it with: dashboard status`;
  }

  function dashboardCard(d) {
    if (!d) return null;
    const port = h('input', { type: 'number', min: 1, max: 65535, value: d.port, style: { width: '120px' } });
    const savePort = h('button.btn', { disabled: d.dev, onclick: async () => {
      const p = Number(port.value);
      if (p === d.port) return;
      if (!await confirmAction('Change port', `The dashboard on ${node.name} restarts on port ${p}.`
        + (node.local ? ' This page moves to the new address.' : ' The main node reaches it there from now on.'), 'Change port', false)) return;
      const r = await busy(savePort, () => node.local ? api.post('/dashboard/port', { port: p }) : mainApi.post(`/api/nodes/${node.id}/port`, { port: p }));
      if (r && node.local) follow(p);
      else if (r) setTimeout(() => refreshNodes?.(), 4000);
    } }, 'Save');
    const copyFp = h('button.btn.ghost.small', { onclick: async () => {
      try { await navigator.clipboard.writeText(d.fingerprint); toast('Fingerprint copied.'); } catch { toast('Couldn\'t copy', true); }
    } }, 'Copy');
    let presets = null;
    if (d.mode === 'main') {
      const repo = h('input', { type: 'url', value: d.presets || '', placeholder: d.defaultPresets });
      const saveRepo = h('button.btn', { onclick: async () => {
        if (await busy(saveRepo, () => api.post('/dashboard/presets', { url: repo.value.trim() }))) setTimeout(load, 3000);
      } }, 'Save');
      presets = h('div.stack', { style: { gap: '6px', marginTop: '14px' } },
        h('label.field', 'Compose presets repository', h('div.input-row', repo, saveRepo)),
        h('p.faint', { style: { margin: 0, fontSize: '12.5px' } }, 'Where New app finds its presets: a Forgejo/Gitea repository with ',
          h('code', 'Composes/<name>/compose.yml'), '. Empty goes back to the default.'));
    }
    return h('div.card', h('div.card-head', h('h2', 'Dashboard'), h('span.badge', d.mode === 'main' ? 'main node' : 'node')),
      h('dl.kv', { style: { marginBottom: '14px' } },
        h('dt', 'Version'), h('dd.mono', d.version),
        h('dt', 'Addresses'), h('dd', (d.addresses || []).map((ip) => h('div.mono', `https://${ip.includes(':') ? `[${ip}]` : ip}:${d.port}`))),
        h('dt', 'Certificate'), h('dd.token-row', h('code', { style: { overflowWrap: 'anywhere' } }, d.fingerprint), copyFp),
        d.mode === 'node' ? [h('dt', 'Paired'), h('dd', d.paired ? 'Yes, with this main node' : 'No')] : null),
      h('div.row', h('label.field', 'Port', port), h('div', { style: { alignSelf: 'flex-end' } }, savePort)),
      d.dev ? h('p.faint', { style: { margin: '6px 0 0' } }, 'The dev server takes its port from --port.') : null,
      presets,
      h('p.faint', { style: { marginBottom: 0 } }, 'Same as ', h('code', 'dashboard status'), ' and ', h('code', 'dashboard port'),
        ' on the node. Pairing and turning the dashboard off are done there.'));
  }

  function draw(s, zones, d) {
    const hostname = h('input', { type: 'text', value: s.hostname, spellcheck: false });
    const saveHost = h('button.btn.primary', { onclick: async () => {
      if (await busy(saveHost, () => api.post('/settings/hostname', { hostname: hostname.value.trim() }), 'Hostname changed.')) {
        refreshNodes?.();
        load();
      }
    } }, 'Save');

    const tz = h('select', zones.map((z) => h('option', { value: z }, z)));
    tz.value = s.timezone;
    const saveTz = h('button.btn.primary', { onclick: async () => {
      if (await busy(saveTz, () => api.post('/settings/timezone', { timezone: tz.value }), 'Time zone changed.')) load();
    } }, 'Save');

    const power = (label, action, msg) => {
      const b = h('button.btn.danger', { onclick: async () => {
        if (!await confirmAction(label, msg, label)) return;
        busy(b, () => api.post(`/power/${action}`));
      } }, label);
      return b;
    };

    clear(content,
      h('div.grid.two',
        h('div.card', h('h2', 'Hostname'),
          h('div.form-grid', h('label.field', 'Hostname', hostname), h('div.row', saveHost)),
          h('p.faint', { style: { marginBottom: 0 } }, 'Letters, digits and dashes. Takes effect at once; mDNS/DHCP names may take a moment to follow.')),
        h('div.card', h('h2', 'Date and time'),
          h('dl.kv', { style: { marginBottom: '12px' } },
            h('dt', 'Node time'), h('dd', new Date(s.time).toLocaleString([], { dateStyle: 'medium', timeStyle: 'medium' })),
            h('dt', 'Synchronized'), h('dd', s.ntpSynced ? h('span.status.good', 'Yes') : h('span.status.warn', 'No'))),
          h('div.form-grid', h('label.field', 'Time zone', tz), h('div.row', saveTz)),
          h('div.feature-row', { style: { paddingBottom: 0 } },
            switchEl(s.ntp, async (on, input) => {
              input.disabled = true;
              try { await api.post('/settings/ntp', { enabled: on }); toast(`Network time ${on ? 'on' : 'off'}.`); }
              catch (e) { toast(e.message, true); input.checked = !on; }
              input.disabled = false;
            }, 'Network time'),
            h('div.text', h('strong', 'Network time (NTP)'), h('div.desc', 'Keep the clock in sync automatically.'))))),
      h('div.card', h('h2', 'Power'),
        h('p.muted', { style: { marginTop: 0 } }, 'Containers with a restart policy come back up after a reboot.'),
        h('div.row',
          power('Reboot', 'reboot', `Reboot ${node.name} now?`),
          power('Shut down', 'poweroff', `Shut ${node.name} down? You'll need physical or hypervisor access to turn it back on.`))),
      dashboardCard(d));
  }

  load();
  return () => {};
}
