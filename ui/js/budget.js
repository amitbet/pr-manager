// Review budget: re-buckets a result's units from their scores without a
// re-run. place mirrors Score.Place in triage/tiers.go.
import { BUCKETS } from "./util.js";

const RANK = { none: 0, aux: 1, skim: 2, human: 3 };
const KEY = "pr-manager.review_budget";

// budgets are the result's own steps (its repo policy), else the server's.
export const budgets = (r, cfg) => (r?.budgets?.length ? r.budgets : cfg?.budgets) || [];

// chosen is the saved budget if this list has it, else the default.
export function chosen(list, def) {
  const want = localStorage.getItem(KEY);
  if (list.some((b) => b.name === want)) return want;
  return list.some((b) => b.name === def) ? def : list[Math.floor(list.length / 2)]?.name;
}
export const choose = (name) => localStorage.setItem(KEY, name);

const g2 = (x) => String(+x.toFixed(2)); // Go's %.2g for these values

export function place(u, b) {
  const s = u.score, att = u.attention || 0;
  const f = b.trust * (s.clean || 0);
  const lowered = Math.round(s.prior * (1 - f));
  const total = Math.max(att, lowered);
  // A comment pin wins over any other pin but human (Score.pin in Go).
  const [pin, pinWhy] = s.comment_pin && s.pin !== "human" ? [s.comment_pin, s.comment_pin_why] : [s.pin, s.pin_why];
  if (pin) {
    // The code floor and aux raise a pin from the classifier's bucket (Score.Place).
    if (s.code_floor && RANK[s.code_floor] > RANK[pin]) return { bucket: s.code_floor, total, why: `${s.code_floor}: ${s.code_floor_why} (any budget; the classifier's ${s.pin})` };
    if (s.aux && RANK[pin] < RANK.aux) return { bucket: "aux", total, why: `aux: ${s.aux} (any budget; the classifier's ${s.pin})` };
    return { bucket: pin, total, why: `${pin}: ${pinWhy} (any budget)` };
  }
  let bucket = "none", cut = `< ${b.skim}`;
  if (total >= b.human) [bucket, cut] = ["human", `≥ ${b.human}`];
  else if (total >= b.skim) [bucket, cut] = ["skim", `≥ ${b.skim}`];
  let x = String(s.base);
  if (s.kind !== 1) x += ` × kind ${g2(s.kind)}`;
  if (f > 0) x += ` × ${g2(1 - f)} (${s.clean < 1 ? "only low issues" : "clean review"})`;
  if (att > lowered) x = `review attention ${att} (over ${x})`;
  let why = `score ${total} = ${x} → ${bucket} (${cut} on ${b.name})`;
  if (s.floor && RANK[s.floor] > RANK[bucket]) {
    if (b.lift_floors && s.clean === 1) why += `; ${s.floor_why} lifted by the clean review`;
    else {
      bucket = s.floor;
      why += `; raised to ${bucket}: ${s.floor_why}`;
    }
  }
  if (s.aux && RANK[bucket] < RANK.aux) {
    bucket = "aux";
    why += `; raised to aux: ${s.aux}`;
  }
  // No budget lifts the code floor; only a measured risk under skim does.
  if (s.code_floor && RANK[s.code_floor] > RANK[bucket]) {
    if (s.risk_known && s.base < b.skim) why += `; risk ${s.base} < ${b.skim}: measured impact and likelihood alone put it under skim`;
    else {
      bucket = s.code_floor;
      why += `; raised to ${bucket}: ${s.code_floor_why} (no budget lifts this)`;
    }
  }
  return { bucket, total, why };
}

// counts is how many units each bucket gets under b.
export function counts(r, b) {
  const c = Object.fromEntries(BUCKETS.map((k) => [k, 0]));
  for (const f of r.files) for (const u of f.units || []) c[u.score ? place(u, b).bucket : u.decision.bucket]++;
  return c;
}

// apply re-buckets r in place under the chosen budget. Results cached
// before scores keep their buckets.
export function apply(r, cfg) {
  const list = budgets(r, cfg);
  const b = list.find((x) => x.name === chosen(list, r.review_budget || cfg?.review_budget));
  const units = r.files.flatMap((f) => f.units || []);
  if (!b || !units.some((u) => u.score)) return;
  for (const u of units) {
    if (!u.score) continue;
    const p = place(u, b);
    u.decision.bucket = p.bucket;
    Object.assign(u.score, { total: p.total, why: p.why, budget: b.name });
  }
  r.counts = counts(r, b);
  r.shown_budget = b.name;
}
