// Local fix jobs. The result stays open while a fix runs, with a banner that
// shows the job's stage and opens its log. A PR's fixes run side by side
// in its repository's checkout, each one ending in the Issues tab's list
// of the PR's fixes; a local review's run one at a time, and a completed
// one opens its reviewed result and worktree path.
import { $, esc, postJSON, say } from "./util.js";
import { S, render, allUnits } from "./state.js";
import { fixSettings } from "./settings.js";
import { refreshJobs, showLog, stageText, pollJob, cancelJob } from "./jobs.js";
import { refreshFixes } from "./fixes.js";

let onDone = async () => {};
let runs = []; // { id, key: the result being fixed, local, job }

// initFix sets what opens a finished fix's result (its key).
export function initFix(done) {
  onDone = done;
}

// running is whether a fix that blocks another runs: only a local
// review's do, since they share the user's checkout.
const running = () => runs.some((r) => r.local && r.job.status === "running");
const byId = (id) => runs.find((r) => r.id === id);
const drop = (r) => { runs = runs.filter((x) => x !== r); };

// startFix takes one of: all (with comments to add the confirmed review
// threads), unit_id and issue, unit_id and thread, or targets: a list of
// those to fix together. uncommitted is what
// to do with a local checkout's uncommitted changes (commit or branch);
// rev is how a reviewed commit or branch is fixed (checkout or current).
// Without them, the server asks and the reviewer picks. r is the result
// to fix, the open one by default. It resolves to the fix job's id, or
// null when no fix started.
export async function startFix(target, uncommitted = localStorage.getItem(UNCOMMITTED) || "", rev = "", r = S.result) {
  if (running() || !r) return null;
  menuOpen = false;
  const local = !!r.pr.local_path;
  const body = { key: r.key, all: false, unit_id: "", issue: 0, ...target, ...fixSettings(), uncommitted, rev };
  // A local checkout is fixed in place or in a worktree; a PR, which
  // isn't checked out here, in a worktree or the cached clone.
  if (local) body.location = body.location === "branch" ? "branch" : "worktree";
  else if (body.location === "branch") body.location = "worktree";
  const run = { id: "", key: r.key, local, job: { status: "running", stage: "" } };
  runs.push(run);
  render();
  try {
    const job = await postJSON("/api/fix", body);
    run.id = job.id;
    refreshJobs();
    setTimeout(refreshFixes, 1500); // once the fix has claimed its files
    follow(job.id);
    return job.id;
  } catch (e) {
    if (e.code === "rev" && !rev) {
      drop(run);
      render();
      const choice = await askRev(r.pr);
      return choice ? startFix(target, uncommitted, choice, r) : null;
    }
    if (e.code !== "uncommitted" || uncommitted) {
      run.job = { status: "error", error: e.message };
      paint();
      throw e;
    }
    drop(run);
    render();
    const choice = await askUncommitted();
    return choice ? startFix(target, choice, rev, r) : null;
  }
}

// A fix started from a button waits UNDO_MS first, with an Undo toast at
// the bottom of the window, so a click by mistake costs nothing. The chat
// agent's fixes, which the reader approved, start at once.
const UNDO_MS = 5000;
let pending = null; // { r, target, what, at, timer }

function startSoon(target, what) {
  if (running() || !S.result) return;
  if (pending) startPending(); // the one before goes ahead
  menuOpen = false;
  pending = { r: S.result, target, what, at: Date.now() + UNDO_MS };
  pending.timer = setInterval(() => (Date.now() >= pending.at ? startPending() : paintUndo()), 250);
  paintUndo();
}

function startPending() {
  const p = pending;
  clearPending();
  startFix(p.target, undefined, "", p.r).catch(() => {});
}

function clearPending() {
  clearInterval(pending?.timer);
  pending = null;
  paintUndo();
}

function paintUndo() {
  let el = $("#fix-undo");
  if (!pending) { el?.remove(); return; }
  if (!el) {
    el = document.createElement("div");
    el.id = "fix-undo";
    el.setAttribute("role", "status");
    el.addEventListener("click", (e) => {
      const b = e.target.closest("button");
      if (b?.value === "undo") clearPending();
      else if (b?.value === "now") startPending();
    });
    document.body.append(el);
  }
  const secs = Math.max(1, Math.ceil((pending.at - Date.now()) / 1000));
  const text = `${pending.what} starts in ${secs}s`;
  if (el.dataset.text === text) return;
  el.dataset.text = text;
  el.innerHTML = `<span>${esc(text)}</span><button class="linkbtn" value="now">Start now</button><button class="details-btn" value="undo">Undo</button>`;
}

document.addEventListener("keydown", (e) => {
  if (pending && e.key === "Escape") clearPending();
});

// askRev asks how to fix a review of a commit or branch that isn't checked
// out: check it out and fix it, or fix the checkout's current code.
function askRev(pr) {
  const dlg = $("#fix-rev");
  const what = pr.single_commit ? `commit <code>${esc(pr.rev)}</code>` : `branch <code>${esc(pr.rev)}</code>`;
  const branch = pr.single_commit ? `a new branch <code>pr-manager/${esc(pr.head_oid.slice(0, 10))}</code> at the commit` : pr.rev.startsWith("origin/") ? `a local <code>${esc(pr.rev.slice(7))}</code> branch tracking it` : `<code>${esc(pr.rev)}</code>`;
  dlg.querySelector(".fix-rev-what").innerHTML = `This is a review of ${what}, which isn't checked out in <code>${esc(pr.local_path)}</code>.`;
  dlg.querySelector(".fix-rev-checkout").innerHTML = `<b>Check out and fix</b> switches the checkout to ${branch} and fixes it there. The working tree has to be clean.`;
  dlg.querySelector("button[value=checkout]").textContent = `Check out ${pr.single_commit ? pr.rev : pr.rev.replace(/^origin\//, "")} and fix`;
  dlg.returnValue = "";
  dlg.showModal();
  return new Promise((res) => dlg.addEventListener("close", () => {
    const v = dlg.returnValue;
    res(v === "checkout" || v === "current" ? v : "");
  }, { once: true }));
}

// What to do with uncommitted local changes when a fix starts: commit
// them, fix in the current branch, or ask (unset).
const UNCOMMITTED = "pr-manager.fix_uncommitted";

// askUncommitted asks whether to commit the checkout's changes or fix in
// its current branch; "don't ask again" saves the answer.
function askUncommitted() {
  const dlg = $("#fix-uncommitted");
  const again = dlg.querySelector("#fix-uncommitted-save");
  again.checked = false;
  dlg.returnValue = "";
  dlg.showModal();
  return new Promise((res) => dlg.addEventListener("close", () => {
    const v = dlg.returnValue;
    const choice = v === "commit" || v === "branch" ? v : "";
    if (choice && again.checked) {
      localStorage.setItem(UNCOMMITTED, choice);
      const sel = $("#fix_uncommitted");
      if (sel) sel.value = choice;
    }
    res(choice);
  }, { once: true }));
}

// follow polls a fix job, keeping its banner current, and opens the fixed
// result when it's done and its PR is still on screen.
async function follow(id) {
  for (;;) {
    const j = await pollJob(id, () => {
      const run = byId(id);
      if (!run) return;
      run.job = { ...run.job, lost: true };
      paint();
    }).catch((e) => ({ status: "error", error: `connection lost: ${e.message}` }));
    const run = byId(id);
    if (!run) return;
    run.job = j;
    if (j.status === "cancelled") {
      drop(run);
      refreshJobs();
      refreshFixes();
      paint();
      return;
    }
    if (j.status === "done") {
      refreshJobs();
      if (!run.local) {
        // The fix is a commit on the PR's branch now, in the list.
        if (!j.warning) drop(run);
        refreshFixes();
        paint();
        return;
      }
      drop(run);
      if (j.key && S.result?.key === run.key) await onDone(j.key);
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
  else if (runs.some((r) => S.result?.key === r.key)) render();
}

// fixBanner shows the running or failed fix of the open result.
export function fixBanner() {
  const mine = runs.filter((r) => S.result?.key === r.key);
  if (!mine.length) return "";
  return `<div class="fix-banner">${mine.map(runBanner).join("")}</div>`;
}

function runBanner(run) {
  const log = run.id ? `<button class="linkbtn" data-act="fix-log" data-id="${esc(run.id)}">View log</button>` : "";
  const dismiss = `<button class="linkbtn" data-act="fix-dismiss" data-id="${esc(run.id)}">dismiss</button>`;
  if (run.job.status === "error") {
    return `<div class="tr-banner error" role="status">Fix failed: ${esc(run.job.error)} <span class="spacer"></span>${log}${dismiss}</div>`;
  }
  if (run.job.status === "done") {
    return `<div class="tr-banner warn" role="status">Fix done: ${esc(run.job.warning)}<span class="spacer"></span>${log}${dismiss}</div>`;
  }
  const warn = run.job.warning ? `<div class="tr-banner warn" role="status">${esc(run.job.warning)}</div>` : "";
  const stop = run.stopping ? `<span class="muted">stopping…</span>` : run.id && run.job.cancelable ? `<button class="linkbtn" data-act="fix-cancel" data-id="${esc(run.id)}" title="Stop the fix; what it changed so far is undone">Stop</button>` : "";
  return `<div class="tr-banner" role="status"><span class="spinner"></span>Fixing… ${esc(run.job.lost ? "connection lost, retrying…" : run.job.stage ? stageText(run.job) : "starting")}<span class="spacer"></span>${log}${stop}</div>${warn}`;
}

// fixDisabled disables a fix button while a fix runs and on a PR that
// isn't open (as of its triage). Uncommitted local changes are asked
// about when the fix starts.
export function fixDisabled() {
  const r = S.result;
  if (running()) return 'disabled title="A fix is running"';
  if (!r.pr.local_path && r.pr.state !== "OPEN") return `disabled title="The PR is ${esc(r.pr.state.toLowerCase())}; only open PRs and local repositories can be fixed"`;
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
  const why = !t.trusted ? "Left by the PR author or someone without write access. Read it first: its text goes to the fixer." : "The check did not confirm this comment";
  return `<button class="${cls}" data-act="fix-thread" data-unit="${esc(u.id)}" data-thread="${esc(t.id)}" ${fixDisabled() || (sure ? "" : `title="${why}"`)}>${sure ? "Fix comment" : "Fix anyway"}</button>`;
}

// The issues and threads picked in the Issues tab to fix together, for
// the result on screen.
let picked = { key: "", set: new Set() };
const pickKey = (unit, issue, thread) => thread ? `${unit}|t|${thread}` : `${unit}|i|${issue}`;
const picks = () => {
  if (picked.key !== S.result?.key) picked = { key: S.result?.key, set: new Set() };
  return picked.set;
};

// pickBox is a row's checkbox, to fix it along with the other picked ones.
export function pickBox(u, i, thread) {
  const k = pickKey(u.id, i, thread);
  return `<input type="checkbox" class="fix-pick" data-act="fix-pick" data-k="${esc(k)}" ${picks().has(k) ? "checked" : ""} ${fixDisabled() ? "disabled" : ""} aria-label="Pick to fix">`;
}

// fixPickedHTML is the toolbar's Fix selected and Select all, over the
// rows that can be picked now: a pick that got fixed or dismissed drops.
export function fixPickedHTML(rows) {
  const can = new Set(rows.map((r) => pickKey(r.u.id, r.i, r.thread)));
  const set = picks();
  for (const k of set) if (!can.has(k)) set.delete(k);
  if (!can.size) return "";
  const all = set.size === can.size;
  const dis = fixDisabled();
  return `<button class="linkbtn" data-act="fix-pick-all" data-keys="${esc([...can].join("\n"))}" ${dis}>${all ? "Select none" : "Select all"}</button>` +
    (set.size ? `<button class="details-btn primary" data-act="fix-picked" ${dis}>Fix ${set.size} selected</button>` : "");
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
  const issues = allUnits().reduce((n, { u }) => n + (u.issues || []).filter((i) => !i.dismissed && !i.same_as).length, 0);
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
  // A fix that fails to start shows in the banner; startFix also throws
  // it for the agent API.
  "fix-issue": (el) => startSoon({ unit_id: el.dataset.unit, issue: Number(el.dataset.issue) }, "Fixing the issue"),
  "fix-thread": (el) => startSoon({ unit_id: el.dataset.unit, thread: el.dataset.thread }, "Fixing the comment"),
  "fix-all": (el) => startSoon({ all: true, comments: !!el.dataset.onlyComments || includeComments() }, el.textContent.trim().replace(/^Fix/, "Fixing")),
  "fix-all-menu": () => { menuOpen = !menuOpen; },
  "fix-pick": (el) => {
    const set = picks();
    if (set.has(el.dataset.k)) set.delete(el.dataset.k);
    else set.add(el.dataset.k);
  },
  "fix-pick-all": (el) => {
    const set = picks(), keys = el.dataset.keys.split("\n");
    if (keys.every((k) => set.has(k))) set.clear();
    else keys.forEach((k) => set.add(k));
  },
  "fix-picked": () => {
    const targets = [...picks()].map((k) => {
      const [unit_id, kind, ...rest] = k.split("|");
      const id = rest.join("|");
      return kind === "t" ? { unit_id, issue: 0, thread: id } : { unit_id, issue: Number(id) };
    });
    if (!targets.length) return;
    picks().clear();
    startSoon({ targets }, `Fixing ${targets.length} selected`);
  },
  "fix-all-comments": (el) => { localStorage.setItem(INCLUDE, el.checked ? "1" : "0"); },
  "fix-log": (el) => { showLog(el.dataset.id); return false; },
  // Stops a running fix, from its banner or the Issues tab's list of
  // running fixes.
  "fix-cancel": async (el) => {
    const run = byId(el.dataset.id);
    if (run) { run.stopping = true; paint(); }
    try {
      await cancelJob(el.dataset.id);
    } catch (e) {
      if (run) run.stopping = false;
      say(e.message);
    }
    refreshFixes();
  },
  "fix-dismiss": (el) => { const r = byId(el.dataset.id) || runs.find((x) => !x.id); if (r) drop(r); },
};
