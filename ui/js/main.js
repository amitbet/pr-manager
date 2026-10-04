// Entry point: renders the page for the current PR and routes data-act
// clicks to the component that owns them.
//
// Components (review.js, walkthrough.js, treemap.js, comments.js, diff.js)
// export `actions`: handlers keyed by data-act. A handler changes state and
// returns nothing to have the page re-rendered, or false when it rendered
// (or deliberately didn't) itself.
import { $, esc, api, postJSON } from "./util.js";
import { S, onRender, render, syncURL, prBase, repoName, localSrc } from "./state.js";
import { impactPill, likelihoodPill, attLevel } from "./scores.js";
import { prepare, actions as diffActions } from "./diff.js";
import { syncComposer, focusComposer, actions as commentActions, onKeydown as composerKeydown } from "./comments.js";
import { reviewHTML, showDraft, actions as reviewActions } from "./review.js";
import { walkHTML, loadProgress, actions as walkActions, onKeydown as walkKeydown } from "./walkthrough.js";
import { filesHTML, mountFiles, actions as filesActions } from "./files.js";
import { treemapHTML, renderTreemap, actions as treemapActions } from "./treemap.js";
import { initPanel, renderPanel, panelOpen, closePanel, updateReviewButton } from "./panel.js";
import { initSidebar, loadList } from "./sidebar.js";
import { initTriage, triageURL } from "./triage.js";
import { initRevPicker } from "./revpicker.js";
import { initSettings, refreshSettings } from "./settings.js";
import { fixBanner, initFix, actions as fixActions } from "./fix.js";
import { initJobs, triageJobFor, watchJob } from "./jobs.js";
import { translate } from "./translate.js";
import { actions as enActions } from "./entext.js";
import { loadOverview, actions as overviewActions } from "./overview.js";
import { issuesHTML, syncDismiss, actions as issueActions } from "./issues.js";
import { initFixes, actions as pendingActions } from "./fixes.js";
import { sequenceHTML, loadSequence, actions as seqActions, onKeydown as seqKeydown } from "./sequence.js";
import * as budget from "./budget.js";

// TABS are the views of a triaged PR. mount runs after the tab's HTML is on
// the page.
// The Review tab is the walkthrough, the file tree, or the classic list of every unit.
const TABS = [
  { id: "review", label: "Review", html: () => S.mode === "classic" ? reviewHTML() : S.mode === "files" ? filesHTML() : walkHTML(), mount: () => S.mode === "files" && mountFiles() },
  { id: "issues", label: "Issues", html: issuesHTML, count: openClaims },
  { id: "sequence", label: "Sequence", html: sequenceHTML, mount: loadSequence },
  { id: "map", label: "Code map", html: treemapHTML, mount: renderTreemap },
];

// openClaims is the badge on the Issues tab: what is still standing
// against this PR, review issues, lint findings and confirmed comments
// alike, with what has been dismissed or fixed left out.
function openClaims() {
  let n = 0;
  for (const f of S.result.files) {
    for (const u of f.units || []) {
      n += (u.issues || []).filter((i) => !i.dismissed && !i.same_as).length;
      n += (u.lint || []).filter((x) => !x.dismissed).length;
      n += (u.threads || []).filter((t) => !t.fixed && t.status === "valid" && t.duplicate_of == null).length;
    }
  }
  return n;
}
const MODES = [["walk", "Walkthrough"], ["files", "Files"], ["classic", "Classic"]];

const actions = {
  ...diffActions, ...commentActions, ...reviewActions, ...walkActions, ...filesActions, ...treemapActions, ...fixActions, ...enActions, ...overviewActions, ...issueActions, ...pendingActions, ...seqActions,
  tab: (el) => { S.tab = el.dataset.tab; syncURL(); },
  mode: (el) => { S.tab = "review"; S.mode = el.dataset.mode; syncURL(); },
  "create-pr": async (el) => {
    el.disabled = true;
    el.textContent = "Creating PR…";
    try {
      const out = await postJSON(`/api/local/${encodeURIComponent(S.result.key)}/publish`, {});
      S.result.pr.url = out.url;
      render();
      triageURL(out.url);
    } catch (e) { alert(e.message); el.disabled = false; el.textContent = "Create PR"; }
    return false;
  },
};

// carriedLine says how much of this run came from the previous push. The
// numbers are the point of it: a reader should be able to see that the
// review they are reading is mostly not new, and how much of it is.
function carriedLine(r) {
  const c = r.carried;
  if (!c?.reused) return "";
  const total = c.reused + c.reviewed;
  return `<div class="meta carried-line" title="A unit keeps its review when its own diff, and the units it was judged against (callers, callees, file-mates, code that moved between them), are unchanged. Everything else — units, static analysis, impact, likelihood, classification and scoring — was redone on the whole diff.">
    Incremental: <b>${c.reused}</b> of ${total} reviewed unit${total === 1 ? "" : "s"} kept from <code>${esc(c.from.slice(0, 8))}</code>, ${c.reviewed} reviewed again</div>`;
}

// modelsLine names the models of a run. When the review call placed the
// units, the classifier is the summarizer (without its "+repo tools").
function modelsLine(r) {
  if (r.summarizer && r.summarizer !== "off" && r.summarizer.startsWith(r.classifier)) return `analyze <code>${esc(r.summarizer)}</code>`;
  return `classify <code>${esc(r.classifier)}</code> · summarize <code>${esc(r.summarizer)}</code>`;
}

function prHeadHTML(r) {
  const pr = r.pr;
  const local = !!pr.local_path;
  const publishHint = pr.uncommitted ? "Commit changes and triage again" : !pr.ahead ? "No commits ahead of the base branch" : pr.owner === "local" ? "Set a GitHub origin remote" : "";
  return `
    <div class="pr-head">
      <h2>${local ? esc(pr.title || pr.head_ref) : `<a href="${esc(pr.url)}" target="_blank" rel="noopener">${esc(pr.title)}</a> <span style="color:var(--muted);font-weight:400">#${pr.number}</span>`}</h2>
      <div class="meta">${local ? `<code>${esc(pr.local_path)}</code>` : esc(repoName(pr))} · ${esc(pr.author)} · ${esc(pr.state.toLowerCase())} ·
        <code>${esc(pr.base_ref)}@${esc(pr.base_oid.slice(0, 8))}</code> ← <code>${esc(pr.head_ref)}@${esc(pr.head_oid.slice(0, 8))}</code> ·
        +${pr.additions}/−${pr.deletions} · ${modelsLine(r)}${r.summary_lang ? ` in ${esc(r.summary_lang)}` : ""} ·
        ${(r.duration_ms / 1000).toFixed(1)}s</div>
      ${local && pr.single_commit ? `<div class="meta" style="margin-top:6px">single commit <code>${esc(pr.rev)}</code>, from its parent</div>` : ""}
      ${local && pr.rev && !pr.single_commit ? `<div class="meta" style="margin-top:6px">branch <code>${esc(pr.rev)}</code> as committed: ${pr.ahead || 0} commit${pr.ahead === 1 ? "" : "s"} ahead, ${pr.behind || 0} behind origin/${esc(pr.base_ref)}</div>` : ""}
      ${local && !pr.rev ? `<div class="meta" style="margin-top:6px">${pr.ahead || 0} commit${pr.ahead === 1 ? "" : "s"} ahead, ${pr.behind || 0} behind origin/${esc(pr.base_ref)}${pr.uncommitted ? " · includes working tree changes" : ""} · ${pr.url ? `<a href="${esc(pr.url)}" target="_blank" rel="noopener">Open PR</a>` : `<button class="primary" data-act="create-pr" ${publishHint ? `disabled title="${esc(publishHint)}"` : ""}>Create PR</button>${publishHint ? ` <span>${esc(publishHint)}</span>` : ""}`}</div>` : ""}
      ${r.impact || r.likelihood || r.attention ? `<div class="meta" style="margin-top:6px;display:flex;gap:6px;align-items:center;flex-wrap:wrap">
        ${impactPill(r.impact, "max impact")}${r.impact?.basis ? `<code>${esc(r.impact.basis)}</code>` : ""}
        ${likelihoodPill(r.likelihood, "max likelihood")}
        <span class="dz ${attLevel(r.attention)}" title="highest review attention">max attention ${r.attention}</span>
        ${r.codemap ? `<span title="code map build">map ${esc(r.codemap)}</span>` : ""}</div>` : ""}
      ${carriedLine(r)}
      ${r.local_fix_dir ? `<div class="meta">Local fix branch: <code>${esc(r.local_fix_branch || "detached")}</code> · ${r.local_fix_location === "clone" ? "cached clone" : r.local_fix_location === "branch" ? "current checkout" : "worktree"}: <code>${esc(r.local_fix_dir)}</code> · ${r.fix_rounds} fix and review round${r.fix_rounds === 1 ? "" : "s"}${r.fix_from_rev ? ` · fixes issues found in <code>${esc(r.fix_from_rev)}</code>` : ""}</div>` : ""}
      ${r.fix_warning ? `<div class="tr-banner warn" role="status">${esc(r.fix_warning)}</div>` : ""}
    </div>`;
}

// translateBanner says the text is being replaced, or why it wasn't.
const translateBanner = (r) => r.translating
  ? `<div class="tr-banner" role="status"><span class="spinner"></span>Translating to ${esc(r.translating)}… the English below is replaced when it's ready.</div>`
  : r.translate_error ? `<div class="tr-banner error" role="status">${esc(r.translate_error)}</div>` : "";

const tabsHTML = () => `<div class="tabs">${TABS.map((t) => {
    const n = t.count?.() || 0;
    return `<button class="${S.tab === t.id ? "on" : ""}" data-act="tab" data-tab="${t.id}">${t.label}${n ? `<span class="tab-count">${n}</span>` : ""}</button>`;
  }).join("")}
  <span class="spacer"></span>${S.tab === "review" ? `<span class="seg mode" title="How to review">${MODES.map(([m, l]) =>
    `<button class="${S.mode === m ? "on" : ""}" data-act="mode" data-mode="${m}">${l}</button>`).join("")}</span>` : ""}</div>`;

// Any render can come from something the reader didn't do (an LLM call
// returning, a timer), so it must not take what they are typing: the
// fields are copied into state first, and the one being typed in gets
// its focus, caret and scroll back once the page is rebuilt. The review
// summary in the panel keeps its text itself, on every keystroke.
const typing = (el) => el?.id && (el.tagName === "TEXTAREA" || (el.tagName === "INPUT" && el.selectionStart != null));

function keepTyping(draw) {
  const a = document.activeElement;
  const was = typing(a) && { id: a.id, value: a.value, start: a.selectionStart, end: a.selectionEnd, dir: a.selectionDirection, top: a.scrollTop };
  draw();
  if (!was) return false;
  // A field under the same id with other text is another one (a composer
  // opened on another line, the dismiss form of another claim).
  const b = document.getElementById(was.id);
  if (!b || b.value !== was.value) return false;
  if (b !== a) {
    b.focus({ preventScroll: true });
    b.setSelectionRange(was.start, was.end, was.dir);
    b.scrollTop = was.top;
  }
  return true;
}

onRender(() => {
  const r = S.result;
  if (!r) return;
  syncComposer();
  syncDismiss();
  const kept = keepTyping(() => {
    const tab = TABS.find((t) => t.id === S.tab) || TABS[0];
    updateReviewButton();
    $("#main").innerHTML = prHeadHTML(r) + translateBanner(r) + fixBanner() + tabsHTML() + tab.html();
    $("#main").classList.toggle("translating", !!r.translating);
    tab.mount?.();
    if (panelOpen()) renderPanel();
  });
  if (!kept) focusComposer();
});

// showing counts showKey calls, so one overtaken by a later click stops
// at its next await instead of showing its PR over the later one.
let showing = 0;

async function showKey(key) {
  const call = ++showing;
  const r = await api(`/api/results/${encodeURIComponent(key)}`);
  if (call !== showing) return;
  budget.apply(r, S.cfg);
  r.overview_loading = !r.overview; // loadOverview below writes it
  prepare(r); // before S.result, so no render sees it unprepared
  S.result = r;
  S.drafts = []; // the previous PR's, until this one's arrive
  Object.assign(S, { collapsed: new Set(), details: new Set(), more: new Set(), showEn: new Set(), diffOpen: {}, allHidden: false, above: {}, below: {}, files: {}, composer: null, dismissing: null, showDismissed: false, seqText: false, seqView: "after", fv: { path: null, closed: new Set(), tried: new Set(), loading: null } });
  S.tm.zoom = [];
  loadProgress();
  const drafts = await api(`${prBase()}/drafts`).catch(() => []);
  if (call !== showing || S.result !== r) return;
  S.drafts = drafts;
  syncURL();
  $("#url").value = localSrc(r.pr) || r.pr.url;
  closePanel();
  render();
  refreshSettings();
  loadList();
  // The translation takes the overview along, so it waits for one being written.
  loadOverview().finally(() => { if (S.result === r) translate(); });
  loadSequence(); // before the tab is opened, so it is not waited for there
}

// openResult shows a result, or the log of the triage that is replacing it
// while that runs.
async function openResult(key, src) {
  const job = await triageJobFor(src);
  if (job) watchJob(job.id);
  else await showKey(key);
}

$("#main").addEventListener("submit", async (e) => {
  const el = e.target.closest("[data-act]");
  const handler = el && actions[el.dataset.act];
  if (!handler) return;
  e.preventDefault();
  if ((await handler(el, e)) !== false) render();
});
$("#main").addEventListener("click", async (e) => {
  const el = e.target.closest("[data-act]");
  const handler = el && actions[el.dataset.act];
  // A form's action runs on submit (above), not on a click inside it.
  if (!handler || el.tagName === "FORM") return;
  syncComposer();
  if ((await handler(el, e)) !== false) render();
});
$("#main").addEventListener("keydown", composerKeydown);
document.addEventListener("keydown", walkKeydown);
document.addEventListener("keydown", seqKeydown);

(async () => {
  S.cfg = await api("/api/config").catch(() => null);
  initSidebar(openResult);
  initPanel(showDraft);
  initTriage();
  initRevPicker();
  initJobs(showKey, loadList);
  initFix(showKey);
  initFixes(showKey);
  initSettings(() => { if (S.result) { budget.apply(S.result, S.cfg); render(); } }, translate);
  await loadList();
  const q = new URLSearchParams(location.search);
  if (TABS.some((t) => t.id === q.get("tab"))) S.tab = q.get("tab");
  if (q.get("tab") === "walk") S.mode = "walk"; // links from before the tabs merged
  if (q.get("key")) await openResult(q.get("key"), q.get("pr") || q.get("path")).catch(() => {});
  else if (q.get("pr") || q.get("path")) triageURL(q.get("pr") || q.get("path"));
})();
