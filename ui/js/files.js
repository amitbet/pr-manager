// Files mode of the Review tab: the PR's directory tree, each changed file
// colored by its most important unit's bucket. The open file shows whole,
// each unit's review beside its code, as in a grouped walkthrough step.
import { $, esc, BUCKETS, LABEL } from "./util.js";
import { S, render, fileOfUnit, focusTop } from "./state.js";
import { SEV_RANK, SEV_CLASS } from "./scores.js";
import { canExpand, fullyExpanded, expandAllButton, expandWhole } from "./diff.js";
import { unitSegmentsHTML, noteHTML, markReviewed, leadUnit, scrollToSeg } from "./walkthrough.js";

// bucketOf is a file's most important bucket: BUCKETS runs from human
// review down to no review.
const bucketOf = (f) => BUCKETS[Math.min(...f.units.map((u) => BUCKETS.indexOf(u.decision.bucket)))];
const changed = () => S.result.files.filter((f) => f.units?.length);
const fileDone = (f) => f.units.every((u) => S.wz.done.has(u.id));
const liveIssues = (f) => f.units.flatMap((u) => (u.issues || []).filter((i) => !i.dismissed && !i.same_as));

// tree is the changed files by directory. A directory holding only one
// directory is joined to it ("a/b"), as GitHub shows it.
function tree(files) {
  const root = { name: "", path: "", dirs: new Map(), files: [] };
  for (const f of files) {
    let d = root;
    for (const p of f.path.split("/").slice(0, -1)) {
      if (!d.dirs.has(p)) d.dirs.set(p, { name: p, path: d.path ? `${d.path}/${p}` : p, dirs: new Map(), files: [] });
      d = d.dirs.get(p);
    }
    d.files.push(f);
  }
  const join = (d) => {
    for (const [k, c] of d.dirs) {
      let x = c;
      while (x.dirs.size === 1 && !x.files.length) {
        const only = [...x.dirs.values()][0];
        x = { ...only, name: `${x.name}/${only.name}` };
      }
      d.dirs.set(k, join(x));
    }
    d.bucket = BUCKETS[Math.min(...[...d.files.map(bucketOf), ...[...d.dirs.values()].map((c) => c.bucket)].map((b) => BUCKETS.indexOf(b)))];
    return d;
  };
  return join(root);
}

const sortedDirs = (d) => [...d.dirs.values()].sort((a, b) => a.name.localeCompare(b.name));
const sortedFiles = (d) => [...d.files].sort((a, b) => a.path.localeCompare(b.path));

// order is the files in tree order, for the default file and Previous / Next.
function order(d, out = []) {
  for (const c of sortedDirs(d)) order(c, out);
  out.push(...sortedFiles(d));
  return out;
}

// selected is the open file: the one picked, else the first in tree order
// of the most important bucket.
function selected(files) {
  const f = S.fv.path && files.find((x) => x.path === S.fv.path);
  if (f) return f;
  const all = order(tree(files));
  const best = Math.min(...all.map((x) => BUCKETS.indexOf(bucketOf(x))));
  return all.find((x) => BUCKETS.indexOf(bucketOf(x)) === best);
}

function fileRowHTML(f, cur, depth) {
  const b = bucketOf(f), n = liveIssues(f).length;
  const worst = n ? liveIssues(f).sort((a, x) => (SEV_RANK[a.severity] ?? 9) - (SEV_RANK[x.severity] ?? 9))[0].severity : "";
  const name = f.path.slice(f.path.lastIndexOf("/") + 1);
  const title = `${f.old_path && f.old_path !== f.path ? `${f.old_path} → ` : ""}${f.path}\n${LABEL[b]} · ${f.units.length} change${f.units.length > 1 ? "s" : ""} · ${f.status}`;
  return `<button class="fv-file ${b} ${f === cur ? "cur" : ""} ${f.status === "deleted" ? "deleted" : ""}" style="--d:${depth}" data-act="fv-open" data-path="${esc(f.path)}" title="${esc(title)}">
    <span class="fv-mark" aria-hidden="true"></span><span class="fv-name">${esc(name)}</span>
    ${n ? `<span class="dz ${SEV_CLASS[worst] || "high"}">${n}</span>` : ""}${fileDone(f) ? `<span class="fv-done" title="reviewed">✓</span>` : ""}</button>`;
}

function dirHTML(d, cur, depth) {
  const kids = sortedDirs(d).map((c) => {
    const open = !S.fv.closed.has(c.path);
    return `<button class="fv-dir" style="--d:${depth}" data-act="fv-dir" data-path="${esc(c.path)}" aria-expanded="${open}">
        <span class="car">${open ? "▾" : "▸"}</span><span class="fv-name">${esc(c.name)}</span>${open ? "" : `<span class="fv-dot ${c.bucket}" title="${esc(LABEL[c.bucket])}"></span>`}</button>
      ${open ? dirHTML(c, cur, depth + 1) : ""}`;
  }).join("");
  return kids + sortedFiles(d).map((f) => fileRowHTML(f, cur, depth)).join("");
}

function legendHTML(files) {
  const c = {};
  files.forEach((f) => c[bucketOf(f)] = (c[bucketOf(f)] || 0) + 1);
  return `<div class="fv-legend">${BUCKETS.filter((b) => c[b]).map((b) => `<span class="fv-file ${b}" title="files whose most important change is ${esc(LABEL[b])}"><span class="fv-mark"></span>${c[b]} ${esc(LABEL[b])}</span>`).join("")}</div>`;
}

// fileHTML is the open file: the whole file once it has loaded, each
// unit's review beside its code. Units with no lines to show (a binary or
// renamed file) list their reviews above.
function fileHTML(f, all) {
  const b = bucketOf(f), units = f.units, i = all.indexOf(f);
  const whole = fullyExpanded(f), done = fileDone(f);
  const segs = unitSegmentsHTML(f, units, b, units.length === 1);
  const bare = units.filter((u) => !(u.hunks || []).length).map((u) => `<div class="wz-seg"><div class="wz-seg-note">${noteHTML(f, u, new Set(), b, units.length === 1)}</div><div class="wz-seg-code"></div></div>`).join("");
  const fd = S.drafts.filter((d) => d.path === f.path).length;
  const state = whole ? "whole file" : canExpand(f) ? "changed lines only" : "";
  return `<div class="wz-card fv-card ${b}">
    <section class="wz-code">
      <div class="wz-codebar">
        <button class="details-btn" data-act="fv-step" data-d="-1" ${i <= 0 ? "disabled" : ""} title="Previous file">←</button>
        <button class="details-btn" data-act="fv-step" data-d="1" ${i >= all.length - 1 ? "disabled" : ""} title="Next file">→</button>
        <span class="pill ${b}">${LABEL[b]}</span>
        <span class="path">${f.old_path && f.old_path !== f.path ? esc(f.old_path) + " → " : ""}${esc(f.path)}</span>
        <span>${esc(f.status)}${f.binary ? ", binary" : ""} · ${units.length} change${units.length > 1 ? "s" : ""}${state ? ` · ${state}` : ""}</span>
        ${expandAllButton(f, "wz-expand-all")}
        <span class="spacer"></span>${fd ? `<span class="pill draft">${fd} comment${fd > 1 ? "s" : ""}</span>` : ""}
        <label class="wz-reviewed ${done ? "on" : ""}"><input type="checkbox" data-act="fv-done" ${done ? "checked" : ""}> Reviewed</label>
      </div>
      ${bare}${segs}
    </section></div>`;
}

// shownFile is the path of the open file, for the chat agent.
export const shownFile = () => { const files = changed(); return files.length ? selected(files)?.path || "" : ""; };

export function filesHTML() {
  const files = changed();
  if (!files.length) return `<div class="empty">No changed files.</div>`;
  const cur = selected(files), all = order(tree(files));
  const done = files.filter(fileDone).length;
  return `<div class="wz-top"><span><b>${done}</b> of <b>${files.length}</b> files reviewed</span><span class="spacer"></span>
      <span class="seg"><button class="${S.wz.view === "split" ? "on" : ""}" data-act="wz-view" data-v="split">Split</button><button class="${S.wz.view === "unified" ? "on" : ""}" data-act="wz-view" data-v="unified">Unified</button></span></div>
    <div class="fv">
      <nav class="fv-tree" aria-label="Changed files">${legendHTML(files)}${dirHTML(tree(files), cur, 0)}</nav>
      <div class="fv-main">${fileHTML(cur, all)}</div>
    </div>`;
}

// mountFiles loads the open file whole, once per file, so collapsing it
// with the file bar's button sticks.
export function mountFiles() {
  const files = changed();
  if (!files.length) return;
  const f = selected(files), r = S.result;
  if (S.fv.tried.has(f.path) || !canExpand(f) || fullyExpanded(f)) return;
  S.fv.tried.add(f.path);
  S.fv.loading = f.path;
  const done = () => { if (S.fv.loading === f.path) S.fv.loading = null; if (S.result === r) render(); };
  expandWhole(f).then(done, done);
}

// whenLoaded runs fn now, and again once file f has loaded whole, which
// moves its units down.
function whenLoaded(f, fn) {
  fn();
  if (S.fv.loading !== f.path) return;
  const wait = setInterval(() => { if (S.fv.loading !== f.path) { clearInterval(wait); fn(); } }, 50);
  setTimeout(() => clearInterval(wait), 10000);
}

// open shows file path, back at the top of the file when scrolled past it,
// or, with Settings → Review view → focus on, at its highest-ranked change.
function open(path) {
  S.fv.path = path;
  S.composer = null;
  render();
  const f = S.result.files.find((x) => x.path === path);
  // The whole file shows, so even a first change can be far down.
  if (focusTop() && f?.units?.length) {
    const id = leadUnit(f.units).id;
    whenLoaded(f, () => scrollToSeg(id));
    return;
  }
  const top = $(".fv-main")?.getBoundingClientRect().top;
  if (top < 54) window.scrollTo({ top: top + window.scrollY - 54 });
}

// showFileUnit opens unit id's file with its review open and scrolls to it.
export function showFileUnit(id) {
  const f = fileOfUnit(id);
  if (!f) return false;
  S.wz.noteOpen.set(id, true);
  S.fv.path = f.path;
  render();
  whenLoaded(f, () => {
    const el = document.querySelector(`.wz-note[data-note="${CSS.escape(id)}"]`);
    if (el) { el.closest(".wz-seg").scrollIntoView({ block: "start" }); el.classList.add("flash"); }
  });
  return true;
}

export const actions = {
  "fv-open": (el) => { open(el.dataset.path); return false; },
  "fv-dir": (el) => { const p = el.dataset.path; S.fv.closed.has(p) ? S.fv.closed.delete(p) : S.fv.closed.add(p); },
  "fv-step": (el) => {
    const all = order(tree(changed())), i = all.indexOf(selected(changed())) + +el.dataset.d;
    if (all[i]) open(all[i].path);
    return false;
  },
  "fv-done": () => {
    const f = selected(changed());
    markReviewed(f.units.map((u) => u.id), !fileDone(f));
  },
};
