// Tests for the phone bottom bar in ../static/app.js: the
// Sections button opens the sections sheet (and the backdrop,
// Escape, a close button, and choosing a link all close it),
// the bar and sheet mark the current section — the Sections
// button itself when the section lives only in the sheet — and
// a content swap re-marks them from the sidebar state swap.js
// synced. app.js runs under a small DOM stub carrying the shell
// pieces the bar reads and writes, plus the pure helper app.js
// exports for node (phoneSectionState).
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

// The shell as base.html renders it: the bar (Home, the slotted
// sections, the Sections button), the sheet with its section
// rows and group links, and the sidebar state the bar syncs
// from (an active branch, or an active top-level link, plus the
// current page's active menu link).
function makeEnv(opts) {
  opts = opts || {};
  FakeRO.live = [];
  const docListeners = {};
  const winListeners = {};

  function barItem(section, href) {
    const a = new El("a");
    a.className = "phone-bar-item";
    a.setAttribute("data-section", section);
    if (href) a.setAttribute("href", href);
    return a;
  }
  const sectionsBtn = new El("button");
  sectionsBtn.className = "phone-bar-item";
  sectionsBtn.id = "phone-sections-btn";
  sectionsBtn.setAttribute("data-section", "sections");
  sectionsBtn.setAttribute("aria-expanded", "false");
  const barItems = [
    barItem("home", "/"), barItem("pilot", "/character/"),
    barItem("industry", "/industry/"), barItem("market", "/market/"),
    sectionsBtn,
  ];

  const sheet = new El("section");
  sheet.id = "phone-sheet";
  sheet.hidden = true;
  const backdrop = new El("div");
  backdrop.id = "phone-sheet-backdrop";
  backdrop.hidden = true;
  const closeBtn = new El("button");
  closeBtn.id = "phone-sheet-close";
  const sheetLink = new El("a");
  sheetLink.setAttribute("href", "/market/");
  sheet.appendChild(sheetLink);

  const sheetSections = ["pilot", "fitting", "industry", "market", "corporation", "intel", "tools"].map(function (s) {
    const a = new El("a");
    a.className = "phone-sheet-section";
    a.setAttribute("data-section", s);
    a.setAttribute("href", "/" + s + "/");
    return a;
  });
  const groupLinks = ["/orders/", "/assets/"].map(function (h) {
    const a = new El("a");
    a.setAttribute("href", h);
    return a;
  });

  const branches = ["pilot", "industry", "market", "corporation"].map(function (s) {
    const d = new El("details");
    d.className = "branch" + (opts.branch === s ? " active" : "");
    d.setAttribute("data-nav-category", s);
    return d;
  });
  let navlinkActive = null;
  if (opts.navlink) {
    navlinkActive = new El("a");
    navlinkActive.className = "navlink active";
    navlinkActive.setAttribute("href", opts.navlink);
  }
  let menuActive = null;
  if (opts.menu) {
    menuActive = new El("a");
    menuActive.className = "active";
    menuActive.setAttribute("href", opts.menu);
  }

  const byId = {
    "phone-sections-btn": sectionsBtn,
    "phone-sheet": sheet,
    "phone-sheet-backdrop": backdrop,
    "phone-sheet-close": closeBtn,
  };
  const doc = {
    _listeners: docListeners,
    addEventListener(type, fn) { (docListeners[type] = docListeners[type] || []).push(fn); },
    removeEventListener() {},
    getElementById(id) { return byId[id] || null; },
    querySelector(sel) {
      if (sel.indexOf("a.navlink.active") >= 0) return navlinkActive;
      if (sel.indexOf(".menu a.active") >= 0) return menuActive;
      return null;
    },
    querySelectorAll(sel) {
      if (sel.indexOf("phone-bar-item") >= 0) return barItems;
      if (sel.indexOf("phone-sheet-section") >= 0) return sheetSections;
      if (sel.indexOf("phone-sheet-groups") >= 0) return groupLinks;
      if (sel.indexOf("details.branch") >= 0) return branches;
      return [];
    },
    createElement(tag) { return new El(tag); },
    documentElement: new El("html"),
    body: new El("body"),
    hidden: false,
    fonts: { ready: Promise.resolve() },
  };
  function fireDoc(type, extra) {
    const ev = Object.assign({
      target: new El("div"), key: "", detail: null, stopPropagation() {}, preventDefault() {},
    }, extra || {});
    (docListeners[type] || []).slice().forEach(function (fn) { fn(ev); });
  }
  return {
    doc, fireDoc, sectionsBtn, sheet, backdrop, closeBtn, sheetLink,
    barItems, sheetSections, groupLinks, branches,
    setBranch(section) {
      branches.forEach(function (b) {
        b.classList.toggle("active", b.getAttribute("data-nav-category") === section);
      });
    },
    setMenu(href) {
      menuActive = null;
      if (href) {
        menuActive = new El("a");
        menuActive.className = "active";
        menuActive.setAttribute("href", href);
      }
    },
  };
}

function loadAppJS(env) {
  global.document = env.doc;
  global.window = global;
  global.innerWidth = 390;
  global.innerHeight = 844;
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

const BAR_SECTIONS = ["home", "pilot", "industry", "market"];
const SHEET_SECTIONS = ["pilot", "fitting", "industry", "market", "corporation", "intel", "tools"];

test("phoneSectionState: a slotted section marks its own slot", function () {
  const m = loadAppJS(makeEnv({}));
  assert.deepEqual(m.phoneSectionState("market", BAR_SECTIONS, SHEET_SECTIONS),
    { bar: "market", sectionsButton: false });
  assert.deepEqual(m.phoneSectionState("home", BAR_SECTIONS, SHEET_SECTIONS),
    { bar: "home", sectionsButton: false });
});

test("phoneSectionState: a sheet-only section marks the Sections button", function () {
  const m = loadAppJS(makeEnv({}));
  assert.deepEqual(m.phoneSectionState("fitting", BAR_SECTIONS, SHEET_SECTIONS),
    { bar: "", sectionsButton: true });
  assert.deepEqual(m.phoneSectionState("corporation", BAR_SECTIONS, SHEET_SECTIONS),
    { bar: "", sectionsButton: true });
});

test("phoneSectionState: a section in neither marks nothing", function () {
  const m = loadAppJS(makeEnv({}));
  assert.deepEqual(m.phoneSectionState("sync", BAR_SECTIONS, SHEET_SECTIONS),
    { bar: "", sectionsButton: false });
  assert.deepEqual(m.phoneSectionState("", BAR_SECTIONS, SHEET_SECTIONS),
    { bar: "", sectionsButton: false });
});

test("the Sections button opens the sheet; backdrop, close and Escape close it", function () {
  const env = makeEnv({ branch: "market", menu: "/market/" });
  loadAppJS(env);
  assert.equal(env.sheet.hidden, true);

  fireEl(env.sectionsBtn, "click");
  assert.equal(env.sheet.hidden, false);
  assert.equal(env.backdrop.hidden, false);
  assert.equal(env.sectionsBtn.getAttribute("aria-expanded"), "true");
  assert.equal(env.closeBtn.focused, true);

  fireEl(env.backdrop, "click");
  assert.equal(env.sheet.hidden, true);
  assert.equal(env.backdrop.hidden, true);
  assert.equal(env.sectionsBtn.getAttribute("aria-expanded"), "false");

  fireEl(env.sectionsBtn, "click");
  assert.equal(env.sheet.hidden, false);
  env.fireDoc("keydown", { key: "Escape" });
  assert.equal(env.sheet.hidden, true);

  fireEl(env.sectionsBtn, "click");
  assert.equal(env.sheet.hidden, false);
  fireEl(env.closeBtn, "click");
  assert.equal(env.sheet.hidden, true);
});

test("choosing a link in the sheet closes it", function () {
  const env = makeEnv({ branch: "market", menu: "/market/" });
  loadAppJS(env);
  fireEl(env.sectionsBtn, "click");
  assert.equal(env.sheet.hidden, false);
  fireEl(env.sheet, "click", { target: env.sheetLink });
  assert.equal(env.sheet.hidden, true);
  assert.equal(env.backdrop.hidden, true);
});

test("the bar and sheet mark the current section on load", function () {
  const env = makeEnv({ branch: "market", menu: "/orders/" });
  loadAppJS(env);
  const market = env.barItems.find(function (i) { return i.getAttribute("data-section") === "market"; });
  assert.ok(market.classList.contains("active"));
  assert.equal(market.getAttribute("aria-current"), "page");
  const pilot = env.barItems.find(function (i) { return i.getAttribute("data-section") === "pilot"; });
  assert.ok(!pilot.classList.contains("active"));
  assert.ok(!env.sectionsBtn.classList.contains("active"));
  const sheetMarket = env.sheetSections.find(function (s) { return s.getAttribute("data-section") === "market"; });
  assert.ok(sheetMarket.classList.contains("active"));
  assert.equal(sheetMarket.getAttribute("aria-current"), "page");
  const orders = env.groupLinks.find(function (g) { return g.getAttribute("href") === "/orders/"; });
  assert.ok(orders.classList.contains("active"));
});

test("a swap to a sheet-only section moves the mark to the Sections button", function () {
  const env = makeEnv({ branch: "market", menu: "/market/" });
  loadAppJS(env);
  env.setBranch("corporation");
  env.setMenu("/corporations/");
  env.fireDoc("evesynapse:content-ready", { detail: { root: env.doc } });
  assert.ok(env.sectionsBtn.classList.contains("active"));
  const market = env.barItems.find(function (i) { return i.getAttribute("data-section") === "market"; });
  assert.ok(!market.classList.contains("active"));
  assert.equal(market.getAttribute("aria-current"), null);
  const corp = env.sheetSections.find(function (s) { return s.getAttribute("data-section") === "corporation"; });
  assert.ok(corp.classList.contains("active"));
  const sheetMarket = env.sheetSections.find(function (s) { return s.getAttribute("data-section") === "market"; });
  assert.ok(!sheetMarket.classList.contains("active"));
});
