// Tests for the read-only module tooltip in ../static/fit.js:
// on a public fit's page (a .fit-ro workbench, no editor),
// hovering or tapping a module shows the shared themed
// tooltip — name, the server's computed stat rows with their
// good/bad classes, the loaded charge — and never state
// buttons. Driven through a small DOM stub; fit.js's editor
// block bails in this environment (no #fit-editor), which is
// exactly the public-page situation.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");

function hasClass(el, cls) {
  return (el.className || "").split(/\s+/).indexOf(cls) >= 0;
}

function matchesSimple(el, sel) {
  if (!el || !el.tagName) return false;
  if (sel.charAt(0) === ".") return hasClass(el, sel.slice(1));
  if (sel.charAt(0) === "#") return el.id === sel.slice(1);
  if (sel === "[data-tip-name]") return el.getAttribute("data-tip-name") !== null;
  return false;
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
    this.offsetWidth = 0;
    this.offsetHeight = 0;
    this._text = "";
    this._listeners = {};
  }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
    if (k === "class") this.className = String(v);
    if (k === "id") this.id = String(v);
  }
  getAttribute(k) {
    return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null;
  }
  appendChild(c) {
    c.parentNode = this;
    this.children.push(c);
    return c;
  }
  removeChild(c) {
    const i = this.children.indexOf(c);
    if (i >= 0) this.children.splice(i, 1);
    c.parentNode = null;
    return c;
  }
  addEventListener(type, fn) {
    (this._listeners[type] = this._listeners[type] || []).push(fn);
  }
  set textContent(v) {
    this._text = String(v);
    this.children = [];
  }
  get textContent() {
    let out = this._text;
    for (const c of this.children) out += c.textContent;
    return out;
  }
  matches(sel) {
    return sel === ":hover" ? false : matchesSimple(this, sel);
  }
  closest(sel) {
    // The one descendant selector the read-only wiring uses.
    if (sel === ".fit-ro [data-tip-name]") {
      let n = this;
      while (n) {
        if (matchesSimple(n, "[data-tip-name]")) {
          let a = n.parentNode;
          while (a) {
            if (hasClass(a, "fit-ro")) return n;
            a = a.parentNode;
          }
        }
        n = n.parentNode;
      }
      return null;
    }
    let n = this;
    while (n) {
      if (matchesSimple(n, sel)) return n;
      n = n.parentNode;
    }
    return null;
  }
  querySelectorAll() { return []; }
  getBoundingClientRect() {
    return { left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 };
  }
}

function findByClass(el, cls, out) {
  out = out || [];
  if (hasClass(el, cls)) out.push(el);
  for (const c of el.children) findByClass(c, cls, out);
  return out;
}

function makeEnv() {
  const listeners = {};
  const wb = new El("div");
  wb.className = "fit-wb fit-ro";
  const slot = new El("span");
  slot.className = "fit-vslot filled";
  slot.setAttribute("data-tip-name", "Fixture Blaster");
  slot.setAttribute("data-tip-sub", "High Slots");
  slot.setAttribute("data-tip-meta", "Tech II");
  slot.setAttribute("data-tip-lines", "DPS|20|good;CPU|15 tf|");
  slot.setAttribute("data-tip-charge", "Loaded: Fixture Charge S");
  wb.appendChild(slot);
  const doc = {
    _listeners: listeners,
    addEventListener(type, fn) {
      (listeners[type] = listeners[type] || []).push(fn);
    },
    getElementById() { return null; },
    createElement(tag) { return new El(tag); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };
  function fire(type, ev) {
    (listeners[type] || []).slice().forEach(function (fn) { fn(ev); });
  }
  return { doc, wb, slot, fire };
}

function ev(target, extra) {
  return Object.assign({
    target: target,
    relatedTarget: null,
    key: "",
    stopPropagation() {},
    preventDefault() {},
  }, extra || {});
}

function loadFitJS(env) {
  global.document = env.doc;
  global.window = global;
  const path = require.resolve("../static/fit.js");
  delete require.cache[path];
  require(path);
}

function delay(ms) {
  return new Promise(function (resolve) { setTimeout(resolve, ms); });
}

test("read-only hover shows the themed tooltip, info only", function () {
  const env = makeEnv();
  loadFitJS(env);
  env.fire("mouseover", ev(env.slot));
  const tips = findByClass(env.wb, "fit-tip");
  assert.equal(tips.length, 1, "one tooltip in the workbench");
  const tip = tips[0];
  assert.equal(findByClass(tip, "fit-tip-name")[0].textContent, "Fixture Blaster");
  const rows = findByClass(tip, "fit-tip-line");
  assert.equal(rows.length, 2, "two stat rows");
  assert.equal(findByClass(rows[0], "fit-tip-k")[0].textContent, "DPS");
  const v0 = findByClass(rows[0], "fit-tip-v")[0];
  assert.equal(v0.textContent, "20");
  assert.ok(hasClass(v0, "good"), "improved value carries the good class");
  const v1 = findByClass(rows[1], "fit-tip-v")[0];
  assert.equal(v1.textContent, "15 tf");
  assert.ok(!hasClass(v1, "good") && !hasClass(v1, "bad"), "unchanged value stays neutral");
  assert.ok(tip.textContent.indexOf("Loaded: Fixture Charge S") >= 0, "charge line present");
  assert.equal(findByClass(tip, "fit-tip-state").length, 0, "no state buttons on a read-only surface");
});

test("read-only tooltip dismisses on leave, toggles on tap, closes on Escape", async function () {
  const env = makeEnv();
  loadFitJS(env);
  env.fire("mouseover", ev(env.slot));
  assert.equal(findByClass(env.wb, "fit-tip").length, 1);
  env.fire("mouseout", ev(env.slot));
  await delay(220);
  assert.equal(findByClass(env.wb, "fit-tip").length, 0, "gone after the close delay");
  env.fire("click", ev(env.slot));
  assert.equal(findByClass(env.wb, "fit-tip").length, 1, "tap shows it");
  env.fire("click", ev(env.slot));
  assert.equal(findByClass(env.wb, "fit-tip").length, 0, "second tap hides it");
  env.fire("click", ev(env.slot));
  assert.equal(findByClass(env.wb, "fit-tip").length, 1);
  env.fire("keydown", ev(env.wb, { key: "Escape" }));
  assert.equal(findByClass(env.wb, "fit-tip").length, 0, "Escape closes it");
});
