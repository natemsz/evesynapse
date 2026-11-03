// Tests for the Assets matrix row initializer in ../static/app.js:
// a matrix row click selects its item, clicks on real controls
// inside the row pass, and a row is wired at most once. app.js
// runs under a small DOM stub.
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
    this.isConnected = true;
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
    if (k === "id") this.id = String(v);
  }
  getAttribute(k) {
    return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null;
  }
  hasAttribute(k) { return Object.prototype.hasOwnProperty.call(this.attrs, k); }
  removeAttribute(k) { delete this.attrs[k]; }
  appendChild(c) { c.parentNode = this; this.children.push(c); return c; }
  removeChild(c) { return c; }
  addEventListener(type, fn) {
    (this._listeners[type] = this._listeners[type] || []).push(fn);
  }
  removeEventListener() {}
  focus() {}
  querySelector() { return null; }
  querySelectorAll() { return []; }
}

class FakeRO {
  constructor(cb) { this.cb = cb; FakeRO.live.push(this); }
  observe() {}
  disconnect() {}
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

function fakeRow(href) {
  const row = new El("tr");
  row.setAttribute("data-sel", href);
  return row;
}

function rootWith(rows) {
  return {
    querySelectorAll(sel) {
      return sel === ".amx-trow[data-sel]" ? rows : [];
    },
  };
}

function click(row, onControl) {
  const ev = { target: { closest() { return onControl ? {} : null; } } };
  for (const fn of row._listeners.click || []) fn(ev);
}

test("initMatrixRows: a row click opens the row's item", function () {
  const row = fakeRow("/assets/item/34");
  app.initMatrixRows(rootWith([row]));
  global.location.href = "https://example.test/assets/";
  click(row, false);
  assert.equal(global.location.href, "/assets/item/34");
});

test("initMatrixRows: clicks on controls inside the row pass", function () {
  const row = fakeRow("/assets/item/34");
  app.initMatrixRows(rootWith([row]));
  global.location.href = "https://example.test/assets/";
  click(row, true);
  assert.equal(global.location.href, "https://example.test/assets/");
});

test("initMatrixRows: a row is wired at most once", function () {
  const row = fakeRow("/assets/item/34");
  app.initMatrixRows(rootWith([row]));
  app.initMatrixRows(rootWith([row]));
  assert.equal((row._listeners.click || []).length, 1);
});
