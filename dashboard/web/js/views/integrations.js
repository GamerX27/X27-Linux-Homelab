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

// The MCP server's name in clients: homelab-<hostname>, so several dashboards don't clash.
const serverName = (me) => 'homelab-' + ((me?.hostname || 'main').toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-+|-+$/g, '') || 'main');

// How to connect a client. With a token (right after creating one) every command is filled
// in and ready to paste; without one, the commands say where the token goes.
function setupSnippets(secret, me) {
  const url = `${location.origin}/mcp`;
  const name = serverName(me);
  const tok = secret || 'PASTE-YOUR-TOKEN-HERE';
  const selfSigned = location.protocol === 'https:';
  const pem = `~/.config/homelab/${name}.pem`;
  const note = (...text) => h('p.faint', { style: { margin: 0 } }, ...text);
  const step = (title, ...body) => h('li', h('strong', title), ...body);
  const env = { AUTH_HEADER: `Bearer ${tok}` };
  if (selfSigned) env.NODE_EXTRA_CA_CERTS = `/home/<you>/.config/homelab/${name}.pem`;
  // The header goes through an env variable because some clients split args on spaces.
  const stdio = { mcpServers: { [name]: { command: 'npx', args: ['-y', 'mcp-remote', url, '--header', 'Authorization:${AUTH_HEADER}'], env } } };

  return [
    h('h3.setup-head', 'Claude Code'),
    h('ol.setup-steps',
      secret ? null : step('Create a token', note('Click ', h('strong', 'New token'), ' above. The commands below then appear with your token already filled in.')),
      selfSigned ? step('Download this dashboard\'s certificate',
        note('It\'s self-signed, so Claude Code has to be told to trust it. ',
          h('a', { href: '/ca.pem', download: `${name}.pem` }, `Download ${name}.pem`),
          me?.fingerprint ? [' and check that its fingerprint is ', h('code', me.fingerprint), ' (also under Settings → Dashboard).'] : '.'),
        snippet('Put it where Claude Code will find it', `mkdir -p ~/.config/homelab\nmv ~/Downloads/${name}.pem ${pem}`)) : null,
      selfSigned ? step('Trust it in every terminal',
        snippet('bash or zsh', `echo 'export NODE_EXTRA_CA_CERTS=${pem}' >> ~/.bashrc   # ~/.zshrc for zsh`),
        snippet('fish', `set -Ux NODE_EXTRA_CA_CERTS ${pem}`),
        note('This only takes effect in a new terminal. It holds one file: for several dashboards, combine them with ',
          h('code', 'cat ~/.config/homelab/*.pem > ~/.config/homelab/all.pem'), ' and point it at all.pem.')) : null,
      step('Add the server',
        snippet('Run once', `claude mcp add --scope user --transport http ${name} ${url} --header "Authorization: Bearer ${tok}"`),
        note(h('code', '--scope user'), ' makes it available in every folder; without it, only in the folder you run it from.',
          secret ? '' : [' Replace ', h('code', 'PASTE-YOUR-TOKEN-HERE'), ' with the whole token (it starts with ', h('code', 'x27hl_'), ').'])),
      step('Check it',
        note('Start Claude Code from a new terminal and type ', h('code', '/mcp'), ': ', h('code', name), ' should say connected.'),
        h('ul.faint.setup-trouble',
          selfSigned ? h('li', h('code', 'UNABLE_TO_VERIFY_LEAF_SIGNATURE'), ' or ', h('code', 'self-signed certificate'),
            ': the certificate isn\'t trusted yet. Check "Trust it in every terminal", then start Claude Code from a new terminal.') : null,
          h('li', h('code', '401'), ' or ', h('code', 'no bearer token'), ': the token is wrong or incomplete. Run ',
            h('code', `claude mcp remove ${name} --scope user`), ' and add it again with the whole token.')))),

    h('h3.setup-head', 'LM Studio, Claude Desktop, Cursor and other desktop apps'),
    snippet('mcp.json', JSON.stringify(stdio, null, 2)),
    note('Runs the server through the ', h('code', 'mcp-remote'), ' bridge, so it needs Node.js (', h('code', 'npx'), ') on that computer.',
      selfSigned ? [' Use the certificate downloaded above, with an absolute path: ', h('code', '~'), ' isn\'t expanded in ', h('code', 'env'), '.'] : null,
      ' In LM Studio: Program → Install → Edit ', h('code', 'mcp.json'), ', and replace an existing ', h('code', 'url'), '/', h('code', 'headers'), ' entry with this one.'),

    h('h3.setup-head', 'Other clients'),
    snippet('Address and header', `URL:    ${url}\nHeader: Authorization: Bearer ${tok}`),
    note('For clients that can trust a certificate themselves, or when the dashboard is behind a reverse proxy with a real certificate.'),
  ];
}

export function renderIntegrations(el, { menuButton }) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  clear(el,
    h('div.page-head', h('div.row', menuButton(), h('div', h('h1', 'Integrations'),
      h('div.sub', 'Connect other programs to your homelab.')))),
    content);

  let me = null;

  async function load() {
    try {
      const [tokens, t, m] = await Promise.all([api.get('/api/tokens'), api.get('/api/tools'), api.get('/auth/me').catch(() => null)]);
      me = m;
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
      snippet('Token', r.secret), ...setupSnippets(r.secret, me));
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
      h('div.card', h('h2', 'Connecting a client'), h('div.stack', setupSnippets('', me))),
      h('div.card', h('h2', `Tools (${list.length})`),
        h('p.muted', { style: { marginTop: 0 } }, 'There\'s no tool for files, the terminal or secrets: Gotify tokens, .env values and passwords never reach a model.'),
        h('div.table-wrap', h('table',
          h('thead', h('tr', h('th', 'Tool'), h('th', 'Access'), h('th', 'What it does'))),
          h('tbody', list.map((t) => h('tr', h('td.mono', t.name), h('td', scopeBadge(t.scope)), h('td.muted', t.description))))))));
  }

  load();
  return () => {};
}
