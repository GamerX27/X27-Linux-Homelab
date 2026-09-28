import { h, clear, when, busy, switchEl, confirmAction, toast } from '../ui.js';

const WEEKDAYS = [['mon', 'Monday'], ['tue', 'Tuesday'], ['wed', 'Wednesday'], ['thu', 'Thursday'], ['fri', 'Friday'], ['sat', 'Saturday'], ['sun', 'Sunday']];

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

export function renderFeatures(el, { api }) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  clear(el, content);

  async function load() {
    try {
      draw(await api.get('/features'));
    } catch (e) {
      clear(content, h('div.notice.warn', e.message));
    }
  }

  function draw(f) {
    clear(content, autoupdateCard(f.autoupdate), gotifyCard(f.autoupdate), servicesCard(f.services || []));
  }

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
        'On schedule, the node checks for a new image. If there is one it installs it and reboots; no update, no reboot. Same as ',
        h('code', 'autoupdate on …'), ' on the node.'),
      a.enabled ? h('dl.kv', { style: { marginBottom: '14px' } },
        h('dt', 'Schedule'), h('dd', a.schedule || a.calendar),
        h('dt', 'Next run'), h('dd', when(a.nextRun)),
        h('dt', 'Last run'), h('dd', when(a.lastRun), a.lastResult ? h('span.status', { class: a.lastResult === 'success' ? 'good' : 'crit', style: { marginLeft: '8px' } }, a.lastResult) : null))
        : a.lastRun ? h('dl.kv', { style: { marginBottom: '14px' } }, h('dt', 'Last run'), h('dd', when(a.lastRun), ` · ${a.lastResult}`)) : null,
      h('div.form-grid',
        h('label.field', 'How often', freq), wdField, dayField, h('label.field', 'Time (24-hour)', time),
        h('div.row', save, off)));
  }

  function gotifyCard(a) {
    if (!a.available) return null;
    const url = h('input', { type: 'url', placeholder: 'https://gotify.example.com', value: a.gotifyUrl || '' });
    const token = h('input', { type: 'password', placeholder: a.gotify ? 'Stored, enter a new one to change it' : 'App token', autocomplete: 'off' });
    const save = h('button.btn.primary', { onclick: async () => {
      const r = await busy(save, () => api.post('/features/gotify', { url: url.value.trim(), token: token.value.trim() }), 'Gotify set up. A test message was sent.');
      if (r) load();
    } }, a.gotify ? 'Save' : 'Set up');
    const test = a.gotify ? h('button.btn', { onclick: () => busy(test, () => api.post('/features/gotify/test'), 'Test message sent.') }, 'Send test') : null;
    const off = a.gotify ? h('button.btn.danger', { onclick: async () => {
      const r = await busy(off, () => api.post('/features/gotify/off'), 'Gotify off.');
      if (r) load();
    } }, 'Turn off') : null;
    return h('div.card',
      h('div.card-head', h('h2', 'Gotify messages'), a.gotify ? h('span.status.good', 'On') : h('span.status.idle', 'Off')),
      h('p.muted', { style: { marginTop: 0 } }, 'A message before an update reboot, and when an update fails. The setup sends a test message and only saves if it arrives.'),
      h('div.form-grid', h('label.field', 'Server URL', url), h('label.field', 'App token', token), h('div.row', save, test, off)));
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
        h('span.status', { class: s.active === 'active' ? 'good' : s.active === 'failed' ? 'crit' : 'idle' }, s.active)))
        : h('div.faint', 'No services found.'));
  }

  load();
  return () => {};
}
