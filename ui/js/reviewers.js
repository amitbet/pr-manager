// Extra reviewers: the reader's own review skills or prompts, each on a
// provider and model of its own, run on a result only from the Issues
// tab's Run reviewers button (see reviewers.go). What they find is pooled
// with the review's issues, checked by the critic and deduplicated, and
// each issue says which reviewer raised it. Kept in localStorage under
// pr-manager.reviewers as a JSON list; none set up changes nothing.
import { $, esc, api } from "./util.js";

const KEY = "pr-manager.reviewers";
const EFFORTS = ["low", "medium", "high", "xhigh", "max"];
let providers = null; // GET /api/providers

// reviewers is the saved list, each { name, provider, model, effort,
// skill, prompt, off }.
export function reviewers() {
  try {
    const v = JSON.parse(localStorage.getItem(KEY) || "[]");
    return Array.isArray(v) ? v.filter((r) => r && typeof r === "object") : [];
  } catch { return []; }
}

// enabledReviewers are the ones Run reviewers runs, as the server takes
// them.
export const enabledReviewers = () => reviewers()
  .filter((r) => !r.off && r.name?.trim() && r.provider && (r.skill?.trim() || r.prompt?.trim()))
  .map(({ name, provider, model, effort, skill, prompt }) => ({ name: name.trim(), provider, model, effort, skill, prompt }));

function save(list) {
  if (list.length) localStorage.setItem(KEY, JSON.stringify(list));
  else localStorage.removeItem(KEY);
}

const cli = (id) => id === "codex" || id === "claude-code";

function cardHTML(r, i) {
  const list = (providers?.providers || []).filter((p) => p.summarize);
  const ids = list.map((p) => p.id);
  if (r.provider && !ids.includes(r.provider)) ids.push(r.provider);
  const models = list.find((p) => p.id === r.provider)?.models || [];
  const skillOff = r.provider && !cli(r.provider);
  return `<div class="rev-card${r.off ? " off" : ""}" data-i="${i}">
      <div class="rev-row">
        <label class="check" title="Run it with Run reviewers"><input type="checkbox" data-f="on" ${r.off ? "" : "checked"}> on</label>
        <input data-f="name" value="${esc(r.name || "")}" placeholder="name, e.g. cc-review" spellcheck="false" aria-label="reviewer name">
        <select data-f="provider" aria-label="reviewer provider"><option value="">provider…</option>${ids.map((id) => `<option value="${esc(id)}" ${id === r.provider ? "selected" : ""}>${esc(id)}</option>`).join("")}</select>
        <input data-f="model" list="rev-models-${i}" value="${esc(r.model || "")}" placeholder="default model" spellcheck="false" aria-label="reviewer model">
        <datalist id="rev-models-${i}">${models.map((m) => `<option value="${esc(m.id)}">${esc(m.label || m.id)}</option>`).join("")}</datalist>
        <select data-f="effort" aria-label="reviewer effort"><option value="">default effort</option>${EFFORTS.map((e) => `<option value="${e}" ${e === r.effort ? "selected" : ""}>${e}</option>`).join("")}</select>
        <button type="button" class="linkbtn" data-rev="remove" aria-label="remove reviewer">remove</button>
      </div>
      <input data-f="skill" value="${esc(r.skill || "")}" ${skillOff ? `disabled title="skills need codex or claude-code"` : ""} placeholder="${skillOff ? "skills need codex or claude-code" : "skill or slash command, e.g. /code-review high"}" spellcheck="false" aria-label="reviewer skill">
      <textarea data-f="prompt" rows="2" placeholder="instructions: added to the skill, or the whole review without one" aria-label="reviewer prompt">${esc(r.prompt || "")}</textarea>
    </div>`;
}

function show() {
  const list = reviewers();
  const box = $("#reviewers-list");
  box.innerHTML = list.length
    ? list.map(cardHTML).join("")
    : `<div class="hint">None set up: the Issues tab shows only the built-in review, static analysis and GitHub comments.</div>`;
}

// initReviewers wires the Reviewers section of Settings.
export function initReviewers() {
  const box = $("#reviewers-list");
  const edit = (e) => {
    const card = e.target.closest(".rev-card");
    const f = e.target.dataset.f;
    if (!card || !f) return;
    const list = reviewers();
    const r = list[Number(card.dataset.i)];
    if (!r) return;
    if (f === "on") r.off = !e.target.checked;
    else r[f] = e.target.value;
    if (f === "provider" && !cli(r.provider)) r.skill = "";
    save(list);
    if (f === "provider" || f === "on") show();
  };
  box.addEventListener("change", edit);
  box.addEventListener("click", (e) => {
    if (e.target.dataset.rev !== "remove") return;
    const list = reviewers();
    list.splice(Number(e.target.closest(".rev-card").dataset.i), 1);
    save(list);
    show();
  });
  $("#reviewers-add").onclick = () => {
    save([...reviewers(), { name: "", provider: "claude-code", model: "", effort: "", skill: "", prompt: "" }]);
    show();
  };
  show();
  api("/api/providers").then((p) => { providers = p; show(); }).catch(() => {});
}
