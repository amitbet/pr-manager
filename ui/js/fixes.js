// Pending fixes, which a new triage of the PR can't see. A PR's fixes are
// commits on its branch in the repository's fix checkout: the Issues tab
// lists them with what each fixed and which earlier fix's lines it
// changed, shows their changes, pushes the branch, drops a fix, and
// completes the PR. Fixes made before that, each in a worktree or clone
// of its own (and a local review's), are listed as they are, to commit,
// push or throw away. The issues fixes fixed, or are fixing, say so.
import { esc, api, postJSON, ask } from "./util.js";
import { S, render, syncURL } from "./state.js";
import { SEV_CLASS } from "./scores.js";
import { fixSettings } from "./settings.js";
import { triageURL } from "./triage.js";
import { prepare, unitRows, diffTable } from "./diff.js";

let open = async () => {};

// initFixes sets what opens a fix's result (its key).
export function initFixes(show) {
  open = show;
}

// F is the fixes of the result on screen: list and marks from the server,
// diffs by key once asked for, the issue whose changes a diff shows, and
// the action running on each key.
let F = { key: "", loading: false, list: [], running: [], marks: new Map(), diffs: {}, shown: new Set(), focus: new Map(), collapsed: new Set(), busy: {}, error: "", note: null };

// rowId names a listed fix: a commit on the PR's branch, which a fix made
// or not, or a fix result in a checkout of its own.
const rowId = (p) => p.kind === "branch" ? `c:${p.commit}` : p.key;
const byRow = (id) => F.list.find((p) => rowId(p) === id);

const markKey = (unit, issue, thread) => thread ? `${unit}|t|${thread}` : `${unit}|i|${issue}`;

async function load(key) {
  F = { ...F, key, loading: true, error: "" };
  try {
    const out = await api(`/api/results/${encodeURIComponent(key)}/fixes`);
    if (F.key !== key) return;
    F.list = out.fixes || [];
    F.running = out.running || [];
    F.marks = new Map((out.marks || []).map((m) => [markKey(m.unit, m.issue, m.thread), m]));
    for (const k of Object.keys(F.diffs)) if (!byRow(k)) delete F.diffs[k];
  } catch (e) {
    if (F.key !== key) return;
    F.list = [];
    F.running = [];
    F.marks = new Map();
    F.error = e.message;
  }
  F.loading = false;
  if (S.result?.key === key) render();
}

// ensureFixes loads the open result's fixes the first time it, or an
// issue a fix marks, is shown.
function ensureFixes() {
  if (S.result && F.key !== S.result.key) {
    F = { key: "", loading: false, list: [], running: [], marks: new Map(), diffs: {}, shown: new Set(), focus: new Map(), collapsed: new Set(), busy: {}, error: "", note: null };
    load(S.result.key);
  }
}

const reload = () => load(S.result.key);

// refreshFixes reloads the open result's fixes, as when a fix of it ends.
export const refreshFixes = () => { if (S.result && F.key === S.result.key) reload(); };

const STATE = {
  uncommitted: ["fixed · not committed", "medium"],
  committed: ["fixed · committed, not pushed", "low"],
  pushed: ["fixed · pushed", "low"],
  superseded: ["superseded by the PR", "unknown"],
  running: ["fix running", "medium"],
  waiting: ["fix waiting", "unknown"],
};

// stateChip says where a fix's changes are.
const stateChip = (state) => {
  const [label, cls] = STATE[state] || [state, "unknown"];
  return `<span class="dz ${cls} fix-state ${esc(state)}">${esc(label)}</span>`;
};

// issueFixMark is the chip and link an issue or thread a pending fix fixed
// gets in place of its Fix button, or "".
export function issueFixMark(u, i, thread) {
  const m = fixMark(u, i, thread);
  if (!m) return "";
  if (m.state === "running" || m.state === "waiting") {
    return `<span title="${esc(m.reason ? `Waiting: ${m.reason}` : "A fix is working on this")}">${stateChip(m.state)}</span>`;
  }
  const swept = m.swept ? `<span class="chip" title="${esc(m.reason || "")}">resolved along the way</span> ` : "";
  const title = thread ? "the review thread" : u.issues?.[i]?.title || "";
  return `${swept}${stateChip(m.state)} <button class="linkbtn" data-act="pf-goto" data-key="${esc(m.key)}" data-commit="${esc(m.commit || "")}"
    data-unit="${esc(u.id)}" data-file="${esc(u.file)}" data-title="${esc(title)}" title="The changes the fix made in this code">Review fix</button>`;
}

// fixMark is the pending fix that fixed an issue or thread, or the fix
// running on it, if any; fixedBy only the first.
export const fixMark = (u, i, thread) => {
  ensureFixes();
  return (F.key === S.result?.key && F.marks.get(markKey(u.id, i, thread))) || null;
};
export const fixedBy = (u, i, thread) => {
  const m = fixMark(u, i, thread);
  return m && m.state !== "running" && m.state !== "waiting" ? m : null;
};

const where = (p) => p.location === "clone" ? "cached clone" : p.location === "branch" ? "your checkout" : p.location === "repo" ? "the repository's fix checkout" : "worktree";
const pushable = (p) => !p.no_push && (p.state === "uncommitted" || p.state === "committed");
const ownCheckout = (p) => p.kind !== "branch";

function totals(p) {
  const adds = p.files.reduce((n, f) => n + f.adds, 0), dels = p.files.reduce((n, f) => n + f.dels, 0);
  return `${p.files.length} file${p.files.length === 1 ? "" : "s"} <span class="pf-add">+${adds}</span> <span class="pf-del">−${dels}</span>`;
}

function fixedList(p) {
  const id = rowId(p);
  if (!p.fixed?.length) return `<div class="hint">No issue was recorded as fixed. Fixes made before this version don't record what they fixed.</div>`;
  const note = p.inferred ? `<div class="hint">This fix was made before fixes recorded what they fixed: these are the issues of this review its result no longer has.</div>` : "";
  return `${note}<ul class="pf-fixed">${p.fixed.map((f) => `<li><span class="dz ${SEV_CLASS[f.severity] || "unknown"}">${esc(f.severity || "comment")}</span>
      ${f.url ? `<a href="${esc(f.url)}" target="_blank" rel="noopener">${esc(f.title)}</a>` : esc(f.title)}
      <span class="muted">${esc(f.unit_id)}</span>
      <button class="linkbtn" data-act="pf-focus" data-id="${esc(id)}" data-unit="${esc(f.unit_id)}" data-file="${esc(f.file)}" data-title="${esc(f.title)}">its changes</button>${f.swept ? ` <span class="chip" title="${esc(f.reason || "")}">resolved along the way</span>` : ""}</li>`).join("")}</ul>`;
}

// diffHTML is a fix's changes, file by file, in the Review tab's file
// cards and diff tables.
function diffHTML(p) {
  const id = rowId(p);
  const d = F.diffs[id];
  if (!d) return `<div class="hint">Loading the changes…</div>`;
  if (d.error) return `<div class="dismiss-error">${esc(d.error)}</div>`;
  if (!d.files.length) return `<div class="hint">No changes.</div>`;
  const { shown, note } = focused(p, d);
  const view = `<div class="toolbar">${note}<span class="spacer"></span>
      <span class="seg"><button class="${S.view === "split" ? "on" : ""}" data-act="view" data-v="split">Split</button><button class="${S.view === "unified" ? "on" : ""}" data-act="view" data-v="unified">Unified</button></span></div>`;
  return view + shown.map(({ f, units }) => {
    const collapsed = F.collapsed.has(`${id}|${f.path}`);
    const hunks = units.flatMap((u) => u.hunks).sort((a, b) => a.new_start - b.new_start || a.old_start - b.old_start);
    const rows = f.binary ? [] : unitRows(f, { hunks });
    const n = (c) => hunks.reduce((k, h) => k + h.lines.filter((l) => l[0] === c).length, 0);
    const [adds, dels] = units === f.units ? [f.adds, f.dels] : [n("+"), n("-")];
    return `<div class="file ${collapsed ? "collapsed" : ""}">
      <div class="file-head" data-act="pf-collapse" data-key="${esc(id)}" data-path="${esc(f.path)}">
        <span class="path">${f.old_path && f.old_path !== f.path ? esc(f.old_path) + " → " : ""}${esc(f.path)}</span>
        <span class="status">${esc(f.status)}${f.binary ? ", binary" : ""}</span>
        <span class="counts"><span class="pf-add">+${adds}</span> <span class="pf-del">−${dels}</span></span>
      </div>
      <div class="units"><div class="diffwrap">${f.binary ? `<div class="nodiff">binary file</div>` : diffTable(f, rows, S.view)}</div></div>
    </div>`;
  }).join("");
}

// focused is the files and units of a fix's diff to show: when it was
// opened for an issue, the changes it made in the issue's unit (or file,
// when the fix's units don't have it), and what note says about them.
function focused(p, d) {
  const all = d.files.map((f) => ({ f, units: f.units }));
  const fo = F.focus.get(rowId(p));
  if (!fo) return { shown: all, note: "" };
  const others = (p.fixed?.length || 1) - 1;
  const rest = others > 0 ? ` The fix's other changes are for the ${others} other issue${others === 1 ? "" : "s"} it fixed.` : "";
  const what = fo.title ? ` for “${esc(fo.title)}”` : "";
  const toggle = `<button class="linkbtn" data-act="pf-unfocus" data-id="${esc(rowId(p))}">Show all its changes</button>`;
  let shown = d.files.map((f) => ({ f, units: f.units.filter((u) => u.uid === fo.unit) })).filter((x) => x.units.length);
  let where = `<code>${esc(fo.unit)}</code>`;
  if (!shown.length) {
    shown = all.filter(({ f }) => f.path === fo.file);
    where = `<code>${esc(fo.file)}</code>`;
  }
  if (!shown.length) return { shown: all, note: `<span class="pf-focus">The fix changed nothing in <code>${esc(fo.file)}</code>${what}; these are all its changes.</span>` };
  return { shown, note: `<span class="pf-focus">Its changes in ${where}${what}.${rest} ${toggle}</span>` };
}

// touchesHTML says which earlier fixes' lines a fix on the branch changed.
function touchesHTML(p) {
  if (!p.touches?.length) return "";
  return `<div class="pf-touches">${p.touches.map((t) => {
    const other = F.list.find((x) => x.commit === t.commit);
    const what = other?.fixed?.length ? other.fixed.map((f) => f.title).join("; ") : `fix ${t.commit.slice(0, 10)}`;
    return `<span class="chip warn" title="${esc(what)}">changes lines ${esc(t.lines.join(", "))} of <code>${esc(t.file)}</code> that an earlier fix wrote</span> <button class="linkbtn" data-act="pf-goto-row" data-id="c:${esc(t.commit)}">that fix</button>`;
  }).join(" ")}</div>`;
}

// branchRow is one commit on the PR's branch.
function branchRow(p) {
  const id = rowId(p);
  const busy = F.busy[id];
  const dis = Object.keys(F.busy).length ? "disabled" : "";
  const title = p.other ? esc(p.subject || "A commit no fix made") : p.fixed?.length ? `${p.fixed.length} fixed` : "Fix";
  const btns = [
    `<button class="details-btn" data-act="pf-diff" data-id="${esc(id)}">${F.shown.has(id) ? "Hide changes" : "Review changes"}</button>`,
    p.state === "superseded" ? `<button class="details-btn" data-act="pf-drop" data-id="${esc(id)}" ${dis} title="Take it off this list">${busy === "drop" ? "Dismissing…" : "Dismiss"}</button>` : "",
    p.state !== "pushed" && p.state !== "superseded" ? `<button class="details-btn" data-act="pf-drop" data-id="${esc(id)}" ${dis} title="Take this commit off the PR's branch">${busy === "drop" ? "Dropping…" : "Drop"}</button>` : "",
    p.key && p.key !== S.result.key ? `<button class="linkbtn" data-act="pf-open" data-key="${esc(p.key)}">Open fix result</button>` : "",
  ].filter(Boolean).join(" ");
  return `<li class="pf ${esc(p.state)}${p.stale ? " stale" : ""}" id="pf-${esc(id)}">
      <div class="claim-head">${stateChip(p.state)}
        <b>${title}</b>
        <span class="muted">${p.created_at && !p.created_at.startsWith("0001") ? new Date(p.created_at).toLocaleString() + " · " : ""}${p.other ? "" : `${p.rounds} round${p.rounds === 1 ? "" : "s"} · `}${totals(p)} · <code>${esc(p.commit.slice(0, 10))}</code></span>
        <span class="spacer"></span>${btns}
      </div>
      ${touchesHTML(p)}
      ${p.state === "superseded" ? `<div class="hint">The PR changed the same lines in <code>${esc(p.superseded_by.slice(0, 10))}</code>, so its version was kept and this ${p.other ? "commit" : "fix"} was dropped. Fix the issues again if they are still there.</div>` : ""}
      ${p.other ? "" : `<details class="pf-issues"><summary>What it fixed</summary>${fixedList(p)}</details>`}
      ${F.shown.has(id) ? `<div class="pf-diff">${diffHTML(p)}</div>` : ""}
    </li>`;
}

function fixRow(p) {
  if (p.kind === "branch") return branchRow(p);
  const busy = F.busy[p.key];
  const dis = busy || Object.keys(F.busy).length ? "disabled" : "";
  const here = p.key === S.result.key;
  const btns = [
    `<button class="details-btn" data-act="pf-diff" data-id="${esc(p.key)}">${F.shown.has(p.key) ? "Hide changes" : "Review changes"}</button>`,
    p.state === "uncommitted" ? `<button class="details-btn" data-act="pf-commit" data-key="${esc(p.key)}" ${dis} title="${p.location === "branch" ? "Commits everything uncommitted in your checkout" : "Commit in the fix checkout, with a message the summarizer writes"}">${busy === "commit" ? "Committing…" : "Commit"}</button>` : "",
    pushable(p) ? `<button class="details-btn primary" data-act="pf-push" data-key="${esc(p.key)}" ${dis} title="Commit what is uncommitted and push it to the PR's branch">${busy === "push" ? "Pushing…" : "Push"}</button>` : "",
    p.location !== "branch" ? `<button class="details-btn" data-act="pf-discard" data-key="${esc(p.key)}" ${dis}>${busy === "discard" ? "Discarding…" : "Discard"}</button>` : "",
    here ? "" : `<button class="linkbtn" data-act="pf-open" data-key="${esc(p.key)}">Open fix result</button>`,
  ].filter(Boolean).join(" ");
  return `<li class="pf ${esc(p.state)}${p.stale ? " stale" : ""}" id="pf-${esc(p.key)}">
      <div class="claim-head">${stateChip(p.state)}${p.stale ? `<span class="chip" title="The fix started from ${esc(p.base.slice(0, 10))}, and this review is of ${esc(S.result.pr.head_oid.slice(0, 10))}">older head</span>` : ""}
        <b>${p.fixed?.length ? `${p.fixed.length} fixed` : "Fix"}</b>
        <span class="muted">${new Date(p.created_at).toLocaleString()} · ${p.rounds} round${p.rounds === 1 ? "" : "s"} · ${totals(p)}${p.commits ? ` · ${p.commits} commit${p.commits === 1 ? "" : "s"}` : ""}</span>
        <span class="spacer"></span>${btns}
      </div>
      <div class="pf-where muted"><code>${esc(p.branch || "detached")}</code> in ${where(p)} <code>${esc(p.dir)}</code>${p.pushed_as ? ` · pushed as <code>${esc(p.pushed_as.slice(0, 10))}</code>` : ""}${p.no_push && p.state !== "pushed" ? ` · not pushable: ${esc(p.no_push)}` : ""}</div>
      <details class="pf-issues"><summary>What it fixed</summary>${fixedList(p)}</details>
      ${F.shown.has(p.key) ? `<div class="pf-diff">${diffHTML(p)}</div>` : ""}
    </li>`;
}

// noteHTML is the outcome of the last push, commit or discard.
function noteHTML() {
  const n = F.note;
  if (!n) return "";
  if (n.error) return `<div class="tr-banner error" role="status">${esc(n.error)}<span class="spacer"></span><button class="linkbtn" data-act="pf-note-close">dismiss</button></div>`;
  return `<div class="tr-banner" role="status">${esc(n.text)}<span class="spacer"></span>${n.retriage ? `<button class="linkbtn" data-act="pf-retriage">Triage the PR again</button>` : ""}<button class="linkbtn" data-act="pf-note-close">dismiss</button></div>`;
}

// pendingFixesHTML is the box at the top of the Issues tab, or "" when
// the PR has no fix waiting.
export function pendingFixesHTML() {
  ensureFixes();
  if (F.error) return `<div class="tr-banner error" role="status">Couldn't read the fixes: ${esc(F.error)}</div>`;
  if (!F.list.length && !F.running.length) return noteHTML();
  const own = F.list.filter((p) => ownCheckout(p) && pushable(p));
  const onBranch = F.list.filter((p) => p.kind === "branch");
  const toPush = onBranch.filter(pushable);
  const dis = Object.keys(F.busy).length ? "disabled" : "";
  const branch = S.result.pr.head_ref;
  const place = onBranch[0] ? ` on <code>${esc(onBranch[0].branch)}</code> in <code>${esc(onBranch[0].dir)}</code>` : "";
  return `${noteHTML()}<section class="pending-fixes">
      <div class="toolbar"><b>Fixes not on the PR yet</b>
        <span class="muted">${S.result.pr.local_path ? "in fix checkouts of this repository" : `${place}; pushed to <code>${esc(branch)}</code> they become the PR's next commits`}</span>
        <span class="spacer"></span>
        <button class="linkbtn" data-act="pf-reload" ${dis}>${F.loading ? "Refreshing…" : "Refresh"}</button>
        ${own.length > 1 ? `<button class="details-btn primary" data-act="pf-push-all" ${dis} title="Commit each fix and push them to the PR's branch as one series">${F.busy["*"] ? "Pushing…" : `Push all ${own.length}`}</button>` : ""}
        ${toPush.length ? `<button class="details-btn primary" data-act="pf-push-branch" ${dis} title="Push the PR's branch, every fix on it, to ${esc(branch)}">${F.busy.branch === "push" ? "Pushing…" : `Push ${toPush.length} fix${toPush.length === 1 ? "" : "es"}`}</button>` : ""}
        ${!S.result.pr.local_path && (onBranch.length || F.running.length) ? `<button class="details-btn" data-act="pf-complete" ${dis || (F.running.length ? "disabled" : "")} title="Done fixing this PR: removes its branch and fix results, and the repository's fix checkout when no other PR has fixes in it. Happens on its own 10 days after the last fix.">${F.busy.branch === "complete" ? "Completing…" : "Complete"}</button>` : ""}
      </div>
      ${runningHTML()}
      <ul class="claims">${F.list.map(fixRow).join("")}</ul>
    </section>`;
}

// runningHTML is the fixes of the PR running, or waiting their turn, in
// the repository's checkout.
function runningHTML() {
  if (!F.running.length) return "";
  return `<ul class="pf-running">${F.running.map((c) => `<li><span class="spinner"></span>
      ${c.waiting ? `Waiting: ${esc(c.waiting)}` : "Fixing"} · ${c.targets?.length || 0} issue${c.targets?.length === 1 ? "" : "s"} in ${c.files.map((f) => `<code>${esc(f)}</code>`).join(", ")}</li>`).join("")}</ul>`;
}

async function showDiff(id) {
  const p = byRow(id);
  if (!p) return;
  F.shown.add(id);
  if (F.diffs[id] && !F.diffs[id].error) return render();
  delete F.diffs[id];
  render();
  try {
    const d = await api(p.kind === "branch"
      ? `/api/results/${encodeURIComponent(S.result.key)}/fixes/commit/${encodeURIComponent(p.commit)}`
      : `/api/results/${encodeURIComponent(p.key)}/fix/diff`);
    // One unit per file, so the Review tab's diff tables can draw it.
    for (const f of d.files) {
      f.readonly = true;
      const units = f.units?.length ? f.units : [{ id: f.path, hunks: f.hunks || [] }];
      f.units = units.map((u) => ({ id: `${id}|${u.id}`, uid: u.id, hunks: u.hunks || [] }));
    }
    prepare(d);
    F.diffs[id] = d;
  } catch (e) {
    F.diffs[id] = { error: e.message };
  }
  render();
}

// gotoRow shows a listed fix's changes, those for an issue when focus
// names it, and scrolls to it on the Issues tab.
function gotoRow(id, focus) {
  if (S.tab !== "issues") { S.tab = "issues"; syncURL(); }
  if (focus?.unit) F.focus.set(id, focus);
  else F.focus.delete(id);
  showDiff(id).then(() => document.getElementById(`pf-${id}`)?.scrollIntoView({ block: "start", behavior: "smooth" }));
  return false;
}

// run does one action on a fix, with its button busy, then reloads the list.
async function run(key, what, fn) {
  F.busy[key] = what;
  F.note = null;
  render();
  try {
    F.note = await fn();
  } catch (e) {
    F.note = { error: e.message };
  }
  delete F.busy[key];
  delete F.diffs[key];
  await reload();
  return false;
}

async function push(keys, busyKey) {
  const branch = S.result.pr.head_ref;
  const n = keys.length;
  if (!(await ask(`Commit ${n === 1 ? "this fix" : `these ${n} fixes`} and push ${n === 1 ? "it" : "them"} to ${branch} on GitHub? It becomes the PR's new head.`, "Commit and push"))) return false;
  return run(busyKey, "push", async () => {
    const out = await postJSON("/api/fixes/push", { ...fixSettings(), keys });
    return { text: `Pushed to ${branch}: the PR's head is ${out.head.slice(0, 10)} now. Triage it again to review the new head.`, retriage: true };
  });
}

export const actions = {
  // A fix result's key: its row, a commit on the branch or a checkout.
  "pf-goto": (el) => {
    const { key, commit, unit, file, title } = el.dataset;
    const p = (commit && F.list.find((x) => x.commit === commit)) || F.list.find((x) => x.key === key);
    return p ? gotoRow(rowId(p), { unit, file, title }) : false;
  },
  "pf-focus": (el) => {
    const { id, unit, file, title } = el.dataset;
    return gotoRow(id, { unit, file, title });
  },
  "pf-unfocus": (el) => { F.focus.delete(el.dataset.id); },
  "pf-goto-row": (el) => gotoRow(el.dataset.id),
  "pf-diff": (el) => {
    const id = el.dataset.id;
    F.focus.delete(id);
    if (F.shown.has(id)) { F.shown.delete(id); return; }
    showDiff(id);
    return false;
  },
  "pf-push-branch": async () => {
    const branch = S.result.pr.head_ref;
    if (!(await ask(`Push the PR's fixes to ${branch} on GitHub? They become the PR's new commits.`, "Push"))) return false;
    return run("branch", "push", async () => {
      const out = await postJSON(`/api/results/${encodeURIComponent(S.result.key)}/fixes/push`, {});
      return { text: `Pushed to ${branch}: the PR's head is ${out.head.slice(0, 10)} now. Triage it again to review the new head.`, retriage: true };
    });
  },
  "pf-drop": async (el) => {
    const p = byRow(el.dataset.id);
    const gone = p?.state === "superseded";
    if (!p || (!gone && !(await ask(`Take ${p.other ? "this commit" : "this fix"} off the PR's branch? Its changes are gone; the fixes after it stay.`, "Drop")))) return false;
    return run(rowId(p), "drop", async () => {
      await postJSON(`/api/results/${encodeURIComponent(S.result.key)}/fixes/drop`, { commit: p.commit });
      return { text: gone ? "Dismissed the superseded fix." : "Dropped the fix." };
    });
  },
  "pf-complete": async () => {
    const unpushed = F.list.filter((p) => p.kind === "branch" && p.state !== "pushed" && p.state !== "superseded").length;
    if (!(await ask(`Done fixing this PR? Its fix branch and fix results are removed${unpushed ? `, with ${unpushed} fix${unpushed === 1 ? "" : "es"} not pushed` : ""}. This can't be undone.`, "Done fixing"))) return false;
    return run("branch", "complete", async () => {
      await postJSON(`/api/results/${encodeURIComponent(S.result.key)}/fixes/complete`, {});
      return { text: "Completed: the PR's fixes are cleaned up." };
    });
  },
  "pf-collapse": (el) => {
    const k = `${el.dataset.key}|${el.dataset.path}`;
    if (!F.collapsed.delete(k)) F.collapsed.add(k);
  },
  "pf-reload": () => { reload(); return false; },
  "pf-open": (el) => { open(el.dataset.key); return false; },
  "pf-note-close": () => { F.note = null; },
  "pf-retriage": () => { F.note = null; triageURL(S.result.pr.url); return false; },
  "pf-commit": (el) => run(el.dataset.key, "commit", async () => {
    const p = await postJSON(`/api/results/${encodeURIComponent(el.dataset.key)}/fix/commit`, fixSettings());
    return { text: `Committed on ${p.branch} at ${p.head.slice(0, 10)}.` };
  }),
  "pf-push": (el) => push([el.dataset.key], el.dataset.key),
  "pf-push-all": () => push(F.list.filter(pushable).map((p) => p.key), "*"),
  "pf-discard": async (el) => {
    const p = F.list.find((x) => x.key === el.dataset.key);
    if (!p || !(await ask(`Throw away this fix's changes${p.state === "committed" ? " and commits" : ""}, its ${where(p)} and branch ${p.branch}? This can't be undone.`, "Throw away"))) return false;
    if (p.key !== S.result.key) {
      return run(p.key, "discard", async () => {
        await postJSON(`/api/results/${encodeURIComponent(p.key)}/fix/discard`, {});
        return { text: "Discarded the fix." };
      });
    }
    // The result on screen is one of the fix's, which go with it.
    F.busy[p.key] = "discard";
    render();
    postJSON(`/api/results/${encodeURIComponent(p.key)}/fix/discard`, {})
      .then(() => { location.href = location.pathname; })
      .catch((e) => { delete F.busy[p.key]; F.note = { error: e.message }; render(); });
    return false;
  },
};
