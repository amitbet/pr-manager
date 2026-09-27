// Color theme. Follows the system until the header button picks light or
// dark; the choice is saved as pr-manager.theme. A classic script after
// persist.js, so the saved choice is in localStorage and data-theme is set
// on <html> before the first paint. CSS and the treemap read data-theme;
// a "themechange" event on window tells the treemap to redraw.
(() => {
  const KEY = "pr-manager.theme";
  const root = document.documentElement;
  const mq = matchMedia("(prefers-color-scheme: dark)");
  function apply() {
    const saved = localStorage.getItem(KEY);
    const t = saved === "light" || saved === "dark" ? saved : mq.matches ? "dark" : "light";
    if (root.dataset.theme === t) return;
    root.dataset.theme = t;
    dispatchEvent(new Event("themechange"));
  }
  apply();
  mq.addEventListener("change", apply);
  window.toggleTheme = () => {
    localStorage.setItem(KEY, root.dataset.theme === "dark" ? "light" : "dark");
    apply();
  };
})();
