import { h, clear, confirmAction, toast, modal, when } from '../ui.js';
import * as api from '../api.js';

// What an MCP token may do. The server enforces it; this only explains it.
const SCOPES = [
  ['read', 'Read', 'Looks only: status, logs, updates, compose files (no .env values).'],
  ['operate', 'Operate', 'Also starts, stops, restarts and updates apps and containers, runs backups.'],
  ['admin', 'Admin', 'Also OS updates, reboots, channel, services, users\' groups, editing and removing apps.'],
];
const scopeBadge = (s) => h('span.badge', { class: s === 'admin' ? 'warn' : s === 'operate' ? 'update-plain' : '' }, s);

function snippet(label, text) {
  const copy = h('button.btn.ghost.small', { onclick: async () => {
    try { await navigator.clipboard.writeText(text); toast('Copied.'); } catch { toast('Couldn\'t copy', true); }
  } }, 'Copy');
  return h('div.stack', { style: { gap: '4px' } }, h('div.row', h('strong', label), h('span.spacer'), copy), h('pre.output', text));
}

function setupSnippets(secret) {
  const url = `${location.origin}/mcp`;
  const tok = secret || '<token>';
  const tlsNote = location.protocol === 'https:';
  const note = (...text) => h('p.faint', { style: { margin: 0 } }, ...text);
  const env = { AUTH_HEADER: `Bearer ${tok}` };
  if (tlsNote) env.NODE_EXTRA_CA_CERTS = '/home/<you>/homelab-dashboard.pem';
  // The header goes through an env variable because some clients split args on spaces.
  const stdio = { mcpServers: { homelab: { command: 'npx', args: ['-y', 'mcp-remote', url, '--header', 'Authorization:${AUTH_HEADER}'], env } } };
  return [
    snippet('Claude Code', `claude mcp add --transport http homelab ${url} --header "Authorization: Bearer ${tok}"`),
    tlsNote ? note('Then start it with ', h('code', 'NODE_EXTRA_CA_CERTS=~/homelab-dashboard.pem claude'), '.') : null,
    snippet('LM Studio, Claude Desktop, Cursor and other desktop apps', JSON.stringify(stdio, null, 2)),
    note('Runs the server through the ', h('code', 'mcp-remote'), ' bridge, so it needs Node.js (', h('code', 'npx'), ') on that computer.',
      tlsNote ? [' The PEM path must be absolute: ', h('code', '~'), ' isn\'t expanded in ', h('code', 'env'), '.'] : null,
      ' In LM Studio: Program → Install → Edit ', h('code', 'mcp.json'), ', and replace an existing ', h('code', 'url'), '/', h('code', 'headers'), ' entry with this one.'),
    snippet('Clients with their own certificate setting', `URL:    ${url}\nHeader: Authorization: Bearer ${tok}`),
    note('For clients that can trust a certificate themselves, or when the dashboard is behind a reverse proxy with a real certificate.'),
    tlsNote ? h('div.notice', 'The dashboard\'s certificate is self-signed, so clients need to trust it: ',
      h('a', { href: '/ca.pem', download: 'homelab-dashboard.pem' }, 'download it'), ' and check its fingerprint against Settings → Dashboard.') : null,
  ];
}

export function renderIntegrations(el, { menuButton }) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  clear(el,
    h('div.page-head', h('div.row', menuButton(), h('div', h('h1', 'Integrations'),
      h('div.sub', 'Connect other programs to your homelab.')))),
    content);

  async function load() {
    try {
      const [tokens, t] = await Promise.all([api.get('/api/tokens'), api.get('/api/tools')]);
      draw(tokens, t.tools || []);
    } catch (e) { clear(content, h('div.notice.warn', e.message)); }
  }

  async function newToken() {
    const name = h('input', { type: 'text', placeholder: 'e.g. Claude Code on my laptop' });
    const scope = h('select', SCOPES.map(([v, l, d]) => h('option', { value: v }, `${l} · ${d}`)));
    const days = h('select', [[90, '90 days'], [30, '30 days'], [365, '1 year'], [0, 'Never']].map(([v, l]) => h('option', { value: v }, l)));
    const err = h('div.error-text');
    const r = await modal('New MCP token', h('div.stack',
      h('label.field', 'Name', name), h('label.field', 'Access', scope), h('label.field', 'Expires after', days),
      h('p.faint', { style: { margin: 0 } }, 'The client acts as you, on every node, with the access you pick. Admin can reboot nodes and change the OS.'), err), [
      { label: 'Cancel', value: null },
      { label: 'Create token', class: 'primary', onclick: async () => {
        try { return await api.post('/api/tokens', { name: name.value, scope: scope.value, days: Number(days.value) }); }
        catch (e) { err.textContent = e.message; return false; }
      } }]);
    if (!r) return;
    const box = h('div.stack', { 'data-wide': '1' },
      h('div.notice.warn', 'Copy the token now. It isn\'t shown again; only a hash of it is stored.'),
      snippet('Token', r.secret), ...setupSnippets(r.secret));
    box.dataset.wide = '1';
    await modal(`Token "${r.token.name}" created`, box, [{ label: 'Done', value: true, class: 'primary' }]);
    load();
  }

  function draw(tokens, list) {
    const rows = tokens.map((t) => h('tr',
      h('td', h('strong', t.name), h('div.sub', `by ${t.owner}`)),
      h('td', scopeBadge(t.scope)),
      h('td.muted', when(t.createdAt)),
      h('td.muted', t.lastUsed ? when(t.lastUsed) : 'never'),
      h('td.muted', t.expiresAt ? when(t.expiresAt) : 'never'),
      h('td.actions', h('button.btn.small.danger', { onclick: async () => {
        if (!await confirmAction('Revoke token', `Clients using "${t.name}" lose access right away.`, 'Revoke')) return;
        try { await api.del(`/api/tokens/${t.id}`); toast('Token revoked.'); load(); } catch (e) { toast(e.message, true); }
      } }, 'Revoke'))));
    clear(content,
      h('div.card',
        h('div.card-head', h('h2', 'AI clients (MCP)'), h('button.btn.primary', { onclick: newToken }, 'New token')),
        h('p.muted', { style: { marginTop: 0 } }, 'AI programs that speak the ',
          h('a', { href: 'https://modelcontextprotocol.io', target: '_blank', rel: 'noopener' }, 'Model Context Protocol'),
          ' (Claude Code, Claude Desktop, LM Studio, Cursor…) connect to ', h('code', `${location.origin}/mcp`),
          ' with a token. They can then look at, and with enough access manage, every node.'),
        tokens.length ? h('div.table-wrap', h('table',
          h('thead', h('tr', h('th', 'Name'), h('th', 'Access'), h('th', 'Created'), h('th', 'Last used'), h('th', 'Expires'), h('th', ''))),
          h('tbody', rows))) : h('div.faint', 'No tokens yet.')),
      h('div.card', h('h2', 'Connecting a client'), h('div.stack', setupSnippets(''))),
      h('div.card', h('h2', `Tools (${list.length})`),
        h('p.muted', { style: { marginTop: 0 } }, 'There\'s no tool for files, the terminal or secrets: Gotify tokens, .env values and passwords never reach a model.'),
        h('div.table-wrap', h('table',
          h('thead', h('tr', h('th', 'Tool'), h('th', 'Access'), h('th', 'What it does'))),
          h('tbody', list.map((t) => h('tr', h('td.mono', t.name), h('td', scopeBadge(t.scope)), h('td.muted', t.description))))))));
  }

  load();
  return () => {};
}
