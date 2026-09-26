// The diff model (hunk order, gaps, commentable lines) and the split and
// unified diff tables with GitHub-style context expansion.
import { esc, api } from "./util.js";
import { S, prBase, fileByPath } from "./state.js";
import { threadRow } from "./comments.js";

const EXPAND_STEP = 20;
// GitHub review comments must land on a changed line or within this many
// context lines of one.
const GH_CONTEXT = 3;

// hunkEnd walks a hunk and returns the next old/new line after it.
function hunkEnd(h) {
  let o = h.old_start, n = h.new_start;
  for (const l of h.lines || []) {
    if (l[0] === "+") n++;
    else if (l[0] === "-") o++;
    else if (l[0] !== "\\") { o++; n++; }
  }
  return { o, n };
}

// prepare computes, per file, hunk order, the gap above each hunk, and
// which lines GitHub will accept comments on.
export function prepare(r) {
  for (const f of r.files) {
    const hunks = [];
    (f.units || []).forEach((u) => (u.hunks || []).forEach((h, i) => hunks.push({ h, key: `${f.path}#${u.id}#${i}` })));
    hunks.sort((a, b) => a.h.new_start - b.h.new_start || a.h.old_start - b.h.old_start);
    let prev = { o: 1, n: 1 };
    const changes = []; // {n, del} positions in new-file coordinates
    for (const x of hunks) {
      x.h._key = x.key;
      x.h._gapFrom = prev.n;                        // first new line not yet shown
      x.h._offset = x.h.old_start - x.h.new_start;  // old = new + offset inside the gap
      let n = x.h.new_start;
      for (const l of x.h.lines || []) {
        if (l[0] === "+") { changes.push({ n, del: false }); n++; }
        else if (l[0] === "-") changes.push({ n, del: true });
        else if (l[0] !== "\\") n++;
      }
      prev = hunkEnd(x.h);
    }
    f._last = hunks.length ? hunks[hunks.length - 1].h : null;
    f._tailFrom = prev;
    // A context line at new line n is in GitHub's diff if it's within
    // GH_CONTEXT of an added line, or of the spot a deletion sits at.
    f._commentable = (n) => changes.some((c) => {
      const d = c.del ? (n >= c.n ? n - c.n + 1 : c.n - n) : Math.abs(n - c.n);
      return d <= GH_CONTEXT;
    });
  }
}

const headLines = (f) => S.files[`head:${f.path}`];
const canExpand = (f) => f.status !== "deleted" && f.status !== "added" && !f.binary && !!f._last;
const fileHunks = (f) => (f.units || []).flatMap((u) => u.hunks || []);

// fullyExpanded reports whether every gap in f, and its tail, is open.
export function fullyExpanded(f) {
  const lines = headLines(f);
  if (!lines || !canExpand(f)) return false;
  return fileHunks(f).every((h) => (S.above[h._key] || 0) >= h.new_start - h._gapFrom) &&
    (S.below[f.path] || 0) >= lines.length - f._tailFrom.n + 1;
}

// expandAllButton is the file header toggle that shows the whole file,
// like GitHub's "Expand all".
export function expandAllButton(f, act = "expand-all") {
  if (!canExpand(f)) return "";
  const on = fullyExpanded(f);
  return `<button class="details-btn expand-all" data-act="${act}" data-path="${esc(f.path)}" title="${on ? "Collapse expanded context" : "Show the whole file"}">${on ? "⤒ collapse all" : "↕ expand all"}</button>`;
}

async function ensureHead(f) {
  const k = `head:${f.path}`;
  if (!S.files[k]) {
    const q = new URLSearchParams({ key: S.result.key, path: f.path, side: "head" });
    S.files[k] = (await api(`${prBase()}/file?${q}`)).lines;
  }
  return S.files[k];
}

// rowsFor turns a hunk (plus expanded context) into display rows:
// {t: "ctx"|"add"|"del"|"hh"|"meta"|"exp", o, n, text, x (expanded), cm (commentable)}
function rowsFor(f, h, isLast) {
  const rows = [];
  const expandable = canExpand(f);
  if (expandable) {
    const gap = h.new_start - h._gapFrom;
    const shown = Math.min(S.above[h._key] || 0, gap);
    if (gap - shown > 0) rows.push({ t: "exp", dir: "up", key: h._key, path: f.path, left: gap - shown });
    if (shown > 0) rows.push({ t: "exp", dir: "fold-up", key: h._key, left: shown });
    const lines = headLines(f);
    for (let n = h.new_start - shown; n < h.new_start; n++) {
      rows.push({ t: "ctx", o: n + h._offset, n, text: " " + (lines?.[n - 1] ?? ""), x: true });
    }
  }
  rows.push({ t: "hh", text: h.header });
  let o = h.old_start, n = h.new_start;
  for (const l of h.lines || []) {
    if (l[0] === "+") rows.push({ t: "add", n: n++, text: l, cm: true });
    else if (l[0] === "-") rows.push({ t: "del", o: o++, text: l, cm: true });
    else if (l[0] === "\\") rows.push({ t: "meta", text: l });
    else { rows.push({ t: "ctx", o: o++, n, text: l, cm: f._commentable(n) }); n++; }
  }
  if (isLast && expandable) {
    const lines = headLines(f);
    const shown = S.below[f.path] || 0;
    const end = f._tailFrom;
    for (let i = 0; i < shown && lines && end.n + i <= lines.length; i++) {
      rows.push({ t: "ctx", o: end.o + i, n: end.n + i, text: " " + lines[end.n + i - 1], x: true });
    }
    const more = lines ? lines.length - (end.n + shown - 1) : null;
    const expanded = lines ? Math.max(0, Math.min(shown, lines.length - end.n + 1)) : 0;
    if (expanded > 0) rows.push({ t: "exp", dir: "fold-down", path: f.path, left: expanded });
    if (more === null || more > 0) rows.push({ t: "exp", dir: "down", path: f.path, left: more });
  }
  return rows;
}

// fileRows is every display row of f, all units' hunks in file order.
// Rows of other units' hunks are flagged other; the first changed row of
// focus is flagged start, so the walkthrough can dim and scroll.
export function fileRows(f, focus) {
  const mine = new Set(focus.hunks || []);
  const rows = [];
  let started = false;
  for (const h of fileHunks(f).sort((a, b) => a.new_start - b.new_start || a.old_start - b.old_start)) {
    for (const r of rowsFor(f, h, h === f._last)) {
      if (!mine.has(h) && (r.t === "add" || r.t === "del" || (r.t === "ctx" && !r.x) || r.t === "hh")) r.other = true;
      if (mine.has(h) && !started && (r.t === "add" || r.t === "del")) r.start = started = true;
      rows.push(r);
    }
  }
  return rows;
}

// unitRows is every display row for a unit's hunks.
export function unitRows(f, u) {
  const rows = [];
  (u.hunks || []).forEach((h) => rows.push(...rowsFor(f, h, h === f._last)));
  return rows;
}

function gutter(f, side, line, num, cls, cm) {
  const btn = cm ? `<button class="cm" title="Add a review comment" data-act="comment" data-path="${esc(f.path)}" data-side="${side}" data-line="${line}">+</button>` : "";
  return `<td class="n ${cls}">${btn}${num ?? ""}</td>`;
}

function expRow(r, cols) {
  const lines = (n) => `${n} line${n > 1 ? "s" : ""}`;
  let label, data;
  switch (r.dir) {
    case "up":
      label = r.left <= EXPAND_STEP ? `↕ expand ${lines(r.left)} hidden` : `↑ expand ${EXPAND_STEP} lines (${r.left} hidden)`;
      data = `data-act="up" data-key="${esc(r.key)}" data-path="${esc(r.path)}"`;
      break;
    case "down":
      label = r.left === null ? "↓ expand below" : `↓ expand ${Math.min(EXPAND_STEP, r.left)} of ${r.left} lines below`;
      data = `data-act="down" data-path="${esc(r.path)}"`;
      break;
    case "fold-up":
      label = `⤒ collapse ${lines(r.left)} of expanded context`;
      data = `data-act="fold-up" data-key="${esc(r.key)}"`;
      break;
    case "fold-down":
      label = `⤓ collapse ${lines(r.left)} of expanded context`;
      data = `data-act="fold-down" data-path="${esc(r.path)}"`;
      break;
  }
  return `<tr class="exp ${r.dir.startsWith("fold") ? "fold" : ""}"><td colspan="${cols}"><button ${data}>${label}</button></td></tr>`;
}

// Rows flagged with iss (a review issue's line) get a marker and data-iss,
// so the walkthrough can scroll to them.
const issAttrs = (x, r, start = r?.start) => `class="${x ? "ctx-x" : ""} ${r?.iss ? "iss" : ""} ${r?.other ? "other" : ""} ${start ? "focus-start" : ""}"${r?.iss ? ` data-iss="${r.n}"` : ""}`;

function unifiedTable(f, rows) {
  let out = "";
  for (const r of rows) {
    if (r.t === "exp") { out += expRow(r, 3); continue; }
    if (r.t === "hh" || r.t === "meta") { out += `<tr class="hh ${r.other ? "other" : ""}"><td class="n"></td><td class="n"></td><td class="c">${esc(r.text)}</td></tr>`; continue; }
    const cls = r.t === "add" ? "add" : r.t === "del" ? "del" : "";
    const cm = !r.x && r.cm;
    // Unified: deletions comment on the old line, everything else on the new one.
    out += `<tr ${issAttrs(r.x, r)}>
      ${gutter(f, "LEFT", r.o, r.o, cls, cm && r.t === "del")}
      ${gutter(f, "RIGHT", r.n, r.n, cls, cm && r.t !== "del")}
      <td class="c ${cls}">${esc(r.text)}</td></tr>`;
    if (!r.x) {
      const anchors = r.t === "ctx" ? [{ side: "LEFT", line: r.o }, { side: "RIGHT", line: r.n }]
        : r.t === "del" ? [{ side: "LEFT", line: r.o }] : [{ side: "RIGHT", line: r.n }];
      out += threadRow(f, anchors, 3);
    }
  }
  return `<table class="diff unified"><colgroup><col style="width:52px"><col style="width:52px"><col></colgroup>${out}</table>`;
}

function splitTable(f, rows) {
  // Pair each run of deletions with the run of additions that follows it.
  const pairs = [];
  let dels = [], adds = [];
  const flush = () => {
    for (let i = 0; i < Math.max(dels.length, adds.length); i++) pairs.push({ l: dels[i], r: adds[i] });
    dels = []; adds = [];
  };
  for (const r of rows) {
    if (r.t === "del") { if (adds.length) flush(); dels.push(r); }
    else if (r.t === "add") adds.push(r);
    else { flush(); pairs.push(r.t === "ctx" ? { l: r, r } : { whole: r }); }
  }
  flush();

  const cell = (x, side) => {
    const sep = side === "LEFT" ? "split-l" : "";
    if (!x) return `<td class="n empty"></td><td class="c empty ${sep}"></td>`;
    const cls = x.t === "add" ? "add" : x.t === "del" ? "del" : "";
    const line = side === "LEFT" ? x.o : x.n;
    return `${gutter(f, side, line, line, cls, !x.x && x.cm)}<td class="c ${cls} ${sep}">${esc(x.text.slice(1))}</td>`;
  };
  let out = "";
  for (const p of pairs) {
    if (p.whole) {
      out += p.whole.t === "exp" ? expRow(p.whole, 4) : `<tr class="hh ${p.whole.other ? "other" : ""}"><td class="n"></td><td class="c" colspan="3">${esc(p.whole.text)}</td></tr>`;
      continue;
    }
    const x = (p.l || p.r).x;
    out += `<tr ${issAttrs(x, p.r || p.l, p.l?.start || p.r?.start)}>${cell(p.l, "LEFT")}${cell(p.r, "RIGHT")}</tr>`;
    if (!x) {
      const anchors = [];
      if (p.l) anchors.push({ side: "LEFT", line: p.l.o });
      if (p.r) anchors.push({ side: "RIGHT", line: p.r.n });
      out += threadRow(f, anchors, 4);
    }
  }
  return `<table class="diff split"><colgroup><col style="width:52px"><col><col style="width:52px"><col></colgroup>${out}</table>`;
}

// diffTable renders rows as a split or unified table.
export function diffTable(f, rows, view) {
  if (!rows.length) return `<div class="nodiff">no textual diff</div>`;
  return view === "split" ? splitTable(f, rows) : unifiedTable(f, rows);
}

async function expand(el, apply) {
  el.disabled = true;
  el.textContent = "loading…";
  try { await ensureHead(fileByPath(el.dataset.path)); }
  catch (err) { el.textContent = "could not load file: " + err.message; return false; }
  apply();
}

export const actions = {
  "fold-up": (el) => { delete S.above[el.dataset.key]; },
  "fold-down": (el) => { delete S.below[el.dataset.path]; },
  up: (el) => expand(el, () => { S.above[el.dataset.key] = (S.above[el.dataset.key] || 0) + EXPAND_STEP; }),
  down: (el) => expand(el, () => { S.below[el.dataset.path] = (S.below[el.dataset.path] || 0) + EXPAND_STEP; }),
  "expand-all": (el) => {
    const f = fileByPath(el.dataset.path);
    if (fullyExpanded(f)) {
      fileHunks(f).forEach((h) => delete S.above[h._key]);
      delete S.below[f.path];
      return;
    }
    S.collapsed.delete(f.path);
    (f.units || []).forEach((u) => S.diffOpen[u.id] = true);
    return expand(el, () => {
      fileHunks(f).forEach((h) => { S.above[h._key] = h.new_start - h._gapFrom; });
      S.below[f.path] = headLines(f).length;
    });
  },
};
