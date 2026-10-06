// Entry point: renders the page for the current PR and routes data-act
// clicks to the component that owns them.
//
// Components (review.js, walkthrough.js, treemap.js, comments.js, diff.js)
// export `actions`: handlers keyed by data-act. A handler changes state and
// returns nothing to have the page re-rendered, or false when it rendered
// (or deliberately didn't) itself.
import { $, esc, api, postJSON, say, kindBadge, kindOf, prState, KIND_TITLE } from "./util.js";
import { S, onRender, render, syncURL, prBase, repoName, localSrc, allUnits } from "./state.js";
import { impactPill, likelihoodPill, attLevel } from "./scores.js";
import { prepare, actions as diffActions } from "./diff.js";
import { syncComposer, focusComposer, actions as commentActions, onKeydown as composerKeydown } from "./comments.js";
import { reviewHTML, showDraft, jumpToUnit, actions as reviewActions } from "./review.js";
import { walkHTML, loadProgress, syncDots, shownStep, actions as walkActions, onKeydown as walkKeydown } from "./walkthrough.js";
import { filesHTML, mountFiles, shownFile, actions as filesActions } from "./files.js";
import { treemapHTML, renderTreemap, actions as treemapActions } from "./treemap.js";
import { initPanel, renderPanel, panelOpen, closePanel, updateReviewButton } from "./panel.js";
import { initSidebar, loadList } from "./sidebar.js";
import { initTriage, triageURL, syncURLKind } from "./triage.js";
import { initRevPicker } from "./revpicker.js";
import { initSettings, refreshSettings, jobSettings } from "./settings.js";
import { fixBanner, initFix, actions as fixActions } from "./fix.js";
import { initJobs, triageJobFor, watchJob } from "./jobs.js";
import { translate } from "./translate.js";
import { actions as enActions } from "./entext.js";
import { loadOverview, actions as overviewActions } from "./overview.js";
import { issuesHTML, syncDismiss, actions as issueActions } from "./issues.js";
import { initFixes, actions as pendingActions } from "./fixes.js";
import { sequenceHTML, loadSequence, actions as seqActions, onKeydown as seqKeydown } from "./sequence.js";
import { initChat } from "./chat.js";
import { initViewCtx, selection, clearSelection, fields as openFields, fieldText } from "./viewctx.js";
import { initAgentAPI, changeOf, describe as agentDescribe, run as agentRun } from "./agentapi.js";
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
    el.textContent = "Writing description…";
    try {
      const out = await postJSON(`/api/local/${encodeURIComponent(S.result.key)}/publish`, jobSettings());
      S.result.pr.url = out.url;
      render();
      triageURL(out.url);
    } catch (e) { say(e.message); el.disabled = false; el.textContent = "Create PR"; }
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

// webBase is the repo's page on its forge, GitHub or GitLab, with the
// path GitLab puts before tree and commit pages; null for a checkout
// without a remote.
function webBase(pr) {
  if (pr.owner === "local") return null;
  const host = pr.host || "github.com";
  return { repo: `https://${host}/${pr.owner}/${pr.repo}`, sub: /gitlab/i.test(host) ? "/-" : "", forge: /gitlab/i.test(host) ? "GitLab" : /github/i.test(host) ? "GitHub" : host };
}
// openLink is the small icon after a repo, branch or commit that opens it
// on its forge. A local branch or commit not pushed yet has no page there.
const OPEN_ICON = `<svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/></svg>`;
const iconLink = (url, t) => `<a class="open-link" href="${esc(url)}" target="_blank" rel="noopener" title="${esc(t)}" aria-label="${esc(t)}">${OPEN_ICON}</a>`;
const openLink = (w, path, what) => (w ? iconLink(w.repo + path, `Open ${what} on ${w.forge}`) : "");
const refPath = (w, ref) => `${w.sub}/tree/${ref.split("/").map(encodeURIComponent).join("/")}`;
const commitPath = (w, oid) => `${w.sub}/commit/${encodeURIComponent(oid)}`;
const refLink = (w, ref) => (w ? openLink(w, refPath(w, ref), `branch ${ref}`) : "");
const commitLink = (w, oid) => (w ? openLink(w, commitPath(w, oid), `commit ${oid.slice(0, 8)}`) : "");

// titleLink is the page the header title opens: the PR, else a single
// commit's page or the branch's, or none without a forge.
function titleLink(pr, w) {
  if (pr.url) return { url: pr.url, what: `Open the PR on ${w?.forge || "GitHub"}` };
  if (!w) return null;
  if (pr.single_commit) return { url: w.repo + commitPath(w, pr.head_oid), what: `Open commit ${pr.head_oid.slice(0, 8)} on ${w.forge}` };
  return { url: w.repo + refPath(w, pr.head_ref), what: `Open branch ${pr.head_ref} on ${w.forge}` };
}

function prHeadHTML(r) {
  const pr = r.pr;
  const local = !!pr.local_path;
  const w = webBase(pr);
  // A single commit's refs are its parent's id (or the empty tree) and nothing.
  const branch = (ref) => (pr.single_commit || !ref ? "" : refLink(w, ref));
  const commit = (oid) => (pr.base_ref === "empty tree" && oid === pr.base_oid ? "" : commitLink(w, oid));
  const tl = titleLink(pr, w);
  const title = (h) => (tl ? `<a class="title" href="${esc(tl.url)}" target="_blank" rel="noopener" title="${esc(tl.what)}">${h}</a>` : h);
  const publishHint = pr.uncommitted ? "Commit changes and triage again" : !pr.ahead ? "No commits ahead of the base branch" : pr.owner === "local" ? "Set a GitHub origin remote" : "";
  return `
    <div class="pr-head">
      <h2>${kindBadge(pr)}${title(local ? `<span class="${pr.title ? "" : "ref"}">${esc(pr.title || pr.head_ref)}</span>` : `${esc(pr.title)}`)}${local ? "" : ` <span style="color:var(--muted);font-weight:400">#${pr.number}</span>`} <span class="kind-label ${kindOf(pr)}" data-state="${esc(prState(pr))}">${KIND_TITLE[kindOf(pr)]}</span></h2>
      <div class="meta">${local ? `<code>${esc(pr.local_path)}</code>` : esc(repoName(pr))}${openLink(w, "", "repository")} · ${esc(pr.author)} · ${esc(pr.state.toLowerCase())} ·
        <code>${esc(pr.base_ref)}</code>${branch(pr.base_ref)}<code>@${esc(pr.base_oid.slice(0, 8))}</code>${commit(pr.base_oid)} ←
        <code>${esc(pr.head_ref)}</code>${branch(pr.head_ref)}<code>@${esc(pr.head_oid.slice(0, 8))}</code>${commit(pr.head_oid)} ·
        +${pr.additions}/−${pr.deletions} · ${modelsLine(r)}${r.summary_lang ? ` in ${esc(r.summary_lang)}` : ""} ·
        ${(r.duration_ms / 1000).toFixed(1)}s</div>
      ${local && pr.single_commit ? `<div class="meta" style="margin-top:6px">single commit <code>${esc(pr.rev)}</code>${commit(pr.head_oid)}, from its parent</div>` : ""}
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
  ? `<div class="tr-banner" role="status"><span class="spinner"></span>Translating to ${esc(r.translating)}… the text below is replaced when it's ready.</div>`
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

// chatWhere tells the chat agent what is on screen, so "this" in a
// question means something: the tab and mode, what is selected, and the
// text boxes open (viewctx.js).
function chatWhere() {
  if (!S.result) return {};
  const sel = selection(), fs = openFields();
  return { ...chatPlace(), ...(sel ? { selection: sel } : {}), ...(fs.length ? { fields: fs, field_text: fieldText() } : {}) };
}
function chatPlace() {
  if (S.tab === "review") {
    if (S.mode === "walk") {
      const s = shownStep();
      return s ? { where: `Review tab, walkthrough step ${s.n} of ${s.total}`, units: s.ids } : { where: "Review tab, the walkthrough's overview page" };
    }
    if (S.mode === "files") return { where: "Review tab, Files mode", path: shownFile() };
    return { where: "Review tab, the classic list of every unit" };
  }
  if (S.tab === "issues") return { where: "Issues tab: every claim against the PR (review issues, lint findings, GitHub comments), worst first" };
  if (S.tab === "sequence") return { where: `Sequence tab: the call flow ${S.seqView === "before" ? "before" : "after"} the PR` };
  if (S.tab === "map") return { where: `Code map tab, colored by ${S.tm.mode}${S.tm.zoom.length ? `, zoomed into ${S.tm.zoom.map(String).join("/")}` : ""}` };
  return {};
}
const unitName = (u) => u.symbol ? `${u.file.split("/").pop()} · ${u.symbol}` : u.file;
const chat = initChat({
  key: () => S.result?.key || null,
  title: () => S.result?.pr.title || S.result?.pr.head_ref || "",
  where: chatWhere,
  clearSelection,
  openUnit: jumpToUnit,
  unitLabel: (id) => { const x = S.result && allUnits().find(({ u }) => u.id === id); return x ? unitName(x.u) : id; },
  labels: () => Object.fromEntries((S.result ? allUnits() : []).map(({ u }) => [u.id, unitName(u)])),
  cfg: () => S.cfg,
  change: () => changeOf(S.result),
  describe: agentDescribe,
  runAction: agentRun,
});

onRender(() => {
  const r = S.result;
  if (!r) return;
  syncComposer();
  syncDismiss();
  const kept = keepTyping(() => {
    const tab = TABS.find((t) => t.id === S.tab) || TABS[0];
    updateReviewButton();
    const dotsX = $(".wz-steps")?.scrollLeft;
    $("#main").innerHTML = prHeadHTML(r) + translateBanner(r) + fixBanner() + tabsHTML() + tab.html();
    syncDots(dotsX);
    $("#main").classList.toggle("translating", !!r.translating);
    tab.mount?.();
    if (panelOpen()) renderPanel();
  });
  chat.sync();
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
  syncURLKind(kindOf(r.pr));
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
  if (S.cfg?.version) {
    const v = $("#app-version");
    v.textContent = S.cfg.version;
    v.title = `commit ${S.cfg.commit}, built ${S.cfg.built}`;
  }
  initSidebar(openResult);
  initPanel(showDraft);
  initTriage();
  initRevPicker();
  initJobs(showKey, loadList);
  initFix(showKey);
  initFixes(showKey);
  initAgentAPI({ showKey });
  initViewCtx();
  initSettings(() => { if (S.result) { budget.apply(S.result, S.cfg); render(); } }, translate);
  await loadList();
  const q = new URLSearchParams(location.search);
  if (TABS.some((t) => t.id === q.get("tab"))) S.tab = q.get("tab");
  if (q.get("tab") === "walk") S.mode = "walk"; // links from before the tabs merged
  if (q.get("key")) await openResult(q.get("key"), q.get("pr") || q.get("path")).catch(() => {});
  else if (q.get("pr") || q.get("path")) triageURL(q.get("pr") || q.get("path"));
})();
