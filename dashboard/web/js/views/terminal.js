import { Terminal } from '../../vendor/xterm.mjs';
import { FitAddon } from '../../vendor/addon-fit.mjs';
import { h, clear } from '../ui.js';

export function renderTerminal(el, { api, node, me }) {
  const status = h('span.status.idle', 'Connecting…');
  const reconnect = h('button.btn.small.hidden', { onclick: () => connect() }, 'Reconnect');
  const wrap = h('div.term-wrap');
  clear(el, h('div.stack',
    h('div.row', status, h('span.faint', `· shell as ${me.user} on ${node.name}`), h('span.spacer'), reconnect),
    wrap));

  const term = new Terminal({
    cursorBlink: true,
    fontFamily: 'ui-monospace, "JetBrains Mono", "Cascadia Code", Menlo, Consolas, monospace',
    fontSize: 13.5,
    theme: { background: '#0f0f0e', foreground: '#e8e7e2', cursor: '#3987e5', selectionBackground: '#184f95' },
    scrollback: 5000,
  });
  const fit = new FitAddon();
  term.loadAddon(fit);
  term.open(wrap);
  let ws;

  const sendSize = () => {
    if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'resize', cols: term.cols, rows: term.rows }));
  };
  const doFit = () => { try { fit.fit(); } catch {} sendSize(); };

  function connect() {
    reconnect.classList.add('hidden');
    status.className = 'status idle'; status.textContent = 'Connecting…';
    ws = new WebSocket(api.wsURL('/terminal'));
    ws.binaryType = 'arraybuffer';
    ws.onopen = () => {
      status.className = 'status good'; status.textContent = 'Connected';
      doFit();
      term.focus();
    };
    ws.onmessage = (e) => term.write(typeof e.data === 'string' ? e.data : new Uint8Array(e.data));
    ws.onclose = () => {
      status.className = 'status crit';
      status.textContent = 'Disconnected';
      reconnect.classList.remove('hidden');
    };
  }

  const onData = term.onData((d) => { if (ws?.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: 'input', data: d })); });
  const ro = new ResizeObserver(() => doFit());
  ro.observe(wrap);
  requestAnimationFrame(() => { doFit(); connect(); });

  return () => {
    ro.disconnect();
    onData.dispose();
    if (ws) { ws.onclose = null; ws.close(); }
    term.dispose();
  };
}
