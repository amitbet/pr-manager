// Small helpers shared by every component.

// The project was called pr-triage. Carry its saved settings over once.
// util.js has no imports, so this runs before any module reads a key.
for (const k of Object.keys(localStorage)) {
  if (!k.startsWith("pr-triage.")) continue;
  const n = "pr-manager." + k.slice("pr-triage.".length);
  if (localStorage.getItem(n) === null) localStorage.setItem(n, localStorage.getItem(k));
  localStorage.removeItem(k);
}

// The desktop app's webview ignores target="_blank", so a link out of the
// app goes to the system browser through the Wails runtime instead.
document.addEventListener("click", (e) => {
  const a = e.target.closest?.('a[target="_blank"]');
  if (!a || !window.runtime?.BrowserOpenURL || !/^https?:/.test(a.href)) return;
  e.preventDefault();
  window.runtime.BrowserOpenURL(a.href);
}, true);

export const $ = (s) => document.querySelector(s);
export const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

// kindBadge is the colored icon square before a title saying what was
// triaged: a PR, a local repo's working tree, a branch in it or one commit.
// A PR's is colored by its state (open, merged or closed).
const KIND_ICON = {
  pr: `<circle cx="18" cy="18" r="3"/><circle cx="6" cy="6" r="3"/><path d="M13 6h3a2 2 0 0 1 2 2v7M6 9v12"/>`,
  repo: `<path d="M4 20h16a2 2 0 0 0 2-2V8a2 2 0 0 0-2-2h-7.9a2 2 0 0 1-1.7-.9l-.8-1.2A2 2 0 0 0 7.9 3H4a2 2 0 0 0-2 2v13a2 2 0 0 0 2 2z"/><circle cx="12" cy="13" r="2"/><path d="M7 13h3M14 13h3"/>`,
  branch: `<circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M6 3v12M18 9a9 9 0 0 1-9 9"/>`,
  commit: `<circle cx="12" cy="12" r="3"/><path d="M3 12h6M15 12h6"/>`,
};
export const KIND_TITLE = { pr: "pull request", repo: "working tree", branch: "local branch", commit: "local commit" };
export const kindOf = (p) => (!p.local_path ? "pr" : p.single_commit ? "commit" : p.rev ? "branch" : "repo");
export const prState = (p) => (kindOf(p) === "pr" && p.state ? p.state.toLowerCase() : "");
export const kindBadge = (p) => {
  const k = kindOf(p);
  const st = prState(p);
  const label = st ? `${st} ${KIND_TITLE[k]}` : KIND_TITLE[k];
  return `<span class="kind ${k}" data-state="${esc(st)}" title="${esc(label)}" aria-label="${esc(label)}"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">${KIND_ICON[k]}</svg></span>`;
};

export const BUCKETS = ["human", "skim", "aux", "none"];
export const LABEL = { human: "human review", skim: "skim", aux: "auxiliary", none: "no review" };

export async function api(path, opts) {
  const r = await fetch(path, opts);
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw Object.assign(new Error(body.error || r.statusText), { status: r.status, code: body.code });
  if (opts?.method && opts.method !== "GET" && !NOT_USE.test(path)) used(path.match(/^\/api\/(?:results|local)\/([^/?]+)/)?.[1]);
  return body;
}
// used tells the sidebar a person changed something on a result: its key,
// or the open result's when not given. A change through api counts, except
// what the app asks for by itself (translations, the overview and sequence,
// triage jobs and their cancels, the code map, saved chats, the PR watch).
export const USED_EVENT = "pr-manager:used";
const NOT_USE = /\/(translate|overview|sequence)$|^\/api\/(triage|jobs|codemap|chats|prwatch)\b/;
export const used = (key) => document.dispatchEvent(new CustomEvent(USED_EVENT, { detail: key && decodeURIComponent(key) }));
export const postJSON = (url, v) => api(url, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(v) });

export function pills(c) {
  return BUCKETS.map((b) => `<span class="pill ${b}" title="${LABEL[b]}">${c?.[b] || 0}</span>`).join("");
}

// firstSentence trims long model text down to its opening sentence.
function firstSentence(t) {
  const m = String(t || "").match(/^[\s\S]*?[.!?](?=\s|$)/);
  return (m ? m[0] : String(t || "")).trim();
}

// headline is the one line shown per unit: the summarizer's headline, else
// the first sentence of its summary, else the classifier's reason.
export function headline(u) {
  return u.headline || u.decision.headline || firstSentence(u.summary) || firstSentence(u.decision.reason) || "(no description)";
}

// ask, say and askText stand in for confirm, alert and prompt, which the
// desktop app's macOS webview (Wails) doesn't implement: there confirm
// answers false without showing anything, alert shows nothing and prompt
// returns null. ask resolves true for OK; askText resolves the text, or
// null when cancelled.
function dialog(message, { ok = "OK", cancel = "Cancel", input = null } = {}) {
  const dlg = document.createElement("dialog");
  dlg.className = "settings-dlg ask-dlg";
  dlg.innerHTML = `<form method="dialog"><p class="ask-msg">${esc(message)}</p>
    ${input === null ? "" : `<input class="ask-input" spellcheck="false" value="${esc(input)}">`}
    <div class="dlg-actions">${cancel ? `<button value="">${esc(cancel)}</button> ` : ""}<button class="primary" value="ok">${esc(ok)}</button></div></form>`;
  document.body.append(dlg);
  const field = dlg.querySelector(".ask-input");
  return new Promise((resolve) => {
    dlg.addEventListener("close", () => {
      resolve(dlg.returnValue === "ok" ? (field ? field.value : true) : (field ? null : false));
      dlg.remove();
    });
    // Enter in the field submits as OK, not as the first button (Cancel).
    field?.addEventListener("keydown", (e) => { if (e.key === "Enter") { e.preventDefault(); dlg.close("ok"); } });
    dlg.showModal();
    (field || dlg.querySelector(".primary")).focus();
  });
}
export const ask = (message, ok = "OK") => dialog(message, { ok });
export const say = (message) => dialog(message, { cancel: "" });
export const askText = (message, value = "") => dialog(message, { input: value });
