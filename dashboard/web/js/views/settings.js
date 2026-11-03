import { h, clear, busy, switchEl, confirmAction, toast } from '../ui.js';
import * as mainApi from '../api.js';

export function renderSettings(el, { api, node, refreshNodes }) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  let netTimer, stopped = false;
  clear(el, content);

  async function load() {
    try {
      const [s, zones, d, n] = await Promise.all([api.get('/settings'), api.get('/settings/timezones'),
        api.get('/dashboard').catch(() => null), api.get('/settings/network').catch(() => null)]);
      if (!stopped) draw(s, zones || [], d, n);
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

  // IPv4 per connection. A change is reverted by the node unless it's kept in time, so a
  // wrong address can't lock it out; the main node keeps a paired node's change itself
  // once it answers on the new address.
  function networkCard(n) {
    if (!n) return null;
    clearInterval(netTimer);
    const ip = (a) => a.split('/')[0];
    const list = (v) => v.split(/[\s,]+/).filter(Boolean);
    const moveNote = h('div');
    let pendingNote = null;
    if (n.pending) {
      const left = h('strong');
      const tick = () => {
        const sec = n.pending.deadline - Math.floor(Date.now() / 1000);
        if (sec <= 0) { clearInterval(netTimer); load(); return; }
        left.textContent = `${Math.floor(sec / 60)}:${String(sec % 60).padStart(2, '0')}`;
      };
      tick();
      // A paired node's change is kept by the main node, so look for that every 5 s.
      let ticks = 0;
      netTimer = setInterval(() => { tick(); if (!node.local && ++ticks % 5 === 0) load(); }, 1000);
      const keep = h('button.btn.primary', { onclick: async () => {
        if (await busy(keep, () => api.post('/settings/network/keep'), 'Network settings kept.')) load();
      } }, 'Keep');
      const revert = h('button.btn', { onclick: async () => {
        if (await busy(revert, () => api.post('/settings/network/revert'), 'Network settings reverted.')) setTimeout(load, 2000);
      } }, 'Revert now');
      pendingNote = h('div.notice.warn', { style: { display: 'flex', gap: '10px', alignItems: 'center', flexWrap: 'wrap' } },
        h('span', { style: { flex: '1' } }, `The change to ${n.pending.name} goes back in `, left, ' unless you keep it.'),
        keep, revert);
    }

    const conn = (c) => {
      const mode = h('select', h('option', { value: 'auto' }, 'DHCP'), h('option', { value: 'manual' }, 'Static'));
      mode.value = c.method === 'manual' ? 'manual' : 'auto';
      const lease = c.method === 'manual' ? null : c.current;
      const addr = h('input', { type: 'text', spellcheck: false, autocapitalize: 'off', placeholder: '192.168.1.10/24',
        value: (c.method === 'manual' ? c.addresses : lease.addresses).join(', ') });
      const gw = h('input', { type: 'text', spellcheck: false, autocapitalize: 'off', placeholder: '192.168.1.1',
        value: c.method === 'manual' ? c.gateway : lease.gateway });
      const dns = h('input', { type: 'text', spellcheck: false, autocapitalize: 'off', value: c.dns.join(', '),
        placeholder: c.method === 'manual' ? '1.1.1.1, 9.9.9.9' : 'From DHCP' });
      const staticFields = [h('label.field', 'Address / prefix', addr), h('label.field', 'Gateway', gw)];
      const show = () => staticFields.forEach((f) => { f.style.display = mode.value === 'manual' ? '' : 'none'; });
      mode.onchange = show;
      show();

      const save = h('button.btn.primary', { disabled: !!n.pending, onclick: async () => {
        const body = { method: mode.value, addresses: mode.value === 'manual' ? list(addr.value) : [],
          gateway: mode.value === 'manual' ? gw.value.trim() : '', dns: list(dns.value) };
        const here = node.local && c.current.addresses.some((a) => ip(a) === location.hostname);
        const moving = here && body.method === 'manual' && body.addresses.length && !body.addresses.some((a) => ip(a) === location.hostname);
        const lost = here && body.method === 'auto' && c.method === 'manual';
        if (!await confirmAction('Change network', `${c.device} on ${node.name} is reconfigured. If you don't keep the change within `
          + `${Math.round(n.revertAfter / 60)} minutes, it goes back to how it is now.`
          + (moving ? ` This page loses the node; open it on the new address to keep the change.` : '')
          + (lost ? ' DHCP may give the node another address; if this page loses it, the change goes back by itself.' : ''), 'Apply', false)) return;
        const r = await busy(save, () => node.local ? api.post(`/settings/network/${encodeURIComponent(c.uuid)}`, body)
          : mainApi.post(`/api/nodes/${encodeURIComponent(node.id)}/network/${encodeURIComponent(c.uuid)}`, body));
        if (!r) return;
        if (moving) {
          const url = `${location.protocol}//${ip(body.addresses[0])}${location.port ? `:${location.port}` : ''}/${location.hash}`;
          clear(moveNote, h('div.notice.warn', { style: { marginTop: '12px' } }, 'Applying… Open ',
            h('a', { href: url }, url), ', log in and press Keep on this page there.'));
        } else if (r.movedTo) {
          clear(moveNote, h('div.notice', { style: { marginTop: '12px' } },
            `The main node follows ${node.name} to ${r.movedTo} and keeps the change once it answers there.`));
          setTimeout(() => refreshNodes?.(), 8000);
        }
        setTimeout(load, 3000);
      } }, 'Apply');

      const cur = c.current;
      return h('div.stack', { style: { gap: '10px', paddingTop: '12px', borderTop: '1px solid var(--border)' } },
        h('div', h('strong.mono', c.device), h('span.faint', ` ${c.name}`)),
        h('dl.kv',
          h('dt', 'Address'), h('dd.mono', cur.addresses.join(', ') || '—'),
          h('dt', 'Gateway'), h('dd.mono', cur.gateway || '—'),
          h('dt', 'DNS'), h('dd.mono', cur.dns.join(', ') || '—')),
        h('div.form-grid', h('label.field', 'IPv4', mode), ...staticFields, h('label.field', 'DNS servers', dns), h('div.row', save)));
    };

    return h('div.card', h('h2', 'Network'), pendingNote,
      n.connections.length ? n.connections.map(conn) : h('div.faint', 'No network connections found.'),
      moveNote,
      h('p.faint', { style: { marginBottom: 0 } }, 'IPv4 through NetworkManager. DNS servers are separated by commas; '
        + 'with DHCP they replace the ones the network hands out. Leave them empty to use those.'));
  }

  function draw(s, zones, d, n) {
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
      networkCard(n),
      dashboardCard(d));
  }

  load();
  return () => { stopped = true; clearInterval(netTimer); };
}
