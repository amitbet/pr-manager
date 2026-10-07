// Sidebar: triaged PRs, latest result per PR, grouped by repo.
import { $, esc, api, postJSON, pills, ask, say, kindBadge, prState, USED_EVENT } from "./util.js";
import { S, repoName, localSrc, render } from "./state.js";
import { impactPill, likelihoodPill } from "./scores.js";
import { markTriaging } from "./jobs.js";

// Collapsed repo sections, remembered across reloads.
const collapsedRepos = new Set(JSON.parse(localStorage.getItem("pr-manager.collapsedRepos") || "[]"));
const saveCollapsedRepos = () => localStorage.setItem("pr-manager.collapsedRepos", JSON.stringify([...collapsedRepos]));

// sideShown is how many results a repo lists before "Show N more"
// (settings, 0 for all). The open result is always listed. Repos expanded
// with "Show N more" stay expanded until the page reloads.
export const sideShown = () => {
  const v = localStorage.getItem("pr-manager.side_shown");
  return v === null || v === "" ? 3 : Math.max(0, Number(v) || 0);
};
const expandedRepos = new Set();

// sideKeepDays is how long a change stays listed without being touched:
// triaged, used (below) or open (settings, 0 to keep every change). Its
// results stay in the cache, so opening its link again lists it again.
export const sideKeepDays = () => {
  const v = localStorage.getItem("pr-manager.side_keep_days");
  return v === null || v === "" ? 10 : Math.max(0, Number(v) || 0);
};
let showStale = false; // "show" on the hidden-changes line, until reload

// confirmRemove: whether a row's x asks before deleting the change
// (settings, off by default).
export const confirmRemove = () => localStorage.getItem("pr-manager.side_confirm_remove") === "1";

// Each change's last use (a PR, branch, commit or working tree): when a
// person last changed something on it (marked units reviewed, dismissed an
// issue, drafted a comment, fixed). Kept by change, not by result, so a new
// triage of it keeps it. A repo lists its changes by the later of their
// last use and their latest triage.
const USED_KEY = "pr-manager.lastUsed";
const lastUsed = JSON.parse(localStorage.getItem(USED_KEY) || "{}");
const identityOf = (r) => r.local_path ? `${r.local_path}#${r.rev ? `rev:${r.rev}` : r.head_ref}` : `${repoName(r.pr)}#${r.pr.number}`;
let shownList = null; // the last /api/results, to list again without fetching it
// When each change was last open, for sideKeepDays: opening a change
// doesn't move it up the list, but it keeps it listed.
const OPENED_KEY = "pr-manager.lastOpened";
const lastOpened = JSON.parse(localStorage.getItem(OPENED_KEY) || "{}");
const openRow = () => S.result && { ...S.result.pr, pr: S.result.pr };

// Hidden sidebar, remembered across reloads.
const SIDE_KEY = "pr-manager.sidebarCollapsed";
function setSideCollapsed(collapsed) {
  document.documentElement.classList.toggle("side-collapsed", collapsed);
  const btn = $("#side-btn");
  const label = collapsed ? "Show the sidebar" : "Hide the sidebar";
  btn.title = label;
  btn.setAttribute("aria-label", label);
  btn.setAttribute("aria-expanded", String(!collapsed));
}
setSideCollapsed(localStorage.getItem(SIDE_KEY) === "1");

let onPick = () => {};

// initSidebar sets what happens when a PR is picked (its result key, and
// the PR link or local path it was triaged from).
export function initSidebar(pick) {
  onPick = pick;
  $("#side-btn").addEventListener("click", () => {
    const collapsed = !document.documentElement.classList.contains("side-collapsed");
    setSideCollapsed(collapsed);
    localStorage.setItem(SIDE_KEY, collapsed ? "1" : "0");
    reportShown();
  });
  watchStates();
  document.addEventListener(USED_EVENT, (e) => {
    const key = e.detail || S.result?.key;
    const r = shownList?.find((x) => x.key === key) || (S.result?.key === key && { ...S.result.pr, pr: S.result.pr });
    if (!r) return;
    lastUsed[identityOf(r)] = Date.now();
    localStorage.setItem(USED_KEY, JSON.stringify(lastUsed));
    if (shownList) renderList(shownList);
  });
  $("#list").addEventListener("click", (e) => {
    const del = e.target.closest(".pr-del");
    if (del) {
      e.preventDefault();
      removeChange(del.closest(".pr-item").dataset.key).catch((err) => say(err.message));
      return;
    }
    const item = e.target.closest(".pr-item");
    if (item) { Promise.resolve(onPick(item.dataset.key, item.dataset.src)).catch((err) => say(err.message)); return; }
    if (e.target.closest(".side-stale button")) { showStale = !showStale; if (shownList) renderList(shownList); return; }
    const more = e.target.closest(".repo-more");
    if (more) {
      const sec = more.closest(".repo");
      const all = !sec.classList.contains("all");
      sec.classList.toggle("all", all);
      more.textContent = all ? "Show less" : more.dataset.more;
      all ? expandedRepos.add(more.dataset.repo) : expandedRepos.delete(more.dataset.repo);
      reportShown();
      return;
    }
    const head = e.target.closest(".repo-head");
    if (!head) return;
    const repo = head.dataset.repo;
    const sec = head.parentElement;
    const collapse = !sec.classList.contains("collapsed");
    sec.classList.toggle("collapsed", collapse);
    head.querySelector(".caret").textContent = collapse ? "▸" : "▾";
    collapse ? collapsedRepos.add(repo) : collapsedRepos.delete(repo);
    saveCollapsedRepos();
    reportShown();
  });
}

// removeChange deletes a change's results (every triage of the PR, branch,
// commit or working tree) from the cache, after asking if the setting is
// on. Removing the open change goes back to the empty page.
async function removeChange(key) {
  const r = shownList?.find((x) => x.key === key);
  if (!r) return;
  const id = identityOf(r);
  const name = r.local_path ? r.head_ref : `#${r.pr.number}`;
  if (confirmRemove() && !await ask(`Delete ${name} ${r.title} from ${repoName(r.pr)}? Its results go with it; triaging it again lists it again.`, "Delete")) return;
  const keys = shownList.filter((x) => identityOf(x) === id).map((x) => x.key);
  for (const k of keys) {
    // Not through api, which would count this as using the change.
    const res = await fetch(`/api/results/${encodeURIComponent(k)}`, { method: "DELETE" });
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
  }
  if (S.result && keys.includes(S.result.key)) { location.href = location.pathname; return; }
  await loadList();
}

// reportShown tells the PR watch (prwatch.go) which PRs are on screen,
// the only ones it asks GitHub about: the sidebar's rows that aren't
// under "Show N more" or in a folded repo (none while the sidebar is
// hidden), and the open result. A PR coming into view is asked about then.
let lastShown = "";
function reportShown() {
  const shown = document.documentElement.classList.contains("side-collapsed") ? [] :
    [...document.querySelectorAll("#list .pr-item")].filter((el) => el.offsetParent)
      .map((el) => shownList?.find((r) => r.key === el.dataset.key)).filter(Boolean)
      .map((r) => ({ ...r.pr, local_path: r.local_path, state: r.state }));
  if (S.result?.pr) shown.push(S.result.pr);
  const prs = shown.filter((p) => !p.local_path)
    .map((p) => ({ host: p.host, owner: p.owner, repo: p.repo, number: p.number, state: p.state }));
  const body = JSON.stringify(prs);
  // Unchanged, the watch may still have PRs to ask about: opening a result
  // asks again about its repo's.
  if (body === lastShown) { watchStates(); return; }
  lastShown = body;
  postJSON("/api/prwatch/visible", { prs }).then(watchStates, () => { lastShown = ""; });
}

// watchStates reloads the list when the server's PR watch (prwatch.go)
// saved a new state, such as a PR merged on GitHub, and spins the badge
// of the PR it is asking GitHub about. The watch does the polite polling
// of GitHub; this only asks the local server, often while the watch has
// PRs to ask about, and at once when the sidebar reports what's on screen.
const WATCH_EVERY = 30 * 1000;
const WATCH_BUSY = 500;
let gen = 0; // the server starts at 0, and its first checks run before the first poll
let asking = [], due = false;
let watchTimer = 0, watching = false, watchAgain = false;
async function watchStates() {
  if (watching) { watchAgain = true; return; }
  watching = true;
  clearTimeout(watchTimer);
  const w = await api("/api/prwatch").catch(() => null);
  if (w) {
    if (w.gen !== gen) loadList();
    gen = w.gen;
    asking = w.asking || [];
    due = w.due;
    markUpdating();
  }
  watching = false;
  watchTimer = setTimeout(watchStates, watchAgain ? 0 : asking.length || due ? WATCH_BUSY : WATCH_EVERY);
  watchAgain = false;
}

// markUpdating spins the sidebar's PR the watch is asking GitHub about.
function markUpdating() {
  document.querySelectorAll("#list .pr-item").forEach((el) =>
    el.classList.toggle("updating", asking.includes(el.dataset.src)));
}

export async function loadList() {
  const list = await api("/api/results");
  // The open result takes a state the watch saved since it was loaded.
  const now = S.result?.pr && list.find((r) => r.key === S.result.key);
  if (now && now.state !== S.result.pr.state) {
    S.result.pr.state = now.state;
    $("#url").dataset.state = prState(S.result.pr);
    render();
  }
  shownList = list;
  // Changes no longer listed lose their last use.
  const ids = new Set(list.map(identityOf));
  const stale = Object.keys(lastUsed).filter((id) => !ids.has(id));
  if (stale.length) {
    stale.forEach((id) => delete lastUsed[id]);
    localStorage.setItem(USED_KEY, JSON.stringify(lastUsed));
  }
  const gone = Object.keys(lastOpened).filter((id) => !ids.has(id));
  gone.forEach((id) => delete lastOpened[id]);
  // The list reloads every so often while a change is open: its stamp is
  // saved again at most hourly, which is plenty for counting days.
  const openId = S.result && identityOf(openRow());
  const stamp = openId && Date.now() - (lastOpened[openId] || 0) > 60 * 60 * 1000;
  if (stamp) lastOpened[openId] = Date.now();
  if (gone.length || stamp) localStorage.setItem(OPENED_KEY, JSON.stringify(lastOpened));
  renderList(list);
}

function renderList(list) {
  // Most recently used first, so the "Show N more" cut hides what was left
  // longest; a change's results then come newest first, and the first one
  // seen is its latest.
  const created = (r) => Date.parse(r.created_at) || 0;
  const used = (r) => Math.max(created(r), lastUsed[identityOf(r)] || 0);
  list = [...list].sort((a, b) => used(b) - used(a) || created(b) - created(a));
  const seen = new Set();
  const repos = new Map();
  const keep = sideKeepDays() * 24 * 60 * 60 * 1000;
  const touched = (r) => Math.max(used(r), lastOpened[identityOf(r)] || 0);
  const openId = S.result && identityOf(openRow());
  let hiddenStale = 0;
  for (const r of list) {
    const repo = repoName(r.pr);
    const identity = identityOf(r);
    if (seen.has(identity)) continue;
    seen.add(identity);
    if (keep && identity !== openId && Date.now() - touched(r) > keep) {
      hiddenStale++;
      if (!showStale) continue;
    }
    if (!repos.has(repo)) repos.set(repo, []);
    repos.get(repo).push(r);
  }
  const current = S.result ? repoName(S.result.pr) : null;
  const limit = sideShown();
  $("#list").innerHTML = [...repos.keys()].sort().map((repo) => {
    const prs = repos.get(repo);
    const open = !collapsedRepos.has(repo) || repo === current;
    const human = prs.reduce((n, r) => n + (r.counts?.human || 0), 0);
    const extra = (r, i) => limit && i >= limit && S.result?.key !== r.key;
    const hidden = prs.filter(extra).length;
    const all = expandedRepos.has(repo);
    const moreLabel = `Show ${hidden} more`;
    return `
    <div class="repo ${open ? "" : "collapsed"} ${all ? "all" : ""}">
      <button class="repo-head" data-repo="${esc(repo)}">
        <span class="caret">${open ? "▾" : "▸"}</span>
        <span class="rn" title="${esc(repo)}">${esc(repo)}</span>
        <span class="rc" title="${prs.length} results, ${human} units need human review">${prs.length}</span>
      </button>
      <div class="repo-prs">${prs.map((r, i) => `
        <a class="pr-item ${S.result?.key === r.key ? "active" : ""} ${extra(r, i) ? "extra" : ""}" data-key="${esc(r.key)}" data-src="${esc(localSrc(r) || `https://${r.pr.host || "github.com"}/${r.pr.owner}/${r.pr.repo}/pull/${r.pr.number}`)}">
          <button class="pr-del" title="Delete this change from the list" aria-label="Delete ${esc(r.local_path ? r.head_ref : `#${r.pr.number}`)}">✕</button>
          <span class="t">${kindBadge(r)}${r.local_path ? `<code>${esc(r.head_ref)}</code>` : `#${r.pr.number}`} ${esc(r.title)}</span>
          <span class="m">${pills(r.counts)} ${r.impact ? impactPill(r.impact, "imp") : ""}${r.likelihood ? likelihoodPill(r.likelihood, "lik") : ""} <span>${esc(r.state.toLowerCase())}</span> <span title="${esc(r.classifier)}">· ${esc(r.classifier.split("/").pop())}</span></span>
        </a>`).join("")}${hidden ? `
        <button class="repo-more" data-repo="${esc(repo)}" data-more="${moreLabel}">${all ? "Show less" : moreLabel}</button>` : ""}</div>
    </div>`;
  }).join("") + (hiddenStale ? `
    <div class="side-stale">${hiddenStale} not touched in ${sideKeepDays()} day${sideKeepDays() === 1 ? "" : "s"} ${showStale ? "shown" : "hidden"} <button class="linkbtn">${showStale ? "hide" : "show"}</button></div>` : "") || `<div class="empty">none yet</div>`;
  markTriaging();
  markUpdating();
  reportShown();
}
