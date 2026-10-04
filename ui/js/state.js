// App state, shared by every component. Components change S and call
// render(); main.js owns the actual page render.

// wzSteps is what a walkthrough step holds (Settings → Walkthrough steps):
// "unit", "file" or "related". pr-manager.wz_group=1 is the old "file".
function wzSteps() {
  const v = localStorage.getItem("pr-manager.wz_steps");
  if (v === "unit" || v === "file" || v === "related") return v;
  return localStorage.getItem("pr-manager.wz_group") === "1" ? "file" : "related";
}

// The defaults Settings → Review view picks. The view and mode buttons on
// the page change them for the session only. diff_view applies to every
// diff; before it, the classic list and the walkthrough each kept the
// last one used (pr-manager.view, pr-manager.wzview).
export const diffViewDefault = () => {
  const v = localStorage.getItem("pr-manager.diff_view") || localStorage.getItem("pr-manager.wzview");
  return v === "split" ? "split" : "unified";
};
export const reviewModeDefault = () => {
  const v = localStorage.getItem("pr-manager.reviewmode");
  return v === "files" || v === "classic" ? v : "walk";
};
// focusTop is whether opening a walkthrough step of several changes, or a
// file in the Files mode, scrolls to the highest-ranked change.
export const focusTop = () => localStorage.getItem("pr-manager.focus_top") === "1";

export const S = {
  result: null,
  cfg: null,
  view: diffViewDefault(),
  hidden: new Set(),
  collapsed: new Set(),   // file paths
  diffOpen: {},           // unit id -> bool (default: open unless bucket none)
  details: new Set(),     // unit ids with the details panel open
  more: new Set(),        // unit ids with the rest of the details open too
  showEn: new Set(),      // "unit id|path" of translated texts shown in English
  ovOpen: localStorage.getItem("pr-manager.ovopen") !== "0", // the overview above the classic list
  allHidden: false,
  above: {},              // hunk key -> lines expanded above it
  below: {},              // file path -> lines expanded after its last hunk
  files: {},              // "head:path" -> lines
  drafts: [],
  composer: null,         // {path, side, line, id?, body}
  tab: "review",          // review | issues | sequence | map
  issueKinds: new Set(),  // claim kinds hidden on the Issues tab
  dismissing: null,       // {key, reason, busy?, error?} while the dismiss form is open
  showDismissed: false,
  seqCopied: false,
  seqText: false,         // show the Mermaid text (no clipboard access)
  seqView: "after",       // the Sequence tab: the flow before | after the PR
  mode: reviewModeDefault(), // the Review tab: walk(through) | files | classic
  // group: a step is one file's changes (Settings → Walkthrough); usesOpen: "step|unit" of opened definitions.
  wz: { cur: null, done: new Set(), all: false, finished: false, intro: false, view: diffViewDefault(), steps: wzSteps(), usesOpen: new Set(), noteOpen: new Map() },
  // Files mode: the open file, closed directories, files already loaded whole (or tried) and the one loading.
  fv: { path: null, closed: new Set(), tried: new Set(), loading: null },
  tm: { scope: "repo", zoom: [], sort: "risk", mode: localStorage.getItem("pr-manager.tmmode") || "both" }, // treemap: repo | all, zoom path, color by impact | likelihood | both
  trees: {},              // treemap data by repo ("all" = workspace)
};

let renderFn = () => {};
export const onRender = (fn) => { renderFn = fn; };
export const render = () => renderFn();

export const prBase = () => { const p = S.result.pr; return p.local_path ? `/api/local/${encodeURIComponent(S.result.key)}` : `/api/prs/${p.host || "github.com"}/${p.owner}/${p.repo}/${p.number}`; };
// localSrc is what a local result was triaged from: the checkout's path,
// with #rev for a commit or branch in it.
export const localSrc = (p) => p.local_path && (p.rev ? `${p.local_path}#${p.rev}` : p.local_path);

// repoName is owner/repo, with the host for GitHub Enterprise repos.
export const repoName = (p) => `${p.host ? `${p.host}/` : ""}${p.owner}/${p.repo}`;
export const allUnits = () => S.result.files.flatMap((f) => (f.units || []).map((u) => ({ u, f })));
export const fileByPath = (p) => S.result.files.find((f) => f.path === p);
export const fileOfUnit = (id) => S.result.files.find((f) => (f.units || []).some((u) => u.id === id));

// syncURL keeps ?pr, ?key and ?tab shareable.
export function syncURL() {
  if (!S.result) return;
  const q = new URLSearchParams({ key: S.result.key });
  if (S.result.pr.local_path) q.set("path", localSrc(S.result.pr));
  else q.set("pr", S.result.pr.url);
  if (S.tab !== "review") q.set("tab", S.tab);
  history.replaceState(null, "", `?${q}`);
}
