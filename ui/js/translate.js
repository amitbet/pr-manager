// Summary language. PRs are reviewed in English; another language is
// translated by the server at the end of the triage (or, failing that, when
// the PR is opened), and cached there. The
// English text is kept so switching language, or back to English, needs
// no new triage.
import { postJSON } from "./util.js";
import { S, render } from "./state.js";
import { jobSettings, summaryLang } from "./settings.js";
import { syncComposer } from "./comments.js";
import * as budget from "./budget.js";
import { english } from "./entext.js";
import { OVERVIEW_ID, overviewText } from "./overview.js";
let seq = 0;

const units = (r) => r.files.flatMap((f) => f.units || []);
const textOf = (u) => ({
  headline: u.headline, summary: u.summary, focus: u.focus && [...u.focus],
  issues: (u.issues || []).map((i) => ({ title: i.title, detail: i.detail, failure_scenario: i.failure_scenario })),
  reason: u.decision.reason, escalated: u.decision.escalated && [...u.decision.escalated],
  pin_why: u.score?.pin_why, floor_why: u.score?.floor_why,
});

function put(u, t, keep) {
  const set = (o, k, v) => { if (o && (v || !keep)) o[k] = v; };
  const list = (to, from) => { if (from && to?.length === from.length) from.forEach((v, i) => set(to, i, v)); };
  set(u, "headline", t.headline);
  set(u, "summary", t.summary);
  list(u.focus, t.focus);
  if (t.issues && (u.issues || []).length === t.issues.length) {
    t.issues.forEach((is, i) => { for (const k of ["title", "detail", "failure_scenario"]) set(u.issues[i], k, is[k]); });
  }
  set(u.decision, "reason", t.reason);
  list(u.decision.escalated, t.escalated);
  set(u.score, "pin_why", t.pin_why);
  set(u.score, "floor_why", t.floor_why);
}

// restore puts the result's own text back.
function restore(r) {
  const en = english.get(r);
  if (!en) return;
  for (const u of units(r)) if (en.units[u.id]) put(u, en.units[u.id], false);
  if (en.units[OVERVIEW_ID]) r.overview = en.units[OVERVIEW_ID];
  r.summary_lang = en.lang;
  english.delete(r);
  S.showEn.clear();
  budget.apply(r, S.cfg); // the bucket text quotes pin_why and floor_why
}

// translate shows the open result in the chosen summary language. English
// (or the result's own language) makes no request.
export async function translate() {
  const r = S.result;
  if (!r) return;
  const n = ++seq;
  const had = english.has(r) || r.translating || r.translate_error;
  restore(r);
  r.translating = r.translate_error = "";
  const lang = summaryLang();
  if (!lang || lang.toLowerCase() === "english" || lang.toLowerCase() === (r.summary_lang || "").toLowerCase()) {
    if (had) { syncComposer(); render(); }
    return;
  }
  r.translating = lang;
  syncComposer();
  render();
  try {
    const tr = await postJSON(`/api/results/${encodeURIComponent(r.key)}/translate`, { ...jobSettings(), summary_lang: lang });
    if (n !== seq || S.result !== r) return;
    if (tr.lang) {
      const en = Object.fromEntries(units(r).map((u) => [u.id, textOf(u)]));
      if (r.overview) en[OVERVIEW_ID] = overviewText(r.overview);
      english.set(r, { lang: r.summary_lang, units: en });
      for (const u of units(r)) if (tr.units[u.id]) put(u, tr.units[u.id], true);
      if (r.overview && tr.overview) r.overview = tr.overview;
      r.summary_lang = tr.lang;
      budget.apply(r, S.cfg);
    }
  } catch (e) {
    if (n !== seq || S.result !== r) return;
    r.translate_error = `not translated to ${lang}: ${e.message}`;
  }
  r.translating = "";
  syncComposer();
  render();
}
