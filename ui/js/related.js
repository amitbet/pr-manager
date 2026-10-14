// Related PRs (chatrelated.go): other PRs, in this repository or another,
// connected to the change on screen. The header shows "Related (n)";
// hovering it, or clicking to keep it open, shows them, each with a link
// to the PR and a button to review it here, and a box to link another
// (a PR link or owner/repo#N, and why). The chat agent links and unlinks
// them with link_pr and unlink_pr, through link and unlink here. Links go
// both ways: the other PR shows this one too.
import { esc, api, say } from "./util.js";
import { S, render } from "./state.js";
import { changeOf } from "./agentapi.js";
import { triageURL } from "./triage.js";

const lists = new Map(); // change -> its related PRs, as loaded
const loading = new Set();
let pinned = false; // the popup kept open by a click
let flip = false; // the popup opens leftward, the button being too near the window's right edge

const relURL = (change, extra = "") => `/api/related?change=${encodeURIComponent(change)}&key=${encodeURIComponent(S.result?.key || "")}${extra}`;

// related is the change's related PRs, loading them once.
export function related(change = changeOf(S.result)) {
  if (!change) return [];
  if (!lists.has(change) && !loading.has(change)) {
    loading.add(change);
    api(relURL(change)).then((r) => { lists.set(change, r.prs || []); }, () => { lists.set(change, []); })
      .finally(() => { loading.delete(change); if (changeOf(S.result) === change) render(); });
  }
  return lists.get(change) || [];
}

// link connects a PR to the change on screen; by is "reader" or "agent".
export async function link(url, note = "", by = "reader") {
  const change = changeOf(S.result);
  const res = await api(relURL(change), { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ url, note, by }) });
  lists.set(change, (await api(relURL(change))).prs || []);
  lists.delete(res.change); // its link back: loaded again when it is opened
  render();
  return res;
}

export async function unlink(url) {
  const change = changeOf(S.result);
  const was = lists.get(change) || [];
  await api(relURL(change, `&url=${encodeURIComponent(url)}`), { method: "DELETE" });
  const now = (await api(relURL(change))).prs || [];
  lists.set(change, now);
  for (const p of was) if (!now.some((q) => q.change === p.change)) lists.delete(p.change); // its link back
  render();
}

const BY = { reader: "linked by you", agent: "linked by the chat agent", backlink: "linked from that PR" };

// relatedHTML is the header's chip and its popup.
export function relatedHTML(r) {
  if (!r?.pr) return "";
  const prs = related(changeOf(r));
  const items = prs.map((p) => `
    <li>
      <a href="${esc(p.url)}" target="_blank" rel="noopener" title="Open on GitHub"><code>${esc(p.change.replace(/^github\.com\//, ""))}</code></a>
      <span class="rel-actions"><button class="linkbtn" data-act="rel-open" data-url="${esc(p.url)}" title="Review it here">review</button>
      <button class="linkbtn" data-act="rel-del" data-url="${esc(p.url)}" title="Unlink it, here and on that PR">unlink</button></span>
      ${p.title ? `<span class="rel-title" title="${esc(p.title)}">${esc(p.title)}</span>` : ""}
      <div class="rel-why">${p.note ? `${esc(p.note)} · ` : ""}${BY[p.by] || BY.reader}</div>
    </li>`).join("");
  return `<span class="rel-wrap${pinned ? " pinned" : ""}${flip ? " flip" : ""}">
    <button type="button" class="rel-chip${prs.length ? " has" : ""}" data-act="rel-pin" aria-expanded="${pinned}" title="PRs connected to this change, in this repository or another: the chat agent reads them with this one">${prs.length ? `Related (${prs.length})` : "+ Related PR"}</button>
    <div class="rel-pop" role="dialog" aria-label="Related PRs">
      ${prs.length ? `<ul>${items}</ul>` : `<p class="rel-empty">No related PRs. Link the other half of this change, a PR it depends on, or the same change in another repository; the chat agent reads them with this one. You can also ask the agent to find them.</p>`}
      <div class="rel-add">
        <input id="rel-url" type="text" placeholder="PR link or owner/repo#N" aria-label="Related PR" spellcheck="false">
        <input id="rel-note" type="text" placeholder="Why it is related (optional)" aria-label="Why it is related">
        <button class="primary" data-act="rel-add">Link</button>
      </div>
    </div>
  </span>`;
}

async function addFromForm() {
  const url = document.getElementById("rel-url")?.value.trim();
  const note = document.getElementById("rel-note")?.value.trim() || "";
  if (!url) { document.getElementById("rel-url")?.focus(); return; }
  const btn = document.querySelector('[data-act="rel-add"]');
  if (btn) { btn.disabled = true; btn.textContent = "Linking…"; }
  try { pinned = true; await link(url, note); } catch (e) { say(e.message); if (btn) { btn.disabled = false; btn.textContent = "Link"; } }
}

export const actions = {
  "rel-pin": () => { pinned = !pinned; if (pinned) setTimeout(() => document.getElementById("rel-url")?.focus(), 0); },
  "rel-add": () => { addFromForm(); return false; },
  "rel-del": (el) => { unlink(el.dataset.url).catch((e) => say(e.message)); return false; },
  "rel-open": (el) => { pinned = false; triageURL(el.dataset.url); },
};

// onKeydown: Enter in the boxes links, Esc closes the popup.
export function onKeydown(e) {
  if (e.target.id !== "rel-url" && e.target.id !== "rel-note") return;
  if (e.key === "Enter") { e.preventDefault(); addFromForm(); }
  if (e.key === "Escape") { pinned = false; render(); }
}

// place opens the popup leftward when it wouldn't fit to the right.
function place(wrap) {
  const pop = wrap.querySelector(".rel-pop");
  const width = pop ? Math.min(420, innerWidth * 0.8) : 420;
  flip = wrap.getBoundingClientRect().left + width + 12 > innerWidth;
  wrap.classList.toggle("flip", flip);
}
for (const ev of ["mouseover", "focusin"]) {
  document.addEventListener(ev, (e) => { const w = e.target.closest?.(".rel-wrap"); if (w) place(w); });
}

// A click outside closes a pinned popup.
document.addEventListener("click", (e) => {
  if (pinned && !e.target.closest(".rel-wrap")) { pinned = false; render(); }
});
