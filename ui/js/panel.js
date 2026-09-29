// Submit-review panel: summary, event, the pending comments, and the
// GitHub submit (or dry-run payload).
import { $, esc, postJSON } from "./util.js";
import { S, prBase, render } from "./state.js";

// The review being written is kept per PR (prBase, so it outlives a
// re-triage of the same PR): one PR's summary and verdict never show, or
// get submitted, on another.
const reviews = new Map();
const fresh = () => ({ event: "COMMENT", body: "", msg: "", err: false, payload: null, busy: false });
function review() {
  const k = prBase();
  if (!reviews.has(k)) reviews.set(k, fresh());
  return reviews.get(k);
}
let onJump = () => {};

export const panelOpen = () => !$("#panel").hidden;
export const closePanel = () => { $("#panel").hidden = true; };

export function openPanel() {
  $("#panel").hidden = false;
  Object.assign(review(), { msg: "", payload: null });
  renderPanel();
}

// initPanel wires the header button; jump(draft) shows a pending comment.
export function initPanel(jump) {
  onJump = jump;
  $("#review-btn").onclick = () => panelOpen() ? closePanel() : openPanel();
}

// updateReviewButton shows the header button with the pending-comment count.
export function updateReviewButton() {
  const n = S.drafts.length;
  $("#review-btn").hidden = false;
  $("#review-btn").innerHTML = `${S.result.pr.local_path ? "Notes" : "Review"}${n ? ` <span class="pill draft">${n}</span>` : ""}`;
}

export function renderPanel() {
  const p = $("#panel");
  const ds = S.drafts;
  const dry = S.cfg?.review_dry_run;
  const local = !!S.result.pr.local_path;
  const RV = review();
  p.innerHTML = `
    <h3>${local ? "Review notes" : "Submit review"}${!local && dry ? ` <span class="chip">dry run: nothing is posted</span>` : ""}</h3>
    ${local ? `<p class="hint">Create the PR when the branch is ready. Pending comments are saved here.</p>` : ""}
    ${local ? "" : `
    <textarea id="rv-body" placeholder="Review summary (optional for Comment and Approve)">${esc(RV.body)}</textarea>
    <div class="ev">
      ${[["COMMENT", "Comment", "Feedback without approving."],
         ["APPROVE", "Approve", "GitHub won't let you approve your own PR."],
         ["REQUEST_CHANGES", "Request changes", "Needs a summary."]].map(([v, l, h]) =>
        `<label><input type="radio" name="rv-ev" value="${v}" ${RV.event === v ? "checked" : ""}> ${l}<small>${h}</small></label>`).join("")}
    </div>`}
    <div class="dl">${ds.length ? ds.map((d) => `<a href="#" data-jump="${d.id}">${esc(d.path)}:${d.line} (${d.side === "LEFT" ? "old" : "new"}) · ${esc(d.body.slice(0, 80))}</a>`).join("") : `<div style="padding:6px 0;color:var(--muted)">No pending line comments. Hover a line and click + to add one.</div>`}</div>
    <div style="display:flex;gap:8px;justify-content:flex-end">
      <button id="rv-close">Close</button>
      ${local ? "" : `<button id="rv-submit" class="primary" ${RV.busy ? "disabled" : ""}>${RV.busy ? "Submitting…" : `Submit review${ds.length ? ` (${ds.length} comment${ds.length > 1 ? "s" : ""})` : ""}`}</button>`}
    </div>
    ${RV.msg ? `<div class="msg ${RV.err ? "err" : ""}">${RV.msg}</div>` : ""}
    ${RV.payload ? `<pre>${esc(JSON.stringify(RV.payload, null, 2))}</pre>` : ""}`;
  if (!local) {
    $("#rv-body").oninput = (e) => RV.body = e.target.value;
    p.querySelectorAll("input[name=rv-ev]").forEach((i) => i.onchange = () => RV.event = i.value);
    $("#rv-submit").onclick = submitReview;
  }
  $("#rv-close").onclick = closePanel;
  p.querySelectorAll("[data-jump]").forEach((a) => a.onclick = (e) => {
    e.preventDefault();
    onJump(S.drafts.find((x) => x.id === a.dataset.jump));
  });
}

// submitReview posts the review of the PR on screen. The PR can change
// while it is in flight; the answer goes to the review it was sent from.
async function submitReview() {
  const base = prBase();
  const RV = review();
  const here = () => S.result && prBase() === base;
  Object.assign(RV, { busy: true, msg: "", err: false, payload: null });
  renderPanel();
  try {
    const res = await postJSON(`${base}/review`, { event: RV.event, body: RV.body, commit_id: S.result.pr.head_oid });
    if (res.dry_run) {
      Object.assign(RV, { busy: false, msg: "Dry run: this is what would be sent to GitHub. Your drafts are kept.", payload: res.payload });
    } else {
      Object.assign(RV, fresh(), { msg: `Posted: <a href="${esc(res.html_url)}" target="_blank" rel="noopener">${esc(res.html_url)}</a>` });
      if (here()) { S.drafts = []; render(); }
    }
  } catch (e) {
    Object.assign(RV, { busy: false, err: true, msg: esc(e.message) });
  }
  if (here() && panelOpen()) renderPanel();
}
