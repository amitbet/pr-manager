// Saved settings. The browser keeps localStorage per origin, so a server
// on another port, or another browser, would start from defaults. The
// server keeps the pr-manager.* keys in a file: this classic script runs
// before the modules, puts the file's values (window.SAVED_SETTINGS, from
// api/settings.js) into localStorage and sends every change back.
(() => {
  const PREFIX = "pr-manager.";
  const saved = window.SAVED_SETTINGS;
  if (!saved) return; // the server has no settings file support
  const proto = Storage.prototype;
  const setItem = proto.setItem, removeItem = proto.removeItem;
  const pending = {};
  let timer = 0;

  function flush() {
    timer = 0;
    const body = JSON.stringify(pending);
    for (const k of Object.keys(pending)) delete pending[k];
    fetch("api/settings", { method: "POST", headers: { "Content-Type": "application/json" }, body, keepalive: true }).catch(() => {});
  }
  function send(k, v) {
    pending[k] = v;
    clearTimeout(timer);
    timer = setTimeout(flush, 300);
  }

  // Values only this browser has, saved before the file existed, are
  // sent once; the file wins for the rest.
  for (const k of Object.keys(localStorage)) {
    if (k.startsWith(PREFIX) && !(k in saved)) send(k, localStorage.getItem(k));
  }
  for (const [k, v] of Object.entries(saved)) setItem.call(localStorage, k, v);

  proto.setItem = function (k, v) {
    setItem.call(this, k, v);
    if (this === localStorage && String(k).startsWith(PREFIX)) send(String(k), String(v));
  };
  proto.removeItem = function (k) {
    removeItem.call(this, k);
    if (this === localStorage && String(k).startsWith(PREFIX)) send(String(k), null);
  };
  addEventListener("pagehide", () => { if (timer) { clearTimeout(timer); flush(); } });
})();
