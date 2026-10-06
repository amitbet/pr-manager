// Sidebar: triaged PRs, latest result per PR, grouped by repo.
import { $, esc, api, pills, say, kindBadge, USED_EVENT } from "./util.js";
import { S, repoName, localSrc } from "./state.js";
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

// Each change's last use (a PR, branch, commit or working tree): when a
// person last changed something on it (marked units reviewed, dismissed an
// issue, drafted a comment, fixed). Kept by change, not by result, so a new
// triage of it keeps it. A repo lists its changes by the later of their
// last use and their latest triage.
const USED_KEY = "pr-manager.lastUsed";
const lastUsed = JSON.parse(localStorage.getItem(USED_KEY) || "{}");
const identityOf = (r) => r.local_path ? `${r.local_path}#${r.rev ? `rev:${r.rev}` : r.head_ref}` : `${repoName(r.pr)}#${r.pr.number}`;
let shownList = null; // the last /api/results, to list again without fetching it

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
  });
  document.addEventListener(USED_EVENT, (e) => {
    const key = e.detail || S.result?.key;
    const r = shownList?.find((x) => x.key === key) || (S.result?.key === key && { ...S.result.pr, pr: S.result.pr });
    if (!r) return;
    lastUsed[identityOf(r)] = Date.now();
    localStorage.setItem(USED_KEY, JSON.stringify(lastUsed));
    if (shownList) renderList(shownList);
  });
  $("#list").addEventListener("click", (e) => {
    const item = e.target.closest(".pr-item");
    if (item) { Promise.resolve(onPick(item.dataset.key, item.dataset.src)).catch((err) => say(err.message)); return; }
    const more = e.target.closest(".repo-more");
    if (more) {
      const sec = more.closest(".repo");
      const all = !sec.classList.contains("all");
      sec.classList.toggle("all", all);
      more.textContent = all ? "Show less" : more.dataset.more;
      all ? expandedRepos.add(more.dataset.repo) : expandedRepos.delete(more.dataset.repo);
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
  });
}

export async function loadList() {
  const list = await api("/api/results");
  shownList = list;
  // Changes no longer listed lose their last use.
  const ids = new Set(list.map(identityOf));
  const stale = Object.keys(lastUsed).filter((id) => !ids.has(id));
  if (stale.length) {
    stale.forEach((id) => delete lastUsed[id]);
    localStorage.setItem(USED_KEY, JSON.stringify(lastUsed));
  }
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
  for (const r of list) {
    const repo = repoName(r.pr);
    const identity = identityOf(r);
    if (seen.has(identity)) continue;
    seen.add(identity);
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
          <span class="t">${kindBadge(r)}${r.local_path ? `<code>${esc(r.head_ref)}</code>` : `#${r.pr.number}`} ${esc(r.title)}</span>
          <span class="m">${pills(r.counts)} ${r.impact ? impactPill(r.impact, "imp") : ""}${r.likelihood ? likelihoodPill(r.likelihood, "lik") : ""} <span>${esc(r.state.toLowerCase())}</span> <span title="${esc(r.classifier)}">· ${esc(r.classifier.split("/").pop())}</span></span>
        </a>`).join("")}${hidden ? `
        <button class="repo-more" data-repo="${esc(repo)}" data-more="${moreLabel}">${all ? "Show less" : moreLabel}</button>` : ""}</div>
    </div>`;
  }).join("") || `<div class="empty">none yet</div>`;
  markTriaging();
}
