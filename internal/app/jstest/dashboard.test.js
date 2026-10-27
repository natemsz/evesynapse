"use strict";
// The intel dashboard's pure parts: which threads ping (dashboard.js)
// and the ping's timeline on the map (map.js pingRing): one plain
// echo, five softer pulses, then a slow faint breath.
const assert = require("node:assert");
const test = require("node:test");
const path = require("node:path");

const dash = require(path.join(__dirname, "..", "static", "dashboard.js"));
const map = require(path.join(__dirname, "..", "static", "map.js"));

test("a thread pings while recent and unopened", () => {
  const row = { id: 7, report: 70, at: 1000 };
  assert.strictEqual(dash.shouldPing(row, {}, 1100, 1800), true);
  assert.strictEqual(dash.shouldPing(row, { 7: 70 }, 1100, 1800), false, "opened");
  assert.strictEqual(dash.shouldPing(row, { 7: 69 }, 1100, 1800), true, "a newer report than the one opened");
  assert.strictEqual(dash.shouldPing(row, {}, 1000 + 1800, 1800), false, "no longer recent");
});

test("row words", () => {
  assert.strictEqual(dash.whatText({ n: 1 }), "1 hostile");
  assert.strictEqual(dash.whatText({ n: 12 }), "12 hostiles");
  assert.strictEqual(dash.whatText({ clear: true }), "Clear");
  assert.strictEqual(dash.whatText({ count: 5, noun: "ship" }), "5 ships");
  assert.strictEqual(dash.whatText({ count: 1, noun: "pilot" }), "1 pilot");
  assert.strictEqual(dash.whatText({}), "Hostiles");
  assert.strictEqual(dash.hhmm(Date.UTC(2026, 9, 27, 9, 5) / 1000), "09:05");
});

test("the ping settles from echo to pulses to a faint breath", () => {
  const echo = map.pingRing(0, false), pulse = map.pingRing(1600 + 10, false);
  const breath = map.pingRing(map.PING_SETTLE_MS + 10, false);
  assert.ok(echo.alpha > pulse.alpha && pulse.alpha > breath.alpha, "each phase fainter");
  assert.ok(echo.size > pulse.size && pulse.size > breath.size, "each phase smaller");
  assert.strictEqual(map.PING_SETTLE_MS, 1600 + 5 * 1800, "one echo and five pulses");
  // The breath repeats for as long as the report is recent.
  const later = map.pingRing(map.PING_SETTLE_MS + 6000 * 40 + 10, false);
  assert.ok(Math.abs(later.alpha - breath.alpha) < 1e-9);
  // Reduced motion: one still ring.
  assert.deepStrictEqual(map.pingRing(0, true), map.pingRing(99999, true));
});
