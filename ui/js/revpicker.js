// Git object picker: lists a checkout's branches and recent commits and
// triages the one picked as path#rev. The box may hold the checkout's
// path, or a repository's URL or owner/repo: its checkout in the code
// directory or the workspace is used, and one without a checkout is
// listed from GitHub or GitLab, without cloning, its picks named on the
// repository's page (https://github.com/acme/api#feature).
//
// The Base picks what the changes are compared with, for commits and
// branches alike: automatic (a commit's parent, a branch's fork from the
// default branch), the default branch, a branch's origin branch, a
// fork's upstream, or any commit or branch picked from the list or from
// the upstream's. With a base, a pick is reviewed as base...pick: from
// where the two meet, which for a fork's branch and its upstream is the
// commit it was forked from.
import { $, esc, api, postJSON } from "./util.js";
import { triageURL } from "./triage.js";
import { codeSources } from "./settings.js";

let list = null; // the repository's revList
let box = ""; // what the box held when the picker opened
let tab = localStorage.getItem("pr-manager.revtab") || "commits";
// mode is "auto", "default", "origin" (each branch's origin branch),
// "upstream" (the fork's upstream's default branch) or "ref" (picked);
// a picked base belongs to its repository, so it isn't kept.
let mode = localStorage.getItem("pr-manager.revbase") || "auto";
let base = null; // {ref, label}: what reviews name the base by, for "upstream" and "ref"
let picking = false; // clicks pick the base
let upList = null; // the upstream's list, while picking from it
let parent = null; // the fork's upstream, once known
let worktree = localStorage.getItem("pr-manager.revworktree") !== "0";

const ago = (iso) => {
  const s = (Date.now() - new Date(iso)) / 1000;
  for (const [n, u] of [[86400 * 365, "y"], [86400 * 30, "mo"], [86400, "d"], [3600, "h"], [60, "m"]]) if (s >= n) return `${Math.floor(s / n)}${u} ago`;
  return "just now";
};

const matches = (q, ...fields) => !q || fields.some((f) => String(f || "").toLowerCase().includes(q));
const isSHA = (ref) => /^[0-9a-f]{40}$/.test(ref);
const short = (ref) => (isSHA(ref) ? ref.slice(0, 10) : ref);
const at = () => `${list.remote ? list.web : list.dir}#`;

// token is the base picks are compared with, or "" for automatic and a
// branch's own origin branch.
function token() {
  if (mode === "default") return list.base;
  if (mode === "upstream" || mode === "ref") return base?.ref || "";
  return "";
}

// counted says whether the server counted the branches from the base in
// use (countFrom).
const counted = (b) => b.ahead >= 0 && !!list.compared && list.compared === token();

function branchView(b) {
  const cur = !b.remote && b.name === list.current;
  if (mode === "origin") {
    if (!b.upstream) return { src: "", off: b.remote ? "This is origin's branch" : "Not on origin" };
    const counts = b.unpushed >= 0 ? `${b.unpushed} unpushed${b.unpulled > 0 ? `, ${b.unpulled} behind ${esc(b.upstream)}` : ""}` : "";
    return { src: `${at()}${b.upstream}...${b.name}`, counts, off: b.unpushed === 0 ? `No commits that ${b.upstream} doesn't have` : "" };
  }
  const t = token();
  if (t) {
    const counts = counted(b) ? `${b.ahead} ahead of ${esc(short(t))}${b.fork ? ` · left it at <code>${esc(b.fork.slice(0, 8))}</code>` : ""}` : "";
    const off = t === b.name ? "This is the base" : counted(b) && b.ahead === 0 ? `No commits that ${short(t)} doesn't have` : "";
    return { src: `${at()}${t}...${b.name}`, counts, off };
  }
  // Automatic: a branch with an open PR opens as the PR, the checked-out
  // branch as the bare path, working tree included.
  const own = b.ahead >= 0 && !list.compared;
  const counts = own ? `${b.ahead} ahead of ${esc(list.base || list.base_ref)}` : "";
  if (b.pr) return { src: b.pr, counts, off: "" };
  const wt = cur && worktree;
  return { src: wt ? list.dir : `${at()}${b.name}`, counts, wt, off: own && b.ahead === 0 && !wt ? `No commits ahead of ${list.base || list.base_ref}` : "" };
}

function branchRow(b) {
  const cur = !b.remote && b.name === list.current;
  const v = branchView(b);
  const chip = cur ? ` <span class="chip">checked out${v.wt ? " · includes working tree" : ""}</span>`
    : b.pr && mode === "auto" ? ` <span class="chip" title="Opens the PR">PR #${b.pr_number}</span>` : "";
  return `<button type="button" class="rev-item" data-src="${esc(v.src)}" ${v.off ? `disabled title="${esc(v.off)}"` : ""}>
    <span class="rev-name"><code>${esc(b.name)}</code>${chip}</span>
    <span class="rev-title">${esc(b.title)}</span>
    <span class="rev-meta">${v.counts ? `${v.counts} · ` : ""}${esc(b.author)} · ${ago(b.date)}</span></button>`;
}

function commitBody(c, mergeChip) {
  const files = c.files >= 0 ? `${c.files} file${c.files === 1 ? "" : "s"} ` : "";
  return `<span class="rev-name"><code>${esc(c.oid.slice(0, 8))}</code>${c.merge && mergeChip ? ` <span class="chip" title="Reviewed against its first parent">merge</span>` : ""}</span>
    <span class="rev-title">${esc(c.title)}</span>
    <span class="rev-meta">${c.merge ? "" : `${files}<span class="rev-add">+${c.additions}</span> <span class="rev-del">−${c.deletions}</span> · `}${esc(c.author)} · ${ago(c.date)}</span>`;
}

function commitRow(c) {
  const t = mode === "origin" ? "" : token(); // a commit has no origin branch: its parent
  const oid = list.remote || t ? c.oid : c.oid.slice(0, 12);
  const src = t ? `${at()}${t}...${oid}` : `${at()}${oid}`;
  const off = t && t === c.oid ? `disabled title="This is the base"` : "";
  return `<button type="button" class="rev-item" data-src="${esc(src)}" ${off}>${commitBody(c, !t)}</button>`;
}

// pickRow is a row that picks the base: a branch or commit of the list,
// or of the upstream's when up is set.
function pickRow(item, branch, up) {
  const ref = branch ? item.name : item.oid;
  const label = `${up ? `${parent.repo} ` : ""}${branch ? item.name : `${item.oid.slice(0, 8)} ${item.title}`}`;
  const body = branch ? `<span class="rev-name"><code>${esc(item.name)}</code></span><span class="rev-title">${esc(item.title)}</span>
    <span class="rev-meta">${esc(item.author)} · ${ago(item.date)}</span>` : commitBody(item, false);
  return `<button type="button" class="rev-item" data-ref="${esc(ref)}" data-up="${up ? 1 : ""}" data-label="${esc(label)}">${body}</button>`;
}

function renderBase() {
  const dlg = $("#rev-picker");
  const sel = dlg.querySelector(".rev-base");
  const def = list.base || (list.base_ref ? `origin/${list.base_ref}` : "the default branch");
  const opt = (v, text) => `<option value="${v}">${esc(text)}</option>`;
  sel.innerHTML = [
    opt("auto", `Automatic: a commit's parent, a branch from ${def}`),
    opt("default", `${def} (default branch)`),
    list.remote ? "" : opt("origin", "Each branch's origin branch (unpushed commits)"),
    parent ? opt("upstream", `Upstream ${parent.repo}: ${parent.default}`) : "",
    mode === "ref" && base ? opt("ref", `Picked: ${base.label}`) : "",
    opt("pick", "Pick a commit or branch below…"),
    parent?.web ? opt("pickup", `Pick from upstream ${parent.repo}…`) : "",
  ].join("");
  sel.value = picking ? (upList ? "pickup" : "pick") : mode;
  const wt = dlg.querySelector(".rev-worktree");
  wt.checked = worktree;
  wt.closest("label").hidden = picking || mode !== "auto" || !list.current || tab !== "branches";
}

function renderList() {
  const dlg = $("#rev-picker");
  const q = dlg.querySelector(".rev-filter").value.trim().toLowerCase();
  dlg.querySelectorAll(".rev-tabs button").forEach((b) => b.classList.toggle("on", b.dataset.tab === tab));
  dlg.querySelector(".rev-compare").hidden = false;
  dlg.querySelector(".rev-list").classList.toggle("picking", picking);
  renderBase();
  let rows;
  if (picking) {
    const from = upList || list;
    rows = tab === "branches"
      ? from.branches.filter((b) => matches(q, b.name, b.title, b.author)).map((b) => pickRow(b, true, !!upList))
      : from.commits.filter((c) => matches(q, c.oid, c.title, c.author)).map((c) => pickRow(c, false, !!upList));
  } else {
    // Comparing with origin's copy only means something for local branches.
    const branches = mode === "origin" ? list.branches.filter((b) => !b.remote) : list.branches;
    rows = tab === "branches"
      ? branches.filter((b) => matches(q, b.name, b.title, b.author)).map(branchRow)
      : list.commits.filter((c) => matches(q, c.oid, c.title, c.author)).map(commitRow);
  }
  dlg.querySelector(".rev-list").innerHTML = rows.join("") || `<p class="hint">Nothing matches.</p>`;
}

// status shows a line over the list, or hides it for "".
function status(text) {
  const el = $("#rev-picker .rev-status");
  el.textContent = text;
  el.hidden = !text;
}

// show puts a listed repository in the dialog.
function show(next) {
  const dlg = $("#rev-picker");
  list = next;
  if (list.parent) parent = list.parent;
  dlg.querySelector(".rev-dir").textContent = list.remote ? `${list.repo} · listed from ${list.web.split("/")[2]}, not cloned` : list.repo ? `${list.repo} · ${list.dir}` : list.dir;
  renderList();
  dlg.querySelector(".rev-filter").focus();
}

// relist lists the repository again: fetching origin first for fetch, and
// with a checkout's branches counted from the base in use.
async function relist(fetch) {
  const t = mode === "origin" ? "" : token();
  const q = list.remote ? { path: box, ...codeSources() } : { path: list.dir, ...(fetch ? { fetch: "1" } : {}), ...(t ? { base: t } : {}) };
  const next = await api(`/api/revs?${new URLSearchParams(q)}`);
  show({ ...next, repo: list.repo });
}

// setMode switches the base. An upstream branch or commit is fetched into
// a checkout first; a list from the forge has its picks fetched when one
// is reviewed.
async function setMode(next, picked) {
  const prev = { mode, base };
  picking = false;
  upList = null;
  mode = next;
  if (next !== "ref") localStorage.setItem("pr-manager.revbase", next);
  try {
    if (next === "ref") {
      base = picked;
      if (picked.up && !list.remote) {
        status(`Fetching ${short(picked.ref)} from ${parent.repo}…`);
        base = { ...picked, ref: (await postJSON("/api/revs/upstream", { path: list.dir, ref: picked.ref })).base };
      } else if (picked.up && !isSHA(picked.ref)) {
        base = { ...picked, ref: `upstream/${picked.ref}` };
      }
    } else if (next === "upstream") {
      const label = `${parent.repo} ${parent.default}`;
      if (!list.remote) {
        status(`Fetching ${parent.default} from ${parent.repo}…`);
        base = { ref: (await postJSON("/api/revs/upstream", { path: list.dir, ref: "" })).base, label };
      } else base = { ref: `upstream/${parent.default}`, label };
    }
    if (!list.remote) {
      status("Counting…");
      await relist(false);
    }
    status("");
  } catch (e) {
    ({ mode, base } = prev);
    status(`Can't compare with that: ${e.message}`);
  }
  renderList();
}

// startPicking makes clicks pick the base, from the list or the
// upstream's.
async function startPicking(up) {
  picking = true;
  upList = null;
  if (up) {
    status(`Listing ${parent.repo}…`);
    try {
      upList = await api(`/api/revs?${new URLSearchParams({ path: parent.web, remote: "1" })}`);
    } catch (e) {
      picking = false;
      status(`Can't list ${parent.repo}: ${e.message}`);
      renderList();
      return;
    }
  }
  status(`Click the commit or branch${up ? ` of ${parent.repo}` : ""} to compare with. Esc cancels.`);
  renderList();
}

function stopPicking() {
  picking = false;
  upList = null;
  status("");
  renderList();
}

// findParent asks whether a checkout is a fork, for the upstream
// choices; a list from the forge says so itself.
async function findParent() {
  if (!list || list.remote) return;
  const dir = list.dir;
  try {
    parent = (await api(`/api/revs/parent?${new URLSearchParams({ path: dir })}`)).parent;
  } catch {
    parent = null;
  }
  if (list?.dir !== dir) return;
  if (mode === "upstream") setMode(parent ? "upstream" : "auto");
  else renderList();
}

async function open() {
  const dlg = $("#rev-picker");
  const body = dlg.querySelector(".rev-list");
  box = $("#url").value.trim();
  dlg.querySelector(".rev-filter").value = "";
  dlg.querySelector(".rev-dir").textContent = "";
  dlg.querySelector(".rev-compare").hidden = true;
  if (!dlg.open) dlg.showModal();
  list = parent = base = upList = null;
  picking = false;
  if (mode === "ref") mode = "auto";
  status("");
  if (!box) {
    body.innerHTML = `<p class="hint">Type a repository path, a GitHub or GitLab URL, or owner/repo in the box first, then pick a commit or branch in it.</p>`;
    return;
  }
  body.innerHTML = `<p class="hint">loading…</p>`;
  try {
    const res = await api(`/api/revs?${new URLSearchParams({ path: box, ...codeSources() })}`);
    // The upstream base waits for the upstream to be known.
    const want = mode;
    if (want === "upstream") mode = "auto";
    show(res);
    // A checkout found for a repository named by URL is fetched, so its
    // branches are origin's as they are now. A list from the forge is.
    if (res.repo && !res.remote) await refresh();
    if (want === "upstream" && res.remote && parent) await setMode("upstream");
    else if (want === "default" && !res.remote) await setMode("default");
    else if (want === "upstream") mode = "upstream"; // findParent settles it
    findParent();
  } catch (e) {
    body.innerHTML = `<div class="error">${esc(e.message)}</div>`;
  }
}

// refresh fetches origin and lists the checkout again. The filter, tab and
// base stay; a failed fetch still shows the local history, with why.
async function refresh() {
  const dlg = $("#rev-picker");
  const btn = dlg.querySelector(".rev-refresh");
  if (!list || btn.disabled) return;
  btn.disabled = true;
  btn.setAttribute("aria-busy", "true");
  status(list.remote ? `Listing ${list.repo}…` : "Fetching origin…");
  try {
    await relist(true);
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
  // Esc while picking the base stops picking rather than closing.
  dlg.addEventListener("cancel", (e) => {
    if (!picking) return;
    e.preventDefault();
    stopPicking();
  });
  dlg.querySelector(".rev-tabs").addEventListener("click", (e) => {
    const b = e.target.closest("button[data-tab]");
    if (!b || !list) return;
    tab = b.dataset.tab;
    localStorage.setItem("pr-manager.revtab", tab);
    renderList();
  });
  dlg.querySelector(".rev-base").addEventListener("change", (e) => {
    if (!list) return;
    const v = e.target.value;
    if (v === "pick" || v === "pickup") startPicking(v === "pickup");
    else if (v === "ref") stopPicking();
    else setMode(v);
  });
  dlg.querySelector(".rev-worktree").addEventListener("change", (e) => {
    worktree = e.target.checked;
    localStorage.setItem("pr-manager.revworktree", worktree ? "1" : "0");
    if (list) renderList();
  });
  dlg.querySelector(".rev-list").addEventListener("click", (e) => {
    const b = e.target.closest(".rev-item");
    if (!b || b.disabled) return;
    if (picking) {
      setMode("ref", { ref: b.dataset.ref, label: b.dataset.label, up: !!b.dataset.up });
      return;
    }
    dlg.close();
    triageURL(b.dataset.src);
  });
}
