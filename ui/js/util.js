// Small helpers shared by every component.

// The project was called pr-triage. Carry its saved settings over once.
// util.js has no imports, so this runs before any module reads a key.
for (const k of Object.keys(localStorage)) {
  if (!k.startsWith("pr-triage.")) continue;
  const n = "pr-manager." + k.slice("pr-triage.".length);
  if (localStorage.getItem(n) === null) localStorage.setItem(n, localStorage.getItem(k));
  localStorage.removeItem(k);
}

export const $ = (s) => document.querySelector(s);
export const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

export const BUCKETS = ["human", "skim", "aux", "none"];
export const LABEL = { human: "human review", skim: "skim", aux: "auxiliary", none: "no review" };

export async function api(path, opts) {
  const r = await fetch(path, opts);
  const body = await r.json().catch(() => ({}));
  if (!r.ok) throw Object.assign(new Error(body.error || r.statusText), { status: r.status, code: body.code });
  return body;
}
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
