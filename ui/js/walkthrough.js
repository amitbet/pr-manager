// Walkthrough tab: one step at a time, most important first, with the
// explanation beside the code. A step is one unit, one file's changes, or
// related units (Settings → Walkthrough steps).
import { trText, trDir } from "./entext.js";
import { $, esc, LABEL, headline } from "./util.js";
import { S, render, allUnits, fileByPath, focusTop } from "./state.js";
import { SEV_CLASS, SEV_RANK, issueCapChip, issueScenarioHTML, risk, impactPill, likelihoodPill, attentionPill, decisionChips, scoresHTML, classificationHTML, movesHTML } from "./scores.js";
import { unitRows, fileRows, fullyExpanded, expandAllButton, diffTable, actions as diffActions } from "./diff.js";
import { issueDraftButton } from "./comments.js";
import { issueFixMark } from "./fixes.js";
import { issueFixButton } from "./fix.js";
import { threadsHTML, raisedBy, raisedChip } from "./threads.js";
import { lintHTML } from "./lint.js";
import { openPanel } from "./panel.js";
import { overviewHTML, hasOverview } from "./overview.js";

// Units are ranked by bucket, then score, then review attention, then risk
// (impact times likelihood), then file order. "no review" units only on request.
const BRANK = { human: 0, skim: 1, aux: 2, none: 3 };
// Auxiliary and no-review units are left out of the steps unless asked for.
const optional = (u) => u.decision.bucket === "none" || u.decision.bucket === "aux";
const byRank = (a, b) => BRANK[a.u.decision.bucket] - BRANK[b.u.decision.bucket]
  || (b.u.score?.total ?? 0) - (a.u.score?.total ?? 0)
  || (b.u.attention || 0) - (a.u.attention || 0)
  || risk(b.u) - risk(a.u)
  || a.i - b.i;

// STEP_LINES is the most changed lines a file step holds before it is split,
// the size past which review effectiveness falls (SmartBear/Cisco; see
// experiments/grouping-readability/FINDINGS.md).
const STEP_LINES = 400;

const changedLines = (u) => (u.hunks || []).reduce((n, h) => n + (h.lines || []).filter((l) => l[0] === "+" || l[0] === "-").length, 0);
const firstLine = (u) => Math.min(Infinity, ...(u.hunks || []).map((h) => h.new_start));

// A step is {f, members, lead, why}: its units in reading order, the
// highest-ranked of them, which ranks the step and gives its bucket and f,
// and for related steps the kinds of link that joined them. Grouping here
// is only how a person reads the PR. The reviewer model's grouping
// (triage/group.go) is separate and only saves calls.
//
// steps runs on every render and key press, so the last answer is kept
// until the result, the settings, a unit (replaced, or re-placed by a
// budget or a dismissal) or its diff changes.
let stepsMemo = null;
function steps() {
  const all = allUnits();
  const sig = all.map(({ u }) => `${u.decision?.bucket}|${u.score?.total ?? 0}|${u.attention || 0}|${risk(u)}|${u.symbol || ""}`).join("\n");
  const m = stepsMemo;
  if (m && m.result === S.result && m.all === S.wz.all && m.mode === S.wz.steps && m.sig === sig && m.units.length === all.length
    && all.every(({ u, f }, i) => u === m.units[i].u && f === m.units[i].f && u.hunks === m.units[i].hunks)) return m.st;
  const units = all.map((x, i) => ({ ...x, i })).filter(({ u }) => S.wz.all || !optional(u));
  const groups = S.wz.steps === "file" ? fileGroups(units).map((members) => ({ members }))
    : S.wz.steps === "related" ? relatedGroups(units)
    : units.map((x) => ({ members: [x] }));
  const st = groups.map(({ members, why }) => {
    const lead = [...members].sort(byRank)[0];
    return { f: lead.f, members: readingOrder(members, lead), lead, why };
  }).sort((a, b) => byRank(a.lead, b.lead));
  stepsMemo = { result: S.result, all: S.wz.all, mode: S.wz.steps, sig, units: all.map(({ u, f }) => ({ u, f, hunks: u.hunks })), st };
  return st;
}

// readingOrder puts the lead's file first, then the other files in PR
// order, each file's units top to bottom.
function readingOrder(members, lead) {
  const rank = (x) => x.f.path === lead.f.path ? -1 : x.i;
  const fileRank = new Map();
  for (const x of members) fileRank.set(x.f.path, Math.min(fileRank.get(x.f.path) ?? Infinity, rank(x)));
  return [...members].sort((a, b) => fileRank.get(a.f.path) - fileRank.get(b.f.path) || firstLine(a.u) - firstLine(b.u) || a.i - b.i);
}

// fileGroups puts each file's units in one step. A file whose changes pass
// STEP_LINES is split between units into runs of at most STEP_LINES (a
// bigger unit stays whole); in practice that is a large test file.
function fileGroups(units) {
  const byPath = new Map();
  for (const x of units) byPath.set(x.f.path, [...(byPath.get(x.f.path) || []), x]);
  const out = [];
  for (const xs of byPath.values()) {
    xs.sort((a, b) => firstLine(a.u) - firstLine(b.u) || a.i - b.i);
    const split = xs.reduce((n, x) => n + changedLines(x.u), 0) > STEP_LINES;
    let run = [], n = 0;
    for (const x of xs) {
      const c = changedLines(x.u);
      if (split && run.length && n + c > STEP_LINES) { out.push(run); run = []; n = 0; }
      run.push(x);
      n += c;
    }
    out.push(run);
  }
  return out;
}

// STEP_UNITS caps a related step's unit count, like grouping.max_members
// in triage/group.go: single-linkage merging chains without it.
const STEP_UNITS = 8;
// SMALL_PAIR is how small two changes in one file must be together, in
// diff characters, before "same file" alone joins them (smallPairChars in
// triage/group.go).
const SMALL_PAIR = 1500;
// WHY labels the links that join a related step.
const WHY = { ref: "caller and callee", test: "code and its test", file: "small changes in one file" };

const diffText = (u) => (u.hunks || []).flatMap((h) => h.lines || []).join("\n");

// testFor matches triage/group.go: test is a test named after symbol sym,
// e.g. TestRetryCaps for retryCaps.
const testFor = (test, sym) => sym.length > 4 && /^(Test|Benchmark)/.test(test) && test.toLowerCase().includes(sym.toLowerCase());

// relatedGroups joins units a reviewer reads together, with the links of
// triage/group.go: units that name each other, a test named after the
// symbol it exercises, and two small changes in one file. Merging is
// greedy from the strongest link down and stops at STEP_LINES changed
// lines and STEP_UNITS units.
function relatedGroups(units) {
  const text = units.map((x) => unitWords(x.u)), name = units.map((x) => shortName(x.u.symbol));
  const links = [];
  for (let i = 0; i < units.length; i++) {
    for (let j = i + 1; j < units.length; j++) {
      const kinds = [];
      let score = 0;
      const ij = mentions(text[i].diff, name[j]), ji = mentions(text[j].diff, name[i]);
      if (ij || ji) { score += (ij ? 4 : 0) + (ji ? 4 : 0); kinds.push("ref"); }
      if (testFor(name[i], name[j]) || testFor(name[j], name[i])) { score += 3; kinds.push("test"); }
      if (units[i].f.path === units[j].f.path && text[i].diffLen + text[j].diffLen < SMALL_PAIR) { score += 1; kinds.push("file"); }
      if (score) links.push({ score, i, j, kinds });
    }
  }
  links.sort((a, b) => b.score - a.score || a.i - b.i || a.j - b.j);
  const owner = units.map((_, i) => i);
  const groups = units.map((x, i) => ({ idx: [i], lines: changedLines(x.u), why: new Set() }));
  for (const l of links) {
    const a = owner[l.i], b = owner[l.j];
    if (a === b) continue;
    const ga = groups[a], gb = groups[b];
    if (ga.lines + gb.lines > STEP_LINES || ga.idx.length + gb.idx.length > STEP_UNITS) continue;
    ga.idx.push(...gb.idx);
    ga.lines += gb.lines;
    for (const k of [...gb.why, ...l.kinds]) ga.why.add(k);
    for (const m of gb.idx) owner[m] = a;
    groups[b] = null;
  }
  return groups.filter(Boolean).map((g) => ({ members: g.idx.map((i) => units[i]), why: [...g.why].map((k) => WHY[k]) }));
}

const has = (s, id) => s.members.some((x) => x.u.id === id);
const stepDone = (s) => s.members.every((x) => S.wz.done.has(x.u.id));

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
// markReviewed marks units reviewed, or not, for the Files mode, which
// shares the walkthrough's progress.
export function markReviewed(ids, on) {
  for (const id of ids) on ? S.wz.done.add(id) : S.wz.done.delete(id);
  save();
}

// current is the index of the shown step: the one holding the saved unit,
// else the first unreviewed one. Progress is kept per unit, so it survives
// switching the grouping.
function current(st) {
  const i = st.findIndex((s) => has(s, S.wz.cur));
  if (i >= 0) return i;
  const open = st.findIndex((s) => !stepDone(s));
  return open >= 0 ? open : 0;
}

// shownStep is the step on screen, for the chat agent: its place and its
// units. null on the overview.
export function shownStep() {
  const st = steps();
  if (!st.length || onIntro()) return null;
  const i = current(st);
  return { n: i + 1, total: st.length, ids: st[i].members.map((x) => x.u.id) };
}

// pinTop is where the page header and the tabs pinned under it end.
export const pinTop = () => 54 + ($(".tabs")?.offsetHeight || 0);

// showStep opens the walkthrough on the step holding unit id, including
// auxiliary and no-review units if that's what it is, and scrolls to the unit's code;
// false if it isn't in a step even then.
export function showStep(id) {
  let i = steps().findIndex((s) => has(s, id));
  if (i < 0 && !S.wz.all) {
    S.wz.all = true;
    i = steps().findIndex((s) => has(s, id));
  }
  if (i < 0) return false;
  go(i);
  if (steps()[i].members[0].u.id !== id) scrollToUnit(id);
  return true;
}

// scrollToUnit brings unit id into view: its review in a step of several,
// else its first changed row.
function scrollToUnit(id) {
  const row = document.querySelector(`.wz-note[data-note="${CSS.escape(id)}"], .wz-code tr[data-unit="${CSS.escape(id)}"]`);
  if (row) { row.scrollIntoView({ block: "center" }); row.classList.add("flash"); }
}

// firstHumanNote is the review, highest on the page, of a unit in human
// review; null when the page has none.
export function firstHumanNote() {
  const human = new Set(allUnits().filter(({ u }) => u.decision.bucket === "human").map(({ u }) => u.id));
  return [...document.querySelectorAll(".wz-note[data-note]")].find((n) => human.has(n.dataset.note)) || null;
}

// scrollToSeg puts unit id's review and code at the top, just under the
// pinned header and step dots. A single-unit step has no review beside its
// code, so its first changed row is centered instead. False if neither is
// on the page.
export function scrollToSeg(id) {
  const seg = document.querySelector(`.wz-note[data-note="${CSS.escape(id)}"]`)?.closest(".wz-seg");
  if (!seg) {
    const row = document.querySelector(".wz-code tr.focus-start");
    row?.scrollIntoView({ block: "center" });
    return !!row;
  }
  const pinned = pinTop() + ($(".wz-steps")?.offsetHeight || 0);
  window.scrollTo({ top: seg.getBoundingClientRect().top + window.scrollY - pinned });
  return true;
}

// go shows step i. When the page is scrolled past the card, it scrolls back
// to the card's top, just under the pinned step dots, so the new step is
// read from its start. With Settings → Review view → focus on, a step
// opens on its first change in human review instead, when that isn't
// already its first row (or the step shows its whole file), unless atTop:
// a step picked by its dot opens at its top, so the page doesn't jump down
// under the click.
function go(i, atTop = false) {
  const st = steps();
  if (!st.length) return;
  const s = st[Math.max(0, Math.min(st.length - 1, i))];
  S.wz.cur = s.members[0].u.id;
  S.wz.finished = S.wz.intro = false;
  S.composer = null;
  save();
  render();
  if (!atTop && focusTop()) {
    const id = focusTarget(s);
    if (id && scrollToSeg(id)) return;
  }
  toCardTop();
}
// toCardTop scrolls a card the page is scrolled past back to its top, just
// under the pinned step dots, and leaves a page that isn't alone: a reader
// scrolled down far enough to pin the tabs keeps them pinned, one at the
// top of the page keeps the PR header in view. The card is at least a
// window tall (walkthrough.css), so a short step can always sit there.
function toCardTop() {
  const card = $(".wz-card");
  if (!card) return;
  const top = card.getBoundingClientRect().top + card.clientTop; // inside the colored top border
  const pinned = pinTop() + ($(".wz-steps")?.offsetHeight || 0);
  if (top < pinned) window.scrollTo({ top: top + window.scrollY - pinned });
}
// focusTarget is the unit go scrolls to with focus on: the step's first
// change in human review, unless the step starts with it. A single-unit
// step has no review rows, so it only scrolls when it shows its whole file.
function focusTarget(s) {
  const first = $(".wz-note[data-note]");
  if (!first) return s.lead.u.decision.bucket === "human" && fullyExpanded(s.lead.f) ? s.lead.u.id : null;
  const n = firstHumanNote();
  return n && (n !== first || fullyExpanded(s.lead.f)) ? n.dataset.note : null;
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
  toCardTop();
}

function toggleReviewed() {
  const st = steps();
  if (!st.length) return;
  const s = st[current(st)], on = !stepDone(s);
  for (const { u } of s.members) on ? S.wz.done.add(u.id) : S.wz.done.delete(u.id);
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
  for (const { u } of st[i].members) S.wz.done.add(u.id);
  if (i < st.length - 1) { go(i + 1); return; }
  const open = st.findIndex((s) => !stepDone(s));
  if (open >= 0) { go(open); return; }
  S.wz.finished = true;
  save();
  render();
  toCardTop();
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
  acts.push(issueFixMark(u, i) || issueFixButton(u, i, "linkbtn"));
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

// noteOpen is whether unit u's review is open in a grouped step: set by
// hand, else open when it is the step's only unit, or for human review or
// live issues, the reviews that ask something of the reader.
const noteOpen = (u, alone) => S.wz.noteOpen.get(u.id) ?? (alone || u.decision.bucket === "human" || worstFirst(u.issues || []).length > 0);

// noteHTML is one unit's review in a grouped step, placed beside its code.
// Closed, it is one line: bucket, symbol, headline and issue count. A unit
// in a lower bucket than the step is dimmed. The header is a div, not a
// button, because the headline's EN toggle is a button and buttons don't
// nest.
export function noteHTML(f, u, shown, bucket, alone) {
  const b = u.decision.bucket, open = noteOpen(u, alone);
  const live = worstFirst(u.issues || []);
  const count = live.length ? `<span class="dz ${SEV_CLASS[live[0].severity] || "high"}">${live.length} issue${live.length > 1 ? "s" : ""}</span>`
    : u.reviewed ? `<span class="wz-clean">✓ no issues</span>` : "";
  return `<div class="wz-note wz-explain ${open ? "open" : ""} ${BRANK[b] > BRANK[bucket] ? "low" : ""}" data-note="${esc(u.id)}">
    <div class="wz-note-h" data-act="wz-note" data-id="${esc(u.id)}"><button class="car" data-act="wz-note" data-id="${esc(u.id)}" aria-expanded="${open}" aria-label="${open ? "Collapse" : "Expand"} review">${open ? "▾" : "▸"}</button><span class="pill ${b}">${LABEL[b]}</span>${u.symbol ? `<span class="sym">${esc(u.symbol)}</span>` : ""}${count}<span class="h tr" ${trDir(u, "headline", headline(u))}>${trText(u, "headline", headline(u))}</span></div>
    ${open ? `<div class="wz-note-b">${explainHTML(f, u, shown)}</div>` : ""}</div>`;
}

// splitAtUnits cuts a file's rows before each unit's first hunk, so each
// unit's review can sit beside its code. hunks are the hunks rows was built
// from, in its order: one "hh" row each. Unless the whole file shows, the
// hunk's expand-above rows and the context they opened go with it; in the
// whole file that context is the code between units, so it stays put.
// Returns [{u, rows}], u null for rows before the first unit.
function splitAtUnits(rows, hunks, units, whole) {
  const owner = new Map(units.flatMap((u) => (u.hunks || []).map((h) => [h, u])));
  const out = [{ u: null, rows: [] }], seen = new Set();
  const above = (r) => (r.t === "exp" && (r.dir === "up" || r.dir === "fold-up")) || (r.t === "ctx" && r.x);
  let k = 0;
  for (const r of rows) {
    if (r.t === "hh") {
      const u = owner.get(hunks[k++]);
      if (u && !seen.has(u.id)) {
        seen.add(u.id);
        const prev = out[out.length - 1].rows, moved = [];
        while (!whole && prev.length && above(prev[prev.length - 1])) moved.unshift(prev.pop());
        out.push({ u, rows: moved });
      }
    }
    out[out.length - 1].rows.push(r);
  }
  return out.filter((c) => c.u || c.rows.length);
}

// markIssues flags the rows on a review issue's line and returns the set
// of new-file lines shown, for "show line".
function markIssues(rows, units) {
  const issueLines = new Set(units.flatMap((x) => x.issues || []).map((x) => x.line).filter(Boolean));
  const shown = new Set();
  rows.forEach((r) => {
    if (r.t === "del" || !r.n || r.other) return;
    shown.add(r.n);
    if (issueLines.has(r.n)) r.iss = true;
  });
  return shown;
}

const byHunkPos = (a, b) => a.new_start - b.new_start || a.old_start - b.old_start;

// unitSegmentsHTML is f's rows cut at each of units, a row per unit with
// its review beside its code. The Files mode (files.js) shows a whole file
// this way. alone is whether units is a single unit.
export function unitSegmentsHTML(f, units, bucket, alone) {
  const whole = fullyExpanded(f);
  const mine = units.flatMap((x) => x.hunks || []);
  const rows = whole ? fileRows(f, { hunks: mine }) : stepRows(f, units);
  const hunks = (whole ? (f.units || []).flatMap((x) => x.hunks || []) : mine).sort(byHunkPos);
  const shown = markIssues(rows, units);
  return splitAtUnits(rows, hunks, units, whole).map((c) => `<div class="wz-seg">
      <div class="wz-seg-note">${c.u ? noteHTML(f, c.u, shown, bucket, alone) : ""}</div>
      <div class="wz-seg-code">${c.rows.length ? diffTable(f, c.rows, S.wz.view) : ""}</div></div>`).join("");
}

// fileSectionHTML is one file of a grouped step: its bar, then a row for
// each of the step's units in it, the unit's review beside its code. alone is whether
// the step has only this one unit.
function fileSectionHTML(f, units, bucket, alone) {
  const whole = fullyExpanded(f);
  const fd = S.drafts.filter((d) => d.path === f.path).length;
  return `<section class="wz-code">
    <div class="wz-codebar"><span class="path">${esc(f.path)}</span><span>${units.length} change${units.length > 1 ? "s" : ""}</span>${expandAllButton(f, "wz-expand-all")}${whole ? `<span>whole file · other changes dimmed</span>` : ""}<span class="spacer"></span>${fd ? `<span class="pill draft">${fd} comment${fd > 1 ? "s" : ""} in this file</span>` : ""}<span>hover a line and click + to comment</span></div>
    ${unitSegmentsHTML(f, units, bucket, alone)}</section>`;
}

// newSide is the code a reader reads as the change: added and context
// lines. Removed lines name the old code, not what the step depends on.
const newSide = (u) => (u.hunks || []).flatMap((h) => (h.lines || []).filter((l) => l[0] === "+" || l[0] === " ").map((l) => l.slice(1))).join("\n");

// shortName and mentions match triage/prcontext.go: the identifier other
// code uses for a unit's symbol, and a whole-word mention of it.
function shortName(sym) {
  if (!sym || sym === "imports") return "";
  const last = sym.trim().split(/\s+/).pop();
  return last.slice(last.lastIndexOf(".") + 1);
}
// words holds a unit's identifiers. Since a name is one identifier, a
// \b-bounded match of it is the same as it being one of the maximal word
// runs of the text.
const mentions = (words, name) => name.length >= 3 && /^[A-Za-z_][A-Za-z0-9_]*$/.test(name) && words.has(name);
const words = (text) => new Set(text.match(/[A-Za-z0-9_]+/g));

// unitWords is a unit's diff length and the words of its diff and of its
// new side, read once per unit (again only if its hunks are replaced).
const wordCache = new WeakMap();
function unitWords(u) {
  let c = wordCache.get(u);
  if (!c || c.hunks !== u.hunks) {
    const diff = diffText(u);
    c = { hunks: u.hunks, diffLen: diff.length, diff: words(diff), newSide: words(newSide(u)) };
    wordCache.set(u, c);
  }
  return c;
}

// uses is the changed units outside step s whose name s's new code
// mentions: the types, helpers and callees it relies on. Matching is by
// name, so it can miss indirect uses and over-match common names.
function uses(s) {
  const mine = new Set(s.members.map((x) => x.u.id));
  const text = s.members.map((x) => unitWords(x.u).newSide);
  return allUnits().filter(({ u }) => !mine.has(u.id) && text.some((w) => mentions(w, shortName(u.symbol))));
}

// usesHTML shows the definitions a step uses, collapsed to their name with
// a link to the step that reviews them; opening one shows its diff for
// reference.
function usesHTML(s, st) {
  const us = uses(s);
  if (!us.length) return "";
  const rows = us.map(({ u, f }) => {
    const j = st.findIndex((t) => has(t, u.id));
    const k = `${s.members[0].u.id}|${u.id}`, open = S.wz.usesOpen.has(k);
    const where = j >= 0 ? `<button class="linkbtn" data-act="wz-go" data-i="${j}">step ${j + 1}</button>` : `<span class="chip">${LABEL[u.decision.bucket]}, not a step</span>`;
    return `<div class="wz-use ${open ? "open" : ""}"><div class="wz-use-h">
        <button class="wz-use-t" data-act="wz-use" data-k="${esc(k)}" aria-expanded="${open}"><span class="car">${open ? "▾" : "▸"}</span><span class="sym">${esc(u.symbol)}</span><span class="path">${esc(f.path)}${u.line ? `:${u.line}` : ""}</span></button>
        <span class="h tr" ${trDir(u, "headline", headline(u))}>${trText(u, "headline", headline(u))}</span>${where}</div>
      ${open ? diffTable(f, unitRows(f, u), S.wz.view) : ""}</div>`;
  }).join("");
  return `<div class="wz-uses"><div class="wz-uses-bar"><b>Uses ${us.length} changed definition${us.length > 1 ? "s" : ""} from elsewhere in the PR</b><span>reviewed in their own step, shown here for reference</span></div>${rows}</div>`;
}

function progressHTML(st, i, toggle) {
  const done = st.filter(stepDone).length;
  const intro = onIntro();
  const ovDot = hasOverview(S.result) ? `<button class="wz-dot ov-dot ${intro ? "cur" : ""}" data-act="wz-intro" title="Overview of the PR">Overview</button>` : "";
  const dots = ovDot + st.map((s, j) => {
    const ok = stepDone(s);
    const title = s.members.length > 1 ? `${j + 1}. ${s.f.path}\n${s.members.map(({ u, f }) => `· ${f === s.f ? "" : `${f.path}: `}${headline(u)}`).join("\n")}` : `${j + 1}. ${s.lead.u.id}\n${headline(s.lead.u)}`;
    return `<button class="wz-dot ${s.lead.u.decision.bucket} ${ok ? "done" : ""} ${j === i && !S.wz.finished && !intro ? "cur" : ""}"
      data-act="wz-go" data-i="${j}" title="${esc(title)}">${ok ? "✓" : j + 1}</button>`;
  }).join("");
  return `
    <div class="wz-top"><span><b>${done}</b> of <b>${st.length}</b> reviewed</span><span class="spacer"></span>${toggle}
      <span class="seg"><button class="${S.wz.view === "split" ? "on" : ""}" data-act="wz-view" data-v="split">Split</button><button class="${S.wz.view === "unified" ? "on" : ""}" data-act="wz-view" data-v="unified">Unified</button></span></div>
    <div class="wz-prog"><div style="width:${(100 * done) / st.length}%"></div></div>
    <div class="wz-steps">${dots}</div>`;
}

// syncDots puts the step dots back where they were scrolled sideways before
// a re-render (x), then scrolls just enough to show the current dot.
export function syncDots(x) {
  const row = $(".wz-steps");
  if (!row) return;
  if (x != null) row.scrollLeft = x;
  const cur = row.querySelector(".wz-dot.cur");
  if (!cur) return;
  const l = cur.offsetLeft, r = l + cur.offsetWidth, pad = 8;
  if (l - pad < row.scrollLeft) row.scrollLeft = l - pad;
  else if (r + pad > row.scrollLeft + row.clientWidth) row.scrollLeft = r + pad - row.clientWidth;
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

// stepRows is the diff of a step's units in file order. The first changed
// row of each unit carries its id (data-unit), so "line N" can scroll to it.
function stepRows(f, units) {
  const owner = new Map(units.flatMap((u) => (u.hunks || []).map((h) => [h, u.id])));
  const seen = new Set();
  return [...owner.keys()].sort((a, b) => a.new_start - b.new_start || a.old_start - b.old_start).flatMap((h) => {
    const rows = unitRows(f, { hunks: [h] }), id = owner.get(h);
    const first = !seen.has(id) && rows.find((r) => r.t === "add" || r.t === "del");
    if (first) { first.uid = id; seen.add(id); }
    return rows;
  });
}

// singleBodyHTML is a one-unit step: the explanation beside the code.
function singleBodyHTML(s, st, i) {
  const { f } = s, u = s.lead.u;
  // Expanded, the whole file shows, with other units' changes dimmed.
  const whole = fullyExpanded(f);
  const rows = whole ? fileRows(f, { hunks: u.hunks || [] }) : stepRows(f, [u]);
  const shown = markIssues(rows, [u]);
  const fd = S.drafts.filter((d) => d.path === f.path).length;
  return `<div class="wz-body">
        <section class="wz-explain">${explainHTML(f, u, shown)}</section>
        <section class="wz-code">
          <div class="wz-codebar"><b>Step ${i + 1} of ${st.length}</b><span>${esc(u.id)}</span>${expandAllButton(f, "wz-expand-all")}${whole ? `<span>whole file · other changes dimmed</span>` : ""}<span class="spacer"></span>${fd ? `<span class="pill draft">${fd} comment${fd > 1 ? "s" : ""} in this file</span>` : ""}<span>hover a line and click + to comment</span></div>
          ${diffTable(f, rows, S.wz.view)}
          ${usesHTML(s, st)}
        </section>
      </div>`;
}

function cardHTML(st, i) {
  const s = st[i], { f } = s, u = s.lead.u;
  // With grouping on, every step uses the grouped layout, one unit or
  // several, so the layout doesn't change from step to step.
  const units = s.members.map((x) => x.u), group = units.length > 1, grouped = S.wz.steps !== "unit";
  const b = u.decision.bucket;
  const files = [...new Set(s.members.map((x) => x.f))];
  const isDone = stepDone(s);
  return `
    <div class="wz-card ${b}">
      <div class="wz-head">
        <div class="wz-where"><span class="pill ${b}">${LABEL[b]}</span><b>Step ${i + 1} of ${st.length}</b> ·
          <span class="path">${f.old_path && f.old_path !== f.path ? esc(f.old_path) + " → " : ""}${esc(f.path)}${!group && u.line ? `:${u.line}` : ""}</span>
          ${group ? `<span class="chip">${units.length} changes${files.length > 1 ? ` in ${files.length} files` : ""}</span>` : u.symbol ? `<span class="sym">${esc(u.symbol)}</span>` : ""}<span class="status chip">${esc(f.status)}</span>${s.why?.length ? `<span class="chip">${esc(s.why.join(" · "))}</span>` : ""}</div>
        <div class="wz-headline tr" ${trDir(u, "headline", headline(u))}>${trText(u, "headline", headline(u))}</div>
        <div class="row">${impactPill(u.impact)}${likelihoodPill(u.likelihood)}${attentionPill(u)}${decisionChips(u)}</div>
      </div>
      ${grouped ? `<div class="wz-body seq">
        ${files.map((ff) => fileSectionHTML(ff, s.members.filter((x) => x.f === ff).map((x) => x.u), b, !group)).join("")}
        <section class="wz-code">${usesHTML(s, st)}</section>
      </div>` : singleBodyHTML(s, st, i)}
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
  const nNone = allUnits().filter(({ u }) => optional(u)).length;
  // Grouped, these units join other steps rather than adding one each,
  // so a count would not match the steps it adds.
  const what = S.wz.steps === "unit" ? `${nNone} auxiliary and no-review unit${nNone > 1 ? "s" : ""}` : "auxiliary and no-review units";
  const toggle = nNone ? `<label><input type="checkbox" data-act="wz-all" ${S.wz.all ? "checked" : ""}> include ${what}</label>` : "";
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
  "wz-go": (el) => { go(+el.dataset.i, true); return false; },
  "wz-intro": () => { showIntro(); return false; },
  "wz-start": () => { go(current(steps())); return false; },
  "wz-prev": () => { step(-1); return false; },
  "wz-next": () => { step(1); return false; },
  "wz-mark": () => { markAndNext(); return false; },
  "wz-toggle": () => { toggleReviewed(); return false; },
  "wz-reset": () => { S.wz.done.clear(); S.wz.cur = null; go(0); return false; },
  "wz-all": (el) => { S.wz.all = el.checked; },
  "wz-view": (el) => { S.wz.view = el.dataset.v; },
  "wz-submit": () => { openPanel(); return false; },
  "wz-unit": (el) => { scrollToUnit(el.dataset.id); return false; },
  "wz-use": (el) => { const k = el.dataset.k; S.wz.usesOpen.has(k) ? S.wz.usesOpen.delete(k) : S.wz.usesOpen.add(k); },
  "wz-note": (el) => { const n = el.closest(".wz-note"); if (n) S.wz.noteOpen.set(el.dataset.id, !n.classList.contains("open")); },
  // The issue's own file section first: a related step can show the same
  // line number in two files.
  "wz-line": (el) => {
    const sel = `tr[data-iss="${+el.dataset.line}"]`;
    const row = el.closest(".wz-code")?.querySelector(sel) || document.querySelector(`.wz-code ${sel}`);
    if (row) { row.scrollIntoView({ block: "center", behavior: "smooth" }); row.classList.add("flash"); }
    return false;
  },
};

export function onKeydown(e) {
  if (S.tab !== "review" || S.mode !== "walk" || !S.result || e.metaKey || e.ctrlKey || e.altKey) return;
  if (e.target.closest?.("input, textarea, select")) return;
  // Keys belong to an open dialog (job log, settings), and Enter or Space
  // on a focused button or link is that control's own click.
  if (document.querySelector("dialog[open]")) return;
  if ((e.key === "Enter" || e.key === " ") && e.target.closest?.("button, a, summary, [contenteditable]:not([contenteditable=false])")) return;
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
