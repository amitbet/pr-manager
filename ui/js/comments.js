// Pending review comments: the threads under diff lines, the composer, and
// the actions that create, edit and delete drafts.
import { $, esc, api, postJSON } from "./util.js";
import { S, prBase, render, fileOfUnit } from "./state.js";

const draftsAt = (path, side, line) => S.drafts.filter((d) => d.path === path && d.side === side && d.line === line);
function composerAt(path, side, line) {
  const c = S.composer;
  return c && !c.id && c.path === path && c.side === side && c.line === line;
}

// threadRow renders pending comments (and an open composer) anchored to
// any of the given {side, line} positions.
export function threadRow(f, anchors, cols) {
  let html = "";
  for (const a of anchors) {
    for (const d of draftsAt(f.path, a.side, a.line)) {
      html += S.composer?.id === d.id ? composerHTML(S.composer) : `
        <div class="draft" id="draft-${d.id}">
          <div class="dh"><span class="pill draft">pending</span> <span>${a.side === "LEFT" ? "old" : "new"} line ${a.line}</span>
            <span class="spacer"></span>
            <button class="linkbtn" data-act="edit" data-id="${d.id}">edit</button>
            <button class="linkbtn" data-act="delete" data-id="${d.id}">delete</button></div>
          <div class="db">${esc(d.body)}</div>
        </div>`;
    }
    if (composerAt(f.path, a.side, a.line)) html += composerHTML(S.composer);
  }
  return html ? `<tr class="thread"><td colspan="${cols}">${html}</td></tr>` : "";
}

// shown is the composer the page's textarea was rendered for. syncComposer
// copies only into that one, so a composer opened in its place (or none,
// after a save or cancel) never takes the previous one's text.
let shown = null;

function composerHTML(c) {
  shown = c;
  return `<div class="composer">
    <textarea id="composer-text" placeholder="Leave a comment (saved as a pending review comment)">${esc(c.body || "")}</textarea>
    <div class="actions"><span class="hint">⌘/Ctrl+Enter to save · stays pending until you submit the review</span>
      <button data-act="cancel">Cancel</button>
      <button class="primary" data-act="save">${c.id ? "Update comment" : "Add review comment"}</button></div>
  </div>`;
}

// issueDraftButton offers to turn a review issue into a pending comment,
// when GitHub would accept a comment on its line.
export function issueDraftButton(f, u, i) {
  const is = u.issues[i];
  return is.line > 0 && f._commentable(is.line)
    ? `<button class="linkbtn" data-act="issue-draft" data-unit="${esc(u.id)}" data-idx="${i}">→ draft comment</button>` : "";
}

// syncComposer copies the composer's text into state before a re-render.
export function syncComposer() {
  const ta = $("#composer-text");
  if (ta && S.composer && S.composer === shown) S.composer.body = ta.value;
}

// focusComposer puts the cursor at the end of an open composer after a render.
export function focusComposer() {
  const ta = $("#composer-text");
  if (ta) { ta.focus({ preventScroll: true }); ta.setSelectionRange(ta.value.length, ta.value.length); }
}

async function saveComposer() {
  syncComposer();
  const c = S.composer;
  if (!c || !c.body.trim()) return;
  try {
    S.drafts = await postJSON(`${prBase()}/drafts`, { id: c.id, path: c.path, side: c.side, line: c.line, body: c.body });
    S.composer = null;
    render();
  } catch (e) { alert(e.message); }
}

export const actions = {
  comment: (el) => { S.composer = { path: el.dataset.path, side: el.dataset.side, line: +el.dataset.line, body: "" }; },
  edit: (el) => { S.composer = { ...S.drafts.find((x) => x.id === el.dataset.id) }; },
  cancel: () => { S.composer = null; },
  save: async () => { await saveComposer(); return false; },
  delete: async (el) => {
    if (!confirm("Delete this pending comment?")) return false;
    S.drafts = await api(`${prBase()}/drafts/${el.dataset.id}`, { method: "DELETE" });
  },
  "issue-draft": async (el) => {
    const f = fileOfUnit(el.dataset.unit);
    const u = f.units.find((x) => x.id === el.dataset.unit);
    const is = u.issues[+el.dataset.idx];
    const body = `**${is.severity}**: ${is.title}${is.detail ? `\n\n${is.detail}` : ""}`;
    try { S.drafts = await postJSON(`${prBase()}/drafts`, { path: f.path, side: "RIGHT", line: is.line, body }); }
    catch (err) { alert(err.message); return false; }
    S.diffOpen[u.id] = true;
  },
};

export function onKeydown(e) {
  if (e.target.id !== "composer-text") return;
  if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) { e.preventDefault(); saveComposer(); }
  if (e.key === "Escape") { S.composer = null; render(); }
}
