'use strict';

// suno-web UI. Vanilla JS, no build step — the whole app is this file plus
// index.html/style.css, embedded into the Go binary.
//
// Content from the archive (titles, tags, lyrics, persona names) is always
// inserted via textContent / el() helpers, never innerHTML, since it is
// arbitrary user-authored text.

// ── helpers ────────────────────────────────────────────────────

const $ = (sel) => document.querySelector(sel);

function el(tag, attrs = {}, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') n.className = v;
    else if (k === 'text') n.textContent = v;
    else if (k.startsWith('on')) n.addEventListener(k.slice(2), v);
    else n.setAttribute(k, v === true ? '' : v);
  }
  for (const kid of kids) {
    if (kid === null || kid === undefined || kid === false) continue;
    n.append(kid.nodeType ? kid : document.createTextNode(String(kid)));
  }
  return n;
}

const fmtTime = (s) => {
  if (!isFinite(s) || s < 0) s = 0;
  const m = Math.floor(s / 60);
  return `${m}:${String(Math.floor(s % 60)).padStart(2, '0')}`;
};

const fmtBytes = (b) => {
  if (!b) return '—';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
  return `${b.toFixed(b < 10 && i > 0 ? 1 : 0)} ${u[i]}`;
};

const fmtDate = (iso) => {
  const d = new Date(iso);
  return isNaN(d) ? '—' : d.toLocaleDateString(undefined, { year: 'numeric', month: 'short', day: 'numeric' });
};

// Human labels for the derivation edge kinds the API emits.
const KIND_LABEL = {
  cover: 'cover of', edit: 'edited from', artist: 'artist ref',
  stem: 'stem of', styles: 'styles from', mashup: 'mashup of',
  extend: 'extends', concat: 'concat of', persona: 'persona root',
};

const api = async (path, opts) => {
  const r = await fetch(path, opts);
  if (!r.ok) throw new Error((await r.json().catch(() => ({}))).error || r.statusText);
  return r.status === 204 ? null : r.json();
};

// ── state ──────────────────────────────────────────────────────

const MULTI = ['model', 'version', 'project', 'persona', 'task', 'kind', 'tag'];
const TRI = ['liked', 'public', 'instrumental', 'remix', 'hasLyrics', 'hasPersona'];

const state = {
  view: 'library',
  q: '', sort: 'created', order: 'desc', group: true,
  sel: Object.fromEntries(MULTI.map((k) => [k, []])),
  tri: Object.fromEntries(TRI.map((k) => [k, null])),
  from: '', to: '', durMin: '', durMax: '',
  playlist: null,
  offset: 0, limit: 100,
  result: null,
  playlists: [],
  queue: [], qi: -1, queueLabel: '', current: null,
  backfillDone: false,
};

function params() {
  const p = new URLSearchParams();
  if (state.q) p.set('q', state.q);
  p.set('sort', state.sort);
  p.set('order', state.order);
  p.set('group', state.group ? '1' : '0');
  p.set('offset', state.offset);
  p.set('limit', state.limit);
  for (const k of MULTI) for (const v of state.sel[k]) p.append(k, v);
  for (const k of TRI) if (state.tri[k] !== null) p.set(k, state.tri[k] ? '1' : '0');
  for (const k of ['from', 'to', 'durMin', 'durMax']) if (state[k]) p.set(k, state[k]);
  if (state.playlist) p.set('playlist', state.playlist);
  return p;
}

// ── data loading ───────────────────────────────────────────────

let loadSeq = 0;

async function load(append = false) {
  const seq = ++loadSeq;
  if (!append) state.offset = 0;
  try {
    const res = await api('/api/tracks?' + params());
    if (seq !== loadSeq) return; // a newer request superseded this one
    state.result = append && state.result
      ? { ...res, rows: state.result.rows.concat(res.rows) }
      : res;
    renderContent();
    renderFilters();
    renderActive();
  } catch (e) {
    $('#content').replaceChildren(el('div', { class: 'empty', text: 'Failed to load: ' + e.message }));
  }
}

async function refreshStatus() {
  try {
    const st = await api('/api/status');
    state.backfillDone = !!st.backfillDone;
    const sub = $('#brandSub');
    sub.textContent = st.ready
      ? `${st.indexed.toLocaleString()} tracks`
      : `indexing ${Number(st.scanned).toLocaleString()}/${Number(st.total).toLocaleString()}…`;

    const box = $('#indexStatus');
    box.replaceChildren();
    if (!st.ready || st.scanned < st.total) {
      const pct = st.total ? Math.round((st.scanned / st.total) * 100) : 0;
      box.append(
        el('div', { text: `Indexing ${pct}%` }),
        el('div', { class: 'bar' }, el('i', { style: `width:${pct}%` })),
      );
    } else if (!st.backfillDone) {
      box.append(el('div', { text: `${st.indexed.toLocaleString()} archived · download in progress` }));
    } else {
      box.append(el('div', { text: `${st.indexed.toLocaleString()} archived · complete` }));
    }
    if (st.error) box.append(el('div', { text: st.error, style: 'color:#f87171' }));
  } catch { /* transient */ }
}

async function loadPlaylists() {
  try {
    const { playlists } = await api('/api/playlists');
    state.playlists = playlists || [];
    renderPlaylists();
  } catch { /* ignore */ }
}

// ── rendering: track rows ──────────────────────────────────────

function trackRow(row, i) {
  const t = row;
  const playing = state.current && state.current.id === t.id;

  const sub = el('div', { class: 'row-sub' });
  if (t.persona) {
    sub.append(el('button', {
      class: 'tag persona-chip', text: t.persona, title: 'Filter by persona',
      onclick: (e) => { e.stopPropagation(); toggleSel('persona', t.persona); },
    }));
  }
  if (t.task) {
    sub.append(el('button', {
      class: 'tag', text: t.task.replace(/_/g, ' '), title: 'Filter by relationship type',
      onclick: (e) => { e.stopPropagation(); toggleSel('task', t.task); },
    }));
  }
  sub.append(el('span', { text: fmtDate(t.created) }));
  if (t.model) sub.append(el('span', { text: '· ' + t.model }));
  for (const tag of (t.tags || []).slice(0, 3)) {
    sub.append(el('button', {
      class: 'tag', text: tag,
      onclick: (e) => { e.stopPropagation(); toggleSel('tag', tag); },
    }));
  }
  if (row.variants && row.variants.length) {
    sub.append(el('span', { class: 'variant-badge', text: `+${row.variants.length} variant${row.variants.length > 1 ? 's' : ''}` }));
  }
  if (t.liked) sub.append(el('span', { text: '♥', style: 'color:var(--k-cover)' }));

  const actions = el('div', { class: 'row-actions' });
  if (row.kids) {
    actions.append(el('button', {
      class: 'kids-badge', text: `⑃ ${row.kids}`,
      title: `${row.kids} track${row.kids > 1 ? 's' : ''} derived from this`,
      onclick: (e) => { e.stopPropagation(); openLineage(t.id); },
    }));
  }
  actions.append(
    el('span', { class: 'dur', text: fmtTime(t.duration) }),
    el('button', { class: 'icon-btn', text: '⑃', title: 'Relationships', onclick: (e) => { e.stopPropagation(); openLineage(t.id); } }),
    el('button', { class: 'icon-btn', text: 'ⓘ', title: 'Details', onclick: (e) => { e.stopPropagation(); openDetail(t.id); } }),
    el('button', { class: 'icon-btn', text: '+', title: 'Add to playlist', onclick: (e) => { e.stopPropagation(); addToPlaylist([t.id]); } }),
  );

  return el('div', {
    class: 'row' + (playing ? ' playing' : ''),
    'data-track-id': t.id,
    onclick: () => setQueue(state.result.rows.map((r) => r), i, 'results'),
  },
    el('img', { class: 'row-cover', loading: 'lazy', alt: '', src: t.hasCover ? `/api/cover/${t.id}` : '' }),
    el('div', { class: 'row-main' }, el('div', { class: 'row-title', text: t.title || '(untitled)' }), sub),
    actions,
  );
}

function renderContent() {
  if (state.view === 'personas') return renderPersonas();

  const c = $('#content');
  const res = state.result;
  c.replaceChildren();
  if (!res) return;

  const totals = res.totals || {};
  const head = el('div', { class: 'result-head' },
    el('div', { text:
      `${res.total.toLocaleString()} ${state.group ? 'generations' : 'tracks'}` +
      (state.group && res.tracks !== res.total ? ` · ${res.tracks.toLocaleString()} clips` : '') }),
    el('div', { text:
      `${fmtTime(totals.duration || 0)} total · ${fmtBytes(totals.bytes || 0)} · ` +
      `${(totals.liked || 0).toLocaleString()} liked · ${(totals.personas || 0).toLocaleString()} personas` }),
  );
  c.append(head);

  if (!res.rows.length) {
    c.append(el('div', { class: 'empty', text: 'Nothing matches these filters.' }));
    return;
  }

  const rows = el('div', { class: 'rows' });
  res.rows.forEach((r, i) => rows.append(trackRow(r, i)));
  c.append(rows);

  if (res.rows.length < res.total) {
    c.append(el('div', { style: 'text-align:center;padding:18px' },
      el('button', {
        class: 'ctl-btn', text: `Load more (${(res.total - res.rows.length).toLocaleString()} left)`,
        onclick: () => { state.offset = res.rows.length; load(true); },
      })));
  }
}

async function renderPersonas() {
  const c = $('#content');
  c.replaceChildren(el('div', { class: 'empty', text: 'Loading personas…' }));
  try {
    const { personas, total } = await api('/api/personas');
    c.replaceChildren(
      el('div', { class: 'result-head' }, el('div', { text: `${total} personas` }),
        el('div', { text: 'Voices and artist identities used across the archive' })),
    );
    const grid = el('div', { class: 'persona-grid' });
    for (const p of personas) {
      grid.append(el('div', {
        class: 'persona-card',
        onclick: () => { state.view = 'library'; setNav('library'); state.sel.persona = [p.name]; load(); },
      },
        el('div', { class: 'persona-name', text: p.name }),
        el('div', { class: 'persona-meta', text: `${p.count.toLocaleString()} tracks · ${p.liked} liked` }),
        el('div', { class: 'persona-meta', text: (p.voice ? 'voice persona · ' : '') + `last ${fmtDate(p.latest)}` }),
      ));
    }
    c.append(grid);
  } catch (e) {
    c.replaceChildren(el('div', { class: 'empty', text: 'Failed: ' + e.message }));
  }
}

// ── filters ────────────────────────────────────────────────────

function toggleSel(key, val) {
  const arr = state.sel[key];
  const i = arr.indexOf(val);
  if (i >= 0) arr.splice(i, 1); else arr.push(val);
  load();
}

function setTri(key, val) {
  state.tri[key] = state.tri[key] === val ? null : val;
  load();
}

const FACET_GROUPS = [
  ['persona', 'Persona / voice'],
  ['task', 'Relationship type'],
  ['model', 'Model'],
  ['tag', 'Style tags'],
  ['project', 'Project'],
  ['version', 'Model version'],
  ['kind', 'Clip type'],
];

function renderFilters() {
  const body = $('#filterBody');
  if ($('#filters').classList.contains('hidden')) return;
  const facets = (state.result && state.result.facets) || {};
  const openState = {};
  body.querySelectorAll('details.fgroup').forEach((d) => { openState[d.dataset.key] = d.open; });
  body.replaceChildren();

  // Boolean flags first — they're the quickest cuts.
  const flags = el('details', { class: 'fgroup', 'data-key': 'flags', open: openState.flags !== false },
    el('summary', {}, 'Flags'));
  for (const [key, label] of [
    ['liked', 'Liked'], ['hasLyrics', 'Has lyrics'], ['hasPersona', 'Has persona'],
    ['instrumental', 'Instrumental'], ['remix', 'Remix'], ['public', 'Public'],
  ]) {
    flags.append(el('div', { class: 'frow' },
      el('span', { style: 'flex:1;color:var(--dim);font-size:13px', text: label }),
      el('div', { class: 'tri' },
        el('button', { class: state.tri[key] === true ? 'on' : '', text: 'Yes', onclick: () => setTri(key, true) }),
        el('button', { class: state.tri[key] === false ? 'on' : '', text: 'No', onclick: () => setTri(key, false) }),
      )));
  }
  body.append(flags);

  for (const [key, label] of FACET_GROUPS) {
    const opts = facets[key] || [];
    if (!opts.length) continue;
    const d = el('details', { class: 'fgroup', 'data-key': key, open: openState[key] ?? (key === 'persona' || key === 'task') },
      el('summary', {}, el('span', { text: label }), el('span', { class: 'n', text: String(opts.length) })));
    const box = el('div', { class: 'fopts' });
    for (const f of opts) {
      const checked = state.sel[key].includes(f.value);
      box.append(el('div', { class: 'fopt' },
        el('label', {},
          el('input', { type: 'checkbox', checked, onchange: () => toggleSel(key, f.value) }),
          el('span', { class: 'name', text: f.value, title: f.value })),
        el('span', { class: 'n', text: f.count.toLocaleString() }),
      ));
    }
    d.append(box);
    body.append(d);
  }

  // Ranges.
  const rng = el('details', { class: 'fgroup', 'data-key': 'range', open: openState.range === true },
    el('summary', {}, 'Date & duration'));
  const mk = (k, ph, type = 'text') => el('input', {
    type, placeholder: ph, value: state[k],
    onchange: (e) => { state[k] = e.target.value.trim(); load(); },
  });
  rng.append(
    el('div', { class: 'frow' }, mk('from', 'from', 'date'), mk('to', 'to', 'date')),
    el('div', { class: 'frow' }, mk('durMin', 'min secs'), mk('durMax', 'max secs')),
  );
  body.append(rng);
}

function renderActive() {
  const bar = $('#activeFilters');
  bar.replaceChildren();
  const chip = (label, val, onx) => bar.append(
    el('span', { class: 'chip' }, el('b', { text: label }), String(val),
      el('button', { text: '×', onclick: onx })));

  for (const k of MULTI) {
    for (const v of [...state.sel[k]]) chip(k, v, () => toggleSel(k, v));
  }
  for (const k of TRI) {
    if (state.tri[k] !== null) chip(k, state.tri[k] ? 'yes' : 'no', () => setTri(k, state.tri[k]));
  }
  for (const k of ['from', 'to', 'durMin', 'durMax']) {
    if (state[k]) chip(k, state[k], () => { state[k] = ''; load(); });
  }
  if (state.playlist) {
    const p = state.playlists.find((x) => x.id === state.playlist);
    chip('playlist', p ? p.name : state.playlist, () => selectPlaylist(null));
  }
}

function resetFilters() {
  for (const k of MULTI) state.sel[k] = [];
  for (const k of TRI) state.tri[k] = null;
  state.from = state.to = state.durMin = state.durMax = '';
  state.q = ''; $('#search').value = '';
  load();
}

// ── lineage ────────────────────────────────────────────────────

function kindBadge(kind) {
  return el('span', { class: `kind-badge k-${kind}`, text: KIND_LABEL[kind] || kind });
}

// flattenFamily lists every resolved clip in the tree in display (top-down)
// order. That ordering is the family's play queue, so ⏭ walks the lineage the
// way it is drawn.
function flattenFamily(node, out = []) {
  if (node.track) out.push(node.track);
  for (const c of node.children || []) flattenFamily(c, out);
  return out;
}

// subtreeTracks is the same, but for one branch — "play from here down".
function subtreeTracks(node) {
  return flattenFamily(node, []);
}

function lineageNode(node, focusId, depth, family) {
  const wrap = el('div', { class: 'node' });
  const t = node.track;
  const hasKids = node.children && node.children.length;

  // Collapse deep or wide branches by default so a 400-child family opens
  // readable rather than as a wall.
  let collapsed = hasKids && (depth >= 2 || node.children.length > 12);
  if (node.id === focusId) collapsed = false;

  const twisty = el('button', {
    class: 'twisty', text: hasKids ? (collapsed ? '▸' : '▾') : '',
    onclick: (e) => {
      e.stopPropagation();
      const kids = wrap.querySelector(':scope > .kids');
      if (!kids) return;
      const now = kids.classList.toggle('collapsed');
      twisty.textContent = now ? '▸' : '▾';
    },
  });

  const sub = el('div', { class: 'node-sub' });
  if (node.kind) sub.append(kindBadge(node.kind));
  if (node.at) sub.append(el('span', { text: `@ ${fmtTime(node.at)}` }));
  if (t) {
    if (t.persona) sub.append(el('span', { class: 'tag persona-chip', text: t.persona }));
    sub.append(el('span', { text: fmtDate(t.created) }));
    if (t.task) sub.append(el('span', { text: t.task.replace(/_/g, ' ') }));
  } else {
    sub.append(el('span', {
      class: 'missing',
      text: state.backfillDone ? 'not in archive' : 'not archived yet',
    }));
  }

  const actions = el('div', { class: 'node-actions' });
  if (node.descendants) {
    actions.append(el('button', {
      class: 'icon-btn', text: '▶⁺', title: `Play this branch (${node.descendants + (t ? 1 : 0)} tracks)`,
      onclick: (e) => {
        e.stopPropagation();
        const list = subtreeTracks(node);
        if (list.length) setQueue(list, 0, 'branch');
      },
    }));
    actions.append(el('span', { class: 'node-kids', text: `${node.descendants} below` }));
  }
  if (t) {
    actions.append(el('button', {
      class: 'icon-btn', text: '⑃', title: 'Centre the tree on this track',
      onclick: (e) => { e.stopPropagation(); openLineage(t.id); },
    }));
    actions.append(el('button', {
      class: 'icon-btn', text: '+', title: 'Add to playlist',
      onclick: (e) => { e.stopPropagation(); addToPlaylist([t.id]); },
    }));
  }

  // Clicking a node plays it in the context of the whole family, so ⏭ keeps
  // moving through related tracks rather than stopping at one.
  const row = el('div', {
    class: 'node-row' + (node.id === focusId ? ' focus' : '') + (t ? '' : ' unresolved')
      + (t && state.current && state.current.id === t.id ? ' playing' : ''),
    'data-track-id': t ? t.id : null,
    title: t ? 'Play (⑃ re-centres the tree)' : '',
    onclick: () => {
      if (!t) return;
      const i = family.findIndex((x) => x.id === t.id);
      if (i >= 0) setQueue(family, i, 'family');
      else playTrack(t);
    },
  },
    twisty,
    el('img', { class: 'node-cover', alt: '', loading: 'lazy', src: t && t.hasCover ? `/api/cover/${t.id}` : '' }),
    el('div', { class: 'node-main' },
      el('div', { class: 'node-title', text: t ? (t.title || '(untitled)') : node.id }),
      sub),
    actions,
  );
  wrap.append(row);

  if (hasKids) {
    const kids = el('div', { class: 'kids' + (collapsed ? ' collapsed' : '') });
    for (const c of node.children) kids.append(lineageNode(c, focusId, depth + 1, family));
    wrap.append(kids);
  }
  return wrap;
}

async function openLineage(id) {
  openDrawer('#lineage');
  const body = $('#lineageBody');
  body.replaceChildren(el('div', { class: 'empty', text: 'Building family tree…' }));
  try {
    const { lineage } = await api('/api/lineage/' + encodeURIComponent(id));
    const focus = lineage.ancestors[lineage.ancestors.length - 1];
    const ft = focus && focus.track;
    $('#lineageSub').textContent =
      `${ft ? ft.title : id} · family of ${lineage.nodes.toLocaleString()}` +
      (lineage.truncated ? ' (truncated)' : '');

    body.replaceChildren();

    // The family in drawn order — the queue behind every play action here.
    const family = flattenFamily(lineage.root);
    const focusIdx = family.findIndex((x) => x.id === id);

    body.append(el('div', { class: 'family-actions' },
      el('button', {
        class: 'ctl-btn', text: `▶ Play family (${family.length})`,
        title: 'Play every related track, in tree order',
        onclick: () => family.length && setQueue(family, 0, 'family'),
      }),
      focusIdx > 0 ? el('button', {
        class: 'ctl-btn', text: '▶ Play from this track',
        title: 'Start at the focused track and continue through its relatives',
        onclick: () => setQueue(family, focusIdx, 'family'),
      }) : null,
      el('button', {
        class: 'ctl-btn', text: '⇄ Shuffle family',
        onclick: () => {
          const s = family.slice();
          for (let i = s.length - 1; i > 0; i--) {
            const j = Math.floor(Math.random() * (i + 1));
            [s[i], s[j]] = [s[j], s[i]];
          }
          setQueue(s, 0, 'family shuffle');
        },
      }),
      el('button', {
        class: 'ctl-btn', text: '+ Playlist',
        title: 'Add every related track to a playlist',
        onclick: () => addToPlaylist(family.map((x) => x.id)),
      }),
    ));

    // Legend: which colour means which kind of derivation.
    const kinds = new Set();
    (function walk(n) { if (n.kind) kinds.add(n.kind); (n.children || []).forEach(walk); })(lineage.root);
    if (kinds.size) {
      const legend = el('div', { class: 'legend' });
      for (const k of [...kinds].sort()) legend.append(kindBadge(k));
      body.append(legend);
    }

    // Ancestor chain: root → … → focus.
    if (lineage.ancestors.length > 1) {
      const anc = el('div', { class: 'anc' });
      lineage.ancestors.forEach((a, i) => {
        if (i) anc.append(el('span', { class: 'arr', text: '→' }));
        if (a.kind) anc.append(kindBadge(a.kind));
        anc.append(el('span', {
          class: 'crumb' + (a.id === id ? ' focus' : ''),
          text: a.track ? (a.track.title || '(untitled)') : a.id.slice(0, 8) + '…',
          onclick: () => a.track && openLineage(a.id),
        }));
      });
      body.append(el('div', {}, el('h3', { style: 'font-size:12px;color:var(--dimmer);margin:0 0 6px', text: 'ANCESTRY' }), anc));
    }

    body.append(el('h3', { style: 'font-size:12px;color:var(--dimmer);margin:14px 0 6px', text: 'FAMILY TREE' }));
    body.append(el('div', { class: 'tree' }, lineageNode(lineage.root, id, 0, family)));

    if (lineage.truncated) {
      body.append(el('div', { class: 'empty', text: 'Tree truncated — this family is very large.' }));
    }
  } catch (e) {
    body.replaceChildren(el('div', { class: 'empty', text: 'Failed: ' + e.message }));
  }
}

// ── detail ─────────────────────────────────────────────────────

async function openDetail(id) {
  openDrawer('#detail');
  const body = $('#detailBody');
  body.replaceChildren(el('div', { class: 'empty', text: 'Loading…' }));
  try {
    const d = await api('/api/track/' + encodeURIComponent(id));
    const t = d.track, meta = d.meta || {};
    $('#detailSub').textContent = t.title || id;
    body.replaceChildren();

    const facts = el('div', { class: 'detail-facts' });
    if (t.persona) facts.append(el('span', { class: 'tag persona-chip', text: t.persona }));
    if (t.task) facts.append(el('span', { class: 'tag', text: t.task.replace(/_/g, ' ') }));
    if (t.model) facts.append(el('span', { class: 'tag', text: t.model }));
    if (t.liked) facts.append(el('span', { class: 'tag', text: '♥ liked' }));

    body.append(el('div', { class: 'detail-top' },
      el('img', { class: 'detail-cover', alt: '', src: t.hasCover ? `/api/cover/${t.id}` : '' }),
      el('div', {},
        el('div', { class: 'detail-h1', text: t.title || '(untitled)' }),
        facts,
        el('div', { style: 'margin-top:10px;display:flex;gap:8px' },
          el('button', { class: 'ctl-btn', text: '▶ Play', onclick: () => playTrack(t) }),
          el('button', { class: 'ctl-btn', text: '⑃ Relationships', onclick: () => openLineage(t.id) }),
          el('button', { class: 'ctl-btn', text: '+ Playlist', onclick: () => addToPlaylist([t.id]) }),
          el('a', { class: 'ctl-btn', href: `/api/audio/${t.id}`, download: '', text: '⇩ WAV' }),
        ))));

    const kv = el('dl', { class: 'kv' });
    const put = (k, v) => { if (v !== '' && v !== null && v !== undefined) kv.append(el('dt', { text: k }), el('dd', { text: String(v) })); };
    put('Created', fmtDate(t.created));
    put('Duration', fmtTime(t.duration));
    put('Model', `${t.model || '—'} (${t.version || '—'})`);
    put('Task', t.task);
    put('Clip type', t.kind);
    put('Project', t.project);
    put('Persona', t.persona + (t.voicePersona ? ' (voice)' : ''));
    put('Plays / upvotes', `${t.plays} / ${t.upvotes}`);
    put('WAV size', fmtBytes(t.wavBytes));
    put('Instrumental', t.instrumental ? 'yes' : 'no');
    put('Remix', t.remix ? 'yes' : 'no');
    put('Clip id', t.id);
    body.append(el('div', { class: 'sect' }, el('h3', { text: 'Facts' }), kv));

    if (t.tags && t.tags.length) {
      const tags = el('div', { class: 'detail-facts' });
      for (const tag of t.tags) tags.append(el('button', { class: 'tag', text: tag, onclick: () => { closeDrawers(); toggleSel('tag', tag); } }));
      body.append(el('div', { class: 'sect' }, el('h3', { text: 'Style tags' }), tags));
    }
    if (t.negTags && t.negTags.length) {
      const neg = el('div', { class: 'detail-facts' });
      for (const tag of t.negTags) neg.append(el('span', { class: 'tag', text: tag }));
      body.append(el('div', { class: 'sect' }, el('h3', { text: 'Negative tags' }), neg));
    }

    // Related lists play as their own queue, so hitting one of a track's 40
    // covers and pressing ⏭ walks the covers rather than the search results.
    const relate = (label, list) => {
      if (!list || !list.length) return;
      const playable = list.filter((r) => r.resolved).map((r) => r.track);
      const head = el('div', { style: 'display:flex;gap:8px;align-items:center;margin-bottom:8px' },
        el('h3', { style: 'margin:0', text: label }),
        playable.length ? el('button', {
          class: 'ctl-btn', text: `▶ Play ${playable.length}`,
          onclick: () => setQueue(playable, 0, label.toLowerCase()),
        }) : null,
        playable.length ? el('button', {
          class: 'ctl-btn', text: '+ Playlist',
          onclick: () => addToPlaylist(playable.map((x) => x.id)),
        }) : null,
      );
      const box = el('div');
      for (const r of list) {
        box.append(el('div', {
          class: 'node-row' + (r.resolved && state.current && state.current.id === r.id ? ' playing' : '')
            + (r.resolved ? '' : ' unresolved'),
          'data-track-id': r.resolved ? r.id : null,
          title: r.resolved ? 'Play' : '',
          onclick: () => {
            if (!r.resolved) return;
            const i = playable.findIndex((x) => x.id === r.id);
            setQueue(playable, Math.max(0, i), label.toLowerCase());
          },
        },
          el('span', {}),
          el('img', { class: 'node-cover', alt: '', src: r.resolved && r.track.hasCover ? `/api/cover/${r.id}` : '' }),
          el('div', { class: 'node-main' },
            el('div', { class: 'node-title', text: r.resolved ? (r.track.title || '(untitled)') : r.id }),
            el('div', { class: 'node-sub' }, kindBadge(r.kind),
              r.resolved ? el('span', { text: fmtDate(r.track.created) })
                : el('span', { class: 'missing', text: state.backfillDone ? 'not in archive' : 'not archived yet' }))),
          r.resolved ? el('button', {
            class: 'icon-btn', text: '⑃', title: 'Show its relationships',
            onclick: (e) => { e.stopPropagation(); openLineage(r.id); },
          }) : el('span', {})));
      }
      body.append(el('div', { class: 'sect' }, head, box));
    };
    relate(`Derived from (${d.parents.length})`, d.parents);
    relate(`Derived works (${d.children.length})`, d.children);

    const lyrics = meta.metadata && meta.metadata.prompt;
    if (lyrics && lyrics.trim()) {
      body.append(el('div', { class: 'sect' }, el('h3', { text: 'Lyrics' }), el('div', { class: 'lyrics', text: lyrics })));
    }
    const desc = meta.metadata && meta.metadata.gpt_description_prompt;
    if (desc) body.append(el('div', { class: 'sect' }, el('h3', { text: 'Description prompt' }), el('div', { class: 'lyrics', text: desc })));

    body.append(el('div', { class: 'sect' }, el('h3', { text: 'Raw metadata' }),
      el('div', { class: 'rawjson', text: JSON.stringify(meta, null, 2) })));
  } catch (e) {
    body.replaceChildren(el('div', { class: 'empty', text: 'Failed: ' + e.message }));
  }
}

// ── playlists ──────────────────────────────────────────────────

function renderPlaylists() {
  const box = $('#playlists');
  box.replaceChildren();
  if (!state.playlists.length) {
    box.append(el('div', { style: 'color:var(--dimmer);font-size:12px;padding:6px 10px', text: 'No playlists yet' }));
  }
  for (const p of state.playlists) {
    box.append(el('div', {
      class: 'pl-item' + (state.playlist === p.id ? ' active' : ''),
      onclick: () => selectPlaylist(p.id),
    },
      el('span', { class: 'pl-name', text: p.name, title: p.note || p.name }),
      el('span', { class: 'pl-count', text: String(p.trackIds.length) }),
      el('button', {
        class: 'pl-count', text: '×', title: 'Delete playlist',
        onclick: async (e) => {
          e.stopPropagation();
          if (!confirm(`Delete playlist "${p.name}"?`)) return;
          await api('/api/playlists/' + p.id, { method: 'DELETE' });
          if (state.playlist === p.id) state.playlist = null;
          await loadPlaylists();
          load();
        },
      })));
  }
}

function selectPlaylist(id) {
  state.playlist = id;
  state.view = 'library';
  setNav('library');
  renderPlaylists();
  load();
}

async function addToPlaylist(ids) {
  if (!ids.length) return;
  const names = state.playlists.map((p, i) => `${i + 1}. ${p.name}`).join('\n');
  const ans = prompt(
    `Add ${ids.length} track(s) to which playlist?\n\n${names}\n\nEnter a number, or a new playlist name:`,
    state.playlists.length ? '1' : 'My playlist');
  if (ans === null) return;
  const idx = parseInt(ans, 10);
  try {
    if (!isNaN(idx) && idx >= 1 && idx <= state.playlists.length) {
      await api(`/api/playlists/${state.playlists[idx - 1].id}/tracks`,
        { method: 'POST', body: JSON.stringify({ trackIds: ids }) });
    } else {
      await api('/api/playlists', { method: 'POST', body: JSON.stringify({ name: ans, trackIds: ids }) });
    }
    await loadPlaylists();
  } catch (e) { alert('Failed: ' + e.message); }
}

// ── player ─────────────────────────────────────────────────────

const audio = $('#audio');

// setQueue replaces the play queue and starts at index i. The queue is
// whatever list the user played from — search results, a playlist, or a
// lineage family — so next/prev always walk the context they clicked in.
function setQueue(tracks, i = 0, label = '') {
  state.queue = tracks.filter(Boolean);
  state.queueLabel = label;
  playFrom(i);
}

function playTrack(t) {
  const i = state.queue.findIndex((r) => r.id === t.id);
  if (i >= 0) return playFrom(i);
  setQueue([t], 0);
}

function playFrom(i) {
  const t = state.queue[i];
  if (!t) return;
  state.qi = i;
  state.current = t;
  audio.src = `/api/audio/${t.id}`;
  audio.play().catch(() => {});
  $('#nowTitle').textContent = t.title || '(untitled)';
  $('#nowSub').textContent = [
    t.persona, t.model, fmtDate(t.created),
    state.queueLabel ? `${i + 1}/${state.queue.length} · ${state.queueLabel}` : null,
  ].filter(Boolean).join(' · ');
  $('#nowCover').src = t.hasCover ? `/api/cover/${t.id}` : '';
  $('#playBtn').textContent = '❚❚';
  markPlaying();
  if (nowUI.open) { paintNow(); loadLyricsFor(t); }
}

// markPlaying highlights the current track wherever it is on screen — the
// result list and the lineage tree can both show it at once, so this matches
// on id rather than on row position.
function markPlaying() {
  const id = state.current && state.current.id;
  document.querySelectorAll('[data-track-id]').forEach((n) => {
    n.classList.toggle('playing', !!id && n.dataset.trackId === id);
  });
}

function step(delta) {
  if (state.qi < 0) return;
  const n = state.qi + delta;
  if (n >= 0 && n < state.queue.length) playFrom(n);
}

// ── full-screen now playing ────────────────────────────────────
//
// Suno archives no word timings, alignment data or LRC of any kind (checked
// across the archive: no such field exists), so the lyrics cannot be truly
// synced. What we do instead is scroll them proportionally to playback
// position, which tracks a song loosely and reads well; the user can turn it
// off, and any manual scroll suspends it briefly so it doesn't fight them.

const nowUI = {
  open: false,
  trackId: null,
  showLyrics: true,
  autoScroll: true,
  suspendUntil: 0,
  lines: [],
};

// Renders lyric text, promoting Suno's [Section] markers to styled pills.
function renderLyrics(text) {
  const box = $('#nowLyrics');
  box.replaceChildren();
  nowUI.lines = [];
  const trimmed = (text || '').trim();
  if (!trimmed) {
    box.append(el('div', { class: 'now-none', text: 'No lyrics archived for this track.' }));
    return;
  }
  for (const raw of trimmed.split('\n')) {
    const line = raw.trim();
    if (!line) { box.append(el('div', { class: 'blank' })); continue; }
    const m = line.match(/^\[(.+)\]$/);
    if (m) {
      box.append(el('div', {}, el('span', { class: 'sec', text: m[1] })));
      continue;
    }
    const n = el('div', { class: 'ln', text: line });
    box.append(n);
    nowUI.lines.push(n);
  }
}

// Scroll the lyric column to match playback progress.
function syncLyricScroll() {
  if (!nowUI.open || !nowUI.autoScroll || !nowUI.showLyrics) return;
  if (Date.now() < nowUI.suspendUntil) return;
  const box = $('#nowLyrics');
  const span = box.scrollHeight - box.clientHeight;
  if (span <= 0 || !audio.duration || !isFinite(audio.duration)) return;
  const p = Math.min(1, Math.max(0, audio.currentTime / audio.duration));
  const target = p * span;
  // Ease toward the target so the text glides instead of stepping once a
  // second with timeupdate.
  box.scrollTop += (target - box.scrollTop) * 0.12;

  // Emphasise the lines nearest the middle of the viewport.
  const mid = box.scrollTop + box.clientHeight / 2;
  for (const ln of nowUI.lines) {
    const c = ln.offsetTop + ln.offsetHeight / 2;
    ln.classList.toggle('near', Math.abs(c - mid) < box.clientHeight * 0.18);
  }
}

async function loadLyricsFor(t) {
  if (!t) return;
  nowUI.trackId = t.id;
  const box = $('#nowLyrics');
  box.replaceChildren(el('div', { class: 'now-none', text: 'Loading lyrics…' }));
  try {
    const d = await api('/api/lyrics/' + encodeURIComponent(t.id));
    if (nowUI.trackId !== t.id) return; // track changed while fetching
    renderLyrics(d.lyrics || d.description || '');
    box.scrollTop = 0;
  } catch {
    if (nowUI.trackId === t.id) renderLyrics('');
  }
}

function paintNow() {
  const t = state.current;
  if (!t) return;
  $('#nowFsTitle').textContent = t.title || '(untitled)';
  $('#nowFsSub').textContent = [
    t.persona, t.model, fmtDate(t.created),
    state.queueLabel ? `${state.qi + 1}/${state.queue.length} · ${state.queueLabel}` : null,
  ].filter(Boolean).join(' · ');

  const art = t.hasCover ? `/api/cover/${t.id}` : '';
  $('#nowFsArt').src = art;
  $('#nowBg').style.backgroundImage = art ? `url("${art}")` : 'none';

  const meta = $('#nowFsMeta');
  meta.replaceChildren();
  if (t.task) meta.append(el('span', { class: 'tag', text: t.task.replace(/_/g, ' ') }));
  if (t.version) meta.append(el('span', { class: 'tag', text: t.version }));
  for (const tag of (t.tags || []).slice(0, 6)) meta.append(el('span', { class: 'tag', text: tag }));
}

function openNow() {
  if (!state.current) return;
  nowUI.open = true;
  $('#now').classList.remove('hidden');
  paintNow();
  loadLyricsFor(state.current);
}

function closeNow() {
  nowUI.open = false;
  $('#now').classList.add('hidden');
}

// ── drawers / nav ──────────────────────────────────────────────

function openDrawer(sel) {
  closeDrawers();
  $(sel).classList.remove('hidden');
  $('#scrim').classList.remove('hidden');
}

function closeDrawers() {
  $('#lineage').classList.add('hidden');
  $('#detail').classList.add('hidden');
  $('#scrim').classList.add('hidden');
}

function setNav(view) {
  document.querySelectorAll('.nav-item').forEach((b) => b.classList.toggle('active', b.dataset.view === view));
}

// ── wiring ─────────────────────────────────────────────────────

let searchTimer;
$('#search').addEventListener('input', (e) => {
  clearTimeout(searchTimer);
  const v = e.target.value;
  searchTimer = setTimeout(() => { state.q = v; load(); }, 220);
});
$('#clearSearch').addEventListener('click', () => { $('#search').value = ''; state.q = ''; load(); });

$('#sort').addEventListener('change', (e) => { state.sort = e.target.value; load(); });
$('#orderBtn').addEventListener('click', (e) => {
  state.order = state.order === 'desc' ? 'asc' : 'desc';
  e.target.textContent = state.order === 'desc' ? '↓' : '↑';
  load();
});
$('#groupToggle').addEventListener('change', (e) => { state.group = e.target.checked; load(); });

$('#filterBtn').addEventListener('click', () => {
  const f = $('#filters');
  f.classList.toggle('hidden');
  $('#filterBtn').classList.toggle('on', !f.classList.contains('hidden'));
  renderFilters();
});
$('#closeFilters').addEventListener('click', () => {
  $('#filters').classList.add('hidden');
  $('#filterBtn').classList.remove('on');
});
$('#resetFilters').addEventListener('click', resetFilters);

document.querySelectorAll('.nav-item').forEach((b) => b.addEventListener('click', () => {
  const v = b.dataset.view;
  setNav(v);
  if (v === 'liked') {
    state.view = 'library'; state.playlist = null; state.tri.liked = true;
  } else if (v === 'personas') {
    state.view = 'personas';
  } else {
    state.view = 'library'; state.playlist = null;
  }
  renderPlaylists();
  if (state.view === 'personas') renderPersonas(); else load();
}));

$('#newPlaylist').addEventListener('click', async () => {
  const name = prompt('New playlist name:');
  if (!name) return;
  try {
    await api('/api/playlists', { method: 'POST', body: JSON.stringify({ name }) });
    await loadPlaylists();
  } catch (e) { alert('Failed: ' + e.message); }
});

$('#closeLineage').addEventListener('click', closeDrawers);
$('#closeDetail').addEventListener('click', closeDrawers);
$('#scrim').addEventListener('click', closeDrawers);
document.addEventListener('keydown', (e) => {
  if (e.key === 'Escape') { if (nowUI.open) closeNow(); else closeDrawers(); }
  if (e.key === ' ' && e.target === document.body) {
    e.preventDefault();
    audio.paused ? audio.play() : audio.pause();
  }
  // 'f' toggles the full-screen view, like a player.
  if ((e.key === 'f' || e.key === 'F') && e.target === document.body) {
    nowUI.open ? closeNow() : openNow();
  }
});

$('#playBtn').addEventListener('click', () => { audio.paused ? audio.play() : audio.pause(); });
$('#prevBtn').addEventListener('click', () => step(-1));
$('#nextBtn').addEventListener('click', () => step(1));
$('#lineageBtn').addEventListener('click', () => {
  const t = state.queue[state.qi];
  if (t) openLineage(t.id);
});
$('#addBtn').addEventListener('click', () => {
  const t = state.queue[state.qi];
  if (t) addToPlaylist([t.id]);
});

audio.addEventListener('play', () => { $('#playBtn').textContent = '❚❚'; $('#nowPlay').textContent = '❚❚'; });
audio.addEventListener('pause', () => { $('#playBtn').textContent = '▶'; $('#nowPlay').textContent = '▶'; });
audio.addEventListener('ended', () => step(1));
audio.addEventListener('timeupdate', () => {
  $('#curTime').textContent = fmtTime(audio.currentTime);
  if (audio.duration) $('#seek').value = String((audio.currentTime / audio.duration) * 1000);
  if (nowUI.open) {
    $('#nowCur').textContent = fmtTime(audio.currentTime);
    if (audio.duration) $('#nowSeek').value = String((audio.currentTime / audio.duration) * 1000);
    syncLyricScroll();
  }
});
audio.addEventListener('loadedmetadata', () => {
  $('#durTime').textContent = fmtTime(audio.duration);
  $('#nowDur').textContent = fmtTime(audio.duration);
});

// Full-screen now-playing wiring.
$('#expandBtn').addEventListener('click', openNow);
$('#nowCover').addEventListener('click', openNow);
$('#closeNow').addEventListener('click', closeNow);
$('#nowPlay').addEventListener('click', () => { audio.paused ? audio.play() : audio.pause(); });
$('#nowPrev').addEventListener('click', () => step(-1));
$('#nowNext').addEventListener('click', () => step(1));
$('#nowSeek').addEventListener('input', (e) => {
  if (audio.duration) audio.currentTime = (e.target.value / 1000) * audio.duration;
});
$('#nowLineage').addEventListener('click', () => {
  if (state.current) { closeNow(); openLineage(state.current.id); }
});
$('#nowAdd').addEventListener('click', () => { if (state.current) addToPlaylist([state.current.id]); });
$('#nowLyricsToggle').addEventListener('click', (e) => {
  nowUI.showLyrics = !nowUI.showLyrics;
  e.target.classList.toggle('on', nowUI.showLyrics);
  $('.now-lyrics-wrap').classList.toggle('hidden', !nowUI.showLyrics);
  $('.now-body').style.gridTemplateColumns = nowUI.showLyrics ? '' : '1fr';
});
$('#nowAutoScroll').addEventListener('click', (e) => {
  nowUI.autoScroll = !nowUI.autoScroll;
  e.target.classList.toggle('on', nowUI.autoScroll);
  $('#nowLyrics').classList.toggle('dim', nowUI.autoScroll);
});
// A manual scroll suspends the auto-scroll briefly rather than fighting it.
$('#nowLyrics').addEventListener('wheel', () => { nowUI.suspendUntil = Date.now() + 6000; }, { passive: true });
$('#nowLyrics').addEventListener('touchstart', () => { nowUI.suspendUntil = Date.now() + 6000; }, { passive: true });
$('#nowLyrics').classList.add('dim');
$('#seek').addEventListener('input', (e) => {
  if (audio.duration) audio.currentTime = (e.target.value / 1000) * audio.duration;
});
$('#volume').addEventListener('input', (e) => { audio.volume = e.target.value / 100; });

// ── boot ───────────────────────────────────────────────────────

refreshStatus();
setInterval(refreshStatus, 15000);
loadPlaylists();
load();
