// Fetch wrapper. Every request that changes something carries the session's CSRF token.
let csrf = '';
export const setCSRF = (t) => { csrf = t; };
export const csrfToken = () => csrf;

export class HTTPError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

let onUnauthorized = () => {};
export const setUnauthorizedHandler = (fn) => { onUnauthorized = fn; };

export async function req(method, path, body) {
  const opts = { method, headers: {}, credentials: 'same-origin' };
  if (method !== 'GET') opts.headers['X-CSRF-Token'] = csrf;
  if (body !== undefined) {
    opts.headers['Content-Type'] = 'application/json';
    opts.body = JSON.stringify(body);
  }
  const r = await fetch(path, opts);
  let data = null;
  try { data = await r.json(); } catch { /* empty body */ }
  if (!r.ok) {
    if (r.status === 401 && !path.startsWith('/auth/')) onUnauthorized();
    throw new HTTPError(r.status, data?.error || r.statusText);
  }
  return data;
}

export const get = (p) => req('GET', p);
export const post = (p, b = {}) => req('POST', p, b);
export const del = (p) => req('DELETE', p);

// Per-node API: /api/n/<id>/... (the main node proxies to paired nodes).
export function node(id) {
  const base = `/api/n/${encodeURIComponent(id)}`;
  return {
    id,
    get: (p) => get(base + p),
    post: (p, b) => post(base + p, b),
    url: (p) => base + p,
    wsURL: (p) => `${location.protocol === 'https:' ? 'wss' : 'ws'}://${location.host}${base}${p}`,
  };
}
