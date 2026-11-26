// Tests for EveSynapseContent.iskCompact in ../static/app.js, the
// browser's copy of Go's humanize.ISKCompact: the same cases as
// internal/humanize/isk_test.go give the same strings. app.js loads
// under a do-nothing DOM stub; only the helper is exercised.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");

function stubEl() {
  return {
    addEventListener() {}, removeEventListener() {}, setAttribute() {}, getAttribute() { return null; },
    hasAttribute() { return false; }, removeAttribute() {}, appendChild(c) { return c; },
    querySelector() { return null; }, querySelectorAll() { return []; }, closest() { return null; },
    classList: { add() {}, remove() {}, toggle() {}, contains() { return false; } },
    style: {}, dataset: {}, children: [],
  };
}

function loadApp() {
  const doc = stubEl();
  doc.documentElement = stubEl();
  doc.body = stubEl();
  doc.head = stubEl();
  doc.readyState = "complete";
  doc.createElement = stubEl;
  doc.createTextNode = function (t) { return { textContent: t }; };
  doc.getElementById = function () { return null; };
  global.document = doc;
  global.window = global;
  global.location = { origin: "https://example.test", pathname: "/", href: "https://example.test/", search: "" };
  Object.defineProperty(global, "navigator", { value: { userAgent: "node", platform: "node" }, configurable: true });
  global.localStorage = { getItem() { return null; }, setItem() {}, removeItem() {} };
  global.sessionStorage = global.localStorage;
  global.matchMedia = function () { return { matches: false, addEventListener() {}, addListener() {} }; };
  global.requestAnimationFrame = function () { return 0; };
  global.addEventListener = function () {};
  global.removeEventListener = function () {};
  const path = require.resolve("../static/app.js");
  delete require.cache[path];
  require(path);
  return global.window.EveSynapseContent;
}

test("iskCompact matches Go's humanize.ISKCompact", function () {
  const isk = loadApp().iskCompact;
  const cases = [
    [0, "0"], [940.4, "940"], [999.4, "999"],
    [1000, "1.00K"], [940000, "940.00K"], [999996, "1.00M"],
    [312400000, "312.40M"], [1274000000, "1.27B"], [4210000000000, "4.21T"],
    [-1740000000, "-1.74B"], [-0.2, "0"],
  ];
  for (const [v, want] of cases) assert.equal(isk(v), want, "iskCompact(" + v + ")");
});
