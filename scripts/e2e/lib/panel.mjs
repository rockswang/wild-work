// Panel harness: runs the real `cmd/wild-work/web/app.js` against a real daemon.
//
// The panel script is NOT stubbed or re-implemented — the file is loaded verbatim and
// driven through the same listeners the browser would fire (focus / input / click /
// keydown), with `fetch` proxying to a live daemon. So a gesture in a scenario goes
// through the shipping code path: handler → app.js state → HTTP → Go backend.
//
// What is faked and why: the DOM. The repo has no browser toolchain (see lib/dom.mjs),
// and nothing in this feature depends on layout/paint — it depends on event wiring and
// on which model ids survive into `fbRules` / the saved payload.
import { readFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { createDocument, Event, fire } from "./dom.mjs";

const HERE = dirname(fileURLToPath(import.meta.url));
export const REPO_ROOT = resolve(HERE, "../../..");
export const WEB_DIR = join(REPO_ROOT, "cmd", "wild-work", "web");

export function readPanelHtml() {
  return readFileSync(join(WEB_DIR, "index.html"), "utf8");
}
export function readPanelScript() {
  return readFileSync(join(WEB_DIR, "app.js"), "utf8");
}

const EXPORTS = `
; globalThis.__T = {
  openSettings, closeSettings, renderFbList, saveModelFallback, loadState, loadFees,
  fbSanitize, fbFromSet, fbAppendTo, fbSelfLoop,
  __setFbRules: (rs) => { fbRules = rs; },
  __getFbRules: () => fbRules,
  __getState: () => state,
};`;

/**
 * Loads the panel the way index.html does (`<script src="./app.js">`), waits for its
 * `init()` to finish, and returns handles that speak in user gestures.
 */
export async function openPanel({ baseURL }) {
  const doc = createDocument();
  const localStorage = {
    _m: new Map(),
    getItem(k) { return this._m.has(k) ? this._m.get(k) : null; },
    setItem(k, v) { this._m.set(k, String(v)); },
    removeItem(k) { this._m.delete(k); },
  };
  const reloads = [];
  const window = {
    addEventListener() {},
    open() {},
    location: { search: "", reload: () => reloads.push(Date.now()) },
  };
  const location = window.location;
  const navigator = { userAgent: "wild-work-e2e", clipboard: { writeText: async () => {} } };
  // The panel's own console output is captured: a killed daemon can make an in-flight
  // poll fail, and that must not look like a suite failure on stdout. It is printed
  // only when a story actually fails, where it is valuable debugging context.
  const consoleLines = [];
  const consoleShim = {
    log: (...a) => consoleLines.push(a.map(fmtArg).join(" ")),
    warn: (...a) => consoleLines.push(a.map(fmtArg).join(" ")),
    error: (...a) => consoleLines.push(a.map(fmtArg).join(" ")),
    info: (...a) => consoleLines.push(a.map(fmtArg).join(" ")),
  };
  const fetchProxy = (path, opts) => {
    const url = String(path).startsWith("http") ? String(path) : baseURL + path;
    return fetch(url, opts);
  };
  // Neutralise the panel's polling timers: scenarios drive refreshes explicitly, and a
  // stray 15s/30s poll would mutate state mid-assertion.
  const noTimer = () => 0;

  const source = readPanelScript()
    .replace("(async function init() {", "globalThis.__e2eInit = (async function init() {")
    + EXPORTS;

  // Markup first, script second — `<script src="./app.js">` sits at the end of body in
  // index.html, so the script always sees the full static DOM. The panel does query it at
  // load time (`$("apiAddr").querySelector(".val")`), so fiducial order matters here.
  doc.attachHTML(readPanelHtml());

  const factory = new Function(
    "document", "window", "localStorage", "fetch", "location", "navigator", "Event",
    "setInterval", "clearInterval", "confirm", "alert", "prompt", "requestAnimationFrame",
    "console",
    source,
  );
  factory(doc, window, localStorage, fetchProxy, location, navigator, Event,
    noTimer, noTimer, () => true, () => {}, () => "", () => 0, consoleShim);

  // `__e2eInit` is the promise returned by the panel's `init()` IIFE.
  if (!globalThis.__e2eInit || typeof globalThis.__e2eInit.then !== "function") {
    throw new Error("panel bootstrap not captured: app.js init() shape changed");
  }
  await globalThis.__e2eInit;
  const T = globalThis.__T;
  if (!T) throw new Error("panel exports missing: app.js functions renamed?");

  const el = (id) => doc.getElementById(id);
  const fbList = () => el("fbList");
  const wrapOf = (input) => {
    const wrap = input.closest(".pk-wrap");
    if (!wrap) throw new Error("picker input is not inside .pk-wrap");
    return wrap;
  };
  const menuOf = (input) => wrapOf(input).querySelector(".pk-menu");

  /** Opens the picker dropdown the way a user does: type a keyword, then focus. */
  const openMenu = (input, keyword) => {
    if (keyword !== undefined) input.value = keyword;
    input.focus();
    fire(input, "input");
    const menu = menuOf(input);
    if (!menu) throw new Error("dropdown did not attach to this input");
    return menu;
  };
  const menuIds = (input, keyword) => openMenu(input, keyword).querySelectorAll(".pk-item").map((n) => n.dataset.id);

  const inlineToInput = (ruleIdx) => {
    const input = fbList().querySelector(`.fb-to-add[data-add-idx="${ruleIdx}"]`);
    if (!input) throw new Error(`no inline fallback input for rule ${ruleIdx}`);
    return input;
  };
  const inlineAddButton = (ruleIdx) => {
    const btn = fbList().querySelector(`[data-add-to="${ruleIdx}"]`);
    if (!btn) throw new Error(`no ＋ button for rule ${ruleIdx}`);
    return btn;
  };

  const helpers = {
    doc,
    T,
    el,
    reloads,
    /** Chip models of rule `i`, in display (= priority) order. */
    chipsOf(ruleIdx) {
      return fbList().querySelectorAll(".fb-to-chip")
        .filter((c) => String(c.dataset.chip || "").startsWith(`${ruleIdx}:`))
        .map((c) => c.dataset.model);
    },
    allChips() {
      return fbList().querySelectorAll(".fb-to-chip").map((c) => ({ chip: c.dataset.chip, model: c.dataset.model }));
    },
    inlineToInput,
    inlineAddButton,
    menuIdsOfInline(ruleIdx, keyword) { return menuIds(inlineToInput(ruleIdx), keyword); },
    menuIdsOfBottomTo(keyword) { return menuIds(el("fbToInput"), keyword); },
    menuIdsOfBottomFrom(keyword) { return menuIds(el("fbFromInput"), keyword); },
    pickInlineFromMenu(ruleIdx, modelId, keyword) {
      const input = inlineToInput(ruleIdx);
      const menu = openMenu(input, keyword);
      const item = menu.querySelector(`.pk-item[data-id="${modelId}"]`);
      if (!item) throw new Error(`candidate ${modelId} not in inline menu`);
      fire(item, "click");
      return helpers.chipsOf(ruleIdx);
    },
    /** Types into the inline fallback box and presses Enter (user's manual-entry path). */
    typeInlineAndEnter(ruleIdx, value) {
      const input = inlineToInput(ruleIdx);
      input.value = value;
      fire(input, "keydown", { key: "Enter" });
      return helpers.chipsOf(ruleIdx);
    },
    /** Types into the inline fallback box and clicks that row's ＋ button. */
    typeInlineAndClickPlus(ruleIdx, value) {
      inlineToInput(ruleIdx).value = value;
      fire(inlineAddButton(ruleIdx), "click");
      return helpers.chipsOf(ruleIdx);
    },
    /** Fills the bottom "new rule" form and clicks 添加规则. */
    addRuleViaForm(from, to) {
      el("fbFromInput").value = from;
      el("fbToInput").value = to;
      el("btnFbAdd").onclick();
      return { fbErr: el("fbErr").textContent, rules: T.__getFbRules() };
    },
    /** Opens the settings overlay (the only entry to the fallback editor). */
    openSettings() { T.openSettings(); },
    /** Clicks 保存降级链 and waits for the POST to settle. */
    async save() {
      await T.saveModelFallback();
      return { fbErr: el("fbErr").textContent, toast: helpers.toast(), rules: T.__getFbRules() };
    },
    toast: () => el("toast").textContent,
    clearToast() { el("toast").textContent = ""; },
    logs: () => consoleLines.slice(),
    fbErr: () => el("fbErr").textContent,
    dirty: () => el("fbDirty").style.display,
    ruleCards: () => fbList().querySelectorAll(".fb-rule-card").length,
    fromInputs: () => fbList().querySelectorAll(".fb-from-edit").map((n) => n.value),
    async reloadState() { await T.loadState(); },
    close() { T.closeSettings(); },
  };
  return helpers;
}

function fmtArg(v) {
  if (typeof v === "string") return v;
  if (v instanceof Error) return v.message;
  try { return JSON.stringify(v); } catch { return String(v); }
}
