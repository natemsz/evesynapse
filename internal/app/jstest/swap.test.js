// Unit tests for the swap predicate and fallback rules in
// ../static/swap.js. Run with: node --test "internal/app/jstest/*.test.js"
// (CI runs the same command). The functions under test are pure —
// plain data in, verdict out — so no DOM or browser is needed.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");
const swap = require("../static/swap.js");

function click(overrides) {
  return Object.assign({
    button: 0,
    metaKey: false,
    ctrlKey: false,
    shiftKey: false,
    altKey: false,
    target: "",
    download: false,
    noSwap: false,
    sameOrigin: true,
    hashOnly: false,
    path: "/market/"
  }, overrides || {});
}

test("ordinary internal links qualify", function () {
  assert.equal(swap.shouldSwapClick(click()), true);
});

test("modifier keys, non-left buttons and targets stay full loads", function () {
  assert.equal(swap.shouldSwapClick(click({ button: 1 })), false);
  assert.equal(swap.shouldSwapClick(click({ metaKey: true })), false);
  assert.equal(swap.shouldSwapClick(click({ ctrlKey: true })), false);
  assert.equal(swap.shouldSwapClick(click({ shiftKey: true })), false);
  assert.equal(swap.shouldSwapClick(click({ altKey: true })), false);
  assert.equal(swap.shouldSwapClick(click({ target: "_blank" })), false);
  assert.equal(swap.shouldSwapClick(click({ target: "named" })), false);
  assert.equal(swap.shouldSwapClick(click({ download: true })), false);
  assert.equal(swap.shouldSwapClick(click({ noSwap: true })), false);
  assert.equal(swap.shouldSwapClick(click({ sameOrigin: false })), false);
  assert.equal(swap.shouldSwapClick(click({ hashOnly: true })), false);
  assert.equal(swap.shouldSwapClick(click({ target: "_self" })), true);
});

test("simulator, saved fits, planner, auth and switch stay full loads", function () {
  for (const path of [
    "/fittings/",
    "/fittings/saved/",
    "/fittings/saved/?x=1",
    "/planner/",
    "/planner/?product=123",
    "/auth/eve",
    "/auth/callback",
    "/characters/switch?character=1"
  ]) {
    assert.equal(swap.shouldSwapClick(click({ path: path })), false, path);
  }
});

test("content pages qualify, including public fits and query strings", function () {
  for (const path of [
    "/",
    "/market/",
    "/market/?type=34",
    "/fittings/public/",
    "/fittings/public/fit/7/",
    "/items/",
    "/character/",
    "/sync/"
  ]) {
    assert.equal(swap.shouldSwapClick(click({ path: path })), true, path);
  }
});

test("deniedTarget covers the full-navigation pages", function () {
  assert.equal(swap.deniedTarget("/fittings/"), true);
  assert.equal(swap.deniedTarget("/fittings"), true);
  assert.equal(swap.deniedTarget("/planner"), true);
  assert.equal(swap.deniedTarget("/market/"), false);
  assert.equal(swap.deniedTarget("/plannersham/"), false);
});

test("deniedPrefetch skips the auto-refresh page only", function () {
  assert.equal(swap.deniedPrefetch("/sync/"), true);
  assert.equal(swap.deniedPrefetch("/sync/?character=1"), true);
  assert.equal(swap.deniedPrefetch("/market/"), false);
  assert.equal(swap.deniedPrefetch("/planner/"), false);
});

test("fallback: bad status, redirects, missing main, autorefresh", function () {
  assert.equal(swap.checkSwapResponse({ ok: false }), "bad-status");
  assert.equal(swap.checkSwapResponse({ ok: true, redirected: true }), "redirected");
  assert.equal(
    swap.checkSwapResponse({ ok: true, redirected: false, hasMain: false }),
    "no-main");
  assert.equal(
    swap.checkSwapResponse({ ok: true, redirected: false, hasMain: true, autorefresh: true }),
    "autorefresh");
  assert.equal(
    swap.checkSwapResponse({
      ok: true, redirected: false, hasMain: true,
      autorefresh: false, missingScripts: ["/static/fit.js"]
    }),
    "missing-scripts");
  assert.equal(
    swap.checkSwapResponse({
      ok: true, redirected: false, hasMain: true,
      autorefresh: false, missingScripts: []
    }),
    "");
});

test("scriptsMissing ignores the shell set and dedupes", function () {
  assert.deepEqual(
    swap.scriptsMissing(
      ["/static/app.js", "/static/fit.js", "/static/fit.js"],
      ["/static/boot.js"]),
    ["/static/fit.js"]);
  assert.deepEqual(
    swap.scriptsMissing(["/static/app.js", "/static/swap.js"], []),
    []);
});

test("bestNavHref prefers exact matches, then longest prefix", function () {
  const hrefs = ["/", "/market/", "/market/scanner/", "/wallet/"];
  assert.equal(swap.bestNavHref(hrefs, "/market/"), "/market/");
  assert.equal(swap.bestNavHref(hrefs, "/market"), "/market/");
  assert.equal(swap.bestNavHref(hrefs, "/market/scanner/"), "/market/scanner/");
  assert.equal(swap.bestNavHref(hrefs, "/market/restock/"), "/market/");
  assert.equal(swap.bestNavHref(hrefs, "/nope/"), null);
});

test("isSlowConnection honors save-data and 2g", function () {
  assert.equal(swap.isSlowConnection(null), false);
  assert.equal(swap.isSlowConnection({}), false);
  assert.equal(swap.isSlowConnection({ saveData: true }), true);
  assert.equal(swap.isSlowConnection({ effectiveType: "2g" }), true);
  assert.equal(swap.isSlowConnection({ effectiveType: "slow-2g" }), true);
  assert.equal(swap.isSlowConnection({ effectiveType: "4g" }), false);
});
