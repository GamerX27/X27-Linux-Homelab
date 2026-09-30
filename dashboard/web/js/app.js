import { h, clear, icon, toast, modal } from './ui.js';
import * as api from './api.js';
import { renderFleet, addNodeDialog } from './views/fleet.js';
import { renderOverview } from './views/overview.js';
import { renderUpdates } from './views/updates.js';
import { renderDocker } from './views/docker.js';
import { renderTerminal } from './views/terminal.js';
import { renderFiles } from './views/files.js';
import { renderFeatures } from './views/features.js';
import { renderSettings } from './views/settings.js';
import { renderRollout } from './views/rollout.js';

const TABS = [
  ['overview', 'Overview', renderOverview],
  ['updates', 'Updates', renderUpdates],
  ['docker', 'Docker', renderDocker],
  ['terminal', 'Terminal', renderTerminal],
  ['files', 'Files', renderFiles],
  ['features', 'Features', renderFeatures],
  ['settings', 'Settings', renderSettings],
];

const root = document.getElementById('root');
const state = { me: null, nodes: [], rollout: null, cleanup: null, nodesTimer: null };

// Theme: auto (OS), light or dark. Kept per browser; storage may be unavailable.
function applyTheme(t) {
  if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
  else delete document.documentElement.dataset.theme;
}
function savedTheme() { try { return localStorage.getItem('theme') || 'auto'; } catch { return 'auto'; } }
applyTheme(savedTheme());

api.setUnauthorizedHandler(() => { if (state.me) { state.me = null; showLogin(); } });

async function boot() {
  try {
    state.me = await api.get('/auth/me');
    api.setCSRF(state.me.csrf);
    showApp();
  } catch {
    showLogin();
  }
}

function showLogin() {
  stopView();
  clearInterval(state.nodesTimer);
  const user = h('input', { type: 'text', autocomplete: 'username', required: true, autocapitalize: 'off', spellcheck: false });
  const pass = h('input', { type: 'password', autocomplete: 'current-password', required: true });
  const err = h('div.error-text');
  const btn = h('button.btn.primary', { type: 'submit' }, 'Log in');
  const form = h('form.card', {
    onsubmit: async (e) => {
      e.preventDefault();
      err.textContent = '';
      btn.disabled = true; btn.classList.add('busy');
      try {
        const r = await api.post('/auth/login', { username: user.value.trim(), password: pass.value });
        api.setCSRF(r.csrf);
        state.me = await api.get('/auth/me');
        showApp();
      } catch (ex) {
        err.textContent = ex.message;
        pass.value = '';
        pass.focus();
      } finally { btn.disabled = false; btn.classList.remove('busy'); }
    },
  },
  h('div.brand', icon('server', 22), 'Homelab Dashboard'),
  h('p.muted', { style: { margin: 0 } }, 'Log in with your Linux username and password.'),
  h('label.field', 'Username', user),
  h('label.field', 'Password', pass),
  err, btn);
  clear(root, h('div.login', form));
  user.focus();
}

let shell;

function showApp() {
  const nav = h('nav.side-nodes');
  const main = h('main.main');
  const themeSel = h('select', { 'aria-label': 'Theme', style: { width: 'auto', padding: '2px 6px' },
    onchange: () => { try { localStorage.setItem('theme', themeSel.value); } catch {} applyTheme(themeSel.value); } },
    ['auto', 'light', 'dark'].map((t) => h('option', { value: t }, t[0].toUpperCase() + t.slice(1))));
  themeSel.value = savedTheme();
  const app = h('div.app',
    h('aside.sidebar',
      h('div.brand', icon('server', 22), 'Homelab'),
      h('a.side-link', { href: '#/', 'data-route': 'fleet' }, icon('grid', 16), h('span.name', 'All nodes')),
      h('div.side-label', 'Nodes'),
      nav,
      h('button.side-link', { onclick: () => addNodeDialog(refreshNodes) }, icon('plus', 16), h('span.name', 'Add node')),
      h('div.side-foot',
        h('button.profile-btn', { onclick: profileDialog, title: 'Profile picture (Gravatar)' },
          state.avatarEl = avatar(state.me.user, state.me.gravatar),
          h('span', h('span.muted', { style: { display: 'block', fontSize: '11.5px' } }, 'Signed in as'), h('strong', state.me.user))),
        h('div.row', h('span.muted', 'Theme'), themeSel),
        h('div.row', h('span.faint', `dashboard ${state.me.version}`),
          h('button.btn.small.ghost', { onclick: logout, title: 'Log out' }, icon('logout', 14), 'Log out')))),
    main);
  app.addEventListener('click', (e) => { if (e.target.closest('.side-link')) app.classList.remove('nav-open'); });
  shell = { app, nav, main };
  clear(root, app);
  refreshNodes();
  clearInterval(state.nodesTimer);
  state.nodesTimer = setInterval(refreshNodes, 15000);
  route();
}

// Gravatar picture, or the user's initials when there's none (d=404 makes Gravatar fail
// instead of serving its default image, so the fallback shows).
function avatar(user, hash, size = 32) {
  const initials = h('span.avatar', { 'aria-hidden': 'true', style: { width: size + 'px', height: size + 'px' } },
    (user || '?').slice(0, 2).toUpperCase());
  if (!hash) return initials;
  const img = h('img.avatar', { alt: '', width: size, height: size, referrerpolicy: 'no-referrer',
    src: `https://www.gravatar.com/avatar/${hash}?s=${size * 2}&d=404` });
  img.addEventListener('error', () => img.replaceWith(initials));
  return img;
}

async function profileDialog() {
  const email = h('input', { type: 'email', placeholder: 'you@example.com', autocomplete: 'email' });
  const err = h('div.error-text');
  const body = h('div.stack',
    h('div.row', avatar(state.me.user, state.me.gravatar, 56), h('div', h('strong', state.me.user),
      h('div.muted', state.me.gravatar ? 'Picture from Gravatar' : 'No Gravatar set'))),
    h('label.field', 'Gravatar email', email),
    h('p.faint', { style: { margin: 0, fontSize: '12.5px' } },
      'Only a hash of the address is stored. Your browser loads the picture from gravatar.com. Leave empty to remove it.'),
    err);
  await modal('Profile', body, [
    { label: 'Cancel', value: false },
    { label: 'Save', class: 'primary', onclick: async () => {
      try {
        const r = await api.post('/api/profile', { email: email.value });
        state.me.gravatar = r.gravatar;
        const next = avatar(state.me.user, r.gravatar);
        state.avatarEl.replaceWith(next);
        state.avatarEl = next;
        toast(r.gravatar ? 'Profile picture set.' : 'Profile picture removed.');
        return true;
      } catch (e) { err.textContent = e.message; return false; }
    } },
  ]);
}

async function logout() {
  try { await api.post('/auth/logout'); } catch {}
  state.me = null;
  showLogin();
}

function nodeStatus(n) {
  if (!n.online) return ['crit', 'Offline'];
  if (n.summary?.updateAvailable) return ['warn', 'Update available'];
  return ['good', 'Online'];
}

async function refreshNodes() {
  try {
    const [nodes, r] = await Promise.all([api.get('/api/nodes'), api.get('/api/rollout')]);
    state.nodes = nodes;
    state.rollout = r.job;
  } catch (e) {
    // The update page shows its own reconnect notice while the main node reboots.
    if (e.status !== 401 && !location.hash.startsWith('#/update')) toast(e.message, true);
    return;
  }
  renderNav();
  drawHead();
  document.dispatchEvent(new CustomEvent('nodes', { detail: state.nodes }));
}

function renderNav() {
  if (!shell) return;
  const cur = parseRoute();
  clear(shell.nav, state.nodes.map((n) => {
    const [cls, label] = nodeStatus(n);
    return h('a.side-link', { href: `#/n/${n.id}/overview`, class: cur.id === n.id ? 'active' : '', title: `${n.name}: ${label}` },
      h('span.dot', { class: cls, 'aria-label': label }), h('span.name', n.name),
      n.summary?.updateAvailable ? h('span.badge.update', { title: 'OS update available' }, '') : null);
  }));
  shell.app.querySelector('[data-route=fleet]').classList.toggle('active', !cur.id && !cur.update);
}

function findNode(id) {
  return state.nodes.find((x) => x.id === id) || { id, name: id === 'local' ? state.me.hostname : id, online: true, local: id === 'local' };
}

// The node's title line; redrawn when the node list refreshes.
function drawHead() {
  const r = parseRoute();
  if (!r.id || !shell.head?.isConnected) return;
  const n = findNode(r.id);
  const [cls, label] = nodeStatus(n);
  clear(shell.head,
    h('h1', n.name),
    h('div.sub.row', h('span.status', { class: cls }, label),
      n.summary?.info?.prettyName ? h('span', `· ${n.summary.info.prettyName}`) : null,
      n.local ? h('span.badge', 'main node') : h('span.faint', `· ${n.address || ''}`)));
}

function parseRoute() {
  if (/^#\/update\b/.test(location.hash)) return { id: null, update: true };
  const m = location.hash.match(/^#\/n\/([^/]+)\/?([a-z]*)/);
  return m ? { id: decodeURIComponent(m[1]), tab: m[2] || 'overview' } : { id: null };
}

function stopView() {
  if (state.cleanup) { try { state.cleanup(); } catch {} }
  state.cleanup = null;
}

function menuButton() {
  return h('button.btn.small.menu-btn', { onclick: () => shell.app.classList.toggle('nav-open'), 'aria-label': 'Menu' }, icon('menu', 16));
}

function route() {
  if (!state.me || !shell) return;
  stopView();
  renderNav();
  const r = parseRoute();
  const main = shell.main;
  if (r.update) {
    state.cleanup = renderRollout(main, { menuButton, refreshNodes });
    return;
  }
  if (!r.id) {
    state.cleanup = renderFleet(main, { nodes: () => state.nodes, rollout: () => state.rollout, refreshNodes, menuButton });
    return;
  }
  const n = findNode(r.id);
  const tab = TABS.find((t) => t[0] === r.tab) || TABS[0];
  const body = h('div');
  clear(main,
    h('div.page-head', h('div.row', menuButton(), shell.head = h('div'))),
    h('div.tabs', { role: 'tablist' }, TABS.map(([id, name]) =>
      h('a.tab', { href: `#/n/${encodeURIComponent(r.id)}/${id}`, role: 'tab', class: id === tab[0] ? 'active' : '',
        'aria-selected': String(id === tab[0]) }, name))),
    body);
  drawHead();
  state.cleanup = tab[2](body, { api: api.node(r.id), node: n, getNode: () => findNode(r.id), me: state.me, refreshNodes });
}

window.addEventListener('hashchange', route);
boot();
