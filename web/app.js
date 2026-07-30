'use strict';

/* ── state ──────────────────────────────────────────────────────────── */
const S = {
  bases: [], base: null, sources: [], source: null, tasks: {},
  tree: [], file: null, preview: null, selChunk: -1, treeFilter: '', spans: [],
  editing: null,
  // Bumped whenever the active base changes. Every async load captures it and
  // discards its result if it no longer matches, so a slow response for the
  // previous base cannot land in the new one's view.
  gen: 0,
  previewGen: 0,
};
const $ = (id) => document.getElementById(id);
const esc = (s) => String(s ?? '').replace(/[&<>"]/g, (c) =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]));
const n = (v) => (v ?? 0).toLocaleString();

/* ── http ───────────────────────────────────────────────────────────── */
async function api(method, path, body) {
  const res = await fetch(path, {
    method,
    headers: body !== undefined ? { 'Content-Type': 'application/json' } : {},
    body: body !== undefined ? JSON.stringify(body) : undefined,
  });
  const text = await res.text();
  let data = {};
  if (text) { try { data = JSON.parse(text); } catch { data = { error: text }; } }
  if (!res.ok) throw new Error(data.error || `${res.status} ${res.statusText}`);
  return data;
}

function toast(title, msg, kind) {
  const el = document.createElement('div');
  el.className = 'toast' + (kind ? ' ' + kind : '');
  el.innerHTML = `<b>${esc(title)}</b><span>${esc(msg || '')}</span>`;
  $('toasts').appendChild(el);
  setTimeout(() => el.remove(), kind === 'err' ? 9000 : 4500);
}
const fail = (e) => toast('操作失败', e.message, 'err');

/* Acknowledge a click before the work finishes.
   Every action here costs several round trips, and on a slow link a button
   that neither disables nor changes reads as "nothing happened" — which is
   exactly how a successful save gets reported as a bug. */
async function withBusy(btn, busyLabel, fn) {
  if (!btn || btn.disabled) return;
  const label = btn.textContent;
  btn.disabled = true;
  btn.textContent = busyLabel;
  try {
    return await fn();
  } finally {
    btn.disabled = false;
    btn.textContent = label;
  }
}

/* ── navigation ─────────────────────────────────────────────────────── */
function show(name) {
  document.querySelectorAll('.plate').forEach((p) => p.classList.toggle('on', p.id === 'plate-' + name));
  document.querySelectorAll('.nav[data-plate]').forEach((b) => b.classList.toggle('on', b.dataset.plate === name));
  if (name === 'chunks') { loadTree(); requestAnimationFrame(drawConnectors); }
}
document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-plate]');
  if (b && !b.disabled) { e.preventDefault(); show(b.dataset.plate); }
});

/* Light by default rather than following the OS: this is a working surface
   people keep open next to an editor, and a tool that flips appearance with
   the system schedule is disorienting. The choice is remembered once made. */
(function initTheme() {
  let saved = null;
  try { saved = localStorage.getItem('wb-theme'); } catch { /* private mode */ }
  document.documentElement.setAttribute('data-theme', saved === 'dark' ? 'dark' : 'light');
})();

$('themeBtn').addEventListener('click', () => {
  const root = document.documentElement;
  const next = root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark';
  root.setAttribute('data-theme', next);
  try { localStorage.setItem('wb-theme', next); } catch { /* private mode */ }
});

/* ── bases ──────────────────────────────────────────────────────────── */
async function loadBases() {
  const { bases } = await api('GET', '/api/bases');
  S.bases = bases || [];
  $('navBaseCount').textContent = S.bases.length || '';
  $('basesSub').textContent = S.bases.length ? `${S.bases.length} 个知识库` : '';
  renderBases();
}

function renderBases() {
  if (!S.bases.length) {
    $('basesBody').innerHTML = `<div class="empty">
      <h3>还没有知识库</h3>
      <p>知识库是 source、向量索引和图谱的归属单位。<br>先建一个，再往里加 source。</p>
      <button class="btn pri" onclick="document.getElementById('newBaseBtn').click()">+ New base</button>
    </div>`;
    return;
  }
  $('basesBody').innerHTML = '<div class="kb-grid">' + S.bases.map((b) => {
    const ready = b.embedder && b.embedder.model;
    const tone = !ready ? 'var(--faint)' : b.chunk_count ? 'var(--ok)' : 'var(--warn)';
    const status = !ready
      ? '<span class="chip"><span class="dot"></span>未配置 embedder</span>'
      : b.chunk_count
        ? '<span class="chip ok"><span class="dot ok"></span>ready</span>'
        : '<span class="chip warn"><span class="dot warn"></span>未入库</span>';
    return `<button class="kb-card" style="--st:${tone}" data-base="${esc(b.id)}" type="button">
      <div class="kb-top">
        <div style="flex:1">
          <h3>${esc(b.name)}</h3>
          <p>${esc(b.description || '没有描述')}</p>
        </div>${status}
      </div>
      <div class="kb-chips">
        <span class="chip cy">${esc(b.vector_store?.type || 'inmemory')}</span>
        ${ready ? `<span class="chip">${esc(b.embedder.model)}${b.embedder.dimensions ? ' · ' + b.embedder.dimensions + 'd' : ''}</span>` : ''}
        <span class="chip">rev ${b.index_revision}</span>
      </div>
      <div class="kb-stats">
        <div class="kb-stat"><span class="lbl">sources</span><b>${n(b.source_count)}</b></div>
        <div class="kb-stat"><span class="lbl">chunks</span><b>${n(b.chunk_count)}</b></div>
        <div class="kb-stat"><span class="lbl">docs</span><b>${n(b.doc_count)}</b></div>
      </div>
    </button>`;
  }).join('') + '</div>';
}

document.addEventListener('click', (e) => {
  const card = e.target.closest('[data-base]');
  if (card) selectBase(card.dataset.base, 'sources');
});

async function selectBase(id, plate) {
  // Invalidate anything in flight for the previous base before touching state,
  // so a response that arrives late is discarded rather than mixed in.
  const gen = ++S.gen;
  clearBaseScopedState();
  const base = await api('GET', '/api/bases/' + id);
  if (gen !== S.gen) return;          // superseded while fetching
  S.base = base;
  try { localStorage.setItem('wb-base', id); } catch { /* private mode */ }
  applyBaseToChrome();
  renderSettings();
  renderKbPop();
  await loadSources();
  loadCapabilities().catch(() => {});
  if (plate) show(plate);
}

// Drop everything scoped to the previous base in one step.
//
// Switching bases used to leave the inspector showing the old base's file,
// preview and source text, which read as the new base already having content.
function clearBaseScopedState() {
  S.sources = [];
  S.source = null;
  S.tasks = {};
  S.editing = null;
  S.caps = null;
  S.tree = [];
  S.file = null;
  S.fileSource = null;
  S.preview = null;
  S.content = '';
  S.spans = [];
  S.selChunk = -1;
  S.metaKeys = [];
  S.filters = [];
  S.norm = null;
  S.normFor = null;
  $('ciTree').innerHTML = '';
  $('ciChunks').innerHTML = '';
  $('ciDoc').textContent = '';
  $('ciMetrics').innerHTML = '';
  $('ciBanner').innerHTML = '';
  $('ciCrumb').textContent = '选择左侧文件';
  $('ciApplyBtn').disabled = true;
  $('srcBody').innerHTML = '';
  $('srcSide').innerHTML = '';
  $('rtBody').innerHTML = '';
  $('rtFilters').innerHTML = '';
  drawConnectors();
}

// Push the current base into the shell: title bar, rail label, readout, and
// the scoped nav's enabled state. Shared so saving settings can refresh the
// chrome without reloading every list behind it.
function applyBaseToChrome() {
  const b = S.base;
  if (!b) return;
  $('kbWrap').hidden = false;
  $('kbName').textContent = b.name;
  $('kbDot').className = 'dot ' + (b.embedder?.model ? 'ok' : '');
  $('railBase').textContent = b.name;
  $('roStore').textContent = b.vector_store?.type || 'inmemory';
  $('roEmbed').textContent = b.embedder?.model
    ? b.embedder.model + (b.embedder.dimensions ? ' · ' + b.embedder.dimensions + 'd' : '')
    : '未配置';
  document.querySelectorAll('.nav.scoped').forEach((n) => {
    n.disabled = false;
    n.removeAttribute('title');
  });
  $('srcSub').textContent = b.name;
  $('rtSub').textContent = b.name;
}

/* The caret promises a menu, so give it one — switching bases is the single
   most common navigation and should not require a round trip through the
   bases plate. */
function renderKbPop() {
  $('kbPop').innerHTML = S.bases.map((b) => {
    const ready = b.embedder && b.embedder.model;
    return `<button class="pop-item" type="button" role="menuitem" data-goto="${esc(b.id)}">
      <span class="dot ${ready ? 'ok' : ''}"></span>
      <span class="tx"><b>${esc(b.name)}</b>
        <span class="m">${esc(b.vector_store?.type || 'inmemory')} · ${n(b.chunk_count)} chunks</span></span>
      ${S.base && b.id === S.base.id ? '<span class="tick">✓</span>' : ''}
    </button>`;
  }).join('') + `<div class="pop-sep"></div>
    <button class="pop-item" type="button" role="menuitem" data-plate="bases">
      <span class="tx"><b>所有知识库…</b></span></button>
    <button class="pop-item" type="button" role="menuitem" data-plate="settings">
      <span class="tx"><b>当前库设置…</b></span></button>`;
}

const closeKbPop = () => {
  $('kbPop').classList.remove('on');
  $('kbSwitch').setAttribute('aria-expanded', 'false');
};
$('kbSwitch').addEventListener('click', (e) => {
  e.stopPropagation();
  const open = $('kbPop').classList.toggle('on');
  $('kbSwitch').setAttribute('aria-expanded', open ? 'true' : 'false');
});
$('kbPop').addEventListener('click', async (e) => {
  e.stopPropagation();
  const item = e.target.closest('[data-goto]');
  closeKbPop();
  if (item) { try { await selectBase(item.dataset.goto, 'sources'); } catch (err) { fail(err); } }
});
document.addEventListener('click', closeKbPop);
document.addEventListener('keydown', (e) => { if (e.key === 'Escape') closeKbPop(); });

$('newBaseBtn').addEventListener('click', () => {
  openModal('新建知识库', `
    <div class="frow"><span class="fl">名称</span><div class="fctl">
      <input class="inp" id="f_name" style="width:100%" placeholder="trpc-agent-go / code"></div></div>
    <div class="frow"><span class="fl">描述<small>agent 选库时会读它</small></span><div class="fctl">
      <input class="inp" id="f_desc" style="width:100%"></div></div>
    <div class="frow"><span class="fl">Vector store</span><div class="fctl">
      <span class="chip cy">inmemory</span>
      <span class="hint">v0 只接了 inmemory；pgvector 等会在建库后报明确错误</span></div></div>
    <div class="frow"><span class="fl">Embedding model<small>留空则只能用分块预览，不能入库</small></span>
      <div class="fctl"><input class="inp" id="f_model" placeholder="bge-m3" style="width:150px">
      <input class="inp num" id="f_dims" placeholder="1024" style="width:80px" aria-label="dimensions"></div></div>
    <div class="frow"><span class="fl">Base URL</span><div class="fctl">
      <input class="inp mono" id="f_base" placeholder="http://127.0.0.1:11434/v1" style="width:100%"></div></div>
    <div class="frow"><span class="fl">API key</span><div class="fctl">
      <input class="inp mono" id="f_key" placeholder="ollama" style="width:100%"></div></div>`,
    async () => {
      const name = $('f_name').value.trim();
      if (!name) throw new Error('名称不能为空');
      const b = await api('POST', '/api/bases', {
        name,
        description: $('f_desc').value.trim(),
        vector_store: { type: 'inmemory' },
        embedder: {
          provider: 'openai',
          model: $('f_model').value.trim(),
          dimensions: parseInt($('f_dims').value, 10) || 0,
          base_url: $('f_base').value.trim(),
          api_key: $('f_key').value.trim(),
        },
        enable_source_sync: true,
      });
      await loadBases();
      await selectBase(b.id, 'sources');
      toast('已创建', b.name, 'ok');
    });
});

/* ── settings ───────────────────────────────────────────────────────── */
function renderSettings() {
  const b = S.base;
  if (!b) return;
  $('setTitle').textContent = b.name;
  $('setId').textContent = b.id;
  $('setOverview').innerHTML = `
    <div class="met"><span class="lbl">sources</span><b>${n(b.source_count)}</b><span class="d">配置中</span></div>
    <div class="met"><span class="lbl">chunks</span><b>${n(b.chunk_count)}</b><span class="d">已入库</span></div>
    <div class="met"><span class="lbl">index revision</span><b>${b.index_revision}</b>
      <span class="d">改 embedder 会 +1</span></div>
    <div class="met"><span class="lbl">source sync</span><b>${b.enable_source_sync ? 'on' : 'off'}</b>
      <span class="d">增量同步</span></div>`;
  const e = b.embedder || {};
  const d = b.source_defaults || {};
  $('setBody').innerHTML = `
    <section class="set-sec">
      <h3>General</h3>
      <p class="d">知识库是 source、向量索引与图谱的归属单位。这里的改动只影响本库。</p>
      <div class="frow"><span class="fl">名称</span><div class="fctl">
        <input class="inp" id="s_name" value="${esc(b.name)}" style="width:320px"></div></div>
      <div class="frow"><span class="fl">描述</span><div class="fctl">
        <input class="inp" id="s_desc" value="${esc(b.description)}" style="width:100%"></div></div>
      <div class="frow"><span class="fl">Base ID</span><div class="fctl">
        <span class="chip">${esc(b.id)}</span><span class="hint">创建后不可修改</span></div></div>
    </section>

    <section class="set-sec">
      <h3>Storage</h3>
      <p class="d">向量维度由 embedder 决定并锁定列宽。改维度需要重新嵌入。</p>
      <div class="frow"><span class="fl">Vector store</span><div class="fctl">
        <span class="chip cy">${esc(b.vector_store?.type || 'inmemory')}</span>
        <span class="hint">v0 只接了 inmemory</span></div></div>
      <div class="frow"><span class="fl">维度<small>由 embedder 决定</small></span><div class="fctl">
        <span class="chip">${e.dimensions || '—'}</span></div></div>
    </section>

    <section class="set-sec">
      <h3>Embedding</h3>
      <p class="d">改动 model 或 dimensions 会让已有向量失效，保存时 index revision 会 +1。</p>
      <div class="frow"><span class="fl">Model</span><div class="fctl">
        <input class="inp mono" id="s_model" value="${esc(e.model || '')}" style="width:220px" placeholder="bge-m3"></div></div>
      <div class="frow"><span class="fl">Dimensions</span><div class="fctl">
        <input class="inp num" id="s_dims" value="${e.dimensions || ''}" style="width:100px"></div></div>
      <div class="frow"><span class="fl">Base URL</span><div class="fctl">
        <input class="inp mono" id="s_base" value="${esc(e.base_url || '')}" style="width:100%"
               placeholder="http://127.0.0.1:11434/v1"></div></div>
      <div class="frow"><span class="fl">API key<small>${e.api_key_configured ? '已配置，留空表示不修改' : '尚未配置'}</small></span>
        <div class="fctl">
        <input class="inp mono" id="s_key" type="password" value="" autocomplete="off"
               placeholder="${e.api_key_configured ? '••••••••（留空保持不变）' : 'sk-… 或 ollama'}"
               style="width:100%"></div></div>
    </section>

    <section class="set-sec">
      <h3>Chunking 默认值</h3>
      <p class="d">只用来预填新建 source 的表单，<b>不参与运行时</b>。每个 source 自带完整参数，
        改这里不会影响已有 source —— 否则改一个默认值就会静默改变一堆 source 的分块结果。</p>
      <div class="frow"><span class="fl">Strategy</span><div class="fctl">
        <select class="inp" id="s_strategy">
          <option value="" ${!d.strategy ? 'selected' : ''}>default（跟随 reader）</option>
          ${['recursive', 'fixed', 'markdown', 'json'].map((v) =>
            `<option value="${v}" ${d.strategy === v ? 'selected' : ''}>${v}</option>`).join('')}
        </select>
        <span class="hint">markdown reader → markdown，text/csv → fixed，json → json</span></div></div>
      <div class="frow"><span class="fl">Chunk size</span><div class="fctl">
        <input class="inp num" id="s_size" value="${d.chunk_size || 1024}" style="width:100px">
        <span class="chip">runes</span>
        <span class="hint">JSON 策略下单位是序列化字节</span></div></div>
      <div class="frow"><span class="fl">Overlap<small>计入 chunk size 预算</small></span><div class="fctl">
        <input class="inp num" id="s_ov" value="${d.chunk_overlap ?? 0}" style="width:100px"></div></div>
    </section>

    <section class="set-sec">
      <h3>Retrieval 默认值</h3>
      <div class="frow"><span class="fl">top-k</span><div class="fctl">
        <input class="inp num" id="s_topk" value="${b.retrieval?.top_k || 5}" style="width:100px"></div></div>
    </section>

    <section class="set-sec">
      <h3 style="color:var(--crit)">危险操作</h3>
      <p class="d">删除会连同 source 配置与向量一并移除，不可撤销。</p>
      <div class="danger">
        <div class="frow"><span class="fl">删除知识库</span><div class="fctl">
          <button class="btn dng" id="s_delete" type="button">Delete base</button>
          <span class="hint">需要输入库名确认</span></div></div>
      </div>
    </section>`;

  $('s_delete').addEventListener('click', async () => {
    const typed = prompt(`输入库名以确认删除：\n${b.name}`);
    if (typed !== b.name) return;
    try {
      await api('DELETE', '/api/bases/' + b.id);
      S.base = null;
      $('kbSwitch').hidden = true;
      $('railBase').textContent = '未选择知识库';
      document.querySelectorAll('.nav.scoped').forEach((x) => (x.disabled = true));
      await loadBases();
      show('bases');
      toast('已删除', b.name, 'ok');
    } catch (e) { fail(e); }
  });
}

$('saveBaseBtn').addEventListener('click', () => withBusy($('saveBaseBtn'), '保存中…', async () => {
  if (!S.base) return;
  try {
    const r = await api('PATCH', '/api/bases/' + S.base.id, {
      name: $('s_name').value.trim(),
      description: $('s_desc').value.trim(),
      vector_store: S.base.vector_store,
      embedder: {
        provider: 'openai',
        model: $('s_model').value.trim(),
        dimensions: parseInt($('s_dims').value, 10) || 0,
        base_url: $('s_base').value.trim(),
        api_key: $('s_key').value.trim(),
      },
      enable_source_sync: S.base.enable_source_sync,
      source_defaults: {
        strategy: $('s_strategy').value,
        chunk_size: parseInt($('s_size').value, 10) || 1024,
        chunk_overlap: parseInt($('s_ov').value, 10) || 0,
      },
      retrieval: { top_k: parseInt($('s_topk').value, 10) || 5 },
    });
    // Report before refreshing. Reloading the base pulls the source list, the
    // task list and the file tree behind it; waiting for all of that turns a
    // successful save into seconds of silence.
    toast('已保存', r.reindex_required
      ? `embedding 配置变了，index revision → ${r.base.index_revision}，已有向量需要重新入库`
      : '设置已写入', 'ok');
    S.base = r.base;
    renderSettings();
    applyBaseToChrome();
    await loadBases();
    renderKbPop();
  } catch (e) { fail(e); }
}));

/* ── sources ────────────────────────────────────────────────────────── */
const TYPE_TONE = { dir: 'cyan', file: 'ochre', url: 'overprint', repo: 'magenta', auto: 'cyan' };
const STATUS_CHIP = {
  synced: '<span class="chip ok"><span class="dot ok"></span>synced</span>',
  stale: '<span class="chip warn"><span class="dot warn"></span>配置已改</span>',
  never_synced: '<span class="chip"><span class="dot"></span>未入库</span>',
  failed: '<span class="chip crit"><span class="dot crit"></span>failed</span>',
};

async function loadSources() {
  if (!S.base) return;
  // Read the id once: it is awaited across two requests, and re-reading it
  // after a switch would pair one base's sources with another's tasks.
  const gen = S.gen, baseID = S.base.id;
  const { sources } = await api('GET', `/api/bases/${baseID}/sources`);
  if (gen !== S.gen) return;
  const { tasks } = await api('GET', `/api/bases/${baseID}/tasks`);
  if (gen !== S.gen) return;
  S.sources = sources || [];
  $('navSrcCount').textContent = S.sources.length || '';
  S.tasks = {};
  (tasks || []).forEach((t) => {
    if (t.source_id && !S.tasks[t.source_id]) S.tasks[t.source_id] = t;
  });
  renderSources();
}

function inputSummary(s) {
  const c = s.config || {};
  const parts = c.paths || c.dirs || c.urls || c.inputs ||
    (c.repository ? [c.repository.url || c.repository.dir] : []);
  return (parts || []).join(', ');
}

function renderSources() {
  if (!S.sources.length) {
    $('srcBody').innerHTML = `<div class="empty">
      <h3>这个库还没有 source</h3>
      <p>source 描述一批待入库的内容：一个目录、一个文件、一个仓库或一组 URL。</p>
      <button class="btn pri" onclick="document.getElementById('addSrcBtn').click()">+ Add source</button>
    </div>`;
    $('srcSide').innerHTML = '';
    return;
  }
  $('srcBody').innerHTML = `<table class="tbl"><thead><tr>
      <th>Source</th><th>Type</th><th>Chunking</th>
      <th style="text-align:right">Chunks</th><th>Status</th></tr></thead><tbody>` +
    S.sources.map((row) => {
      const s = row.source;
      const t = S.tasks[s.id];
      const running = t && (t.state === 'running' || t.state === 'queued');
      const c = s.config || {};
      const chunking = row.supports_chunking
        ? `<span class="chip">${esc(c.strategy || 'default')} ${c.chunk_size}/${c.chunk_overlap || 0}</span>`
        : '<span class="chip op">AST 抽取 · 无分块参数</span>';
      let status = STATUS_CHIP[row.sync_status] || '';
      if (running) {
        const p = t.progress || {};
        const pct = p.source_total ? Math.round((p.source_processed / p.source_total) * 100) : 0;
        status = `<span class="chip cy"><span class="dot run"></span>${t.state === 'queued' ? 'queued' : pct + '%'}</span>
          <div class="prog"><i style="width:${pct}%"></i></div>`;
      }
      return `<tr data-src="${esc(s.id)}" class="${S.source === s.id ? 'on' : ''}">
        <td><div class="src-name">
          <span class="tico" style="background:var(--${TYPE_TONE[s.type]}-soft);color:var(--${TYPE_TONE[s.type]})">${esc(s.type.toUpperCase())}</span>
          <span><b>${esc(s.display_name)}</b><span class="p">${esc(inputSummary(s))}</span></span></div></td>
        <td><span class="chip">${esc(c.reader_type || 'auto')}</span></td>
        <td>${chunking}</td>
        <td class="n">${n(s.chunk_count)}</td>
        <td>${status}</td></tr>`;
    }).join('') + '</tbody></table>';
  renderSourceSide();
}

document.addEventListener('click', (e) => {
  const tr = e.target.closest('tr[data-src]');
  if (!tr || tr.dataset.src === S.source) return;
  S.editing = null;                 // moving to another source abandons the edit
  S.source = tr.dataset.src;
  renderSources();
});

const READER_OPTS = ['text', 'markdown', 'json', 'csv', 'go', 'proto', 'pdf', 'docx'];
const STRATEGY_OPTS = ['recursive', 'fixed', 'markdown', 'json'];
const opt = (v, cur, label) =>
  `<option value="${v}" ${cur === v ? 'selected' : ''}>${label || v}</option>`;

function renderSourceSide() {
  const row = S.sources.find((r) => r.source.id === S.source) || S.sources[0];
  if (!row) { $('srcSide').innerHTML = ''; return; }
  // A live sync republishes the source list every second; rebuilding the panel
  // underneath a half-typed edit would throw the edit away.
  if (S.editing && S.editing === row.source.id && $('e_size')) return;
  S.source = row.source.id;
  const s = row.source;
  const c = s.config || {};
  const t = S.tasks[s.id];
  const running = t && (t.state === 'running' || t.state === 'queued');
  const chunkable = row.supports_chunking;

  $('srcSide').innerHTML = `
    <div class="side-head">
      <h3>${esc(s.display_name)}</h3>
      <div class="p">${esc(s.id)}</div>
      <div style="display:flex;gap:5px;margin-top:9px;flex-wrap:wrap">
        <span class="chip cy">${esc(s.type)} source</span>${STATUS_CHIP[row.sync_status] || ''}
        <span class="chip">${n(s.chunk_count)} chunks</span>
      </div>
    </div>
    <div class="side-body">
      <div class="side-grp"><span class="lbl">输入<small style="text-transform:none;letter-spacing:0"> · 一行一个</small></span>
        <textarea class="inp" id="e_input" rows="2" style="width:100%">${esc(inputList(s).join('\n'))}</textarea>
      </div>

      ${chunkable ? `
      <div class="side-grp"><span class="lbl">Chunking</span>
        <div class="sform">
          <label>Strategy
            <select class="inp" id="e_strategy">
              ${opt('', c.strategy || '', 'default（跟随 reader）')}
              ${STRATEGY_OPTS.map((v) => opt(v, c.strategy || '')).join('')}
            </select></label>
          <label>Reader
            <select class="inp" id="e_reader">
              ${opt('', c.reader_type || '', 'auto（按扩展名）')}
              ${READER_OPTS.map((v) => opt(v, c.reader_type || '')).join('')}
            </select></label>
          <div class="sform-2">
            <label>Chunk size<input class="inp num" id="e_size" value="${c.chunk_size ?? 1024}"></label>
            <label>Overlap<input class="inp num" id="e_ov" value="${c.chunk_overlap ?? 0}"></label>
          </div>
          <p class="shint">overlap 计入 chunk size 预算。上游默认 1024/0，overlap 需要显式开启。</p>
        </div>
      </div>` : `
      <div class="side-grp"><span class="lbl">Chunking</span>
        <div class="banner warn" style="margin:0">repo source 走 AST 抽取，上游不接受分块参数。</div>
      </div>`}

      ${s.type === 'dir' ? `
      <div class="side-grp"><span class="lbl">目录选项</span>
        <div class="sform">
          <label>扩展名过滤<input class="inp mono" id="e_ext" placeholder="留空 = 全部"
            value="${esc((c.file_extensions || []).join(', '))}"></label>
          <label class="srow"><input type="checkbox" id="e_rec" ${c.recursive !== false ? 'checked' : ''}
            style="accent-color:var(--cyan)">遍历子目录</label>
        </div>
      </div>` : ''}

      <div class="side-grp" id="e_saveRow">
        <button class="btn pri" id="sideSave" type="button" style="width:100%;justify-content:center">
          保存配置</button>
        <p class="shint" style="margin-top:7px">改动只写数据库，需要重新同步才会生效。</p>
      </div>

      ${row.needs_sync && !s.last_error ? `<div class="side-grp">
        <div class="banner warn" style="margin:0">配置与上次入库的内容不一致，重新同步后才生效。</div></div>` : ''}
      ${s.last_error ? `<div class="side-grp"><span class="lbl">最近错误</span>
        <div class="err-box">${esc(s.last_error)}</div></div>` : ''}

      <div class="side-grp">
        <span class="lbl">文件 <span id="e_fileCount" style="color:var(--faint)"></span></span>
        <div id="e_files"><p class="shint">加载中…</p></div>
      </div>

      <div class="side-grp"><span class="lbl">同步</span>
        <dl class="kv">
          <dt>Last sync</dt><dd>${s.last_synced_at ? new Date(s.last_synced_at).toLocaleString() : '从未'}</dd>
          <dt>Config hash</dt><dd>${esc((s.config_hash || '').slice(0, 12))}</dd>
        </dl>
      </div>
    </div>
    <div class="side-foot">
      <button class="btn" id="sideSync" type="button" ${running ? 'disabled' : ''} style="flex:1">
        ${running ? '同步中…' : '⟳ Sync'}</button>
      ${running ? '<button class="btn" id="sideCancel" type="button">中止</button>' : ''}
      <button class="btn dng" id="sideDel" type="button">Delete</button>
    </div>`;

  // Any keystroke marks the panel as being edited so live updates leave it be.
  $('srcSide').querySelectorAll('input,select,textarea').forEach((el) => {
    el.addEventListener('input', () => { S.editing = s.id; });
    el.addEventListener('change', () => { S.editing = s.id; });
  });

  $('sideSave')?.addEventListener('click', () => saveSourceConfig(s));
  $('sideSync')?.addEventListener('click', async () => {
    try { S.editing = null; await api('POST', `/api/sources/${s.id}/sync`); await loadSources(); }
    catch (e) { fail(e); }
  });
  $('sideCancel')?.addEventListener('click', async () => {
    try { await api('POST', `/api/tasks/${t.id}/cancel`); } catch (e) { fail(e); }
  });
  $('sideDel')?.addEventListener('click', async () => {
    if (!confirm(`删除 source「${s.display_name}」？\n已写入的向量会一并删除。`)) return;
    try {
      S.editing = null;
      await api('DELETE', `/api/sources/${s.id}`);
      S.source = null;
      setTimeout(loadSources, 600);
    } catch (e) { fail(e); }
  });

  renderSourceFiles(s, chunkable);
}

// The exact input list, not the joined summary, so it round-trips through the
// textarea without mangling paths that contain a comma.
function inputList(s) {
  const c = s.config || {};
  if (c.paths) return c.paths;
  if (c.dirs) return c.dirs;
  if (c.urls) return c.urls;
  if (c.inputs) return c.inputs;
  if (c.repository) return [c.repository.url || c.repository.dir];
  return [];
}

// The files a source would actually read. Clicking one opens it in the chunk
// inspector with this source's parameters already applied.
async function renderSourceFiles(s, chunkable) {
  const box = $('e_files');
  if (!box) return;
  if (!chunkable) {
    box.innerHTML = '<p class="shint">repo source 由上游克隆并做 AST 抽取，这里无法列出文件。</p>';
    return;
  }
  try {
    const r = await api('GET', `/api/sources/${s.id}/tree`);
    const files = r.entries || [];
    const cnt = $('e_fileCount');
    if (cnt) cnt.textContent = `(${files.length})`;
    if (!files.length) {
      box.innerHTML = '<p class="shint">没有匹配到文件。检查路径和扩展名过滤。</p>';
      return;
    }
    box.innerHTML = `<div class="flist">` + files.map((f) => `
      <button class="frow-item" type="button" data-open="${esc(f.path)}" data-src="${esc(s.id)}"
              title="${esc(f.path)}">
        <span class="fn">${esc(f.name)}</span>
        <span class="n">${(f.size / 1024).toFixed(1)}k</span>
      </button>`).join('') + `</div>`;
  } catch (e) {
    box.innerHTML = `<p class="shint" style="color:var(--crit)">${esc(e.message)}</p>`;
  }
}

// Open a file from the source panel straight in the inspector.
document.addEventListener('click', (e) => {
  const b = e.target.closest('[data-open]');
  if (!b) return;
  S.file = b.dataset.open;
  S.fileSource = b.dataset.src;
  const row = S.sources.find((r) => r.source.id === b.dataset.src);
  if (row) {
    const c = row.source.config || {};
    $('ciStrategy').value = c.strategy || '';
    $('ciReader').value = c.reader_type || '';
    if (c.chunk_size) { $('ciSize').value = c.chunk_size; $('ciSizeV').textContent = c.chunk_size; }
    $('ciOv').value = c.chunk_overlap || 0;
    $('ciOvV').textContent = c.chunk_overlap || 0;
  }
  $('ciCrumb').textContent = S.file;
  $('ciApplyBtn').disabled = false;
  show('chunks');
  runPreview();
});

async function saveSourceConfig(s) {
  const lines = $('e_input').value.split('\n').map((x) => x.trim()).filter(Boolean);
  if (!lines.length) { fail(new Error('输入不能为空')); return; }
  const config = {};
  if (s.type === 'file') config.paths = lines;
  else if (s.type === 'dir') config.dirs = lines;
  else if (s.type === 'url') config.urls = lines;
  else if (s.type === 'auto') config.inputs = lines;
  else if (s.type === 'repo') {
    config.repository = Object.assign({}, s.config.repository,
      lines[0].startsWith('/') ? { dir: lines[0], url: '' } : { url: lines[0], dir: '' });
    // repo carries its own extras and no chunking at all.
    for (const k of ['file_extensions', 'doc_extensions', 'skip_dirs', 'skip_suffixes', 'parse_concurrency']) {
      if (s.config[k]) config[k] = s.config[k];
    }
  }
  if (s.type !== 'repo') {
    const st = $('e_strategy').value;
    if (st) config.strategy = st;
    const rd = $('e_reader').value;
    if (rd) config.reader_type = rd;
    config.chunk_size = parseInt($('e_size').value, 10) || 1024;
    config.chunk_overlap = parseInt($('e_ov').value, 10) || 0;
  }
  if (s.type === 'dir') {
    const ext = ($('e_ext').value || '').split(',').map((x) => x.trim()).filter(Boolean);
    if (ext.length) config.file_extensions = ext;
    config.recursive = $('e_rec').checked;
  }
  try {
    await api('PATCH', `/api/sources/${s.id}`, {
      display_name: s.display_name, type: s.type, config,
      user_metadata: s.user_metadata || undefined,
    });
    S.editing = null;
    await loadSources();
    toast('已保存', '配置只写了数据库，点 Sync 才会重新入库。', 'ok');
  } catch (e) { fail(e); }
}

$('syncAllBtn').addEventListener('click', async () => {
  try {
    const r = await api('POST', `/api/bases/${S.base.id}/sync`);
    toast('已入队', `${(r.tasks || []).length} 个任务。同一个库的写操作是串行的，会逐个执行。`);
    await loadSources();
  } catch (e) { fail(e); }
});

/* ── add source ─────────────────────────────────────────────────────── */
$('addSrcBtn').addEventListener('click', () => {
  const d = S.base.source_defaults || {};
  openModal('新建 source', `
    <div class="frow"><span class="fl">名称</span><div class="fctl">
      <input class="inp" id="a_name" style="width:100%" placeholder="knowledge/"></div></div>
    <div class="frow"><span class="fl">类型</span><div class="fctl">
      <select class="inp" id="a_type">
        <option value="dir">dir — 目录</option>
        <option value="file">file — 文件</option>
        <option value="auto">auto — 自动识别</option>
        <option value="url">url — 网页</option>
        <option value="repo">repo — 仓库（AST）</option>
      </select></div></div>
    <div class="frow"><span class="fl" id="a_inputLabel">目录<small>多个用换行分隔</small></span><div class="fctl">
      <textarea class="inp" id="a_input" rows="2" style="width:100%"></textarea></div></div>
    <div id="a_dirOnly">
      <div class="frow"><span class="fl">扩展名过滤<small>留空表示全部</small></span><div class="fctl">
        <input class="inp mono" id="a_ext" placeholder=".go, .md" style="width:100%"></div></div>
      <div class="frow"><span class="fl">递归</span><div class="fctl">
        <label style="display:flex;gap:7px;align-items:center;font-size:12.5px">
          <input type="checkbox" id="a_rec" checked style="accent-color:var(--cyan)">遍历子目录</label></div></div>
    </div>
    <div id="a_chunking">
      <div class="frow"><span class="fl">Strategy</span><div class="fctl">
        <select class="inp" id="a_strategy">
          <option value="" ${!d.strategy ? 'selected' : ''}>default（跟随 reader）</option>
          ${['recursive', 'fixed', 'markdown', 'json'].map((v) =>
            `<option value="${v}" ${d.strategy === v ? 'selected' : ''}>${v}</option>`).join('')}
        </select>
        <span class="hint">default = 不覆盖，reader 用自己的策略</span></div></div>
      <div class="frow"><span class="fl">Reader</span><div class="fctl">
        <select class="inp" id="a_reader">
          <option value="">auto（按扩展名）</option>
          ${['text', 'markdown', 'json', 'csv', 'go', 'proto', 'pdf', 'docx'].map((v) =>
            `<option value="${v}">${v}</option>`).join('')}
        </select></div></div>
      <div class="frow"><span class="fl">Chunk size / overlap</span><div class="fctl">
        <input class="inp num" id="a_size" value="${d.chunk_size || 1024}" style="width:90px">
        <input class="inp num" id="a_ov" value="${d.chunk_overlap ?? 0}" style="width:90px">
        <span class="hint">overlap 计入 chunk size 预算</span></div></div>
    </div>
    <div id="a_repoNote" hidden>
      <div class="banner warn" style="margin:12px 0 0">
        repo source 走 AST 抽取，<b>没有分块参数</b>。上游 <code>reposource</code> 不接受
        chunk size/overlap，所以这里不提供 —— 填了也不会生效。</div>
    </div>
    <div class="frow"><span class="fl">立即同步</span><div class="fctl">
      <label style="display:flex;gap:7px;align-items:center;font-size:12.5px">
        <input type="checkbox" id="a_sync" checked style="accent-color:var(--cyan)">创建后立刻入库</label></div></div>`,
    async () => {
      const type = $('a_type').value;
      const lines = $('a_input').value.split('\n').map((x) => x.trim()).filter(Boolean);
      if (!lines.length) throw new Error('输入不能为空');
      const config = {};
      if (type === 'file') config.paths = lines;
      else if (type === 'dir') config.dirs = lines;
      else if (type === 'url') config.urls = lines;
      else if (type === 'auto') config.inputs = lines;
      else if (type === 'repo') {
        config.repository = lines[0].startsWith('/') ? { dir: lines[0] } : { url: lines[0] };
      }
      if (type === 'dir') {
        const ext = $('a_ext').value.split(',').map((x) => x.trim()).filter(Boolean);
        if (ext.length) config.file_extensions = ext;
        config.recursive = $('a_rec').checked;
      }
      // repo takes no chunking options at all — sending them would be rejected.
      if (type !== 'repo') {
        const st = $('a_strategy').value;
        if (st) config.strategy = st;
        config.chunk_size = parseInt($('a_size').value, 10) || 1024;
        config.chunk_overlap = parseInt($('a_ov').value, 10) || 0;
        const reader = $('a_reader').value;
        if (reader) config.reader_type = reader;
      }
      await api('POST', `/api/bases/${S.base.id}/sources`, {
        display_name: $('a_name').value.trim() || lines[0],
        type, config, sync: $('a_sync').checked,
      });
      await loadSources();
      await loadBases();
      toast('已创建', '', 'ok');
    });

  const sync = () => {
    const t = $('a_type').value;
    $('a_dirOnly').hidden = t !== 'dir';
    $('a_chunking').hidden = t === 'repo';
    $('a_repoNote').hidden = t !== 'repo';
    $('a_inputLabel').innerHTML = {
      dir: '目录<small>多个用换行分隔</small>',
      file: '文件路径<small>多个用换行分隔</small>',
      auto: '路径<small>文件或目录</small>',
      url: 'URL<small>多个用换行分隔</small>',
      repo: '仓库<small>github.com/x/y 或本地路径</small>',
    }[t];
  };
  $('a_type').addEventListener('change', sync);
  sync();
});

/* ── modal ──────────────────────────────────────────────────────────── */
function openModal(title, bodyHTML, onOK) {
  const veil = document.createElement('div');
  veil.className = 'veil';
  veil.innerHTML = `<div class="modal" role="dialog" aria-modal="true">
    <div class="modal-head"><h3>${esc(title)}</h3></div>
    <div class="modal-body">${bodyHTML}</div>
    <div class="modal-foot">
      <button class="btn" data-act="cancel" type="button">取消</button>
      <button class="btn pri" data-act="ok" type="button">确定</button>
    </div></div>`;
  document.body.appendChild(veil);
  const close = () => veil.remove();
  veil.addEventListener('click', (e) => { if (e.target === veil) close(); });
  veil.querySelector('[data-act="cancel"]').addEventListener('click', close);
  const ok = veil.querySelector('[data-act="ok"]');
  ok.addEventListener('click', async () => {
    ok.disabled = true;
    try { await onOK(); close(); } catch (e) { fail(e); ok.disabled = false; }
  });
  document.addEventListener('keydown', function onEsc(e) {
    if (e.key === 'Escape') { close(); document.removeEventListener('keydown', onEsc); }
  });
  veil.querySelector('input,textarea,select')?.focus();
}

/* ── resizable panes ────────────────────────────────────────────────── */

// Drag a gutter to resize the pane on its right. The width lands on a CSS
// custom property so the pane keeps its own layout rules, and is remembered
// per gutter — panel width is a per-person preference, not a default worth
// arguing about.
function makeResizable(gutterId, cssVar, { min = 260, max = 900, side = 'right', reset = 420 } = {}) {
  const g = $(gutterId);
  if (!g) return;
  const key = 'wb-w-' + gutterId;
  // A gutter left of its pane measures from the window's right edge; one right
  // of its pane measures from the pane's own left edge.
  const widthAt = (clientX) => side === 'right'
    ? window.innerWidth - clientX
    : clientX - (g.previousElementSibling
      ? g.previousElementSibling.getBoundingClientRect().left
      : 0);
  const apply = (px) => {
    const w = Math.max(min, Math.min(max, Math.round(px)));
    document.documentElement.style.setProperty(cssVar, w + 'px');
    try { localStorage.setItem(key, String(w)); } catch { /* private mode */ }
    scheduleConnectors();
  };
  let saved = null;
  try { saved = localStorage.getItem(key); } catch { /* private mode */ }
  if (saved) apply(+saved);

  const onMove = (e) => apply(widthAt(e.clientX));
  const stop = () => {
    g.classList.remove('dragging');
    document.body.classList.remove('resizing');
    window.removeEventListener('mousemove', onMove);
    window.removeEventListener('mouseup', stop);
  };
  g.addEventListener('mousedown', (e) => {
    e.preventDefault();
    g.classList.add('dragging');
    document.body.classList.add('resizing');
    window.addEventListener('mousemove', onMove);
    window.addEventListener('mouseup', stop);
  });
  // Keyboard resizing, so the pane is reachable without a pointer.
  g.addEventListener('keydown', (e) => {
    const cur = parseInt(getComputedStyle(document.documentElement)
      .getPropertyValue(cssVar), 10) || reset;
    const grow = side === 'right' ? 'ArrowLeft' : 'ArrowRight';
    if (e.key === grow) { apply(cur + 24); e.preventDefault(); }
    else if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') { apply(cur - 24); e.preventDefault(); }
  });
  g.addEventListener('dblclick', () => apply(reset));
}

/* ── chunk inspector ────────────────────────────────────────────────── */
async function loadTree() {
  if (!S.base) return;
  const gen = S.gen;
  const groups = [];
  for (const row of S.sources) {
    if (!row.supports_chunking) { groups.push({ row, entries: [], note: 'AST 抽取，无分块可看' }); continue; }
    try {
      const r = await api('GET', `/api/sources/${row.source.id}/tree`);
      groups.push({ row, entries: r.entries || [] });
    } catch { groups.push({ row, entries: [], note: '读取失败' }); }
  }
  if (gen !== S.gen) return;
  S.tree = groups;
  renderTree();
}

function renderTree() {
  const f = S.treeFilter.toLowerCase();
  if (!S.tree.length) {
    $('ciTree').innerHTML = '<div class="empty" style="padding:30px 16px"><p>先建一个 source</p></div>';
    return;
  }
  $('ciTree').innerHTML = S.tree.map((g, gi) => {
    const s = g.row.source;
    const entries = g.entries.filter((e) => !f || e.path.toLowerCase().includes(f));
    const head = `<button class="tsrc" type="button" data-g="${gi}">
      <span class="tw">▼</span>
      <span class="tico" style="background:var(--${TYPE_TONE[s.type]}-soft);color:var(--${TYPE_TONE[s.type]})">${esc(s.type.toUpperCase())}</span>
      <b>${esc(s.display_name)}</b><span class="n">${g.note ? '—' : entries.length}</span></button>`;
    const body = g.note
      ? `<div class="tfiles"><div class="tfile" style="cursor:default"><span class="fn">${esc(g.note)}</span></div></div>`
      : `<div class="tfiles">${entries.map((e) => `
          <button class="tfile ${S.file === e.path ? 'on' : ''}" type="button"
                  data-file="${esc(e.path)}" data-src="${esc(s.id)}">
            <span class="fn" title="${esc(e.path)}">${esc(e.name)}</span>
            <span class="n">${(e.size / 1024).toFixed(1)}k</span></button>`).join('')}</div>`;
    return head + body;
  }).join('');
}

$('ciFilter').addEventListener('input', (e) => { S.treeFilter = e.target.value; renderTree(); });

$('ciTree').addEventListener('click', (e) => {
  const g = e.target.closest('.tsrc');
  if (g) { g.classList.toggle('closed'); g.nextElementSibling?.toggleAttribute('hidden'); return; }
  const f = e.target.closest('.tfile[data-file]');
  if (!f) return;
  S.file = f.dataset.file;
  S.fileSource = f.dataset.src;
  const row = S.sources.find((r) => r.source.id === f.dataset.src);
  if (row) {
    const c = row.source.config || {};
    $('ciStrategy').value = c.strategy || '';
    if (c.chunk_size) { $('ciSize').value = c.chunk_size; $('ciSizeV').textContent = c.chunk_size; }
    $('ciOv').value = c.chunk_overlap || 0;
    $('ciOvV').textContent = c.chunk_overlap || 0;
    $('ciReader').value = c.reader_type || '';
  }
  $('ciCrumb').textContent = S.file;
  $('ciApplyBtn').disabled = false;
  renderTree();
  runPreview();
});

let previewTimer = null;
function schedulePreview() {
  clearTimeout(previewTimer);
  previewTimer = setTimeout(runPreview, 220);
}

async function runPreview() {
  if (!S.file || !S.fileSource) return;
  // Preview is re-run on every slider move; without a generation check a slow
  // response for an old file or an old chunk size overwrites a newer one.
  const gen = ++S.previewGen;
  const baseGen = S.gen;
  const file = S.file, sourceID = S.fileSource;
  const config = {
    chunk_size: parseInt($('ciSize').value, 10),
    chunk_overlap: parseInt($('ciOv').value, 10),
  };
  // An empty strategy is meaningful: the server resolves the reader's own
  // default and reports which one it picked.
  const strat = $('ciStrategy').value;
  if (strat) config.strategy = strat;
  const reader = $('ciReader').value;
  if (reader) config.reader_type = reader;
  try {
    const r = await api('POST', '/api/preview/file',
      { source_id: sourceID, path: file, config });
    if (gen !== S.previewGen || baseGen !== S.gen) return;   // superseded
    S.preview = r.preview;
    S.content = r.content;
    S.spans = [];
    S.selChunk = -1;
    $('ciBanner').innerHTML = '';
    renderPreview();
  } catch (e) {
    if (gen !== S.previewGen || baseGen !== S.gen) return;
    S.preview = null;
    $('ciBanner').innerHTML = `<div class="banner">${esc(e.message)}</div>`;
    $('ciChunks').innerHTML = '';
    $('ciMetrics').innerHTML = '';
    $('ciDoc').textContent = S.content || '';
  }
}

['ciSize', 'ciOv'].forEach((id) => $(id).addEventListener('input', () => {
  $(id + 'V').textContent = $(id).value;
  schedulePreview();
}));
['ciStrategy', 'ciReader'].forEach((id) => $(id).addEventListener('change', runPreview));

function renderPreview() {
  const p = S.preview;
  if (!p) return;
  $('ciUnit').textContent = 'unit: ' + p.unit;
  $('ciSrcMeta').textContent = n(p.source_size) + ' ' + p.unit;
  $('ciMetrics').innerHTML = `
    <div class="met"><span class="lbl">chunks</span><b>${n(p.count)}</b>
      <span class="d">${esc(p.strategy)}${p.resolved_from ? ' ← ' + esc(p.resolved_from) : ''} · ${p.unit}</span></div>
    <div class="met"><span class="lbl">avg size</span><b>${n(p.avg_size)}</b><span class="d">${p.unit}</span></div>
    <div class="met"><span class="lbl">max size</span><b>${n(p.max_size)}</b>
      <span class="d">budget ${p.chunk_size}</span></div>
    <div class="met"><span class="lbl">over budget</span>
      <b style="color:${p.over_budget ? 'var(--crit)' : ''}">${p.over_budget}</b>
      <span class="d">chunks</span></div>`;

  if (p.over_budget) {
    $('ciBanner').innerHTML = `<div class="banner warn">
      上游产出了 <b>${p.over_budget}</b> 个超预算的 chunk，最大 ${p.max_size} / 预算 ${p.chunk_size}
      （超出 ${p.max_size - p.chunk_size}）。这是 overlap 被叠加在满额 core 之上导致的，
      trpc-group/trpc-agent-go#2215 修的正是这个。</div>`;
  }

  // Locate before the note reads it: renderDoc runs at the end of this
  // function, so computing spans there would leave the note a render behind.
  S.spans = locateChunks();

  const totalOv = p.chunks.reduce((a, c) => a + (c.overlap_size || 0), 0);
  // Say when the source pane cannot show where chunks came from. The JSON
  // strategy re-serialises its input, so its chunks share no text with the
  // file on disk and no band or connector can honestly be drawn.
  const located = (S.spans || []).filter(Boolean).length;
  const unlocated = p.count - located;
  $('ciNote').textContent = [
    p.truncated ? `只显示前 ${p.count} 块` : '',
    totalOv ? `overlap 合计 ${n(totalOv)} ${p.unit}` : '无 overlap',
    unlocated > 0 && S.spans && S.spans.length
      ? (located === 0
        ? '无法在原文中定位（json 策略会重新序列化，文本与原文不同）'
        : `${unlocated} 块无法在原文中定位`)
      : '',
  ].filter(Boolean).join(' · ');

  $('ciChunks').innerHTML = p.chunks.map((c, i) => {
    // The leading overlap is the slice of this chunk that also ends the
    // previous one. Slice by code point so it lines up with a rune budget.
    const chars = Array.from(c.content);
    const ov = Math.min(c.overlap_size || 0, chars.length);
    const body = ov > 0
      ? `<span class="ovl">${esc(chars.slice(0, ov).join(''))}</span>${esc(chars.slice(ov).join(''))}`
      : esc(c.content);
    const core = c.size - ov;
    // How much of this chunk's tail the next one will carry. It is the next
    // chunk's overlap, not this one's, so it is labelled but not highlighted —
    // marking it here too would show twice the configured overlap per chunk.
    const lent = p.chunks[i + 1] ? (p.chunks[i + 1].overlap_size || 0) : 0;
    return `<article class="ck ${i === S.selChunk ? 'on' : ''}" data-i="${i}" tabindex="0">
      <div class="ck-h">
        <span class="ix">${String(i + 1).padStart(2, '0')}</span>
        <span class="chip ${c.over_budget ? 'crit' : ''}">${c.size} / ${p.chunk_size}</span>
        ${ov > 0 ? `<span class="chip op" title="借自上一块，计入本块预算">↰ overlap ${ov}</span>`
                 : '<span class="chip">no overlap</span>'}
        <span class="spacer"></span>
        <span class="lbl">core ${core}</span>
        ${lent > 0 ? `<span class="lbl" title="这段会成为下一块的 overlap，不计入本块预算">尾部 ${lent} → 下块</span>` : ''}
      </div>
      <div class="ck-txt">${body}</div>
      <div class="budget">
        ${ov > 0 ? `<i style="width:${(ov / p.chunk_size) * 100}%;background:var(--overprint)"></i>` : ''}
        <i class="${c.over_budget ? 'over' : ''}" style="width:${Math.min(100, (core / p.chunk_size) * 100)}%"></i>
      </div>
    </article>`;
  }).join('');

  renderDoc();
}

// Build a whitespace-collapsed view of the text plus an index back to the
// original offsets.
//
// Matching chunks against the raw file does not work: upstream normalises
// whitespace while chunking, so a chunk of a Go file shares almost no exact
// substring with the file on disk. Measured over this repo, matching raw text
// located 20 of 106 chunks; matching through this view located 105.
function buildNorm(text) {
  const buf = [], map = [];
  let ws = false;
  for (let i = 0; i < text.length; i++) {
    const ch = text[i];
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === '\f' || ch === '\v') {
      if (!ws) { buf.push(' '); map.push(i); ws = true; }
    } else { buf.push(ch); map.push(i); ws = false; }
  }
  map.push(text.length);
  return { s: buf.join(''), map };
}
const normText = (s) => s.replace(/\s+/g, ' ').trim();

// Locate every chunk inside the source text. The API returns chunk content,
// not offsets, because upstream strategies do not report them. Scanning
// forward recovers them: chunks tile the document in order, so a forward scan
// cannot land on the wrong copy of a repeated line. A chunk that still cannot
// be found simply gets no band, and the rest stay aligned.
function locateChunks() {
  const p = S.preview, text = S.content || '';
  if (!p) return [];
  if (S.normFor !== text) { S.norm = buildNorm(text); S.normFor = text; }
  const ns = S.norm.s, map = S.norm.map;

  const spans = [];
  let from = 0;
  for (const c of p.chunks) {
    const body = c.content || '';
    const flat = normText(body);
    const probe = flat.slice(0, 160);
    if (!probe) { spans.push(null); continue; }
    let at = ns.indexOf(probe, from);
    if (at < 0) at = ns.indexOf(probe);
    if (at < 0) { spans.push(null); continue; }
    const nEnd = Math.min(ns.length, at + flat.length);
    // Overlap is a code-point prefix of the chunk; normalise it the same way
    // so the core boundary lands in the same coordinate space.
    const ovChars = Math.min(c.overlap_size || 0, Array.from(body).length);
    const ovFlat = normText(Array.from(body).slice(0, ovChars).join(''));
    const nCore = Math.min(nEnd, at + ovFlat.length);
    spans.push({ start: map[at], coreAt: map[nCore], end: map[nEnd], index: c.index });
    from = at + 1;
  }
  return spans;
}

// Paint every chunk onto the source: adjacent chunks alternate cyan/magenta so
// a boundary is visible, and the stretch two chunks share is overprinted.
function renderDoc() {
  const p = S.preview, text = S.content || '';
  const doc = $('ciDoc');
  if (!p) { doc.textContent = text; S.spans = []; drawConnectors(); return; }
  if (!S.spans || !S.spans.length) S.spans = locateChunks();

  // Cut points from every boundary, then attribute each slice.
  const pts = new Set([0, text.length]);
  for (const s of S.spans) { if (s) { pts.add(s.start); pts.add(s.coreAt); pts.add(s.end); } }
  const cuts = [...pts].filter((x) => x >= 0 && x <= text.length).sort((a, b) => a - b);

  // Nothing is highlighted until a chunk is picked: the source reads as the
  // file it is. Spans are still emitted so clicking the text can select the
  // chunk that owns it, but they carry no class and so no appearance.
  //
  // Only the selected chunk is painted, and only its own head overlap — the
  // stretch its tail lends to the next chunk is that chunk's budget, not this
  // one's, so marking it here would show twice the configured overlap.
  const sel = S.selChunk >= 0 ? S.spans[S.selChunk] : null;
  let html = '';
  for (let i = 0; i < cuts.length - 1; i++) {
    const a = cuts[i], b = cuts[i + 1];
    if (b <= a) continue;
    let owner = -1;
    for (const s of S.spans) {
      if (s && s.coreAt <= a && a < s.end) owner = s.index;
    }
    const piece = esc(text.slice(a, b));
    if (owner < 0 && !(sel && a >= sel.start && b <= sel.end)) { html += piece; continue; }

    let cls = '', tip = '';
    if (sel && a >= sel.start && b <= sel.end) {
      if (a < sel.coreAt) {
        cls = 'band ovl';
        tip = '借自上一块，计入本块的 overlap';
      } else {
        cls = 'band core';
      }
    }
    html += `<span${cls ? ` class="${cls}"` : ''} data-ci="${owner}"` +
      `${tip ? ` title="${tip}"` : ''}>${piece}</span>`;
  }
  doc.innerHTML = html;

  if (sel) {
    doc.querySelector('.band')?.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }
  requestAnimationFrame(drawConnectors);
}

// Clicking the source selects the chunk that owns that stretch.
$('ciDoc').addEventListener('click', (e) => {
  const el = e.target.closest('[data-ci]');
  if (!el) return;
  const i = +el.dataset.ci;
  if (i < 0) return;
  S.selChunk = S.selChunk === i ? -1 : i;
  renderPreview();
  if (S.selChunk >= 0) {
    $('ciChunks').querySelector('.ck.on')?.scrollIntoView({ block: 'center', behavior: 'smooth' });
  }
});

// Connector lines from a band in the source to its card in the chunk list.
// Only chunks visible in both panes are drawn; the selected one is emphasised.
function drawConnectors() {
  const svg = $('ciConn');
  if (!svg || !$('plate-chunks').classList.contains('on')) return;
  const view = document.querySelector('.ci-view');
  const box = view.getBoundingClientRect();
  svg.setAttribute('viewBox', `0 0 ${box.width} ${box.height}`);
  svg.setAttribute('width', box.width);
  svg.setAttribute('height', box.height);

  const doc = $('ciDoc'), list = $('ciChunks');
  if (!doc || !list || !S.preview) { svg.innerHTML = ''; return; }
  const docBox = doc.getBoundingClientRect(), listBox = list.getBoundingClientRect();

  // A connector is a highlight too, so it only appears for the selection.
  const wanted = S.selChunk >= 0 ? [S.selChunk] : [];

  let out = '';
  for (const i of wanted) {
    const band = doc.querySelector('.band');
    const card = list.querySelector(`.ck[data-i="${i}"]`);
    if (!band || !card) continue;
    const bb = band.getBoundingClientRect(), cb = card.getBoundingClientRect();
    // Skip anything scrolled out of its own pane.
    if (bb.bottom < docBox.top || bb.top > docBox.bottom) continue;
    if (cb.bottom < listBox.top || cb.top > listBox.bottom) continue;

    const x1 = docBox.right - box.left, y1 = bb.top + bb.height / 2 - box.top;
    const x2 = cb.left - box.left, y2 = cb.top + Math.min(18, cb.height / 2) - box.top;
    const mid = (x1 + x2) / 2;
    const on = i === S.selChunk;
    out += `<path class="link${on ? ' on' : ''}" d="M ${x1} ${y1} C ${mid} ${y1} ${mid} ${y2} ${x2} ${y2}"/>`;
    if (on) out += `<circle class="cap" cx="${x2}" cy="${y2}" r="2.5"/>`;
  }
  svg.innerHTML = out;
}

let connTimer = null;
const scheduleConnectors = () => {
  cancelAnimationFrame(connTimer);
  connTimer = requestAnimationFrame(drawConnectors);
};
$('ciDoc').addEventListener('scroll', scheduleConnectors, { passive: true });
$('ciChunks').addEventListener('scroll', scheduleConnectors, { passive: true });
window.addEventListener('resize', scheduleConnectors);

$('ciChunks').addEventListener('click', (e) => {
  const el = e.target.closest('.ck');
  if (!el) return;
  const i = +el.dataset.i;
  S.selChunk = S.selChunk === i ? -1 : i;
  renderPreview();
});

$('ciApplyBtn').addEventListener('click', async () => {
  const row = S.sources.find((r) => r.source.id === S.fileSource);
  if (!row) return;
  const s = row.source;
  const config = Object.assign({}, s.config, {
    chunk_size: parseInt($('ciSize').value, 10),
    chunk_overlap: parseInt($('ciOv').value, 10),
  });
  const strat = $('ciStrategy').value;
  if (strat) config.strategy = strat; else delete config.strategy;
  const reader = $('ciReader').value;
  if (reader) config.reader_type = reader; else delete config.reader_type;
  try {
    await api('PATCH', `/api/sources/${s.id}`, {
      display_name: s.display_name, type: s.type, config,
      user_metadata: s.user_metadata || undefined,
    });
    await loadSources();
    toast('已写入 source', '配置只落到数据库，需要重新同步才会生效。', 'ok');
  } catch (e) { fail(e); }
});

/* ── retrieval ──────────────────────────────────────────────────────── */
// Hits carry the upstream source name, which is the immutable source id.
// Show the display name the user actually chose.
function srcLabel(id) {
  const row = S.sources.find((r) => r.source.id === id);
  return row ? row.source.display_name : id;
}

// The store decides which of these actually work, so the controls are built
// from what the server reports rather than hard-coded.
async function loadCapabilities() {
  if (!S.base) return;
  const gen = S.gen;
  let caps = null;
  try { caps = await api('GET', `/api/bases/${S.base.id}/capabilities`); } catch { /* keep null */ }
  if (gen !== S.gen) return;
  S.caps = caps;
  renderRetrievalControls();
}

function renderRetrievalControls() {
  const c = S.caps;
  if (!c) { $('rtModes').innerHTML = ''; $('rtNotes').innerHTML = ''; return; }
  if (!S.mode) S.mode = 'vector';

  $('rtModes').innerHTML = c.modes.map((m) => `
    <button type="button" data-mode="${esc(m.value)}"
            class="${S.mode === m.value ? 'on' : ''}${m.exact ? '' : ' inexact'}"
            title="${esc(m.note || '')}">${esc(m.label)}</button>`).join('');

  // Say plainly what this store cannot do, instead of omitting the control and
  // leaving the user to wonder where RRF went.
  const notes = [];
  const cur = c.modes.find((m) => m.value === S.mode);
  if (cur && cur.note) notes.push({ kind: '', text: cur.note });
  if (c.fusion && !c.fusion.available) notes.push({ kind: 'info', text: c.fusion.note });
  $('rtNotes').innerHTML = notes.map((nt) =>
    `<div class="rt-note ${nt.kind}">${esc(nt.text)}</div>`).join('');
}

$('rtModes').addEventListener('click', (e) => {
  const b = e.target.closest('[data-mode]');
  if (!b) return;
  S.mode = b.dataset.mode;
  renderRetrievalControls();
});

/* Metadata filters. Fields are bare metadata keys; the server prefixes them,
   because a bare key reaches the store as an unknown field and matches nothing
   without erroring. */
S.filters = [];

function renderFilters() {
  const ops = (S.caps && S.caps.operators) || [{ value: 'eq', label: '=' }];
  $('rtFilters').innerHTML = S.filters.map((f, i) => `
    <div class="frow-filter" data-i="${i}">
      <input class="inp mono" data-k="field" list="rtFields" placeholder="metadata key"
             value="${esc(f.field)}" style="flex:1;max-width:280px" aria-label="字段">
      <select class="inp" data-k="op" aria-label="操作符">
        ${ops.map((o) => `<option value="${esc(o.value)}" ${f.op === o.value ? 'selected' : ''}>${esc(o.label)}</option>`).join('')}
      </select>
      <input class="inp mono" data-k="value" placeholder="值（in / between 用逗号分隔）"
             value="${esc(f.value)}" style="flex:1" aria-label="值">
      <button class="x" data-x="${i}" type="button" aria-label="删除该条件">✕</button>
    </div>`).join('') +
    (S.filters.length > 1
      ? `<div class="f-row"><span class="lbl">组合</span>
         <div class="seg">
           <button type="button" data-join="and" class="${S.join !== 'or' ? 'on' : ''}">AND</button>
           <button type="button" data-join="or" class="${S.join === 'or' ? 'on' : ''}">OR</button>
         </div>
         <span class="hint" style="font-size:11px;color:var(--faint)">数字用 in 会匹配不到（上游 in 不做数值转换），改用 between 或 =</span>
         </div>`
      : '') +
    `<datalist id="rtFields">${(S.metaKeys || []).map((k) => `<option value="${esc(k)}">`).join('')}</datalist>`;
}

$('rtAddFilter').addEventListener('click', () => {
  S.filters.push({ field: '', op: 'eq', value: '' });
  renderFilters();
});
$('rtFilters').addEventListener('input', (e) => {
  const row = e.target.closest('[data-i]');
  if (!row) return;
  const f = S.filters[+row.dataset.i];
  if (f && e.target.dataset.k) f[e.target.dataset.k] = e.target.value;
});
$('rtFilters').addEventListener('change', (e) => {
  const row = e.target.closest('[data-i]');
  if (row && e.target.dataset.k === 'op') S.filters[+row.dataset.i].op = e.target.value;
});
$('rtFilters').addEventListener('click', (e) => {
  const x = e.target.closest('[data-x]');
  if (x) { S.filters.splice(+x.dataset.x, 1); renderFilters(); return; }
  const j = e.target.closest('[data-join]');
  if (j) { S.join = j.dataset.join; renderFilters(); }
});

// Turn a filter row into an upstream UniversalFilterCondition. Values arrive as
// text, so coerce numbers — "eq 1" against a numeric field must not compare a
// string to a number.
function coerce(v) {
  const t = v.trim();
  if (t === '') return t;
  if (t === 'true') return true;
  if (t === 'false') return false;
  return /^-?\d+(\.\d+)?$/.test(t) ? Number(t) : t;
}

function buildFilter() {
  const rows = S.filters.filter((f) => f.field.trim() && f.value.trim());
  if (!rows.length) return null;
  const conds = rows.map((f) => {
    const multi = f.op === 'in' || f.op === 'not in' || f.op === 'between';
    return {
      field: f.field.trim(),
      operator: f.op,
      value: multi ? f.value.split(',').map(coerce) : coerce(f.value),
    };
  });
  if (conds.length === 1) return conds[0];
  return { operator: S.join === 'or' ? 'or' : 'and', value: conds };
}

$('rtGo').addEventListener('click', doSearch);
$('rtQuery').addEventListener('keydown', (e) => { if (e.key === 'Enter') doSearch(); });

async function doSearch() {
  if (!S.base) return;
  const q = $('rtQuery').value.trim();
  const mode = S.mode || 'vector';
  if (!q && mode !== 'filter') {
    toast('需要 query', '只有 filter 模式可以不填查询词', 'err');
    return;
  }
  $('rtBody').innerHTML = '<div class="empty"><p>检索中…</p></div>';
  try {
    const body = {
      query: q,
      mode,
      top_k: parseInt($('rtTopK').value, 10) || 5,
      min_score: parseFloat($('rtMinScore').value) || 0,
    };
    const filter = buildFilter();
    if (filter) body.filter = filter;
    const r = await api('POST', `/api/bases/${S.base.id}/search`, body);
    const results = r.results || [];
    // Remember the metadata keys actually present, so the filter field input
    // can suggest real ones instead of leaving the user to guess.
    const keys = new Set(S.metaKeys || []);
    results.forEach((h) => Object.keys(h.metadata || {}).forEach((k) => keys.add(k)));
    S.metaKeys = [...keys];
    if (S.filters.length) renderFilters();

    const head = `<div class="rt-note info" style="margin-bottom:4px">
      ${r.count} 条 · ${r.took_ms} ms · mode=${esc(r.mode)}${r.degraded ? ' · ' + esc(r.degraded) : ''}</div>`;
    if (!results.length) {
      $('rtBody').innerHTML = head +
        '<div class="empty"><p>没有命中。检查这个库是否已入库，或放宽 min score / 过滤条件。</p></div>';
      return;
    }
    $('rtBody').innerHTML = head + results.map((h, i) => {
      const score = h.score ?? 0;
      const meta = h.metadata || {};
      const src = meta['trpc_agent_go_source_name'];
      const idx = meta['trpc_agent_go_chunk_index'];
      const uri = meta['trpc_agent_go_uri'];
      return `<article class="hit">
        <div class="hit-h"><span class="rank">${String(i + 1).padStart(2, '0')}</span>
          <b>${esc(h.name || h.id || '')}</b>
          ${uri ? `<span class="lbl">${esc(String(uri).split('/').slice(-2).join('/'))}${idx ? ' · chunk ' + esc(idx) : ''}</span>` : ''}
          <span class="spacer"></span>
          ${src ? `<span class="chip">${esc(srcLabel(src))}</span>` : ''}
          <span class="score"><span class="bar"><i style="width:${Math.max(0, Math.min(100, score * 100))}%"></i></span>
            <span class="v">${score.toFixed(3)}</span></span></div>
        <div class="hit-txt">${esc(h.content || '')}</div>
      </article>`;
    }).join('');
  } catch (e) {
    $('rtBody').innerHTML = `<div class="empty"><p>${esc(e.message)}</p></div>`;
  }
}

/* ── live events ────────────────────────────────────────────────────── */
function connect() {
  const es = new EventSource('/api/events');
  es.onopen = () => { $('roSSE').className = 'dot ok'; };
  es.onerror = () => { $('roSSE').className = 'dot crit'; };
  es.onmessage = (m) => {
    let ev;
    try { ev = JSON.parse(m.data); } catch { return; }
    if (!S.base || ev.base_id !== S.base.id) return;
    if (ev.type === 'progress' && ev.progress) {
      const t = Object.values(S.tasks).find((x) => x.id === ev.task_id);
      if (t) { t.progress = ev.progress; renderSources(); }
      return;
    }
    if (ev.type === 'task' && ev.task) {
      if (ev.task.source_id) S.tasks[ev.task.source_id] = ev.task;
      if (['succeeded', 'failed', 'canceled'].includes(ev.task.state)) {
        const delta = (ev.task.counters_after?.chunks || 0) - (ev.task.counters_before?.chunks || 0);
        if (ev.task.state === 'succeeded') {
          toast('同步完成', delta > 0 ? `+${n(delta)} chunks` : '没有新增内容', 'ok');
        } else if (ev.task.state === 'failed') {
          toast('同步失败', ev.task.error, 'err');
        }
        loadSources().catch(() => {});
        loadBases().catch(() => {});
      } else {
        renderSources();
      }
      return;
    }
    if (ev.type === 'sources_changed') loadSources().catch(() => {});
  };
}

/* ── boot ───────────────────────────────────────────────────────────── */

// Everything scoped to a knowledge base is disabled until one is picked, which
// on a single-base install means the whole left rail is dead until the user
// guesses that the card is clickable. Pick one automatically: the last one
// used, or the only one there is.
async function autoSelectBase() {
  if (!S.bases.length) return;
  let want = null;
  try { want = localStorage.getItem('wb-base'); } catch { /* private mode */ }
  const remembered = S.bases.find((b) => b.id === want);
  const target = remembered || (S.bases.length === 1 ? S.bases[0] : null);
  if (!target) return;
  // No plate argument: stay on the bases list, just make the rail usable.
  await selectBase(target.id);
}

(async function boot() {
  makeResizable('srcGutter', '--side-w');
  makeResizable('ciGutter', '--ci-tree-w', { min: 180, max: 560, side: 'left', reset: 270 });
  try {
    await loadBases();
    await autoSelectBase();
  } catch (e) { fail(e); }
  connect();
})();
