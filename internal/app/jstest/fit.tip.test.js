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

// --- editor workbench -------------------------------------------
// The editor block runs only with #fit-editor present, so these
// tests drive it through a fuller stub: selectors with descendant
// and attribute parts, capture/bubble dispatch, and a simulate
// round trip the test releases by hand.

function parseCompound(s) {
  const c = { tag: "", cls: [], id: "", attrs: [], hover: false };
  const re = /^[a-zA-Z*]+|\.[\w-]+|#[\w-]+|\[([\w-]+)(?:="([^"]*)")?\]|:hover/g;
  let m;
  while ((m = re.exec(s))) {
    const t = m[0];
    if (t === ":hover") c.hover = true;
    else if (t[0] === ".") c.cls.push(t.slice(1));
    else if (t[0] === "#") c.id = t.slice(1);
    else if (t[0] === "[") c.attrs.push({ k: m[1], v: m[2] });
    else if (t !== "*") c.tag = t.toUpperCase();
  }
  return c;
}

function matchCompound(el, c) {
  if (!el || !el.tagName) return false;
  if (c.tag && el.tagName !== c.tag) return false;
  if (c.id && el.id !== c.id) return false;
  for (const k of c.cls) if (!hasClass(el, k)) return false;
  for (const a of c.attrs) {
    const v = el.getAttribute(a.k);
    if (v === null || (a.v !== undefined && v !== a.v)) return false;
  }
  if (c.hover && !el.hovered) return false;
  return true;
}

function matchSel(el, sel) {
  return sel.split(",").some(function (part) {
    // Descendant steps split on spaces outside [attr="..."] values.
    const parts = part.trim().match(/(?:[^\s[]|\[[^\]]*\])+/g).map(parseCompound);
    if (!matchCompound(el, parts[parts.length - 1])) return false;
    let n = el.parentNode;
    for (let i = parts.length - 2; i >= 0; i--) {
      while (n && !matchCompound(n, parts[i])) n = n.parentNode;
      if (!n) return false;
      n = n.parentNode;
    }
    return true;
  });
}

class EEl {
  constructor(tag, env) {
    this.tagName = (tag || "div").toUpperCase();
    this.env = env;
    this.className = "";
    this.id = "";
    this.children = [];
    this.parentNode = null;
    this.attrs = {};
    this.style = {};
    this.hidden = false;
    this.value = "";
    this.offsetWidth = 0;
    this.offsetHeight = 0;
    this.rect = { left: 0, top: 0, width: 0, height: 0 };
    this._text = "";
    this._listeners = [];
    const self = this;
    this.classList = {
      add(k) { if (!hasClass(self, k)) self.className = (self.className + " " + k).trim(); },
      remove(k) { self.className = self.className.split(/\s+/).filter(function (x) { return x && x !== k; }).join(" "); },
      contains(k) { return hasClass(self, k); },
      toggle(k, on) { if (on === undefined ? !hasClass(self, k) : on) this.add(k); else this.remove(k); },
    };
  }
  setAttribute(k, v) {
    this.attrs[k] = String(v);
    if (k === "class") this.className = String(v);
    if (k === "id") this.id = String(v);
  }
  getAttribute(k) {
    if (k === "class") return this.className;
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
    const i = this.children.indexOf(c);
    if (i >= 0) this.children.splice(i, 1);
    c.parentNode = null;
    return c;
  }
  replaceWith(n) {
    const p = this.parentNode;
    const i = p.children.indexOf(this);
    p.children[i] = n;
    n.parentNode = p;
    this.parentNode = null;
  }
  contains(n) {
    for (; n; n = n.parentNode) if (n === this) return true;
    return false;
  }
  addEventListener(type, fn, capture) {
    this._listeners.push({ type: type, fn: fn, capture: !!capture });
  }
  set textContent(v) { this._text = String(v); this.children = []; }
  get textContent() {
    let out = this._text;
    for (const c of this.children) out += c.textContent;
    return out;
  }
  set innerHTML(v) {
    this.children = [];
    this._text = "";
    if (this.env && this.env.html[v]) this.appendChild(this.env.html[v]());
  }
  matches(sel) { return matchSel(this, sel); }
  closest(sel) {
    for (let n = this; n && n.tagName; n = n.parentNode) if (matchSel(n, sel)) return n;
    return null;
  }
  querySelectorAll(sel) {
    const out = [];
    (function walk(n) {
      for (const c of n.children) {
        if (matchSel(c, sel)) out.push(c);
        walk(c);
      }
    })(this);
    return out;
  }
  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
  getBoundingClientRect() {
    const r = this.rect;
    return { left: r.left, top: r.top, width: r.width, height: r.height, right: r.left + r.width, bottom: r.top + r.height };
  }
  focus() {}
  setPointerCapture() {}
}

// A ring of slots like the workbench renders: a 544px visual,
// slots 46px across on a 239px radius around its center.
const RING = { size: 544, radius: 239, slot: 46 };

function ringSlot(env, angleDeg, attrs) {
  const s = new EEl("button", env);
  const a = (angleDeg * Math.PI) / 180;
  const cx = RING.size / 2 + RING.radius * Math.cos(a);
  const cy = RING.size / 2 + RING.radius * Math.sin(a);
  s.rect = { left: cx - RING.slot / 2, top: cy - RING.slot / 2, width: RING.slot, height: RING.slot };
  for (const k of Object.keys(attrs)) s.setAttribute(k, attrs[k]);
  return s;
}

function buildWorkbench(env, ro) {
  const wb = new EEl("div", env);
  wb.className = ro ? "fit-wb fit-ro" : "fit-wb";
  wb.rect = { left: 0, top: 0, width: 900, height: 700 };
  wb.setAttribute("data-fit-state", JSON.stringify({
    name: "Fixture", shipTypeId: 1,
    items: [{ typeId: 100, qty: 1 }, { typeId: 200, qty: 1 }], charges: {},
  }));
  const visual = new EEl("div", env);
  visual.className = "fit-visual";
  visual.rect = { left: 0, top: 0, width: RING.size, height: RING.size };
  wb.appendChild(visual);
  const filled = function (angle, typeID, name, idx) {
    const s = ringSlot(env, angle, {
      class: "fit-vslot filled", "data-fit-vslot": typeID, "data-v-index": idx, "data-v-family": "high",
      "data-tip-key": typeID + ":0", "data-tip-name": name, "data-tip-sub": "High Slots",
      "data-tip-states": "offline,online,active,overheated", "data-tip-state": "active",
    });
    const img = new EEl("img", env);
    s.appendChild(img);
    visual.appendChild(s);
    return s;
  };
  filled(-100, 100, "Alpha Blaster", 0);
  filled(-80, 200, "Beta Blaster", 1);
  const empty = ringSlot(env, -60, {
    class: "fit-vslot", "data-fit-add": "high", "data-v-index": 2, "data-v-family": "high", title: "Add High Slot",
  });
  const plus = new EEl("span", env);
  plus.setAttribute("aria-hidden", "true");
  plus.textContent = "+";
  empty.appendChild(plus);
  visual.appendChild(empty);
  for (let i = 0; i < 9; i++) {
    visual.appendChild(ringSlot(env, -40 + i * 40, { class: "fit-vslot", "data-fit-add": "med", "data-v-index": i, "data-v-family": "med" }));
  }
  return wb;
}

function makeEditorEnv() {
  const env = { html: {}, sims: [] };
  const root = new EEl("html", env);
  const body = new EEl("body", env);
  root.appendChild(body);
  const editor = new EEl("div", env);
  editor.id = "fit-editor";
  body.appendChild(editor);
  const picker = new EEl("div", env);
  picker.id = "fit-picker";
  picker.hidden = true;
  editor.appendChild(picker);
  editor.appendChild(buildWorkbench(env, false));
  env.html.SIM = function () { return buildWorkbench(env, false); };
  const docListeners = [];
  const doc = {
    body: body,
    addEventListener(type, fn, capture) { docListeners.push({ type: type, fn: fn, capture: !!capture }); },
    getElementById(id) {
      if (root.id === id) return root;
      return root.querySelector("#" + id);
    },
    createElement(tag) { return new EEl(tag, env); },
    querySelector(sel) { return root.querySelector(sel); },
    querySelectorAll(sel) { return root.querySelectorAll(sel); },
  };
  // Capture from the document down, then the target, then bubble.
  function dispatch(target, type, extra) {
    let stopped = false;
    const e = Object.assign({
      type: type, target: target, relatedTarget: null, key: "", button: 0, pointerId: 1,
      clientX: 0, clientY: 0,
      stopPropagation() { stopped = true; },
      preventDefault() {},
    }, extra || {});
    const path = [];
    for (let n = target; n; n = n.parentNode) path.push(n);
    const run = function (list, capture) {
      list.filter(function (l) { return l.type === type && (capture === null || l.capture === capture); })
        .forEach(function (l) { if (!stopped) l.fn(e); });
    };
    run(docListeners, true);
    for (let i = path.length - 1; i > 0 && !stopped; i--) run(path[i]._listeners, true);
    if (!stopped) run(target._listeners, null);
    for (let i = 1; i < path.length && !stopped; i++) run(path[i]._listeners, false);
    if (!stopped) run(docListeners, false);
  }
  global.document = doc;
  global.window = global;
  // Each simulate POST waits until the test releases it.
  global.fetch = function (url) {
    if (String(url).indexOf("/fittings/simulate/") !== 0) {
      return Promise.resolve({ ok: true, json() { return Promise.resolve([]); }, text() { return Promise.resolve(""); } });
    }
    return new Promise(function (resolve) {
      env.sims.push(function () {
        resolve({ ok: true, text() { return Promise.resolve("SIM"); } });
      });
    });
  };
  const path = require.resolve("../static/fit.js");
  delete require.cache[path];
  require(path);
  env.editor = editor;
  env.picker = picker;
  env.dispatch = dispatch;
  env.wb = function () { return editor.querySelector(".fit-wb"); };
  env.slot = function (name) { return env.wb().querySelector('.fit-vslot[data-tip-name="' + name + '"]'); };
  env.tips = function () { return findByClass(editor, "fit-tip"); };
  env.tipName = function () {
    const t = env.tips();
    return t.length ? findByClass(t[0], "fit-tip-name")[0].textContent : "";
  };
  env.hover = function (from, to) {
    if (from) dispatch(from, "mouseout", { relatedTarget: to });
    if (to) dispatch(to, "mouseover", { relatedTarget: from });
  };
  env.tap = function (el) {
    dispatch(el, "pointerdown");
    dispatch(el, "pointerup");
    dispatch(el, "click");
  };
  env.release = async function () {
    const r = env.sims.shift();
    assert.ok(r, "a simulate request is in flight");
    r();
    await delay(5);
  };
  return env;
}

// Longer than the editor's tooltip close delay.
const PAST_CLOSE = 480;

test("an empty slot opens the picker from anywhere on it, not just its border", function () {
  const env = makeEditorEnv();
  const empty = env.wb().querySelector('.fit-vslot[data-fit-add="high"]');
  env.tap(empty.children[0]); // the "+" glyph that fills the slot's middle
  assert.equal(env.picker.hidden, false, "a click on the slot's glyph opens the picker");
  env.picker.hidden = true;
  env.tap(empty);
  assert.equal(env.picker.hidden, false, "a click on the slot itself still opens it");
});

test("moving from one module to the next keeps the next module's tooltip up", async function () {
  const env = makeEditorEnv();
  const a = env.slot("Alpha Blaster");
  const b = env.slot("Beta Blaster");
  env.hover(null, a);
  assert.equal(env.tipName(), "Alpha Blaster");
  env.hover(a, b);
  assert.equal(env.tipName(), "Beta Blaster");
  await delay(PAST_CLOSE);
  assert.equal(env.tips().length, 1, "A's close timer must not take B's tooltip down");
  assert.equal(env.tipName(), "Beta Blaster");
  env.hover(b, null);
  await delay(PAST_CLOSE);
  assert.equal(env.tips().length, 0, "leaving B still closes its tooltip");
});

test("a state change re-anchors the open tooltip and leaves none orphaned", async function () {
  const env = makeEditorEnv();
  env.hover(null, env.slot("Alpha Blaster"));
  const overheat = env.tips()[0].querySelectorAll(".fit-tip-state").filter(function (b) {
    return b.textContent === "Overheat";
  })[0];
  env.dispatch(overheat, "click");
  await env.release();
  assert.equal(env.tips().length, 1, "one tooltip after the re-render");
  assert.equal(env.tipName(), "Alpha Blaster");
  assert.equal(env.tips()[0].parentNode, env.wb(), "it lives in the new workbench");

  // The pointer moves on to B while the re-simulate is in flight.
  const oldB = env.slot("Beta Blaster");
  env.dispatch(env.tips()[0].querySelectorAll(".fit-tip-state")[0], "click");
  env.hover(env.slot("Alpha Blaster"), oldB);
  await env.release();
  assert.notEqual(env.tipName(), "Alpha Blaster", "A's tooltip must not come back over B");
  env.hover(env.slot("Beta Blaster"), null);
  await delay(PAST_CLOSE);
  assert.equal(env.tips().length, 0, "nothing is left stuck on screen after the pointer leaves");
});

function overlaps(a, b) {
  return a.left < b.left + b.width && b.left < a.left + a.width &&
    a.top < b.top + b.height && b.top < a.top + a.height;
}

test("a ring tooltip covers no module, in the editor and on a public fit", function () {
  const env = makeEditorEnv();
  const check = function (wb, slot, where) {
    const tip = findByClass(wb, "fit-tip")[0];
    assert.ok(tip, where + ": a tooltip is open");
    const box = { left: parseFloat(tip.style.left), top: parseFloat(tip.style.top), width: 190, height: 110 };
    for (const s of wb.querySelectorAll(".fit-vslot")) {
      assert.ok(!overlaps(box, s.rect), where + ": the tooltip for " + slot + " covers a slot at " + JSON.stringify(s.rect));
    }
  };
  env.hover(null, env.slot("Alpha Blaster"));
  check(env.wb(), "Alpha Blaster", "editor");

  // The public page: same workbench, read-only, no editor.
  const ro = buildWorkbench(env, true);
  const page = new EEl("main", env);
  page.appendChild(ro);
  env.editor.parentNode.appendChild(page);
  env.editor.parentNode.removeChild(env.editor);
  env.dispatch(ro.querySelector('.fit-vslot[data-tip-name="Beta Blaster"]'), "mouseover");
  check(ro, "Beta Blaster", "public fit");
});
