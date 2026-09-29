// Walkthrough tab: one unit per step, most important first, with the
// explanation beside the code.
import { trText, trDir } from "./entext.js";
import { $, esc, LABEL, headline } from "./util.js";
import { S, render, allUnits, fileByPath } from "./state.js";
import { SEV_CLASS, SEV_RANK, issueCapChip, issueScenarioHTML, risk, impactPill, likelihoodPill, attentionPill, decisionChips, scoresHTML, classificationHTML, movesHTML } from "./scores.js";
import { unitRows, fileRows, fullyExpanded, expandAllButton, diffTable, actions as diffActions } from "./diff.js";
import { issueDraftButton } from "./comments.js";
import { issueFixButton } from "./fix.js";
import { threadsHTML, raisedBy, raisedChip } from "./threads.js";
import { lintHTML } from "./lint.js";
import { openPanel } from "./panel.js";
import { overviewHTML, hasOverview } from "./overview.js";

// Steps are ordered by bucket, then score, then review attention, then risk
// (impact times likelihood), then file order. "no review" units only on request.
const BRANK = { human: 0, skim: 1, none: 2 };

function steps() {
  return allUnits()
    .map((x, i) => ({ ...x, i }))
    .filter(({ u }) => S.wz.all || u.decision.bucket !== "none")
    .sort((a, b) => BRANK[a.u.decision.bucket] - BRANK[b.u.decision.bucket]
      || (b.u.score?.total ?? 0) - (a.u.score?.total ?? 0)
      || (b.u.attention || 0) - (a.u.attention || 0)
      || risk(b.u) - risk(a.u)
      || a.i - b.i);
}

// Progress is kept per result, so a reload resumes where the reviewer left
// off. A walkthrough not started yet opens on the overview.
const storeKey = () => `pr-manager.walk.${S.result.key}`;
export function loadProgress() {
  const saved = JSON.parse(localStorage.getItem(storeKey()) || "{}");
  Object.assign(S.wz, { cur: saved.cur || null, done: new Set(saved.done || []), finished: false });
  S.wz.intro = !S.wz.cur && !S.wz.done.size;
}
// onIntro is whether the overview is the page shown.
const onIntro = () => S.wz.intro && hasOverview(S.result);
const save = () => localStorage.setItem(storeKey(), JSON.stringify({ cur: S.wz.cur, done: [...S.wz.done] }));

// current is the index of the shown step: the saved one, else the first
// unreviewed one.
function current(st) {
  const i = st.findIndex((s) => s.u.id === S.wz.cur);
  if (i >= 0) return i;
  const open = st.findIndex((s) => !S.wz.done.has(s.u.id));
  return open >= 0 ? open : 0;
}

// HEADER_H is the sticky page header.
const HEADER_H = 54;

// showStep opens the walkthrough on unit id, including no-review units
// if that's what it is; false if it isn't a step even then.
export function showStep(id) {
  let i = steps().findIndex((s) => s.u.id === id);
  if (i < 0 && !S.wz.all) {
    S.wz.all = true;
    i = steps().findIndex((s) => s.u.id === id);
  }
  if (i < 0) return false;
  go(i);
  return true;
}

// go shows step i. When the page is scrolled past the card, it scrolls back
// to the card's top, just under the pinned step dots, so the new step is
// read from its start.
function go(i) {
  const st = steps();
  if (!st.length) return;
  S.wz.cur = st[Math.max(0, Math.min(st.length - 1, i))].u.id;
  S.wz.finished = S.wz.intro = false;
  S.composer = null;
  save();
  render();
  const card = $(".wz-card");
  if (!card) return;
  const top = card.getBoundingClientRect().top + card.clientTop; // inside the colored top border
  const pinned = HEADER_H + ($(".wz-steps")?.offsetHeight || 0);
  if (top < pinned) window.scrollTo({ top: top + window.scrollY - pinned });
}
// step moves d steps; back from the first step is the overview.
function step(d) {
  const i = current(steps()) + d;
  if (i < 0 && hasOverview(S.result)) { showIntro(); return; }
  go(i);
}
function showIntro() {
  S.wz.intro = true;
  S.wz.finished = false;
  S.composer = null;
  render();
  window.scrollTo({ top: 0 });
}

function toggleReviewed() {
  const st = steps();
  if (!st.length) return;
  const id = st[current(st)].u.id;
  S.wz.done.has(id) ? S.wz.done.delete(id) : S.wz.done.add(id);
  save();
  render();
}

// markAndNext marks the current step reviewed and moves to the next step.
// After the last step it shows the finish screen when everything is
// reviewed, else the first step still open.
function markAndNext() {
  const st = steps();
  if (!st.length) return;
  const i = current(st);
  S.wz.done.add(st[i].u.id);
  if (i < st.length - 1) { go(i + 1); return; }
  const open = st.findIndex((s) => !S.wz.done.has(s.u.id));
  if (open >= 0) { go(open); return; }
  S.wz.finished = true;
  save();
  render();
  window.scrollTo({ top: 0 });
}

// Dismissed issues are left out here: the walkthrough is the reading
// order, and a claim a person rejected is not part of it. It stays on the
// Issues tab, where it can be restored.
const worstFirst = (issues) => issues.filter((i) => !i.dismissed).sort((a, b) => (SEV_RANK[a.severity] ?? 9) - (SEV_RANK[b.severity] ?? 9));

function rankWhy(u) {
  const p = [LABEL[u.decision.bucket]];
  const live = worstFirst(u.issues || []);
  if (live.length) p.push(`${live.length} issue${live.length > 1 ? "s" : ""}, worst ${live[0].severity}`);
  if (u.reviewed) p.push(`attention ${u.attention}`);
  if (u.impact && u.impact.level !== "unknown") p.push(`impact ${u.impact.score}`);
  if (u.likelihood) p.push(`likelihood ${u.likelihood.score}${u.likelihood.factors?.length ? ` (${u.likelihood.factors[0].detail})` : ""}`);
  return p.join(" · ");
}

// issueCard is one review issue; shown is the set of new-file lines in the
// diff, so "show line" only appears when there's a line to scroll to.
function issueCard(f, u, is, shown) {
  const i = u.issues.indexOf(is), sev = SEV_CLASS[is.severity] || "high";
  const acts = [];
  if (is.line && shown.has(is.line)) acts.push(`<button class="linkbtn" data-act="wz-line" data-line="${is.line}">show line ${is.line}</button>`);
  else if (is.line) acts.push(`<span class="chip">line ${is.line}</span>`);
  const dup = raisedBy(u, i);
  acts.push(dup ? raisedChip(dup) : issueDraftButton(f, u, i));
  acts.push(issueFixButton(u, i, "linkbtn"));
  const a = acts.join("");
  return `<div class="wz-issue ${sev}"><div class="it"><span class="dz ${sev}">${esc(is.severity)}</span><span class="tr" ${trDir(u, `issues.${i}.title`, is.title)}>${trText(u, `issues.${i}.title`, is.title)}${issueCapChip(is)}</span></div>
    ${is.detail ? `<div class="idt tr" ${trDir(u, `issues.${i}.detail`, is.detail)}>${trText(u, `issues.${i}.detail`, is.detail)}</div>` : ""}${issueScenarioHTML(is, "idt", u, i)}${a ? `<div class="ia">${a}</div>` : ""}</div>`;
}

function explainHTML(f, u, shown) {
  const d = u.decision, parts = [];
  const live = worstFirst(u.issues || []);
  if (live.length) {
    parts.push(`<div class="wz-sec"><h4>Issues found in review</h4>${live.map((is) => issueCard(f, u, is, shown)).join("")}</div>`);
  } else if (u.reviewed) {
    parts.push(`<div class="wz-sec"><h4>Review</h4><span class="wz-clean">✓ No issues found</span></div>`);
  }
  parts.push(lintHTML(u, "wz"));
  parts.push(threadsHTML(u, "wz"));
  if (u.summary) parts.push(`<div class="wz-sec"><h4>${d.bucket === "human" ? "Review notes" : "Summary"}</h4><p class="tr" ${trDir(u, "summary", u.summary)}>${trText(u, "summary", u.summary)}</p></div>`);
  if (u.focus?.length) parts.push(`<div class="wz-sec"><h4>What to check</h4><ul class="wz-check">${u.focus.map((x, j) => `<li class="tr" ${trDir(u, `focus.${j}`, x)}>${trText(u, `focus.${j}`, x)}</li>`).join("")}</ul></div>`);
  parts.push(`<div class="wz-sec"><h4>Why it's here</h4><p><b>${esc(rankWhy(u))}</b></p>${movesHTML(u)}${d.reason ? `<p><span class="lbl">Classifier</span><span class="tr" ${trDir(u, "reason", d.reason)}>${trText(u, "reason", d.reason)}</span></p>` : ""}</div>`);
  parts.push(`<details class="wz-sec wz-more"><summary>Impact, likelihood and classification</summary>${scoresHTML(u)}${classificationHTML(d)}</details>`);
  return parts.join("");
}

function progressHTML(st, i, toggle) {
  const done = st.filter((s) => S.wz.done.has(s.u.id)).length;
  const intro = onIntro();
  const ovDot = hasOverview(S.result) ? `<button class="wz-dot ov-dot ${intro ? "cur" : ""}" data-act="wz-intro" title="Overview of the PR">Overview</button>` : "";
  const dots = ovDot + st.map((s, j) => {
    const ok = S.wz.done.has(s.u.id);
    return `<button class="wz-dot ${s.u.decision.bucket} ${ok ? "done" : ""} ${j === i && !S.wz.finished && !intro ? "cur" : ""}"
      data-act="wz-go" data-i="${j}" title="${esc(`${j + 1}. ${s.u.id}\n${headline(s.u)}`)}">${ok ? "✓" : j + 1}</button>`;
  }).join("");
  return `
    <div class="wz-top"><span><b>${done}</b> of <b>${st.length}</b> reviewed</span><span class="spacer"></span>${toggle}
      <span class="seg"><button class="${S.wz.view === "split" ? "on" : ""}" data-act="wz-view" data-v="split">Split</button><button class="${S.wz.view === "unified" ? "on" : ""}" data-act="wz-view" data-v="unified">Unified</button></span></div>
    <div class="wz-prog"><div style="width:${(100 * done) / st.length}%"></div></div>
    <div class="wz-steps">${dots}</div>`;
}

function finishHTML(st) {
  const nd = S.drafts.length;
  return `<div class="wz-card none"><div class="wz-finish">
    <h2>All ${st.length} steps reviewed</h2>
    <p>${nd ? `${nd} pending comment${nd > 1 ? "s" : ""} ready to submit.` : "No pending comments."}</p>
    <div class="actions"><button data-act="wz-go" data-i="0">Back to step 1</button><button data-act="wz-reset">Start over</button>
      <button class="primary" data-act="wz-submit">Submit review…</button></div></div></div>`;
}

// introHTML is the overview page before step 1.
function introHTML(st, i) {
  const started = S.wz.cur || S.wz.done.size;
  return `
    <div class="wz-card none"><div class="wz-intro">${overviewHTML(S.result, false)}</div>
      <div class="wz-nav">
        <span class="keys"><kbd>→</kbd> ${started ? "back to the steps" : "start"}</span>
        <span class="spacer"></span>
        <button class="primary" data-act="wz-start">${started ? `Back to step ${i + 1} →` : `Start walkthrough: ${st.length} step${st.length > 1 ? "s" : ""} →`}</button>
      </div>
    </div>`;
}

function cardHTML(st, i) {
  const { u, f } = st[i];
  const b = u.decision.bucket;
  // Expanded, the whole file shows, with other units' changes dimmed.
  const whole = fullyExpanded(f);
  const rows = whole ? fileRows(f, u) : unitRows(f, u);
  const issueLines = new Set((u.issues || []).map((x) => x.line).filter(Boolean));
  const shown = new Set();
  rows.forEach((r) => {
    if (r.t === "del" || !r.n || r.other) return;
    shown.add(r.n);
    if (issueLines.has(r.n)) r.iss = true;
  });
  const isDone = S.wz.done.has(u.id);
  const fd = S.drafts.filter((d) => d.path === f.path).length;
  return `
    <div class="wz-card ${b}">
      <div class="wz-head">
        <div class="wz-where"><span class="pill ${b}">${LABEL[b]}</span><b>Step ${i + 1} of ${st.length}</b> ·
          <span class="path">${f.old_path && f.old_path !== f.path ? esc(f.old_path) + " → " : ""}${esc(f.path)}${u.line ? `:${u.line}` : ""}</span>
          ${u.symbol ? `<span class="sym">${esc(u.symbol)}</span>` : ""}<span class="status chip">${esc(f.status)}</span></div>
        <div class="wz-headline tr" ${trDir(u, "headline", headline(u))}>${trText(u, "headline", headline(u))}</div>
        <div class="row">${impactPill(u.impact)}${likelihoodPill(u.likelihood)}${attentionPill(u)}${decisionChips(u)}</div>
      </div>
      <div class="wz-body">
        <section class="wz-explain">${explainHTML(f, u, shown)}</section>
        <section class="wz-code">
          <div class="wz-codebar"><b>Step ${i + 1} of ${st.length}</b><span>${esc(u.id)}</span>${expandAllButton(f, "wz-expand-all")}${whole ? `<span>whole file · other changes dimmed</span>` : ""}<span class="spacer"></span>${fd ? `<span class="pill draft">${fd} comment${fd > 1 ? "s" : ""} in this file</span>` : ""}<span>hover a line and click + to comment</span></div>
          ${diffTable(f, rows, S.wz.view)}
        </section>
      </div>
      <div class="wz-nav">
        <button data-act="wz-prev" ${i === 0 && !hasOverview(S.result) ? "disabled" : ""}>← ${i === 0 ? "Overview" : "Previous"}</button>
        <button data-act="wz-next" ${i === st.length - 1 ? "disabled" : ""}>Next →</button>
        <span class="keys"><kbd>←</kbd> <kbd>→</kbd> move · <kbd>x</kbd> toggle reviewed · <kbd>r</kbd> reviewed and next</span>
        <span class="spacer"></span>
        <label class="wz-reviewed ${isDone ? "on" : ""}"><input type="checkbox" data-act="wz-toggle" ${isDone ? "checked" : ""}> Reviewed</label>
        <button class="primary" data-act="wz-mark">✓ Reviewed, next →</button>
      </div>
    </div>`;
}

export function walkHTML() {
  const st = steps();
  const nNone = allUnits().filter(({ u }) => u.decision.bucket === "none").length;
  const toggle = nNone ? `<label><input type="checkbox" data-act="wz-all" ${S.wz.all ? "checked" : ""}> include ${nNone} no-review unit${nNone > 1 ? "s" : ""}</label>` : "";
  if (!st.length) return `<div class="wz-top">${toggle}</div>${overviewHTML(S.result, true)}<div class="empty">Nothing in this PR needs review.</div>`;
  const i = current(st);
  return progressHTML(st, i, toggle) + (onIntro() ? introHTML(st, i) : S.wz.finished ? finishHTML(st) : cardHTML(st, i));
}

export const actions = {
  // expand-all, then bring this step's change back into view.
  "wz-expand-all": async (el) => {
    const opening = !fullyExpanded(fileByPath(el.dataset.path));
    if ((await diffActions["expand-all"](el)) === false) return false;
    if (opening) setTimeout(() => document.querySelector(".wz-code tr.focus-start")?.scrollIntoView({ block: "center" }));
  },
  "wz-go": (el) => { go(+el.dataset.i); return false; },
  "wz-intro": () => { showIntro(); return false; },
  "wz-start": () => { go(current(steps())); return false; },
  "wz-prev": () => { step(-1); return false; },
  "wz-next": () => { step(1); return false; },
  "wz-mark": () => { markAndNext(); return false; },
  "wz-toggle": () => { toggleReviewed(); return false; },
  "wz-reset": () => { S.wz.done.clear(); S.wz.cur = null; go(0); return false; },
  "wz-all": (el) => { S.wz.all = el.checked; },
  "wz-view": (el) => { S.wz.view = el.dataset.v; localStorage.setItem("pr-manager.wzview", S.wz.view); },
  "wz-submit": () => { openPanel(); return false; },
  "wz-line": (el) => {
    const row = document.querySelector(`.wz-code tr[data-iss="${+el.dataset.line}"]`);
    if (row) { row.scrollIntoView({ block: "center", behavior: "smooth" }); row.classList.add("flash"); }
    return false;
  },
};

export function onKeydown(e) {
  if (S.tab !== "review" || S.mode !== "walk" || !S.result || e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.target.closest?.("input, textarea, select")) return;
  if (onIntro()) {
    if (e.key !== "ArrowRight" && e.key !== "j" && e.key !== "Enter") return;
    e.preventDefault();
    go(current(steps()));
    return;
  }
  if (e.key === "ArrowRight" || e.key === "j") S.wz.finished ? go(current(steps())) : step(1);
  else if (e.key === "ArrowLeft" || e.key === "k") S.wz.finished ? go(current(steps())) : step(-1);
  else if (e.key === "r" && !S.wz.finished) markAndNext();
  else if (e.key === "x" && !S.wz.finished) toggleReviewed();
  else return;
  e.preventDefault();
}
