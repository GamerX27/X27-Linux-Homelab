// Small DOM helpers: no framework, no build step.

// h('div.card', {onclick}, child, 'text', [more children])
export function h(tag, attrs, ...children) {
  const [name, ...classes] = tag.split('.');
  const el = document.createElement(name || 'div');
  if (classes.length) el.className = classes.join(' ');
  if (attrs && (typeof attrs !== 'object' || attrs instanceof Node || Array.isArray(attrs))) {
    children.unshift(attrs);
    attrs = null;
  }
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (k === 'class') el.className += (el.className ? ' ' : '') + v;
    else if (k === 'style' && typeof v === 'object') Object.assign(el.style, v);
    else if (k in el && k !== 'list' && typeof v !== 'string') el[k] = v;
    else el.setAttribute(k, v === true ? '' : v);
  }
  append(el, children);
  return el;
}

function append(el, children) {
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
}

export function svg(tag, attrs = {}) {
  const el = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  return el;
}

export function clear(el, ...children) {
  el.replaceChildren();
  append(el, children);
  return el;
}

export function icon(name, size = 18) {
  const paths = {
    server: 'M3 4h18v7H3zM3 13h18v7H3zM7 7.5h.01M7 16.5h.01',
    grid: 'M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z',
    plus: 'M12 5v14M5 12h14',
    menu: 'M4 6h16M4 12h16M4 18h16',
    logout: 'M9 21H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h4M16 17l5-5-5-5M21 12H9',
    refresh: 'M21 12a9 9 0 1 1-3-6.7L21 8M21 3v5h-5',
  };
  const s = svg('svg', { width: size, height: size, viewBox: '0 0 24 24', fill: 'none', stroke: 'currentColor',
    'stroke-width': 2, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', 'aria-hidden': 'true' });
  s.append(svg('path', { d: paths[name] || '' }));
  return s;
}

export function toast(msg, error = false) {
  const t = h('div.toast', { class: error ? 'error' : '', role: error ? 'alert' : 'status' }, msg);
  document.getElementById('toasts').append(t);
  setTimeout(() => t.remove(), error ? 9000 : 4500);
}

export function modal(title, body, actions = []) {
  return new Promise((resolve) => {
    const close = (v) => { back.remove(); document.removeEventListener('keydown', onKey); resolve(v); };
    const onKey = (e) => { if (e.key === 'Escape') close(null); };
    const box = h('div.modal', { role: 'dialog', 'aria-modal': 'true' },
      h('h2', title), body,
      actions.length ? h('div.actions', actions.map((a) =>
        h('button.btn', { class: a.class || '', type: a.submit ? 'submit' : 'button', onclick: async (e) => {
          if (a.onclick) {
            const btn = e.currentTarget;
            btn.disabled = true; btn.classList.add('busy');
            try {
              const v = await a.onclick();
              if (v !== false) close(v === undefined ? a.value : v);
            } finally { btn.disabled = false; btn.classList.remove('busy'); }
          } else close(a.value);
        } }, a.label))) : null);
    if (body?.dataset?.wide) box.classList.add('wide');
    const back = h('div.modal-back', { onclick: (e) => { if (e.target === back) close(null); } }, box);
    document.addEventListener('keydown', onKey);
    document.body.append(back);
    (box.querySelector('input, select') || box.querySelector('.btn.primary, .btn'))?.focus();
  });
}

export function confirmAction(title, message, label = 'Continue', danger = true) {
  return modal(title, h('p.muted', { style: { margin: 0 } }, message), [
    { label: 'Cancel', value: false },
    { label, value: true, class: danger ? 'danger solid' : 'primary' },
  ]);
}

// Runs an async action on a button with a spinner, toasting the result.
export async function busy(btn, fn, okMsg) {
  btn.disabled = true; btn.classList.add('busy');
  try {
    const r = await fn();
    const msg = okMsg ?? r?.output;
    if (msg) toast(msg);
    return r;
  } catch (e) {
    toast(e.message, true);
  } finally {
    btn.disabled = false; btn.classList.remove('busy');
  }
}

export function switchEl(checked, onchange, label) {
  const input = h('input', { type: 'checkbox', role: 'switch', 'aria-label': label });
  input.checked = checked;
  input.addEventListener('change', () => onchange(input.checked, input));
  return h('label.switch', input, h('span'));
}

export function meter(pct, label) {
  const p = Math.max(0, Math.min(100, pct || 0));
  return h('div.meter', { class: p >= 90 ? 'crit' : p >= 75 ? 'warn' : '', role: 'meter',
    'aria-valuemin': 0, 'aria-valuemax': 100, 'aria-valuenow': Math.round(p), 'aria-label': label },
    h('span', { style: { width: p + '%' } }));
}

// Single-series sparkline; the tile's title names it, so no legend.
export function sparkline(values, max = 100) {
  const w = 200, hgt = 36, n = 60;
  const s = svg('svg', { class: 'spark', viewBox: `0 0 ${w} ${hgt}`, preserveAspectRatio: 'none', 'aria-hidden': 'true' });
  if (values.length < 2) return s;
  // Spread whatever history there is (up to n samples) across the full width.
  const pts = values.slice(-n).map((v, i, a) =>
    [(i / (a.length - 1)) * w, hgt - 2 - (Math.min(v, max) / max) * (hgt - 4)]);
  const d = pts.map((p, i) => (i ? 'L' : 'M') + p[0].toFixed(1) + ' ' + p[1].toFixed(1)).join(' ');
  s.append(svg('path', { class: 'area', d: `${d} L${pts.at(-1)[0]} ${hgt} L${pts[0][0]} ${hgt} Z` }));
  s.append(svg('path', { class: 'line', d, 'vector-effect': 'non-scaling-stroke' }));
  return s;
}

export function bytes(n, digits = 1) {
  if (n == null || isNaN(n)) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB', 'PB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return n.toFixed(i === 0 ? 0 : digits) + ' ' + u[i];
}

export function rate(n) { return bytes(n) + '/s'; }

export function duration(sec) {
  sec = Math.floor(sec || 0);
  const d = Math.floor(sec / 86400), hr = Math.floor(sec % 86400 / 3600), m = Math.floor(sec % 3600 / 60);
  if (d) return `${d}d ${hr}h`;
  if (hr) return `${hr}h ${m}m`;
  return `${m}m`;
}

export function when(unix) {
  if (!unix) return 'never';
  const d = new Date(unix * 1000);
  const diff = (Date.now() - d) / 1000;
  const rel = Math.abs(diff) < 60 ? 'just now'
    : Math.abs(diff) < 3600 ? `${Math.round(Math.abs(diff) / 60)} min`
    : Math.abs(diff) < 86400 ? `${Math.round(Math.abs(diff) / 3600)} h`
    : `${Math.round(Math.abs(diff) / 86400)} d`;
  const label = rel === 'just now' ? rel : diff > 0 ? `${rel} ago` : `in ${rel}`;
  return `${d.toLocaleString([], { dateStyle: 'medium', timeStyle: 'short' })} (${label})`;
}

export function pct(n) { return (n ?? 0).toFixed(0) + '%'; }
