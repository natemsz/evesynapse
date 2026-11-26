// Tests for the My Loadouts delete confirmation dialog in ../static/fit.js:
// submitting a saved-fit delete form opens the native confirmation modal
// with the fit's name, id and return path, and fallback listeners close
// the modal when Invoker Commands or closedby are absent.
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
    this.value = "";
    this.textContent = "";
    this.returnValue = "";
    this.open = false;
    this.children = [];
    this.parentNode = null;
    this.attrs = {};
    this.listeners = {};
    this.rect = { left: 200, top: 200, right: 600, bottom: 400, width: 400, height: 200 };
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
  matches(sel) {
    if (sel.startsWith(".")) return hasClass(this, sel.slice(1));
    if (sel === "form") return this.tagName === "FORM";
    const mInput = sel.match(/^input\[name="([^"]+)"\]$/);
    if (mInput) return this.tagName === "INPUT" && this.getAttribute("name") === mInput[1];
    const mAttr = sel.match(/^\[([^=]+)="([^"]+)"\]$/);
    if (mAttr) return this.getAttribute(mAttr[1]) === mAttr[2];
    return false;
  }
  closest(sel) {
    for (let n = this; n && n.tagName; n = n.parentNode) {
      if (n.matches(sel)) return n;
    }
    return null;
  }
  querySelectorAll(sel) {
    const out = [];
    (function walk(el) {
      for (const c of el.children) {
        if (c.matches(sel)) out.push(c);
        walk(c);
      }
    })(this);
    return out;
  }
  querySelector(sel) {
    return this.querySelectorAll(sel)[0] || null;
  }
  addEventListener(type, fn) {
    (this.listeners[type] = this.listeners[type] || []).push(fn);
  }
  dispatch(type, ev) {
    (this.listeners[type] || []).slice().forEach(function (fn) { fn(ev || {}); });
  }
  getBoundingClientRect() { return this.rect; }
  showModal() { this.open = true; }
  close(val) {
    if (val !== undefined) this.returnValue = String(val);
    this.open = false;
    this.dispatch("close", {});
  }
}

function makeEnv(opts) {
  const options = opts || {};
  const docListeners = {};
  const dlg = new El("dialog");
  dlg.setAttribute("id", "lo-delete-dialog");
  const msg = new El("p");
  msg.setAttribute("id", "lo-delete-msg");
  const closeBtn = new El("button");
  closeBtn.setAttribute("command", "close");
  const cancelBtn = new El("button");
  cancelBtn.setAttribute("command", "close");
  const dlgForm = new El("form");
  const dlgId = new El("input");
  dlgId.setAttribute("name", "id");
  const dlgNext = new El("input");
  dlgNext.setAttribute("name", "next");
  dlgNext.value = "/fittings/saved/";
  dlgForm.appendChild(dlgId);
  dlgForm.appendChild(dlgNext);
  dlgForm.appendChild(cancelBtn);
  dlg.appendChild(closeBtn);
  dlg.appendChild(msg);
  dlg.appendChild(dlgForm);

  function makeCardForm(id, name, next, isPublic) {
    const form = new El("form");
    form.setAttribute("class", "inline-form lo-delete-form");
    form.setAttribute("data-fit-delete", name);
    if (isPublic) form.setAttribute("data-fit-public", "1");
    const idIn = new El("input");
    idIn.setAttribute("name", "id");
    idIn.value = String(id);
    const nextIn = new El("input");
    nextIn.setAttribute("name", "next");
    nextIn.value = next;
    form.appendChild(idIn);
    form.appendChild(nextIn);
    return form;
  }

  const byId = { "lo-delete-dialog": dlg, "lo-delete-msg": msg };
  const doc = {
    addEventListener(type, fn) {
      (docListeners[type] = docListeners[type] || []).push(fn);
    },
    getElementById(id) { return byId[id] || null; },
    createElement(tag) { return new El(tag); },
    querySelector() { return null; },
    querySelectorAll() { return []; },
  };

  global.document = doc;
  global.window = global;
  global.innerWidth = 1024;
  global.innerHeight = 768;
  global.matchMedia = function () { return { matches: false }; };
  global.addEventListener = function () {};
  global.removeEventListener = function () {};
  global.HTMLButtonElement = options.hasInvoker
    ? { prototype: { commandForElement: null } }
    : { prototype: {} };
  global.HTMLDialogElement = options.hasClosedBy
    ? { prototype: { closedBy: "any" } }
    : { prototype: {} };

  const path = require.resolve("../static/fit.js");
  delete require.cache[path];
  require(path);

  function submit(form) {
    let prevented = false;
    const ev = { target: form, preventDefault() { prevented = true; } };
    (docListeners.submit || []).slice().forEach(function (fn) { fn(ev); });
    return prevented;
  }

  return { dlg, msg, dlgId, dlgNext, closeBtn, cancelBtn, makeCardForm, submit };
}

test("submitting a saved-fit delete form opens the modal with fit details", function () {
  const env = makeEnv({ hasInvoker: true, hasClosedBy: true });
  const priv = env.makeCardForm(7, "Harbor Private Rifter", "/fittings/saved/?source=evesynapse", false);
  assert.equal(env.submit(priv), true, "form submit is intercepted for confirmation");
  assert.equal(env.dlg.open, true, "confirmation dialog opens as modal");
  assert.equal(env.dlgId.value, "7");
  assert.equal(env.dlgNext.value, "/fittings/saved/?source=evesynapse");
  assert.match(env.msg.textContent, /Harbor Private Rifter/);
  assert.doesNotMatch(env.msg.textContent, /public page/i);

  const pub = env.makeCardForm(9, "Harbor Public Rifter", "/fittings/saved/?source=public", true);
  assert.equal(env.submit(pub), true);
  assert.equal(env.dlgId.value, "9");
  assert.equal(env.dlgNext.value, "/fittings/saved/?source=public");
  assert.match(env.msg.textContent, /public page/i);

  env.dlg.close("cancel");
  assert.equal(env.dlg.returnValue, "", "returnValue resets after close");
});

test("fallback listeners close the modal when Invoker Commands and closedby are absent", function () {
  const env = makeEnv({ hasInvoker: false, hasClosedBy: false });
  const form = env.makeCardForm(12, "Fallback Rifter", "/fittings/saved/", false);
  env.submit(form);
  assert.equal(env.dlg.open, true);

  env.cancelBtn.dispatch("click");
  assert.equal(env.dlg.open, false, "Cancel closes dialog via fallback when commandForElement is missing");

  env.submit(form);
  assert.equal(env.dlg.open, true);
  // Drag starting inside and ending on backdrop must not dismiss.
  env.dlg.dispatch("pointerdown", { clientX: 300, clientY: 300 });
  env.dlg.dispatch("pointerup", { clientX: 50, clientY: 50 });
  assert.equal(env.dlg.open, true, "drag from inside to backdrop does not dismiss");

  // Press and release outside the dialog box dismisses.
  env.dlg.dispatch("pointerdown", { clientX: 50, clientY: 50 });
  env.dlg.dispatch("pointerup", { clientX: 50, clientY: 50 });
  assert.equal(env.dlg.open, false, "click outside dialog box dismisses via fallback");
});
