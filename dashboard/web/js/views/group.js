import { h, clear } from '../ui.js';

// A node tab made of sub-tabs: a .seg pill row like Docker's sections over one of the
// existing views, rendered with the same context. The sub-tab is in the address
// (#/n/<id>/<tab>/<sub>), so a reload or a shared link lands on it.
// sections: [[id, label, render], …]; the first is the default.
export function renderGroup(sections) {
  return (el, ctx) => {
    const cur = sections.find(([id]) => id === ctx.sub) || sections[0];
    const body = h('div');
    clear(el, h('div.stack',
      h('div.row', h('div.seg', { role: 'tablist' }, sections.map(([id, label]) =>
        h('button.seg-btn', { role: 'tab', 'aria-selected': String(id === cur[0]), class: id === cur[0] ? 'active' : '',
          onclick: () => { location.hash = `#/n/${encodeURIComponent(ctx.node.id)}/${ctx.tab}/${id}`; } }, label)))),
      body));
    return cur[2](body, ctx);
  };
}
