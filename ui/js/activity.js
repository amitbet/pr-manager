// Activity panel: the running job's threads (gh and git commands, each
// unit's classify and review calls) with their log lines, each collapsible.
// Updated in place so open threads and scroll positions survive a refresh.
import { esc, api } from "./util.js";

const KIND = { gh: "gh", git: "git", llm: "model", job: "job", "pr-manager": "codemap" };
const MAX_DOM_LINES = 400;

function took(t) {
  const end = t.end ? new Date(t.end) : new Date();
  const s = Math.max(0, (end - new Date(t.start)) / 1000);
  return s < 60 ? `${s.toFixed(1)}s` : `${Math.floor(s / 60)}m${String(Math.round(s % 60)).padStart(2, "0")}s`;
}

const clock = (t) => new Date(t).toLocaleTimeString([], { hour12: false });

const isDiff = (s) => /^(diff --git |--- |@@ )/.test(s) || /\n@@ -\d/.test(s);

function diffBlock(s) {
  const cls = (l) =>
    /^(diff --git|index |--- |\+\+\+ |new file|deleted file)/.test(l) ? "hd"
    : l.startsWith("@@") ? "hunk" : l.startsWith("+") ? "add" : l.startsWith("-") ? "del" : "";
  return `<div class="act-block act-diff">${s.split("\n").map((l) => `<span class="${cls(l)}">${esc(l) || " "}</span>`).join("")}</div>`;
}

// value lays out a model's structured output: fields as labeled rows,
// multi-line text and patches as blocks, lists as numbered items.
function value(v) {
  if (v === null || v === undefined) return `<span class="act-nil">null</span>`;
  if (typeof v === "string") {
    if (isDiff(v)) return diffBlock(v);
    if (v.includes("\n") || v.length > 160) return `<div class="act-block">${esc(v)}</div>`;
    return v ? `<span class="act-str">${esc(v)}</span>` : `<span class="act-nil">""</span>`;
  }
  if (typeof v !== "object") return `<span class="act-lit">${esc(String(v))}</span>`;
  if (Array.isArray(v)) {
    if (!v.length) return `<span class="act-nil">none</span>`;
    if (v.every((x) => x === null || typeof x !== "object") && v.join(", ").length < 160 && !v.some((x) => typeof x === "string" && x.includes("\n")))
      return `<span class="act-str">${v.map((x) => esc(String(x))).join(", ")}</span>`;
    return `<ol class="act-arr">${v.map((x) => `<li>${value(x)}</li>`).join("")}</ol>`;
  }
  const keys = Object.keys(v);
  if (!keys.length) return `<span class="act-nil">{}</span>`;
  return `<div class="act-obj">${keys.map((k) => `<div class="act-k">${esc(k)}</div><div class="act-v">${value(v[k])}</div>`).join("")}</div>`;
}

// output renders a line's attached data as a collapsible section, open
// when small enough to read at a glance.
function output(data) {
  const raw = JSON.stringify(data);
  const keys = data && typeof data === "object" && !Array.isArray(data) ? Object.keys(data).join(", ") : "";
  return `<details class="act-data"${raw.length < 1500 ? " open" : ""}><summary>output${keys ? ` · ${esc(keys)}` : ""}<button type="button" class="linkbtn act-copy">copy JSON</button></summary>${value(data)}</details>`;
}

// mountActivity renders the panel into el and returns refresh(), which
// fetches the job's log and updates the panel.
export function mountActivity(el, jobId) {
  el.innerHTML = `<div class="act">
    <div class="act-head">
      <b>Activity</b><span class="act-count"></span><span class="spacer"></span>
      <label class="act-opt"><input type="checkbox" class="act-follow"> follow running</label>
      <label class="act-opt"><input type="checkbox" class="act-show" checked> show finished</label>
      <button type="button" class="linkbtn act-open">expand all</button>
      <button type="button" class="linkbtn act-close">collapse all</button>
    </div>
    <div class="act-list"></div>
  </div>`;
  const list = el.querySelector(".act-list");
  const follow = el.querySelector(".act-follow");
  const show = el.querySelector(".act-show");
  const rows = new Map(); // thread id -> { el, seen, touched }
  show.onchange = () => list.classList.toggle("hide-done", !show.checked);
  el.querySelector(".act-open").onclick = () => rows.forEach((r) => { r.el.open = true; r.touched = true; });
  el.querySelector(".act-close").onclick = () => rows.forEach((r) => { r.el.open = false; r.touched = true; });

  function row(t) {
    let r = rows.get(t.id);
    if (r) return r;
    const d = document.createElement("details");
    d.className = "act-thread";
    d.innerHTML = `<summary><span class="act-dot"></span><span class="act-kind"></span><span class="act-name"></span><span class="act-time"></span></summary><pre class="act-lines"></pre>`;
    d.querySelector(".act-kind").textContent = KIND[t.kind] || t.kind;
    d.querySelector(".act-name").textContent = t.name;
    d.querySelector(".act-name").title = t.name;
    r = { el: d, seen: 0, touched: false };
    // A thread the user opened or closed keeps that state.
    d.querySelector("summary").addEventListener("click", () => { r.touched = true; });
    rows.set(t.id, r);
    list.appendChild(d);
    return r;
  }

  function update(t) {
    const r = row(t);
    const d = r.el;
    d.dataset.status = t.status;
    d.querySelector(".act-time").textContent = `${t.lines.length ? t.lines.length + (t.dropped || 0) + " lines · " : ""}${took(t)}`;
    if (!r.touched && follow.checked) d.open = t.status === "running";
    const total = (t.dropped || 0) + t.lines.length;
    const fresh = t.lines.slice(Math.max(0, t.lines.length - (total - r.seen)));
    if (!fresh.length) return;
    r.seen = total;
    const pre = d.querySelector(".act-lines");
    const atBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 20;
    const frag = document.createDocumentFragment();
    for (const l of fresh) {
      const div = document.createElement("div");
      div.className = /^(error|✗|stderr:)/.test(l.text) ? "act-line err" : "act-line";
      div.innerHTML = `<span class="act-ts">${esc(clock(l.t))}</span>${esc(l.text)}${l.data !== undefined ? output(l.data) : ""}`;
      const copy = div.querySelector(".act-copy");
      if (copy) copy.onclick = (e) => {
        e.preventDefault();
        navigator.clipboard?.writeText(JSON.stringify(l.data, null, 2)).then(() => { copy.textContent = "copied"; });
      };
      frag.appendChild(div);
    }
    pre.appendChild(frag);
    while (pre.childElementCount > MAX_DOM_LINES) pre.firstElementChild.remove();
    if (atBottom) pre.scrollTop = pre.scrollHeight;
  }

  return async function refresh() {
    const threads = (await api(`/api/jobs/${jobId}/log`)).map((t) => ({ ...t, lines: t.lines || [] }));
    const listAtBottom = list.scrollHeight - list.scrollTop - list.clientHeight < 20;
    threads.forEach(update);
    const running = threads.filter((t) => t.status === "running").length;
    const failed = threads.filter((t) => t.status === "error").length;
    el.querySelector(".act-count").textContent =
      ` ${threads.length} threads · ${running} running${failed ? ` · ${failed} failed` : ""}`;
    if (listAtBottom) list.scrollTop = list.scrollHeight;
  };
}
