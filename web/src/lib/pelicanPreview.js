export const PELICAN_HTML_LIMIT = 2 * 1024 * 1024;
const MAX_DIMENSION = 8192;
const PREVIEW_CSP = "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; media-src data: blob:; font-src data:; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'";

export function pelicanOutput(record) {
  if (typeof record?.html === "string" && record.html.trim()) return record.html.trim();
  return typeof record?.svg === "string" ? record.svg.trim() : "";
}

export function isPelicanSVG(output) {
  return typeof output === "string" && /^<svg(?:\s|>)/i.test(output.replace(/<!--[\s\S]*?-->/g, "").trim());
}

export function pelicanDownload(output, format = "html") {
  const svg = isPelicanSVG(output);
  if (format === "svg") {
    if (!svg) throw new Error("当前作品不是独立 SVG 文件");
    return { content: output, type: "image/svg+xml;charset=utf-8", filename: "pelican-bicycle.svg" };
  }
  return { content: svg ? `<!doctype html><html><head><meta charset="utf-8"><title>鹈鹕骑自行车</title></head><body>${output}</body></html>` : output, type: "text/html;charset=utf-8", filename: "pelican-bicycle.html" };
}

export function validPelicanOutput(record) {
  const output = pelicanOutput(record);
  const limit = record.html ? PELICAN_HTML_LIMIT : 262144;
  if (!output || output.length > limit) return false;
  let bytes = 0;
  for (const character of output) { const point = character.codePointAt(0); bytes += point < 128 ? 1 : point < 2048 ? 2 : point < 65536 ? 3 : 4; if (bytes > limit) return false; }
  const boundary = output.replace(/<!--[\s\S]*?-->/g, "").trim();
  return /^(?:<!doctype\s+html\s*>\s*)?<html(?:\s|>)/i.test(boundary) && /<\/html\s*>$/i.test(boundary)
    || /^<svg(?:\s|>)/i.test(boundary) && /(?:<\/svg\s*>|\/>)$/i.test(boundary);
}

function dimension(value) { return Number.isFinite(value) && value > 0 && value <= MAX_DIMENSION; }

// Parse off-page. Authored code only executes after it enters the opaque iframe.
export function preparePelicanPreview(output) {
  const bareSVG = isPelicanSVG(output);
  const markup = bareSVG ? `<html><head><style>html,body{margin:0;height:100%}body{display:flex;align-items:center;justify-content:center}svg{max-width:100%;max-height:100%}</style></head><body>${output}</body></html>` : output;
  const policy = `<meta http-equiv="Content-Security-Policy" content="${PREVIEW_CSP}">`;
  const doc = new DOMParser().parseFromString(`<!doctype html>${policy}${markup}`, "text/html");
  doc.querySelectorAll('base, meta[http-equiv="refresh" i]').forEach(element => element.remove());
  const children = [...doc.body.children].filter(element => !["SCRIPT", "STYLE", "META", "LINK"].includes(element.tagName));
  const svg = children.length === 1 && children[0].tagName.toLowerCase() === "svg" ? children[0] : null;
  let width = 1024, height = 768;
  if (svg && ![...doc.body.childNodes].some(node => node.nodeType === 3 && node.textContent.trim())) {
    const pixels = name => /^\s*\d+(?:\.\d+)?(?:px)?\s*$/i.test(svg.getAttribute(name) || "") ? parseFloat(svg.getAttribute(name)) : NaN;
    const box = (svg.getAttribute("viewBox") || "").trim().split(/[\s,]+/).map(Number);
    let w = pixels("width"), h = pixels("height");
    if (box.length === 4 && dimension(box[2]) && dimension(box[3])) {
      if (dimension(w) && !dimension(h)) h = w * box[3] / box[2];
      else if (dimension(h) && !dimension(w)) w = h * box[2] / box[3];
      else if (!dimension(w) && !dimension(h)) [w, h] = box.slice(2);
    }
    if (dimension(w) && dimension(h)) [width, height] = [w, h];
  }
  const channel = [...crypto.getRandomValues(new Uint8Array(16))].map(byte => byte.toString(16).padStart(2, "0")).join("");
  const viewport = { width, height };
  // The policy precedes all model markup, including any authored scripts.
  const measurement = `<script>(${measurePelicanDocument.toString()})(window,${JSON.stringify(channel)},${JSON.stringify(viewport)},${MAX_DIMENSION});<\/script>`;
  return { viewport, channel, srcDoc: `<!doctype html>${policy}${measurement}${doc.documentElement.outerHTML}` };
}

export function pelicanSizeMessage(event, source, channel) {
  const data = event.data;
  if (!source || event.source !== source || !data || data.type !== "pelican:size" || data.channel !== channel || !dimension(data.width) || !dimension(data.height)) return null;
  return { width: Math.ceil(data.width), height: Math.ceil(data.height) };
}

// Serialized into srcdoc; it has no access to the application's DOM or state.
function measurePelicanDocument(target, channel, viewport, limit) {
  const doc = target.document;
  let scheduled = 0, updates = 0, stopped = false, scale = 1, offsetX = 0, offsetY = 0;
  let width = viewport.width, height = viewport.height, authorZoom = 1;
  let authorTranslate = ["0px", "0px"];
  let resize, mutation;
  const observed = new WeakSet(), timers = [];
  function schedule() { if (!stopped && !scheduled && updates < 120) scheduled = target.setTimeout(measure, 100); }
  // Zoom changes the CSS viewport used by viewport units. Resolve those units
  // against the native iframe once, including declarations introduced later.
  // Leave quoted strings, comments and URL payloads byte-for-byte intact.
  function fixedViewportUnits(value) {
    if (!/[sld]?v(?:w|h|min|max)\b/i.test(value)) return value;
    let result = "", index = 0;
    while (index < value.length) {
      const remaining = value.slice(index);
      const protectedToken = remaining.match(/^(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|\/\*[\s\S]*?\*\/)/);
      if (protectedToken) { result += protectedToken[0]; index += protectedToken[0].length; continue; }
      const urlStart = remaining.match(/^url\s*\(/i);
      if (urlStart) {
        let end = index + urlStart[0].length, nesting = 1, quote = "";
        while (end < value.length && nesting) {
          const char = value[end++];
          if (char === "\\") { end = Math.min(value.length, end + 1); continue; }
          if (quote) { if (char === quote) quote = ""; continue; }
          if (char === "'" || char === '"') quote = char;
          else if (char === "(") nesting++;
          else if (char === ")") nesting--;
        }
        result += value.slice(index, end); index = end; continue;
      }
      const numeric = !index || !/[\w.\\-]/.test(value[index - 1])
        ? remaining.match(/^([-+]?(?:\d+(?:\.\d*)?|\.\d+))([sld]?v(?:w|h|min|max))\b/i) : null;
      if (!numeric) { result += value[index++]; continue; }
      const unit = numeric[2].toLowerCase();
      const basis = unit.endsWith("min") ? Math.min(viewport.width, viewport.height)
        : unit.endsWith("max") ? Math.max(viewport.width, viewport.height)
          : unit.endsWith("w") ? viewport.width : viewport.height;
      result += `${Number(numeric[1]) * basis / 100}px`;
      index += numeric[0].length;
    }
    return result;
  }
  function pinStyle(style) {
    if (!style) return;
    for (const property of [...style]) {
      const original = style.getPropertyValue(property), pinned = fixedViewportUnits(original);
      if (pinned !== original) style.setProperty(property, pinned, style.getPropertyPriority(property));
    }
  }
  function pinSheet(rules, budget) {
    for (const rule of rules) {
      if (--budget.left < 0) return;
      pinStyle(rule.style);
      if (rule.cssRules) pinSheet(rule.cssRules, budget);
    }
  }
  function prepareLayout() {
    const budget = { left: 10000 };
    for (const sheet of [...doc.styleSheets]) {
      try { pinSheet(sheet.cssRules, budget); } catch { /* A blocked sheet has no readable rules. */ }
      if (budget.left < 0) break;
    }
    const elements = [...doc.querySelectorAll("html,body,body *")].slice(0, 2000);
    for (const element of elements) {
      pinStyle(element.style);
      if (resize && !observed.has(element)) { observed.add(element); resize.observe(element); }
    }
    return elements;
  }
  function translateParts(value) {
    if (!value || value === "none") return ["0px", "0px"];
    const parts = []; let begin = 0, depth = 0;
    for (let i = 0; i < value.length; i++) {
      if (value[i] === "(") depth++;
      else if (value[i] === ")") depth--;
      else if (/\s/.test(value[i]) && !depth) { if (i > begin) parts.push(value.slice(begin, i)); begin = i + 1; }
    }
    if (begin < value.length) parts.push(value.slice(begin));
    return [parts[0] || "0px", parts[1] || "0px", ...parts.slice(2)];
  }
  function measure() {
    scheduled = 0;
    if (stopped || updates >= 120) return;
    const elements = prepareLayout();
    let left = 0, top = 0, right = Math.max(viewport.width, doc.body.scrollWidth * authorZoom), bottom = Math.max(viewport.height, doc.body.scrollHeight * authorZoom);
    for (const element of elements) {
      if (["SCRIPT", "STYLE", "META", "LINK"].includes(element.tagName)) continue;
      const svg = element.closest("svg");
      if (svg && svg !== element) continue;
      const rect = element.getBoundingClientRect();
      left = Math.min(left, (rect.left + target.scrollX) / scale - offsetX);
      top = Math.min(top, (rect.top + target.scrollY) / scale - offsetY);
      right = Math.max(right, (rect.right + target.scrollX) / scale - offsetX);
      bottom = Math.max(bottom, (rect.bottom + target.scrollY) / scale - offsetY);
    }
    const nextX = Math.min(limit, Math.max(offsetX, Math.ceil(-left)));
    const nextY = Math.min(limit, Math.max(offsetY, Math.ceil(-top)));
    const nextWidth = Math.min(limit, Math.max(width, Math.ceil(right + nextX)));
    const nextHeight = Math.min(limit, Math.max(height, Math.ceil(bottom + nextY)));
    if (updates && nextWidth === width && nextHeight === height && nextX === offsetX && nextY === offsetY) return;
    [width, height, offsetX, offsetY] = [nextWidth, nextHeight, nextX, nextY];
    const root = doc.documentElement;
    root.style.setProperty("translate", `calc(${authorTranslate[0]} + ${offsetX / authorZoom}px) calc(${authorTranslate[1]} + ${offsetY / authorZoom}px)${authorTranslate[2] ? ` ${authorTranslate[2]}` : ""}`, "important");
    scale = Math.min(1, viewport.width / width, viewport.height / height);
    root.style.setProperty("zoom", String(authorZoom * scale), "important");
    updates++;
    target.parent.postMessage({ type: "pelican:size", channel, width, height }, "*");
  }
  function start() {
    if (stopped || !doc.body) return;
    prepareLayout();
    const root = doc.documentElement, authored = target.getComputedStyle(root), zoom = parseFloat(authored.zoom);
    if (Number.isFinite(zoom) && zoom > 0) authorZoom = authored.zoom.endsWith("%") ? zoom / 100 : zoom;
    authorTranslate = translateParts(authored.translate);
    const pixels = (value, fallback) => /^\d+(?:\.\d+)?px$/.test(value) && parseFloat(value) > 0 ? Math.min(limit, parseFloat(value)) : fallback;
    // Preserve the author's percentage basis while only the outer paint shrinks.
    const rootWidth = pixels(authored.width, viewport.width), rootHeight = pixels(authored.height, viewport.height);
    root.style.setProperty("width", `${rootWidth}px`, "important");
    root.style.setProperty("height", `${rootHeight}px`, "important");
    if (target.ResizeObserver) resize = new target.ResizeObserver(schedule);
    if (target.MutationObserver) { mutation = new target.MutationObserver(schedule); mutation.observe(root, { subtree: true, childList: true, attributes: true, characterData: true }); }
    doc.addEventListener("load", schedule, true);
    measure();
    for (const delay of [100, 500, 1500, 5000]) timers.push(target.setTimeout(schedule, delay));
    doc.fonts?.ready.then(schedule).catch(() => {});
  }
  target.addEventListener("pagehide", () => { stopped = true; target.clearTimeout(scheduled); timers.forEach(timer => target.clearTimeout(timer)); resize?.disconnect(); mutation?.disconnect(); doc.removeEventListener("load", schedule, true); }, { once: true });
  if (doc.readyState === "loading") doc.addEventListener("DOMContentLoaded", start, { once: true }); else start();
}
