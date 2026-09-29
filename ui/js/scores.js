// Impact, likelihood and review-attention badges, and their details blocks.
import { trText, trDir } from "./entext.js";
import { esc } from "./util.js";

// Issue severity -> pill color: low issues are yellow, not green.
export const SEV_CLASS = { low: "medium", medium: "high", high: "critical", critical: "critical" };
export const SEV_RANK = { critical: 0, high: 1, medium: 2, low: 3 };
// issueCapChip says why the severity is lower than the reviewer's claim.
export const issueCapChip = (is) => is.claimed_severity
  ? ` <span class="chip" title="${esc(is.capped || "")}">${is.critic_rejected ? "critic rejected" : is.pre_existing ? "pre-existing" : "capped"} · claimed ${esc(is.claimed_severity)}</span>` : "";

// issueScenarioHTML is the failure scenario of u's issue i and the quoted
// evidence.
export const issueScenarioHTML = (is, cls, u, i) =>
  (is.failure_scenario ? `<span class="${cls}"><b>When:</b> <span class="tr" ${trDir(u, `issues.${i}.failure_scenario`, is.failure_scenario)}>${trText(u, `issues.${i}.failure_scenario`, is.failure_scenario)}</span></span>` : "") +
  (is.evidence ? `<code class="${cls} ievidence">${esc(is.evidence)}</code>` : "");

export const attLevel = (a) => a >= 75 ? "critical" : a >= 45 ? "high" : a > 0 ? "medium" : "low";

// risk mirrors Unit.Risk in Go: impact (50 when unknown) times likelihood.
export const risk = (u) => (u.impact && u.impact.level !== "unknown" ? u.impact.score : 50) * (u.likelihood?.score || 0);

export function impactPill(d, prefix = "impact") {
  if (!d) return "";
  if (d.level === "unknown") return `<span class="dz unknown" title="${esc((d.notes || []).join("\n") || "not in the code map")}">${prefix} ?</span>`;
  const title = `Impact ${d.score}/100 (${d.level}): how much a bad change here can break\nCodeRank ${Math.round(d.rank)}th percentile · rollback ${d.rollback}\n${d.matched}: ${d.basis || ""}`;
  return `<span class="dz ${esc(d.level)}" title="${esc(title)}">${prefix} ${d.score}</span>`;
}

export function likelihoodPill(l, prefix = "likelihood") {
  if (!l) return "";
  const why = (l.factors || []).slice(0, 4).map((f) => `+${f.points} ${f.detail}`);
  const title = `Likelihood ${l.score}/100 (${l.level}): how likely this change is to go wrong\n${why.join("\n") || (l.notes || []).join("\n") || "no risk factors"}`;
  return `<span class="dz ${esc(l.level)}" title="${esc(title)}">${prefix} ${l.score}</span>`;
}

// confirmedComments are u's open review comments counted in its attention
// (TierPolicy.ApplyThreads in Go).
const confirmedComments = (u) => (u.threads || []).filter((t) => t.status === "valid" && !t.fixed && t.issue && t.duplicate_of == null);

export function attentionPill(u) {
  const cs = confirmedComments(u);
  if (!u.reviewed && !cs.length) return "";
  const n = u.issues?.length || 0;
  const lines = [...(u.issues || []).map((i) => `${i.severity}: ${i.title}`), ...cs.map((t) => `${t.issue.severity}: ${t.issue.title} (@${t.author})`)];
  const what = [n ? `${n} issue${n > 1 ? "s" : ""}` : "", cs.length ? `${cs.length} comment${cs.length > 1 ? "s" : ""}` : ""].filter(Boolean).join(" + ");
  return `<span class="dz ${attLevel(u.attention)}" title="${esc(lines.join("\n") || "review found no issues")}">attention ${u.attention}${what ? ` · ${what}` : ""}</span>`;
}

// carriedChip marks a unit that kept the review an earlier run of the
// same change earned, because neither its diff nor anything it was judged
// against moved.
export const carriedChip = (u) => u.carried_from
  ? `<span class="chip carried" title="Its diff, and the units it was judged against, are the same as at ${esc(u.carried_from.slice(0, 8))}, so this review was not paid for again.">kept from ${esc(u.carried_from.slice(0, 8))}</span>` : "";

// decisionChips are the score and "escalated" markers for a unit.
export function decisionChips(u) {
  const d = u.decision, s = u.score;
  return (s ? `<span class="chip" title="${esc(s.why)}">score ${s.total}${s.pin || s.comment_pin ? " · pinned" : ""}</span>` : "") +
    carriedChip(u) +
    (d.escalated?.length ? `<span class="esc-chip" title="${esc(d.escalated.join("\n"))}">↑ escalated</span>` : "");
}

export function impactHTML(d) {
  if (!d) return "";
  if (d.level === "unknown") return `<p><span class="lbl">Impact</span>${esc((d.notes || []).join(" ") || "not in the code map")}</p>`;
  const tags = (d.tags || []).map((t) => `<span class="chip" title="rollback difficulty ${t.s}${t.via ? `, ${t.via} call hop(s) away` : ""}">${esc(t.id)} ${t.s}${t.via ? ` · via ${t.via}` : ""}</span>`).join("");
  const rows = [
    `${impactPill(d)} matched <b>${esc(d.matched)}</b>${d.basis ? ` <code>${esc(d.basis)}</code>` : ""}`,
    `CodeRank ${Math.round(d.rank)}th percentile · ${d.callers} direct caller${d.callers === 1 ? "" : "s"} · ${d.dep_files} dependent file${d.dep_files === 1 ? "" : "s"} · rollback ${d.rollback}/100`,
  ];
  if (d.dep_repos?.length) rows.push(`Used by repos: ${d.dep_repos.map((r) => `<code>${esc(r)}</code>`).join(", ")}`);
  if (d.top_callers?.length) rows.push(`Top callers: ${d.top_callers.map((c) => `<code>${esc(c)}</code>`).join(", ")}`);
  if (d.also?.length) rows.push(`Also touches: ${d.also.map((c) => `<code>${esc(c)}</code>`).join(", ")}`);
  if (tags) rows.push(`<span class="chips">${tags}</span>`);
  if (d.notes?.length) rows.push(`<span style="color:var(--muted)">${esc(d.notes.join(" · "))}</span>`);
  return `<p><span class="lbl">Impact${d.commit ? ` (code map indexed at ${esc(d.commit.slice(0, 8))})` : ""}</span>${rows.join("<br>")}</p>`;
}

// likelihoodHTML lists what adds up to a unit's likelihood, biggest first,
// and the measurements behind it.
export function likelihoodHTML(l) {
  if (!l) return "";
  const m = l.metrics || {};
  const rows = [likelihoodPill(l)];
  if (l.factors?.length) rows.push(`<ul class="lk-factors">${l.factors.map((f) => `<li><b>+${f.points}</b> ${esc(f.detail)}</li>`).join("")}</ul>`);
  else rows.push("no risk factors");
  const facts = [];
  if (m.decls?.length) facts.push(`complexity ${m.cyclo_base !== m.cyclo ? `${m.cyclo_base} → ` : ""}${m.cyclo}, nesting ${m.nest} (${m.decls.map((d) => `<code>${esc(d)}</code>`).join(", ")})`);
  if (m.hist) facts.push(`file history: ${m.hist.commits} commits, ${m.hist.fixes} fixes, ${m.hist.reverts} reverts, ${m.hist.authors} authors${m.hist.age_days >= 0 ? `, last change ${m.hist.age_days}d before the map` : ""}`);
  if (m.author_file_commits >= 0) facts.push(`author: ${m.author_file_commits} commits to this file, ${m.author_repo_commits} in the repo${m.author_created ? ", created it" : ""}`);
  if (m.missing_partners?.length) facts.push(`usual partners not in the PR: ${m.missing_partners.map((p) => `<code>${esc(p)}</code>`).join(", ")}`);
  if (facts.length) rows.push(`<span style="color:var(--muted)">${facts.join(" · ")}</span>`);
  if (l.notes?.length) rows.push(`<span style="color:var(--muted)">${esc(l.notes.join(" · "))}</span>`);
  return `<p><span class="lbl">Likelihood</span>${rows.join("<br>")}</p>`;
}

// scoresHTML is the impact and likelihood details, shared by Review and the
// walkthrough.
export const scoresHTML = (u) => impactHTML(u.impact) + likelihoodHTML(u.likelihood);

// classificationHTML is the risk signals and "classified by" footer, shared
// by the Review details and the walkthrough.
export function classificationHTML(d) {
  const parts = [];
  if (d.risk_signals?.length) parts.push(`<p><span class="lbl">Risk signals</span><span class="chips">${d.risk_signals.map((r) => `<span class="chip">${esc(r)}</span>`).join("")}</span></p>`);
  parts.push(`<p><span class="lbl">Classified by</span>${esc(d.source)}${d.confidence ? ` at ${(d.confidence * 100).toFixed(0)}% confidence` : ""}${d.change_kind ? ` · ${esc(d.change_kind)}` : ""}</p>`);
  return parts.join("");
}

// movesHTML is how the bucket was picked, and the classifier's escalations.
export function movesHTML(u) {
  const esc_ = u.decision.escalated;
  return (u.score?.why ? `<p><span class="lbl">Bucket</span>${esc(u.score.why)}</p>` : "") +
    (esc_?.length ? `<p><span class="lbl">Escalated</span></p><ul>${esc_.map((e) => `<li>${esc(e)}</li>`).join("")}</ul>` : "");
}
