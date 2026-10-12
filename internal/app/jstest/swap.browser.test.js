// Browser-path tests for ../static/swap.js: the click interceptor,
// fetch-to-swap flow, fallbacks and history, driven through a small
// DOM stub (no browser needed). Async waits poll for the expected
// condition with a deadline — never a fixed sleep — so the suite
// runs as fast as the code allows and fails deterministically.
// Absence assertions (no fetch fired) drain the task queue instead:
// the denied paths are fully synchronous, so once the queue is
// empty nothing more can happen.
"use strict";
const test = require("node:test");
const assert = require("node:assert/strict");

const FIT_HTML = "<html><head><title>Market · EveSynapse</title>" +
  '<script src="/static/app.js"></script>' +
  '<script src="/static/swap.js"></script></head>' +
  "<body><main id=\"main-content\"><div class=\"panel\">MARKET</div></main></body></html>";
const LOGIN_HTML = "<html><head><title>Home · EveSynapse</title></head>" +
  "<body><main id=\"main-content\">LANDING</main></body></html>";

function waitFor(cond, label) {
  const deadline = Date.now() + 4000;
  return new Promise(function (resolve, reject) {
    (function poll() {
      let ok = false;
      try {
        ok = cond();
      } catch (e) {
        reject(e);
        return;
      }
      if (ok) {
        resolve();
        return;
      }
      if (Date.now() > deadline) {
        reject(new Error("timed out waiting for: " + label));
        return;
      }
      setTimeout(poll, 10);
    })();
  });
}

function flush() {
  // Drain pending microtasks and macrotasks; the denied paths do
  // all their work synchronously, so afterwards a fetch can no
  // longer appear.
  return new Promise(function (resolve) {
    setImmediate(function () {
      setImmediate(function () {
        setImmediate(resolve);
      });
    });
  });
}

function makeEnv(opts) {
  opts = opts || {};
  const listeners = {};
  const mainEl = {
    innerHTML: "HOME",
    attrs: {},
    setAttribute: function (k, v) { this.attrs[k] = v; },
    focus: function () { this.focused = true; }
  };
  const scrolls = [];
  const mainDoc = {
    getElementById: function (id) { return id === "main-content" ? mainEl : null; },
    querySelectorAll: function () { return []; },
    addEventListener: function (type, fn) {
      (listeners[type] = listeners[type] || []).push(fn);
    },
    dispatchEvent: function () { return true; },
    title: "Home",
    documentElement: { getAttribute: function () { return null; } },
    contains: function () { return true; },
    hidden: false,
    visibilityState: "visible"
  };
  const fetchedDoc = function (html) {
    return {
      getElementById: function (id) {
        if (id !== "main-content" || html.indexOf('id="main-content"') === -1) return null;
        return { innerHTML: html.split('<main id="main-content">')[1].split("</main>")[0] };
      },
      querySelectorAll: function () { return []; },
      querySelector: function (sel) {
        if (sel === "title") {
          const m = html.match(/<title>([^<]*)<\/title>/);
          return m ? { textContent: m[1] } : null;
        }
        return null;
      },
      body: { getAttribute: function () { return null; } }
    };
  };
  const pushes = [];
  const win = {
    location: { href: "https://app.test/characters/", origin: "https://app.test" },
    history: {
      replaceState: function () {},
      pushState: function (st, t, url) { pushes.push(url); win.location.href = url; }
    },
    scrollTo: function (x, y) { scrolls.push([x, y]); },
    fetch: function (url) {
      win.fetched.push(url);
      if (opts.redirect) {
        return Promise.resolve({ ok: true, redirected: true, url: url, text: function () { return Promise.resolve(""); } });
      }
      if (opts.status) {
        return Promise.resolve({ ok: false, status: opts.status, redirected: false, text: function () { return Promise.resolve("err"); } });
      }
      const html = opts.landing ? LOGIN_HTML : FIT_HTML;
      return Promise.resolve({ ok: true, redirected: false, url: url, text: function () { return Promise.resolve(html); } });
    },
    fetched: [],
    DOMParser: function () {},
    matchMedia: function () { return { matches: false }; },
    navigator: {},
    addEventListener: function (type, fn) {
      (listeners[type] = listeners[type] || []).push(fn);
    },
    CustomEvent: function (type, init) { this.type = type; this.detail = init && init.detail; },
    URL: URL,
    AbortController: AbortController
  };
  win.DOMParser.prototype.parseFromString = function (html) { return fetchedDoc(html); };
  return {
    listeners: listeners, mainEl: mainEl, mainDoc: mainDoc,
    win: win, pushes: pushes, scrolls: scrolls,
    firePopstate: function (state) {
      (listeners.popstate || []).forEach(function (fn) { fn({ state: state }); });
    }
  };
}

function loadSwap(env) {
  global.window = env.win;
  global.document = env.mainDoc;
  global.DOMParser = env.win.DOMParser;
  global.CustomEvent = env.win.CustomEvent;
  global.URL = URL;
  delete require.cache[require.resolve("../static/swap.js")];
  require("../static/swap.js");
  // Globals stay installed: the swap handlers resolve document and
  // window when clicks land, not at load. Tests in this file run
  // sequentially and each one reinstalls its own env on load.
}

function anchor(path) {
  return {
    getAttribute: function (k) {
      if (k === "href") return path;
      if (k === "target") return "";
      return "";
    },
    hasAttribute: function () { return false; }
  };
}

function clickOn(env, path, extra) {
  const a = anchor(path);
  const ev = Object.assign({
    button: 0, metaKey: false, ctrlKey: false, shiftKey: false, altKey: false,
    defaultPrevented: false,
    preventDefault: function () { this.defaultPrevented = true; },
    target: { closest: function () { return a; } }
  }, extra || {});
  env.listeners.click.forEach(function (fn) { fn(ev); });
  return ev;
}

test("a qualifying click swaps main without a load", async function () {
  const env = makeEnv();
  loadSwap(env);
  assert.equal(env.mainEl.attrs.tabindex, "-1");
  clickOn(env, "/market/");
  await waitFor(function () { return env.mainEl.innerHTML.includes("MARKET"); }, "swap paint");
  assert.deepEqual(env.pushes, ["https://app.test/market/"]);
  assert.equal(env.mainDoc.title, "Market · EveSynapse");
  assert.deepEqual(env.scrolls, [[0, 0]]);
  assert.equal(env.mainEl.focused, true);
});

test("a redirect (expired session) falls back to a full load", async function () {
  const env = makeEnv({ redirect: true });
  loadSwap(env);
  let navigated = "";
  env.win.location = {
    origin: "https://app.test",
    set href(v) { navigated = v; },
    get href() { return "https://app.test/characters/"; }
  };
  clickOn(env, "/wallet/");
  await waitFor(function () { return navigated !== ""; }, "fallback navigation");
  assert.equal(navigated, "https://app.test/wallet/");
  assert.match(env.mainEl.innerHTML, /HOME/);
});

test("denied targets never fetch", async function () {
  const env = makeEnv();
  loadSwap(env);
  clickOn(env, "/fittings/");
  clickOn(env, "/planner/?product=1");
  await flush();
  assert.deepEqual(env.win.fetched, []);
});

test("modifier clicks and hash navs are left alone", async function () {
  const env = makeEnv();
  loadSwap(env);
  const ev = clickOn(env, "/market/", { metaKey: true });
  assert.equal(ev.defaultPrevented, false);
  await flush();
  assert.deepEqual(env.win.fetched, []);
});

test("back to a cached page restores without fetching or pushing", async function () {
  const env = makeEnv();
  loadSwap(env);
  clickOn(env, "/market/");
  await waitFor(function () { return env.mainEl.innerHTML.includes("MARKET"); }, "swap paint");
  const fetches = env.win.fetched.length;
  const pushes = env.pushes.length;
  env.win.location.href = "https://app.test/characters/";
  env.firePopstate({ esSwap: 1, scroll: 123 });
  await waitFor(function () { return env.mainEl.innerHTML.includes("HOME"); }, "back restore");
  assert.equal(env.mainDoc.title, "Home");
  assert.deepEqual(env.scrolls[env.scrolls.length - 1], [0, 123]);
  assert.equal(env.win.fetched.length, fetches, "back to cache must not fetch");
  assert.equal(env.pushes.length, pushes, "back must not push state");
});

test("back to an uncached page fetches and swaps without pushing", async function () {
  const env = makeEnv();
  loadSwap(env);
  const pushes = env.pushes.length;
  env.win.location.href = "https://app.test/unvisited/";
  env.firePopstate({ esSwap: 1, scroll: 0 });
  await waitFor(function () { return env.mainEl.innerHTML.includes("MARKET"); }, "back fetch swap");
  assert.ok(env.win.fetched.includes("https://app.test/unvisited/"), "back to cold page fetches");
  assert.equal(env.pushes.length, pushes, "back must not push state");
});
