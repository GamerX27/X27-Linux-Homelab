import { h, clear, icon, toast } from './ui.js';

// The file isn't called logview.js: ad blockers refuse to load scripts with "log" in the name.
// A log viewer shared by the journal (System → Logs) and container logs: a dark list of
// lines with a time, a source and a message, search with highlighted matches, follow,
// line wrap, download, and colours by level (0 emergency … 7 debug, like syslog).

const LEVEL = ['emerg', 'alert', 'crit', 'err', 'warning', 'notice', 'info', 'debug'];

const fmtTime = (ms) => {
  if (!ms) return '';
  const d = new Date(ms);
  return d.toLocaleString([], { month: 'short', day: '2-digit', hour: '2-digit', minute: '2-digit', second: '2-digit' });
};

// A level for a line from an app that doesn't give one: what its words suggest.
export function guessLevel(msg) {
  if (/\b(fatal|panic|critical|crit|emerg(ency)?)\b/i.test(msg)) return 2;
  if (/\b(error|err|exception|traceback|failed|failure)\b|level=error|"level":"error"|\[error\]|\bE\d{4} /i.test(msg)) return 3;
  if (/\b(warn|warning)\b|level=warn|"level":"warn|\[warn(ing)?\]/i.test(msg)) return 4;
  if (/\b(debug|trace)\b|level=debug|\[debug\]/i.test(msg)) return 7;
  return 6;
}

// Wraps the matches of q in <mark>.
function highlight(text, q) {
  if (!q) return text;
  const lower = text.toLowerCase(), needle = q.toLowerCase();
  const out = [];
  let i = 0;
  for (let j = lower.indexOf(needle); j !== -1; j = lower.indexOf(needle, i)) {
    if (j > i) out.push(text.slice(i, j));
    out.push(h('mark', text.slice(j, j + q.length)));
    i = j + q.length;
  }
  out.push(text.slice(i));
  return out;
}

/**
 * opts:
 *   fetch({ cursor, grep }) → { entries: [{ time, src, level, message, ... }], cursor }
 *     Without a cursor it's the first page; with one, only what came after.
 *   tools: elements for the toolbar (filters); call view.reload() when they change.
 *   serverSearch: the search text goes to fetch as grep; otherwise lines are filtered here.
 *   source: show the source column.
 *   fileName(): the download's name, without .log.
 *   every: how often following polls, in ms.
 */
export function logView({ fetch, tools = [], serverSearch = false, source = true, fileName = () => 'log', every = 2000,
  maxLines = 5000, empty = 'No log entries match.', follow: startFollowing = false, tall = true } = {}) {
  let timer, stopped = false, cursor = '', lines = [], following = false, gen = 0, loaded = false;
  let wrap = localStorage.getItem('log-wrap') !== '0';

  const grep = h('input.filter-input', { type: 'search', placeholder: serverSearch ? 'Search…' : 'Search these lines…', spellcheck: false, 'aria-label': 'Search' });
  const followBtn = h('button.btn', { type: 'button', onclick: () => setFollow(!following) }, 'Follow');
  const wrapBtn = h('button.btn', { type: 'button', title: 'Wrap long lines', 'aria-pressed': String(wrap), onclick: () => {
    wrap = !wrap; localStorage.setItem('log-wrap', wrap ? '1' : '0'); applyWrap();
  } }, 'Wrap');
  const dl = h('button.btn', { type: 'button', title: 'Download what\'s shown', 'aria-label': 'Download', onclick: download }, icon('download', 15));
  const info = h('span.faint.log-count');
  const jump = h('button.btn.small.log-jump.hidden', { type: 'button', onclick: () => { list.scrollTop = list.scrollHeight; } }, '↓ Newest');
  const list = h('div.log-view', { role: 'log', 'aria-live': 'off', class: [source ? '' : 'no-src', tall ? '' : 'short'].join(' ') });
  const el = h('div.logview',
    h('div.row.log-tools', tools, grep, h('span.spacer'), info, wrapBtn, followBtn, dl),
    h('div.log-wrap', list, jump));

  function applyWrap() {
    list.classList.toggle('nowrap', !wrap);
    wrapBtn.classList.toggle('primary', wrap);
    wrapBtn.setAttribute('aria-pressed', String(wrap));
  }
  applyWrap();

  const q = () => grep.value.trim();
  const shown = (e) => serverSearch || !q() || e.message.toLowerCase().includes(q().toLowerCase()) || (e.src || '').toLowerCase().includes(q().toLowerCase());

  const row = (e) => h('div.log-line', { class: `p${e.level ?? 6}${e.stream === 'stderr' ? ' stderr' : ''}`, title: e.title || null },
    h('span.t', fmtTime(e.time)),
    source ? h('span.src', { style: e.color ? { color: e.color } : null }, e.src || '') : null,
    h('span.msg', highlight(e.message, q())));

  function countText() {
    const n = lines.filter(shown).length;
    info.textContent = !serverSearch && q() ? `${n} of ${lines.length} lines` : `${lines.length} line${lines.length === 1 ? '' : 's'}`;
  }

  function render(entries, append) {
    const atBottom = list.scrollHeight - list.scrollTop - list.clientHeight < 40;
    if (!append) { lines = []; clear(list); }
    lines.push(...entries);
    const frag = document.createDocumentFragment();
    entries.filter(shown).forEach((e) => frag.append(row(e)));
    if (append && entries.length) list.querySelector(':scope > .empty')?.remove();
    list.append(frag);
    let drop = lines.length - maxLines;
    if (drop > 0) {
      const removed = lines.splice(0, drop);
      // Remove the rows of dropped lines (only the shown ones have rows).
      let rows = removed.filter(shown).length;
      while (rows-- > 0) list.querySelector(':scope > .log-line')?.remove();
    }
    if (!list.querySelector(':scope > .log-line')) clear(list, h('div.empty', empty));
    countText();
    if (!append || atBottom) list.scrollTop = list.scrollHeight;
  }

  // Search without asking the server again: draw the lines already here.
  function redraw() {
    const all = lines;
    lines = [];
    render(all, false);
  }

  async function load() {
    const my = ++gen;
    clearTimeout(timer);
    try {
      const r = await fetch({ grep: serverSearch ? q() : '' });
      if (stopped || my !== gen) return;
      cursor = r.cursor || '';
      loaded = true;
      render(r.entries || [], false);
    } catch (e) { if (my === gen && !stopped) clear(list, h('div.notice.warn', e.message)); }
    if (following && !stopped) schedule();
  }

  async function poll() {
    const my = gen;
    // Without a cursor (nothing logged yet) there's nothing to continue from: fetch it all again.
    const append = !!cursor;
    try {
      const r = await fetch({ cursor, grep: serverSearch ? q() : '' });
      if (stopped || my !== gen) return;
      if (r.cursor) cursor = r.cursor;
      if (!append) render(r.entries || [], false);
      else if (r.entries?.length) render(r.entries, true);
    } catch { /* keep trying */ }
    if (following && my === gen) schedule();
  }

  function schedule() { clearTimeout(timer); if (!stopped) timer = setTimeout(poll, every); }

  function setFollow(on) {
    following = on;
    followBtn.classList.toggle('primary', on);
    followBtn.textContent = on ? 'Following' : 'Follow';
    if (on) { list.scrollTop = list.scrollHeight; if (loaded) schedule(); } else clearTimeout(timer);
  }

  list.addEventListener('scroll', () => {
    jump.classList.toggle('hidden', list.scrollHeight - list.scrollTop - list.clientHeight < 80);
  });

  function download() {
    const out = lines.filter(shown);
    if (!out.length) { toast('Nothing to download.', true); return; }
    const text = out.map((e) => `${e.time ? new Date(e.time).toISOString() : '-'} ${e.src || '-'}${e.pid ? `[${e.pid}]` : ''} ${LEVEL[e.level ?? 6] || ''}: ${e.message}`).join('\n');
    const a = h('a', { href: URL.createObjectURL(new Blob([text + '\n'], { type: 'text/plain' })),
      download: `${fileName()}-${new Date().toISOString().slice(0, 19)}.log` });
    document.body.append(a); a.click(); a.remove();
    setTimeout(() => URL.revokeObjectURL(a.href), 1000);
  }

  let debounce;
  grep.addEventListener('input', () => {
    clearTimeout(debounce);
    debounce = setTimeout(serverSearch ? load : redraw, serverSearch ? 350 : 120);
  });

  if (startFollowing) setFollow(true);
  return {
    el,
    reload: load,
    setFollow,
    stop() { stopped = true; clearTimeout(timer); clearTimeout(debounce); },
  };
}
