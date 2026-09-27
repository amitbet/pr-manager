// Overview of the whole PR: why it was made, how it does it, and what it
// may break. It opens the walkthrough and sits above the classic list.
import { esc, postJSON } from "./util.js";
import { S, render } from "./state.js";
import { jobSettings } from "./settings.js";
import { trText, trDir } from "./entext.js";

// OVERVIEW_ID is the overview's key among the translated texts, beside
// the unit IDs, so the EN toggle works on it as on a unit's text.
export const OVERVIEW_ID = "__overview";
const ou = { id: OVERVIEW_ID };

export const overviewText = (ov) => ({ why: ov.why, how: [...(ov.how || [])], issues: [...(ov.issues || [])] });

// hasOverview is whether there is an overview to show, or one coming.
export const hasOverview = (r) => !!(r.overview || r.overview_loading);

// loadOverview gets the overview of a result from before overviews,
// which the server writes and saves the first time.
export async function loadOverview() {
  const r = S.result;
  if (!r || r.overview) return;
  r.overview_loading = true;
  try {
    const ov = await postJSON(`/api/results/${encodeURIComponent(r.key)}/overview`, jobSettings());
    r.overview = ov;
  } catch (e) {
    r.overview_error = e.message;
  }
  r.overview_loading = false;
  if (S.result === r) render();
}

const item = (path, text) => `<li class="tr" ${trDir(ou, path, text)}>${trText(ou, path, text)}</li>`;

// overviewBody is the three sections; potential issues only when there are some.
function overviewBody(ov) {
  const parts = [`<div class="ov-sec"><h4>Why</h4><p class="tr" ${trDir(ou, "why", ov.why)}>${trText(ou, "why", ov.why)}</p></div>`];
  if (ov.how?.length) parts.push(`<div class="ov-sec"><h4>How</h4><ul>${ov.how.map((x, i) => item(`how.${i}`, x)).join("")}</ul></div>`);
  if (ov.issues?.length) parts.push(`<div class="ov-sec issues"><h4>Potential issues</h4><ul>${ov.issues.map((x, i) => item(`issues.${i}`, x)).join("")}</ul></div>`);
  return parts.join("");
}

const loadingHTML = `<div class="ov-loading"><span class="spinner"></span>Writing the overview…</div>`;

// overviewHTML is the overview of r. In the classic list it can be
// collapsed; a failed overview only says why there is none.
export function overviewHTML(r, collapsible) {
  if (!r.overview && !r.overview_loading) {
    return collapsible && r.overview_error ? `<div class="ov-none">No overview: ${esc(r.overview_error)}</div>` : "";
  }
  const open = !collapsible || S.ovOpen;
  const head = collapsible
    ? `<button class="ov-head" data-act="ov-toggle" aria-expanded="${open}">${open ? "▾" : "▸"} Overview</button>`
    : `<h3 class="ov-title">Overview</h3>`;
  const body = !open ? "" : r.overview ? overviewBody(r.overview) : loadingHTML;
  return `<div class="ov${collapsible ? " collapsible" : ""}">${head}${body}</div>`;
}

export const actions = {
  "ov-toggle": () => { S.ovOpen = !S.ovOpen; localStorage.setItem("pr-manager.ovopen", S.ovOpen ? "1" : "0"); },
};
