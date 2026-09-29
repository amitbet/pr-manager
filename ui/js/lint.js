// Static-analysis findings on the lines the PR adds (triage/lint.go).
// They are deterministic, so they are shown whether or not the review
// agreed with them, and they are kept apart from the review's own issues.
import { esc } from "./util.js";

export const LINT_CLASS = { error: "high", warning: "medium" };
export const LINT_RANK = { error: 0, warning: 1 };

export const liveLint = (u) => (u.lint || []).filter((f) => !f.dismissed);

// lintLabel is the rule behind a finding, or the tool when it has none
// (an ESLint parse error).
const lintLabel = (f) => f.label || (f.rule ? `${f.tool} ${f.rule}` : f.tool);

// lintLine is one finding as a single row: where it is, which rule said
// so, and what it said.
export const lintLine = (f) =>
  `<span class="dz ${LINT_CLASS[f.severity] || "medium"}">${esc(f.severity)}</span>` +
  `${f.line ? `<span class="ln">line ${f.line}</span>` : ""}` +
  `<code class="lint-rule">${esc(lintLabel(f))}</code> ` +
  `<span class="lint-msg">${esc(f.message)}</span>`;

// lintHTML is a unit's findings inside the review details; wrap is "wz"
// in the walkthrough, which uses its own section heading.
export function lintHTML(u, wrap = "details") {
  const fs = liveLint(u);
  if (!fs.length) return "";
  const items = `<ul class="issues lint">${fs.map((f) => `<li>${lintLine(f)}</li>`).join("")}</ul>`;
  const head = "Static analysis";
  if (wrap === "wz") return `<div class="wz-sec"><h4>${head}</h4>${items}</div>`;
  return `<p><span class="lbl">${head}</span></p>${items}`;
}

// lintChip counts a unit's findings for its header row.
export function lintChip(u) {
  const fs = liveLint(u);
  if (!fs.length) return "";
  const errs = fs.filter((f) => f.severity === "error").length;
  const title = fs.map((f) => `${f.severity}: ${lintLabel(f)} — ${f.message}`).join("\n");
  return `<span class="chip lint-chip" title="${esc(title)}">${fs.length} lint${errs ? ` · ${errs} error${errs > 1 ? "s" : ""}` : ""}</span>`;
}
