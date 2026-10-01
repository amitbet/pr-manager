// Open review threads from GitHub, placed on the unit their line falls in
// and checked like the review's own issues. They sit under the issues;
// the fix buttons live in fix.js.
import { esc } from "./util.js";
import { allUnits } from "./state.js";
import { SEV_CLASS } from "./scores.js";
import { threadFixButton } from "./fix.js";

const STATUS = {
  rejected: ["not confirmed", "The check did not find this problem in the code"],
  question: ["question", "Asks the author something; makes no claim about the code"],
  nit: ["nit", "Naming, style or chatter: nothing to fix"],
  untrusted: ["not checked", "Left by the PR author, or by someone without write access (or who hides their org membership): not checked, and fixed only if you ask"],
  unchecked: ["not checked", "The check failed or no review model is configured"],
};

function statusChip(t) {
  if (t.fixed) return `<span class="dz low" title="${esc(t.fix_note || "")}">fixed · round ${t.fix_round}</span>`;
  if (t.status === "valid") return `<span class="dz ${SEV_CLASS[t.issue.severity] || "high"}">${esc(t.issue.severity)}</span>`;
  const [label, title] = STATUS[t.status] || [t.status || "not checked", ""];
  return `<span class="dz unknown" title="${esc(title)}">${esc(label)}</span>`;
}

// firstLine is a comment's opening line, cut to a title's length.
function firstLine(s) {
  const l = String(s || "").trim().split("\n")[0];
  return l.length > 120 ? l.slice(0, 117).trimEnd() + "…" : l;
}

function threadItem(u, t) {
  const is = t.issue;
  const title = is?.title || firstLine(t.comments?.[0]?.body) || "(empty comment)";
  const line = is?.line || t.line;
  const d = dupTarget(u, t);
  const dup = d ? `<span class="chip" title="${esc(d.is.title)}">${d.u === u ? `same as review issue ${d.i + 1}` : `same as a review issue on ${esc(d.u.file)}`}</span>` : "";
  const why = t.fixed ? t.fix_note : t.reason;
  const quoted = (t.comments || []).map((c) => `<div class="tq"><a href="${esc(c.url)}" target="_blank" rel="noopener">@${esc(c.author)}</a>${c.trusted ? "" : ` <span class="chip">${c.pr_author ? "PR author" : "no write access"}</span>`}<blockquote>${esc(c.body)}</blockquote></div>`).join("");
  return `<li class="thread-item"><div>${statusChip(t)}${line ? `<span class="ln">line ${line}</span>` : ""}<b>${esc(title)}</b>
      <a class="tauthor" href="${esc(t.url)}" target="_blank" rel="noopener">@${esc(t.author)}</a>${dup} ${threadFixButton(u, t)}</div>
    ${is?.detail ? `<span class="idetail">${esc(is.detail)}</span>` : ""}
    ${is?.failure_scenario ? `<span class="idetail"><b>When:</b> ${esc(is.failure_scenario)}</span>` : ""}
    ${why ? `<span class="idetail"><b>${t.fixed ? "Fix:" : "Check:"}</b> ${esc(why)}</span>` : ""}
    <details class="tcomment"><summary>${t.comments?.length > 1 ? `${t.comments.length} comments` : "comment"} on GitHub</summary>${quoted}</details></li>`;
}

// threadsHTML lists u's open review threads; wrap is "details" for the
// classic list and "wz" for the walkthrough.
export function threadsHTML(u, wrap = "details") {
  if (!u.threads?.length) return "";
  const items = `<ul class="issues threads">${u.threads.map((t) => threadItem(u, t)).join("")}</ul>`;
  if (wrap === "wz") return `<div class="wz-sec"><h4>Existing review comments</h4>${items}</div>`;
  return `<p><span class="lbl">Existing review comments</span></p>${items}`;
}

// threadsChip counts u's open threads for the unit's header row.
export function threadsChip(u) {
  const open = (u.threads || []).filter((t) => !t.fixed);
  if (!open.length) return "";
  const ok = open.filter((t) => t.status === "valid").length;
  return `<span class="chip" title="${esc(open.map((t) => `@${t.author}: ${t.issue?.title || firstLine(t.comments?.[0]?.body)}`).join("\n"))}">${open.length} review comment${open.length > 1 ? "s" : ""}${ok ? ` · ${ok} confirmed` : ""}</span>`;
}

// dupTarget is the review issue thread t on u repeats, if any: one of
// u's, or of the unit duplicate_unit names.
export function dupTarget(u, t) {
  if (t.duplicate_of == null) return null;
  const o = t.duplicate_unit ? allUnits().find(({ u: x }) => x.id === t.duplicate_unit)?.u : u;
  const is = o?.issues?.[t.duplicate_of];
  return is ? { u: o, i: t.duplicate_of, is } : null;
}

// raisedBy is the thread that already raises u's issue i, if any, from
// whichever unit it is on.
export function raisedBy(u, i) {
  for (const { u: o } of allUnits()) {
    const t = (o.threads || []).find((t) => t.duplicate_of === i && (t.duplicate_unit || o.id) === u.id);
    if (t) return t;
  }
  return null;
}

export const raisedChip = (t) => `<a class="chip" href="${esc(t.url)}" target="_blank" rel="noopener" title="A review comment already raises this, so there is nothing to draft">raised by @${esc(t.author)}</a>`;
