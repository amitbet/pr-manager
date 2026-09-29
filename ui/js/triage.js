// Header triage form: starts a job and shows it in the job view.
import { $, esc, postJSON } from "./util.js";
import { jobSettings } from "./settings.js";
import { watchJob, refreshJobs } from "./jobs.js";

// triage starts a job for the form's PR link or local path. A PR already
// triaged at this commit reopens its saved result; with ask set, the user
// is offered to run it again instead.
async function triage(ask, force = false) {
  const url = $("#url").value.trim();
  if (!url) return;
  const isPath = isLocalPath(url);
  const body = { [isPath ? "path" : "url"]: url, force, ...jobSettings() };
  $("#go").disabled = true;
  $("#main").innerHTML = `<div class="progress">starting…</div>`;
  try {
    const job = await postJSON("/api/triage", body);
    refreshJobs();
    watchJob(job.id, ask && ((j) => {
      const when = new Date(j.cached).toLocaleString();
      // A saved result from another model is offered, but named as such.
      const model = j.cached_by ? `\n\nThe saved result was made with ${j.cached_by}; your settings now use ${j.runs_with}.` : "";
      if (!confirm(`This ${isPath ? "state" : "commit"} was already triaged (${when}). Run it again?${model}\n\nOK runs it again; Cancel opens the saved result.`)) return true;
      triage(false, true);
      return false;
    }));
  } catch (e) {
    $("#main").innerHTML = `<div class="error">${esc(e.message)}</div>`;
  } finally {
    $("#go").disabled = false;
  }
}

// isLocalPath says whether the form's text is a local path rather than a
// PR. A path may end in #rev (a commit or branch), so owner/repo#N only
// counts when it doesn't start like a path.
export const isLocalPath = (s) => /^([/~.\\]|[A-Za-z]:)/.test(s) || (!/^https?:\/\//.test(s) && !/^[^\s]+\/[^\s]+#\d+$/.test(s));

// initTriage wires the form.
export function initTriage() {
  $("#go").onclick = () => triage(true);
  $("#url").addEventListener("keydown", (e) => e.key === "Enter" && triage(true));
}

// triageURL fills the form with a PR link and starts triaging it.
export function triageURL(url) {
  $("#url").value = url;
  triage(false);
}
