// What the reader points at, for the chat agent: the text they selected
// on the page (in a diff, with its file and lines), and the text boxes
// open on it, with what they say, which the agent can fill (fill_field in
// agentapi.js). main.js sends both with every question (chatWhere).
//
// A selection is kept until the reader clicks in the page again: clicking
// into the chat's prompt, or a render of the page, doesn't lose it. A
// change fires prm-view, so the chat's "Looking at" line follows it.
import { S, render, fileByPath } from "./state.js";
import { panelOpen, openPanel, setReviewBody } from "./panel.js";
import { jumpToUnit } from "./review.js";

const SEL_MAX = 8000;
const FIELD_IDS = { "composer-text": "comment", "dismiss-why": "dismiss_reason", "rv-body": "review_summary" };

let sel = null;   // the last selection in the page, as chatSelection in chatview.go
let lastField = ""; // the text box the reader last typed in
const changed = () => dispatchEvent(new Event("prm-view"));
const inPage = (n) => !!n && !!(document.getElementById("main")?.contains(n) || document.getElementById("panel")?.contains(n));
const elementOf = (n) => (n?.nodeType === 1 ? n : n?.parentElement);

export function initViewCtx() {
  document.addEventListener("pointerdown", (e) => {
    if (sel && inPage(e.target) && !e.target.closest("textarea, input")) { sel = null; changed(); }
  });
  document.addEventListener("selectionchange", () => {
    const s = getSelection();
    if (!s?.rangeCount || s.isCollapsed || !inPage(s.anchorNode)) return;
    const next = describe(s);
    if (next && next.text !== sel?.text) { sel = next; changed(); }
  });
  let typed;
  document.addEventListener("focusin", (e) => { if (FIELD_IDS[e.target.id]) lastField = FIELD_IDS[e.target.id]; });
  document.addEventListener("input", (e) => {
    if (!FIELD_IDS[e.target.id]) return;
    lastField = FIELD_IDS[e.target.id];
    clearTimeout(typed);
    typed = setTimeout(changed, 400);
  });
}

// describe reads a selection: in a diff table, the file and the old and
// new line numbers of the rows it spans.
function describe(s) {
  const text = s.toString();
  if (!text.trim()) return null;
  const r = s.getRangeAt(0);
  const out = { text: text.length > SEL_MAX ? text.slice(0, SEL_MAX) + "\n[cut]" : text };
  const a = elementOf(r.startContainer), b = elementOf(r.endContainer);
  const table = a?.closest("table.diff");
  if (table && table === b?.closest("table.diff")) {
    out.path = table.dataset.path;
    const rows = [...table.rows];
    const i = rows.indexOf(a.closest("tr")), j = rows.indexOf(b.closest("tr"));
    const old = [], neu = [];
    for (const tr of rows.slice(Math.max(0, Math.min(i, j)), Math.max(i, j) + 1)) {
      const n = tr.querySelectorAll("td.n");
      if (n.length < 2) continue;
      const o = parseInt(n[0].textContent.replace(/\D/g, ""), 10), w = parseInt(n[n.length - 1].textContent.replace(/\D/g, ""), 10);
      if (o) old.push(o);
      if (w) neu.push(w);
    }
    if (neu.length) Object.assign(out, { new_start: Math.min(...neu), new_end: Math.max(...neu) });
    if (old.length) Object.assign(out, { old_start: Math.min(...old), old_end: Math.max(...old) });
  } else if (!a?.closest("#main, #panel")) return null;
  out.unit = a?.closest("[data-uid]")?.dataset.uid || undefined;
  return out;
}

export const selection = () => sel;
export function clearSelection() { sel = null; changed(); }

// fields are the text boxes open on the page, as chatField in chatview.go.
export function fields() {
  const out = [];
  const val = (id, fallback) => document.getElementById(id)?.value ?? fallback ?? "";
  if (S.composer) {
    const c = S.composer;
    out.push({ name: "comment", text: val("composer-text", c.body), path: c.path, side: c.side, line: c.line, draft: c.id || undefined });
  }
  if (S.dismissing) {
    const [kind, unit, index] = S.dismissing.key.split("|");
    out.push({ name: "dismiss_reason", text: val("dismiss-why", S.dismissing.reason), kind, unit, index: Number(index) || 0 });
  }
  if (S.result && !S.result.pr.local_path && panelOpen()) out.push({ name: "review_summary", text: val("rv-body") });
  for (const f of out) if (f.name === lastField) f.focused = true;
  return out;
}

// fieldText says, for the "Looking at" line, which text box is open.
export function fieldText() {
  const f = fields().find((x) => x.focused) || fields()[0];
  if (!f) return "";
  return f.name === "comment" ? `writing a comment on ${f.path}:${f.line}` : f.name === "dismiss_reason" ? "writing why a claim is wrong" : "writing the review summary";
}

// fill writes text into a text box, for the chat agent. A comment box is
// opened on path:line when none is open; it stays a draft the reader
// saves. It returns what the box said before.
export function fill(name, text, a = {}) {
  if (typeof text !== "string") throw new Error("missing text");
  const set = (id, v) => { const el = document.getElementById(id); if (el) { el.value = v; el.dispatchEvent(new Event("input", { bubbles: true })); } };
  let was = "";
  switch (name) {
    case "comment": {
      if (!S.composer) {
        const line = Number(a.line), side = a.side === "LEFT" ? "LEFT" : "RIGHT";
        const f = a.path && fileByPath(a.path);
        if (!f || !line) throw new Error("no comment box is open: give path and line to open one");
        const near = (h) => side === "LEFT" ? line >= h.old_start - 3 && line < h.old_start + h.old_lines + 3 : line >= h.new_start - 3 && line < h.new_start + h.new_lines + 3;
        const u = (f.units || []).find((x) => (x.hunks || []).some(near));
        if (!u) throw new Error(`${a.path}:${line} is not a changed line (or within 3 lines of one), so GitHub would not take a comment there`);
        jumpToUnit(u.id);
        S.composer = { path: f.path, side, line, body: text };
      } else {
        was = document.getElementById("composer-text")?.value ?? S.composer.body ?? "";
        S.composer.body = text;
        set("composer-text", text);
      }
      break;
    }
    case "dismiss_reason":
      if (!S.dismissing) throw new Error("no dismissal is being written: open one with Dismiss on the Issues tab, or use the dismiss action");
      was = document.getElementById("dismiss-why")?.value ?? S.dismissing.reason ?? "";
      S.dismissing.reason = text;
      set("dismiss-why", text);
      break;
    case "review_summary":
      if (S.result.pr.local_path) throw new Error("a local review has no summary");
      if (!panelOpen()) openPanel();
      was = document.getElementById("rv-body")?.value ?? "";
      setReviewBody(text);
      break;
    default:
      throw new Error(`no text box ${name} (comment, dismiss_reason, review_summary)`);
  }
  lastField = name;
  render();
  const id = Object.keys(FIELD_IDS).find((k) => FIELD_IDS[k] === name);
  const el = document.getElementById(id);
  if (el) { el.focus({ preventScroll: false }); el.setSelectionRange?.(el.value.length, el.value.length); }
  changed();
  return was;
}
