import { h, clear, busy, switchEl, confirmAction, toast } from '../ui.js';

export function renderSettings(el, { api, node, refreshNodes }) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  clear(el, content);

  async function load() {
    try {
      const [s, zones] = await Promise.all([api.get('/settings'), api.get('/settings/timezones')]);
      draw(s, zones || []);
    } catch (e) {
      clear(content, h('div.notice.warn', e.message));
    }
  }

  function draw(s, zones) {
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
      h('div.card', h('h2', 'Dashboard'),
        h('p.muted', { style: { margin: 0 } }, 'Port, mode and pairing are changed on the node itself: ',
          h('code', 'dashboard status'), ', ', h('code', 'dashboard port <port>'), ', ', h('code', 'dashboard pair'), '.')));
  }

  load();
  return () => {};
}
