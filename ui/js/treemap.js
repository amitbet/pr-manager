// Code map tab: a squarified treemap of the repo colored by impact, by
// likelihood, or by both at once, with the PR's units as dots, and a table
// of every change.
import { $, esc, api, LABEL, headline } from "./util.js";
import { S, allUnits } from "./state.js";
import { risk, impactPill, likelihoodPill, attentionPill } from "./scores.js";
import { jumpToUnit } from "./review.js";

// Impact and likelihood are magnitudes, so each gets one hue, near-surface
// at 0 and strongest at 100: red for impact, green for likelihood. Dark mode
// has its own steps: low recedes into the dark surface, critical is the
// brightest.
const STOPS = {
  impact: {
    light: [[0, [243, 241, 238]], [35, [247, 206, 201]], [55, [238, 140, 132]], [75, [214, 66, 61]], [100, [124, 23, 23]]],
    dark: [[0, [38, 36, 36]], [35, [82, 40, 38]], [55, [139, 51, 47]], [75, [190, 58, 54]], [100, [222, 78, 70]]],
  },
  // The green is yellowish and lighter than the red at every step, so the
  // two stay apart for red-green colorblind readers (deutan ΔE 15.9 light,
  // 17.5 dark between the "high impact" and "likely to break" corners).
  likelihood: {
    light: [[0, [243, 241, 238]], [35, [222, 240, 200]], [55, [178, 218, 140]], [75, [120, 188, 90]], [100, [70, 150, 60]]],
    dark: [[0, [38, 36, 36]], [35, [44, 74, 40]], [55, [78, 130, 60]], [75, [124, 180, 86]], [100, [168, 220, 124]]],
  },
};
const MODES = { impact: "impact", likelihood: "likelihood", both: "impact + likelihood" };
const isDark = () => document.documentElement.dataset.theme === "dark";
const BRANK = { none: 0, aux: 1, skim: 2, human: 3 };

function rampRGB(kind, d) {
  const st = STOPS[kind][isDark() ? "dark" : "light"];
  d = Math.max(0, Math.min(100, d || 0));
  for (let i = 1; i < st.length; i++) {
    const [d0, c0] = st[i - 1], [d1, c1] = st[i];
    if (d <= d1) {
      const t = (d - d0) / (d1 - d0);
      return c0.map((v, k) => Math.round(v + (c1[k] - v) * t));
    }
  }
  return st[st.length - 1][1];
}
const rgb = (c) => `rgb(${c.join(",")})`;

// bothRGB mixes the two ramps the way overlapping inks mix: on light, a
// multiply blend, so a cell high on both is the darkest (olive to near
// black); on dark, a screen blend, so it is the brightest (yellow). The
// lightness step keeps "both high" readable without telling red from green.
function bothRGB(imp, lk) {
  const a = rampRGB("impact", imp), b = rampRGB("likelihood", lk), s = rampRGB("impact", 0);
  return a.map((v, k) => Math.round(isDark()
    ? 255 - ((255 - v) * (255 - b[k])) / (255 - s[k])
    : (v * b[k]) / s[k]));
}
function fillRGB(n) {
  if (S.tm.mode === "impact") return rampRGB("impact", n.im);
  if (S.tm.mode === "likelihood") return rampRGB("likelihood", n.lk);
  return bothRGB(n.im, n.lk);
}
// Labels stay in text ink; switch to the inverse ink on fills too dark for it.
function inkFor(c) {
  const [r, g, b] = c.map((v) => { v /= 255; return v <= 0.03928 ? v / 12.92 : ((v + 0.055) / 1.055) ** 2.4; });
  const L = 0.2126 * r + 0.7152 * g + 0.0722 * b;
  return L > 0.25 ? "#1f2328" : "#ffffff";
}

function prepTree(n) {
  if (n._size !== undefined) return n._size;
  n._size = n.c?.length ? n.c.reduce((a, c) => a + prepTree(c), 0) : Math.max(1, n.s || 1);
  (n.c || []).forEach((c) => (c._parent = n));
  return n._size;
}

// squarify lays items (sorted by value, largest first) into the rect with
// aspect ratios close to 1 (Bruls, Huizing & van Wijk).
function squarify(items, x, y, w, h) {
  const total = items.reduce((a, i) => a + i.v, 0);
  const out = [];
  if (total <= 0 || w <= 0 || h <= 0) return out;
  const scale = (w * h) / total;
  const rest = items.map((i) => ({ node: i.node, a: i.v * scale }));
  let row = [];
  const worst = (r, side) => {
    const sum = r.reduce((a, i) => a + i.a, 0);
    let mx = 0, mn = Infinity;
    for (const i of r) { mx = Math.max(mx, i.a); mn = Math.min(mn, i.a); }
    return Math.max((side * side * mx) / (sum * sum), (sum * sum) / (side * side * mn));
  };
  const flush = () => {
    if (!row.length) return;
    const sum = row.reduce((a, i) => a + i.a, 0);
    if (w >= h) {
      const cw = sum / h; let yy = y;
      for (const i of row) { const rh = i.a / cw; out.push({ node: i.node, x, y: yy, w: cw, h: rh }); yy += rh; }
      x += cw; w -= cw;
    } else {
      const rh = sum / w; let xx = x;
      for (const i of row) { const rw = i.a / rh; out.push({ node: i.node, x: xx, y, w: rw, h: rh }); xx += rw; }
      y += rh; h -= rh;
    }
    row = [];
  };
  while (rest.length) {
    const side = Math.min(w, h);
    if (!row.length || worst([...row, rest[0]], side) <= worst(row, side)) row.push(rest.shift());
    else flush();
  }
  flush();
  return out;
}

const HEAD = 16, PAD = 2;
function layoutTree(node, x, y, w, h, depth, out) {
  node._rect = { x, y, w, h };
  node._open = false;
  out.push({ node, depth });
  if (!node.c?.length || w < 28 || h < 28) return;
  const head = h > 40 && w > 48 ? HEAD : 0;
  const ix = x + PAD, iy = y + head + PAD, iw = w - 2 * PAD, ih = h - head - 2 * PAD;
  if (iw < 6 || ih < 6) return;
  node._open = true;
  node._head = head;
  const items = node.c.map((c) => ({ node: c, v: c._size })).filter((i) => i.v > 0).sort((a, b) => b.v - a.v);
  for (const r of squarify(items, ix, iy, iw, ih)) layoutTree(r.node, r.x, r.y, r.w, r.h, depth + 1, out);
}

// findNode walks path segments ("api/v1/user/server.go") from a repo node
// and returns the deepest existing node.
function findNode(repoNode, path) {
  let n = repoNode;
  for (const seg of String(path || "").split("/").filter(Boolean)) {
    const next = (n.c || []).find((c) => c.n === seg);
    if (!next) break;
    n = next;
  }
  return n;
}
const nodePath = (n) => { const out = []; for (let x = n; x?._parent; x = x._parent) out.unshift(x.n); return out; };

// Dot marks: filled = human review, ring = skim, small ring = auxiliary,
// small = no review.
// Ink with a surface ring so they read on every step of the red ramp.
function dotMark(bucket, cx, cy) {
  const ink = isDark() ? "#f0f3f6" : "#1f2328", surf = isDark() ? "#0d1117" : "#ffffff";
  if (bucket === "human") return `<circle class="mark" cx="${cx}" cy="${cy}" r="6" fill="${ink}" stroke="${surf}" stroke-width="2"/>`;
  if (bucket === "skim") return `<circle cx="${cx}" cy="${cy}" r="6.5" fill="none" stroke="${surf}" stroke-width="5"/><circle class="mark" cx="${cx}" cy="${cy}" r="5" fill="${surf}" stroke="${ink}" stroke-width="2.5"/>`;
  if (bucket === "aux") return `<circle cx="${cx}" cy="${cy}" r="5" fill="none" stroke="${surf}" stroke-width="4"/><circle class="mark" cx="${cx}" cy="${cy}" r="3.5" fill="${surf}" stroke="${ink}" stroke-width="2"/>`;
  return `<circle class="mark" cx="${cx}" cy="${cy}" r="3.5" fill="${ink}" stroke="${surf}" stroke-width="2"/>`;
}

// Dashed ring around a dot whose file is not in the code map.
function ghostRing(cx, cy) {
  return `<circle cx="${cx}" cy="${cy}" r="9" fill="${isDark() ? "#0d1117" : "#ffffff"}" fill-opacity=".75" stroke="${isDark() ? "#f0f3f6" : "#1f2328"}" stroke-width="1.5" stroke-dasharray="2.5 2"/>`;
}

// testSubject maps a test file to the file it tests: foo_test.go -> foo.go,
// foo.test.ts / foo.spec.tsx -> foo.ts / foo.tsx. "" if not a test file.
function testSubject(path) {
  const m = /^(.*)_test\.go$/.exec(path) || /^(.*)\.(?:test|spec)(\.[jt]sx?)$/.exec(path);
  if (!m) return "";
  return m[2] ? m[1] + m[2] : m[1] + ".go";
}

// resolveFile finds a changed file's tree node. place: "file" (the file
// itself), "subject" (the file a test file tests) or "missing" (not in the
// map; node is the deepest existing folder).
function resolveFile(repoNode, path) {
  const n = findNode(repoNode, path);
  if (n.k === "file" && n.p === path) return { node: n, place: "file" };
  const subj = testSubject(path);
  if (subj) {
    const s = findNode(repoNode, subj);
    if (s.k === "file" && s.p === subj) return { node: s, place: "subject" };
  }
  return { node: n, place: "missing" };
}

export const treemapHTML = () => `<div id="tm-root"><div class="progress">loading code map…</div></div>`;

// renderTreemap fills #tm-root once the tab's shell is on the page.
export async function renderTreemap() {
  const root = $("#tm-root");
  if (!root) return;
  const pr = S.result.pr;
  const scope = S.tm.scope === "all" ? "all" : pr.repo;
  if (!S.trees[scope]) {
    try { S.trees[scope] = (await api(`/api/codemap/tree?repo=${encodeURIComponent(scope)}`)).tree; }
    catch (e) { root.innerHTML = `<div class="empty">Code map unavailable: ${esc(e.message)}</div>`; return; }
    prepTree(S.trees[scope]);
    if (S.tab !== "map" || !$("#tm-root")) return;
  }
  const tree = S.trees[scope];
  const repoNode = scope === "all" ? (tree.c || []).find((c) => c.n === pr.repo) : tree;
  // zoom target
  let view = tree;
  for (const seg of S.tm.zoom) { const n = (view.c || []).find((c) => c.n === seg); if (!n) break; view = n; }
  const crumbs = [tree, ...S.tm.zoom.map((_, i) => { let n = tree; for (const seg of S.tm.zoom.slice(0, i + 1)) n = (n.c || []).find((c) => c.n === seg) || n; return n; })];

  // PR changes resolved to map nodes; those outside the zoomed view are only counted.
  const placed = [];
  let outside = 0;
  if (repoNode) {
    const inView = (n) => { for (let x = n; x; x = x._parent) if (x === view) return true; return false; };
    allUnits().forEach(({ u, f }, ui) => {
      const { node, place } = resolveFile(repoNode, f.status === "renamed" && f.old_path ? f.old_path : f.path);
      if (inView(node)) placed.push({ u, f, ui, node, place });
      else outside++;
    });
  }

  const shape = (b) => `<svg width="18" height="18">${dotMark(b, 9, 9)}</svg>`;
  const units = allUnits();
  const sortKey = {
    risk: (x) => -risk(x.u), impact: (x) => -(x.u.impact?.score ?? -1), likelihood: (x) => -(x.u.likelihood?.score ?? -1),
    bucket: (x) => -BRANK[x.u.decision.bucket], file: (x) => x.u.id,
  };
  const key = sortKey[S.tm.sort] || sortKey.risk;
  units.sort((a, b) => { const ka = key(a), kb = key(b); return ka < kb ? -1 : ka > kb ? 1 : a.u.id < b.u.id ? -1 : 1; });

  root.innerHTML = `
    <div class="tm-bar">
      <span class="seg"><button class="${S.tm.scope !== "all" ? "on" : ""}" data-act="tm-scope" data-scope="repo">${esc(pr.repo)}</button><button class="${S.tm.scope === "all" ? "on" : ""}" data-act="tm-scope" data-scope="all">all repos</button></span>
      <span class="seg" role="group" aria-label="color by">${Object.entries(MODES).map(([m, l]) => `<button class="${S.tm.mode === m ? "on" : ""}" data-act="tm-mode" data-mode="${m}">${l}</button>`).join("")}</span>
      <span class="tm-crumbs">${crumbs.map((n, i) => i === crumbs.length - 1 ? `<b>${esc(n.n)}</b>` : `<a data-act="tm-crumb" data-depth="${i}">${esc(n.n)}</a>`).join(" / ")}</span>
      <span class="spacer"></span>
      <span style="color:var(--muted)">area = lines of code · click an area to zoom${outside ? ` · ${outside} change${outside > 1 ? "s" : ""} outside this view` : ""}${repoNode ? "" : ` · ${esc(pr.repo)} is not in the code map`}</span>
    </div>
    <div class="tm-legend">
      ${legendHTML()}
      <span class="tm-shapes">PR changes: ${shape("human")} ${LABEL.human} ${shape("skim")} ${LABEL.skim} ${shape("aux")} ${LABEL.aux} ${shape("none")} ${LABEL.none} <svg width="22" height="22">${ghostRing(11, 11)}${dotMark("none", 11, 11)}</svg> file not in map (on its folder's header)</span>
    </div>
    <div class="tm-wrap"><svg role="img" aria-label="Treemap of ${esc(view.n)} colored by ${esc(MODES[S.tm.mode])}, with this PR's changes as dots"></svg><div class="tm-tip" hidden></div></div>
    <table class="tm-table"><thead><tr>
      ${[["file", "change"], ["bucket", "bucket"], ["risk", "risk"], ["impact", "impact"], ["likelihood", "likelihood"]].map(([k, l]) =>
        `<th data-act="tm-sort" data-sort="${k}" ${S.tm.sort === k ? `aria-sort="descending" class="on"` : ""}>${l}</th>`).join("")}<th>top likelihood factor</th><th>matched</th><th>used by repos</th><th>attention</th>
    </tr></thead><tbody>${units.map(({ u }) => `<tr data-act="tm-unit" data-unit="${esc(u.id)}">
      <td class="id">${esc(u.id)}</td><td><span class="pill ${u.decision.bucket}">${LABEL[u.decision.bucket]}</span></td>
      <td class="num">${Math.round(risk(u) / 100)}</td><td>${impactPill(u.impact)}</td><td>${likelihoodPill(u.likelihood)}</td>
      <td class="mut">${esc(u.likelihood?.factors?.[0]?.detail || "")}</td><td class="id">${esc(u.impact?.basis || u.impact?.matched || "")}</td>
      <td>${esc((u.impact?.dep_repos || []).join(", "))}</td><td>${attentionPill(u)}</td></tr>`).join("")}</tbody></table>`;

  // Size the map to the screen space left below the bar and legend, so the
  // whole map is visible without scrolling (the table follows below it).
  const W = Math.max(480, root.clientWidth || $("#main").clientWidth - 48);
  const wrap = root.querySelector(".tm-wrap"), tip = root.querySelector(".tm-tip"), svgEl = wrap.querySelector("svg");
  const svgTop = wrap.getBoundingClientRect().top + window.scrollY;
  const H = Math.max(320, Math.round(window.innerHeight - svgTop - 16));

  const cells = [];
  layoutTree(view, 0, 0, W, H, 0, cells);

  let svg = "";
  cells.forEach((c, i) => {
    const n = c.node, r = n._rect;
    if (r.w < 1 || r.h < 1) return;
    const fill = fillRGB(n);
    const zoomable = n.c?.length && n !== view;
    svg += `<rect class="cell ${zoomable ? "zoomable" : ""}" data-i="${i}" x="${r.x.toFixed(1)}" y="${r.y.toFixed(1)}" width="${Math.max(0, r.w - 0.5).toFixed(1)}" height="${Math.max(0, r.h - 0.5).toFixed(1)}" rx="2" fill="${rgb(fill)}" stroke="${isDark() ? "#0d1117" : "#ffffff"}" stroke-width="1"/>`;
    const maxChars = Math.floor((r.w - 8) / 6.2);
    const label = (t) => (t.length > maxChars ? t.slice(0, Math.max(0, maxChars - 1)) + "…" : t);
    if (n._open && n._head && maxChars >= 3) {
      svg += `<text class="head" x="${r.x + 4}" y="${r.y + 12}" fill="${inkFor(fill)}">${esc(label(n === view && n.k !== "repo" ? nodePath(n).join("/") || n.n : n.n))}</text>`;
    } else if (!n._open && r.h >= 15 && maxChars >= 3) {
      svg += `<text x="${r.x + 4}" y="${r.y + 12}" fill="${inkFor(fill)}">${esc(label(n.n))}</text>`;
    }
  });

  // PR changes as dots, placed in the drawn rect of their file at the unit's
  // line. Test files (never in the map) go on the file they test; anything
  // else the map lacks goes in its folder's header strip, marked "not in map",
  // so it doesn't sit on unrelated files.
  const dots = [];
  if (repoNode) {
    const groups = new Map();
    placed.forEach(({ u, f, ui, node, place }) => {
      let n = node;
      while (n && !n._rect) n = n._parent;
      // children not laid out (rect too small): climb to the drawn cell
      while (n._parent && n._parent._rect && !n._parent._open) n = n._parent;
      // a folder drawn with its children: keep missing files off them
      const ghost = place === "missing" && n._open;
      if (!groups.has(n)) groups.set(n, { in: [], ghost: [] });
      groups.get(n)[ghost ? "ghost" : "in"].push({ u, f, ui, place, host: node });
    });
    for (const [n, g] of groups) {
      const r = n._rect, top = r.y + (n._open ? n._head : 0) + 7, bottom = r.y + r.h - 7;
      g.in.sort((a, b) => (a.u.line || 0) - (b.u.line || 0));
      g.in.forEach((it, j) => {
        const cx = r.x + ((j + 1) * r.w) / (g.in.length + 1);
        // a test file's lines say nothing about where in its subject it lands
        const frac = n.k === "file" && n.s && it.place === "file" ? Math.min(1, Math.max(0, (it.u.line || 1) / n.s)) : 0.5;
        const cy = top + frac * Math.max(0, bottom - top);
        dots.push({ ...it, cx, cy });
      });
      // right-aligned in the header, clear of the folder label on the left
      const step = Math.min(16, Math.max(4, (r.w - 24) / Math.max(1, g.ghost.length)));
      const cy = n._head ? r.y + n._head / 2 : r.y + 7;
      g.ghost.forEach((it, j) => dots.push({ ...it, cx: r.x + r.w - 12 - j * step, cy, ghost: true }));
    }
  }
  dots.sort((a, b) => BRANK[a.u.decision.bucket] - BRANK[b.u.decision.bucket]);
  for (const d of dots) {
    svg += `<g class="dot" data-u="${d.ui}" data-act="tm-unit" data-unit="${esc(d.u.id)}"><circle cx="${d.cx}" cy="${d.cy}" r="11" fill="transparent"/>${d.ghost ? ghostRing(d.cx, d.cy) : ""}${dotMark(d.u.decision.bucket, d.cx, d.cy)}</g>`;
  }

  svgEl.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svgEl.setAttribute("height", H);
  svgEl.innerHTML = svg;

  const scaleXY = (e) => { const b = svgEl.getBoundingClientRect(); return { x: e.clientX - b.left, y: e.clientY - b.top }; };
  svgEl.addEventListener("mousemove", (e) => {
    const dot = e.target.closest(".dot"), cell = e.target.closest("rect.cell");
    let html = "";
    if (dot) html = unitTip(dots.find((d) => String(d.ui) === dot.dataset.u));
    else if (cell) html = areaTip(cells[+cell.dataset.i].node);
    if (!html) { tip.hidden = true; return; }
    tip.innerHTML = html;
    tip.hidden = false;
    const p = scaleXY(e), tw = tip.offsetWidth, th = tip.offsetHeight;
    tip.style.left = `${Math.min(p.x + 14, wrap.clientWidth - tw - 4)}px`;
    tip.style.top = `${p.y + 16 + th > wrap.clientHeight ? Math.max(0, p.y - th - 10) : p.y + 16}px`;
  });
  svgEl.addEventListener("mouseleave", () => (tip.hidden = true));
  svgEl.addEventListener("click", (e) => {
    if (e.target.closest(".dot")) return; // handled by data-act
    const cell = e.target.closest("rect.cell");
    if (!cell) return;
    const n = cells[+cell.dataset.i].node;
    if (!n.c?.length || n === view) return;
    S.tm.zoom = nodePath(n);
    renderTreemap();
  });
}

// levelOf mirrors the map's cut-offs for tooltips on tree nodes.
const levelOf = (v) => v >= 75 ? "critical" : v >= 55 ? "high" : v >= 35 ? "medium" : "low";

function areaTip(n) {
  const path = nodePath(n).join("/") || n.n;
  const hist = n.cm || n.fx || n.rv ? `${n.cm || 0} commit${n.cm === 1 ? "" : "s"} · ${n.fx || 0} fix${n.fx === 1 ? "" : "es"}${n.rv ? ` · ${n.rv} revert${n.rv > 1 ? "s" : ""}` : ""} · ${n.au || 0} author${n.au === 1 ? "" : "s"} in the last year` : "no commits in the last year";
  return `<div class="tt">${esc(path)}${n.k === "dir" ? "/" : ""}</div>
    <div class="row">${impactPill({ score: n.im, level: n.l || "low", rank: n.rk, rollback: n.rb, matched: n.k })}${likelihoodPill({ score: n.lk, level: n.ll || levelOf(n.lk) })}</div>
    <div class="mut">impact: ${esc(n.k)} · CodeRank ${Math.round(n.rk)}th pct · rollback ${n.rb}${n.t ? ` (${esc(n.t)})` : ""}${n.dr ? ` · used by ${n.dr} other repo${n.dr > 1 ? "s" : ""}` : ""}${n.s ? ` · ${n.s} lines` : ""}</div>
    <div class="mut">likelihood${n.k === "dir" ? " (75th-percentile file)" : ""}: ${hist}${n.cy ? ` · worst function complexity ${n.cy}, nesting ${n.ne || 0}` : ""}</div>`;
}

function unitTip(d) {
  if (!d) return "";
  const u = d.u, dz = u.impact, lk = u.likelihood;
  const rows = [`<div class="tt">${esc(u.id)}</div>`,
    `<div class="row"><span class="pill ${u.decision.bucket}">${LABEL[u.decision.bucket]}</span>${impactPill(dz)}${likelihoodPill(lk)}${attentionPill(u)}</div>`,
    `<div>${esc(headline(u))}</div>`];
  if (d.place === "subject") rows.push(`<div class="mut">test file, not in the code map · shown on <code>${esc(d.host.p)}</code></div>`);
  else if (d.place === "missing") rows.push(`<div class="mut">file not in the code map · shown on <code>${esc(d.host.p || d.host.n)}/</code></div>`);
  if (dz && dz.level !== "unknown") {
    rows.push(`<div class="mut">impact: matched ${esc(dz.matched)} <code>${esc(dz.basis || "")}</code> · CodeRank ${Math.round(dz.rank)}th pct · ${dz.callers} caller${dz.callers === 1 ? "" : "s"} · rollback ${dz.rollback}</div>`);
    if (dz.dep_repos?.length) rows.push(`<div>used by: ${dz.dep_repos.map((r) => `<code>${esc(r)}</code>`).join(", ")}</div>`);
    if (dz.tags?.length) rows.push(`<div class="row">${dz.tags.slice(0, 4).map((t) => `<span class="chip">${esc(t.id)} ${t.s}${t.via ? ` · via ${t.via}` : ""}</span>`).join("")}</div>`);
  } else if (dz) rows.push(`<div class="mut">${esc((dz.notes || []).join(" "))}</div>`);
  if (lk?.factors?.length) rows.push(`<div class="mut">likelihood: ${lk.factors.slice(0, 3).map((f) => `+${f.points} ${esc(f.detail)}`).join("<br>")}</div>`);
  if (u.issues?.length) rows.push(`<div>${u.issues.slice(0, 3).map((i) => `⚠ ${esc(i.severity)}: ${esc(i.title)}`).join("<br>")}</div>`);
  rows.push(`<div class="mut">click to open in Review</div>`);
  return rows.join("");
}

// legendHTML is a ramp for one score, or a 3x3 grid for both: impact grows
// to the right, likelihood grows up.
function legendHTML() {
  const ticks = `<span class="ticks">${[[0, "0"], [35, "medium"], [55, "high"], [75, "critical"], [100, "100"]].map(([v, l]) => `<span style="left:${v}%">${l}</span>`).join("")}</span>`;
  if (S.tm.mode !== "both") {
    const ramp = [0, 25, 35, 45, 55, 65, 75, 88, 100].map((v) => `${rgb(rampRGB(S.tm.mode, v))} ${v}%`).join(",");
    return `<span class="tm-scale">${esc(S.tm.mode)} <span><span class="ramp" style="display:block;background:linear-gradient(90deg,${ramp})"></span>${ticks}</span></span>`;
  }
  const steps = [15, 55, 90], sz = 16, gap = 2, x0 = 58, W = x0 + 3 * (sz + gap) + 4, H = 3 * (sz + gap) + 30;
  let g = "";
  steps.forEach((lk, row) => steps.forEach((im, col) => {
    g += `<rect x="${x0 + col * (sz + gap)}" y="${(2 - row) * (sz + gap)}" width="${sz}" height="${sz}" rx="2" fill="${rgb(bothRGB(im, lk))}"><title>impact ${["low", "high", "critical"][col]}, likelihood ${["low", "high", "critical"][row]}</title></rect>`;
  }));
  const ink = "currentColor";
  g += `<text x="${x0 - 6}" y="${sz - 3}" text-anchor="end" fill="${ink}">likely</text><text x="${x0 - 6}" y="${3 * (sz + gap) - 5}" text-anchor="end" fill="${ink}">unlikely</text>`;
  g += `<text x="${x0}" y="${3 * (sz + gap) + 12}" fill="${ink}">low</text><text x="${x0 + 3 * (sz + gap)}" y="${3 * (sz + gap) + 12}" text-anchor="end" fill="${ink}">high</text>`;
  g += `<text x="${x0 + 1.5 * (sz + gap)}" y="${3 * (sz + gap) + 25}" text-anchor="middle" fill="${ink}">impact →</text>`;
  return `<span class="tm-bivar"><svg width="${W}" height="${H}" role="img" aria-label="Legend: red is high impact, green is high likelihood, darkest is both">${g}</svg>
    <span class="tm-bivar-key"><span><i style="background:${rgb(bothRGB(90, 15))}"></i> high impact</span><span><i style="background:${rgb(bothRGB(15, 90))}"></i> likely to break</span><span><i style="background:${rgb(bothRGB(90, 90))}"></i> both</span></span></span>`;
}

export const actions = {
  "tm-scope": (el) => { S.tm.scope = el.dataset.scope; S.tm.zoom = []; },
  "tm-crumb": (el) => { S.tm.zoom = S.tm.zoom.slice(0, +el.dataset.depth); },
  "tm-sort": (el) => { S.tm.sort = el.dataset.sort; },
  "tm-mode": (el) => { S.tm.mode = el.dataset.mode; localStorage.setItem("pr-manager.tmmode", S.tm.mode); },
  "tm-unit": (el) => { jumpToUnit(el.dataset.unit); return false; },
};

// Re-lay out on resize and on a light/dark switch.
let resizeTimer;
window.addEventListener("resize", () => {
  if (S.tab !== "map") return;
  clearTimeout(resizeTimer);
  resizeTimer = setTimeout(renderTreemap, 150);
});
addEventListener("themechange", () => S.tab === "map" && renderTreemap());
