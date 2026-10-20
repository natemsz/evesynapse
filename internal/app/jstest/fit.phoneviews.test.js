"use strict";
// Fitting tool, phone views: at phone width the workbench is
// three views (Slot Racks, Resists & Cap, Radial) under a tab
// bar, opening on the Radial; the module search rides with the
// racks and radial views; and a fitted module's card in Slot
// Racks cycles its state line-wide through the states the module
// allows, committed as one simulate payload. Above the band
// nothing is hidden and the search stays in its home slot.
//fit.js self-executes against whatever globals exist when it is
// required, so each block below rebuilds the globals it needs
// and requires the file fresh.

const test = require("node:test");
const assert = require("node:assert/strict");
const path = require("node:path");

const fitFile = path.join(__dirname, "..", "static", "fit.js");

// ---------- minimal DOM (only what fit.js touches) ----------

function parseCompound(s) {
  const out = { tag: null, id: null, classes: [], attrs: [] };
  const re = /([a-zA-Z][\w-]*)|#([\w-]+)|\.([\w-]+)|\[([\w-]+)(?:="([^"]*)")?\]/g;
  let m;
  while ((m = re.exec(s))) {
    if (m[1]) out.tag = m[1].toUpperCase();
    else if (m[2]) out.id = m[2];
    else if (m[3]) out.classes.push(m[3]);
    else if (m[4]) out.attrs.push([m[4], m[5] === undefined ? null : m[5]]);
  }
  return out;
}

function matchCompound(el, c) {
  if (!el || el.nodeType !== 1) return false;
  if (c.tag && el.tagName !== c.tag) return false;
  if (c.id && el.getAttribute("id") !== c.id) return false;
  for (const cl of c.classes) if (!el.classList.contains(cl)) return false;
  for (const [name, val] of c.attrs) {
    const got = el.getAttribute(name);
    if (got === null) return false;
    if (val !== null && got !== val) return false;
  }
  return true;
}

// One selector group: compounds joined by descendant/child
// combinators, matched right to left.
function matchGroup(el, group) {
  const parts = group.replace(/\s*>\s*/g, " > ").trim().split(/\s+/);
  const seq = []; // [{compound, comb}] comb links to the previous part
  let comb = " ";
  for (const p of parts) {
    if (p === ">") { comb = ">"; continue; }
    seq.push({ c: parseCompound(p), comb });
    comb = " ";
  }
  let node = el;
  let i = seq.length - 1;
  if (!matchCompound(node, seq[i].c)) return false;
  while (i > 0) {
    const want = seq[i - 1].c;
    if (seq[i].comb === ">") {
      node = node.parentNode;
      if (!matchCompound(node, want)) return false;
    } else {
      node = node.parentNode;
      while (node && !matchCompound(node, want)) node = node.parentNode;
      if (!node) return false;
    }
    i--;
  }
  return true;
}

function matchesSel(el, sel) {
  return sel.split(",").some((g) => matchGroup(el, g.trim()));
}

function queryAll(root, sel, out) {
  out = out || [];
  let scopeRoot = root;
  let s = sel.trim();
  if (s.startsWith(":scope")) {
    s = s.slice(6).trim();
    if (s.startsWith(">")) s = s.slice(1).trim();
  }
  const walk = (node) => {
    for (const child of node.childNodes.slice()) {
      if (child.nodeType !== 1) continue;
      if (matchesSel(child, s)) out.push(child);
      walk(child);
    }
  };
  walk(scopeRoot);
  return out;
}

class TextNode {
  constructor(text) {
    this.nodeType = 3;
    this.parentNode = null;
    this._text = String(text);
  }
  get textContent() { return this._text; }
  set textContent(v) { this._text = String(v); }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
}

class El {
  constructor(tag) {
    this.nodeType = 1;
    this.tagName = String(tag).toUpperCase();
    this.parentNode = null;
    this.childNodes = [];
    this._attrs = {};
    this._listeners = {};
    this.style = {};
    this.value = "";
    this.checked = false;
    this.disabled = false;
    this.hidden = false;
    this._innerHTML = "";
    this._className = "";
    const self = this;
    this.classList = {
      add(...cs) { const s = new Set(self._classes()); cs.forEach((c) => s.add(c)); self._className = [...s].join(" "); },
      remove(...cs) { const s = new Set(self._classes()); cs.forEach((c) => s.delete(c)); self._className = [...s].join(" "); },
      toggle(c, force) {
        const has = self.classList.contains(c);
        const want = force === undefined ? !has : !!force;
        if (want) self.classList.add(c); else self.classList.remove(c);
        return want;
      },
      contains(c) { return self._classes().includes(c); },
    };
  }
  _classes() { return this._className ? this._className.split(/\s+/).filter(Boolean) : []; }
  get className() { return this._className; }
  set className(v) { this._className = String(v || ""); }
  get children() { return this.childNodes.filter((n) => n.nodeType === 1); }
  get firstChild() { return this.childNodes[0] || null; }
  get nextSibling() {
    if (!this.parentNode) return null;
    const sib = this.parentNode.childNodes;
    const i = sib.indexOf(this);
    return i >= 0 && i + 1 < sib.length ? sib[i + 1] : null;
  }
  get previousSibling() {
    if (!this.parentNode) return null;
    const sib = this.parentNode.childNodes;
    const i = sib.indexOf(this);
    return i > 0 ? sib[i - 1] : null;
  }
  get textContent() {
    return this.childNodes.map((n) => n.textContent).join("");
  }
  set textContent(v) {
    for (const n of this.childNodes) n.parentNode = null;
    this.childNodes = [];
    const t = new TextNode(v);
    t.parentNode = this;
    this.childNodes.push(t);
  }
  get innerHTML() { return this._innerHTML; }
  set innerHTML(v) {
    this._innerHTML = String(v || "");
    for (const n of this.childNodes) n.parentNode = null;
    this.childNodes = [];
  }
  setAttribute(name, value) {
    if (name === "class") { this._className = String(value); return; }
    if (name === "id") { this._attrs.id = String(value); return; }
    this._attrs[name] = String(value);
  }
  getAttribute(name) {
    if (name === "class") return this._className || null;
    return Object.prototype.hasOwnProperty.call(this._attrs, name) ? this._attrs[name] : null;
  }
  hasAttribute(name) { return this.getAttribute(name) !== null; }
  removeAttribute(name) { delete this._attrs[name]; }
  appendChild(node) {
    if (node.parentNode) node.parentNode.removeChild(node);
    node.parentNode = this;
    this.childNodes.push(node);
    return node;
  }
  insertBefore(node, ref) {
    if (!ref) return this.appendChild(node);
    if (node.parentNode) node.parentNode.removeChild(node);
    const i = this.childNodes.indexOf(ref);
    if (i < 0) return this.appendChild(node);
    node.parentNode = this;
    this.childNodes.splice(i, 0, node);
    return node;
  }
  removeChild(node) {
    const i = this.childNodes.indexOf(node);
    if (i >= 0) { this.childNodes.splice(i, 1); node.parentNode = null; }
    return node;
  }
  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  contains(node) {
    let n = node;
    while (n) { if (n === this) return true; n = n.parentNode; }
    return false;
  }
  closest(sel) {
    let n = this;
    while (n && n.nodeType === 1) {
      if (matchesSel(n, sel)) return n;
      n = n.parentNode;
    }
    return null;
  }
  querySelector(sel) { return queryAll(this, sel)[0] || null; }
  querySelectorAll(sel) { return queryAll(this, sel); }
  addEventListener(type, fn) {
    (this._listeners[type] = this._listeners[type] || []).push(fn);
  }
  removeEventListener(type, fn) {
    const l = this._listeners[type] || [];
    const i = l.indexOf(fn);
    if (i >= 0) l.splice(i, 1);
  }
  focus() {}
  blur() {}
  select() {}
  scrollIntoView() {}
  getBoundingClientRect() { return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; }
}

function makeDocument() {
  const doc = {
    nodeType: 9,
    _listeners: {},
    documentElement: null,
    body: null,
    createElement(tag) { return new El(tag); },
    createTextNode(text) { return new TextNode(text); },
    getElementById(id) {
      let found = null;
      const walk = (node) => {
        if (found || !node || node.nodeType !== 1) return;
        if (node.getAttribute("id") === id) { found = node; return; }
        for (const c of node.childNodes) walk(c);
      };
      walk(doc.documentElement);
      return found;
    },
    querySelector(sel) { return queryAll(doc.documentElement, sel)[0] || null; },
    querySelectorAll(sel) { return queryAll(doc.documentElement, sel); },
    addEventListener(type, fn) {
      (doc._listeners[type] = doc._listeners[type] || []).push(fn);
    },
    removeEventListener() {},
  };
  return doc;
}

function el(doc, tag, cls, attrs) {
  const e = doc.createElement(tag);
  if (cls) e.className = cls;
  if (attrs) for (const k of Object.keys(attrs)) e.setAttribute(k, attrs[k]);
  return e;
}

// A page with the editor open on a fit: the module search in its
// home slot, the workbench fragment with the phone header + tab
// bar, and one fitted line of two active launchers.
function buildPage(doc) {
  const html = el(doc, "html");
  const body = el(doc, "body");
  html.appendChild(body);
  doc.documentElement = html;
  doc.body = body;

  const editor = el(doc, "section", "", { id: "fit-editor" });
  body.appendChild(editor);

  const pilot = el(doc, "select", "", { id: "fit-pilot" });
  pilot.value = "0";
  editor.appendChild(pilot);
  const clone = el(doc, "select", "", { id: "fit-clone" });
  clone.value = "0";
  editor.appendChild(clone);

  const modsearch = el(doc, "div", "fit-modsearch searchpanel", { id: "fit-modsearch" });
  const modInput = el(doc, "input", "", { id: "fit-mod-search" });
  modsearch.appendChild(modInput);
  modsearch.appendChild(el(doc, "div", "", { id: "fit-mod-suggest" }));
  editor.appendChild(modsearch);

  const wb = el(doc, "div", "fit-wb");
  wb.setAttribute("data-fit-state", JSON.stringify({
    name: "Fixture Fit",
    shipTypeId: 587,
    items: [{ typeId: 999, qty: 2, states: ["active", "active"] }],
    charges: {},
    implants: {},
  }));
  editor.appendChild(wb);

  wb.appendChild(el(doc, "div", "fit-mhead"));
  const tabs = el(doc, "div", "fit-mtabs");
  const tabBtns = {};
  for (const v of ["racks", "stats", "radial"]) {
    const b = el(doc, "button", "", { "data-fit-view": v });
    b.textContent = v;
    tabs.appendChild(b);
    tabBtns[v] = b;
  }
  wb.appendChild(tabs);

  const center = el(doc, "div", "fit-center");
  wb.appendChild(center);
  const cols = el(doc, "div", "fit-cols");
  wb.appendChild(cols);
  const slots = el(doc, "div", "fit-slots");
  const bays = el(doc, "div", "fit-bays");
  cols.appendChild(slots);
  cols.appendChild(bays);
  const group = el(doc, "div", "fit-group", { "data-fit-group": "high" });
  slots.appendChild(group);
  const chip = el(doc, "span", "fit-chip st-active", {
    "data-fit-rowtype": "999",
    "data-fit-states": "offline,online,active,overheated",
    "data-fit-state": "active",
    "data-tip-name": "Fixture Launcher",
    "data-tip-lines": "",
  });
  group.appendChild(chip);
  const stats = el(doc, "div", "fit-stats");
  cols.appendChild(stats);

  return { editor, modsearch, wb, tabs, tabBtns, center, slots, bays, stats, chip };
}

function click(target) {
  const ev = {
    type: "click",
    target,
    _stopped: false,
    stopPropagation() { this._stopped = true; },
    preventDefault() {},
  };
  // Bubble from the target to the document root, like the DOM.
  let node = target;
  while (node) {
    ev.currentTarget = node;
    const l = (node._listeners && node._listeners.click) || [];
    for (const fn of l.slice()) fn(ev);
    if (ev._stopped) break;
    node = node.parentNode;
  }
}

function bootEditor(matches45) {
  const doc = makeDocument();
  const page = buildPage(doc);
  const fetches = [];
  const mqListeners = [];
  const mq = {
    media: "(max-width: 45rem)",
    matches: matches45,
    addEventListener(type, fn) { if (type === "change") mqListeners.push(fn); },
    removeEventListener() {},
  };
  global.document = doc;
  global.window = global;
  global.location = { href: "" };
  global.addEventListener = function () {};
  global.removeEventListener = function () {};
  global.matchMedia = function (q) {
    if (q === "(max-width: 45rem)") return mq;
    return { media: q, matches: false, addEventListener() {}, removeEventListener() {} };
  };
  global.fetch = function (url, opts) {
    fetches.push({ url, opts });
    return Promise.resolve({
      ok: true,
      status: 200,
      text: () => Promise.resolve(""),
      json: () => Promise.resolve(null),
    });
  };
  delete require.cache[require.resolve(fitFile)];
  require(fitFile);
  return {
    doc, page, fetches,
    setPhone(on) {
      mq.matches = on;
      for (const fn of mqListeners.slice()) fn({ matches: on });
    },
  };
}

const tick = () => new Promise((r) => setImmediate(r));

// ---------- pure view decisions ----------

test("phone views: section map and state cycle", () => {
  // No editor on the page: fit.js still loads and exports the
  // pure helpers.
  const doc = makeDocument();
  doc.documentElement = el(doc, "html");
  global.document = doc;
  global.window = global;
  global.location = { href: "" };
  global.addEventListener = function () {};
  global.fetch = undefined;
  delete require.cache[require.resolve(fitFile)];
  const { FitPhoneViews } = require(fitFile);

  assert.deepEqual(FitPhoneViews.sections("radial"),
    { center: true, slots: false, bays: false, stats: false, search: true });
  assert.deepEqual(FitPhoneViews.sections("racks"),
    { center: false, slots: true, bays: true, stats: false, search: true });
  assert.deepEqual(FitPhoneViews.sections("stats"),
    { center: false, slots: false, bays: false, stats: true, search: false });

  const all = "offline,online,active,overheated";
  assert.equal(FitPhoneViews.nextState(all, "online"), "active");
  assert.equal(FitPhoneViews.nextState(all, "active"), "overheated");
  assert.equal(FitPhoneViews.nextState(all, "overheated"), "offline");
  assert.equal(FitPhoneViews.nextState(all, "offline"), "online");
  // Mixed lines ("") enter at the first state in the order.
  assert.equal(FitPhoneViews.nextState(all, ""), "online");
  // The cycle stays inside the states the module allows.
  assert.equal(FitPhoneViews.nextState("offline,online", "online"), "offline");
  assert.equal(FitPhoneViews.nextState("offline,online", "offline"), "online");
  assert.equal(FitPhoneViews.nextState("", "active"), "");
});

// ---------- editor wiring ----------

test("phone: opening a fit lands on the Radial", async () => {
  const { page } = bootEditor(true);
  await tick();
  assert.equal(page.center.classList.contains("fv-off"), false, "radial visible");
  assert.equal(page.slots.classList.contains("fv-off"), true, "racks hidden");
  assert.equal(page.bays.classList.contains("fv-off"), true, "bays hidden");
  assert.equal(page.stats.classList.contains("fv-off"), true, "stats hidden");
  assert.equal(page.tabBtns.radial.classList.contains("on"), true, "radial tab marked");
  assert.equal(page.tabBtns.radial.getAttribute("aria-selected"), "true");
  assert.equal(page.tabBtns.racks.getAttribute("aria-selected"), "false");
  // The search parks inside the workbench, right under the tabs.
  assert.equal(page.modsearch.parentNode, page.wb, "search parked in the workbench");
  assert.equal(page.modsearch.previousSibling, page.tabs, "search sits under the tab bar");
  assert.equal(page.modsearch.classList.contains("fv-off"), false, "search visible on the radial");
});

test("phone: tabs switch views; the search leaves with the stats view", async () => {
  const { page } = bootEditor(true);
  await tick();
  click(page.tabBtns.stats);
  assert.equal(page.stats.classList.contains("fv-off"), false, "stats visible");
  assert.equal(page.center.classList.contains("fv-off"), true, "radial hidden");
  assert.equal(page.slots.classList.contains("fv-off"), true, "racks hidden");
  assert.equal(page.modsearch.classList.contains("fv-off"), true, "search hidden on stats");
  assert.equal(page.tabBtns.stats.classList.contains("on"), true);

  click(page.tabBtns.racks);
  assert.equal(page.slots.classList.contains("fv-off"), false, "racks visible");
  assert.equal(page.bays.classList.contains("fv-off"), false, "bays visible");
  assert.equal(page.stats.classList.contains("fv-off"), true, "stats hidden");
  assert.equal(page.modsearch.classList.contains("fv-off"), false, "search back on racks");
  assert.equal(page.modsearch.parentNode, page.wb, "search still parked");
});

test("phone: tapping a module card in Slot Racks cycles the whole line", async () => {
  const { page, fetches } = bootEditor(true);
  await tick();
  click(page.tabBtns.racks);
  fetches.length = 0;
  click(page.chip);
  const sims = fetches.filter((f) => String(f.url).includes("/fittings/simulate/"));
  assert.equal(sims.length, 1, "one simulate for the tap");
  const payload = JSON.parse(sims[0].opts.body);
  const line = payload.items.find((i) => i.typeId === 999);
  assert.ok(line, "the fitted line is in the payload");
  assert.deepEqual(line.states, ["overheated", "overheated"],
    "active advances to overheated for every instance, as one commit");
});

test("phone: a card tap outside Slot Racks does not cycle", async () => {
  const { page, fetches } = bootEditor(true);
  await tick();
  fetches.length = 0;
  // Still on the Radial landing view: the same tap is not a cycle.
  click(page.chip);
  const sims = fetches.filter((f) => String(f.url).includes("/fittings/simulate/"));
  assert.equal(sims.length, 0, "no simulate from a radial-view tap");
});

test("desktop widths: no view is hidden and the search stays home", async () => {
  const { page, setPhone } = bootEditor(false);
  await tick();
  for (const sec of [page.center, page.slots, page.bays, page.stats]) {
    assert.equal(sec.classList.contains("fv-off"), false, "nothing hidden");
  }
  assert.equal(page.modsearch.parentNode, page.editor, "search in its home slot");
  assert.equal(page.tabBtns.radial.classList.contains("on"), false, "no tab marked");

  // Crossing into the band applies the landing view; crossing
  // back restores everything.
  setPhone(true);
  assert.equal(page.center.classList.contains("fv-off"), false, "radial lands");
  assert.equal(page.stats.classList.contains("fv-off"), true, "stats hidden");
  assert.equal(page.modsearch.parentNode, page.wb, "search parked");
  setPhone(false);
  assert.equal(page.stats.classList.contains("fv-off"), false, "stats restored");
  assert.equal(page.modsearch.parentNode, page.editor, "search back home");
});
