// Tests for the home grid helpers in ../static/app.js: the size
// bounds a module may be offered (homeSizeAllowed: inside its
// floor and ceiling both) and the two-across fill plan
// (homeFillPlan: which module closes a row short and widens to
// finish it). app.js runs under a small DOM stub; only the pure
// helpers it exports for node are exercised.
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
  removeChild(c) {
    const at = this.children.indexOf(c);
    if (at >= 0) this.children.splice(at, 1);
    c.parentNode = null;
    return c;
  }
  addEventListener(type, fn) {
    (this._listeners[type] = this._listeners[type] || []).push(fn);
  }
  removeEventListener() {}
  focus() { this.focused = true; }
  querySelector() { return null; }
  querySelectorAll() { return []; }
}

class FakeRO {
  constructor(cb) { this.cb = cb; this.dead = false; FakeRO.live.push(this); }
  observe() {}
  disconnect() { this.dead = true; }
}
FakeRO.live = [];

function loadAppJS() {
  const doc = new El("html");
  doc.documentElement = doc;
  doc.body = new El("body");
  doc.createElement = function (tag) { return new El(tag); };
  doc.getElementById = function () { return null; };
  global.document = doc;
  global.window = global;
  global.innerWidth = 1280;
  global.innerHeight = 900;
  global.location = { origin: "https://example.test", pathname: "/", href: "https://example.test/" };
  Object.defineProperty(global, "navigator", { value: { userAgent: "node" }, configurable: true });
  global.localStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
  global.sessionStorage = global.localStorage;
  global.matchMedia = function () { return { matches: false, addEventListener() {}, addListener() {} }; };
  global.requestAnimationFrame = function (fn) { return setTimeout(fn, 0); };
  global.cancelAnimationFrame = function (id) { clearTimeout(id); };
  global.getComputedStyle = function (el) { return el._cs; };
  global.ResizeObserver = FakeRO;
  global.addEventListener = function () {};
  global.removeEventListener = function () {};
  const path = require.resolve("../static/app.js");
  delete require.cache[path];
  return require(path);
}

const app = loadAppJS();

test("homeSizeAllowed: inside the floor and ceiling both", function () {
  const min = { w: 2, h: 1 }, max = { w: 3, h: 1 }; // a band's bounds
  assert.equal(app.homeSizeAllowed(2, 1, min, max), true);
  assert.equal(app.homeSizeAllowed(3, 1, min, max), true);
  assert.equal(app.homeSizeAllowed(1, 1, min, max), false, "below the floor");
  assert.equal(app.homeSizeAllowed(3, 2, min, max), false, "past the ceiling: only empty cells");
  assert.equal(app.homeSizeAllowed(2, 2, min, max), false, "past the ceiling on height alone");
});

test("homeSizeAllowed: an unbounded module takes the whole grid only", function () {
  const min = { w: 1, h: 1 }, max = { w: 3, h: 3 };
  assert.equal(app.homeSizeAllowed(3, 3, min, max), true);
  assert.equal(app.homeSizeAllowed(4, 1, min, max), false);
  assert.equal(app.homeSizeAllowed(1, 4, min, max), false);
});

test("homeResizeFrame: at rest the frame is exactly the module", function () {
  // A module standing taller than its rows (its content needs the
  // room) and a grip pressed inside the corner: the frame that
  // shows up must cover the module, not stop at the pointer or at
  // the nominal cells.
  assert.deepEqual(app.homeResizeFrame(200, 260, 0, 0, 294, 176, 100, 182), { w: 200, h: 260 });
  assert.deepEqual(app.homeResizeFrame(300, 176, 0, 0, 294, 176, 100, 182), { w: 300, h: 176 }, "never below the size it started at");
});

test("homeResizeFrame: follows the pointer, held to the ceiling and half a cell", function () {
  assert.deepEqual(app.homeResizeFrame(200, 176, 50, 40, 294, 358, 100, 182), { w: 250, h: 216 }, "movement tracks 1:1");
  assert.deepEqual(app.homeResizeFrame(200, 176, 500, 500, 294, 358, 100, 182), { w: 294, h: 358 }, "the ceiling caps growth");
  assert.deepEqual(app.homeResizeFrame(200, 176, -500, -500, 294, 358, 100, 182), { w: 50, h: 91 }, "half a cell is the floor");
});

test("homeRowPitch: the resized module's own row step, not the smallest on the page", function () {
  // Rows size by content: the first row stands 400 tall, the
  // resized module's row 160. The old measure took the smallest
  // step on the page, so a drag in the tall row committed cells
  // the frame never covered.
  const tops = [0, 400, 560, 720];
  assert.equal(app.homeRowPitch(tops, 400, 182), 160, "the step out of the module's own row");
  assert.equal(app.homeRowPitch(tops, 0, 182), 400, "the first row steps to the second");
  assert.equal(app.homeRowPitch(tops, 720, 182), 160, "the last row reads the step into it");
  assert.equal(app.homeRowPitch([0, 0, 400, 400], 400, 182), 400, "cards sharing a row top collapse to one row");
  assert.equal(app.homeRowPitch([120], 120, 182), 182, "a single row falls back");
});

test("homeSnapCommit: release commits the cells the frame covered", function () {
  // The drag's hysteresis holds the starting cell until the
  // pointer is 62% across the next one; on release the committed
  // size is the frame's covered cells, rounded plainly, so the
  // size stored is the size the frame showed.
  assert.equal(app.homeSnapCommit(2, 100, 180, 3), 3, "past halfway into the next cell commits it");
  assert.equal(app.homeSnapCommit(2, -100, 180, 3), 1, "past halfway back commits the smaller size");
  assert.equal(app.homeSnapCommit(2, 40, 180, 3), 2, "a short drag keeps the size");
  assert.equal(app.homeSnapCommit(2, 9999, 180, 3), 3, "the ceiling clamps the commit");
  assert.equal(app.homeSnapCommit(2, -9999, 180, 3), 1, "one cell is the commit floor");
});

test("homeFillPlan: the module closing a row short widens to finish it", function () {
  assert.deepEqual(app.homeFillPlan([1], 2), [0], "a lone cell fills its row");
  assert.deepEqual(app.homeFillPlan([1, 1], 2), [], "an exact row leaves nothing to fill");
  assert.deepEqual(app.homeFillPlan([2, 1, 1], 2), [], "the wide module's row is finished by the pair after it");
  assert.deepEqual(app.homeFillPlan([1, 2], 2), [0], "the narrow module before a wide one finishes its row");
  assert.deepEqual(app.homeFillPlan([2, 2], 3), [0, 1], "two wides on a three-column grid each close short");
});
