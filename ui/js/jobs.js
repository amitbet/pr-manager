// Jobs: the job view (a triage job's progress and activity log in #main),
// the log dialog (a fix job's, over the result it is fixing) and the
// sidebar's list of running jobs. A job keeps running on the
// server when its view is left or the page reloads; the list reopens it.
import { $, esc, api } from "./util.js";
import { mountActivity } from "./activity.js";

const FAILED_FOR = 60 * 60 * 1000; // failed jobs stay listed this long
const KIND = { triage: "triage", fix: "fix", index: "index" };

let onDone = async () => {};
let onFinished = () => {};
let timer = null;
const status = new Map(); // job id -> last seen status
let triaging = []; // running triage jobs
let busy = []; // running triage and fix jobs

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
    const item = e.target.closest(".job-item");
    if (item) item.dataset.kind === "fix" ? showLog(item.dataset.id) : watchJob(item.dataset.id);
  });
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
    list: "listing the org's repos",
    fix: `fix round ${j.done} of up to ${j.total}`,
    check: `checking fix round ${j.done} of up to ${j.total}`,
    triage: j.kind === "fix" ? "re-triaging the fixed code" : undefined,
    clone: j.kind === "index" ? `cloning ${j.done + 1}/${j.total}` : `${repo} is not in the code map: cloning it into the workspace…`,
    codemap: j.kind === "index" ? "building the code map" : `${repo} is not in the code map: building it before triage (a few minutes the first time)…`,
  }[j.stage];
  if (what) return what;
  if (!j.stage) return "starting…";
  return `${j.stage} ${j.done}/${j.total}${j.kind === "triage" ? " units" : ""}`;
}

// watchJob shows a job in #main and follows it until it ends or the view is
// replaced. A finished job with a result opens it; onCached, if set, gets
// a job that reused a cached result first and opens it only if it returns
// true.
export async function watchJob(id, onCached) {
  $("#main").innerHTML = `<div class="job-view"><div class="progress"><div class="progress-what">loading…</div></div><div class="activity"></div></div>`;
  const view = $("#main .job-view");
  const refreshLog = mountActivity(view.querySelector(".activity"), id);
  markActive(id);
  try {
    for (;;) {
      const j = await api(`/api/jobs/${id}`);
      if (!view.isConnected) return;
      document.querySelectorAll("#list .pr-item").forEach((el) => el.classList.toggle("active", same(el.dataset.src, j.url)));
      await refreshLog().catch(() => {});
      if (!view.isConnected) return;
      if (j.status === "done" && j.key) {
        refreshJobs();
        if (!j.cached || !onCached || onCached(j)) await onDone(j.key);
        return;
      }
      const p = view.querySelector(".progress");
      if (j.status === "done") { p.innerHTML = `${esc(j.url)}<br>finished`; break; }
      if (j.status === "error") { p.outerHTML = `<div class="error">${esc(j.error)}</div>`; break; } // the log stays up to show what failed
      const pct = j.total ? Math.round((100 * j.done) / j.total) : 0;
      p.querySelector(".progress-what").innerHTML = `${esc(j.url)}<br>${esc(stageText(j))}<div class="bar"><div style="width:${pct}%"></div></div>`;
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
  body.innerHTML = `<div class="job-log-stage">loading…</div><div class="activity"></div>`;
  const refreshLog = mountActivity(body.querySelector(".activity"), id);
  if (!dlg.open) dlg.showModal();
  const live = () => dlg.open && dlg.token === token;
  while (live()) {
    const j = await api(`/api/jobs/${id}`).catch((e) => ({ status: "error", error: e.message }));
    await refreshLog().catch(() => {});
    if (!live()) return;
    if (j.kind) dlg.querySelector("h3").textContent = `${KIND[j.kind] || j.kind} log`;
    const stage = body.querySelector(".job-log-stage");
    if (j.status === "error") { stage.innerHTML = `<div class="error">${esc(j.error)}</div>`; return; }
    if (j.status === "done") { stage.textContent = "finished"; return; }
    const pct = j.total ? Math.round((100 * j.done) / j.total) : 0;
    stage.innerHTML = `${esc(stageText(j))}<div class="bar"><div style="width:${pct}%"></div></div>`;
    await new Promise((res) => setTimeout(res, 700));
  }
}

const watched = () => !!$("#main .job-view");

function markActive(id) {
  document.querySelectorAll("#jobs .job-item").forEach((el) => el.classList.toggle("active", el.dataset.id === id));
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
    status.set(j.id, j.status);
  }
  if (finished) onFinished();
  busy = jobs.filter((j) => (j.kind === "triage" || j.kind === "fix") && j.status === "running");
  triaging = busy.filter((j) => j.kind === "triage");
  markTriaging();
  const shown = jobs.filter((j) => j.status === "running" || (j.status === "error" && Date.now() - new Date(j.started) < FAILED_FOR));
  const active = watched() ? $("#jobs .job-item.active")?.dataset.id : null;
  $("#jobs-head").hidden = !shown.length;
  $("#jobs").innerHTML = shown.map((j) => `
    <a class="job-item ${j.id === active ? "active" : ""}" data-id="${esc(j.id)}" data-kind="${esc(j.kind)}" data-status="${esc(j.status)}" title="${esc(j.error || j.url)}">
      <span class="t"><span class="job-dot"></span><span class="u">${esc(j.url)}</span></span>
      <span class="m"><span class="job-kind">${esc(KIND[j.kind] || j.kind)}</span> ${j.status === "error" ? "failed" : esc(stageText(j))}</span>
    </a>`).join("");
  if (jobs.some((j) => j.status === "running")) timer = setTimeout(refreshJobs, 2000);
}
