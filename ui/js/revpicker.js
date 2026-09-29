// Git object picker: lists a local checkout's branches and recent commits
// and triages the one picked as path#rev.
import { $, esc, api } from "./util.js";
import { isLocalPath, triageURL } from "./triage.js";

let list = null; // the checkout's revList
let tab = localStorage.getItem("pr-manager.revtab") || "commits";

const ago = (iso) => {
  const s = (Date.now() - new Date(iso)) / 1000;
  for (const [n, u] of [[86400 * 365, "y"], [86400 * 30, "mo"], [86400, "d"], [3600, "h"], [60, "m"]]) if (s >= n) return `${Math.floor(s / n)}${u} ago`;
  return "just now";
};

const matches = (q, ...fields) => !q || fields.some((f) => String(f || "").toLowerCase().includes(q));

function branchRow(b) {
  const cur = !b.remote && b.name === list.current;
  const empty = b.ahead === 0 && !cur;
  // The checked-out branch opens as the bare path, working tree included.
  const src = cur ? list.dir : `${list.dir}#${b.name}`;
  const ahead = b.ahead >= 0 ? `${b.ahead} ahead of ${esc(list.base_ref)}` : "";
  return `<button type="button" class="rev-item" data-src="${esc(src)}" ${empty ? `disabled title="No commits ahead of ${esc(list.base_ref)}"` : ""}>
    <span class="rev-name"><code>${esc(b.name)}</code>${cur ? ` <span class="chip">checked out · includes working tree</span>` : ""}</span>
    <span class="rev-title">${esc(b.title)}</span>
    <span class="rev-meta">${ahead ? `${ahead} · ` : ""}${esc(b.author)} · ${ago(b.date)}</span></button>`;
}

function commitRow(c) {
  return `<button type="button" class="rev-item" data-src="${esc(`${list.dir}#${c.oid.slice(0, 12)}`)}">
    <span class="rev-name"><code>${esc(c.oid.slice(0, 8))}</code>${c.merge ? ` <span class="chip" title="Reviewed against its first parent">merge</span>` : ""}</span>
    <span class="rev-title">${esc(c.title)}</span>
    <span class="rev-meta">${c.merge ? "" : `${c.files} file${c.files === 1 ? "" : "s"} <span class="rev-add">+${c.additions}</span> <span class="rev-del">−${c.deletions}</span> · `}${esc(c.author)} · ${ago(c.date)}</span></button>`;
}

function renderList() {
  const dlg = $("#rev-picker");
  const q = dlg.querySelector(".rev-filter").value.trim().toLowerCase();
  dlg.querySelectorAll(".rev-tabs button").forEach((b) => b.classList.toggle("on", b.dataset.tab === tab));
  const rows = tab === "branches"
    ? list.branches.filter((b) => matches(q, b.name, b.title, b.author)).map(branchRow)
    : list.commits.filter((c) => matches(q, c.oid, c.title, c.author)).map(commitRow);
  dlg.querySelector(".rev-list").innerHTML = rows.join("") || `<p class="hint">Nothing matches.</p>`;
}

async function open() {
  const dlg = $("#rev-picker");
  const body = dlg.querySelector(".rev-list");
  const path = $("#url").value.trim();
  dlg.querySelector(".rev-filter").value = "";
  if (!dlg.open) dlg.showModal();
  list = null;
  if (!path || !isLocalPath(path)) {
    body.innerHTML = `<p class="hint">Enter a local repository path first, then pick a commit or branch in it.</p>`;
    return;
  }
  body.innerHTML = `<p class="hint">loading…</p>`;
  try {
    list = await api(`/api/revs?${new URLSearchParams({ path })}`);
    dlg.querySelector(".rev-dir").textContent = list.dir;
    renderList();
    dlg.querySelector(".rev-filter").focus();
  } catch (e) {
    body.innerHTML = `<div class="error">${esc(e.message)}</div>`;
  }
}

// initRevPicker wires the header button and the dialog.
export function initRevPicker() {
  const dlg = $("#rev-picker");
  $("#rev-btn").onclick = open;
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
  dlg.querySelector(".rev-list").addEventListener("click", (e) => {
    const b = e.target.closest(".rev-item");
    if (!b || b.disabled) return;
    dlg.close();
    triageURL(b.dataset.src);
  });
}
