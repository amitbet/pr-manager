// The Issues tab: every claim anyone has made about this PR in one list —
// what the review found, what static analysis found, and what people left
// on GitHub — each with the state it is in.
//
// Dismissing a claim is the point of the tab. It is kept on the record
// with the reason it was rejected, and it stops counting: the unit it was
// holding in human review falls back to wherever its score puts it, and
// the same claim arrives dismissed on the next push. The reasons pile up
// in one file per repository, which is what a reviewer that learns from
// feedback will read (see the dismissals store in dismiss.go).
import { esc, api, postJSON } from "./util.js";
import { S, render, allUnits } from "./state.js";
import { SEV_CLASS, SEV_RANK, issueCapChip, issueScenarioHTML, attLevel } from "./scores.js";
import { issueDraftButton } from "./comments.js";
import { issueFixButton, threadFixButton, fixAllHTML } from "./fix.js";
import { raisedBy, raisedChip } from "./threads.js";
import { lintLine, LINT_RANK } from "./lint.js";
import { trText, trDir } from "./entext.js";
import { jumpToUnit } from "./review.js";

const KINDS = [["issue", "Review"], ["lint", "Static analysis"], ["comment", "Review comments"]];

// claims is every row the tab can show, most severe first. A row knows
// where it came from, so dismissing it names the same thing the server
// does: a unit, a kind and an index.
function claims() {
  const out = [];
  for (const { u, f } of allUnits()) {
    (u.issues || []).forEach((is, i) =>
      out.push({ kind: "issue", u, f, i, is, sev: is.severity, rank: SEV_RANK[is.severity] ?? 3, dismissed: !!is.dismissed, why: is.dismissed_why, dkey: is.dismiss_key }));
    (u.lint || []).forEach((x, i) =>
      out.push({ kind: "lint", u, f, i, lint: x, sev: x.severity === "error" ? "high" : "low", rank: (LINT_RANK[x.severity] ?? 1) + 1, dismissed: !!x.dismissed, why: x.dismissed_why, dkey: x.dismiss_key }));
    (u.threads || []).forEach((t) => {
      if (t.fixed) return;
      out.push({ kind: "comment", u, f, t, sev: t.issue?.severity || "low", rank: (SEV_RANK[t.issue?.severity] ?? 3) + (t.status === "valid" ? 0 : 0.5), dismissed: false });
    });
  }
  out.sort((a, b) => a.rank - b.rank || a.u.id.localeCompare(b.u.id));
  return out;
}

const hidden = () => S.issueKinds;
const visible = (c) => !hidden().has(c.kind);

// claimKey identifies a row in the page, so the dismiss form knows which
// one it is open on.
const claimKey = (c) => `${c.kind}|${c.u.id}|${c.kind === "comment" ? c.t.id : c.i}`;

function unitLink(c) {
  const where = c.u.symbol ? `${c.u.file} · ${c.u.symbol}` : c.u.file;
  return `<button class="linkbtn unit-link" data-act="issue-goto" data-unit="${esc(c.u.id)}" title="Open this change in the Review tab">${esc(where)}</button>`;
}

// dismissForm asks why. The reason is the whole value of the record: a
// dismissal with no reason silences one claim, a dismissal with one says
// something about the codebase that the next review could use.
function dismissForm(c) {
  const d = S.dismissing;
  if (!d || d.key !== claimKey(c)) return "";
  return `<form class="dismiss-form" data-act="dismiss-save" data-key="${esc(d.key)}">
      <label for="dismiss-why">Why is this not a problem? <span class="muted">(optional, kept for this repository)</span></label>
      <input id="dismiss-why" name="why" value="${esc(d.reason || "")}" placeholder="e.g. the caller already holds the lock" autocomplete="off">
      <button class="primary" type="submit" ${d.busy ? "disabled" : ""}>${d.busy ? "Dismissing…" : "Dismiss"}</button>
      <button type="button" data-act="dismiss-cancel">Cancel</button>
      ${d.error ? `<span class="dismiss-error">${esc(d.error)}</span>` : ""}
    </form>`;
}

// A GitHub comment has no Dismiss: it belongs to the person who wrote it,
// and resolving the thread there is what makes it stop counting.
function dismissButton(c) {
  if (c.kind === "comment" || c.dismissed) return "";
  return `<button class="details-btn" data-act="issue-dismiss" data-ckey="${esc(claimKey(c))}">Dismiss</button>`;
}

const restoreButton = (c) => c.dkey
  ? `<button class="linkbtn" data-act="dismiss-restore" data-key="${esc(c.dkey)}">restore</button>`
  : "";

function issueRow(c) {
  const { u, f, is, i } = c;
  const dup = raisedBy(u, i);
  const act = c.dismissed ? "" : `${dup ? raisedChip(dup) : issueDraftButton(f, u, i)} ${issueFixButton(u, i)}`;
  return `<div class="claim-head">
      <span class="dz ${SEV_CLASS[is.severity] || "high"}">${esc(is.severity)}</span>
      ${is.line ? `<span class="ln">line ${is.line}</span>` : ""}
      <b class="tr" ${trDir(u, `issues.${i}.title`, is.title)}>${trText(u, `issues.${i}.title`, is.title)}</b>${issueCapChip(is)}
      <span class="spacer"></span>${act} ${dismissButton(c)}
    </div>
    ${unitLink(c)}
    ${is.detail ? `<span class="idetail tr" ${trDir(u, `issues.${i}.detail`, is.detail)}>${trText(u, `issues.${i}.detail`, is.detail)}</span>` : ""}
    ${issueScenarioHTML(is, "idetail", u, i)}`;
}

function lintRow(c) {
  return `<div class="claim-head">${lintLine(c.lint)}<span class="spacer"></span>${dismissButton(c)}</div>${unitLink(c)}`;
}

function commentRow(c) {
  const t = c.t, is = t.issue;
  const title = is?.title || (t.comments?.[0]?.body || "").split("\n")[0] || "(empty comment)";
  const state = t.status === "valid"
    ? `<span class="dz ${SEV_CLASS[is.severity] || "high"}">${esc(is.severity)}</span>`
    : `<span class="dz unknown">${esc(t.status === "rejected" ? "not confirmed" : t.status || "not checked")}</span>`;
  return `<div class="claim-head">${state}${t.line ? `<span class="ln">line ${t.line}</span>` : ""}
      <b>${esc(title)}</b>
      <a class="tauthor" href="${esc(t.url)}" target="_blank" rel="noopener">@${esc(t.author)}</a>
      <span class="spacer"></span>${threadFixButton(c.u, t)}
    </div>
    ${unitLink(c)}
    ${is?.detail ? `<span class="idetail">${esc(is.detail)}</span>` : ""}`;
}

const rowHTML = { issue: issueRow, lint: lintRow, comment: commentRow };

function claimHTML(c) {
  return `<li class="claim ${c.kind}${c.dismissed ? " dismissed" : ""}">
      ${rowHTML[c.kind](c)}
      ${c.dismissed ? `<span class="idetail dismissed-why"><b>Dismissed</b>${c.why ? `: ${esc(c.why)}` : ""} · ${restoreButton(c)}</span>` : ""}
      ${dismissForm(c)}
    </li>`;
}

// counts is what each filter is worth, so a filter that hides nothing
// says so.
function counts(all) {
  const c = { issue: 0, lint: 0, comment: 0 };
  all.forEach((x) => { if (!x.dismissed) c[x.kind]++; });
  return c;
}

export function issuesHTML() {
  const all = claims();
  const n = counts(all);
  const open = all.filter((c) => !c.dismissed && visible(c));
  const gone = all.filter((c) => c.dismissed && visible(c));
  const weight = { critical: 95, high: 75, medium: 45, low: 15 };
  const worst = open.reduce((m, c) => Math.max(m, weight[c.sev] || 15), 0);
  return `<div class="toolbar">
      ${KINDS.map(([k, label]) => `<span class="filter ${hidden().has(k) ? "off" : ""}" data-act="issues-filter" data-k="${k}"><b>${n[k]}</b> ${label}</span>`).join("")}
      <span class="spacer"></span>
      ${open.length ? `<span class="dz ${attLevel(worst)}" title="the worst claim still standing">${open.length} open</span>` : ""}
      ${fixAllHTML()}
    </div>
    ${open.length
      ? `<ul class="claims">${open.map(claimHTML).join("")}</ul>`
      : `<div class="empty">${all.length ? "Nothing open: every claim on this PR has been dismissed or fixed." : "The review, static analysis and GitHub have nothing to report on this PR."}</div>`}
    ${gone.length ? `<details class="dismissed-box" ${S.showDismissed ? "open" : ""}>
      <summary data-act="issues-dismissed">${gone.length} dismissed</summary>
      <p class="hint">Dismissed claims stay on the record and stop counting toward a unit's bucket. They are remembered for this repository, so the same claim arrives dismissed on the next push.</p>
      <ul class="claims">${gone.map(claimHTML).join("")}</ul>
    </details>` : ""}`;
}

// merge copies the server's re-placed state onto the result already on
// screen, rather than replacing it: the units carry translated text and
// the diff rendering state, and neither survives a swap.
function merge(fresh) {
  const r = S.result;
  Object.assign(r, { counts: fresh.counts, impact: fresh.impact, likelihood: fresh.likelihood, attention: fresh.attention });
  const by = new Map();
  for (const f of fresh.files) for (const u of f.units || []) by.set(u.id, u);
  for (const { u } of allUnits()) {
    const n = by.get(u.id);
    if (!n) continue;
    Object.assign(u, { decision: n.decision, score: n.score, attention: n.attention });
    (u.issues || []).forEach((is, i) => Object.assign(is, { dismissed: n.issues?.[i]?.dismissed, dismissed_why: n.issues?.[i]?.dismissed_why, dismiss_key: n.issues?.[i]?.dismiss_key }));
    (u.lint || []).forEach((x, i) => Object.assign(x, { dismissed: n.lint?.[i]?.dismissed, dismissed_why: n.lint?.[i]?.dismissed_why, dismiss_key: n.lint?.[i]?.dismiss_key }));
  }
}

const url = () => `/api/results/${encodeURIComponent(S.result.key)}/dismissals`;

export const actions = {
  "issues-filter": (el) => { const k = el.dataset.k; hidden().has(k) ? hidden().delete(k) : hidden().add(k); },
  "issues-dismissed": () => { S.showDismissed = !S.showDismissed; return false; },
  "issue-goto": (el) => { jumpToUnit(el.dataset.unit); return false; },
  "issue-dismiss": (el) => { S.dismissing = { key: el.dataset.ckey, reason: "" }; },
  "dismiss-cancel": () => { S.dismissing = null; },
  "dismiss-save": async (el, e) => {
    e.preventDefault();
    const d = S.dismissing;
    if (!d || d.busy) return false;
    const [kind, unit, idx] = d.key.split("|");
    d.reason = el.querySelector("input[name=why]").value.trim();
    d.busy = true;
    render();
    try {
      merge(await postJSON(url(), { unit, kind, index: Number(idx), reason: d.reason }));
      S.dismissing = null;
    } catch (err) {
      d.busy = false;
      d.error = err.message;
    }
    render();
    return false;
  },
  "dismiss-restore": async (el) => {
    el.disabled = true;
    try {
      merge(await api(`${url()}/${encodeURIComponent(el.dataset.key)}`, { method: "DELETE" }));
    } catch (err) { alert(err.message); }
  },
};
