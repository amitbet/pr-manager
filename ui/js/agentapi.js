// Agent API: the app's data and what it can do, as functions. The chat
// agent proposes actions by name (the catalog is in chatactions.go) and
// js/chat.js runs them here; the console has the same as window.prm:
//
//   prm.result()                 the result on screen
//   prm.units()                  its units
//   prm.list()                   the actions, with their risk
//   await prm.run("open_unit", { unit: "a.go:main" })
//
// run resolves to a line saying what happened, which the chat keeps, with
// its time, as an event for the agent. Jobs (fixes, re-analysis) are
// followed until they end; the result they make is opened if the reader
// is still on the same change.
import { api, postJSON } from "./util.js";
import { S, render, syncURL, prBase, allUnits, localSrc } from "./state.js";
import { jumpToUnit } from "./review.js";
import { markReviewed } from "./walkthrough.js";
import { jobSettings } from "./settings.js";
import { pollJob, refreshJobs, stageText } from "./jobs.js";
import { startFix } from "./fix.js";
import { merge } from "./issues.js";
import { refreshFixes } from "./fixes.js";
import { fill } from "./viewctx.js";
import { snapshot } from "./snapshot.js";

let H = { showKey: async () => {} };

// changeOf names the change a result is of, the same across its re-runs
// and fixes: the PR, or the local checkout (and revision).
export const changeOf = (r) => !r?.pr ? "" : r.pr.local_path ? `local:${localSrc(r.pr)}` : `${r.pr.host || "github.com"}/${r.pr.owner}/${r.pr.repo}#${r.pr.number}`;

const need = (v, what) => { if (v === undefined || v === null || v === "") throw new Error(`missing ${what}`); return v; };
const unitOf = (id) => {
  const x = allUnits().find(({ u }) => u.id === id);
  if (!x) throw new Error(`no unit ${id} in this result`);
  return x;
};
const nameOf = (u) => u.symbol ? `${u.file.split("/").pop()} · ${u.symbol}` : u.file;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const resultURL = (suffix) => `/api/results/${encodeURIComponent(S.result.key)}${suffix}`;

// threadOf finds a review thread of the result by its id.
function threadOf(id) {
  for (const { u } of allUnits()) for (const t of u.threads || []) if (t.id === id) return { u, t };
  throw new Error(`no review thread ${id} on this PR`);
}

// unitState is how a unit stands, after a job changed it.
function unitState(id) {
  const x = allUnits().find(({ u }) => u.id === id);
  if (!x) return `${id}: no longer in the result`;
  const open = (x.u.issues || []).filter((i) => !i.dismissed && !i.same_as);
  return `${nameOf(x.u)}: ${x.u.decision?.bucket || "?"}${open.length ? `, ${open.length} open issue${open.length > 1 ? "s" : ""} (${open.map((i) => `${i.severity} "${i.title}"`).join("; ")})` : ", no open issues"}`;
}

// followJob polls a job to its end, telling progress its stage, and opens
// the result it made when the reader is still on the same change.
async function followJob(j, progress, open = true) {
  const change = changeOf(S.result), t0 = Date.now();
  refreshJobs();
  while (j.status === "running") {
    progress?.(stageText(j));
    await sleep(1000);
    j = await pollJob(j.id);
  }
  refreshJobs();
  if (j.status === "error") throw new Error(j.error || "the job failed");
  if (j.status === "cancelled") throw new Error("the reader stopped the job");
  j.secs = Math.round((Date.now() - t0) / 1000);
  if (open && j.key && changeOf(S.result) === change) await H.showKey(j.key);
  return j;
}

// fixOutcome says what a finished fix or change job did: what it fixed or
// changed, and where the commit is.
async function fixOutcome(j) {
  let fixed = "";
  if (j.key) {
    const r = await api(`/api/results/${encodeURIComponent(j.key)}`).catch(() => null);
    const f = r?.fixed_issues || [];
    const changed = f.filter((x) => x.severity === "change"), issues = f.filter((x) => x.severity !== "change");
    fixed = `${changed.length ? ` Changed: ${changed.map((x) => `"${x.title}"`).join("; ")}.` : ""}${issues.length ? ` Fixed: ${issues.map((x) => `"${x.title}"`).join("; ")}.` : ""}`;
    if (r && !f.length) fixed = " It recorded nothing as fixed or changed: see its warning, or the fix's diff in the fixes list.";
  }
  refreshFixes();
  return `fix job ${j.id} finished in ${j.secs}s${j.warning ? `, with a warning: ${j.warning}` : ""}.${fixed}${S.result.pr.local_path ? ` Its result ${j.key} is open.` : " The fix is a commit in the PR's fix checkout, not pushed; push_fix pushes it."}`;
}

const reanalyze = (body, progress) => postJSON(resultURL("/reanalyze"), { ...jobSettings(), ...body }).then((j) => followJob(j, progress));

// ACTIONS are the catalog. risk: view runs at once; local and job wait
// for the reader unless Settings → Chat agent lets them run; outward
// always waits.
export const ACTIONS = {
  open_unit: {
    risk: "view", label: (a) => `Open ${tryName(a.unit)}`,
    run: async (a) => { unitOf(need(a.unit, "unit")); jumpToUnit(a.unit); return `opened ${a.unit} in the Review tab`; },
  },
  open_tab: {
    risk: "view", label: (a) => `Open the ${a.tab} tab`,
    run: async (a) => {
      if (!["review", "issues", "sequence", "map"].includes(a.tab)) throw new Error(`no tab ${a.tab}`);
      S.tab = a.tab; syncURL(); render();
      return `opened the ${a.tab} tab`;
    },
  },
  snapshot: {
    // continues: the agent answers again when it is taken, to look at it.
    risk: "view", continues: true, label: () => "Take a snapshot of the page",
    run: async () => {
      const shot = await snapshot();
      let saved = "";
      if (shot.png) {
        const out = await postJSON(`/api/chats/snapshot?change=${encodeURIComponent(changeOf(S.result))}`, { png: shot.png });
        saved = ` [snapshot: ${out.path}]`;
      }
      return `took a snapshot of the page, ${shot.width}x${shot.height}, without the chat panel${saved || " (this browser can't draw the page as a picture; here is its text)"}. The page's text:\n${shot.text}`;
    },
  },
  fill_field: {
    risk: "view", label: (a) => `Write into ${FIELD_LABEL[a.field] || a.field}`, preview: (a) => a.text,
    run: async (a) => {
      const was = fill(need(a.field, "field"), need(a.text, "text"), a);
      return `wrote ${String(a.text).length} chars into ${FIELD_LABEL[a.field] || a.field}${was ? ` (it said before: ${JSON.stringify(was.slice(0, 2000))})` : ""}; the reader saves it`;
    },
  },
  draft_comment: {
    risk: "local", label: (a) => `${a.id ? "Rewrite the pending comment" : "Draft a comment"} on ${tryName(a.unit)} ${a.side === "LEFT" ? "old " : ""}line ${a.line}`, preview: (a) => a.body,
    run: async (a) => {
      const { u, f } = unitOf(need(a.unit, "unit"));
      const line = Number(need(a.line, "line"));
      const side = a.side === "LEFT" ? "LEFT" : "RIGHT";
      const near = (u.hunks || []).some((h) => side === "LEFT"
        ? line >= h.old_start - 3 && line < h.old_start + h.old_lines + 3
        : line >= h.new_start - 3 && line < h.new_start + h.new_lines + 3);
      if (!near) throw new Error(`${side === "LEFT" ? "old" : "new"} line ${line} is not a changed line of ${u.id} (or within 3 lines of one), so GitHub would not take a comment there`);
      if (a.id && !S.drafts.some((d) => d.id === a.id)) throw new Error(`no pending comment ${a.id}`);
      S.drafts = await postJSON(`${prBase()}/drafts`, { id: a.id || undefined, path: f.path, side, line, body: need(a.body, "body") });
      render();
      return `${a.id ? "rewrote" : "added"} a pending comment on ${f.path}:${line}${side === "LEFT" ? " (old file)" : ""} (${S.drafts.length} pending); nothing is posted until the review is submitted`;
    },
  },
  delete_draft: {
    risk: "local", label: (a) => `Delete pending comment ${a.id}`,
    run: async (a) => {
      S.drafts = await api(`${prBase()}/drafts/${encodeURIComponent(need(a.id, "id"))}`, { method: "DELETE" });
      render();
      return `deleted the pending comment (${S.drafts.length} left)`;
    },
  },
  dismiss: {
    risk: "local", label: (a) => `Dismiss ${a.kind} #${a.index} of ${tryName(a.unit)}`, preview: (a) => a.reason,
    run: async (a) => {
      const { u } = unitOf(need(a.unit, "unit"));
      const kind = a.kind === "lint" ? "lint" : "issue";
      const item = (kind === "lint" ? u.lint : u.issues)?.[Number(a.index)];
      if (!item) throw new Error(`${u.id} has no ${kind} #${a.index}`);
      merge(await postJSON(resultURL("/dismissals"), { unit: u.id, kind, index: Number(a.index), reason: need(a.reason, "reason") }));
      render();
      return `dismissed "${item.title || item.message}"; ${unitState(u.id)}`;
    },
  },
  restore: {
    risk: "local", label: (a) => `Restore ${a.kind} #${a.index} of ${tryName(a.unit)}`,
    run: async (a) => {
      const { u } = unitOf(need(a.unit, "unit"));
      const item = (a.kind === "lint" ? u.lint : u.issues)?.[Number(a.index)];
      if (!item?.dismiss_key) throw new Error(`that ${a.kind} is not dismissed`);
      merge(await api(`${resultURL("/dismissals")}/${encodeURIComponent(item.dismiss_key)}`, { method: "DELETE" }));
      render();
      return `restored "${item.title || item.message}"; ${unitState(u.id)}`;
    },
  },
  mark_reviewed: {
    risk: "local", label: (a) => `Mark ${(a.units || []).length} unit(s) ${a.reviewed === false ? "not reviewed" : "reviewed"}`,
    run: async (a) => {
      const ids = need(a.units, "units");
      ids.forEach(unitOf);
      markReviewed(ids, a.reviewed !== false);
      render();
      return `marked ${ids.join(", ")} ${a.reviewed === false ? "not reviewed" : "reviewed"}`;
    },
  },
  fix: {
    risk: "job", label: (a) => a.all ? `Fix all issues${a.comments ? " and confirmed comments" : ""}` : `Fix ${(a.targets || []).length} issue(s)`,
    run: async (a, progress) => {
      let target;
      if (a.all) target = { all: true, comments: !!a.comments };
      else {
        const ts = need(a.targets, "targets").map((x) => {
          const { u } = unitOf(need(x.unit, "unit"));
          if (x.thread) { threadOf(x.thread); return { unit_id: u.id, issue: 0, thread: x.thread }; }
          if (!u.issues?.[Number(x.issue)]) throw new Error(`${u.id} has no issue #${x.issue}`);
          return { unit_id: u.id, issue: Number(x.issue) };
        });
        target = ts.length === 1 ? ts[0] : { targets: ts };
      }
      const id = await startFix(target);
      if (!id) throw new Error("the fix did not start (another fix of this checkout is running, or the reader cancelled)");
      return fixOutcome(await followJob({ id, status: "running" }, progress, !!S.result.pr.local_path));
    },
  },
  change: {
    risk: "job", label: (a) => `Change ${(a.files || []).join(", ") || "the code"}`, preview: (a) => a.instructions,
    run: async (a, progress) => {
      const files = need(a.files, "files");
      if (!Array.isArray(files) || !files.length) throw new Error("files must list the files the change may touch");
      if (a.unit) unitOf(a.unit);
      const id = await startFix({ change: { instructions: need(a.instructions, "instructions"), files, unit_id: a.unit || "" } });
      if (!id) throw new Error("the change did not start (another fix of this checkout is running, or the reader cancelled)");
      return fixOutcome(await followJob({ id, status: "running" }, progress, !!S.result.pr.local_path));
    },
  },
  commit_edits: {
    risk: "job", label: (a) => `Commit the chat agent's edits${a.title ? `: "${a.title}"` : ""}`, preview: (a) => a.title,
    run: async (a, progress) => {
      if (a.unit) unitOf(a.unit);
      const id = await startFix({ change: { instructions: need(a.title, "title"), from_chat: changeOf(S.result), unit_id: a.unit || "" } });
      if (!id) throw new Error("the commit did not start (another fix of this checkout is running, or the reader cancelled)");
      return fixOutcome(await followJob({ id, status: "running" }, progress, !!S.result.pr.local_path));
    },
  },
  reanalyze_units: {
    risk: "job", label: (a) => `Review ${(a.units || []).map(tryName).join(", ")} again`,
    run: async (a, progress) => {
      const ids = need(a.units, "units");
      ids.forEach(unitOf);
      const j = await reanalyze({ what: "units", units: ids }, progress);
      return `re-reviewed in ${j.secs}s: ${ids.map(unitState).join(" | ")}`;
    },
  },
  retriage: {
    risk: "job", label: (a) => a.fresh ? "Triage everything again from scratch" : "Triage the change again",
    run: async (a, progress) => {
      const j = await reanalyze({ what: "all", fresh: !!a.fresh }, progress);
      const c = S.result.counts || {};
      return `triaged again in ${j.secs}s${j.cached ? " (the saved result was still current)" : ""}: human ${c.human || 0}, skim ${c.skim || 0}, auxiliary ${c.aux || 0}, none ${c.none || 0}${S.result.carried?.reused ? `; ${S.result.carried.reused} unit reviews kept` : ""}`;
    },
  },
  refresh_comments: {
    risk: "job", label: () => "Fetch the GitHub review comments again",
    run: async (_, progress) => {
      const j = await reanalyze({ what: "threads" }, progress);
      const st = S.result.threads || {};
      return `fetched the review comments in ${j.secs}s: ${st.open ?? "?"} open thread(s), ${st.resolved ?? 0} resolved, ${st.outdated ?? 0} outdated`;
    },
  },
  regenerate: {
    risk: "job", label: (a) => `Write the ${a.what} again`,
    run: async (a, progress) => {
      if (a.what !== "overview" && a.what !== "sequence") throw new Error(`can't write ${a.what} again`);
      const j = await reanalyze({ what: a.what }, progress);
      return `wrote the ${a.what} again in ${j.secs}s`;
    },
  },
  reindex_codemap: {
    risk: "job", label: () => "Rebuild the code map",
    run: async (_, progress) => {
      const j = await followJob(await postJSON("/api/codemap/index", jobSettings()), progress, false);
      S.trees = {};
      const r = j.result || {};
      return `rebuilt the code map in ${j.secs}s: ${r.repos ?? "?"} repos${r.failed?.length ? `, ${r.failed.length} failed` : ""}; impact scores change at the next triage`;
    },
  },
  reply_thread: {
    risk: "outward", label: (a) => `Reply on GitHub to ${tryThread(a.thread)}`, preview: (a) => a.body,
    run: async (a) => {
      threadOf(need(a.thread, "thread"));
      const out = await postJSON(resultURL("/threads/reply"), { thread: a.thread, body: need(a.body, "body") });
      return out.dry_run ? "dry run: the reply was not posted" : `posted a reply: ${out.url}`;
    },
  },
  resolve_thread: {
    risk: "outward", label: (a) => `Resolve the GitHub thread ${tryThread(a.thread)}`,
    run: async (a) => {
      threadOf(need(a.thread, "thread"));
      const out = await postJSON(resultURL("/threads/resolve"), { thread: a.thread });
      return out.dry_run ? "dry run: the thread was not resolved" : `resolved ${out.thread}`;
    },
  },
  submit_review: {
    risk: "outward", label: (a) => `Submit the review on GitHub as ${a.event || "COMMENT"} with ${S.drafts.length} comment(s)`, preview: (a) => a.body,
    run: async (a) => {
      if (S.result.pr.local_path) throw new Error("a local review has no PR to submit to");
      const n = S.drafts.length;
      const out = await postJSON(`${prBase()}/review`, { event: a.event || "COMMENT", body: a.body || "", commit_id: S.result.pr.head_oid });
      if (out.dry_run) return "dry run: the review was not posted; the pending comments are kept";
      S.drafts = [];
      render();
      return `posted the review with ${n} comment(s): ${out.html_url}`;
    },
  },
  push_fix: {
    risk: "outward", label: () => `Push the fixes to ${S.result?.pr.head_ref || "the PR's branch"}`,
    run: async () => {
      const out = await postJSON(resultURL("/fixes/push"), {});
      refreshFixes();
      return `pushed: the PR's head is ${String(out.head || "").slice(0, 10)} now; retriage reviews the new head`;
    },
  },
};

const FIELD_LABEL = { comment: "the comment box", dismiss_reason: "the dismissal's reason", review_summary: "the review summary" };

function tryName(id) {
  try { return nameOf(unitOf(id).u); } catch { return String(id); }
}
function tryThread(id) {
  try { const { t } = threadOf(id); return `@${t.comments?.[0]?.author || "?"} on ${t.path}:${t.line}`; } catch { return String(id); }
}

// describe is how an action reads in the chat: its label, and the text
// it would post or save, if any.
export function describe(name, args = {}) {
  const a = ACTIONS[name];
  if (!a) return { label: `Unknown action ${name}`, risk: "outward" };
  return { label: a.label(args), preview: a.preview?.(args), risk: a.risk, continues: !!a.continues };
}

// run runs an action on the result on screen. progress gets a job's stage.
export async function run(name, args = {}, progress) {
  const a = ACTIONS[name];
  if (!a) throw new Error(`no action ${name}`);
  if (!S.result) throw new Error("no result is open");
  return a.run(args || {}, progress);
}

// initAgentAPI takes main.js's showKey, to open the results jobs make.
export function initAgentAPI(hooks) {
  H = { ...H, ...hooks };
  window.prm = {
    result: () => S.result,
    units: () => allUnits().map(({ u }) => u),
    drafts: () => S.drafts,
    list: () => Object.fromEntries(Object.entries(ACTIONS).map(([k, v]) => [k, v.risk])),
    describe, run,
  };
}
