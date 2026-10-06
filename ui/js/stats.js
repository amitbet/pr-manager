// Settings → Statistics: the tokens every model call spent (GET /api/usage,
// see usage.go), over a range of days: totals, a column per day stacked by
// activity, the split by activity, by repository and change (a PR, or a
// local checkout's branch), by model and by kind of call. Every chart has
// its numbers in a table too, and hovering a mark shows its values.
//
// The activities keep one color each, in a fixed order (the dataviz
// reference palette's first six categorical slots, validated against this
// app's light and dark surfaces); text never wears them.
import { esc, api } from "./util.js";

const RANGES = [[7, "7 days"], [30, "30 days"], [90, "90 days"], [0, "All time"]];
let days = 30;
let report = null;
let pane = null;
const open = new Set(); // repositories unfolded into their changes
let dailyTable = false;

const fmt = (n) => {
  n = n || 0;
  if (n >= 1e9) return `${(n / 1e9).toFixed(n >= 1e10 ? 0 : 1)}B`;
  if (n >= 1e6) return `${(n / 1e6).toFixed(n >= 1e7 ? 0 : 1)}M`;
  if (n >= 1e4) return `${Math.round(n / 1e3)}K`;
  if (n >= 1e3) return `${(n / 1e3).toFixed(1)}K`;
  return String(n);
};
const full = (n) => (n || 0).toLocaleString();
const tokens = (s) => (s.in || 0) + (s.out || 0);
const dayLabel = (d) => new Date(`${d}T12:00:00`).toLocaleDateString([], { month: "short", day: "numeric" });

// showStats draws the statistics into el, loading them for the range.
export async function showStats(el) {
  pane = el;
  if (!el.dataset.wired) {
    el.dataset.wired = "1";
    el.addEventListener("click", onClick);
    el.addEventListener("pointerover", onHover);
    el.addEventListener("focusin", onHover);
    el.addEventListener("pointerleave", hideTip);
    el.addEventListener("focusout", hideTip);
    el.addEventListener("keydown", (e) => {
      const row = (e.key === "Enter" || e.key === " ") && e.target.closest?.('[data-st="repo"]');
      if (row) { e.preventDefault(); row.click(); }
    });
  }
  el.innerHTML = `${filtersHTML()}<p class="muted"><span class="spinner"></span> Loading…</p>`;
  try {
    report = await api(`/api/usage?days=${days}`);
  } catch (e) {
    el.innerHTML = `${filtersHTML()}<p class="err">Couldn't load the statistics: ${esc(e.message)}</p>`;
    return;
  }
  draw();
}

function draw() {
  const r = report;
  if (!r.totals.calls) {
    pane.innerHTML = `${filtersHTML()}<p class="muted">No model calls ${days ? `in the last ${days} days` : "yet"}. Tokens are counted from now on, for every analysis, chat, fix and translation.</p>`;
    return;
  }
  pane.innerHTML = `${filtersHTML()}
    ${tilesHTML(r)}
    <h5>Tokens by day <button type="button" class="linkbtn" data-st="daily-table">${dailyTable ? "show chart" : "show table"}</button></h5>
    ${legendHTML(r)}
    ${dailyTable ? dailyTableHTML(r) : `<div class="st-chart" data-st-chart="days">${daysSVG(r)}</div>`}
    <h5>By activity</h5>
    ${activityHTML(r)}
    <h5>By repository and change <span class="muted">· click a repository for its PRs and branches</span></h5>
    ${reposHTML(r)}
    <div class="st-two">
      <div><h5>By model</h5>${barsHTML(r.models.map((m) => ({ label: m.model, s: m })), "models")}</div>
      <div><h5>By kind of call</h5>${barsHTML(r.tools.map((m) => ({ label: m.model || "(none)", s: m })), "tools")}</div>
    </div>
    <div class="st-tip" role="tooltip" hidden></div>`;
}

const filtersHTML = () => `<div class="st-filters"><span class="seg" role="group" aria-label="Range">${RANGES.map(([d, l]) =>
  `<button type="button" data-st="range" data-days="${d}" class="${d === days ? "on" : ""}" aria-pressed="${d === days}">${l}</button>`).join("")}</span>
  <button type="button" class="linkbtn" data-st="reload">refresh</button></div>`;

function tilesHTML(r) {
  const t = r.totals;
  const from = r.from ? new Date(r.from).toLocaleDateString() : "";
  return `<div class="st-tiles">
    <div class="st-tile hero"><div class="st-label">Tokens</div><div class="st-value" title="${full(tokens(t))}">${fmt(tokens(t))}</div><div class="st-sub">${days ? `last ${days} days` : `since ${esc(from)}`}</div></div>
    <div class="st-tile"><div class="st-label">Input</div><div class="st-value" title="${full(t.in)}">${fmt(t.in)}</div><div class="st-sub">cached reads included</div></div>
    <div class="st-tile"><div class="st-label">Output</div><div class="st-value" title="${full(t.out)}">${fmt(t.out)}</div></div>
    <div class="st-tile"><div class="st-label">Model calls</div><div class="st-value">${full(t.calls)}</div></div>
  </div>`;
}

// cats are the activities that spent anything in the range, in their
// fixed order: a color follows its activity, whichever are shown.
const cats = (r) => r.categories.map((c, i) => ({ ...c, slot: i + 1 })).filter((c) => r.totals.by[c.id]);
const legendHTML = (r) => `<div class="st-legend">${cats(r).map((c) => `<span><i class="sw s${c.slot}"></i>${esc(c.label)}</span>`).join("")}</div>`;

// niceStep is a round tick step for a maximum of max.
function niceStep(max, ticks = 4) {
  const raw = max / ticks, p = 10 ** Math.floor(Math.log10(raw || 1));
  return [1, 2, 2.5, 5, 10].map((m) => m * p).find((s) => s >= raw) || p * 10;
}

function daysSVG(r) {
  const W = Math.max(320, (pane.clientWidth || 720) - 24), H = 200, L = 44, B = 22, T = 8;
  const n = r.days.length, slot = (W - L) / n, bw = Math.max(2, Math.min(24, slot * 0.7));
  const cs = cats(r);
  const max = Math.max(1, ...r.days.map(tokens));
  const step = niceStep(max), top = Math.ceil(max / step) * step;
  const y = (v) => T + (H - T - B) * (1 - v / top);
  let out = "";
  for (let v = 0; v <= top + 1e-9; v += step) out += `<line class="grid" x1="${L}" x2="${W}" y1="${y(v)}" y2="${y(v)}"/><text class="tick" x="${L - 6}" y="${y(v) + 4}" text-anchor="end">${fmt(v)}</text>`;
  const every = Math.ceil(n / 7);
  r.days.forEach((d, i) => {
    const x = L + slot * i + (slot - bw) / 2;
    let acc = 0;
    const segs = cs.map((c) => ({ c, v: d.by?.[c.id] || 0 })).filter((s) => s.v);
    segs.forEach((s, k) => {
      const y0 = y(acc), y1 = y(acc + s.v);
      acc += s.v;
      const h = Math.max(0, y0 - y1 - (k ? 2 : 0)); // the 2px surface gap
      if (h <= 0) return;
      const yt = y0 - (k ? 2 : 0) - h, rr = k === segs.length - 1 ? Math.min(4, h, bw / 2) : 0;
      out += rr ? `<path class="s${s.c.slot}" d="M${x},${yt + h}V${yt + rr}Q${x},${yt} ${x + rr},${yt}H${x + bw - rr}Q${x + bw},${yt} ${x + bw},${yt + rr}V${yt + h}Z"/>`
        : `<rect class="s${s.c.slot}" x="${x}" y="${yt}" width="${bw}" height="${h}"/>`;
    });
    if (i % every === 0 || i === n - 1) out += `<text class="tick" x="${x + bw / 2}" y="${H - 6}" text-anchor="middle">${esc(dayLabel(d.day))}</text>`;
    out += `<rect class="hit" x="${L + slot * i}" y="${T}" width="${slot}" height="${H - T - B}" tabindex="0" data-tip="day" data-i="${i}" aria-label="${esc(dayLabel(d.day))}: ${full(tokens(d))} tokens"/>`;
  });
  out += `<line class="axis" x1="${L}" x2="${W}" y1="${y(0)}" y2="${y(0)}"/>`;
  return `<svg width="${W}" height="${H}" viewBox="0 0 ${W} ${H}" role="img" aria-label="Tokens by day, stacked by activity">${out}</svg>`;
}

function dailyTableHTML(r) {
  const cs = cats(r);
  return `<div class="st-scroll"><table class="st-table"><thead><tr><th>Day</th>${cs.map((c) => `<th class="num">${esc(c.label)}</th>`).join("")}<th class="num">Total</th></tr></thead><tbody>
    ${[...r.days].reverse().filter((d) => tokens(d)).map((d) => `<tr><td>${esc(d.day)}</td>${cs.map((c) => `<td class="num">${full(d.by?.[c.id])}</td>`).join("")}<td class="num"><b>${full(tokens(d))}</b></td></tr>`).join("")}
  </tbody></table></div>`;
}

// stackHTML is a horizontal bar of s split by activity, as wide as its
// share of max.
function stackHTML(r, s, max) {
  const total = tokens(s);
  const segs = cats(r).map((c) => ({ c, v: s.by?.[c.id] || 0 })).filter((x) => x.v);
  const w = max ? (100 * total) / max : 0;
  return `<div class="st-stack" style="width:${w.toFixed(2)}%">${segs.map((x) => `<span class="s${x.c.slot}" style="flex:${x.v}"></span>`).join("")}</div>`;
}

function activityHTML(r) {
  const cs = cats(r).sort((a, b) => r.totals.by[b.id] - r.totals.by[a.id]);
  const max = Math.max(...cs.map((c) => r.totals.by[c.id]));
  const all = tokens(r.totals);
  return `<div class="st-bars">${cs.map((c) => {
    const v = r.totals.by[c.id];
    return `<div class="st-row" tabindex="0" data-tip="cat" data-id="${esc(c.id)}"><span class="st-name"><i class="sw s${c.slot}"></i>${esc(c.label)}</span>
      <span class="st-track"><span class="st-bar s${c.slot}" style="width:${(100 * v / max).toFixed(2)}%"></span></span>
      <span class="st-num">${fmt(v)} <span class="muted">${Math.round(100 * v / all)}%</span></span></div>`;
  }).join("")}</div>`;
}

function reposHTML(r) {
  const max = Math.max(...r.repos.map((x) => tokens(x)));
  const row = (cls, attrs, name, sub, s, last) => `<tr class="${cls}" ${attrs} tabindex="0">
      <td class="st-name">${name}${sub ? ` <span class="muted">${esc(sub)}</span>` : ""}</td>
      <td class="st-cell">${stackHTML(r, s, max)}</td>
      <td class="num">${fmt(tokens(s))}</td><td class="num muted">${full(s.calls)}</td><td class="muted">${last || ""}</td></tr>`;
  return `<div class="st-scroll"><table class="st-table st-repos"><thead><tr><th>Repository / change</th><th>Split by activity</th><th class="num">Tokens</th><th class="num">Calls</th><th>Last</th></tr></thead><tbody>
    ${r.repos.map((x, i) => {
      const on = open.has(x.repo);
      const head = row("st-repo", `data-st="repo" data-repo="${esc(x.repo)}" data-tip="repo" data-i="${i}" aria-expanded="${on}"`, `${on ? "▾" : "▸"} <b>${esc(x.repo)}</b>`, `${x.items.length} change${x.items.length === 1 ? "" : "s"}`, x, "");
      const items = on ? x.items.map((it, k) => row("st-item", `data-tip="item" data-i="${i}" data-k="${k}"`, esc(it.label), it.sub, it, esc(new Date(it.last).toLocaleDateString()))).join("") : "";
      return head + items;
    }).join("")}</tbody></table></div>`;
}

function barsHTML(rows, kind) {
  if (!rows.length) return `<p class="muted">None</p>`;
  const max = Math.max(...rows.map((x) => tokens(x.s)));
  return `<div class="st-bars">${rows.slice(0, 12).map((x, i) => `<div class="st-row" tabindex="0" data-tip="${kind}" data-i="${i}"><span class="st-name mono" title="${esc(x.label)}">${esc(x.label)}</span>
    <span class="st-track"><span class="st-bar s1" style="width:${(100 * tokens(x.s) / max).toFixed(2)}%"></span></span>
    <span class="st-num">${fmt(tokens(x.s))}</span></div>`).join("")}</div>`;
}

// The tooltip: the values lead, every activity of the mark, with a line
// key in its color.
function tipRows(s) {
  const t = tokens(s);
  return `${cats(report).filter((c) => s.by?.[c.id]).map((c) => `<div class="st-tip-row"><i class="key s${c.slot}"></i><b>${full(s.by[c.id])}</b> <span>${esc(c.label)}</span></div>`).join("")}
    <div class="st-tip-foot"><b>${full(t)}</b> tokens · ${full(s.in)} in · ${full(s.out)} out · ${full(s.calls)} call${s.calls === 1 ? "" : "s"}</div>`;
}

function onHover(e) {
  const el = e.target.closest?.("[data-tip]");
  const tip = pane.querySelector(".st-tip");
  if (!el || !tip || !report) return;
  const r = report, i = +el.dataset.i;
  let head = "", body = "";
  switch (el.dataset.tip) {
    case "day": { const d = r.days[i]; head = dayLabel(d.day); body = tipRows(d); break; }
    case "repo": { const x = r.repos[i]; head = x.repo; body = tipRows(x); break; }
    case "item": { const x = r.repos[i].items[+el.dataset.k]; head = `${x.label}${x.sub ? ` · ${x.sub}` : ""}`; body = tipRows(x); break; }
    case "models": case "tools": { const x = r[el.dataset.tip][i]; head = x.model || "(none)"; body = tipRows(x); break; }
    case "cat": { const c = r.categories.find((x) => x.id === el.dataset.id); head = c.label; body = `<div class="st-tip-foot"><b>${full(r.totals.by[c.id])}</b> tokens</div>`; break; }
    default: return;
  }
  tip.innerHTML = `<div class="st-tip-head">${esc(head)}</div>${body}`;
  tip.hidden = false;
  const pr = pane.getBoundingClientRect(), er = el.getBoundingClientRect();
  const left = Math.min(er.left - pr.left + er.width / 2 + 12, pane.clientWidth - tip.offsetWidth - 4);
  tip.style.left = `${Math.max(0, left)}px`;
  tip.style.top = `${er.top - pr.top + pane.scrollTop + Math.min(er.height, 40)}px`;
  el.classList.add("hover");
  el.addEventListener("pointerleave", () => el.classList.remove("hover"), { once: true });
}
function hideTip() { const tip = pane?.querySelector(".st-tip"); if (tip) tip.hidden = true; }

function onClick(e) {
  const el = e.target.closest("[data-st]");
  if (!el) return;
  switch (el.dataset.st) {
    case "range": days = +el.dataset.days; showStats(pane); break;
    case "reload": showStats(pane); break;
    case "daily-table": dailyTable = !dailyTable; draw(); break;
    case "repo": open.has(el.dataset.repo) ? open.delete(el.dataset.repo) : open.add(el.dataset.repo); draw(); break;
  }
}
