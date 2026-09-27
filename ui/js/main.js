// Entry point: renders the page for the current PR and routes data-act
// clicks to the component that owns them.
//
// Components (review.js, walkthrough.js, treemap.js, comments.js, diff.js)
// export `actions`: handlers keyed by data-act. A handler changes state and
// returns nothing to have the page re-rendered, or false when it rendered
// (or deliberately didn't) itself.
import { $, esc, api, postJSON } from "./util.js";
import { S, onRender, render, syncURL, prBase, repoName } from "./state.js";
import { impactPill, likelihoodPill, attLevel } from "./scores.js";
import { prepare, actions as diffActions } from "./diff.js";
import { syncComposer, focusComposer, actions as commentActions, onKeydown as composerKeydown } from "./comments.js";
import { reviewHTML, showDraft, actions as reviewActions } from "./review.js";
import { walkHTML, loadProgress, actions as walkActions, onKeydown as walkKeydown } from "./walkthrough.js";
import { treemapHTML, renderTreemap, actions as treemapActions } from "./treemap.js";
import { initPanel, renderPanel, panelOpen, closePanel, updateReviewButton } from "./panel.js";
import { initSidebar, loadList } from "./sidebar.js";
import { initTriage, triageURL } from "./triage.js";
import { initSettings, refreshSettings } from "./settings.js";
import { fixBanner, initFix, actions as fixActions } from "./fix.js";
import { initJobs, triageJobFor, watchJob } from "./jobs.js";
import { translate } from "./translate.js";
import { actions as enActions } from "./entext.js";
import { loadOverview, actions as overviewActions } from "./overview.js";
import * as budget from "./budget.js";

// TABS are the views of a triaged PR. mount runs after the tab's HTML is on
// the page.
// The Review tab is the walkthrough or the classic list of every unit.
const TABS = [
  { id: "review", label: "Review", html: () => S.mode === "classic" ? reviewHTML() : walkHTML() },
  { id: "map", label: "Code map", html: treemapHTML, mount: renderTreemap },
];
const MODES = [["walk", "Walkthrough"], ["classic", "Classic"]];

const actions = {
  ...diffActions, ...commentActions, ...reviewActions, ...walkActions, ...treemapActions, ...fixActions, ...enActions, ...overviewActions,
  tab: (el) => { S.tab = el.dataset.tab; syncURL(); },
  mode: (el) => { S.tab = "review"; S.mode = el.dataset.mode; localStorage.setItem("pr-manager.reviewmode", S.mode); syncURL(); },
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

function prHeadHTML(r) {
  const pr = r.pr;
  const local = !!pr.local_path;
  const publishHint = pr.uncommitted ? "Commit changes and triage again" : !pr.ahead ? "No commits ahead of the base branch" : pr.owner === "local" ? "Set a GitHub origin remote" : "";
  return `
    <div class="pr-head">
      <h2>${local ? esc(pr.title || pr.head_ref) : `<a href="${esc(pr.url)}" target="_blank" rel="noopener">${esc(pr.title)}</a> <span style="color:var(--muted);font-weight:400">#${pr.number}</span>`}</h2>
      <div class="meta">${local ? `<code>${esc(pr.local_path)}</code>` : esc(repoName(pr))} · ${esc(pr.author)} · ${esc(pr.state.toLowerCase())} ·
        <code>${esc(pr.base_ref)}@${esc(pr.base_oid.slice(0, 8))}</code> ← <code>${esc(pr.head_ref)}@${esc(pr.head_oid.slice(0, 8))}</code> ·
        +${pr.additions}/−${pr.deletions} · classify <code>${esc(r.classifier)}</code> · summarize <code>${esc(r.summarizer)}</code>${r.summary_lang ? ` in ${esc(r.summary_lang)}` : ""} ·
        ${(r.duration_ms / 1000).toFixed(1)}s</div>
      ${local ? `<div class="meta" style="margin-top:6px">${pr.ahead || 0} commit${pr.ahead === 1 ? "" : "s"} ahead, ${pr.behind || 0} behind origin/${esc(pr.base_ref)}${pr.uncommitted ? " · includes working tree changes" : ""} · ${pr.url ? `<a href="${esc(pr.url)}" target="_blank" rel="noopener">Open PR</a>` : `<button class="primary" data-act="create-pr" ${publishHint ? `disabled title="${esc(publishHint)}"` : ""}>Create PR</button>${publishHint ? ` <span>${esc(publishHint)}</span>` : ""}`}</div>` : ""}
      ${r.impact || r.likelihood || r.attention ? `<div class="meta" style="margin-top:6px;display:flex;gap:6px;align-items:center;flex-wrap:wrap">
        ${impactPill(r.impact, "max impact")}${r.impact?.basis ? `<code>${esc(r.impact.basis)}</code>` : ""}
        ${likelihoodPill(r.likelihood, "max likelihood")}
        <span class="dz ${attLevel(r.attention)}" title="highest review attention">max attention ${r.attention}</span>
        ${r.codemap ? `<span title="code map build">map ${esc(r.codemap)}</span>` : ""}</div>` : ""}
      ${r.local_fix_dir ? `<div class="meta">Local fix branch: <code>${esc(r.local_fix_branch || "detached")}</code> · ${r.local_fix_location === "clone" ? "cached clone" : "worktree"}: <code>${esc(r.local_fix_dir)}</code> · ${r.fix_rounds} fix and review round${r.fix_rounds === 1 ? "" : "s"}</div>` : ""}
    </div>`;
}

// translateBanner says the text is being replaced, or why it wasn't.
const translateBanner = (r) => r.translating
  ? `<div class="tr-banner" role="status"><span class="spinner"></span>Translating to ${esc(r.translating)}… the English below is replaced when it's ready.</div>`
  : r.translate_error ? `<div class="tr-banner error" role="status">${esc(r.translate_error)}</div>` : "";

const tabsHTML = () => `<div class="tabs">${TABS.map((t) =>
  `<button class="${S.tab === t.id ? "on" : ""}" data-act="tab" data-tab="${t.id}">${t.label}</button>`).join("")}
  <span class="spacer"></span>${S.tab === "review" ? `<span class="seg mode" title="How to review">${MODES.map(([m, l]) =>
    `<button class="${S.mode === m ? "on" : ""}" data-act="mode" data-mode="${m}">${l}</button>`).join("")}</span>` : ""}</div>`;

onRender(() => {
  const r = S.result;
  if (!r) return;
  const tab = TABS.find((t) => t.id === S.tab) || TABS[0];
  updateReviewButton();
  $("#main").innerHTML = prHeadHTML(r) + translateBanner(r) + fixBanner() + tabsHTML() + tab.html();
  $("#main").classList.toggle("translating", !!r.translating);
  tab.mount?.();
  focusComposer();
  if (panelOpen()) renderPanel();
});

async function showKey(key) {
  const r = await api(`/api/results/${encodeURIComponent(key)}`);
  budget.apply(r, S.cfg);
  r.overview_loading = !r.overview; // loadOverview below writes it
  S.result = r;
  Object.assign(S, { collapsed: new Set(), details: new Set(), more: new Set(), showEn: new Set(), diffOpen: {}, allHidden: false, above: {}, below: {}, files: {}, composer: null });
  S.tm.zoom = [];
  loadProgress();
  S.drafts = await api(`${prBase()}/drafts`).catch(() => []);
  prepare(r);
  syncURL();
  $("#url").value = r.pr.local_path || r.pr.url;
  closePanel();
  render();
  refreshSettings();
  loadList();
  // The translation takes the overview along, so it waits for one being written.
  loadOverview().finally(() => { if (S.result === r) translate(); });
}

// openResult shows a result, or the log of the triage that is replacing it
// while that runs.
async function openResult(key, src) {
  const job = await triageJobFor(src);
  if (job) watchJob(job.id);
  else await showKey(key);
}

$("#main").addEventListener("click", async (e) => {
  const el = e.target.closest("[data-act]");
  const handler = el && actions[el.dataset.act];
  if (!handler) return;
  syncComposer();
  if ((await handler(el, e)) !== false) render();
});
$("#main").addEventListener("keydown", composerKeydown);
document.addEventListener("keydown", walkKeydown);

(async () => {
  S.cfg = await api("/api/config").catch(() => null);
  initSidebar(openResult);
  initPanel(showDraft);
  initTriage();
  initJobs(showKey, loadList);
  initFix(showKey);
  initSettings(() => { if (S.result) { budget.apply(S.result, S.cfg); render(); } }, translate);
  await loadList();
  const q = new URLSearchParams(location.search);
  if (TABS.some((t) => t.id === q.get("tab"))) S.tab = q.get("tab");
  if (q.get("tab") === "walk") S.mode = "walk"; // links from before the tabs merged
  if (q.get("key")) await openResult(q.get("key"), q.get("pr") || q.get("path")).catch(() => {});
  else if (q.get("pr") || q.get("path")) triageURL(q.get("pr") || q.get("path"));
})();
