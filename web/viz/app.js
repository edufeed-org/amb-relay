const COLORS = {
  community: '#5c7fdf', author: '#e39a5c',
  project: '#4cae7a', measure: '#5fc0b0', publication: '#b98cd8',
};
const TYPE_LABEL = {
  community: 'Community', author: 'Author / publisher',
  project: 'Projekt (30143)', measure: 'Maßnahme (30144)', publication: 'Publikation (30145)',
};
const visible = { community: true, author: true, transferkiosk: true };
const TK_TYPES = new Set(['project', 'measure', 'publication']);

async function j(url) { const r = await fetch(url); if (!r.ok) throw new Error(url + ' ' + r.status); return r.json(); }

function esc(s) { return String(s ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c])); }

function renderStats(s) {
  const t = s.totals;
  document.getElementById('stats').innerHTML = [
    ['RESOURCES', t.resources], ['CONTENT TYPES', t.content_types],
    ['AUTHORS', t.authors], ['COMMUNITIES', t.communities], ['PROJECTS', t.projects],
  ].map(([l, v]) => `<div class="s"><span class="big">${v ?? 0}</span><span class="lbl">${l}</span></div>`).join('');
}

function renderLegend() {
  document.getElementById('legend').innerHTML =
    Object.entries(TYPE_LABEL).map(([k, lbl]) =>
      `<div><span class="dot" style="background:${COLORS[k]}"></span>${lbl}</div>`).join('') +
    `<div style="margin-top:6px;color:var(--muted)">thicker line = more shared content</div>`;
}

function nodeVisible(n) {
  if (n.type === 'community') return visible.community;
  if (n.type === 'author') return visible.author;
  if (TK_TYPES.has(n.type)) return visible.transferkiosk;
  return true;
}

let Graph, fullData;

function apply() {
  const nodes = fullData.nodes.filter(nodeVisible);
  const ids = new Set(nodes.map(n => n.id));
  const links = fullData.edges
    .filter(e => ids.has(e.source.id || e.source) && ids.has(e.target.id || e.target))
    .map(e => ({ source: e.source.id || e.source, target: e.target.id || e.target, weight: e.weight }));
  Graph.graphData({ nodes, links });
}

function renderToggles() {
  const el = document.getElementById('toggles');
  const rows = [['community', 'Communities'], ['author', 'Authors'], ['transferkiosk', 'Transferkiosk']];
  el.innerHTML = rows.map(([k, lbl]) =>
    `<label><input type="checkbox" data-k="${k}" checked> ${lbl}</label>`).join('');
  el.querySelectorAll('input').forEach(cb => cb.addEventListener('change', () => {
    visible[cb.dataset.k] = cb.checked; apply();
  }));
}

async function openPanel(node) {
  const [typ] = node.id.split(':');
  const ref = node.id.slice(typ.length + 1);
  const panel = document.getElementById('panel');
  panel.classList.remove('hidden');
  panel.innerHTML = `<span class="close">✕</span><h3>${esc(node.label)}</h3><div class="sub">loading…</div>`;
  panel.querySelector('.close').onclick = () => panel.classList.add('hidden');
  try {
    const items = await j(`/viz/node/${encodeURIComponent(node.type)}/${encodeURIComponent(ref)}`);
    const list = (items || []).map(it =>
      `<div class="res"><div>${esc(it.name || '(untitled)')}</div><div class="m">${esc(it.kind || it.eventKind || '')}</div></div>`).join('');
    panel.innerHTML = `<span class="close">✕</span><h3>${esc(node.label)}</h3>` +
      `<div class="sub">${esc(node.type)} · ${(items || []).length} items</div>${list || '<div class="sub">no items</div>'}`;
    panel.querySelector('.close').onclick = () => panel.classList.add('hidden');
  } catch (e) {
    panel.innerHTML = `<span class="close" onclick="this.parentElement.classList.add('hidden')">✕</span><h3>${esc(node.label)}</h3><div class="sub">error: ${esc(e.message)}</div>`;
  }
}

async function main() {
  renderLegend(); renderToggles();
  renderStats(await j('/viz/stats'));
  fullData = await j('/viz/graph');

  Graph = ForceGraph()(document.getElementById('graph'))
    .backgroundColor('#0f1216')
    .nodeColor(n => COLORS[n.type] || '#888')
    .nodeVal(n => Math.max(2, Math.sqrt(n.weight || 1) * 2))
    .nodeLabel(n => `${n.label} (${n.weight})`)
    .nodeCanvasObjectMode(() => 'after')
    .nodeCanvasObject((n, ctx, scale) => {
      const label = n.label || '';
      ctx.font = `${Math.max(3, 11 / scale)}px system-ui`;
      ctx.fillStyle = '#c8ccd2';
      ctx.textAlign = 'center';
      ctx.fillText(label, n.x, n.y + 10 / scale);
    })
    .linkWidth(l => Math.max(1, Math.sqrt(l.weight || 1)))
    .linkColor(() => 'rgba(140,160,200,.35)')
    .onNodeClick(openPanel);

  apply();
}
main().catch(e => { document.getElementById('graph').innerHTML = '<p style="padding:20px">' + esc(e.message) + '</p>'; });
