// The chat agent popped out of the main window (chat.html). It follows the
// result the main window shows, and a click on a unit opens it there.
import { api } from "./util.js";
import { initChat } from "./chat.js";

const q = new URLSearchParams(location.search);
const view = { key: q.get("key"), change: q.get("change") || "", title: "", where: {}, labels: {} };
const cfg = await api("/api/config").catch(() => null);
const channel = new BroadcastChannel("pr-manager-chat");

initChat({
  key: () => view.key,
  title: () => view.title,
  where: () => view.where,
  unitLabel: (id) => view.labels[id] || id,
  openUnit: (id) => channel.postMessage({ type: "open-unit", id }),
  cfg: () => cfg,
  change: () => view.change || view.key,
  setView: (m) => {
    if (m.key !== view.key) view.labels = {};
    Object.assign(view, { key: m.key, change: m.change || "", title: m.title, where: m.where });
    if (m.labels) view.labels = m.labels;
  },
}, { popout: true });
// A theme switched in the main window applies here too.
addEventListener("storage", (e) => {
  if (e.key === "pr-manager.theme" && (e.newValue === "light" || e.newValue === "dark")) document.documentElement.dataset.theme = e.newValue;
});
