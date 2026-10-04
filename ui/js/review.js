// Review tab: every file with its units, filters, and per-unit details.
import { esc, BUCKETS, LABEL, headline } from "./util.js";
import { S, render, syncURL, fileOfUnit } from "./state.js";
import { SEV_CLASS, issueCapChip, issueScenarioHTML, impactPill, likelihoodPill, attentionPill, decisionChips, scoresHTML, classificationHTML, movesHTML } from "./scores.js";
import { unitRows, diffTable, expandAllButton } from "./diff.js";
import { issueDraftButton } from "./comments.js";
import { issueFixMark } from "./fixes.js";
import { fixAllHTML, issueFixButton } from "./fix.js";
import { threadsHTML, threadsChip, raisedBy, raisedChip } from "./threads.js";
import { lintHTML, lintChip } from "./lint.js";
import { trText, trShown, trToggle, trDir } from "./entext.js";
import { showStep } from "./walkthrough.js";
import { showFileUnit } from "./files.js";
import { overviewHTML } from "./overview.js";

const diffShown = (u) => S.diffOpen[u.id] ?? (u.decision.bucket !== "none" && u.decision.bucket !== "aux");

function issuesHTML(f, u) {
  if (!u.issues?.length) return u.reviewed ? `<p><span class="lbl">Review</span>No issues found.</p>` : "";
  const items = u.issues.map((is, i) => {
    if (is.dismissed) return `<li class="dismissed"><span class="dz unknown">dismissed</span><b>${esc(is.title)}</b>${is.dismissed_why ? `<span class="idetail">${esc(is.dismissed_why)}</span>` : ""}</li>`;
    if (is.same_as) return repeatHTML(u, is);
    const dup = raisedBy(u, i);
    const draft = dup ? raisedChip(dup) : issueDraftButton(f, u, i);
    const fix = issueFixMark(u, i) || issueFixButton(u, i);
    return `<li><span class="dz ${SEV_CLASS[is.severity] || "high"}">${esc(is.severity)}</span>` +
      `${is.line ? `<span class="ln">line ${is.line}</span>` : ""}<b class="tr" ${trDir(u, `issues.${i}.title`, is.title)}>${trText(u, `issues.${i}.title`, is.title)}</b>${issueCapChip(is)}` +
      `${draft ? ` ${draft}` : ""} ${fix}` +
      `${is.detail ? `<span class="idetail tr" ${trDir(u, `issues.${i}.detail`, is.detail)}>${trText(u, `issues.${i}.detail`, is.detail)}</span>` : ""}${issueScenarioHTML(is, "idetail", u, i)}</li>`;
  }).join("");
  return `<p><span class="lbl">Issues found in review</span></p><ul class="issues">${items}</ul>`;
}

// repeatHTML is an issue the review raised again here that another one
// stands for (see Dedupe in Go): shown, not counted, and fixed with it.
function repeatHTML(u, is) {
  const to = is.same_as;
  const f = fileOfUnit(to.unit);
  const where = to.unit === u.id ? "this change" : f ? f.path : to.unit;
  return `<li class="repeat"><span class="dz unknown">repeat</span>${is.line ? `<span class="ln">line ${is.line}</span>` : ""}<b>${esc(is.title)}</b> ` +
    `<button class="linkbtn" data-act="issue-goto" data-unit="${esc(to.unit)}" title="${esc(to.title)}">same as an issue on ${esc(where)}</button></li>`;
}

// detailsHTML shows the summary and the review; how the unit was bucketed
// and scored stays collapsed until asked for.
function detailsHTML(f, u) {
  const d = u.decision;
  const main = [];
  if (u.summary) main.push(`<p><span class="lbl">Summary</span><span class="tr" ${trDir(u, "summary", u.summary)}>${trText(u, "summary", u.summary)}</span></p>`);
  main.push(issuesHTML(f, u));
  main.push(lintHTML(u));
  main.push(threadsHTML(u));
  if (u.focus?.length) main.push(`<p><span class="lbl">What to check</span></p><ul>${u.focus.map((x, j) => `<li class="tr" ${trDir(u, `focus.${j}`, x)}>${trText(u, `focus.${j}`, x)}</li>`).join("")}</ul>`);
  const rest = [movesHTML(u), d.reason ? `<p><span class="lbl">Classifier</span><span class="tr" ${trDir(u, "reason", d.reason)}>${trText(u, "reason", d.reason)}</span></p>` : "", scoresHTML(u), classificationHTML(d)];
  const shown = main.filter(Boolean);
  if (!shown.length) return `<div class="details">${rest.join("")}</div>`; // nothing reviewed
  const open = S.more.has(u.id);
  const btn = `<button class="more-btn" data-act="more" data-unit="${esc(u.id)}">${open ? "hide bucket, classifier and scores ▴" : "bucket, classifier and scores ▾"}</button>`;
  return `<div class="details">${shown.join("")}${btn}${open ? rest.join("") : ""}</div>`;
}

function unitHTML(f, u) {
  const d = u.decision, b = d.bucket;
  const open = diffShown(u);
  const more = S.details.has(u.id);
  const diff = open ? diffTable(f, unitRows(f, u), S.view) : "";
  return `
  <div class="unit ${b} ${open ? "" : "dim"}" data-bucket="${b}" data-uid="${esc(u.id)}">
    <div class="unit-head">
      <div class="row">
        <span class="pill ${b}">${LABEL[b]}</span>
        <span class="sym">${esc(u.symbol || "(file)")}</span>
        ${impactPill(u.impact)}${likelihoodPill(u.likelihood)}
        ${attentionPill(u)}
        ${decisionChips(u)}${lintChip(u)}${threadsChip(u)}
        <span class="src">${esc(d.source)}${d.confidence ? ` · ${(d.confidence * 100).toFixed(0)}%` : ""}</span>
      </div>
      <div class="headline"><span class="text tr" ${trDir(u, "headline", headline(u))} title="${esc(headline(u))}">${trShown(u, "headline", headline(u))}</span>${trToggle(u, "headline", headline(u))}${S.result.translating ? `<span class="tr-chip">translating…</span>` : ""}
        <button class="details-btn ${more ? "on" : ""}" data-act="details" data-unit="${esc(u.id)}">${more ? "hide details ▴" : "details ▾"}</button>
        <button class="details-btn" data-act="toggle" data-unit="${esc(u.id)}">${open ? "hide code ▴" : "show code ▾"}</button>
      </div>
      ${more ? detailsHTML(f, u) : ""}
    </div>
    <div class="diffwrap">${diff}</div>
  </div>`;
}

function fileHTML(f) {
  const c = {};
  f.units.forEach((u) => c[u.decision.bucket] = (c[u.decision.bucket] || 0) + 1);
  const shown = f.units.filter((u) => !S.hidden.has(u.decision.bucket));
  if (!shown.length) return "";
  const fd = S.drafts.filter((d) => d.path === f.path).length;
  return `
  <div class="file ${S.collapsed.has(f.path) ? "collapsed" : ""}">
    <div class="file-head" data-act="collapse" data-path="${esc(f.path)}">
      <span class="path">${f.old_path && f.old_path !== f.path ? esc(f.old_path) + " → " : ""}${esc(f.path)}</span>
      <span class="status">${esc(f.status)}${f.binary ? ", binary" : ""}</span>
      ${expandAllButton(f)}
      <span class="counts">${fd ? `<span class="pill draft">${fd} comment${fd > 1 ? "s" : ""}</span>` : ""}${BUCKETS.filter((b) => c[b]).map((b) => `<span class="pill ${b}">${c[b]}</span>`).join("")}</span>
    </div>
    <div class="units">${shown.map((u) => unitHTML(f, u)).join("")}</div>
  </div>`;
}

export function reviewHTML() {
  const r = S.result;
  const files = r.files.filter((f) => f.units?.length);
  return `${overviewHTML(r, true)}
    <div class="toolbar">
      ${BUCKETS.map((b) => `<span class="filter ${b} ${S.hidden.has(b) ? "off" : ""}" data-act="filter" data-b="${b}"><b>${r.counts?.[b] || 0}</b> ${LABEL[b]}</span>`).join("")}
      <span class="spacer"></span>
      ${fixAllHTML()}
      <button class="details-btn" data-act="all-diffs">${S.allHidden ? "show all code ▾" : "hide all code ▴"}</button>
      <span class="seg"><button class="${S.view === "split" ? "on" : ""}" data-act="view" data-v="split">Split</button><button class="${S.view === "unified" ? "on" : ""}" data-act="view" data-v="unified">Unified</button></span>
    </div>
    ${files.map(fileHTML).join("") || `<div class="empty">nothing to show</div>`}`;
}

// jumpToUnit opens the Review tab on one unit: its walkthrough step, its
// file in the Files mode, or in the classic list with its details and code.
export function jumpToUnit(id) {
  S.tab = "review";
  syncURL();
  if (S.mode === "walk" && showStep(id)) return;
  if (S.mode === "files" && showFileUnit(id)) return;
  S.mode = "classic";
  S.hidden.clear();
  S.details.add(id);
  S.diffOpen[id] = true;
  const f = fileOfUnit(id);
  if (f) S.collapsed.delete(f.path);
  render();
  const el = document.querySelector(`.unit[data-uid="${CSS.escape(id)}"]`);
  if (el) { el.scrollIntoView({ block: "start" }); el.classList.add("flash"); }
}

// showDraft opens the file a pending comment is on and scrolls to it.
export function showDraft(d) {
  S.collapsed.delete(d.path);
  (S.result.files.find((f) => f.path === d.path)?.units || []).forEach((u) => S.diffOpen[u.id] = true);
  S.hidden.clear();
  render();
  const el = document.getElementById("draft-" + d.id);
  if (el) { el.scrollIntoView({ block: "center" }); el.classList.add("flash"); }
}

const toggle = (set, v) => set.has(v) ? set.delete(v) : set.add(v);

export const actions = {
  filter: (el) => { toggle(S.hidden, el.dataset.b); },
  collapse: (el) => { toggle(S.collapsed, el.dataset.path); },
  details: (el) => { toggle(S.details, el.dataset.unit); },
  more: (el) => { toggle(S.more, el.dataset.unit); },
  toggle: (el) => {
    const u = S.result.files.flatMap((f) => f.units || []).find((x) => x.id === el.dataset.unit);
    S.diffOpen[u.id] = !diffShown(u);
  },
  "all-diffs": () => {
    S.allHidden = !S.allHidden;
    S.result.files.forEach((f) => (f.units || []).forEach((u) => S.diffOpen[u.id] = !S.allHidden));
  },
  view: (el) => { S.view = el.dataset.v; localStorage.setItem("pr-manager.view", S.view); },
};
