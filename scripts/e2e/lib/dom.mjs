// A minimal DOM used by the E2E panel harness.
//
// Why hand-rolled (and not jsdom): the repo has no JS test toolchain and no Node
// dependency policy ("no Node build chain" is an architecture decision). Keeping the
// E2E scripts dependency-free means `node scripts/e2e/<suite>.mjs` runs on any machine
// with Node — no install step, no network.
//
// What it must do (and all it does): parse the HTML strings the panel itself generates
// (`innerHTML = ...`), let the suite dispatch real events to real listeners, and expose
// the query helpers app.js uses (`getElementById` / `querySelector(All)` / `closest`).
// It is NOT a browser: layout, CSS, and paint are out of scope.

const VOID_TAGS = new Set(["input", "br", "img", "hr", "meta", "link", "source", "area", "col", "embed"]);

const ENTITIES = { amp: "&", lt: "<", gt: ">", quot: '"', apos: "'", nbsp: "\u00a0", "#39": "'" };

function decodeEntities(s) {
  if (!s.includes("&")) return s;
  return s.replace(/&(#x?[0-9a-fA-F]+|[a-zA-Z]+);/g, (m, name) => {
    if (name.startsWith("#")) {
      const code = name[1] === "x" || name[1] === "X" ? parseInt(name.slice(2), 16) : parseInt(name.slice(1), 10);
      return Number.isFinite(code) ? String.fromCodePoint(code) : m;
    }
    return ENTITIES[name] !== undefined ? ENTITIES[name] : m;
  });
}

function escapeText(s) {
  return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

function escapeAttr(s) {
  return escapeText(s).replace(/"/g, "&quot;");
}

function camelToData(name) {
  return "data-" + name.replace(/[A-Z]/g, (c) => "-" + c.toLowerCase());
}

/** Text node: only `textContent` and `parentNode` are meaningful. */
function mkText(data) {
  return {
    tagName: "#TEXT",
    data,
    parentNode: null,
    get textContent() { return this.data; },
    set textContent(v) { this.data = String(v); },
    innerHTML: "",
    outerHTML: "",
  };
}

/** Parses an HTML string into an array of nodes (element/text), with parent links. */
export function parseHTML(html, doc) {
  const out = [];
  const stack = [];
  const src = String(html == null ? "" : html);
  let i = 0;
  const push = (node) => {
    const parent = stack[stack.length - 1];
    if (parent) { node.parentNode = parent; parent.children.push(node); }
    else out.push(node);
  };
  while (i < src.length) {
    const lt = src.indexOf("<", i);
    if (lt < 0) {
      const text = src.slice(i);
      if (text) push(mkText(decodeEntities(text)));
      break;
    }
    if (lt > i) {
      const text = src.slice(i, lt);
      push(mkText(decodeEntities(text)));
    }
    // Comments are dropped.
    if (src.startsWith("<!--", lt)) {
      const end = src.indexOf("-->", lt + 4);
      i = end < 0 ? src.length : end + 3;
      continue;
    }
    const gt = findTagEnd(src, lt);
    const rawTag = src.slice(lt + 1, gt);
    i = gt + 1;
    if (rawTag.startsWith("/")) { stack.pop(); continue; }
    const selfClose = rawTag.endsWith("/");
    const m = /^([a-zA-Z][\w:-]*)([\s\S]*)$/.exec(selfClose ? rawTag.slice(0, -1) : rawTag);
    if (!m) continue;
    const el = new Element(m[1], doc);
    for (const a of parseAttrs(m[2])) el.setAttribute(a.name, a.value);
    push(el);
    if (!selfClose && !VOID_TAGS.has(m[1].toLowerCase())) stack.push(el);
  }
  return out;
}

function findTagEnd(src, from) {
  let quote = "";
  for (let i = from + 1; i < src.length; i++) {
    const c = src[i];
    if (quote) { if (c === quote) quote = ""; continue; }
    if (c === '"' || c === "'") { quote = c; continue; }
    if (c === ">") return i;
  }
  return src.length;
}

function parseAttrs(s) {
  const out = [];
  const re = /([^\s"'>/=]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>`]+)))?/g;
  let m;
  while ((m = re.exec(s))) {
    const value = m[2] !== undefined ? m[2] : m[3] !== undefined ? m[3] : m[4] !== undefined ? m[4] : "";
    out.push({ name: m[1], value: decodeEntities(value) });
  }
  return out;
}

/** Event object: enough of the DOM interface for the handlers app.js registers. */
export class Event {
  constructor(type, opts) {
    this.type = type;
    this.bubbles = Boolean(opts && opts.bubbles);
    this.defaultPrevented = false;
    this.propagationStopped = false;
    this.target = null;
    this.currentTarget = null;
    this.clientX = 0;
    this.clientY = 0;
    this.dataTransfer = null;
    // Extra payload (`key`, `target`, ...) is copied through, the way an EventInit would.
    if (opts) {
      for (const [k, v] of Object.entries(opts)) {
        if (k === "bubbles") continue;
        this[k] = v;
      }
    }
  }
  preventDefault() { this.defaultPrevented = true; }
  stopPropagation() { this.propagationStopped = true; }
}

export class Element {
  constructor(tagName, doc) {
    this.ownerDocument = doc;
    this.tagName = String(tagName).toUpperCase();
    this.attrs = new Map();
    this.children = [];
    this.parentNode = null;
    this.listeners = new Map();
    this.style = {};
    this.value = "";
    this.checked = false;
    this.disabled = false;
    this.title = "";
    this.placeholder = "";
    this.scrollTop = 0;
    this.scrollHeight = 100;
    this.clientHeight = 50;
    this.offsetWidth = 0;
    this._html = null;
    this._text = null;

    const self = this;
    this.classList = {
      add: (...cs) => { const set = self._classSet(); cs.forEach((c) => set.add(c)); self._writeClass(set); },
      remove: (...cs) => { const set = self._classSet(); cs.forEach((c) => set.delete(c)); self._writeClass(set); return set; },
      contains: (c) => self._classSet().has(c),
      toggle: (c, force) => {
        const set = self._classSet();
        const on = force === undefined ? !set.has(c) : Boolean(force);
        if (on) set.add(c); else set.delete(c);
        self._writeClass(set);
        return on;
      },
    };
    this.dataset = new Proxy({}, {
      get: (_, prop) => {
        if (typeof prop !== "string") return undefined;
        return self.attrs.get(camelToData(prop)) || "";
      },
      set: (_, prop, v) => { self.attrs.set(camelToData(prop), String(v)); return true; },
      deleteProperty: (_, prop) => { self.attrs.delete(camelToData(prop)); return true; },
      has: (_, prop) => typeof prop === "string" && self.attrs.has(camelToData(prop)),
      ownKeys: () => [...self.attrs.keys()].filter((k) => k.startsWith("data-")),
      getOwnPropertyDescriptor: () => ({ enumerable: true, configurable: true }),
    });
    // `value` of inputs inside generated markup comes from the attribute.
    this._value = null;
    Object.defineProperty(this, "value", {
      get() { return self._value !== null ? self._value : (self.attrs.get("value") || ""); },
      set(v) { self._value = String(v); },
      configurable: true,
    });
    // `textContent` writes replace children (browser semantics).
    Object.defineProperty(this, "textContent", {
      get() {
        if (self._text !== null) return self._text;
        if (self._html !== null) return stripTags(self._html);
        return self.children.map((c) => c.textContent).join("");
      },
      set(v) { self._text = String(v); self._html = null; for (const old of self.children) old.parentNode = null; self.children = []; },
      configurable: true,
    });
    Object.defineProperty(this, "className", {
      get() { return self.attrs.get("class") || ""; },
      set(v) { self.attrs.set("class", String(v)); },
      configurable: true,
    });
    Object.defineProperty(this, "innerHTML", {
      get() {
        if (self._html !== null) return self._html;
        if (self._text !== null) return escapeText(self._text);
        return self.children.map((c) => serialize(c)).join("");
      },
      set(v) {
        self._html = String(v == null ? "" : v);
        self._text = null;
        const parsed = parseHTML(self._html, self.ownerDocument);
        // Detach the replaced children so `isConnected` reports stale nodes as detached
        // (app.js relies on it: the picker rebuilds the rule list and then bails out).
        for (const old of self.children) old.parentNode = null;
        self.children = [];
        for (const n of parsed) { n.parentNode = self; self.children.push(n); }
      },
      configurable: true,
    });
  }

  _classSet() {
    return new Set((this.attrs.get("class") || "").split(/\s+/).filter(Boolean));
  }
  _writeClass(set) { this.attrs.set("class", [...set].join(" ")); }

  getAttribute(name) { const v = this.attrs.get(name); return v === undefined ? null : v; }
  setAttribute(name, value) { this.attrs.set(name, String(value)); }

  get isConnected() {
    let n = this;
    while (n.parentNode) n = n.parentNode;
    return Boolean(this.ownerDocument && this.ownerDocument._roots.includes(n));
  }

  appendChild(child) {
    if (child.parentNode) child.parentNode.removeChild(child);
    child.parentNode = this;
    this.children.push(child);
    if (this._html !== null) this._html = null;
    return child;
  }
  removeChild(child) {
    const i = this.children.indexOf(child);
    if (i >= 0) this.children.splice(i, 1);
    child.parentNode = null;
    if (this._html !== null) this._html = null;
    return child;
  }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }

  getBoundingClientRect() {
    return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 };
  }
  scrollIntoView() {}

  focus() {
    if (this.ownerDocument) this.ownerDocument.activeElement = this;
    this.dispatchEvent(new Event("focus"));
  }
  blur() {
    if (this.ownerDocument && this.ownerDocument.activeElement === this) this.ownerDocument.activeElement = null;
    this.dispatchEvent(new Event("blur"));
  }
  select() {}

  addEventListener(type, fn) {
    const list = this.listeners.get(type) || [];
    list.push(fn);
    this.listeners.set(type, list);
  }
  removeEventListener(type, fn) {
    const list = this.listeners.get(type) || [];
    this.listeners.set(type, list.filter((x) => x !== fn));
  }

  dispatchEvent(ev) {
    if (!ev.target) ev.target = this;
    let node = this;
    while (node) {
      ev.currentTarget = node;
      const handler = node["on" + ev.type];
      if (typeof handler === "function") handler.call(node, ev);
      for (const fn of node.listeners.get(ev.type) || []) fn.call(node, ev);
      if (!ev.bubbles || ev.propagationStopped) break;
      node = node.parentNode;
    }
    return !ev.defaultPrevented;
  }

  /** Returns this element and its ancestors that match `selector`. */
  closest(selector) {
    let node = this;
    while (node) {
      if (node.matches && node.matches(selector)) return node;
      node = node.parentNode;
    }
    return null;
  }

  /** Compound-only selector support: `tag`, `.cls`, `#id`, `[attr]`, `[attr="v"]` (comma list ok). */
  matches(selector) {
    return splitSelector(selector).some((part) => {
      if (!part) return false;
      const tokens = part.match(/^([a-zA-Z][\w:-]*)|(\.[\w-]+)|(#[\w-]+)|(\[[^\]]+\])/g);
      if (!tokens) return false;
      const consumed = tokens.join("");
      if (consumed.length !== part.length) return false; // unsupported (descendant/`>`/`:hover`)
      return tokens.every((tok) => {
        if (tok.startsWith(".")) return this._classSet().has(tok.slice(1));
        if (tok.startsWith("#")) return this.attrs.get("id") === tok.slice(1) || this.id === tok.slice(1);
        if (tok.startsWith("[")) {
          const m = /^\[([^=\]]+)(?:=(?:"([^"]*)"|'([^']*)'|([^\]]*)))?\]$/.exec(tok);
          if (!m) return false;
          const v = this.attrs.get(m[1]);
          if (m[2] === undefined && m[3] === undefined && m[4] === undefined) return v !== undefined;
          const want = m[2] !== undefined ? m[2] : m[3] !== undefined ? m[3] : m[4];
          return v === want;
        }
        return this.tagName === tok.toUpperCase();
      });
    });
  }

  querySelectorAll(selector) {
    const out = [];
    const visit = (node) => {
      for (const child of node.children) {
        if (child.tagName === "#TEXT") continue;
        if (child.matches(selector)) out.push(child);
        visit(child);
      }
    };
    visit(this);
    return out;
  }
  querySelector(selector) {
    return this.querySelectorAll(selector)[0] || null;
  }
}

function splitSelector(selector) {
  const parts = [];
  let depth = 0;
  let quote = "";
  let cur = "";
  for (const c of String(selector || "")) {
    if (quote) { cur += c; if (c === quote) quote = ""; continue; }
    if (c === '"' || c === "'") { quote = c; cur += c; continue; }
    if (c === "[") depth++;
    if (c === "]") depth--;
    if (c === "," && depth === 0) { parts.push(cur.trim()); cur = ""; continue; }
    cur += c;
  }
  if (cur.trim()) parts.push(cur.trim());
  return parts;
}

function stripTags(html) {
  return decodeEntities(String(html).replace(/<[^>]*>/g, ""));
}

function serialize(node) {
  if (node.tagName === "#TEXT") return escapeText(node.data);
  const attrs = [...node.attrs.entries()].map(([k, v]) => ` ${k}="${escapeAttr(v)}"`).join("");
  if (VOID_TAGS.has(node.tagName.toLowerCase())) return `<${node.tagName.toLowerCase()}${attrs}>`;
  return `<${node.tagName.toLowerCase()}${attrs}>${node.children.map(serialize).join("")}</${node.tagName.toLowerCase()}>`;
}

/**
 * Creates a document stub. Elements are created lazily per id by `getElementById`
 * (the panel binds hundreds of static ids at load), and any element whose `innerHTML`
 * is assigned becomes searchable through `document.querySelector(All)`.
 */
export function createDocument() {
  const byId = new Map();
  const doc = {
    _roots: [],
    _byId: byId,
    activeElement: null,
    listeners: new Map(),
    documentElement: null,
    body: null,
    getElementById(id) {
      const found = byId.get(id);
      if (found) return found;
      for (const root of doc._roots) {
        const hit = root.matches && root.matches(`#${id}`) ? root : root.querySelector(`#${id}`);
        if (hit) { byId.set(id, hit); return hit; }
      }
      const el = new Element("div", doc);
      el.attrs.set("id", id);
      byId.set(id, el);
      doc._roots.push(el);
      return el;
    },
    querySelectorAll(selector) {
      const out = [];
      for (const root of doc._roots) {
        if (root.matches(selector)) out.push(root);
        out.push(...root.querySelectorAll(selector));
      }
      return out;
    },
    querySelector(selector) {
      return doc.querySelectorAll(selector)[0] || null;
    },
    createElement(tag) { return new Element(tag, doc); },
    createTextNode(text) { return mkText(String(text)); },
    addEventListener(type, fn) {
      const list = doc.listeners.get(type) || [];
      list.push(fn);
      doc.listeners.set(type, list);
    },
    removeEventListener(type, fn) {
      doc.listeners.set(type, (doc.listeners.get(type) || []).filter((x) => x !== fn));
    },
    dispatchEvent(ev) {
      for (const fn of doc.listeners.get(ev.type) || []) fn.call(doc, ev);
      return !ev.defaultPrevented;
    },
    execCommand() { return true; },
    /**
     * Loads real static markup (index.html) into the stub body and indexes ids, so that
     * `getElementById` / `querySelector` resolve the same nodes the browser would.
     */
    attachHTML(html) {
      const m = /<body[^>]*>([\s\S]*?)<\/body>/i.exec(String(html));
      const inner = m ? m[1] : String(html);
      doc.body.innerHTML = inner;
      indexIds(doc.body);
    },
  };
  doc.documentElement = doc.getElementById("__html");
  doc.body = doc.getElementById("__body");
  return doc;
}

/** Registers every `[id]` in the subtree so later lookups are O(1). */
function indexIds(root) {
  const doc = root.ownerDocument;
  const visit = (node) => {
    const id = node.attrs && node.attrs.get("id");
    if (id) doc._byId.set(id, node);
    for (const child of (node.children || [])) visit(child);
  };
  visit(root);
}

/** Fires `type` on `target`, bubbling to ancestors (browser-like). */
export function fire(target, type, extra) {
  const ev = new Event(type, { bubbles: true, ...(extra || {}) });
  target.dispatchEvent(ev);
  return ev;
}