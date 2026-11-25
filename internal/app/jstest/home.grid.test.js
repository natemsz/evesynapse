// Tests for the home grid helpers in ../static/home.js: the sizes a
// widget declares and the one a wanted size lands on
// (homeParseSizes, homeSnapToAllowed), where each widget goes
// (homePack), how holes close (homeFillGaps), how many rows of a
// list fit its card (homeFitCount), the rows a growing card sizes
// (homeRowTracks), the size a card takes to fit a space
// (homeFitSpace) and the order a drag intent makes (homeReorder). app.js and home.js run under a
// small DOM stub; only the pure helpers they export for node are
// exercised.
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
  const appPath = require.resolve("../static/app.js");
  delete require.cache[appPath];
  require(appPath);
  const homePath = require.resolve("../static/home.js");
  delete require.cache[homePath];
  return require(homePath);
}

const app = loadAppJS();

test("homeParseSizes: reads a card's declared sizes, narrowest then shortest", function () {
  assert.deepEqual(app.homeParseSizes("2x1 1x2 1x1 junk 9"), [{ w: 1, h: 1 }, { w: 1, h: 2 }, { w: 2, h: 1 }]);
  assert.deepEqual(app.homeParseSizes(""), []);
});

test("homeSnapToAllowed: lands on a declared size by the server's rule", function () {
  const band = app.homeParseSizes("2x1 3x1");
  const table = app.homeParseSizes("2x2 2x3 3x2 3x3");
  const column = app.homeParseSizes("1x1 1x2");
  assert.deepEqual(app.homeSnapToAllowed(table, 3, 2), { w: 3, h: 2 }, "a declared size stands");
  assert.deepEqual(app.homeSnapToAllowed(band, 3, 3), { w: 3, h: 1 }, "same width, nearest height");
  assert.deepEqual(app.homeSnapToAllowed(table, 1, 1), { w: 2, h: 2 }, "the smallest size that covers it");
  assert.deepEqual(app.homeSnapToAllowed(column, 3, 3), { w: 1, h: 2 }, "nothing covers it: the nearest");
  assert.deepEqual(app.homeSnapToAllowed(band, 1, 2), { w: 2, h: 1 }, "nearest, smaller on a tie");
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

test("homePack: each widget takes the first spot it fits after the one before it; nothing moves back into a hole", function () {
  // Three columns: a band, then a tall one beside a square, then a
  // wide one that cannot fit beside them and starts a new row. The
  // square after it does not move back up into the hole the wide one
  // left: it follows the wide one, and the hole stays.
  const got = app.homePack([{ w: 3, h: 1 }, { w: 1, h: 2 }, { w: 1, h: 1 }, { w: 2, h: 2 }, { w: 1, h: 1 }], 3);
  assert.deepEqual(got, [
    { x: 0, y: 0, w: 3, h: 1 },
    { x: 0, y: 1, w: 1, h: 2 },
    { x: 1, y: 1, w: 1, h: 1 },
    { x: 1, y: 2, w: 2, h: 2 },
    { x: 0, y: 3, w: 1, h: 1 },
  ]);
  assert.deepEqual(app.homePack([{ w: 3, h: 1 }, { w: 1, h: 1 }], 2), [{ x: 0, y: 0, w: 2, h: 1 }, { x: 0, y: 1, w: 1, h: 1 }], "a width past the columns draws as the columns");
});

test("homeFillGaps: a hole closes by widening or lengthening, only to an allowed size", function () {
  const sizes = function (list) { return list.split(" ").map(function (s) { return { w: +s[0], h: +s[2] }; }); };
  // A lone square at the end of a three-column grid widens across
  // the hole beside it when it may.
  let placed = [{ x: 0, y: 0, w: 3, h: 1, sizes: sizes("3x1") }, { x: 0, y: 1, w: 1, h: 1, sizes: sizes("1x1 2x1 3x1") }];
  assert.deepEqual(app.homeFillGaps(placed, 3)[1], { x: 0, y: 1, w: 3, h: 1 }, "widened to the row's end");
  placed[1].sizes = sizes("1x1 2x1");
  assert.deepEqual(app.homeFillGaps(placed, 3)[1], { x: 0, y: 1, w: 2, h: 1 }, "as far as an allowed size reaches");
  placed[1].sizes = sizes("1x1");
  assert.deepEqual(app.homeFillGaps(placed, 3)[1], { x: 0, y: 1, w: 1, h: 1 }, "a widget with no wider size stays");
  // A hole under a widget: the one above lengthens into it.
  placed = [
    { x: 0, y: 0, w: 1, h: 2, sizes: sizes("1x2") },
    { x: 1, y: 0, w: 1, h: 1, sizes: sizes("1x1 1x2") },
    { x: 0, y: 2, w: 2, h: 1, sizes: sizes("2x1") },
  ];
  assert.deepEqual(app.homeFillGaps(placed, 2)[1], { x: 1, y: 0, w: 1, h: 2 }, "lengthened into the hole under it");
  // The widget right of a hole widens leftwards when the one on its
  // left cannot grow.
  placed = [
    { x: 0, y: 0, w: 1, h: 1, sizes: sizes("1x1") },
    { x: 2, y: 0, w: 1, h: 1, sizes: sizes("1x1 2x1") },
  ];
  assert.deepEqual(app.homeFillGaps(placed, 3)[1], { x: 1, y: 0, w: 2, h: 1 }, "widened leftwards");
});

test("homeFitCount: the rows whose bottom stays above the limit", function () {
  assert.equal(app.homeFitCount([20, 40, 60, 80], 65), 3);
  assert.equal(app.homeFitCount([20, 40, 60, 80], 80.4), 4, "half a pixel of rounding is forgiven");
  assert.equal(app.homeFitCount([20, 40, 60, 80], 10), 0, "no room, no rows");
  assert.equal(app.homeFitCount([], 100), 0);
});

test("homeRowTracks: rows only growing cards stand in take their height; shared rows keep a fixed row", function () {
  const placed = [{ x: 0, y: 0, w: 3, h: 1 }, { x: 0, y: 1, w: 2, h: 2 }, { x: 2, y: 1, w: 1, h: 1 }, { x: 0, y: 3, w: 1, h: 1 }];
  assert.equal(app.homeRowTracks(placed, [false, false, false, false]), "", "nothing grows: the grid's own rows");
  assert.equal(app.homeRowTracks(placed, [true, false, false, false]), "auto var(--w-row) var(--w-row) var(--w-row)");
  assert.equal(app.homeRowTracks(placed, [false, true, false, false]),
    "var(--w-row) minmax(var(--w-row), auto) auto var(--w-row)", "a row shared with a fixed card keeps its floor");
});

test("homeFitSpace: the allowed size that covers the most of the free cells at a spot", function () {
  const sizes = function (list) { return list.split(" ").map(function (s) { return { w: +s[0], h: +s[2] }; }); };
  // A row of three; the middle cell is free, with a free cell under it.
  const others = [{ x: 0, y: 0, w: 1, h: 2 }, { x: 2, y: 0, w: 1, h: 2 }, { x: 0, y: 2, w: 3, h: 1 }];
  assert.deepEqual(app.homeFitSpace(others, { x: 1, y: 0 }, 3, sizes("1x1 1x2 2x2")), { w: 1, h: 2 }, "grows down into the free cell");
  assert.deepEqual(app.homeFitSpace(others, { x: 1, y: 0 }, 3, sizes("1x1 2x2")), { w: 1, h: 1 }, "shrinks to what the hole holds");
  assert.equal(app.homeFitSpace(others, { x: 1, y: 0 }, 3, sizes("2x1 2x2")), null, "nothing it allows fits");
  assert.deepEqual(app.homeFitSpace([{ x: 0, y: 0, w: 1, h: 1 }], { x: 1, y: 0 }, 3, sizes("1x1 2x1 1x2")), { w: 2, h: 1 },
    "never below the lowest card; the wider of equal sizes");
});

test("homeReorder: a swap trades two places; a move goes beside or before its card", function () {
  const base = ["a", "b", "c", "d"];
  assert.deepEqual(app.homeReorder(base, "a", { kind: "swap", ref: "c" }), ["c", "b", "a", "d"]);
  assert.deepEqual(app.homeReorder(base, "a", { kind: "after", ref: "c" }), ["b", "c", "a", "d"]);
  assert.deepEqual(app.homeReorder(base, "d", { kind: "before", ref: "b" }), ["a", "d", "b", "c"]);
  assert.deepEqual(app.homeReorder(base, "b", { kind: "at", ref: null }), ["a", "c", "d", "b"], "no card after: last");
  assert.deepEqual(base, ["a", "b", "c", "d"], "the base order is untouched");
});

test("homeInsertAt: the place in the order that lands a card on a cell, moving the others least", function () {
  // Three across: two 1x2 cards and a 1x1, so the cell under the 1x1, (2,1), is a hole.
  const others = [{ w: 1, h: 2 }, { w: 1, h: 2 }, { w: 1, h: 1 }, { w: 3, h: 1 }];
  const now = [{ x: 0, y: 0 }, { x: 1, y: 0 }, { x: 2, y: 0 }, { x: 0, y: 2 }];
  assert.equal(app.homeInsertAt(others, now, { w: 1, h: 1 }, { x: 2, y: 1 }, 3), 3, "into the hole, nobody moves");
  assert.equal(app.homeInsertAt(others, now, { w: 2, h: 1 }, { x: 2, y: 1 }, 3), -1, "too wide for the cell's row");
});
