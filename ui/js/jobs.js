// Jobs: the job view (a triage job's progress and activity log in #main),
// the log dialog (a fix job's, over the result it is fixing) and the
// sidebar's list of running jobs. A job keeps running on the
// server when its view is left or the page reloads; the list reopens it.
import { $, esc, api, postJSON, ask, say } from "./util.js";
import { mountActivity } from "./activity.js";
import { S } from "./state.js";

const FAILED_FOR = 60 * 60 * 1000; // failed jobs stay listed this long
const KIND = { triage: "triage", fix: "fix", index: "index" };

let onDone = async () => {};
let onFinished = () => {};
let timer = null;
const status = new Map(); // job id -> last seen status
let triaging = []; // running triage jobs
let busy = []; // running triage and fix jobs
const stopping = new Set();
const confirming = new Set();
const removed = new Set(); // failed jobs taken off the list by hand

// A triage job's url is the PR link or local path it was started with.
const same = (a, b) => !!a && !!b && a.trim().replace(/\/+$/, "").toLowerCase() === b.trim().replace(/\/+$/, "").toLowerCase();

// triageJobFor returns the running triage job for a PR link or local path,
// if there is one.
export async function triageJobFor(src) {
  await refreshJobs();
  return triaging.find((j) => same(j.url, src));
}

// markTriaging flags the sidebar's results that are being triaged again
// or fixed.
export function markTriaging() {
  document.querySelectorAll("#list .pr-item").forEach((el) =>
    el.classList.toggle("triaging", busy.some((j) => same(j.url, el.dataset.src))));
}

// initJobs sets what opens a finished job's result (its key) and what runs
// when any job finishes (refreshing the results list).
export function initJobs(done, finished) {
  onDone = done;
  onFinished = finished;
  $("#jobs").addEventListener("click", (e) => {
    if (handleStopClick(e)) return;
    const x = e.target.closest(".job-x");
    if (x) {
      e.preventDefault();
      removed.add(x.dataset.id);
      refreshJobs();
      return;
    }
    const item = e.target.closest(".job-item");
    if (item) item.dataset.kind === "fix" ? showLog(item.dataset.id) : watchJob(item.dataset.id);
  });
  refreshJobs();
}

// Share confirmation and pending state across the sidebar and both log views.
async function requestStop(id, kind) {
  if (confirming.has(id) || stopping.has(id)) return;
  confirming.add(id);
  try {
    if (!await ask(`Stop this ${KIND[kind] || "action"}? This interrupts the running action. Unfinished work may be lost; changes or results already saved may remain. You will need to start the action again to finish it.`, "Stop action")) return;
    stopping.add(id);
    document.querySelectorAll(".job-stop").forEach((button) => {
      if (button.dataset.id === id) {
        button.disabled = true;
        button.textContent = "Stopping…";
      }
    });
    await cancelJob(id);
  } catch (err) {
    await say(err.message);
  } finally {
    confirming.delete(id);
    stopping.delete(id);
    refreshJobs();
  }
}

function handleStopClick(e) {
  const button = e.target.closest(".job-stop");
  if (!button) return false;
  e.preventDefault();
  e.stopPropagation();
  requestStop(button.dataset.id, button.dataset.kind);
  return true;
}

function stopButton(j) {
  if (j.status !== "running" || (!j.cancelable && !stopping.has(j.id))) return "";
  return `<button type="button" class="job-stop" data-id="${esc(j.id)}" data-kind="${esc(j.kind)}" ${stopping.has(j.id) ? "disabled" : ""}>${stopping.has(j.id) ? "Stopping…" : "Stop"}</button>`;
}

// cancelJob stops a running job and resolves once it has stopped, so what
// it held (a fix's claim on its files) is let go by then.
export async function cancelJob(id) {
  await postJSON(`/api/jobs/${id}/cancel`, {});
  refreshJobs();
  for (let i = 0; i < 60; i++) {
    const j = await pollJob(id).catch(() => null);
    if (!j || j.status !== "running") break;
    await new Promise((res) => setTimeout(res, 500));
  }
  refreshJobs();
}

export function stageText(j) {
  const repo = /^https?:\/\//.test(j.url) ? j.url.split("/")[4] || "this repo" : "this repo";
  const what = {
    fetch: "fetching PR and diffing",
    inspect: "reading the local changes",
    commit: "committing the working tree changes",
    comments: j.total ? `checking review comments ${j.done}/${j.total}` : "loading review comments",
    lint: "running static analysis over the changed lines",
    dedupe: "merging findings raised more than once",
    list: "listing the org's repos",
    fix: `fix round ${j.done} of up to ${j.total}`,
    check: `checking fix round ${j.done} of up to ${j.total}`,
    triage: j.kind === "fix" ? "re-triaging the fixed code" : undefined,
    clone: j.kind === "index" ? `cloning ${j.done + 1}/${j.total}` : `${repo} is not in the code map: cloning it into the workspace…`,
    codemap: j.kind === "index" ? "building the code map" : `${repo} is not in the code map: building it before triage (a few minutes the first time)…`,
  }[j.stage];
  if (what) return what;
  if (j.stage?.startsWith("wait: ")) return `waiting: ${j.stage.slice(6)}`;
  if (!j.stage) return "starting…";
  return `${j.stage} ${j.done}/${j.total}${j.kind === "triage" ? " units" : ""}`;
}

// pollJob fetches a job, retrying a lost connection (no answer, or a 5xx)
// with backoff, about 10s in all, before giving up. retrying runs before
// each wait so the view can say the connection was lost. A 4xx (the job
// is gone) fails at once.
const RETRY = [1000, 3000, 6000];
export async function pollJob(id, retrying = () => {}) {
  for (let i = 0; ; i++) {
    try {
      return await api(`/api/jobs/${id}`);
    } catch (e) {
      if (i >= RETRY.length || (e.status && e.status < 500)) throw e;
      retrying(e);
      await new Promise((res) => setTimeout(res, RETRY[i]));
    }
  }
}
const LOST = "connection lost, retrying…";

// watchJob shows a job in #main and follows it until it ends or the view is
// replaced. A finished job with a result opens it; onCached, if set, gets
// a job that reused a cached result first and opens it only if it returns
// true.
export async function watchJob(id, onCached) {
  $("#main").innerHTML = `<div class="job-view"><div class="progress"><div class="progress-what">loading…</div></div><div class="activity"></div></div>`;
  const view = $("#main .job-view");
  view.addEventListener("click", handleStopClick);
  const refreshLog = mountActivity(view.querySelector(".activity"), id);
  markActive(id);
  try {
    for (;;) {
      const j = await pollJob(id, () => {
        const w = view.querySelector(".progress-what");
        if (w) w.textContent = LOST;
      });
      if (!view.isConnected) return;
      document.querySelectorAll("#list .pr-item").forEach((el) => el.classList.toggle("active", same(el.dataset.src, j.url)));
      await refreshLog().catch(() => {});
      if (!view.isConnected) return;
      if (j.status === "done" && j.key) {
        refreshJobs();
        if (!j.cached || !onCached || (await onCached(j))) await onDone(j.key);
        return;
      }
      const p = view.querySelector(".progress");
      if (j.status === "done") { p.innerHTML = `${esc(j.url)}<br>finished`; break; }
      if (j.status === "error") { p.outerHTML = `<div class="error">${esc(j.error)}</div>`; break; } // the log stays up to show what failed
      if (j.status === "cancelled") { p.innerHTML = `${esc(j.url)}<br>stopped`; break; }
      const pct = j.total ? Math.round((100 * j.done) / j.total) : 0;
      p.querySelector(".progress-what").innerHTML = `${stopButton(j)}${esc(j.url)}<br>${esc(stageText(j))}<div class="bar"><div style="width:${pct}%"></div></div>`;
      await new Promise((res) => setTimeout(res, 700));
    }
  } catch (e) {
    if (view.isConnected) view.innerHTML = `<div class="error">${esc(e.message)}</div>`;
  }
  refreshJobs();
}

// showLog opens a job's progress and activity log in the log dialog, over
// whatever #main shows, and follows the job while the dialog is open.
export async function showLog(id) {
  const dlg = $("#job-log");
  const token = (dlg.token = {});
  const body = dlg.querySelector(".job-log-body");
  body.onclick = handleStopClick;
  body.innerHTML = `<div class="job-log-stage">loading…</div><div class="activity"></div>`;
  const refreshLog = mountActivity(body.querySelector(".activity"), id);
  if (!dlg.open) dlg.showModal();
  const live = () => dlg.open && dlg.token === token;
  while (live()) {
    const j = await pollJob(id, () => {
      const s = live() && body.querySelector(".job-log-stage");
      if (s) s.textContent = LOST;
    }).catch((e) => ({ status: "error", error: `connection lost: ${e.message}` }));
    await refreshLog().catch(() => {});
    if (!live()) return;
    if (j.kind) dlg.querySelector("h3").textContent = `${KIND[j.kind] || j.kind} log`;
    const stage = body.querySelector(".job-log-stage");
    if (j.status === "error") { stage.innerHTML = `<div class="error">${esc(j.error)}</div>`; return; }
    if (j.status === "done") { stage.textContent = "finished"; return; }
    if (j.status === "cancelled") { stage.textContent = "stopped"; return; }
    const pct = j.total ? Math.round((100 * j.done) / j.total) : 0;
    stage.innerHTML = `${stopButton(j)}${esc(stageText(j))}<div class="bar"><div style="width:${pct}%"></div></div>`;
    await new Promise((res) => setTimeout(res, 700));
  }
}

const watched = () => !!$("#main .job-view");

function markActive(id) {
  document.querySelectorAll("#jobs .job-item").forEach((el) => el.classList.toggle("active", el.dataset.id === id));
}

// xButton offers Stop for running jobs and removes failed jobs from the list.
function xButton(j) {
  if (j.status === "error") return `<button class="job-x" data-id="${esc(j.id)}" data-status="error" title="Remove from the list" aria-label="Remove from the list">✕</button>`;
  return stopButton(j);
}

// refreshJobs redraws the running list, and keeps polling while any job
// runs. Call it after starting a job.
export async function refreshJobs() {
  clearTimeout(timer);
  const jobs = await api("/api/jobs").catch(() => null);
  if (!jobs) { timer = setTimeout(refreshJobs, 5000); return; }
  let finished = false;
  for (const j of jobs) {
    if (status.get(j.id) === "running" && j.status === "done") finished = true;
    // A new code map makes the treemap's cached trees stale.
    if (j.kind === "index" && status.get(j.id) === "running" && j.status !== "running") S.trees = {};
    status.set(j.id, j.status);
  }
  if (finished) onFinished();
  busy = jobs.filter((j) => (j.kind === "triage" || j.kind === "fix") && j.status === "running");
  triaging = busy.filter((j) => j.kind === "triage");
  markTriaging();
  const shown = jobs.filter((j) => j.status === "running" || (j.status === "error" && !removed.has(j.id) && Date.now() - new Date(j.started) < FAILED_FOR));
  const active = watched() ? $("#jobs .job-item.active")?.dataset.id : null;
  $("#jobs-head").hidden = !shown.length;
  $("#jobs").innerHTML = shown.map((j) => `
    <a class="job-item ${j.id === active ? "active" : ""}" data-id="${esc(j.id)}" data-kind="${esc(j.kind)}" data-status="${esc(j.status)}" title="${esc(j.error || j.url)}">
      <span class="t"><span class="job-dot"></span><span class="u">${esc(j.url)}</span></span>
      <span class="m"><span class="job-kind">${esc(KIND[j.kind] || j.kind)}</span> ${j.status === "error" ? "failed" : esc(stageText(j))}</span>${xButton(j)}
    </a>`).join("");
  if (jobs.some((j) => j.status === "running")) timer = setTimeout(refreshJobs, 2000);
}
