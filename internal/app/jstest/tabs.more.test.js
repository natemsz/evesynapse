// Tests for the section tab overflow in ../static/app.js: when
// a tab strip's tabs stop fitting its row, the ones that don't
// fit fold into the strip's "More" disclosure instead of
// wrapping; the active tab never folds; widening restores.
// app.js runs under a small DOM stub whose elements carry the
// widths the layout measures, so the geometry is exercised end
// to end — init, fold, restore, disclosure — plus the two pure
// helpers app.js exports for node (planTabFold, tabMoreState).
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");

function hasClass(el, cls) {
  return (el.className || "").split(/\s+/).indexOf(cls) >= 0;
}

class El {
  constructor(tag) {
    this.tagName = (tag || "div").toUpperCase();
    this.className = "";
    this.id = "";
    this.children = [];
    this.parentNode = null;
    this.attrs = {};
    this.style = {};
    this.hidden = false;
    this.textContent = "";
    this.offsetWidth = 0;
    this.offsetLeft = 0;
    this.clientWidth = 0;
    this.scrollWidth = 0;
    this.scrollLeft = 0;
    this.isConnected = true;
    this.focused = false;
    this._listeners = {};
    this._cs = { columnGap: "6px", gap: "6px", paddingLeft: "0px", paddingRight: "0px" };
    const self = this;
    this.classList = {
      add(k) { if (!hasClass(self, k)) self.className = (self.className + " " + k).trim(); },
      remove(k) { self.className = self.className.split(/\s+/).filter(function (x) { return x && x !== k; }).join(" "); },
      contains(k) { return hasClass(self, k); },
      toggle(k, force) {
        const want = force === undefined ? !hasClass(self, k) : !!force;
        if (want) this.add(k); else this.remove(k);
        return want;
      },
    };
  }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
    if (k === "class") this.className = String(v);
    if (k === "id") this.id = String(v);
  }
  getAttribute(k) {
    if (k === "class") return this.className;
    if (k === "id") return this.id;
    return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null;
  }
  removeAttribute(k) { delete this.attrs[k]; }
  appendChild(c) {
    if (c.parentNode) c.parentNode.removeChild(c);
    c.parentNode = this;
    this.children.push(c);
    return c;
  }
  insertBefore(c, ref) {
    if (c.parentNode) c.parentNode.removeChild(c);
    c.parentNode = this;
    const at = ref ? this.children.indexOf(ref) : -1;
    if (at < 0) this.children.push(c);
    else this.children.splice(at, 0, c);
    return c;
  }
  removeChild(c) {
    const at = this.children.indexOf(c);
    if (at >= 0) this.children.splice(at, 1);
    c.parentNode = null;
    return c;
  }
  contains(n) {
    for (; n; n = n.parentNode) if (n === this) return true;
    return false;
  }
  _matches(sel) {
    // One compound: optional tag plus any number of classes.
    const m = /^([a-zA-Z]*)((?:\.[\w-]+)*)$/.exec(sel.trim());
    if (!m) return false;
    if (m[1] && this.tagName !== m[1].toUpperCase()) return false;
    const cls = m[2] ? m[2].slice(1).split(".") : [];
    for (const k of cls) if (!hasClass(this, k)) return false;
    return true;
  }
  querySelector(sel) {
    let found = null;
    (function walk(el) {
      for (const c of el.children) {
        if (found) return;
        if (c._matches(sel)) { found = c; return; }
        walk(c);
      }
    })(this);
    return found;
  }
  querySelectorAll(sel) {
    const parts = sel.split(",");
    const out = [];
    (function walk(el) {
      for (const c of el.children) {
        for (const p of parts) if (c._matches(p)) { out.push(c); break; }
        walk(c);
      }
    })(this);
    return out;
  }
  addEventListener(type, fn) {
    (this._listeners[type] = this._listeners[type] || []).push(fn);
  }
  removeEventListener() {}
  focus() { this.focused = true; }
}

function fireEl(el, type, extra) {
  const ev = Object.assign({
    target: el, key: "", stopPropagation() {}, preventDefault() {},
  }, extra || {});
  (el._listeners[type] || []).slice().forEach(function (fn) { fn(ev); });
}

class FakeRO {
  constructor(cb) { this.cb = cb; this.dead = false; FakeRO.live.push(this); }
  observe() {}
  disconnect() { this.dead = true; }
}
FakeRO.live = [];

function makeTab(label, width, active) {
  const a = new El("a");
  a.textContent = label;
  a.offsetWidth = width;
  if (active) a.className = "active";
  return a;
}

// A strip as the template renders it: tab links in order, then
// the hidden More disclosure (button + empty panel).
function makeStrip(labels, widths, activeIdx, moreWidth) {
  const strip = new El("nav");
  strip.className = "section-tabs";
  const tabs = labels.map(function (label, i) {
    const a = makeTab(label, widths[i], i === activeIdx);
    strip.appendChild(a);
    return a;
  });
  const wrap = new El("span");
  wrap.className = "tabs-more";
  wrap.hidden = true;
  const btn = new El("button");
  btn.className = "tabs-more-btn";
  btn.textContent = "More";
  btn.offsetWidth = moreWidth;
  btn.setAttribute("aria-expanded", "false");
  const panel = new El("span");
  panel.className = "tabs-more-panel";
  panel.hidden = true;
  wrap.appendChild(btn);
  wrap.appendChild(panel);
  strip.appendChild(wrap);
  return { strip, tabs, wrap, btn, panel };
}

function makeEnv(strips) {
  FakeRO.live = [];
  const docListeners = {};
  const winListeners = {};
  const outside = new El("div");
  const doc = {
    _listeners: docListeners,
    addEventListener(type, fn) { (docListeners[type] = docListeners[type] || []).push(fn); },
    removeEventListener() {},
    querySelector() { return null; },
    querySelectorAll(sel) {
      if (sel.indexOf("section-tabs") >= 0) return strips;
      return [];
    },
    getElementById() { return null; },
    createElement(tag) { return new El(tag); },
    documentElement: new El("html"),
    body: new El("body"),
    hidden: false,
    fonts: { ready: Promise.resolve() },
  };
  function fireDoc(type, extra) {
    const ev = Object.assign({
      target: outside, key: "", detail: null, stopPropagation() {}, preventDefault() {},
    }, extra || {});
    (docListeners[type] || []).slice().forEach(function (fn) { fn(ev); });
  }
  function fireWin(type) {
    (winListeners[type] || []).slice().forEach(function (fn) { fn({}); });
  }
  return { doc, outside, fireDoc, fireWin, winListeners };
}

function loadAppJS(env) {
  global.document = env.doc;
  global.window = global;
  global.innerWidth = 1024;
  global.innerHeight = 768;
  global.location = { origin: "https://example.test", pathname: "/", href: "https://example.test/" };
  Object.defineProperty(global, "navigator", { value: { userAgent: "node" }, configurable: true });
  global.localStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
  global.sessionStorage = global.localStorage;
  global.matchMedia = function () { return { matches: false, addEventListener() {}, addListener() {} }; };
  global.requestAnimationFrame = function (fn) { return setTimeout(fn, 0); };
  global.cancelAnimationFrame = function (id) { clearTimeout(id); };
  global.getComputedStyle = function (el) { return el._cs; };
  global.ResizeObserver = FakeRO;
  global.addEventListener = function (type, fn) { (env.winListeners[type] = env.winListeners[type] || []).push(fn); };
  global.removeEventListener = function () {};
  const path = require.resolve("../static/app.js");
  delete require.cache[path];
  return require(path);
}

function delay(ms) {
  return new Promise(function (resolve) { setTimeout(resolve, ms); });
}

// A resize the observers would report; layouts run on the next
// animation frame, which the stub serves as a zero timeout.
async function relayout(env) {
  for (const ro of FakeRO.live) if (!ro.dead) ro.cb([]);
  env.fireWin("resize");
  await delay(20);
}

function rowLabels(strip, wrap) {
  return strip.children.filter(function (c) { return c !== wrap; })
    .map(function (c) { return c.textContent; });
}
function panelLabels(panel) {
  return panel.children.map(function (c) { return c.textContent; });
}

test("planTabFold: all fit, fold from the end, restore math", function () {
  const api = loadAppJS(makeEnv([]));
  assert.deepEqual(api.planTabFold([120, 140, 110, 160], 0, 800, 90, 6),
    { visible: [0, 1, 2, 3], folded: [] });
  assert.deepEqual(api.planTabFold([120, 140, 110, 160], 0, 400, 90, 6),
    { visible: [0, 1], folded: [2, 3] });
});

test("planTabFold: the active tab never folds", function () {
  const api = loadAppJS(makeEnv([]));
  // A naive fold-from-the-end puts tab 2 (active) in the panel
  // first; the protected fold empties the row around it instead.
  assert.deepEqual(api.planTabFold([100, 100, 180], 2, 250, 80, 6),
    { visible: [2], folded: [0, 1] });
  // Active in the middle: everything foldable around it goes
  // before it does.
  assert.deepEqual(api.planTabFold([90, 150, 90, 90], 1, 330, 80, 6),
    { visible: [1], folded: [0, 2, 3] });
  // No active tab: fold from the end, keep at least one visible.
  assert.deepEqual(api.planTabFold([100, 100, 100], -1, 10, 80, 6),
    { visible: [0], folded: [1, 2] });
});

test("tabMoreState: the button reflects a folded active tab", function () {
  const api = loadAppJS(makeEnv([]));
  assert.deepEqual(api.tabMoreState(["Alpha", "Beta", "Gamma"], 2, [0, 2]),
    { label: "Gamma", hasActive: true });
  assert.deepEqual(api.tabMoreState(["Alpha", "Beta", "Gamma"], 2, [0, 1]),
    { label: "More", hasActive: false });
  assert.deepEqual(api.tabMoreState(["Alpha", "Beta"], -1, [0]),
    { label: "More", hasActive: false });
});

test("a strip that fits renders untouched; shrinking folds, widening restores", async function () {
  const s = makeStrip(["Alpha", "Beta", "Gamma", "Delta"], [120, 140, 110, 160], 0, 90);
  s.strip.clientWidth = 800;
  s.strip.scrollWidth = 800;
  const env = makeEnv([s.strip]);
  loadAppJS(env);
  assert.equal(s.wrap.hidden, true, "More stays hidden when everything fits");
  assert.deepEqual(rowLabels(s.strip, s.wrap), ["Alpha", "Beta", "Gamma", "Delta"]);

  s.strip.clientWidth = 400;
  await relayout(env);
  assert.equal(s.wrap.hidden, false, "More appears once a fold has somewhere to go");
  assert.deepEqual(rowLabels(s.strip, s.wrap), ["Alpha", "Beta"], "the fitting tabs stay tabs");
  assert.deepEqual(panelLabels(s.panel), ["Gamma", "Delta"], "the rest fold in order");
  assert.equal(s.btn.textContent, "More");
  assert.equal(s.wrap.classList.contains("has-active"), false);
  assert.ok(s.strip.classList.contains("has-more"));

  s.strip.clientWidth = 800;
  await relayout(env);
  assert.equal(s.wrap.hidden, true, "More hides again when the row fits");
  assert.deepEqual(rowLabels(s.strip, s.wrap), ["Alpha", "Beta", "Gamma", "Delta"], "widening restores every tab");
  assert.deepEqual(panelLabels(s.panel), []);
});

test("the active tab stays in the row at widths that fold everything else", async function () {
  const s = makeStrip(["Alpha", "Beta", "Gamma"], [100, 100, 180], 2, 80);
  s.strip.clientWidth = 250;
  s.strip.scrollWidth = 250;
  const env = makeEnv([s.strip]);
  loadAppJS(env);
  assert.equal(s.tabs[2].parentNode, s.strip, "active tab is in the row, not the panel");
  assert.deepEqual(panelLabels(s.panel), ["Alpha", "Beta"]);
  assert.equal(s.btn.textContent, "More", "no reflection while the active tab is visible");
  assert.equal(s.wrap.classList.contains("has-active"), false);
});

test("the disclosure opens, closes on link click, outside click and Escape", async function () {
  const s = makeStrip(["Alpha", "Beta", "Gamma", "Delta"], [120, 140, 110, 160], 0, 90);
  s.strip.clientWidth = 400;
  s.strip.scrollWidth = 400;
  const env = makeEnv([s.strip]);
  loadAppJS(env);
  assert.equal(s.panel.hidden, true);

  fireEl(s.btn, "click");
  assert.equal(s.panel.hidden, false, "button opens the panel");
  assert.equal(s.btn.getAttribute("aria-expanded"), "true");
  assert.ok(s.strip.classList.contains("more-open"));
  assert.equal(s.btn.getAttribute("aria-controls"), s.panel.id, "button names its panel");

  fireEl(s.panel, "click", { target: s.panel.children[0] });
  assert.equal(s.panel.hidden, true, "choosing a folded tab closes the panel");
  assert.equal(s.btn.getAttribute("aria-expanded"), "false");

  fireEl(s.btn, "click");
  assert.equal(s.panel.hidden, false);
  env.fireDoc("click", { target: env.outside });
  assert.equal(s.panel.hidden, true, "an outside click closes it");

  fireEl(s.btn, "click");
  assert.equal(s.panel.hidden, false);
  env.fireDoc("keydown", { key: "Escape" });
  assert.equal(s.panel.hidden, true, "Escape closes it");
  assert.equal(s.btn.focused, true, "Escape returns focus to the button");
});
