// Sidebar: triaged PRs, latest result per PR, grouped by repo.
import { $, esc, api, pills, say } from "./util.js";
import { S, repoName, localSrc } from "./state.js";
import { impactPill, likelihoodPill } from "./scores.js";
import { markTriaging } from "./jobs.js";

// Collapsed repo sections, remembered across reloads.
const collapsedRepos = new Set(JSON.parse(localStorage.getItem("pr-manager.collapsedRepos") || "[]"));
const saveCollapsedRepos = () => localStorage.setItem("pr-manager.collapsedRepos", JSON.stringify([...collapsedRepos]));

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
  $("#list").addEventListener("click", (e) => {
    const item = e.target.closest(".pr-item");
    if (item) { Promise.resolve(onPick(item.dataset.key, item.dataset.src)).catch((err) => say(err.message)); return; }
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
  const seen = new Set();
  const repos = new Map();
  for (const r of list) {
    const repo = repoName(r.pr);
    const identity = r.local_path ? `${r.local_path}#${r.rev ? `rev:${r.rev}` : r.head_ref}` : `${repo}#${r.pr.number}`;
    if (seen.has(identity)) continue;
    seen.add(identity);
    if (!repos.has(repo)) repos.set(repo, []);
    repos.get(repo).push(r);
  }
  const current = S.result ? repoName(S.result.pr) : null;
  $("#list").innerHTML = [...repos.keys()].sort().map((repo) => {
    const prs = repos.get(repo);
    const open = !collapsedRepos.has(repo) || repo === current;
    const human = prs.reduce((n, r) => n + (r.counts?.human || 0), 0);
    return `
    <div class="repo ${open ? "" : "collapsed"}">
      <button class="repo-head" data-repo="${esc(repo)}">
        <span class="caret">${open ? "▾" : "▸"}</span>
        <span class="rn" title="${esc(repo)}">${esc(repo)}</span>
        <span class="rc" title="${prs.length} results, ${human} units need human review">${prs.length}</span>
      </button>
      <div class="repo-prs">${prs.map((r) => `
        <a class="pr-item ${S.result?.key === r.key ? "active" : ""}" data-key="${esc(r.key)}" data-src="${esc(localSrc(r) || `https://${r.pr.host || "github.com"}/${r.pr.owner}/${r.pr.repo}/pull/${r.pr.number}`)}">
          <span class="t">${r.local_path ? esc(r.head_ref) : `#${r.pr.number}`} ${esc(r.title)}</span>
          <span class="m">${pills(r.counts)} ${r.impact ? impactPill(r.impact, "imp") : ""}${r.likelihood ? likelihoodPill(r.likelihood, "lik") : ""} <span>${esc(r.state.toLowerCase())}</span> <span title="${esc(r.classifier)}">· ${esc(r.classifier.split("/").pop())}</span></span>
        </a>`).join("")}</div>
    </div>`;
  }).join("") || `<div class="empty">none yet</div>`;
  markTriaging();
}
