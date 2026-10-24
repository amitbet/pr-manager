// Git object picker: lists a checkout's branches and recent commits and
// triages the one picked as path#rev. The box may hold the checkout's
// path, or a repository's URL or owner/repo: its checkout in the code
// directory or the workspace is used, and one without a checkout is
// cloned into the workspace when asked. A branch is compared with the
// default branch, its origin branch, or any other branch (base...branch).
import { $, esc, api, postJSON } from "./util.js";
import { triageURL } from "./triage.js";
import { codeSources } from "./settings.js";

let list = null; // the checkout's revList
let box = ""; // what the box held when the picker opened
let tab = localStorage.getItem("pr-manager.revtab") || "commits";
// compare is "default", "upstream" (each branch's origin branch) or
// "branch:<name>".
let compare = localStorage.getItem("pr-manager.revcompare") || "default";
let worktree = localStorage.getItem("pr-manager.revworktree") !== "0";

const ago = (iso) => {
  const s = (Date.now() - new Date(iso)) / 1000;
  for (const [n, u] of [[86400 * 365, "y"], [86400 * 30, "mo"], [86400, "d"], [3600, "h"], [60, "m"]]) if (s >= n) return `${Math.floor(s / n)}${u} ago`;
  return "just now";
};

const matches = (q, ...fields) => !q || fields.some((f) => String(f || "").toLowerCase().includes(q));

// compareBase is the branch picked to compare with, for "branch:<name>".
const compareBase = () => (compare.startsWith("branch:") ? compare.slice(7) : "");

// branchView says what picking b reviews under the compare choice: src,
// the meta line's counts, and why it is disabled, if it is.
function branchView(b) {
  const cur = !b.remote && b.name === list.current;
  const at = `${list.dir}#`;
  if (compare === "upstream") {
    if (!b.upstream) return { src: "", off: "Not on origin" };
    const counts = b.unpushed >= 0 ? `${b.unpushed} unpushed${b.unpulled > 0 ? `, ${b.unpulled} behind ${esc(b.upstream)}` : ""}` : "";
    return { src: `${at}${b.upstream}...${b.name}`, counts, off: b.unpushed === 0 ? `No commits that ${b.upstream} doesn't have` : "" };
  }
  const base = compareBase();
  if (base) return { src: `${at}${base}...${b.name}`, counts: "", off: base === b.name ? "This is the branch compared with" : "" };
  // The checked-out branch opens as the bare path, working tree included.
  const wt = cur && worktree;
  const counts = b.ahead >= 0 ? `${b.ahead} ahead of ${esc(list.base || list.base_ref)}` : "";
  return { src: wt ? list.dir : `${at}${b.name}`, counts, wt, off: b.ahead === 0 && !wt ? `No commits ahead of ${list.base || list.base_ref}` : "" };
}

function branchRow(b) {
  const cur = !b.remote && b.name === list.current;
  const v = branchView(b);
  const chip = cur ? ` <span class="chip">checked out${v.wt ? " · includes working tree" : ""}</span>` : "";
  return `<button type="button" class="rev-item" data-src="${esc(v.src)}" ${v.off ? `disabled title="${esc(v.off)}"` : ""}>
    <span class="rev-name"><code>${esc(b.name)}</code>${chip}</span>
    <span class="rev-title">${esc(b.title)}</span>
    <span class="rev-meta">${v.counts ? `${v.counts} · ` : ""}${esc(b.author)} · ${ago(b.date)}</span></button>`;
}

function commitRow(c) {
  return `<button type="button" class="rev-item" data-src="${esc(`${list.dir}#${c.oid.slice(0, 12)}`)}">
    <span class="rev-name"><code>${esc(c.oid.slice(0, 8))}</code>${c.merge ? ` <span class="chip" title="Reviewed against its first parent">merge</span>` : ""}</span>
    <span class="rev-title">${esc(c.title)}</span>
    <span class="rev-meta">${c.merge ? "" : `${c.files} file${c.files === 1 ? "" : "s"} <span class="rev-add">+${c.additions}</span> <span class="rev-del">−${c.deletions}</span> · `}${esc(c.author)} · ${ago(c.date)}</span></button>`;
}

// renderCompare fills the branch tab's "Compare with" choices from the
// list; a branch picked before that is gone falls back to the default.
function renderCompare() {
  const dlg = $("#rev-picker");
  const sel = dlg.querySelector(".rev-base");
  if (compareBase() && !list.branches.some((b) => b.name === compareBase())) compare = "default";
  const base = list.base || (list.base_ref ? `origin/${list.base_ref}` : "the default branch");
  sel.innerHTML = `<option value="default">${esc(base)} (default branch)</option>
    <option value="upstream">its origin branch (unpushed commits)</option>
    <optgroup label="Another branch">${list.branches.map((b) => `<option value="branch:${esc(b.name)}">${esc(b.name)}</option>`).join("")}</optgroup>`;
  sel.value = compare;
  const wt = dlg.querySelector(".rev-worktree");
  wt.checked = worktree;
  wt.closest("label").hidden = compare !== "default" || !list.current;
}

function renderList() {
  const dlg = $("#rev-picker");
  const q = dlg.querySelector(".rev-filter").value.trim().toLowerCase();
  dlg.querySelectorAll(".rev-tabs button").forEach((b) => b.classList.toggle("on", b.dataset.tab === tab));
  dlg.querySelector(".rev-compare").hidden = tab !== "branches";
  if (tab === "branches") renderCompare();
  // Comparing with origin's copy only means something for local branches.
  const branches = compare === "upstream" ? list.branches.filter((b) => !b.remote) : list.branches;
  const rows = tab === "branches"
    ? branches.filter((b) => matches(q, b.name, b.title, b.author)).map(branchRow)
    : list.commits.filter((c) => matches(q, c.oid, c.title, c.author)).map(commitRow);
  dlg.querySelector(".rev-list").innerHTML = rows.join("") || `<p class="hint">Nothing matches.</p>`;
}

// status shows a line over the list, or hides it for "".
function status(text) {
  const el = $("#rev-picker .rev-status");
  el.textContent = text;
  el.hidden = !text;
}

// show puts a listed checkout in the dialog.
function show(next) {
  const dlg = $("#rev-picker");
  list = next;
  dlg.querySelector(".rev-dir").textContent = list.repo ? `${list.repo} · ${list.dir}` : list.dir;
  renderList();
  dlg.querySelector(".rev-filter").focus();
}

// offerClone says the repository has no checkout here, and clones it into
// the workspace when asked.
function offerClone(res) {
  const dlg = $("#rev-picker");
  const body = dlg.querySelector(".rev-list");
  dlg.querySelector(".rev-dir").textContent = res.repo;
  body.innerHTML = `<div class="rev-clone"><p class="hint"><b>${esc(res.repo)}</b> isn't cloned on this computer, in the code directory or the app's workspace. Clone it to pick a branch or commit in it; reviews then read that clone.</p>
    <p class="hint">Into <code>${esc(res.clone_to)}</code></p>
    <button type="button" class="primary rev-clone-go">Clone</button></div>`;
  body.querySelector(".rev-clone-go").onclick = async (e) => {
    e.target.disabled = true;
    e.target.setAttribute("aria-busy", "true");
    status(`Cloning ${res.repo}… a large repository takes a while.`);
    try {
      show(await postJSON("/api/revs/clone", { path: box, ...codeSources() }));
      status("");
    } catch (err) {
      status(`Clone failed: ${err.message}`);
      e.target.disabled = false;
      e.target.removeAttribute("aria-busy");
    }
  };
}

async function open() {
  const dlg = $("#rev-picker");
  const body = dlg.querySelector(".rev-list");
  box = $("#url").value.trim();
  dlg.querySelector(".rev-filter").value = "";
  dlg.querySelector(".rev-dir").textContent = "";
  dlg.querySelector(".rev-compare").hidden = true;
  if (!dlg.open) dlg.showModal();
  list = null;
  status("");
  if (!box) {
    body.innerHTML = `<p class="hint">Type a repository path, a GitHub or GitLab URL, or owner/repo in the box first, then pick a commit or branch in it.</p>`;
    return;
  }
  body.innerHTML = `<p class="hint">loading…</p>`;
  try {
    const res = await api(`/api/revs?${new URLSearchParams({ path: box, ...codeSources() })}`);
    if (!res.dir) return offerClone(res);
    show(res);
    // A repository named by URL is the remote's: fetch it so its branches
    // are origin's as they are now.
    if (res.repo) refresh();
  } catch (e) {
    body.innerHTML = `<div class="error">${esc(e.message)}</div>`;
  }
}

// refresh fetches origin and lists the checkout again. The filter and
// tab stay; a failed fetch still shows the local history, with why.
async function refresh() {
  const dlg = $("#rev-picker");
  const btn = dlg.querySelector(".rev-refresh");
  if (!list || btn.disabled) return;
  btn.disabled = true;
  btn.setAttribute("aria-busy", "true");
  status("Fetching origin…");
  try {
    const next = await api(`/api/revs?${new URLSearchParams({ path: list.dir, fetch: "1" })}`);
    show({ ...next, repo: list.repo });
    status(list.fetch_error ? `Showing local history: ${list.fetch_error}` : "");
  } catch (e) {
    status(`Refresh failed: ${e.message}`);
  } finally {
    btn.disabled = false;
    btn.removeAttribute("aria-busy");
  }
}

// initRevPicker wires the header button and the dialog.
export function initRevPicker() {
  const dlg = $("#rev-picker");
  $("#rev-btn").onclick = open;
  dlg.querySelector(".rev-refresh").onclick = refresh;
  dlg.querySelector(".rev-filter").addEventListener("input", () => list && renderList());
  dlg.querySelector(".rev-filter").addEventListener("keydown", (e) => {
    if (e.key !== "Enter") return;
    e.preventDefault();
    dlg.querySelector(".rev-item:not([disabled])")?.click();
  });
  dlg.querySelector(".rev-tabs").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-tab]");
    if (!b || !list) return;
    tab = b.dataset.tab;
    localStorage.setItem("pr-manager.revtab", tab);
    renderList();
  });
  dlg.querySelector(".rev-base").addEventListener("change", (e) => {
    compare = e.target.value;
    localStorage.setItem("pr-manager.revcompare", compare);
    if (list) renderList();
  });
  dlg.querySelector(".rev-worktree").addEventListener("change", (e) => {
    worktree = e.target.checked;
    localStorage.setItem("pr-manager.revworktree", worktree ? "1" : "0");
    if (list) renderList();
  });
  dlg.querySelector(".rev-list").addEventListener("click", (e) => {
    const b = e.target.closest(".rev-item");
    if (!b || b.disabled) return;
    dlg.close();
    triageURL(b.dataset.src);
  });
}
