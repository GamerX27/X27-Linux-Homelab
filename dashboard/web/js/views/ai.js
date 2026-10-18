import { h, clear, busy, switchEl, confirmAction, toast, modal, when } from '../ui.js';
import * as api from '../api.js';

const STORE = 'assistant-chat';
const SCOPES = [
  ['read', 'Read', 'Looks only: status, logs, updates, compose files (no .env values).'],
  ['operate', 'Operate', 'Also starts, stops, restarts and updates apps and containers, runs backups.'],
  ['admin', 'Admin', 'Also OS updates, reboots, channel, services, users\' groups, editing and removing apps.'],
];
const scopeBadge = (s) => h('span.badge', { class: s === 'admin' ? 'warn' : s === 'operate' ? 'update-plain' : '' }, s);

// ---- A small, safe Markdown subset for the model's answers (no HTML) -------------------

function inline(text) {
  const out = [];
  const re = /(`[^`]+`|\*\*[^*]+\*\*|\[[^\]]+\]\(https?:\/\/[^)\s]+\))/g;
  let last = 0, m;
  while ((m = re.exec(text))) {
    if (m.index > last) out.push(text.slice(last, m.index));
    const t = m[0];
    if (t[0] === '`') out.push(h('code', t.slice(1, -1)));
    else if (t[0] === '*') out.push(h('strong', t.slice(2, -2)));
    else {
      const [, label, href] = t.match(/^\[([^\]]+)\]\((.+)\)$/);
      out.push(h('a', { href, target: '_blank', rel: 'noopener' }, label));
    }
    last = m.index + t.length;
  }
  if (last < text.length) out.push(text.slice(last));
  return out;
}

function markdown(src) {
  const blocks = [];
  const lines = (src || '').replace(/\r/g, '').split('\n');
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (line.startsWith('```')) {
      const code = [];
      for (i++; i < lines.length && !lines[i].startsWith('```'); i++) code.push(lines[i]);
      blocks.push(h('pre.output', code.join('\n')));
    } else if (/^\s*([-*]|\d+\.)\s+/.test(line)) {
      const ordered = /^\s*\d+\./.test(line);
      const items = [];
      for (; i < lines.length && /^\s*([-*]|\d+\.)\s+/.test(lines[i]); i++) items.push(h('li', inline(lines[i].replace(/^\s*([-*]|\d+\.)\s+/, ''))));
      i--;
      blocks.push(h(ordered ? 'ol' : 'ul', items));
    } else if (/^#{1,4}\s/.test(line)) {
      blocks.push(h('h3.md-h', inline(line.replace(/^#+\s/, ''))));
    } else if (line.trim()) {
      const para = [line];
      for (i++; i < lines.length && lines[i].trim() && !/^(```|#{1,4}\s|\s*([-*]|\d+\.)\s)/.test(lines[i]); i++) para.push(lines[i]);
      i--;
      blocks.push(h('p', inline(para.join(' '))));
    }
  }
  return blocks;
}

// ---- Page --------------------------------------------------------------------------------

export function renderAI(el, { menuButton, query }) {
  const sections = [['chat', 'Assistant'], ['model', 'Model'], ['mcp', 'MCP access']];
  let cur = query?.get('s') || 'chat';
  const body = h('div');
  const tabs = h('div.tabs', { role: 'tablist' });
  const drawTabs = () => clear(tabs, sections.map(([id, name]) => h('a.tab', { href: `#/ai?s=${id}`, role: 'tab',
    class: id === cur ? 'active' : '', 'aria-selected': String(id === cur) }, name)));
  clear(el,
    h('div.page-head', h('div.row', menuButton(), h('div', h('h1', 'AI'),
      h('div.sub', 'A built-in assistant on your own model server, and MCP access for AI clients like Claude Code.')))),
    tabs, body);
  drawTabs();
  let cleanup = () => {};
  const show = () => {
    cleanup();
    cleanup = ({ chat: chatView, model: modelView, mcp: mcpView }[cur] || chatView)(body, query) || (() => {});
  };
  show();
  return () => cleanup();
}

// ---- Assistant ---------------------------------------------------------------------------

function loadChat() {
  try { return JSON.parse(sessionStorage.getItem(STORE)) || { messages: [], pending: null }; } catch { return { messages: [], pending: null }; }
}
function saveChat(c) { try { sessionStorage.setItem(STORE, JSON.stringify(c)); } catch { /* full or unavailable */ } }

function chatView(el, query) {
  const chat = loadChat();
  let running = false, ctrl = null, info = null;
  const log = h('div.chat-log', { role: 'log', 'aria-live': 'polite' });
  const input = h('textarea.chat-input', { rows: 2, placeholder: 'Ask about your servers… (Enter to send, Shift+Enter for a new line)' });
  const send = h('button.btn.primary', { onclick: () => ask(input.value) }, 'Send');
  const stop = h('button.btn.hidden', { onclick: () => ctrl?.abort() }, 'Stop');
  const reset = h('button.btn.ghost.small', { onclick: () => { chat.messages = []; chat.pending = null; saveChat(chat); draw(); } }, 'New chat');
  const status = h('div.faint.chat-status');
  input.addEventListener('keydown', (e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); ask(input.value); } });
  const context = query?.get('ctx') || '';

  clear(el, h('div.card.chat',
    h('div.card-head', h('h2', 'Assistant'), h('div.row', status, reset)),
    log,
    h('div.chat-compose', input, h('div.stack', { style: { gap: '6px' } }, send, stop))));

  api.get('/api/assistant').then((r) => { info = r.settings; draw(); }).catch((e) => toast(e.message, true));
  let prompts = [];
  api.get('/api/tools').then((r) => { prompts = r.prompts || []; draw(); }).catch(() => {});

  // Results of tool calls, by call id, to show under each call.
  const results = () => new Map(chat.messages.filter((m) => m.role === 'tool').map((m) => [m.tool_call_id, m.content]));

  function callChip(tc, res) {
    let args = tc.function?.arguments;
    try { args = typeof args === 'string' ? JSON.parse(args || '{}') : args; } catch { /* shown as is */ }
    const short = Object.entries(args || {}).filter(([k]) => k !== 'compose').map(([k, v]) => `${k}=${typeof v === 'object' ? JSON.stringify(v) : v}`).join(' ');
    const failed = res?.startsWith('ERROR');
    return h('details.tool-chip', { class: failed ? 'failed' : res == null ? 'waiting' : '' },
      h('summary', h('span.mono', tc.function?.name), short ? h('span.faint.mono', ` ${short}`) : null,
        res == null ? h('span.faint', ' · waiting') : failed ? h('span.status.crit', { style: { marginLeft: '6px' } }, 'failed') : null),
      res != null ? h('pre.output', res) : null);
  }

  function draw() {
    const res = results();
    const items = [];
    if (info && !info.enabled) {
      items.push(h('div.notice.warn', 'The assistant isn\'t set up yet. ', h('a', { href: '#/ai?s=model' }, 'Choose a model server and model'),
        ' (Ollama or LM Studio on your network).'));
    }
    if (!chat.messages.length) {
      items.push(h('div.chat-empty',
        h('p.muted', 'Ask about your nodes, apps, logs and updates. It looks things up with the dashboard\'s tools; anything that changes something waits for your OK.'),
        h('div.row', prompts.filter((p) => !p.args?.length).map((p) => h('button.btn.small', { onclick: () => ask(p.text, p.title) }, p.title)),
          h('button.btn.small', { onclick: () => { input.value = 'Why is the app <name> on <node> not working?'; input.focus(); } }, 'Troubleshoot an app…'))));
    }
    for (const m of chat.messages) {
      if (m.role === 'user') items.push(h('div.msg.user', m.display || m.content));
      else if (m.role === 'assistant') {
        if (m.content) items.push(h('div.msg.assistant', markdown(m.content)));
        if (m.tool_calls?.length) items.push(h('div.tool-calls', m.tool_calls.map((tc) => callChip(tc, res.get(tc.id)))));
      }
    }
    if (chat.pending) items.push(pendingCard(chat.pending));
    if (chat.error) items.push(h('div.notice.warn', chat.error));
    if (running) items.push(h('div.msg.assistant.thinking', h('span.spinner'), 'Thinking…'));
    clear(log, items);
    log.scrollTop = log.scrollHeight;
    status.textContent = info?.enabled ? `${info.model} · up to ${info.maxScope}` : '';
    send.disabled = running; input.disabled = running;
    stop.classList.toggle('hidden', !running);
  }

  function pendingCard(p) {
    const yes = h('button.btn.primary', { onclick: () => decide(true) }, 'Approve');
    const no = h('button.btn', { onclick: () => decide(false) }, 'Decline');
    const args = { ...p.args };
    const compose = args.compose; delete args.compose;
    return h('div.card.approve',
      h('div.row', h('strong', 'Approve this change?'), scopeBadge(p.scope)),
      h('div', h('span.mono', p.name), h('span.muted', ` · ${p.title}`)),
      Object.keys(args).length ? h('dl.kv', Object.entries(args).map(([k, v]) => [h('dt', k), h('dd.mono', typeof v === 'object' ? JSON.stringify(v) : String(v))])) : null,
      compose ? h('details', h('summary', 'compose.yml'), h('pre.output', compose)) : null,
      h('div.row', yes, no));
  }

  function decide(allow) {
    const p = chat.pending;
    chat.pending = null;
    run({ [p.id]: allow });
  }

  function ask(text, display) {
    text = (text || '').trim();
    if (!text || running) return;
    input.value = '';
    chat.pending = null;
    chat.messages.push({ role: 'user', content: text, ...(display ? { display } : {}) });
    run(null);
  }

  async function run(approvals) {
    running = true; chat.error = '';
    draw();
    ctrl = new AbortController();
    try {
      const r = await fetch('/api/assistant/chat', { method: 'POST', credentials: 'same-origin', signal: ctrl.signal,
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': api.csrfToken() },
        body: JSON.stringify({ messages: chat.messages.map(({ display, ...m }) => m), approvals, context }) });
      if (!r.ok) {
        let msg = r.statusText;
        try { msg = (await r.json()).error || msg; } catch { /* not JSON */ }
        throw new Error(msg);
      }
      const reader = r.body.getReader();
      const dec = new TextDecoder();
      let buf = '';
      for (;;) {
        const { value, done } = await reader.read();
        if (done) break;
        buf += dec.decode(value, { stream: true });
        let nl;
        while ((nl = buf.indexOf('\n')) >= 0) {
          const line = buf.slice(0, nl).trim();
          buf = buf.slice(nl + 1);
          if (!line) continue;
          const e = JSON.parse(line);
          if (e.type === 'message' || e.type === 'tool') chat.messages.push(e.message);
          else if (e.type === 'pending') chat.pending = e.call;
          else if (e.type === 'error') chat.error = e.error;
          saveChat(chat);
          draw();
        }
      }
    } catch (e) {
      chat.error = e.name === 'AbortError' ? 'Stopped.' : e.message;
    } finally {
      running = false; ctrl = null;
      saveChat(chat);
      draw();
      input.focus();
    }
  }

  draw();
  const q = query?.get('q');
  if (q && !chat.messages.some((m) => m.content === q)) ask(q);
  else input.focus();
  return () => ctrl?.abort();
}

// ---- Model settings ----------------------------------------------------------------------

function modelView(el) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  clear(el, content);

  async function load() {
    try { draw(await api.get('/api/assistant')); } catch (e) { clear(content, h('div.notice.warn', e.message)); }
  }

  function draw({ settings: s, providers }) {
    const provider = h('select', [['ollama', 'Ollama'], ['lmstudio', 'LM Studio'], ['custom', 'Other (OpenAI-compatible)']].map(([v, l]) => h('option', { value: v }, l)));
    provider.value = s.provider || 'ollama';
    const url = h('input', { type: 'url', value: s.baseUrl || providers[provider.value] || '', spellcheck: false });
    const key = h('input', { type: 'password', autocomplete: 'off', placeholder: s.hasKey ? 'Stored; leave empty to keep it' : 'Only if your server needs one' });
    const model = h('select', h('option', { value: s.model || '' }, s.model || 'Load the models first'));
    model.value = s.model || '';
    const scope = h('select', SCOPES.map(([v, l]) => h('option', { value: v }, l)));
    scope.value = s.maxScope || 'operate';
    const scopeHelp = h('div.faint', { style: { fontSize: '12.5px' } });
    const syncScope = () => { scopeHelp.textContent = SCOPES.find(([v]) => v === scope.value)[2] + (scope.value === 'read' ? '' : ' Every change asks you first.'); };
    scope.addEventListener('change', syncScope); syncScope();
    let enabled = s.enabled;
    const on = switchEl(enabled, (v) => { enabled = v; }, 'Assistant on');
    const err = h('div.error-text');
    provider.addEventListener('change', () => { if (providers[provider.value]) url.value = providers[provider.value]; });

    const loadModels = h('button.btn', { onclick: async () => {
      err.textContent = '';
      const r = await busy(loadModels, () => api.post('/api/assistant/models', { baseUrl: url.value.trim(), apiKey: key.value }), '');
      if (!r) return;
      const was = model.value;
      clear(model, r.models.map((m) => h('option', { value: m }, m)));
      model.value = r.models.includes(was) ? was : r.models[0] || '';
      toast(`${r.models.length} model${r.models.length === 1 ? '' : 's'} found.`);
    } }, 'Test & load models');
    const save = h('button.btn.primary', { onclick: async () => {
      err.textContent = '';
      try {
        await api.post('/api/assistant/settings', { enabled, provider: provider.value, baseUrl: url.value.trim(), model: model.value, apiKey: key.value, maxScope: scope.value });
        toast('Assistant settings saved.');
        load();
      } catch (e) { err.textContent = e.message; }
    } }, 'Save');

    clear(content,
      h('div.card',
        h('div.card-head', h('h2', 'Model server'), h('div.row', on, h('span', 'Assistant on'))),
        h('p.muted', { style: { marginTop: 0 } }, 'The assistant runs on a model server you host: ',
          h('a', { href: 'https://ollama.com', target: '_blank', rel: 'noopener' }, 'Ollama'), ', ',
          h('a', { href: 'https://lmstudio.ai', target: '_blank', rel: 'noopener' }, 'LM Studio'),
          ' or anything with the OpenAI chat API. Your questions and what the tools return go only there. The main node talks to it, so ',
          h('code', '127.0.0.1'), ' means the main node itself.'),
        h('div.form-grid',
          h('label.field', 'Server', provider),
          h('label.field', { style: { gridColumn: 'span 2' } }, 'URL', url),
          h('label.field', 'API key', key)),
        h('div.form-grid', { style: { marginTop: '12px' } },
          h('label.field', { style: { gridColumn: 'span 2' } }, 'Model', model),
          h('label.field', 'Most it may do', scope),
          h('div', loadModels)),
        h('div', { style: { marginTop: '6px' } }, scopeHelp),
        err,
        h('div.row', { style: { marginTop: '12px' } }, save)),
      h('div.card', h('h2', 'Setting up a model server'),
        h('ul.plain.stack', { style: { gap: '8px' } },
          h('li', h('strong', 'Ollama: '), 'run it on a machine with a GPU, with ', h('code', 'OLLAMA_HOST=0.0.0.0'), ' so other machines reach it, then ',
            h('code', 'ollama pull qwen3:8b'), '. URL: ', h('code', 'http://<that machine>:11434/v1'), '. It also runs as a Docker app on any node.'),
          h('li', h('strong', 'LM Studio: '), 'Developer → start the server and turn on ', h('em', 'Serve on Local Network'), ', load a model. URL: ',
            h('code', 'http://<that machine>:1234/v1'), '.'),
          h('li', h('strong', 'Which model: '), 'one that supports tool calling. Qwen3 (8B or larger), Llama 3.1/3.3, Mistral Small and gpt-oss work well; '
            + 'very small models often pick the wrong tool.'))));
  }

  load();
}

// ---- MCP access --------------------------------------------------------------------------

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
  return [
    snippet('Claude Code', `claude mcp add --transport http homelab ${url} --header "Authorization: Bearer ${tok}"`),
    snippet('Claude Desktop, Cursor, VS Code and others (mcpServers JSON)', JSON.stringify({ mcpServers: { homelab: { type: 'http', url, headers: { Authorization: `Bearer ${tok}` } } } }, null, 2)),
    tlsNote ? h('div.notice', 'The dashboard\'s certificate is self-signed, so clients need to trust it: ',
      h('a', { href: '/ca.pem', download: 'homelab-dashboard.pem' }, 'download it'), ', then start Node-based clients (Claude Code, Claude Desktop) with ',
      h('code', 'NODE_EXTRA_CA_CERTS=~/homelab-dashboard.pem'), '. Check its fingerprint against Settings → Dashboard.') : null,
  ];
}

function mcpView(el) {
  const content = h('div.stack', h('div.card.empty', 'Loading…'));
  clear(el, content);

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
        h('div.card-head', h('h2', 'MCP tokens'), h('button.btn.primary', { onclick: newToken }, 'New token')),
        h('p.muted', { style: { marginTop: 0 } }, 'AI clients that speak the Model Context Protocol (Claude Code, Claude Desktop, Cursor…) connect to ',
          h('code', `${location.origin}/mcp`), ' with a token and get the same tools as the assistant, for every node.'),
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
}
