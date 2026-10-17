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

test("simulator, saved fits, auth and switch stay full loads", function () {
  for (const path of [
    "/fittings/",
    "/fittings/saved/",
    "/fittings/saved/?x=1",
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
    "/planner/",
    "/planner/?product=123",
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
  assert.equal(swap.deniedTarget("/planner"), false);
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

test("scriptsMissing normalizes cross-origin URLs on both sides", function () {
  assert.deepEqual(
    swap.scriptsMissing(
      ["/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1", "/static/app.js"],
      ["https://static.cloudflareinsights.com/cdn-cgi/challenge-platform/h/b/orchestrate/chl_page/v1"]),
    []);
  assert.deepEqual(
    swap.scriptsMissing(["/static/fit.js"], ["/static/app.js"]),
    ["/static/fit.js"]);
});

test("scriptsMissing ignores edge-injected scripts the live page never carries", function () {
  // The fetched HTML carries Cloudflare's Rocket Loader tag;
  // the live document does not (the loader removes its own tag
  // after running). That difference must not read as a page
  // requirement, or every swap falls back.
  assert.deepEqual(
    swap.scriptsMissing(
      ["/static/boot.js", "/static/app.js", "/static/swap.js",
       "/cdn-cgi/scripts/7d0fa10a/cloudflare-static/rocket-loader.min.js",
       "https://static.cloudflareinsights.com/beacon.min.js/v4bc70e2c01a94c73b74392e4234840661791215815920"],
      ["/static/boot.js", "/static/app.js", "/static/swap.js",
       "https://static.cloudflareinsights.com/beacon.min.js/v4bc70e2c01a94c73b74392e4234840661791215815920"]),
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

// Element-shaped stand-in for an <img>: just the surface
// applyImgFallback touches.
function fakeImg(attrs, src) {
  return {
    attrs: Object.assign({}, attrs),
    src: src || "",
    getAttribute: function (name) {
      return Object.prototype.hasOwnProperty.call(this.attrs, name) ? this.attrs[name] : null;
    },
    removeAttribute: function (name) { delete this.attrs[name]; }
  };
}

test("applyImgFallback trades the src for the fallback, once", function () {
  const img = fakeImg(
    { "data-fallback-src": "https://images.evetech.net/types/34/icon?size=128" },
    "https://images.evetech.net/types/34/render?size=128");
  assert.equal(swap.applyImgFallback(img), true);
  assert.equal(img.src, "https://images.evetech.net/types/34/icon?size=128");
  // The attribute is gone, so a fallback that also fails cannot
  // loop: a second call changes nothing.
  assert.equal(swap.applyImgFallback(img), false);
  assert.equal(img.src, "https://images.evetech.net/types/34/icon?size=128");
});

test("applyImgFallback leaves images without a fallback alone", function () {
  const img = fakeImg({}, "https://images.evetech.net/types/34/icon?size=128");
  assert.equal(swap.applyImgFallback(img), false);
  assert.equal(img.src, "https://images.evetech.net/types/34/icon?size=128");
  assert.equal(swap.applyImgFallback(null), false);
  assert.equal(swap.applyImgFallback({}), false);
});

test("imgSettledFailed is only a completed image with no pixels", function () {
  assert.equal(swap.imgSettledFailed({ complete: true, naturalWidth: 0 }), true);
  assert.equal(swap.imgSettledFailed({ complete: true, naturalWidth: 128 }), false);
  assert.equal(swap.imgSettledFailed({ complete: false, naturalWidth: 0 }), false);
  assert.equal(swap.imgSettledFailed(null), false);
});
