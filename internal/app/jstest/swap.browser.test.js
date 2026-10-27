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
  '<link rel="stylesheet" href="/static/style.css?v=1">' +
  '<link rel="stylesheet" href="/static/bg.css?v=1">' +
  '<link rel="stylesheet" href="/static/phone.css?v=1">' +
  '<script src="/static/app.js"></script>' +
  '<script src="/static/swap.js"></script></head>' +
  "<body><main id=\"main-content\"><div class=\"panel\">MARKET</div></main></body></html>";
const KB_HTML = "<html><head><title>Killboard · EveSynapse</title>" +
  '<link rel="stylesheet" href="/static/style.css?v=1">' +
  '<link rel="stylesheet" href="/static/bg.css?v=1">' +
  '<link rel="stylesheet" href="/static/killboard.css?v=1">' +
  '<link rel="stylesheet" href="/static/phone.css?v=1">' +
  '<script src="/static/app.js"></script>' +
  '<script src="/static/swap.js"></script></head>' +
  "<body><main id=\"main-content\"><div class=\"kb\">KILLS</div></main></body></html>";
const MAP_HTML = "<html><head><title>Map · EveSynapse</title>" +
  '<link rel="stylesheet" href="/static/style.css?v=1">' +
  '<link rel="stylesheet" href="/static/bg.css?v=1">' +
  '<link rel="stylesheet" href="/static/map.css?v=1">' +
  '<link rel="stylesheet" href="/static/phone.css?v=1">' +
  '<script src="/static/app.js"></script>' +
  '<script src="/static/swap.js"></script>' +
  '<script src="/static/map.js"></script></head>' +
  "<body><main id=\"main-content\"><div class=\"map\">MAP</div></main></body></html>";
const LOGIN_HTML = "<html><head><title>Home · EveSynapse</title></head>" +
  "<body><main id=\"main-content\">LANDING</main></body></html>";
const MODAL_HTML = "<div class=\"modal-backdrop\" id=\"theme-modal\">THEME</div>";

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
  const events = [];
  const created = [];
  // Asset elements (stylesheet links, scripts) the swap creates:
  // appending one to the head "loads" it on the next tick, or
  // fails it when the test marked its URL via failStyle/failScript.
  function fakeAssetEl(kind) {
    return {
      kind: kind,
      attrs: {},
      parentNode: null,
      onload: null,
      onerror: null,
      getAttribute: function (k) {
        if (k === "href") return this.href || null;
        if (k === "src") return this.src || null;
        if (k === "media") return this.media || "";
        return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null;
      },
      setAttribute: function (k, v) { this.attrs[k] = v; },
      remove: function () {
        if (this.parentNode && this.parentNode.children) {
          const i = this.parentNode.children.indexOf(this);
          if (i >= 0) this.parentNode.children.splice(i, 1);
        }
        this.parentNode = null;
      }
    };
  }
  function fireLoad(el) {
    setTimeout(function () {
      const url = el.kind === "link" ? el.href : el.src;
      const fail = (el.kind === "link" ? opts.failStyle : opts.failScript) || "";
      if (fail && url && url.indexOf(fail) !== -1) {
        events.push("error:" + url);
        if (el.onerror) el.onerror();
      } else {
        events.push("load:" + url);
        if (el.onload) el.onload();
      }
    }, 0);
  }
  const headEl = {
    children: [],
    querySelectorAll: function (sel) {
      if (sel.indexOf("stylesheet") === -1) return [];
      return this.children.slice();
    },
    appendChild: function (el) {
      el.parentNode = this;
      this.children.push(el);
      if (el.kind) fireLoad(el);
      return el;
    },
    insertBefore: function (el, ref) {
      el.parentNode = this;
      const i = this.children.indexOf(ref);
      if (i === -1) this.children.push(el); else this.children.splice(i, 0, el);
      if (el.kind) fireLoad(el);
      return el;
    }
  };
  (opts.shellStyles || []).forEach(function (href) {
    const el = fakeAssetEl("link");
    el.href = href;
    el.parentNode = headEl;
    headEl.children.push(el);
  });
  const mainEl = {
    innerHTML: "HOME",
    attrs: {},
    setAttribute: function (k, v) { this.attrs[k] = v; },
    querySelectorAll: function () { return []; },
    focus: function () { this.focused = true; }
  };
  const scrolls = [];
  const drawer = opts.drawer ? {
    checked: true,
    events: [],
    dispatchEvent: function (ev) { this.events.push(ev.type); return true; }
  } : null;
  const mainDoc = {
    getElementById: function (id) {
      if (id === "main-content") return mainEl;
      if (id === "nav-drawer-toggle") return drawer;
      return null;
    },
    querySelectorAll: function () { return []; },
    head: headEl,
    createElement: function (tag) {
      const el = fakeAssetEl(tag === "link" ? "link" : "script");
      created.push(el);
      return el;
    },
    addEventListener: function (type, fn) {
      (listeners[type] = listeners[type] || []).push(fn);
    },
    dispatchEvent: function (ev) { events.push("event:" + ev.type); return true; },
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
      querySelectorAll: function (sel) {
        const out = [];
        let m;
        if (sel.indexOf("stylesheet") !== -1) {
          const re = /<link[^>]*href="([^"]+)"[^>]*>/g;
          while ((m = re.exec(html)) !== null) {
            const href = m[1];
            out.push({
              href: href,
              getAttribute: function (k) {
                return k === "href" ? href : (k === "media" ? "" : null);
              }
            });
          }
        } else if (sel.indexOf("script") !== -1) {
          const re = /<script[^>]*src="([^"]+)"[^>]*>/g;
          while ((m = re.exec(html)) !== null) {
            const src = m[1];
            out.push({
              src: src,
              getAttribute: function (k) { return k === "src" ? src : null; }
            });
          }
        }
        return out;
      },
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
    fetch: function (url, init) {
      win.fetched.push(url);
      win.fetchInits.push(init || {});
      if (opts.redirect) {
        return Promise.resolve({ ok: true, redirected: true, url: url, text: function () { return Promise.resolve(""); } });
      }
      if (opts.status) {
        return Promise.resolve({ ok: false, status: opts.status, redirected: false, text: function () { return Promise.resolve("err"); } });
      }
      let html = opts.landing ? LOGIN_HTML : FIT_HTML;
      if (opts.pages) {
        let p = url;
        try { p = new URL(url).pathname; } catch (e) { /* keep default */ }
        if (opts.pages[p]) html = opts.pages[p];
      }
      return Promise.resolve({ ok: true, redirected: false, url: url, text: function () { return Promise.resolve(html); } });
    },
    fetched: [],
    fetchInits: [],
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
  // A start page whose main carries a modal backdrop (opts.modal):
  // the fake modal lives in main until the shell hoists it to the
  // body, and main's innerHTML reads as the live tree would — the
  // modal's markup is in it exactly while the modal is in main.
  let modalEl = null;
  let bodyEl = null;
  if (opts.modal) {
    modalEl = {
      attrs: {},
      parentNode: null,
      setAttribute: function (k, v) { this.attrs[k] = v; },
      getAttribute: function (k) {
        return Object.prototype.hasOwnProperty.call(this.attrs, k) ? this.attrs[k] : null;
      }
    };
    bodyEl = {
      children: [],
      appendChild: function (el) {
        el.parentNode = bodyEl;
        this.children.push(el);
        return el;
      },
      removeChild: function (el) {
        const i = this.children.indexOf(el);
        if (i >= 0) this.children.splice(i, 1);
        if (el.parentNode === bodyEl) el.parentNode = null;
        return el;
      }
    };
    let storedMain = "HOME" + MODAL_HTML;
    modalEl.parentNode = mainEl;
    Object.defineProperty(mainEl, "innerHTML", {
      get: function () {
        return modalEl.parentNode === mainEl ? storedMain : storedMain.split(MODAL_HTML).join("");
      },
      set: function (v) {
        storedMain = v;
        if (v.indexOf(MODAL_HTML) !== -1) modalEl.parentNode = mainEl;
      }
    });
    mainEl.querySelectorAll = function (sel) {
      if (sel === ".modal-backdrop") return modalEl.parentNode === mainEl ? [modalEl] : [];
      return [];
    };
    const docQueryAll = mainDoc.querySelectorAll;
    mainDoc.querySelectorAll = function (sel) {
      if (sel === ".modal-backdrop") return modalEl.parentNode ? [modalEl] : [];
      if (sel === "body > .modal-backdrop[data-hoisted]") {
        return modalEl.parentNode === bodyEl && modalEl.attrs["data-hoisted"] ? [modalEl] : [];
      }
      return docQueryAll.call(mainDoc, sel);
    };
    mainDoc.body = bodyEl;
  }
  return {
    listeners: listeners, mainEl: mainEl, mainDoc: mainDoc, drawer: drawer,
    modalEl: modalEl, bodyEl: bodyEl,
    win: win, pushes: pushes, scrolls: scrolls,
    events: events, headEl: headEl, created: created,
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

test("hovering a link prefetches with zero side effects; opening it counts once", async function () {
  const env = makeEnv();
  loadSwap(env);
  const a = anchor("/market/");
  env.listeners.mouseover.forEach(function (fn) { fn({ target: { closest: function () { return a; } } }); });
  await waitFor(function () { return env.win.fetched.length === 1; }, "prefetch");
  await flush();
  // The prefetch says it is one, so the server counts nothing for it,
  // and the page is not shown or pushed.
  assert.equal(env.win.fetchInits[0].headers["X-Swap-Prefetch"], "1");
  assert.equal(env.mainEl.innerHTML, "HOME");
  assert.deepEqual(env.pushes, []);
  // Hovering again fetches nothing more.
  env.listeners.mouseover.forEach(function (fn) { fn({ target: { closest: function () { return a; } } }); });
  await flush();
  assert.equal(env.win.fetched.length, 1);
  // Opening it paints from the prefetch and sends one plain request:
  // the view, with no prefetch header.
  clickOn(env, "/market/");
  await waitFor(function () { return env.mainEl.innerHTML.includes("MARKET"); }, "swap paint");
  await waitFor(function () { return env.win.fetched.length === 2; }, "commit");
  assert.equal(env.win.fetched[1], "https://app.test/market/");
  assert.equal(env.win.fetchInits[1].headers["X-Swap-Prefetch"], undefined);
  // Opening it again from the cache is not a second commit.
  clickOn(env, "/market/");
  await flush();
  assert.equal(env.win.fetched.length, 2);
});

test("a swap closes the open nav drawer", async function () {
  const env = makeEnv({ drawer: true });
  loadSwap(env);
  assert.equal(env.drawer.checked, true);
  clickOn(env, "/market/");
  await waitFor(function () { return env.mainEl.innerHTML.includes("MARKET"); }, "swap paint");
  assert.equal(env.drawer.checked, false);
  assert.deepEqual(env.drawer.events, ["change"]);
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

test("a fragment-only history move is left to the browser", async function () {
  const env = makeEnv();
  loadSwap(env);
  // An in-page anchor (a "jump to bottom" link) fires popstate with
  // the same document under the hash: no fetch, no repaint, no
  // scroll back to the top.
  env.win.location.href = "https://app.test/characters/#lk-actions";
  env.firePopstate(null);
  await flush();
  assert.deepEqual(env.win.fetched, []);
  assert.equal(env.mainEl.innerHTML, "HOME");
  assert.deepEqual(env.scrolls, []);
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

test("the first page's modals survive a swap away and back", async function () {
  const env = makeEnv({ modal: true, pages: { "/intel/kills/": KB_HTML } });
  loadSwap(env);
  // The boot hoists the start page's modal out of main to the body.
  assert.equal(env.modalEl.parentNode, env.bodyEl);
  clickOn(env, "/intel/kills/");
  await waitFor(function () { return env.mainEl.innerHTML.includes("KILLS"); }, "swap paint");
  // Away on the killboard the modal is gone with its page.
  assert.equal(env.modalEl.parentNode, null);
  env.win.location.href = "https://app.test/characters/";
  env.firePopstate({ esSwap: 1, scroll: 0 });
  await waitFor(function () { return env.modalEl.parentNode === env.bodyEl; }, "modal restored and hoisted");
  assert.ok(env.mainEl.innerHTML.includes("HOME"), "start page restored");
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

test("a swap adopts the fetched page's stylesheet before content-ready", async function () {
  const env = makeEnv({
    pages: { "/intel/kills/": KB_HTML },
    shellStyles: ["/static/style.css?v=1", "/static/bg.css?v=1", "/static/phone.css?v=1"]
  });
  loadSwap(env);
  clickOn(env, "/intel/kills/");
  await waitFor(function () { return env.mainEl.innerHTML.includes("KILLS"); }, "swap paint");
  await waitFor(function () {
    return env.events.includes("event:evesynapse:content-ready");
  }, "content-ready");
  // The page sheet joined the head, seated before phone.css as a
  // full load orders it, and marked as the swap's own.
  assert.deepEqual(
    env.headEl.children.map(function (el) { return el.href; }),
    ["/static/style.css?v=1", "/static/bg.css?v=1", "/static/killboard.css?v=1", "/static/phone.css?v=1"]);
  assert.equal(env.headEl.children[2].getAttribute("data-swap-style"), "/static/killboard.css");
  // The sheet finished loading before content-ready fired, so the
  // page is never presented unstyled or rebooted too early.
  const loadAt = env.events.indexOf("load:/static/killboard.css?v=1");
  const readyAt = env.events.indexOf("event:evesynapse:content-ready");
  assert.ok(loadAt !== -1 && readyAt !== -1 && loadAt < readyAt);
});

test("a swap loads the page's missing script before content-ready", async function () {
  const env = makeEnv({ pages: { "/map/": MAP_HTML } });
  loadSwap(env);
  clickOn(env, "/map/");
  await waitFor(function () { return env.mainEl.innerHTML.includes("MAP"); }, "swap paint");
  await waitFor(function () {
    return env.events.includes("event:evesynapse:content-ready");
  }, "content-ready");
  const script = env.created.find(function (el) { return el.kind === "script"; });
  assert.ok(script, "a script element was created for the page script");
  assert.equal(script.src, "/static/map.js");
  assert.equal(script.async, false);
  const loadAt = env.events.indexOf("load:/static/map.js");
  const readyAt = env.events.indexOf("event:evesynapse:content-ready");
  assert.ok(loadAt !== -1 && readyAt !== -1 && loadAt < readyAt);
  // The map's stylesheet was adopted alongside.
  assert.ok(env.headEl.children.some(function (el) { return el.href === "/static/map.css?v=1"; }));
});

test("a stylesheet that fails to load falls back instead of painting unstyled", async function () {
  const env = makeEnv({
    pages: { "/intel/kills/": KB_HTML },
    failStyle: "/static/killboard.css"
  });
  loadSwap(env);
  clickOn(env, "/intel/kills/");
  await waitFor(function () {
    return env.events.includes("error:/static/killboard.css?v=1");
  }, "sheet error");
  await flush();
  // No paint, no content-ready: the browser takes the navigation
  // and the page loads whole rather than stranding unstyled.
  assert.equal(env.mainEl.innerHTML, "HOME");
  assert.ok(!env.events.includes("event:evesynapse:content-ready"));
});

test("an adopted sheet is pruned on the next swap and restored on back", async function () {
  const env = makeEnv({
    pages: { "/intel/kills/": KB_HTML, "/market/": FIT_HTML },
    shellStyles: ["/static/style.css?v=1", "/static/bg.css?v=1", "/static/phone.css?v=1"]
  });
  loadSwap(env);
  clickOn(env, "/intel/kills/");
  await waitFor(function () {
    return env.headEl.children.some(function (el) { return el.href === "/static/killboard.css?v=1"; });
  }, "sheet adopted");
  clickOn(env, "/market/");
  await waitFor(function () { return env.mainEl.innerHTML.includes("MARKET"); }, "market paint");
  await waitFor(function () {
    return !env.headEl.children.some(function (el) { return el.href === "/static/killboard.css?v=1"; });
  }, "sheet pruned");
  // The shell's own sheets survive pruning: every page lists them.
  assert.deepEqual(
    env.headEl.children.map(function (el) { return el.href; }),
    ["/static/style.css?v=1", "/static/bg.css?v=1", "/static/phone.css?v=1"]);
  env.win.location.href = "https://app.test/intel/kills/";
  env.firePopstate({ esSwap: 1, scroll: 0 });
  await waitFor(function () { return env.mainEl.innerHTML.includes("KILLS"); }, "back paint");
  await waitFor(function () {
    return env.headEl.children.some(function (el) { return el.href === "/static/killboard.css?v=1"; });
  }, "sheet restored");
});

test("the boot page's own sheet is pruned when the shell swaps away", async function () {
  // The session booted on the killboard (a full load): its sheet
  // is part of the boot document, yet it is still a page sheet —
  // swapping to a page that does not carry it prunes it, while
  // the shell sheets every page lists stay.
  const env = makeEnv({
    pages: { "/market/": FIT_HTML },
    shellStyles: ["/static/style.css?v=1", "/static/bg.css?v=1",
      "/static/killboard.css?v=1", "/static/phone.css?v=1"]
  });
  loadSwap(env);
  clickOn(env, "/market/");
  await waitFor(function () { return env.mainEl.innerHTML.includes("MARKET"); }, "market paint");
  await waitFor(function () {
    return !env.headEl.children.some(function (el) { return el.href === "/static/killboard.css?v=1"; });
  }, "boot page sheet pruned");
  assert.deepEqual(
    env.headEl.children.map(function (el) { return el.href; }),
    ["/static/style.css?v=1", "/static/bg.css?v=1", "/static/phone.css?v=1"]);
});

test("a content-mutated event drops the fetched-page cache", async function () {
  const env = makeEnv();
  loadSwap(env);
  clickOn(env, "/market/");
  await waitFor(function () { return env.mainEl.innerHTML.includes("MARKET"); }, "swap paint");
  const fetches = env.win.fetched.length;
  // The page changed server state without navigating (a home
  // layout save): cached copies predate it, so going back must
  // fetch fresh instead of painting the stale page.
  (env.listeners["evesynapse:content-mutated"] || []).forEach(function (fn) { fn({ type: "evesynapse:content-mutated" }); });
  env.win.location.href = "https://app.test/characters/";
  env.firePopstate({ esSwap: 1, scroll: 0 });
  await waitFor(function () { return env.win.fetched.length > fetches; }, "refetch after mutation");
  await waitFor(function () { return env.mainEl.innerHTML.includes("MARKET"); }, "fresh page painted");
});
