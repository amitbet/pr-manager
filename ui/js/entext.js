// The English that translation replaced, so each translated text can be
// read in English too: the EN toggle next to it.
import { esc } from "./util.js";
import { S } from "./state.js";

export const english = new WeakMap(); // result -> {lang, units: unit id -> text}

// original is the English at path ("summary", "focus.0", "issues.1.title")
// of u, or undefined when u wasn't translated.
function original(u, path) {
  const en = english.get(S.result)?.units[u.id];
  if (!en) return undefined;
  let v = en;
  for (const k of path.split(".")) v = v?.[k];
  return path === "headline" ? v || u.decision.headline : v;
}

const keyOf = (u, path) => `${u.id}|${path}`;
const translated = (u, path, text) => { const en = original(u, path); return en != null && en !== text ? en : undefined; };

// trShown is the text at path as it is to be shown: the translation, or
// the English when its toggle is on.
const shown = (u, path, text) => {
  const en = translated(u, path, text);
  return en !== undefined && S.showEn.has(keyOf(u, path)) ? en : text;
};
export const trShown = (u, path, text) => esc(shown(u, path, text));

// trDir is the dir attribute for the text at path as shown. A translation
// often opens with an identifier ("SystemdProperties now …"), which makes
// dir="auto" take a Hebrew or Arabic sentence as left-to-right, so any
// right-to-left letter makes it rtl.
const RTL = /[\u0590-\u08FF\uFB1D-\uFDFF\uFE70-\uFEFF]/;
export const trDir = (u, path, text) => `dir="${RTL.test(shown(u, path, text) || "") ? "rtl" : "auto"}"`;

// trToggle is the EN button for a translated text, "" for the rest.
export function trToggle(u, path, text) {
  if (translated(u, path, text) === undefined) return "";
  const key = keyOf(u, path), on = S.showEn.has(key);
  return `<button class="tr-en${on ? " on" : ""}" data-act="tr-en" data-key="${esc(key)}" title="${on ? `Back to ${esc(S.result.summary_lang)}` : "Show the English"}">EN</button>`;
}

export const trText = (u, path, text) => trShown(u, path, text) + trToggle(u, path, text);

export const actions = {
  "tr-en": (el) => { const k = el.dataset.key; if (!S.showEn.delete(k)) S.showEn.add(k); },
};
