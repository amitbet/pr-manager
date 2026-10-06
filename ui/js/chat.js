// Chat agent: ask about the result on screen, from any tab. The server
// gives the model everything the app knows about the change (the review,
// issues, code map, sequence, fixes and job activity, see chat.go) plus
// what the reader is looking at, which the host tells it.
//
// The panel starts collapsed to an icon at the bottom right on every page
// load; A or Ctrl+/ (⌘/ on a Mac), or the hotkey set in Settings, opens
// it with the cursor in the prompt, and Esc, or the collapse hotkey set in
// Settings, collapses it again. The icon, and the floating panel by its
// header, can be dragged anywhere in the window. Opened, it floats
// at the bottom right or docks as a column on the right, and it can pop
// out into a window of its own (chat.html) to put on another screen. The
// main window and the pop-out talk over a BroadcastChannel: the main
// window sends what is on screen, the pop-out sends clicks on units back.
//
// The agent can also act: its reply lists actions (open a unit, draft a
// comment, fix issues, analyze parts again, post to GitHub; the catalog is
// chatactions.go), shown as cards under its answer. The main window runs
// them through the agent API (js/agentapi.js); the pop-out asks the main
// window to. What happened goes back into the conversation as an event
// turn with its time, and the agent continues from there.
//
// Conversations are kept per change (the PR, or the local checkout), so
// they carry on across re-triages and fixes, in localStorage under
// prm-chat.<change>, outside pr-manager.*, so they don't go into the
// server's settings file. Every turn has its time.
import { esc, api, say } from "./util.js";

const ls = (k) => localStorage.getItem(`pr-manager.${k}`) || "";
const setLs = (k, v) => localStorage.setItem(`pr-manager.${k}`, v);
const convKey = (id) => `prm-chat.${id}`;
const MAX_KEPT = 80;

function loadConv(id) {
  try { return JSON.parse(localStorage.getItem(convKey(id))) || []; } catch { return []; }
}
function saveConv(id, msgs) {
  if (msgs.length) localStorage.setItem(convKey(id), JSON.stringify(msgs.slice(-MAX_KEPT)));
  else localStorage.removeItem(convKey(id));
}

// Who may run what without asking (Settings → Chat agent): navigation
// always; with "local" also drafts, dismissals and marks; with "job" also
// fixes and analysis. Posting to GitHub always asks.
const RISKS = ["view", "local", "job", "outward"];
const RISK_LABEL = { view: "view", local: "edit", job: "job", outward: "posts to GitHub" };
const autoRuns = (risk) => {
  const max = ["view", "local", "job"].indexOf(ls("chat_autorun") || "view");
  const i = RISKS.indexOf(risk);
  return i >= 0 && i <= Math.max(0, max) && risk !== "outward";
};
// MAX_CHAIN is how many answers the agent may give in a row on its own,
// continuing after its actions, before the reader says something.
const MAX_CHAIN = 3;
const now = () => new Date().toISOString();
const clock = (at) => at ? new Date(at).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) : "";

const channel = typeof BroadcastChannel === "function" ? new BroadcastChannel("pr-manager-chat") : null;
const post = (m) => channel?.postMessage(m);
// The desktop app is one webview window: no pop-out there.
const canPopOut = !!channel && !window.runtime && !window.wails && !window.chrome?.webview;

// The hotkey: by default A when not typing, or Ctrl+/ (⌘/) from anywhere.
// Settings can replace both with one combo, kept as pr-manager.chat_hotkey
// ("Ctrl+Shift+K", "Mod+/", "A"): Mod is ⌘ on a Mac and Ctrl elsewhere. A
// combo without Ctrl, Alt or ⌘ only fires when not typing. The collapse
// hotkey, pr-manager.chat_hide_hotkey, works the same way, next to Esc in
// the chat; set to the open hotkey, the one key toggles the chat.
const isMac = /Mac|iPhone|iPad/.test(navigator.platform);
const MODS = ["Mod", "Ctrl", "Alt", "Shift", "Meta"];
const keyName = (k) => (k === " " ? "Space" : k.length === 1 ? k.toUpperCase() : k);

// comboFromEvent is the combo a keydown makes, or "" for a lone modifier
// or a key the chat already uses.
export function comboFromEvent(e) {
  if (["Control", "Alt", "Shift", "Meta", "Escape", "Enter", "Tab", "Dead", "Unidentified"].includes(e.key)) return "";
  const mod = isMac ? e.metaKey : e.ctrlKey;
  const mods = [mod && "Mod", (isMac ? e.ctrlKey : false) && "Ctrl", e.altKey && "Alt", e.shiftKey && "Shift", (!isMac && e.metaKey) && "Meta"];
  // Alt changes the character (⌥J types ∆ on a Mac): name the key instead.
  const code = e.altKey && e.code?.match(/^(?:Key|Digit)(\w)$/)?.[1];
  return [...mods.filter(Boolean), code || keyName(e.key)].join("+");
}

// The default also takes Ctrl+/ on a Mac.
const DEFAULT_COMBOS = isMac ? ["A", "Mod+/", "Ctrl+/"] : ["A", "Mod+/"];
const combos = () => { const c = ls("chat_hotkey"); return c ? [c] : DEFAULT_COMBOS; };

// comboLabel shows a combo the way this platform writes it.
export function comboLabel(c) {
  const i = c.length > 1 ? c.lastIndexOf("+", c.length - 2) : -1; // the key can be +
  const key = c.slice(i + 1), parts = i < 0 ? [] : c.slice(0, i).split("+");
  const sym = isMac ? { Mod: "⌘", Ctrl: "⌃", Alt: "⌥", Shift: "⇧" } : { Mod: "Ctrl", Meta: "Win" };
  const mods = parts.filter((m) => MODS.includes(m)).map((m) => sym[m] || m);
  return isMac ? mods.join("") + key : [...mods, key].join("+");
}

// hotkeyLabel is the hotkey as the chat's hover text names it.
export const hotkeyLabel = () => (ls("chat_hotkey") ? comboLabel(ls("chat_hotkey")) : DEFAULT_COMBOS.slice(0, 2).map(comboLabel).join(" or "));

// hideLabel is the collapse hotkey as the collapse button names it.
export const hideLabel = () => (ls("chat_hide_hotkey") ? `Esc or ${comboLabel(ls("chat_hide_hotkey"))}` : "Esc");

const pressed = (e, list) => {
  const c = comboFromEvent(e);
  if (!c || !list.includes(c)) return false;
  if (e.ctrlKey || e.metaKey || e.altKey) return true;
  return !e.target.closest?.("input, textarea, select, [contenteditable]:not([contenteditable=false])");
};

const hotkey = (e) => pressed(e, combos());
const hideHotkey = (e) => !!ls("chat_hide_hotkey") && pressed(e, [ls("chat_hide_hotkey")]);

const SUGGEST = [
  "What should I look at first in this PR?",
  "Explain what I'm looking at.",
  "Which open issues are real, and which look wrong?",
  "What is the riskiest part of this change, and why?",
];

// md renders the agent's Markdown: fenced code, headings, lists,
// paragraphs, inline code, bold, italic, links and [[unit:ID]] references.
// Everything is escaped first; only these tags are made.
export function md(text, unitLabel = (id) => id) {
  const emph = (s) => s.replace(/\*\*([^*\n]+)\*\*/g, "<b>$1</b>").replace(/(^|[^*\w])\*([^*\s][^*\n]*?)\*(?![\w*])/g, "$1<i>$2</i>");
  const inline = (raw) => {
    const re = /`([^`\n]+)`|\[\[unit:([^\]\n]+)\]\]|\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)/g;
    let out = "", last = 0, m;
    while ((m = re.exec(raw))) {
      out += emph(esc(raw.slice(last, m.index)));
      if (m[1] != null) out += `<code>${esc(m[1])}</code>`;
      else if (m[2] != null) out += `<button type="button" class="chat-unit" data-unit="${esc(m[2].trim())}" title="Open ${esc(m[2].trim())}">${esc(unitLabel(m[2].trim()))}</button>`;
      else out += `<a href="${esc(m[4])}" target="_blank" rel="noopener">${emph(esc(m[3]))}</a>`;
      last = re.lastIndex;
    }
    return out + emph(esc(raw.slice(last)));
  };
  const lines = String(text || "").replace(/\r\n/g, "\n").split("\n");
  let html = "", para = [], list = null;
  const endPara = () => { if (para.length) html += `<p>${para.map(inline).join("<br>")}</p>`; para = []; };
  const endList = () => { if (list) html += `<${list.tag}>${list.items.map((x) => `<li>${inline(x)}</li>`).join("")}</${list.tag}>`; list = null; };
  for (let i = 0; i < lines.length; i++) {
    const l = lines[i];
    const fence = l.match(/^\s*```\s*(\S*)/);
    if (fence) {
      endPara(); endList();
      const code = [];
      while (++i < lines.length && !/^\s*```/.test(lines[i])) code.push(lines[i]);
      const body = fence[1] === "diff"
        ? code.map((c) => `<span class="${c[0] === "+" ? "add" : c[0] === "-" ? "del" : ""}">${esc(c)}</span>`).join("\n")
        : esc(code.join("\n"));
      html += `<pre><code>${body}</code></pre>`;
      continue;
    }
    const h = l.match(/^#{1,6}\s+(.*)/);
    const li = l.match(/^\s*([-*+]|\d+[.)])\s+(.*)/);
    if (!l.trim()) { endPara(); endList(); }
    else if (h) { endPara(); endList(); html += `<h5>${inline(h[1])}</h5>`; }
    else if (li) {
      endPara();
      const tag = /\d/.test(li[1]) ? "ol" : "ul";
      if (list && list.tag !== tag) endList();
      list ??= { tag, items: [] };
      list.items.push(li[2]);
    } else if (list && /^\s+\S/.test(l)) list.items[list.items.length - 1] += " " + l.trim();
    else { endList(); para.push(l.replace(/^>\s?/, "")); }
  }
  endPara(); endList();
  return html;
}

const ICON = {
  chat: `<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z"/></svg>`,
  dock: `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2"/><path d="M15 4v16"/></svg>`,
  float: `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><rect x="3" y="4" width="18" height="16" rx="2"/><rect x="12" y="12" width="7" height="6" rx="1"/></svg>`,
  out: `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M14 4h6v6M20 4l-9 9M18 14v5a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V7a1 1 0 0 1 1-1h5"/></svg>`,
  back: `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M10 20H4v-6M4 20l9-9M6 10V5a1 1 0 0 1 1-1h12a1 1 0 0 1 1 1v12a1 1 0 0 1-1 1h-5"/></svg>`,
  fresh: `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M12 5v14M5 12h14"/></svg>`,
  min: `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" aria-hidden="true"><path d="M5 12h14"/></svg>`,
};

// initChat draws the chat into #chat-root. host tells it about the page:
//   key()        the result on screen (null: no chat)
//   title()      its title
//   where()      what is on screen: {where, units, path} (see chatView in chat.go)
//   openUnit(id) shows a unit
//   unitLabel(id) a short name for a unit
//   cfg()        GET /api/config, for the default chat model
//   change()     the change the result is of: its conversation's id
//   describe(name, args), runAction(name, args, progress): the agent API
//                (main window only; the pop-out asks the main window)
// With popout set this is the pop-out window: it fills the page, and host
// learns what is on screen from the main window (setView).
export function initChat(host, { popout = false } = {}) {
  const root = document.getElementById("chat-root");
  const st = {
    open: popout, // collapsed on every load, so it never takes room unasked
    mode: ls("chat_mode") === "dock" ? "dock" : "float",
    popped: false, // main window: the chat is in the pop-out
    unread: false,
  };
  const busy = new Map(); // conversation -> AbortController, requests from this window
  const remoteBusy = new Set(); // conversations the other window is waiting on
  const drafts = {};
  const progress = new Map(); // action id -> a running job's stage
  let popWin = null, drawn = "", lastCount = -1;
  // cid is the open conversation: the change's. One kept under the result
  // key, from before conversations followed the change, moves over.
  const cid = () => {
    const id = host.change?.() || host.key();
    const key = host.key();
    if (id && key && id !== key && !localStorage.getItem(convKey(id)) && localStorage.getItem(convKey(key))) {
      localStorage.setItem(convKey(id), localStorage.getItem(convKey(key)));
      localStorage.removeItem(convKey(key));
    }
    return id;
  };

  const visible = () => !!host.key() && st.open && !st.popped;
  const applyLayout = () => {
    const docked = !popout && visible() && st.mode === "dock";
    const was = document.documentElement.classList.contains("chat-docked");
    document.documentElement.classList.toggle("chat-docked", docked);
    if (ls("chat_w")) document.documentElement.style.setProperty("--chat-w", `${ls("chat_w")}px`);
    // A sticky walkthrough bar takes the bottom of the window.
    document.documentElement.classList.toggle("chat-above-nav", !!document.querySelector(".wz-nav"));
    if (was !== docked) dispatchEvent(new Event("resize")); // the code map lays out again
  };

  function modelPick() {
    const cfg = host.cfg() || {};
    const prov = ls("chat");
    return { chat: prov, chat_model: ls(`chat_model.${prov || cfg.chat || ""}`) };
  }
  function modelLabel() {
    const cfg = host.cfg() || {};
    const prov = ls("chat") || cfg.chat || "default";
    const m = ls(`chat_model.${prov}`) || (!ls("chat") || ls("chat") === cfg.chat ? cfg.chat_model : "");
    return m ? `${prov}/${m}` : prov;
  }

  function whereText() {
    const w = host.where() || {};
    const units = (w.units || []).slice(0, 3).map((id) => host.unitLabel(id));
    const more = (w.units || []).length > 3 ? ` +${w.units.length - 3}` : "";
    return [w.where, w.path, units.length ? units.join(", ") + more : ""].filter(Boolean).join(" · ");
  }

  function actionHTML(a) {
    const foot = {
      proposed: `<button type="button" class="primary" data-chat="act-run" data-a="${esc(a.id)}">${a.risk === "outward" ? "Post" : "Run"}</button> <button type="button" data-chat="act-skip" data-a="${esc(a.id)}">Skip</button>`,
      running: `<span class="spinner"></span> ${esc(progress.get(a.id) || "running…")}`,
      done: `✓ ${esc(a.result || "done")}`,
      failed: `✗ ${esc(a.result || "failed")} <button type="button" class="linkbtn" data-chat="act-run" data-a="${esc(a.id)}">Retry</button>`,
      declined: "skipped",
      interrupted: "lost track of it when the page reloaded; see the jobs in the sidebar",
    }[a.status] || esc(a.status);
    return `<div class="chat-act risk-${esc(a.risk)} st-${esc(a.status)}">
      <div class="chat-act-head"><span class="chat-risk" title="${a.risk === "outward" ? "Always asks first" : autoRuns(a.risk) ? "Runs without asking (Settings → Chat agent)" : "Asks first (Settings → Chat agent)"}">${esc(RISK_LABEL[a.risk] || a.risk)}</span> <b>${esc(a.label || a.name)}</b></div>
      ${a.why ? `<div class="chat-act-why">${esc(a.why)}</div>` : ""}
      ${a.preview ? `<blockquote class="chat-act-preview">${esc(a.preview)}</blockquote>` : ""}
      <div class="chat-act-foot">${foot}${a.at ? ` <span class="chat-time">${esc(clock(a.at))}</span>` : ""}</div>
    </div>`;
  }

  function messageHTML(m, i) {
    // Events are for the agent; the action cards show them to the reader.
    if (m.role === "event") return "";
    if (m.role === "user") return `<div class="chat-msg user" title="${esc(m.at ? new Date(m.at).toLocaleString() : "")}">${esc(m.content)}</div>`;
    if (m.error) return `<div class="chat-msg err" role="alert">${esc(m.content)} <button type="button" class="linkbtn" data-chat="retry">Retry</button></div>`;
    const meta = [clock(m.at), m.model, m.secs ? `${m.secs}s` : ""].filter(Boolean).join(" · ");
    return `<div class="chat-msg bot"><div class="chat-md">${md(m.content, host.unitLabel)}</div>
      ${(m.actions || []).map(actionHTML).join("")}
      <div class="chat-meta">${esc(meta)} <button type="button" class="linkbtn" data-chat="copy" data-i="${i}">copy</button></div></div>`;
  }

  function draw() {
    const key = host.key();
    applyLayout();
    if (!key) { root.hidden = true; root.innerHTML = ""; drawn = ""; return; }
    root.hidden = false;
    root.className = popout ? "chat-window" : st.popped ? "chat-launch-wrap" : st.open ? `chat-${st.mode}` : "chat-launch-wrap";
    sizeFloat();
    if (!popout && (st.popped || !st.open)) {
      const waiting = busy.has(cid()) || remoteBusy.has(cid()) || running();
      const keys = hotkeyLabel();
      const title = st.popped ? `The chat is in another window: bring it to the front (${keys})` : `Ask the chat agent about this PR (${keys})`;
      root.innerHTML = `<button type="button" class="chat-launch${st.popped ? " popped" : ""}" data-chat="${st.popped ? "focus" : "open"}" data-drag title="${title} · drag to move" aria-label="${title}">${ICON.chat}${st.unread ? `<span class="chat-unread"></span>` : ""}${waiting ? `<span class="chat-busy"></span>` : ""}</button>`;
      place();
      drawn = "";
      return;
    }
    const input = root.querySelector("#chat-input");
    const focused = input && document.activeElement === input;
    const sel = focused ? [input.selectionStart, input.selectionEnd] : null;
    const log = root.querySelector(".chat-log");
    const atBottom = !log || log.scrollHeight - log.scrollTop - log.clientHeight < 40;
    const prevTop = log?.scrollTop;
    const id = cid();
    const msgs = loadConv(id);
    const waiting = busy.has(id) || remoteBusy.has(id);
    const btn = (act, icon, title) => `<button type="button" class="icon" data-chat="${act}" title="${title}" aria-label="${title}">${icon}</button>`;
    root.innerHTML = `
      ${!popout ? `<div class="chat-grip" data-grip title="Drag to resize"></div>` : ""}
      <div class="chat-head"${popout || st.mode === "dock" ? "" : ` data-drag title="Drag to move · double-click to put it back in the corner"`}>
        <span class="chat-title">${ICON.chat} <b>Ask</b> <span class="chat-model" title="Chat agent model, set in Settings → Models">${esc(modelLabel())}</span></span>
        <span class="spacer"></span>
        ${msgs.length ? btn("new", ICON.fresh, "New conversation") : ""}
        ${popout ? btn("dock-back", ICON.back, "Put the chat back in the main window")
          : `${btn("mode", st.mode === "dock" ? ICON.float : ICON.dock, st.mode === "dock" ? "Float over the page" : "Dock to the right")}${canPopOut ? btn("popout", ICON.out, "Pop out into its own window") : ""}${btn("close", ICON.min, `Collapse to an icon (${hideLabel()})`)}`}
      </div>
      <div class="chat-where" title="Sent with each question, so the agent knows what you mean by &quot;this&quot;">${popout && host.title() ? `<b>${esc(host.title())}</b> · ` : ""}Looking at: ${esc(whereText() || "—")}</div>
      <div class="chat-log" aria-live="polite">
        ${msgs.length ? msgs.map(messageHTML).join("") : `<div class="chat-empty"><p>Ask about this PR. The agent has the review, issues, code map, sequence, fixes and job activity, and knows what you are looking at. It can also act for you: draft comments, dismiss or fix issues, analyze parts again, and reply on GitHub, each with your OK.</p>
          ${SUGGEST.map((s) => `<button type="button" class="chat-suggest" data-chat="ask">${esc(s)}</button>`).join("")}</div>`}
        ${waiting ? `<div class="chat-msg bot thinking"><span class="spinner"></span> Thinking…</div>` : ""}
      </div>
      <form class="chat-form" data-chat-form>
        <textarea id="chat-input" rows="1" placeholder="Ask about this PR… (Esc to collapse)" aria-label="Ask the chat agent" spellcheck="true">${esc(drafts[id] || "")}</textarea>
        ${busy.has(id) ? `<button type="button" data-chat="stop" title="Stop">Stop</button>` : `<button class="primary" ${waiting ? "disabled" : ""} title="Send (Enter; Shift+Enter for a new line)">Send</button>`}
      </form>`;
    const ta = root.querySelector("#chat-input");
    grow(ta);
    if (focused) { ta.focus({ preventScroll: true }); ta.setSelectionRange(...sel); }
    const nl = root.querySelector(".chat-log");
    const count = msgs.length + (waiting ? 1 : 0);
    nl.scrollTop = atBottom || count !== lastCount || prevTop == null ? nl.scrollHeight : prevTop;
    lastCount = count;
    place();
    drawn = sig();
    if (st.open) st.unread = false;
  }

  const grow = (ta) => { ta.style.height = "auto"; ta.style.height = `${Math.min(ta.scrollHeight, 180)}px`; };
  const sig = () => JSON.stringify([host.key(), cid(), st.open, st.mode, st.popped, whereText(), modelLabel()]);
  const running = () => loadConv(cid()).some((m) => (m.actions || []).some((a) => a.status === "running"));

  // turnOf is a turn as the server takes it. The agent's turns say what
  // it proposed and what became of it, so it doesn't propose it again.
  const turnOf = (m) => ({
    role: m.role, at: m.at,
    content: m.role === "assistant" && m.actions?.length
      ? `${m.content}\n\n[proposed actions: ${m.actions.map((a) => `${a.name} ${JSON.stringify(a.args)} → ${a.status}`).join("; ")}]`
      : m.content,
  });

  // send asks the agent: text is the reader's turn, or "" to have it
  // continue after its actions ran.
  async function send(text) {
    const key = host.key(), id = cid();
    text = text.trim();
    if (!key || busy.has(id) || remoteBusy.has(id)) return;
    let msgs = loadConv(id);
    if (text) msgs = [...msgs, { role: "user", content: text, at: now() }];
    else if (!msgs.length) return;
    saveConv(id, msgs);
    if (text) drafts[id] = "";
    const ctl = new AbortController();
    busy.set(id, ctl);
    post({ type: "busy", key: id, on: true });
    draw();
    const t0 = performance.now();
    let reply;
    try {
      const res = await api(`/api/results/${encodeURIComponent(key)}/chat`, {
        method: "POST", headers: { "Content-Type": "application/json" }, signal: ctl.signal,
        body: JSON.stringify({ messages: msgs.filter((m) => !m.error).map(turnOf), view: host.where() || {}, ...modelPick() }),
      });
      const actions = await Promise.all((res.actions || []).map(async (a, i) => {
        const d = await describe(a.name, a.args).catch(() => ({ label: a.name, risk: "outward" }));
        return { id: `${Date.now().toString(36)}-${i}`, name: a.name, args: a.args || {}, why: a.why, label: d.label, preview: d.preview, risk: d.risk, status: "proposed" };
      }));
      reply = { role: "assistant", content: res.answer, model: res.model, at: res.at || now(), secs: Math.round((performance.now() - t0) / 100) / 10, actions };
    } catch (e) {
      reply = { role: "assistant", error: true, at: now(), content: e.name === "AbortError" ? "Stopped." : `The chat agent failed: ${e.message}` };
    }
    busy.delete(id);
    saveConv(id, [...loadConv(id), reply]);
    if (!st.open || st.popped || cid() !== id) st.unread = !popout;
    post({ type: "busy", key: id, on: false });
    post({ type: "conv", key: id });
    draw();
    // What may run unasked runs now, in order.
    for (const a of reply.actions || []) if (autoRuns(a.risk)) await runAct(id, a.id);
  }

  // findAct finds an action of conversation id.
  const findAct = (msgs, aid) => {
    for (const m of msgs) for (const a of m.actions || []) if (a.id === aid) return { m, a };
    return null;
  };
  // setAct changes an action, saved, and redraws.
  function setAct(id, aid, change) {
    const msgs = loadConv(id), x = findAct(msgs, aid);
    if (!x) return null;
    Object.assign(x.a, change);
    saveConv(id, msgs);
    post({ type: "conv", key: id });
    draw();
    return x;
  }

  // runAct runs a proposed action (or retries a failed one), records what
  // happened as an event, and lets the agent continue once all of its
  // answer's actions are settled.
  async function runAct(id, aid, declined = false) {
    const x = findAct(loadConv(id), aid);
    if (!x || x.a.status === "running" || x.a.status === "done") return;
    if (!declined && x.a.risk === "outward" && x.a.status !== "proposed" && x.a.status !== "failed") return;
    const started = now();
    let status, result;
    if (declined) { status = "declined"; result = "the reader declined it"; }
    else {
      setAct(id, aid, { status: "running", at: started });
      try {
        result = await runAction(x.a.name, x.a.args, (text) => { progress.set(aid, text); draw(); });
        status = "done";
      } catch (e) { status = "failed"; result = e.message; }
      progress.delete(aid);
    }
    const at = now();
    setAct(id, aid, { status, result, at });
    const msgs = loadConv(id);
    msgs.push({ role: "event", act: aid, at, content: `${x.a.name} ${JSON.stringify(x.a.args)} ${status}${declined ? "" : ` (started ${started})`}: ${result}` });
    saveConv(id, msgs);
    post({ type: "conv", key: id });
    draw();
    continueAfter(id, x.m);
  }

  // continueAfter asks the agent to go on once every action of its answer
  // m is settled, when one of them did something (navigation and declines
  // don't need an answer), and it hasn't answered MAX_CHAIN times on its own.
  function continueAfter(id, m) {
    const msgs = loadConv(id);
    const cur = msgs.find((x) => x.at === m.at && x.role === "assistant") || m;
    const acts = cur.actions || [];
    if (acts.some((a) => a.status === "proposed" || a.status === "running")) return;
    if (!acts.some((a) => a.status === "failed" || (a.status === "done" && a.risk !== "view"))) return;
    let chain = 0;
    for (let i = msgs.length - 1; i >= 0 && msgs[i].role !== "user"; i--) if (msgs[i].role === "assistant") chain++;
    if (chain > MAX_CHAIN || cid() !== id) return;
    send("");
  }

  // The agent API lives in the main window; the pop-out asks it.
  const calls = new Map();
  let rid = 0;
  function rpc(op, name, args, onProgress) {
    return new Promise((resolve, reject) => {
      const id = `${Date.now()}-${++rid}`;
      calls.set(id, { resolve, reject, onProgress });
      post({ type: "rpc", rid: id, op, name, args, change: cid() });
      if (op === "describe") setTimeout(() => calls.has(id) && (calls.delete(id), reject(new Error("the main window did not answer"))), 3000);
    });
  }
  const describe = (name, args) => popout ? rpc("describe", name, args) : Promise.resolve(host.describe(name, args));
  const runAction = (name, args, onProgress) => popout ? rpc("run", name, args, onProgress) : host.runAction(name, args, onProgress);

  // A page reload loses track of what was running.
  {
    const id = host.key() && cid();
    const msgs = id ? loadConv(id) : [];
    let stale = false;
    for (const m of msgs) for (const a of m.actions || []) if (a.status === "running" && !popout) { a.status = "interrupted"; stale = true; }
    if (stale) saveConv(id, msgs);
  }

  function popOut() {
    const key = host.key();
    popWin = window.open(`chat.html?key=${encodeURIComponent(key)}&change=${encodeURIComponent(cid())}`, "pr-manager-chat", "popup,width=480,height=820");
    if (!popWin) { say("The browser blocked the chat window. Allow pop-ups for this page, or dock the chat instead."); return; }
    st.popped = true;
    draw();
    const watch = setInterval(() => {
      if (popWin && !popWin.closed) return;
      clearInterval(watch);
      if (st.popped) { st.popped = false; draw(); }
    }, 1000);
  }

  const acts = {
    open: () => { st.open = true; st.unread = false; draw(); root.querySelector("#chat-input")?.focus(); },
    close: () => {
      st.open = false;
      draw();
      root.querySelector(".chat-launch")?.focus({ preventScroll: true });
    },
    mode: () => { st.mode = st.mode === "dock" ? "float" : "dock"; setLs("chat_mode", st.mode); draw(); },
    popout: popOut,
    focus: () => {
      if (popWin && !popWin.closed) { popWin.focus(); post({ type: "focus-input" }); } else acts.open(st.popped = false);
    },
    "dock-back": () => { post({ type: "dock" }); window.close(); },
    new: () => { const id = cid(); busy.get(id)?.abort(); saveConv(id, []); post({ type: "conv", key: id }); draw(); },
    stop: () => busy.get(cid())?.abort(),
    ask: (el) => send(el.textContent),
    retry: () => {
      const id = cid();
      const msgs = loadConv(id);
      while (msgs.length && msgs[msgs.length - 1].role !== "user") msgs.pop();
      const q = msgs.pop();
      saveConv(id, msgs);
      if (q) send(q.content);
    },
    "act-run": (el) => runAct(cid(), el.dataset.a),
    "act-skip": (el) => runAct(cid(), el.dataset.a, true),
    copy: async (el) => {
      const m = loadConv(cid())[+el.dataset.i];
      try { await navigator.clipboard.writeText(m.content); el.textContent = "copied"; } catch { el.textContent = "no clipboard"; }
      setTimeout(() => { el.textContent = "copy"; }, 1500);
    },
  };

  root.addEventListener("click", (e) => {
    if (dragged) { dragged = false; e.preventDefault(); return; } // the end of a drag, not a click
    const u = e.target.closest(".chat-unit");
    if (u) { host.openUnit(u.dataset.unit); return; }
    const el = e.target.closest("[data-chat]");
    if (el && acts[el.dataset.chat]) acts[el.dataset.chat](el);
  });
  root.addEventListener("submit", (e) => {
    if (!e.target.matches("[data-chat-form]")) return;
    e.preventDefault();
    send(root.querySelector("#chat-input").value);
  });
  root.addEventListener("input", (e) => {
    if (e.target.id !== "chat-input") return;
    drafts[cid()] = e.target.value;
    grow(e.target);
  });
  root.addEventListener("keydown", (e) => {
    if (e.target.id === "chat-input" && e.key === "Enter" && !e.shiftKey && !e.isComposing) {
      e.preventDefault();
      send(e.target.value);
    } else if (e.key === "Escape" && !popout) acts.close();
  });

  // Position: the icon and the floating panel share one spot, kept as the
  // distance of their bottom-right corner from the window's (chat_pos), so
  // the panel opens out of the icon. Unset, it is the corner, above the
  // walkthrough's step bar.
  let dragged = false;
  const savedPos = () => { try { return JSON.parse(ls("chat_pos")); } catch { return null; } };
  function place() {
    const floating = !popout && (st.popped || !st.open || st.mode === "float");
    const pos = floating && savedPos();
    root.style.right = pos ? `${pos.r}px` : "";
    root.style.bottom = pos ? `${pos.b}px` : "";
    if (pos) clampInto(pos);
  }
  // clampInto keeps the whole of it in the window (under the header),
  // after a resize of the window or of the panel.
  function clampInto(pos) {
    const r = root.getBoundingClientRect();
    const right = Math.max(0, Math.min(pos.r, innerWidth - r.width));
    const bottom = Math.max(0, Math.min(pos.b, innerHeight - 54 - r.height));
    root.style.right = `${right}px`;
    root.style.bottom = `${bottom}px`;
    return { r: right, b: bottom };
  }
  addEventListener("resize", () => { if (!popout && savedPos()) place(); });
  root.addEventListener("dblclick", (e) => {
    if (!e.target.closest(".chat-head[data-drag]") || e.target.closest("button")) return;
    localStorage.removeItem("pr-manager.chat_pos");
    place();
  });
  root.addEventListener("pointerdown", (e) => {
    const handle = e.target.closest("[data-drag]");
    if (!handle || e.button !== 0 || (handle.matches(".chat-head") && e.target.closest("button"))) return;
    const r = root.getBoundingClientRect();
    const start = { x: e.clientX, y: e.clientY, r: innerWidth - r.right, b: innerHeight - r.bottom };
    let moving = false, pos = null;
    const move = (ev) => {
      const dx = ev.clientX - start.x, dy = ev.clientY - start.y;
      if (!moving && Math.hypot(dx, dy) < 4) return; // a click
      moving = true;
      root.classList.add("dragging");
      pos = clampInto({ r: start.r - dx, b: start.b - dy });
    };
    const up = () => {
      removeEventListener("pointermove", move);
      removeEventListener("pointerup", up);
      root.classList.remove("dragging");
      if (!moving) return;
      dragged = true;
      setTimeout(() => { dragged = false; }); // if no click follows
      setLs("chat_pos", JSON.stringify(pos));
    };
    addEventListener("pointermove", move);
    addEventListener("pointerup", up);
  });

  // Resize: the docked column from its left edge, the floating panel from
  // its top-left corner.
  root.addEventListener("pointerdown", (e) => {
    if (!e.target.closest("[data-grip]")) return;
    e.preventDefault();
    const r = root.getBoundingClientRect();
    const move = (ev) => {
      const w = Math.round(Math.max(300, Math.min(innerWidth * 0.7, r.right - ev.clientX)));
      if (st.mode === "dock") document.documentElement.style.setProperty("--chat-w", `${w}px`);
      else {
        root.style.width = `${w}px`;
        root.style.height = `${Math.round(Math.max(280, Math.min(innerHeight - 80, r.bottom - ev.clientY)))}px`;
      }
    };
    const up = () => {
      removeEventListener("pointermove", move);
      removeEventListener("pointerup", up);
      if (st.mode === "dock") { setLs("chat_w", parseInt(getComputedStyle(document.documentElement).getPropertyValue("--chat-w"))); dispatchEvent(new Event("resize")); }
      else { setLs("chat_fw", parseInt(root.style.width)); setLs("chat_fh", parseInt(root.style.height)); }
    };
    addEventListener("pointermove", move);
    addEventListener("pointerup", up);
  });
  function sizeFloat() {
    root.style.width = st.open && st.mode === "float" && !popout && ls("chat_fw") ? `${ls("chat_fw")}px` : "";
    root.style.height = st.open && st.mode === "float" && !popout && ls("chat_fh") ? `${ls("chat_fh")}px` : "";
  }

  channel?.addEventListener("message", ({ data: m }) => {
    if (m.type === "busy") { m.on ? remoteBusy.add(m.key) : remoteBusy.delete(m.key); if (m.key === cid()) draw(); }
    else if (m.type === "conv") { if (m.key === cid()) draw(); }
    else if (!popout && m.type === "rpc") {
      // The pop-out's action, run here, where the app is.
      const reply = (x) => post({ type: "rpc-done", rid: m.rid, ...x });
      if (m.change !== cid()) { reply({ ok: false, error: "the main window shows another change: open this one there first" }); return; }
      Promise.resolve(m.op === "describe" ? host.describe(m.name, m.args) : host.runAction(m.name, m.args, (text) => post({ type: "rpc-progress", rid: m.rid, text })))
        .then((value) => reply({ ok: true, value }), (e) => reply({ ok: false, error: e.message }));
    }
    else if (popout && m.type === "rpc-progress") calls.get(m.rid)?.onProgress?.(m.text);
    else if (popout && m.type === "rpc-done") {
      const c = calls.get(m.rid);
      if (!c) return;
      calls.delete(m.rid);
      m.ok ? c.resolve(m.value) : c.reject(new Error(m.error));
    }
    else if (popout && m.type === "view") { host.setView(m); document.title = `Ask · ${m.title || "pr-manager"}`; draw(); }
    else if (!popout && m.type === "hello") { st.popped = true; sendView(true); draw(); }
    else if (!popout && m.type === "bye") { st.popped = false; draw(); }
    else if (!popout && m.type === "dock") { st.popped = false; acts.open(); }
    else if (popout && m.type === "focus-input") root.querySelector("#chat-input")?.focus();
    else if (!popout && m.type === "open-unit") host.openUnit(m.id);
  });

  // sendView tells the pop-out what is on screen, with the unit names
  // when the result changed.
  let sentKey = null;
  function sendView(force) {
    if (popout || !st.popped) return;
    const key = host.key();
    const msg = { type: "view", key, change: cid(), title: host.title(), where: host.where() || {} };
    if (force || key !== sentKey) { msg.labels = host.labels?.() || {}; sentKey = key; }
    post(msg);
  }

  // The hotkey opens the chat with the cursor in the prompt, wherever it
  // is: here, or in the pop-out. The collapse hotkey collapses it here.
  document.addEventListener("keydown", (e) => {
    if (!host.key() || document.querySelector("dialog[open]")) return;
    if (!popout && !st.popped && st.open && hideHotkey(e)) { e.preventDefault(); acts.close(); return; }
    if (!hotkey(e)) return;
    e.preventDefault();
    if (popout) root.querySelector("#chat-input")?.focus();
    else if (st.popped) acts.focus();
    else if (!st.open) acts.open();
    else root.querySelector("#chat-input")?.focus();
  });

  // A new hotkey in Settings (here, or in another window) renames it in
  // the hover text.
  addEventListener("prm-hotkey", () => draw());
  addEventListener("storage", (e) => { if (e.key === "pr-manager.chat_hotkey" || e.key === "pr-manager.chat_hide_hotkey") draw(); });

  if (popout) {
    post({ type: "hello" });
    addEventListener("pagehide", () => post({ type: "bye" }));
  }
  draw();

  return {
    // sync follows the page: called after each render of the main window.
    sync() {
      sendView(false);
      applyLayout();
      if (sig() !== drawn || !root.querySelector(".chat-log")) draw();
    },
  };
}
