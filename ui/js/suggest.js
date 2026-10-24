// Suggested fixes on the Issues tab (suggest.go): the patch the fixer would
// make for one issue or review comment, made in a scratch copy, shown
// under its row before anything changes. Making one is a job, listed in
// the sidebar with the others; its row shows the stage and the time it
// has taken, with its log and Stop. Apply starts a fix whose first round
// applies that patch, then checks, tests and audits it.
import { esc, api, postJSON } from "./util.js";
import { S, render } from "./state.js";
import { jobSettings } from "./settings.js";
import { refreshJobs, pollJob, stageText, watchJob, cancelJob } from "./jobs.js";

const open = new Set(); // rows whose suggestion is shown, by key
const running = new Map(); // row key -> { id, started, job } of its job
const failed = new Map(); // row key -> why the last try failed

// rowKey names an issue (by its title, and which of the unit's issues with
// that title it is: indexes move) or a thread.
const rowKey = (u, is, t) => `${S.result?.key}|${u.id}|${t ? `t:${t.id}` : `i:${is.title}#${(u.issues || []).filter((x) => x.title === is.title).indexOf(is)}`}`;

// suggestionFor is the result's suggestion for an issue or thread.
export function suggestionFor(u, is, t) {
  return (S.result?.suggestions || []).find((s) => s.unit_id === u.id && (t ? s.thread === t.id : !s.thread && s.title === is.title));
}

// suggestButton is the row's Suggest fix, or the toggle for one made.
export function suggestButton(u, i, t) {
  const is = t ? null : u.issues[i];
  const k = rowKey(u, is, t);
  const target = t ? `data-thread="${esc(t.id)}"` : `data-issue="${i}"`;
  if (running.has(k)) {
    const r = running.get(k);
    return `<span class="suggesting" data-sk="${esc(k)}"><span class="spinner"></span><span class="suggest-what">${esc(progressText(r))}</span>
      <button class="linkbtn" data-act="suggest-log" data-id="${esc(r.id)}">log</button>
      <button class="linkbtn" data-act="suggest-stop" data-id="${esc(r.id)}">stop</button></span>`;
  }
  if (suggestionFor(u, is, t)) {
    return `<button class="linkbtn" data-act="suggest-toggle" data-unit="${esc(u.id)}" ${target}>${open.has(k) ? "hide suggested fix" : "suggested fix"}</button>`;
  }
  return `<button class="linkbtn" data-act="suggest-fix" data-unit="${esc(u.id)}" ${target} title="Show the patch the fixer would make, without changing any code">Suggest fix</button>`;
}

// suggestHTML is the suggestion under its row, when shown, or why the
// last try failed.
export function suggestHTML(u, i, t) {
  const is = t ? null : u.issues[i];
  const k = rowKey(u, is, t);
  if (failed.has(k)) return `<span class="idetail dismiss-error">Couldn't suggest a fix: ${esc(failed.get(k))}</span>`;
  const s = suggestionFor(u, is, t);
  if (!s || !open.has(k)) return "";
  const target = t ? `data-thread="${esc(t.id)}"` : `data-issue="${i}"`;
  const lines = s.patch.split("\n").filter((l) => !/^(diff --git|index |new file mode|deleted file mode)/.test(l));
  const body = lines.map((l) => {
    const cls = l.startsWith("+++") || l.startsWith("---") ? "file" : l.startsWith("@@") ? "hunk" : l[0] === "+" ? "add" : l[0] === "-" ? "del" : "";
    return `<span class="${cls}">${esc(l) || " "}</span>`;
  }).join("\n");
  return `<div class="suggestion">
      <div class="muted">Suggested by ${esc(s.model || "the fixer")} · ${esc(new Date(s.at).toLocaleString())} · nothing has changed yet</div>
      ${s.scope ? `<div class="warn">Beyond the issue: ${esc(s.scope)}</div>` : ""}
      ${s.extra ? `<div class="muted">The fixer added: ${esc(s.extra)}</div>` : ""}
      <pre class="suggest-patch">${body}</pre>
      <div class="suggest-acts">
        <button class="details-btn" data-act="fix-suggested" data-unit="${esc(u.id)}" ${target} title="Start a fix whose first round applies this patch, then checks, tests and audits it">Apply this fix</button>
        <button class="linkbtn" data-act="suggest-fix" data-unit="${esc(u.id)}" ${target}>suggest again</button>
      </div>
    </div>`;
}

// progressText is a running suggestion's stage and the time it has taken.
function progressText(r) {
  const s = Math.floor((Date.now() - r.started) / 1000);
  const took = `${Math.floor(s / 60)}:${String(s % 60).padStart(2, "0")}`;
  return `suggesting ${took} · ${r.job ? stageText(r.job) : "starting…"}`;
}

// follow polls a suggestion's job, updating its row's line in place, and
// when it is done loads the suggestion the job kept on the result.
async function follow(k, key) {
  for (;;) {
    const r = running.get(k);
    if (!r) return;
    let j;
    try {
      j = await pollJob(r.id);
    } catch (e) {
      running.delete(k);
      failed.set(k, e.message);
      render();
      return;
    }
    r.job = j;
    if (j.status === "running") {
      const el = document.querySelector(`.suggesting[data-sk="${CSS.escape(k)}"] .suggest-what`);
      if (el) el.textContent = progressText(r);
      await new Promise((res) => setTimeout(res, 1000));
      continue;
    }
    running.delete(k);
    if (j.status === "error") failed.set(k, j.error || "failed");
    if (j.status === "done") {
      const fresh = await api(`/api/results/${encodeURIComponent(key)}`).catch(() => null);
      if (fresh && S.result?.key === key) {
        S.result.suggestions = fresh.suggestions || [];
        open.add(k);
      }
    }
    refreshJobs();
    render();
    return;
  }
}

// rowOf finds the unit and the issue or thread a button names.
function rowOf(el) {
  for (const f of S.result?.files || []) {
    for (const u of f.units || []) {
      if (u.id !== el.dataset.unit) continue;
      if (el.dataset.thread) return { u, t: (u.threads || []).find((t) => t.id === el.dataset.thread) };
      return { u, i: Number(el.dataset.issue), is: u.issues?.[Number(el.dataset.issue)] };
    }
  }
  return {};
}

export const actions = {
  "suggest-fix": async (el) => {
    const r = S.result;
    const { u, i, is, t } = rowOf(el);
    if (!r || !u || (!is && !t)) return false;
    const k = rowKey(u, is, t);
    if (running.has(k)) return false;
    failed.delete(k);
    open.delete(k);
    try {
      const body = { ...jobSettings(), unit_id: u.id, issue: t ? 0 : i, thread: t ? t.id : "" };
      const job = await postJSON(`/api/results/${encodeURIComponent(r.key)}/suggest`, body);
      running.set(k, { id: job.id, started: Date.now(), job });
      refreshJobs();
      follow(k, r.key);
    } catch (e) {
      failed.set(k, e.message);
    }
    render();
    return false;
  },
  "suggest-log": (el) => { watchJob(el.dataset.id); return false; },
  "suggest-stop": (el) => { cancelJob(el.dataset.id).catch(() => {}); return false; },
  "suggest-toggle": (el) => {
    const { u, is, t } = rowOf(el);
    if (!u) return false;
    const k = rowKey(u, is, t);
    if (open.has(k)) open.delete(k);
    else open.add(k);
  },
};
