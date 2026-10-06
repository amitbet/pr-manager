// snapshot draws the page as the reader sees it, without the chat panel,
// for the chat agent (the snapshot action in agentapi.js), and without
// hiding anything on screen: the page is copied, the chat left out of the
// copy, and the copy drawn with the page's styles through an SVG
// foreignObject onto a canvas, at the window's size and scroll. What a
// foreignObject can't draw (pictures from other sites, a canvas's own
// pixels are copied) is left blank. Where the browser won't let a
// foreignObject leave the canvas (Safari, so the macOS app), there is no
// picture, and the agent gets the page's text instead.

const SKIP = new Set(["SCRIPT", "NOSCRIPT", "TEMPLATE", "IFRAME"]);

// cssText is every rule of the page's stylesheets, with :root, html and
// body made classes of the copy.
function cssText() {
  let css = "";
  for (const ss of document.styleSheets) {
    try { for (const r of ss.cssRules) css += r.cssText + "\n"; } catch { /* another origin's */ }
  }
  const sel = (name, cls) => new RegExp(`(^|[\\s,{}>+~(])${name}(?=[\\s.\\[:#{,>+~)])`, "g");
  return css.replace(/:root\b/g, ".snap-root").replace(sel("html", ""), "$1.snap-root").replace(sel("body", ""), "$1.snap-body");
}

// copyTree copies src's children into dst: form values, a canvas as its
// picture, and each scrolled box's scroll, as a shift of its children.
function copyTree(src, dst) {
  for (const a of src.childNodes) {
    if (a.nodeType === 3) { dst.appendChild(document.createTextNode(a.nodeValue)); continue; }
    if (a.nodeType !== 1 || SKIP.has(a.tagName) || a.id === "chat-root") continue;
    if (a.tagName === "DIALOG" && !a.open) continue;
    let b;
    if (a.tagName === "CANVAS") {
      b = document.createElement("img");
      try { b.src = a.toDataURL(); } catch { /* tainted */ }
      b.width = a.width; b.height = a.height;
      b.style.cssText = a.style.cssText;
      b.className = a.className;
      const r = a.getBoundingClientRect();
      b.style.width = `${r.width}px`; b.style.height = `${r.height}px`;
    } else if (a.namespaceURI === "http://www.w3.org/2000/svg" && a.tagName.toLowerCase() === "svg") {
      b = a.cloneNode(true);
    } else {
      b = a.cloneNode(false);
      if (a.tagName === "TEXTAREA") b.textContent = a.value;
      else copyTree(a, b);
      if (a.tagName === "INPUT") {
        if (a.type === "checkbox" || a.type === "radio") { if (a.checked) b.setAttribute("checked", ""); else b.removeAttribute("checked"); }
        else b.setAttribute("value", a.value);
      }
      if (a.tagName === "SELECT") for (const [i, o] of [...a.options].entries()) if (o.selected) b.children[i]?.setAttribute("selected", "");
      if (a.tagName === "DETAILS" && a.open) b.setAttribute("open", "");
    }
    if (a.scrollTop || a.scrollLeft) {
      b.style.overflow = "hidden";
      for (const c of b.children) c.style.translate = `${-a.scrollLeft}px ${-a.scrollTop}px`;
    }
    dst.appendChild(b);
  }
}

// snapshot is a PNG of the page (a data URL, or "" when the browser won't
// give one) and its text.
export async function snapshot() {
  const W = innerWidth, H = innerHeight;
  const text = (document.getElementById("main")?.innerText || document.body.innerText || "").replace(/\n{3,}/g, "\n\n").slice(0, 8000);
  const html = document.documentElement;
  const root = document.createElement("div");
  for (const at of html.attributes) if (at.name !== "xmlns") root.setAttribute(at.name, at.value);
  root.className = `snap-root ${html.className}`;
  root.style.cssText = `${html.style.cssText};width:${W}px;height:${H}px;overflow:hidden;position:relative`;
  const style = document.createElement("style");
  style.textContent = cssText();
  root.appendChild(style);
  const body = document.createElement("div");
  for (const at of document.body.attributes) body.setAttribute(at.name, at.value);
  body.className = `snap-body ${document.body.className}`;
  const se = document.scrollingElement;
  body.style.cssText = `${document.body.style.cssText};min-height:${H}px;translate:${-(se?.scrollLeft || 0)}px ${-(se?.scrollTop || 0)}px`;
  copyTree(document.body, body);
  root.appendChild(body);
  const xhtml = new XMLSerializer().serializeToString(root);
  const svg = `<svg xmlns="http://www.w3.org/2000/svg" width="${W}" height="${H}"><foreignObject x="0" y="0" width="100%" height="100%">${xhtml}</foreignObject></svg>`;
  try {
    const img = new Image();
    img.src = `data:image/svg+xml;charset=utf-8,${encodeURIComponent(svg)}`;
    await img.decode();
    const scale = Math.min(devicePixelRatio || 1, 2, 2400 / W);
    const c = document.createElement("canvas");
    c.width = Math.round(W * scale); c.height = Math.round(H * scale);
    const g = c.getContext("2d");
    g.scale(scale, scale);
    g.fillStyle = getComputedStyle(document.body).backgroundColor || "#fff";
    g.fillRect(0, 0, W, H);
    g.drawImage(img, 0, 0, W, H);
    return { png: c.toDataURL("image/png"), text, width: W, height: H };
  } catch {
    return { png: "", text, width: W, height: H };
  }
}
