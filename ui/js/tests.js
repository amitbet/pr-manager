// The change's test run (triage/tests.go) and the fix audit (fixaudit.go)
// in a result's header: what the tests said, or why they didn't run, with
// a button to run them; and a line about each fix round's audit.
import { esc, postJSON, say } from "./util.js";
import { S, render } from "./state.js";
import { jobSettings } from "./settings.js";

let running = ""; // the key whose tests are running
let reload = async () => {}; // shows a result again, from the server

// initTests gets main's way of showing a result again.
export function initTests(show) {
  reload = show;
}

// testsLine is the header's line about the result's tests.
export function testsLine(r) {
  const t = r.tests;
  if (!t) return "";
  const btn = (label) => running === r.key
    ? `<button class="linkbtn" disabled>running tests…</button>`
    : `<button class="linkbtn" data-act="run-tests" title="Run the change's tests on this machine now">${label}</button>`;
  let what;
  if (!t.ran) {
    what = `<span class="chip tests-chip" title="${esc(t.skipped || "")}">tests not run</span> <span class="muted">${esc(t.skipped || "")}</span> · ${btn("Run tests")}`;
  } else if (!t.failed) {
    what = `<span class="chip tests-chip ok" title="${esc(t.command || "")}">tests ✓ ${t.passed} passed</span>`;
  } else {
    const broke = t.regressions ? `, ${t.regressions} broken by this change` : ", all failing before this change too";
    const names = (t.failures || []).slice(0, 4).map((f) => `<li><code>${esc(failName(f))}</code>${f.pre_existing ? " <span class=\"muted\">(fails at the merge base too)</span>" : ""}</li>`).join("");
    what = `<span class="chip tests-chip ${t.regressions ? "bad" : ""}" title="${esc(t.command || "")}">tests ✗ ${t.failed} failing${broke}</span>${names ? `<ul class="tests-fails">${names}</ul>` : ""}`;
  }
  const took = t.ran && t.duration_ms ? ` <span class="muted">· ${(t.duration_ms / 1000).toFixed(0)}s${t.network ? " · with network" : ""}</span>` : "";
  return `<div class="meta tests-line">${what}${took}${t.ran ? ` · ${btn("run again")}` : ""}</div>`;
}

function failName(f) {
  const where = !f.package ? "" : f.package === "." ? "./" : `./${f.package}`;
  if (!f.test) return where ? `${where} (build or setup failed)` : "the test command";
  return `${f.test} in ${where}`;
}

// auditLine is the header's line about the fix's audits.
export function auditLine(r) {
  if (!r.fix_audit_note) return "";
  return `<div class="meta audit-line" title="A second model checked each fix round: whether it fixed its issues, and what it added beyond them.">Fix audit: ${esc(r.fix_audit_note)}</div>`;
}

export const actions = {
  "run-tests": async () => {
    const r = S.result;
    if (!r || running) return false;
    running = r.key;
    render();
    try {
      // Asked for here, so a PR the settings don't test runs too.
      await postJSON(`/api/results/${encodeURIComponent(r.key)}/tests`, { ...jobSettings(), tests: "all" });
      running = "";
      if (S.result?.key === r.key) await reload(r.key);
    } catch (e) {
      running = "";
      say(e.message);
    }
    render();
    return false;
  },
};
