// The Sequence tab: the call flow the PR changes, drawn from the shape
// the model returned (triage/sequence.go) rather than from text it wrote.
//
// It is drawn here, in SVG, instead of handed to a diagram library. That
// keeps the app dependency-free and offline, and it buys the thing a
// picture of a diagram cannot do: a changed step is a button, and
// clicking it opens the change unit responsible in the Review tab. The
// Mermaid text is offered too, for pasting into a PR description.
//
// The steps hold both sides of the PR (removed ones exist only before it,
// changed ones only after), so the before/after switch is a filter here,
// not a second request.
import { esc, postJSON } from "./util.js";
import { S, render } from "./state.js";
import { jobSettings } from "./settings.js";
import { jumpToUnit } from "./review.js";

const COL = 168;   // distance between participant columns
const ROW = 46;    // distance between steps
const PAD = 28;    // margin around the drawing
const HEAD = 46;   // height of the participant heads
const MINW = 120;

// SEQ_VERSION mirrors triage.SequenceVersion: an older saved diagram has no
// before side, so the server writes it again.
const SEQ_VERSION = 2;

// loadSequence gets the diagram when a result is opened. A triage starts
// writing it as soon as the result is saved, so the request usually waits
// on that call; a result from before gets one written now.
export async function loadSequence() {
  const r = S.result;
  if (!r || r.sequence?.version >= SEQ_VERSION || r.sequence_loading) return;
  r.sequence_loading = true;
  render();
  try {
    r.sequence = await postJSON(`/api/results/${encodeURIComponent(r.key)}/sequence`, jobSettings());
  } catch (e) {
    r.sequence_error = e.message;
  }
  r.sequence_loading = false;
  if (S.result === r) render();
}

// view mirrors Sequence.View in Go: the flow on one side of the PR, with
// changed meaning "the PR touches this step" on that side.
function view(sq, before) {
  const steps = sq.steps
    .filter((s) => (before ? !s.changed : !s.removed))
    .map((s) => ({ ...s, changed: before ? !!s.removed : !!s.changed, removed: false }));
  const used = new Set(steps.flatMap((s) => [s.from, s.to]));
  return { ...sq, before, steps, participants: sq.participants.filter((p) => used.has(p.id)) };
}

// layout puts every participant on a column and every step on a row.
function layout(sq) {
  const xs = new Map();
  sq.participants.forEach((p, i) => xs.set(p.id, PAD + MINW / 2 + i * COL));
  const width = PAD * 2 + MINW + Math.max(0, sq.participants.length - 1) * COL;
  const height = HEAD + PAD + sq.steps.length * ROW + PAD;
  return { xs, width, height };
}

function headsSVG(sq, { xs }) {
  return sq.participants.map((p) => {
    const x = xs.get(p.id);
    return `<g class="seq-actor">
      <rect x="${x - MINW / 2}" y="${PAD - 14}" width="${MINW}" height="30" rx="6"/>
      <text x="${x}" y="${PAD + 6}" text-anchor="middle">${esc(clip(p.label, 18))}<title>${esc(p.label)}${p.kind ? ` (${esc(p.kind)})` : ""}</title></text>
    </g>`;
  }).join("");
}

function lifelinesSVG(sq, { xs, height }) {
  return sq.participants.map((p) =>
    `<line class="seq-life" x1="${xs.get(p.id)}" y1="${PAD + 18}" x2="${xs.get(p.id)}" y2="${height - PAD / 2}"/>`).join("");
}

// stepSVG draws one arrow. A step this PR changed is highlighted, and one
// that names a change unit is clickable.
function stepSVG(s, i, { xs, width }, before) {
  const y = HEAD + PAD + i * ROW;
  const x1 = xs.get(s.from), x2 = xs.get(s.to);
  const cls = `seq-step ${s.kind}${s.changed ? " changed" : ""}${s.unit ? " linked" : ""}`;
  const attrs = s.unit ? ` data-act="seq-goto" data-unit="${esc(s.unit)}" tabindex="0" role="button"` : "";
  const verb = before ? "Removed or replaced" : "Changed";
  const title = `${esc(s.text)}${s.unit ? `\n\n${verb} by ${esc(s.unit)} — click to open it` : s.changed ? `\n\n${verb} by this PR` : ""}`;
  // The band marks a row the PR changed; it spans the drawing, so it has
  // to be measured, not set to 100% of a viewport it does not control.
  const band = s.changed
    ? `<rect class="seq-band" x="${PAD / 2}" y="${y - 20}" width="${width - PAD}" height="${ROW - 4}" rx="4"/>` : "";
  if (s.kind === "note") {
    const w = Math.min(width - PAD, Math.max(MINW, s.text.length * 6.6 + 20));
    const nx = Math.min(width - PAD / 2 - w, Math.max(PAD / 2, x1 - w / 2)); // keep it on the canvas
    return `<g class="${cls}"${attrs}><title>${title}</title>
      ${band}
      <rect class="seq-note" x="${nx}" y="${y - 15}" width="${w}" height="26" rx="4"/>
      <text x="${nx + w / 2}" y="${y + 3}" text-anchor="middle">${esc(clip(s.text, Math.floor(w / 6.6)))}</text></g>`;
  }
  const dir = x2 >= x1 ? 1 : -1;
  const from = x1 + dir * 4, to = x2 - dir * 9;
  const self = s.from === s.to;
  const path = self
    ? `M ${x1} ${y - 8} h 26 v 18 h -26`
    : `M ${from} ${y} H ${to}`;
  const tx = self ? x1 + 34 : (x1 + x2) / 2;
  const anchor = self ? "start" : "middle";
  const room = self ? 30 : Math.abs(x2 - x1) / 6.6 - 2;
  return `<g class="${cls}"${attrs}><title>${title}</title>
    ${band}
    <text x="${tx}" y="${y - 8}" text-anchor="${anchor}">${esc(clip(s.text, Math.max(8, Math.floor(room))))}</text>
    <path class="seq-arrow" d="${path}" marker-end="url(#seq-head)"/>
  </g>`;
}

const clip = (s, n) => (String(s).length > n ? String(s).slice(0, n - 1) + "…" : String(s));

function diagramSVG(sq) {
  const l = layout(sq);
  return `<svg class="seq-svg" viewBox="0 0 ${l.width} ${l.height}" width="${l.width}" height="${l.height}" role="img" aria-label="${esc(sq.title || "sequence diagram")}">
    <defs><marker id="seq-head" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" markerHeight="7" orient="auto-start-reverse">
      <path d="M 0 0 L 10 5 L 0 10 z"/></marker></defs>
    ${lifelinesSVG(sq, l)}
    ${sq.steps.map((s, i) => stepSVG(s, i, l, sq.before)).join("")}
    ${headsSVG(sq, l)}
  </svg>`;
}

export function sequenceHTML() {
  const r = S.result;
  if (r.sequence_loading) return `<div class="ov-loading"><span class="spinner"></span>Drawing the flow this PR changes…</div>`;
  if (r.sequence_error) return `<div class="empty">No diagram: ${esc(r.sequence_error)}</div>`;
  if (!r.sequence) return `<div class="empty">No diagram yet.</div>`;
  if (r.sequence.problem || !r.sequence.steps?.length) {
    return `<div class="empty">${esc(r.sequence.problem || "This PR has no single call flow to draw.")}</div>`;
  }
  const before = S.seqView === "before";
  const sq = view(r.sequence, before);
  const changed = sq.steps.filter((s) => s.changed).length;
  const hint = before
    ? `${changed} of ${sq.steps.length} steps are removed or replaced by this PR.`
    : `${changed} of ${sq.steps.length} steps change in this PR.`;
  const body = sq.participants.length < 2
    ? `<div class="empty">${before ? "This flow did not exist before the PR." : "The PR removes this flow."}</div>`
    : `<div class="seq-scroll">${diagramSVG(sq)}</div>
      ${S.seqText ? `<textarea class="seq-mermaid" readonly rows="${Math.min(24, sq.steps.length + sq.participants.length + 3)}">${esc(mermaid(sq))}</textarea>` : ""}`;
  return `<div class="seq${before ? " before" : ""}">
      <div class="seq-head">
        <h3>${esc(sq.title || "Changed flow")}</h3>
        <span class="spacer"></span>
        <span class="seg" role="group" aria-label="flow"><button class="${before ? "on" : ""}" data-act="seq-view" data-v="before">Before</button><button class="${before ? "" : "on"}" data-act="seq-view" data-v="after">After</button></span>
        <button class="details-btn" data-act="seq-copy">${S.seqCopied ? "Copied" : "Copy as Mermaid"}</button>
      </div>
      <p class="hint">${hint} Highlighted steps that name a change unit open it in the Review tab.</p>
      ${sq.note && !before ? `<p class="seq-note-line"><b>Watch:</b> ${esc(sq.note)}</p>` : ""}
      ${body}
    </div>`;
}

// mermaid mirrors Sequence.Mermaid in Go, so the copied text is the same
// diagram whichever side produces it.
function mermaid(sq) {
  const safe = (s) => String(s).replace(/[\n;<>#"]/g, (c) => ({ "\n": " ", ";": ",", "<": "(", ">": ")", "#": "no.", '"': "'" })[c]).trim();
  const out = ["sequenceDiagram"];
  if (sq.title) out.push("    autonumber", `    %% ${safe(sq.title)}`);
  for (const p of sq.participants) out.push(`    participant ${p.id} as ${safe(p.label)}`);
  let open = false;
  for (const s of sq.steps) {
    if (!!s.changed !== open) { out.push(s.changed ? "    rect rgb(255, 244, 214)" : "    end"); open = !!s.changed; }
    const ind = open ? "        " : "    ";
    if (s.kind === "note") out.push(`${ind}Note over ${s.from}: ${safe(s.text)}`);
    else out.push(`${ind}${s.from}${s.kind === "return" ? "-->>" : "->>"}${s.to}: ${safe(s.text)}`);
  }
  if (open) out.push("    end");
  return out.join("\n") + "\n";
}

export const actions = {
  "seq-goto": (el) => { jumpToUnit(el.dataset.unit); return false; },
  "seq-view": (el) => { S.seqView = el.dataset.v; },
  "seq-copy": async () => {
    const text = mermaid(view(S.result.sequence, S.seqView === "before"));
    try {
      await navigator.clipboard.writeText(text);
      S.seqCopied = true;
      setTimeout(() => { S.seqCopied = false; render(); }, 1500);
    } catch {
      S.seqText = !S.seqText; // no clipboard: show the text to copy by hand
    }
  },
};

// onKeydown makes a linked step reachable without a mouse.
export function onKeydown(e) {
  if (e.key !== "Enter" && e.key !== " ") return;
  const el = e.target.closest?.('[data-act="seq-goto"]');
  if (!el) return;
  e.preventDefault();
  jumpToUnit(el.dataset.unit);
}
