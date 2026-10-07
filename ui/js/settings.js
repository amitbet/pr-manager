// Settings dialog: provider and model pickers, code map sources, summary
// language, review budget, walkthrough steps and review tools. Choices are kept in localStorage under pr-manager.<key>.
import { showStats } from "./stats.js";
import { $, esc, api, BUCKETS, LABEL, askText } from "./util.js";
import { S, render, diffViewDefault, reviewModeDefault, focusTop } from "./state.js";
import * as budget from "./budget.js";
import { comboFromEvent, hotkeyLabel, hideLabel } from "./chat.js";
import { refreshJobs, pollJob } from "./jobs.js";
import { loadList, sideShown, sideKeepDays, confirmRemove } from "./sidebar.js";

// Provider and model pickers. The server lists only the providers this
// machine can run (a logged-in CLI, an API key that is set, a running Ollama
// or OpenJev), each with its own models; changing the provider reloads the
// model list. "default" means the server's choice for that role.
const ROLES = [
  { role: "classifier", model: "classify_model", def: "classify_model", label: "classifier" },
  { role: "summarizer", model: "summary_model", def: "summary_model", label: "reviewer" },
  { role: "translator", model: "translate_model", def: "translate_model", label: "translator", noOff: true },
  { role: "chat", model: "chat_model", def: "chat_model", label: "chat agent", noOff: true },
];
let P = null; // GET /api/providers
const saved = (k) => localStorage.getItem(`pr-manager.${k}`) || "";
// Old provider names, from before the API providers were named *-api.
const RENAMED = { openai: "openai-api", anthropic: "claude-api", claude: "claude-api" };
for (const r of ["classifier", "summarizer"]) {
  const v = saved(r);
  if (RENAMED[v]) localStorage.setItem(`pr-manager.${r}`, RENAMED[v]);
}
for (const [k, v] of Object.entries({ ...localStorage })) {
  const m = k.match(/^pr-manager\.((?:classify|summary)_model)\.(openai|anthropic|claude)$/);
  if (m) { localStorage.setItem(`pr-manager.${m[1]}.${RENAMED[m[2]]}`, v); localStorage.removeItem(k); }
}
const save = (k, v) => localStorage.setItem(`pr-manager.${k}`, v);
let onBudget = () => {};
let onLang = () => {};

function fillProviders(r) {
  const sel = $(`#${r.role}`);
  const list = P.providers.filter((p) => r.role === "classifier" || p.summarize);
  const why = (P.unavailable || []).map((u) => `${u.id}: ${u.reason}`).join("\n");
  sel.title = `${r.label} provider${why ? `\n\nnot available:\n${why}` : ""}`;
  sel.innerHTML = `<option value="">default (${esc(P[r.role])})</option>` +
    list.map((p) => `<option value="${esc(p.id)}" title="${esc(p.reason)}">${esc(p.id)}</option>`).join("") +
    (r.noOff ? "" : `<option value="off">off</option>`);
  const want = saved(r.role);
  sel.value = (want === "off" && !r.noOff) || list.some((p) => p.id === want) ? want : "";
}

function fillModels(r) {
  const sel = $(`#${r.model}`);
  const chosen = $(`#${r.role}`).value;
  const id = chosen || P[r.role];
  const p = P.providers.find((x) => x.id === id);
  if (chosen === "off" || !p || !p.models?.length) {
    sel.innerHTML = `<option value="">${chosen === "off" ? "—" : "default model"}</option>`;
    sel.disabled = true;
    return;
  }
  sel.disabled = false;
  // The server's own default when the provider is its default (a -classify-model flag wins).
  const def = id === P[r.role] ? P[r.def] : p[r.def];
  const opts = p.models.map((m) => `<option value="${esc(m.id)}">${esc(m.label && m.label.toLowerCase() !== m.id ? `${m.label} · ${m.id}` : m.id)}</option>`);
  const custom = saved(`${r.model}.${id}`);
  if (custom && !p.models.some((m) => m.id === custom)) opts.push(`<option value="${esc(custom)}">${esc(custom)}</option>`);
  // With the provider left on default the server resolves the model (and
  // its flags win); with a provider picked, send the default the list shows.
  const defVal = chosen ? def : "";
  sel.innerHTML = `<option value="${esc(defVal)}" data-default>default (${esc(def)})</option>${opts.filter((o, i) => p.models[i]?.id !== def).join("")}<option value="__custom">custom model id…</option>`;
  sel.title = `${r.label} model · ${p.live ? "list from the provider" : "built-in list"} · ${p.reason}`;
  const want = saved(`${r.model}.${id}`);
  sel.value = want && [...sel.options].some((o) => o.value === want) ? want : defVal;
}

async function loadProviders(refresh) {
  try { P = await api(`/api/providers${refresh ? "?refresh=1" : ""}`); }
  catch { return; } // keep the static "default" options
  for (const r of ROLES) {
    fillProviders(r);
    fillModels(r);
    $(`#${r.role}`).onchange = (e) => { save(r.role, e.target.value); fillModels(r); if (r.role === "translator") fillEffort(); showLine(); };
    $(`#${r.model}`).onchange = async (e) => {
      const prov = $(`#${r.role}`).value || P[r.role];
      if (e.target.value === "__custom") {
        const id = ((await askText(`Model id for ${prov}`)) || "").trim();
        save(`${r.model}.${prov}`, id);
        fillModels(r);
      } else save(`${r.model}.${prov}`, e.target.value);
      showLine();
    };
  }
  fillEffort();
  showLine();
}

// Summary languages: the value is the English name the prompt uses, the
// label adds the native name.
const LANGS = [
  ["Arabic", "العربية"], ["Chinese (Simplified)", "简体中文"], ["Chinese (Traditional)", "繁體中文"],
  ["Czech", "Čeština"], ["Dutch", "Nederlands"], ["French", "Français"], ["German", "Deutsch"],
  ["Hebrew", "עברית"], ["Hindi", "हिन्दी"], ["Indonesian", "Bahasa Indonesia"], ["Italian", "Italiano"],
  ["Japanese", "日本語"], ["Korean", "한국어"], ["Polish", "Polski"], ["Portuguese", "Português"],
  ["Romanian", "Română"], ["Russian", "Русский"], ["Spanish", "Español"], ["Swedish", "Svenska"],
  ["Thai", "ไทย"], ["Turkish", "Türkçe"], ["Ukrainian", "Українська"], ["Vietnamese", "Tiếng Việt"],
];

function fillLang() {
  const sel = $("#summary_lang");
  const def = S.cfg?.summary_lang || "";
  const opts = LANGS.filter(([en]) => en !== def).map(([en, native]) => `<option value="${esc(en)}">${esc(native)} · ${esc(en)}</option>`);
  sel.innerHTML = `<option value="">default (${esc(def || "English")})</option>` + (def ? `<option value="English">English</option>` : "") + opts.join("");
  const want = saved("summary_lang");
  sel.value = [...sel.options].some((o) => o.value === want) ? want : "";
  sel.onchange = () => { save("summary_lang", sel.value); showLine(); onLang(); };
}

// summaryLang is the language to show summaries in; "" is English.
export function summaryLang() {
  const lang = $("#summary_lang").value || S.cfg?.summary_lang || "";
  return lang.toLowerCase() === "english" ? "" : lang;
}

// roleText is "provider/model" as the next triage will run it.
function roleText(r) {
  const prov = $(`#${r.role}`).value || P?.[r.role] || "default";
  if (prov === "off") return "off";
  const sel = $(`#${r.model}`);
  const model = sel.disabled ? "" : sel.value || sel.selectedOptions[0]?.textContent.match(/^default \((.*)\)$/)?.[1] || "";
  return model ? `${prov}/${model}` : prov;
}

// Review budget slider: steps from the open result (its repo's policy),
// else the server's.
function budgetList() { return budget.budgets(S.result, S.cfg); }
function budgetDefault() { return S.result?.review_budget || S.cfg?.review_budget; }

function showBudget() {
  const list = budgetList();
  const range = $("#budget");
  if (!list.length) { $("#budget-info").textContent = "no budgets from the server"; range.disabled = true; return; }
  const cur = budget.chosen(list, budgetDefault());
  range.disabled = false;
  range.max = list.length - 1;
  range.value = Math.max(0, list.findIndex((b) => b.name === cur));
  const b = list[range.value];
  const rules = `human at score ≥ ${b.human}, skim ≥ ${b.skim}; a clean review lowers the score ${Math.round(b.trust * 100)}%${b.lift_floors ? " and lifts the skim floor" : ""}`;
  let pr = "";
  if (S.result && S.result.files.some((f) => (f.units || []).some((u) => u.score))) {
    const c = budget.counts(S.result, b);
    pr = `<div>this PR: ${BUCKETS.map((k) => `<span class="pill ${k}" title="${LABEL[k]}">${c[k]}</span>`).join(" ")} (human · skim · auxiliary · none)</div>`;
  } else if (S.result) pr = `<div>this PR was triaged before review budgets: re-run it to re-bucket</div>`;
  $("#budget-info").innerHTML = `<div><b>${esc(b.name)}</b>${b.name === budgetDefault() ? " (default)" : ""}: ${esc(rules)}</div>${pr}`;
  showLine();
}

// classifying: the reviewer is off, so the classifier places the units.
// Otherwise the review call does and the classifier settings do nothing.
function classifying() {
  return ($("#summarizer").value || P?.summarizer) === "off";
}

// Translation effort: "" is the server's choice for the chosen translator.
const EFFORTS = ["none", "minimal", "low", "medium"];
function fillEffort() {
  const sel = $("#translate_effort");
  const p = P?.providers.find((x) => x.id === $("#translator").value);
  const def = (p ? p.translate_effort : S.cfg?.translate_effort) || "model default";
  sel.innerHTML = `<option value="">default (${esc(def)})</option>` + EFFORTS.map((e) => `<option value="${e}">${e}</option>`).join("");
  sel.value = EFFORTS.includes(saved("translate_effort")) ? saved("translate_effort") : "";
  sel.onchange = () => save("translate_effort", sel.value);
}

function showLine() {
  // The translator only matters when there is something to translate.
  $("#translate-opts").style.display = summaryLang() ? "" : "none";
  const cls = classifying();
  for (const el of document.querySelectorAll(".classify-only")) el.style.display = cls ? "" : "none";
  const list = budgetList();
  const b = list.length ? budget.chosen(list, budgetDefault()) : "";
  const lang = summaryLang();
  const models = cls ? `classify ${roleText(ROLES[0])} · no review` : `analyze ${roleText(ROLES[1])}`;
  const text = models + (lang ? ` · ${lang}` : "") + (b ? ` · budget ${b}` : "");
  $("#settings-line").textContent = text;
  $("#settings-line").title = `models${lang ? " · language" : ""} · review budget\n${text}`;
}

// Code map sources. Empty fields fall back to the server's
// PR_MANAGER_CODE_ROOT / PR_MANAGER_ORG, shown as placeholders.
function codeSources() {
  const body = {};
  for (const k of ["code_root", "org"]) {
    const v = $("#" + k).value.trim();
    if (v) body[k] = v;
  }
  return body;
}

function fillCodeSources() {
  for (const k of ["code_root", "org"]) {
    const el = $("#" + k);
    if (S.cfg?.[k]) el.placeholder = `default: ${S.cfg[k]}`;
    el.value = saved(k);
    el.onchange = () => save(k, el.value.trim());
  }
  const n = S.cfg?.codemap_repos || 0;
  $("#index-status").textContent = n ? `${n} repo${n === 1 ? "" : "s"} in the map` : "the map is empty";
  $("#index-btn").onclick = runIndex;
}

async function runIndex() {
  const btn = $("#index-btn"), status = $("#index-status");
  btn.disabled = true;
  status.textContent = "starting…";
  try {
    const res = await fetch("/api/codemap/index", { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(codeSources()) });
    let j = await res.json();
    if (!res.ok) throw new Error(j.error || res.statusText);
    refreshJobs();
    while (j.status === "running") {
      status.textContent = j.stage === "clone" && j.total ? `cloning ${j.done + 1}/${j.total}…` : `${j.stage || "starting"}…`;
      await new Promise((r) => setTimeout(r, 1000));
      j = await pollJob(j.id, () => { status.textContent = "connection lost, retrying…"; });
    }
    if (j.status === "error") throw new Error(j.error);
    const r = j.result;
    const parts = [r.linked?.length && `${r.linked.length} linked`, r.cloned?.length && `${r.cloned.length} cloned`, r.updated?.length && `${r.updated.length} updated`, r.failed?.length && `${r.failed.length} failed`].filter(Boolean);
    status.textContent = `done: ${parts.join(", ") || "nothing new"} · ${r.repos} repos in the map`;
    status.title = (r.failed || []).join("\n");
  } catch (e) {
    status.textContent = `failed: ${e.message}`;
  } finally {
    btn.disabled = false;
    S.trees = {}; // the treemap refetches the new code map
  }
}

// jobSettings are the fields a triage request sends.
export function jobSettings() {
  const body = {};
  for (const k of ["classifier", "classify_model", "summarizer", "summary_model", "translator", "translate_model"]) body[k] = $("#" + k).value.trim();
  body.review_tools = $("#review_tools").checked;
  body.classify_batch = $("#classify_batch").checked;
  // Only sent when it differs from the server's, so the operator's -lint
  // tool list and the repository's lint: policy stay in force otherwise.
  const lint = $("#lint").checked;
  if (lint !== (S.cfg?.lint !== false)) body.lint = lint ? "auto" : "off";
  body.incremental = $("#incremental").checked;
  // Only sent when picked, so the server's -summary-lang stays the default.
  const lang = $("#summary_lang").value;
  if (lang) body.summary_lang = lang;
  const effort = $("#translate_effort").value;
  if (effort) body.translate_effort = effort;
  return { ...body, ...codeSources() };
}

export function fixSettings() {
  return {
    ...jobSettings(),
    location: $("#fix_location").value,
    recursive: $("#recursive_fix").checked,
    agent: $("#fix_agent").checked,
    max_rounds: Math.max(1, Math.min(10, Number($("#max_fix_rounds").value) || 3)),
  };
}

// refreshSettings updates the budget section for the result on screen.
export const refreshSettings = () => showBudget();

// placeTip puts an (i) tooltip below its icon, or above it when there is no
// room, kept inside the window.
function placeTip(el) {
  const tip = el.querySelector(".tip");
  tip.style.display = "block";
  const r = el.getBoundingClientRect(), t = tip.getBoundingClientRect();
  tip.style.display = "";
  const left = Math.max(8, Math.min(r.left - 12, innerWidth - t.width - 8));
  const below = r.bottom + 6;
  tip.style.left = `${left}px`;
  tip.style.top = `${below + t.height > innerHeight - 8 ? Math.max(8, r.top - t.height - 6) : below}px`;
}

// initHotkey wires a chat hotkey field (id, also its pr-manager.* key):
// focused, it takes the next key combo; Backspace or Delete alone goes back
// to the default. label() names what is set.
function initHotkey(id, label) {
  const el = $(`#${id}`);
  const show = () => { el.value = label(); el.placeholder = ""; };
  const set = (combo) => {
    if (combo) save(id, combo);
    else localStorage.removeItem(`pr-manager.${id}`);
    show();
    dispatchEvent(new Event("prm-hotkey"));
  };
  el.onfocus = () => { el.value = ""; el.placeholder = "press the keys…"; };
  el.onblur = show;
  el.onkeydown = (e) => {
    if (e.key === "Tab" || e.key === "Escape") return; // move on, or close the dialog
    e.preventDefault();
    e.stopPropagation();
    const bare = !e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey;
    if (bare && (e.key === "Backspace" || e.key === "Delete")) set("");
    else if (comboFromEvent(e)) set(comboFromEvent(e));
    else return; // a lone modifier: wait for the key
    el.blur();
  };
  $(`#${id}_reset`).onclick = () => set("");
  show();
}

// initSettings wires the dialog. changed() runs when the budget moves,
// langChanged() when the summary language does.
export function initSettings(changed, langChanged) {
  onBudget = changed;
  onLang = langChanged;
  const dlg = $("#settings");
  // Side tabs: each section belongs to one (data-tab); Statistics loads
  // when it is shown (stats.js). The last tab opened is kept for the session.
  const showTab = (tab) => {
    sessionStorage.setItem("prm-settings-tab", tab);
    for (const b of dlg.querySelectorAll("[data-set-tab]")) { const on = b.dataset.setTab === tab; b.classList.toggle("on", on); b.setAttribute("aria-selected", on); }
    let first = true;
    for (const sec of dlg.querySelectorAll(".set-panes > section")) {
      const shown = sec.dataset.tab === tab && sec.style.display !== "none";
      sec.hidden = sec.dataset.tab !== tab;
      sec.classList.toggle("first", shown && first);
      if (shown) first = false;
    }
    dlg.classList.toggle("wide", tab === "stats");
    if (tab === "stats") showStats($("#stats-pane"));
  };
  dlg.querySelector(".set-tabs").onclick = (e) => { const b = e.target.closest("[data-set-tab]"); if (b) showTab(b.dataset.setTab); };
  $("#settings-btn").onclick = () => { showBudget(); showTab(sessionStorage.getItem("prm-settings-tab") || "models"); dlg.showModal(); };
  $("#theme-btn").onclick = () => window.toggleTheme();
  dlg.addEventListener("click", (e) => { if (e.target === dlg) dlg.close(); }); // backdrop
  for (const el of dlg.querySelectorAll(".info")) {
    el.addEventListener("mouseenter", () => placeTip(el));
    el.addEventListener("focus", () => placeTip(el));
  }
  $("#budget").oninput = (e) => {
    const b = budgetList()[e.target.value];
    if (!b) return;
    budget.choose(b.name);
    showBudget();
    onBudget();
  };
  const tools = $("#review_tools");
  tools.checked = saved("review_tools") ? saved("review_tools") === "1" : !!S.cfg?.review_tools;
  tools.onchange = () => save("review_tools", tools.checked ? "1" : "0");
  const steps = $("#wz_steps");
  steps.value = S.wz.steps;
  steps.onchange = () => { S.wz.steps = steps.value; save("wz_steps", steps.value); if (S.result) render(); };
  // Review view defaults. A changed diff view applies at once, a changed
  // mode at the next reload.
  const mode = $("#review_mode");
  mode.value = reviewModeDefault();
  mode.onchange = () => save("reviewmode", mode.value);
  const view = $("#diff_view");
  view.value = diffViewDefault();
  view.onchange = () => { save("diff_view", view.value); S.view = S.wz.view = view.value; if (S.result) render(); };
  const autorun = $("#chat_autorun");
  autorun.value = ["local", "job"].includes(saved("chat_autorun")) ? saved("chat_autorun") : "view";
  autorun.onchange = () => save("chat_autorun", autorun.value);
  const agent = $("#chat_agent");
  agent.value = saved("chat_agent") === "installed" ? "installed" : "";
  agent.onchange = () => save("chat_agent", agent.value);
  const web = $("#chat_web");
  web.checked = saved("chat_web") !== "0";
  web.onchange = () => save("chat_web", web.checked ? "1" : "0");
  const focus = $("#focus_top");
  focus.checked = focusTop();
  focus.onchange = () => save("focus_top", focus.checked ? "1" : "0");
  const shown = $("#side_shown");
  shown.value = sideShown();
  shown.onchange = () => {
    shown.value = String(Math.max(0, Math.min(100, Math.floor(Number(shown.value)) || 0)));
    save("side_shown", shown.value);
    loadList();
  };
  const keep = $("#side_keep_days");
  keep.value = sideKeepDays();
  keep.onchange = () => {
    keep.value = String(Math.max(0, Math.min(365, Math.floor(Number(keep.value)) || 0)));
    save("side_keep_days", keep.value);
    loadList();
  };
  const confirmDel = $("#side_confirm_remove");
  confirmDel.checked = confirmRemove();
  confirmDel.onchange = () => save("side_confirm_remove", confirmDel.checked ? "1" : "0");
  initHotkey("chat_hotkey", hotkeyLabel);
  initHotkey("chat_hide_hotkey", hideLabel);
  const batch = $("#classify_batch");
  batch.checked = saved("classify_batch") ? saved("classify_batch") === "1" : S.cfg?.classify_batch !== false;
  batch.onchange = () => save("classify_batch", batch.checked ? "1" : "0");
  const lint = $("#lint");
  lint.checked = saved("lint") ? saved("lint") === "1" : S.cfg?.lint !== false;
  lint.onchange = () => save("lint", lint.checked ? "1" : "0");
  const incr = $("#incremental");
  incr.checked = saved("incremental") ? saved("incremental") === "1" : S.cfg?.incremental !== false;
  incr.onchange = () => save("incremental", incr.checked ? "1" : "0");
  const recursive = $("#recursive_fix");
  const location = $("#fix_location");
  location.value = saved("fix_location") || S.cfg?.fix_location || "worktree";
  location.onchange = () => save("fix_location", location.value);
  const uncommitted = $("#fix_uncommitted");
  uncommitted.value = saved("fix_uncommitted");
  uncommitted.onchange = () => (uncommitted.value ? save("fix_uncommitted", uncommitted.value) : localStorage.removeItem("pr-manager.fix_uncommitted"));
  recursive.checked = saved("recursive_fix") ? saved("recursive_fix") === "1" : S.cfg?.recursive_fix !== false;
  recursive.onchange = () => save("recursive_fix", recursive.checked ? "1" : "0");
  const agent = $("#fix_agent");
  agent.checked = saved("fix_agent") ? saved("fix_agent") === "1" : S.cfg?.fix_agent !== false;
  agent.onchange = () => save("fix_agent", agent.checked ? "1" : "0");
  const rounds = $("#max_fix_rounds");
  rounds.value = saved("max_fix_rounds") || S.cfg?.max_fix_rounds || 3;
  rounds.onchange = () => {
    rounds.value = String(Math.max(1, Math.min(10, Number(rounds.value) || 3)));
    save("max_fix_rounds", rounds.value);
  };
  fillLang();
  fillEffort();
  fillCodeSources();
  $("#providers-refresh").onclick = () => loadProviders(true);
  loadProviders(false);
  showBudget();
}
