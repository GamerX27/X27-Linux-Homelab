import { h, clear, when, busy, switchEl, confirmAction, toast, modal, bytes, askAI } from '../ui.js';

export const WEEKDAYS = [['mon', 'Monday'], ['tue', 'Tuesday'], ['wed', 'Wednesday'], ['thu', 'Thursday'], ['fri', 'Friday'], ['sat', 'Saturday'], ['sun', 'Sunday']];

// "weekly on Sun at 03:30 (3:30 AM)" / OnCalendar → form values
function parseSchedule(a) {
  const s = { freq: 'weekly', weekday: 'sun', day: '1', time: '04:00' };
  const cal = a.calendar || '';
  const t = cal.match(/(\d{2}:\d{2}):\d{2}$/);
  if (t) s.time = t[1];
  const wd = cal.match(/^(Mon|Tue|Wed|Thu|Fri|Sat|Sun) /);
  const md = cal.match(/^\*-\*-(\d{2}) /);
  if (wd) { s.freq = 'weekly'; s.weekday = wd[1].toLowerCase(); }
  else if (md) { s.freq = 'monthly'; s.day = String(Number(md[1])); }
  else if (cal) s.freq = 'daily';
  return s;
}

export function renderFeatures(el, { api, node }) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  const nbCard = h('div.card', h('h2', 'NetBird VPN'), h('div.faint', 'Loading…'));
  let nbTimer, stopped = false;
  clear(el, content);
  const logsLink = (unit) => h('a.btn.small.ghost', { href: `#/n/${encodeURIComponent(node.id)}/logs?unit=${encodeURIComponent(unit)}` }, 'Logs');

  async function load() {
    try {
      draw(await api.get('/features'));
    } catch (e) {
      clear(content, h('div.notice.warn', e.message));
    }
  }

  function draw(f) {
    clear(content, autoupdateCard(f.autoupdate), gotifyCard(f.autoupdate), nbCard, servicesCard(f.services || []));
  }

  // NetBird: the service switch, connecting and disconnecting, and the peers.
  async function loadNetbird() {
    clearTimeout(nbTimer);
    let s;
    try { s = await api.get('/netbird'); } catch (e) { s = { message: e.message }; }
    if (stopped) return;
    if (!document.querySelector('.modal-back')) drawNetbird(s);
    nbTimer = setTimeout(loadNetbird, s.loginUrl || s.state === 'connecting' ? 3000 : 20000);
  }

  function connectDialog(s, withKey) {
    const key = h('input', { type: 'password', autocomplete: 'off', spellcheck: false, placeholder: 'e.g. A1B2C3D4-…' });
    const mgmt = h('input', { type: 'url', placeholder: 'Leave empty for netbird.io, or your own server', value: s.management?.replace(/:443$/, '') || '' });
    const err = h('div.error-text');
    const body = h('div.stack',
      withKey ? h('p.muted', { style: { margin: 0 } }, 'In the NetBird dashboard: ', h('strong', 'Setup Keys'),
        ' → create a key, then paste it here. It\'s passed to NetBird in a root-only file and not stored by the dashboard.')
        : h('p.muted', { style: { margin: 0 } }, 'You get a link to open in your browser. Log in there, and this node joins your network.'),
      withKey ? h('label.field', 'Setup key', key) : null,
      h('label.field', 'Management server (optional)', mgmt), err);
    return modal(withKey ? 'Connect with a setup key' : 'Log in with a browser', body, [
      { label: 'Cancel', value: null },
      { label: withKey ? 'Connect' : 'Get login link', class: 'primary', onclick: async () => {
        try {
          if (withKey) {
            await api.post('/netbird/up', { setupKey: key.value, managementUrl: mgmt.value.trim() });
            toast('NetBird connected.');
          } else {
            const r = await api.post('/netbird/login', { managementUrl: mgmt.value.trim() });
            if (r.output) window.open(r.output, '_blank', 'noopener');
            else toast('NetBird connected.');
          }
          return true;
        } catch (e) { err.textContent = e.message; return false; }
      } }]).then(() => loadNetbird());
  }

  function drawNetbird(s) {
    const state = { connected: ['good', 'Connected'], 'needs-login': ['warn', 'Needs login'], connecting: ['warn', 'Connecting…'],
      stopped: ['idle', 'Off'] }[s.state] || ['idle', s.state || 'Unknown'];
    const head = h('div.card-head', h('h2', 'NetBird VPN'), h('div.row', h('span.status', { class: state[0] }, state[1]), logsLink('netbird.service')));
    if (!s.installed) { clear(nbCard, head, h('div.notice', s.message || 'NetBird isn\'t installed on this node.')); return; }
    const sw = switchEl(s.enabled || s.running, async (on, input) => {
      if (!on && !await confirmAction('Turn off NetBird', 'If you reach this server over NetBird, turning it off cuts that connection.', 'Turn off')) { input.checked = true; return; }
      input.disabled = true;
      try { await api.post('/netbird/service', { enabled: on }); toast(`NetBird ${on ? 'on' : 'off'}.`); }
      catch (e) { toast(e.message, true); input.checked = !on; }
      input.disabled = false;
      loadNetbird();
    }, 'NetBird service');
    const down = h('button.btn.danger', { onclick: async () => {
      if (!await confirmAction('Disconnect NetBird', 'This node leaves the NetBird network until you connect again. If you reach it over NetBird, you lose that connection.', 'Disconnect')) return;
      if (await busy(down, () => api.post('/netbird/down'), 'Disconnected.')) loadNetbird();
    } }, 'Disconnect');
    const actions = s.running ? h('div.row',
      !s.connected ? h('button.btn.primary', { onclick: () => connectDialog(s, true) }, 'Connect with setup key…') : null,
      !s.connected ? h('button.btn', { onclick: () => connectDialog(s, false) }, 'Log in with browser…') : null,
      s.connected || s.state === 'connecting' ? down : null) : null;
    const peers = (s.peers || []);
    clear(nbCard, head,
      h('div.feature-row', { style: { paddingTop: 0 } }, sw,
        h('div.text', h('strong', 'NetBird service'), h('div.desc', 'Mesh VPN client. Stays connected across reboots and updates.'))),
      s.loginUrl ? h('div.notice.warn', { style: { margin: '10px 0' } }, 'Waiting for the browser login: ',
        h('a', { href: s.loginUrl, target: '_blank', rel: 'noopener' }, 'open the login link')) : null,
      s.message ? h('pre.output', { style: { margin: '10px 0' } }, s.message) : null,
      s.connected ? h('dl.kv', { style: { margin: '12px 0' } },
        h('dt', 'NetBird IP'), h('dd.mono', s.ip),
        h('dt', 'Name'), h('dd.mono', s.fqdn || '—'),
        h('dt', 'Server'), h('dd', s.management, s.signalConnected ? null : h('span.status.warn', { style: { marginLeft: '8px' } }, 'signal down')),
        h('dt', 'Peers'), h('dd', `${s.peersConnected} of ${s.peersTotal} connected`),
        h('dt', 'Version'), h('dd', s.version || '—')) : null,
      actions,
      s.connected && peers.length ? h('div.table-wrap', { style: { marginTop: '12px' } }, h('table',
        h('thead', h('tr', h('th', 'Peer'), h('th', 'IP'), h('th', 'Status'), h('th', 'Connection'), h('th', 'Latency'), h('th', 'Traffic in / out'))),
        h('tbody', peers.map((p) => h('tr', h('td.mono', p.fqdn), h('td.mono', p.ip),
          h('td', h('span.status', { class: p.status === 'Connected' ? 'good' : 'idle' }, p.status)),
          h('td', p.connection || '—'), h('td.num-cell', p.latencyMs ? `${p.latencyMs} ms` : '—'),
          h('td.num-cell.muted', `${bytes(p.received)} / ${bytes(p.sent)}`)))))) : null);
  }

  // systemd's Result for the last check: "success" means it ran, not that it updated.
  const result = (r) => r === 'success' ? 'ok' : r;

  function autoupdateCard(a) {
    if (!a.available) return h('div.card', h('h2', 'Automatic updates'), h('div.notice', 'autoupdate isn\'t installed on this node.'));
    const cur = parseSchedule(a);
    const freq = h('select', ['daily', 'weekly', 'monthly'].map((x) => h('option', { value: x }, x[0].toUpperCase() + x.slice(1))));
    const weekday = h('select', WEEKDAYS.map(([v, l]) => h('option', { value: v }, l)));
    const day = h('select', Array.from({ length: 31 }, (_, i) => h('option', { value: String(i + 1) }, String(i + 1))));
    const time = h('input', { type: 'time', required: true });
    freq.value = cur.freq; weekday.value = cur.weekday; day.value = cur.day; time.value = cur.time;
    const wdField = h('label.field', 'Weekday', weekday);
    const dayField = h('label.field', 'Day of month', day);
    const sync = () => {
      wdField.classList.toggle('hidden', freq.value !== 'weekly');
      dayField.classList.toggle('hidden', freq.value !== 'monthly');
    };
    freq.addEventListener('change', sync);
    sync();

    const save = h('button.btn.primary', { onclick: async () => {
      const r = await busy(save, () => api.post('/features/autoupdate',
        { freq: freq.value, weekday: weekday.value, day: day.value, time: time.value }), 'Schedule saved.');
      if (r) load();
    } }, a.enabled ? 'Save schedule' : 'Turn on');
    const off = a.enabled ? h('button.btn.danger', { onclick: async () => {
      const r = await busy(off, () => api.post('/features/autoupdate', { off: true }), 'Automatic updates off.');
      if (r) load();
    } }, 'Turn off') : null;

    return h('div.card',
      h('div.card-head', h('h2', 'Automatic updates'),
        a.enabled ? h('span.status.good', 'On') : h('span.status.idle', 'Off')),
      h('p.muted', { style: { marginTop: 0 } },
        'At the scheduled time, the node checks for a newer image. If there is one, it installs it and reboots; if not, nothing happens. '
        + 'On the node itself, ', h('code', 'autoupdate on …'), ' does the same.'),
      a.enabled ? h('dl.kv', { style: { marginBottom: '14px' } },
        h('dt', 'Schedule'), h('dd', a.schedule || a.calendar),
        h('dt', 'Next check'), h('dd', when(a.nextRun)),
        h('dt', 'Last check'), h('dd', when(a.lastRun), a.lastResult ? h('span.status', { class: a.lastResult === 'success' ? 'good' : 'crit', style: { marginLeft: '8px' } }, result(a.lastResult)) : null))
        : a.lastRun ? h('dl.kv', { style: { marginBottom: '14px' } }, h('dt', 'Last check'), h('dd', when(a.lastRun), ` · ${result(a.lastResult)}`)) : null,
      h('div.form-grid',
        h('label.field', 'How often', freq), wdField, dayField, h('label.field', 'Time (24-hour)', time),
        h('div.row', save, off)));
  }

  function gotifyCard(a) {
    if (!a.available) return null;
    const head = h('div.card-head', h('h2', 'Gotify messages'), a.gotify ? h('span.status.good', 'On') : h('span.status.idle', 'Off'));
    const intro = h('p.muted', { style: { marginTop: 0 } },
      'Sent from this node to your ', h('a', { href: 'https://gotify.net', target: '_blank', rel: 'noopener' }, 'Gotify'),
      ' server: when an update is installed, just before the reboot (old and new image, image name, and the packages upgraded, added and removed), and when an update fails.');
    const card = h('div.card');

    // The form: setting up, or changing a stored server (an empty token keeps the stored one).
    const form = (editing) => {
      const url = h('input', { type: 'url', placeholder: 'https://gotify.example.com', value: a.gotifyUrl || '', required: true });
      const token = h('input', { type: 'password', autocomplete: 'off', spellcheck: false,
        placeholder: editing ? 'Leave empty to keep the current token' : 'e.g. AbC1dEf2GhI3jKl' });
      const peek = h('button.btn.ghost.small', { type: 'button', onclick: () => {
        const show = token.type === 'password';
        token.type = show ? 'text' : 'password';
        peek.textContent = show ? 'Hide' : 'Show';
      } }, 'Show');
      const save = h('button.btn.primary', { type: 'submit' }, editing ? 'Save' : 'Set up');
      const cancel = editing ? h('button.btn', { type: 'button', onclick: () => draw(false) }, 'Cancel') : null;
      return h('form.stack', { onsubmit: async (e) => {
        e.preventDefault();
        const r = await busy(save, () => api.post('/features/gotify', { url: url.value.trim(), token: token.value.trim() }),
          'Saved. A test message was sent.');
        if (r) load();
      } },
        h('div.form-grid',
          h('label.field', 'Server URL', url),
          h('label.field', 'App token', h('div.input-row', token, peek))),
        h('p.faint', { style: { margin: 0 } }, 'In Gotify, open ', h('strong', 'Apps'), ', create an application (e.g. "',
          location.hostname, '") and copy its token. A test message is sent first; nothing is saved unless it arrives.'),
        h('div.row', save, cancel));
    };

    const summary = () => {
      const masked = '•'.repeat(15);
      const tokenEl = h('code.token', masked);
      let token = null;
      const fetchToken = async () => token ??= (await api.get('/features/gotify/token')).token;
      const show = h('button.btn.ghost.small', { onclick: async () => {
        if (tokenEl.textContent !== masked) { tokenEl.textContent = masked; show.textContent = 'Show'; return; }
        try { tokenEl.textContent = await fetchToken(); show.textContent = 'Hide'; } catch (e) { toast(e.message, true); }
      } }, 'Show');
      const copy = h('button.btn.ghost.small', { onclick: async () => {
        try { await navigator.clipboard.writeText(await fetchToken()); toast('Token copied.'); }
        catch (e) { toast(e.message || 'Couldn\'t copy', true); }
      } }, 'Copy');
      const test = h('button.btn', { onclick: () => busy(test, () => api.post('/features/gotify/test'), 'Test message sent.') }, 'Send test message');
      const off = h('button.btn.danger', { onclick: async () => {
        if (!await confirmAction('Turn off Gotify messages', 'The server URL and app token are deleted from this node.', 'Turn off')) return;
        const r = await busy(off, () => api.post('/features/gotify/off'), 'Gotify off.');
        if (r) load();
      } }, 'Turn off');
      return [
        h('dl.kv', { style: { marginBottom: '14px' } },
          h('dt', 'Server'), h('dd', h('a', { href: a.gotifyUrl, target: '_blank', rel: 'noopener' }, a.gotifyUrl)),
          h('dt', 'App token'), h('dd.token-row', tokenEl, show, copy),
          h('dt', 'Messages'), h('dd', 'Update installed (before the reboot) · Update failed')),
        h('div.row', test, h('button.btn', { onclick: () => draw(true) }, 'Change…'), off),
      ];
    };

    const draw = (editing) => clear(card, head, intro, a.gotify && !editing ? summary() : form(editing));
    draw(false);
    return card;
  }

  function servicesCard(list) {
    return h('div.card', h('h2', 'Services'),
      list.length ? list.map((s) => h('div.feature-row',
        switchEl(s.enabled, async (on, input) => {
          if (!on && s.warning && !await confirmAction(`Turn off ${s.name}`, s.warning, 'Turn off')) { input.checked = true; return; }
          input.disabled = true;
          try {
            await api.post(`/features/services/${s.id}`, { enabled: on });
            toast(`${s.name} ${on ? 'on' : 'off'}.`);
          } catch (e) { toast(e.message, true); input.checked = !on; }
          input.disabled = false;
          load();
        }, s.name),
        h('div.text', h('div.row', h('strong', s.name), h('code.faint', s.unit)), h('div.desc', s.description)),
        h('span.status', { class: s.active === 'active' ? 'good' : s.active === 'failed' ? 'crit' : 'idle' }, s.active), logsLink(s.unit),
        s.active === 'failed' ? askAI(`${s.unit} has failed on node ${node.name}. Read its journal and tell me why and how to fix it.`, `the Features tab of node ${node.name} (id ${node.id})`, 'Why?') : null))
        : h('div.faint', 'No services found.'));
  }

  load();
  loadNetbird();
  return () => { stopped = true; clearTimeout(nbTimer); };
}
