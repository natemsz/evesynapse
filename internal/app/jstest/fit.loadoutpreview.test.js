// Tests for the My Loadouts Modules and cargo preview in
// ../static/fit.js: each saved-fit card renders its grouped item
// lists in a hidden panel, and the trigger button opens it as a
// floating preview instead of expanding the card in place.
// Driven through a small DOM stub; fit.js's editor block bails in
// this environment (no #fit-editor), and the handlers under test
// are delegated from the document, so the stub only needs
// document-level dispatch plus the class selectors the preview
// code uses.
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
    this.focused = false;
    this.rect = { left: 0, top: 0, right: 100, bottom: 100, width: 100, height: 100 };
    const self = this;
    this.classList = {
      add(k) { if (!hasClass(self, k)) self.className = (self.className + " " + k).trim(); },
      remove(k) { self.className = self.className.split(/\s+/).filter(function (x) { return x && x !== k; }).join(" "); },
      contains(k) { return hasClass(self, k); },
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
  appendChild(c) {
    c.parentNode = this;
    this.children.push(c);
    return c;
  }
  contains(n) {
    for (; n; n = n.parentNode) if (n === this) return true;
    return false;
  }
  closest(sel) {
    // Only bare class selectors resolve; anything richer (the
    // tooltip block's compound selectors) matches nothing here.
    if (!/^\.[\w-]+$/.test(sel)) return null;
    const cls = sel.slice(1);
    for (let n = this; n && n.tagName; n = n.parentNode) if (hasClass(n, cls)) return n;
    return null;
  }
  querySelector(sel) {
    if (!/^\.[\w-]+$/.test(sel)) return null;
    const cls = sel.slice(1);
    let found = null;
    (function walk(el) {
      for (const c of el.children) {
        if (found) return;
        if (hasClass(c, cls)) { found = c; return; }
        walk(c);
      }
    })(this);
    return found;
  }
  getBoundingClientRect() { return this.rect; }
  focus() { this.focused = true; }
  addEventListener() {}
}

function makeCard() {
  const wrap = new El("div");
  wrap.className = "loadout-preview";
  const btn = new El("button");
  btn.className = "loadout-preview-btn";
  btn.setAttribute("aria-expanded", "false");
  const panel = new El("div");
  panel.className = "loadout-preview-panel";
  panel.hidden = true;
  wrap.appendChild(btn);
  wrap.appendChild(panel);
  return { wrap, btn, panel };
}

function makeEnv(finePointer) {
  const listeners = {};
  const a = makeCard();
  const b = makeCard();
  const outside = new El("div");
  const doc = {
    _listeners: listeners,
    addEventListener(type, fn) {
      (listeners[type] = listeners[type] || []).push(fn);
    },
    getElementById() { return null; },
    createElement(tag) { return new El(tag); },
    querySelector() { return null; },
    querySelectorAll(sel) {
      if (sel === ".loadout-preview") return [a.wrap, b.wrap];
      return [];
    },
  };
  function fire(type, target, extra) {
    const ev = Object.assign({
      target: target,
      relatedTarget: null,
      key: "",
      stopPropagation() {},
      preventDefault() {},
    }, extra || {});
    (listeners[type] || []).slice().forEach(function (fn) { fn(ev); });
  }
  return { doc, fire, a, b, outside, finePointer: !!finePointer };
}

function loadFitJS(env) {
  global.document = env.doc;
  global.window = global;
  global.innerWidth = 1024;
  global.innerHeight = 768;
  global.matchMedia = function () { return { matches: env.finePointer }; };
  const path = require.resolve("../static/fit.js");
  delete require.cache[path];
  require(path);
}

function delay(ms) {
  return new Promise(function (resolve) { setTimeout(resolve, ms); });
}

test("click opens the preview pinned, second click closes it", function () {
  const env = makeEnv(false);
  loadFitJS(env);
  env.fire("click", env.a.btn);
  assert.equal(env.a.panel.hidden, false, "panel shown after click");
  assert.equal(env.a.btn.getAttribute("aria-expanded"), "true");
  assert.ok(env.a.panel.id, "panel got an id when opened");
  assert.equal(env.a.btn.getAttribute("aria-controls"), env.a.panel.id);
  env.fire("click", env.a.btn);
  assert.equal(env.a.panel.hidden, true, "second click closes");
  assert.equal(env.a.btn.getAttribute("aria-expanded"), "false");
  assert.equal(env.b.panel.hidden, true, "the other card never opened");
});

test("opening one card closes the other; outside click and Escape close", function () {
  const env = makeEnv(false);
  loadFitJS(env);
  env.fire("click", env.a.btn);
  assert.equal(env.a.panel.hidden, false);
  env.fire("click", env.b.btn);
  assert.equal(env.a.panel.hidden, true, "first preview closed when the second opened");
  assert.equal(env.a.btn.getAttribute("aria-expanded"), "false");
  assert.equal(env.b.panel.hidden, false);
  env.fire("click", env.outside);
  assert.equal(env.b.panel.hidden, true, "outside click closes");
  env.fire("click", env.b.btn);
  assert.equal(env.b.panel.hidden, false);
  env.fire("keydown", env.b.btn, { key: "Escape" });
  assert.equal(env.b.panel.hidden, true, "Escape closes");
  assert.equal(env.b.btn.focused, true, "Escape returns focus to the trigger");
});

test("a panel that would overflow the viewport flips up and right-aligns", function () {
  const env = makeEnv(false);
  loadFitJS(env);
  env.a.panel.rect = { left: 900, top: 700, right: 1400, bottom: 1000, width: 500, height: 300 };
  env.fire("click", env.a.btn);
  assert.ok(env.a.panel.classList.contains("flip-up"), "flips up when the bottom overflows");
  assert.ok(env.a.panel.classList.contains("align-right"), "right-aligns when the right edge overflows");
});

test("hover opens unpinned on fine pointers; leaving closes; click pins", async function () {
  const env = makeEnv(true);
  loadFitJS(env);
  env.fire("pointerover", env.a.btn, { relatedTarget: env.outside });
  assert.equal(env.a.panel.hidden, true, "not open before the intent delay");
  await delay(260);
  assert.equal(env.a.panel.hidden, false, "hover opened the preview");
  env.fire("pointerout", env.a.btn, { relatedTarget: env.outside });
  await delay(320);
  assert.equal(env.a.panel.hidden, true, "leaving closed the unpinned preview");

  env.fire("pointerover", env.a.btn, { relatedTarget: env.outside });
  await delay(260);
  assert.equal(env.a.panel.hidden, false);
  env.fire("click", env.a.btn);
  env.fire("pointerout", env.a.btn, { relatedTarget: env.outside });
  await delay(320);
  assert.equal(env.a.panel.hidden, false, "click pinned the hover-opened preview open");
  env.fire("click", env.a.btn);
  assert.equal(env.a.panel.hidden, true, "second click closes the pinned preview");
});

test("no hover wiring without a fine pointer", async function () {
  const env = makeEnv(false);
  loadFitJS(env);
  env.fire("pointerover", env.a.btn, { relatedTarget: env.outside });
  await delay(260);
  assert.equal(env.a.panel.hidden, true, "hover does nothing on touch/coarse pointers");
});
