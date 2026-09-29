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

export const BUCKETS = ["human", "skim", "none"];
export const LABEL = { human: "human review", skim: "skim", none: "no review" };

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
