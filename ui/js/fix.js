// Local fix jobs. The result stays open while a fix runs, with a banner that
// shows the job's stage and opens its log. A completed job opens its
// reviewed result and worktree path.
import { $, esc, api, postJSON } from "./util.js";
import { S, render, allUnits } from "./state.js";
import { fixSettings } from "./settings.js";
import { refreshJobs, showLog, stageText } from "./jobs.js";

let onDone = async () => {};
let run = null; // { id, key: the result being fixed, job }

// initFix sets what opens a finished fix's result (its key).
export function initFix(done) {
  onDone = done;
}

const running = () => run && run.job.status === "running";

// startFix takes one of: all (with comments to add the confirmed review
// threads), unit_id and issue, or unit_id and thread.
async function startFix(target) {
  if (running() || !S.result) return false;
  menuOpen = false;
  const body = { key: S.result.key, all: false, unit_id: "", issue: 0, ...target, ...fixSettings() };
  if (S.result.pr.local_path) body.location = "worktree";
  run = { id: "", key: S.result.key, job: { status: "running", stage: "" } };
  render();
  try {
    const job = await postJSON("/api/fix", body);
    run.id = job.id;
    refreshJobs();
    follow(job.id);
  } catch (e) {
    run.job = { status: "error", error: e.message };
  }
}

// follow polls a fix job, keeping its banner current, and opens the fixed
// result when it's done and its PR is still on screen.
async function follow(id) {
  for (;;) {
    const j = await api(`/api/jobs/${id}`).catch((e) => ({ status: "error", error: e.message }));
    if (run?.id !== id) return;
    run.job = j;
    if (j.status === "done") {
      const key = run.key;
      run = null;
      refreshJobs();
      if (j.key && S.result?.key === key) await onDone(j.key);
      else paint();
      return;
    }
    paint();
    if (j.status === "error") { refreshJobs(); return; }
    await new Promise((res) => setTimeout(res, 700));
  }
}

// paint redraws the banner in place, so a running fix doesn't re-render
// the page under the reviewer.
function paint() {
  const el = $("#main .fix-banner");
  if (el) el.outerHTML = fixBanner();
  else if (run && S.result?.key === run.key) render();
}

// fixBanner shows the running or failed fix of the open result.
export function fixBanner() {
  if (!run || S.result?.key !== run.key) return "";
  const log = run.id ? `<button class="linkbtn" data-act="fix-log">View log</button>` : "";
  if (run.job.status === "error") {
    return `<div class="tr-banner error fix-banner" role="status">Fix failed: ${esc(run.job.error)} <span class="spacer"></span>${log}<button class="linkbtn" data-act="fix-dismiss">dismiss</button></div>`;
  }
  return `<div class="tr-banner fix-banner" role="status"><span class="spinner"></span>Fixing… ${esc(run.job.stage ? stageText(run.job) : "starting")}<span class="spacer"></span>${log}</div>`;
}

// fixDisabled disables a fix button while a fix runs, on a PR that isn't
// open (as of its triage), and when a local checkout has uncommitted
// changes and no separate worktree to fix in.
export function fixDisabled() {
  const r = S.result;
  if (running()) return 'disabled title="A fix is running"';
  if (!r.pr.local_path && r.pr.state !== "OPEN") return `disabled title="The PR is ${esc(r.pr.state.toLowerCase())}; only open PRs and local repositories can be fixed"`;
  if (r.pr.local_path && r.pr.uncommitted && !r.local_fix_dir) return 'disabled title="Commit changes before fixing in a separate worktree"';
  return "";
}

// issueFixButton starts a local fix job for issue i of unit u.
export function issueFixButton(u, i, cls = "details-btn") {
  return `<button class="${cls}" data-act="fix-issue" data-unit="${esc(u.id)}" data-issue="${i}" ${fixDisabled()}>Fix issue</button>`;
}

// threadFixButton fixes one review thread. One the check didn't confirm,
// or from someone without write access, can still be fixed on request.
export function threadFixButton(u, t, cls = "details-btn") {
  if (t.fixed) return "";
  const sure = t.status === "valid";
  const why = !t.trusted ? "Left by someone without write access. Read it first: its text goes to the fixer." : "The check did not confirm this comment";
  return `<button class="${cls}" data-act="fix-thread" data-unit="${esc(u.id)}" data-thread="${esc(t.id)}" ${fixDisabled() || (sure ? "" : `title="${why}"`)}>${sure ? "Fix comment" : "Fix anyway"}</button>`;
}

// The Fix all dropdown: whether confirmed review comments go along.
const INCLUDE = "pr-manager.fix_comments";
const includeComments = () => localStorage.getItem(INCLUDE) !== "0";
let menuOpen = false;

// fixableComments are the confirmed threads Fix all can include: not
// fixed, and not already a review issue of their unit.
const fixableComments = () => allUnits().flatMap(({ u }) => (u.threads || []).filter((t) => t.status === "valid" && t.trusted && !t.fixed && t.duplicate_of == null));

// fixAllHTML is the Fix all button, split with a menu to include the
// confirmed review comments when there are any.
export function fixAllHTML() {
  const issues = allUnits().reduce((n, { u }) => n + (u.issues || []).filter((i) => !i.dismissed).length, 0);
  const comments = fixableComments().length;
  if (!issues && !comments) return "";
  const dis = fixDisabled();
  const s = comments === 1 ? "" : "s";
  if (!comments) return `<button class="details-btn" data-act="fix-all" ${dis}>Fix all issues</button>`;
  if (!issues) return `<button class="details-btn" data-act="fix-all" data-only-comments="1" ${dis}>Fix ${comments} review comment${s}</button>`;
  const inc = includeComments();
  return `<span class="split-btn">` +
    `<button class="details-btn" data-act="fix-all" ${dis}>${inc ? `Fix all issues + ${comments} comment${s}` : "Fix all issues"}</button>` +
    `<button class="details-btn caret ${menuOpen ? "on" : ""}" data-act="fix-all-menu" aria-haspopup="true" aria-expanded="${menuOpen}" title="Fix all options" ${dis}>▾</button>` +
    (menuOpen ? `<div class="split-menu" role="menu"><label><input type="checkbox" data-act="fix-all-comments" ${inc ? "checked" : ""}> Include existing review comments <span class="muted">(${comments} confirmed)</span></label></div>` : "") +
    `</span>`;
}

document.addEventListener("click", (e) => {
  if (menuOpen && !e.target.closest(".split-btn")) { menuOpen = false; render(); }
});

export const actions = {
  "fix-issue": (el) => startFix({ unit_id: el.dataset.unit, issue: Number(el.dataset.issue) }),
  "fix-thread": (el) => startFix({ unit_id: el.dataset.unit, thread: el.dataset.thread }),
  "fix-all": (el) => startFix({ all: true, comments: !!el.dataset.onlyComments || includeComments() }),
  "fix-all-menu": () => { menuOpen = !menuOpen; },
  "fix-all-comments": (el) => { localStorage.setItem(INCLUDE, el.checked ? "1" : "0"); },
  "fix-log": () => { showLog(run.id); return false; },
  "fix-dismiss": () => { run = null; },
};
