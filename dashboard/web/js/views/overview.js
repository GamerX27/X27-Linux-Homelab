import { h, clear, meter, sparkline, bytes, rate, duration, pct } from '../ui.js';

// Refreshes every 3 s while open; keeps a minute of CPU and memory history client-side.
export function renderOverview(el, { api }) {
  const hist = { cpu: [], mem: [] };
  let timer, stopped = false;
  const err = h('div.notice.warn.hidden');
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  clear(el, h('div.stack', err, content));

  const tile = (label, value, unit, detail, extra) =>
    h('div.card.tile', h('div.label', label), h('div.value', value, unit ? h('small', unit) : null),
      detail ? h('div.detail', detail) : null, extra);

  async function tick() {
    try {
      const d = await api.get('/overview');
      err.classList.add('hidden');
      draw(d);
    } catch (e) {
      err.textContent = e.message;
      err.classList.remove('hidden');
    }
    if (!stopped) timer = setTimeout(tick, 3000);
  }

  function draw({ info, stats: s, summary, docker, dockerError }) {
    const memPct = s.memTotal ? s.memUsed / s.memTotal * 100 : 0;
    hist.cpu.push(s.cpuPercent); hist.mem.push(memPct);
    for (const k in hist) if (hist[k].length > 60) hist[k].shift();

    const tiles = h('div.grid.tiles',
      tile('CPU', (s.cpuPercent ?? 0).toFixed(0), '%', `${info.cpuCount} cores · load ${s.load.map((x) => x.toFixed(2)).join(' ')}`, sparkline(hist.cpu)),
      tile('Memory', memPct.toFixed(0), '%', `${bytes(s.memUsed)} of ${bytes(s.memTotal)}`
        + (s.swapTotal ? ` · swap ${bytes(s.swapUsed)}` : ''), sparkline(hist.mem)),
      tile('Uptime', duration(s.uptimeSec), null, `since ${new Date(Date.now() - s.uptimeSec * 1000).toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })}`),
      docker
        ? tile('Containers', docker.ContainersRunning, `of ${docker.Containers} running`, `${docker.Images} images · Docker ${docker.ServerVersion}`)
        : tile('Containers', '—', null, dockerError || 'Docker unavailable'));

    const disks = h('div.card', h('h2', 'Storage'),
      s.disks?.length ? s.disks.map((d) => {
        const p = d.total ? d.used / d.total * 100 : 0;
        return h('div.meter-row', h('span.what', { title: d.mount }, d.mount, h('span.faint', ` ${d.fs}`)),
          meter(p, `${d.mount} used`), h('span.num', `${bytes(d.used)} / ${bytes(d.total)} · ${pct(p)}`));
      }) : h('div.faint', 'No disks found.'));

    const net = h('div.card', h('h2', 'Network'),
      s.net?.length ? h('div.table-wrap', h('table',
        h('thead', h('tr', h('th', 'Interface'), h('th', 'Receive'), h('th', 'Send'), h('th', 'Total in / out'))),
        h('tbody', s.net.map((n) => h('tr', h('td.mono', n.name), h('td.num-cell', rate(n.rxRate)), h('td.num-cell', rate(n.txRate)),
          h('td.num-cell.muted', `${bytes(n.rxBytes)} / ${bytes(n.txBytes)}`)))))) : h('div.faint', 'No interfaces.'));

    const temps = s.temps?.length ? h('div.card', h('h2', 'Temperatures'),
      s.temps.map((t) => h('div.meter-row', h('span.what', t.name), meter(t.celsius, `${t.name} temperature`),
        h('span.num', `${t.celsius.toFixed(0)} °C`)))) : null;

    const sys = h('div.card', h('h2', 'System'), h('dl.kv',
      h('dt', 'Hostname'), h('dd', info.hostname),
      h('dt', 'OS'), h('dd', info.prettyName || '—'),
      h('dt', 'Image version'), h('dd', h('span.mono', summary.version || '—'),
        summary.updateAvailable ? h('span.badge.update', { style: { marginLeft: '8px' } }, `${summary.updateVersion || 'newer'} available`) : null,
        summary.updateStaged ? h('span.badge.warn', { style: { marginLeft: '8px' } }, 'update staged, reboot to apply') : null),
      h('dt', 'Build'), h('dd', h('span.mono', info.buildId || '—')),
      h('dt', 'Kernel'), h('dd', h('span.mono', info.kernel)),
      h('dt', 'CPU'), h('dd', info.cpuModel || '—'),
      h('dt', 'Dashboard'), h('dd', h('span.mono', summary.dashboardVersion))));

    clear(content, tiles, h('div.grid.two', sys, disks), h('div.grid.two', net, temps));
  }

  tick();
  return () => { stopped = true; clearTimeout(timer); };
}
