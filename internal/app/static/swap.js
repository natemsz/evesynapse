// EveSynapse persistent shell: internal navigations swap only the
// <main> content instead of reloading the whole document. The shell
// (sidebar, top bar, footer) renders once and is never touched;
// every behavior below degrades to a plain full navigation, so with
// JavaScript disabled (or when anything is off) the site works
// exactly as before: links stay real links and every page is still
// server-rendered.
//
// What qualifies for a swap (see shouldSwapClick): an unmodified
// left-click on a same-origin link with no target/download marker.
// Pages that need scripts the current shell has not loaded, pages
// that redirect (an expired session bounces to /), pages without a
// <main>, and auto-refresh pages always fall back to a full
// navigation — a failed swap must never strand anyone on stale
// content.
(function () {
  "use strict";

  // Link targets that always load whole: the fitting simulator and
  // saved fits (fit.js state), the build planner (plan.js state),
  // the SSO round-trips, and the character switch (the shell's
  // account area changes identity with it).
  var SWAP_DENY_EXACT = ["/fittings/"];
  var SWAP_DENY_PREFIX = ["/fittings/saved", "/auth/", "/characters/switch"];

  // Prefetch skips more than clicks do: the auto-refresh page
  // always falls back to a full load, so rendering it ahead of
  // time is a wasted server render. Clicks on it still work
  // through the normal fallback.
  var PREFETCH_DENY_PREFIX = ["/sync/"];

  // Scripts the shell always carries. A fetched page that needs
  // anything else (fit.js, plan.js, push.js) loads whole when the
  // current page does not already have it.
  var SHELL_SCRIPTS = ["/static/boot.js", "/static/app.js", "/static/swap.js"];

  function normalizePath(path) {
    if (!path) return "/";
    if (path.charAt(0) !== "/") return path;
    if (path.length > 1 && path.charAt(path.length - 1) === "/") {
      return path.slice(0, -1);
    }
    return path;
  }

  function deniedPrefetch(path) {
    var p = normalizePath(path);
    for (var i = 0; i < PREFETCH_DENY_PREFIX.length; i++) {
      var pre = PREFETCH_DENY_PREFIX[i];
      if (p === normalizePath(pre) || p.indexOf(pre) === 0) return true;
    }
    return false;
  }

  function deniedTarget(path) {
    var p = normalizePath(path);
    for (var i = 0; i < SWAP_DENY_EXACT.length; i++) {
      if (p === SWAP_DENY_EXACT[i] || p === normalizePath(SWAP_DENY_EXACT[i])) return true;
    }
    for (var j = 0; j < SWAP_DENY_PREFIX.length; j++) {
      var pre = SWAP_DENY_PREFIX[j];
      if (p === normalizePath(pre) || p.indexOf(pre) === 0) return true;
    }
    return false;
  }

  // Pure: does this click qualify for a content swap? The argument
  // is plain data (no DOM), so the rule stays unit-testable:
  //   { button, metaKey, ctrlKey, shiftKey, altKey, target,
  //     download, noSwap, sameOrigin, hashOnly, path }
  function shouldSwapClick(c) {
    if (!c || c.button !== 0) return false;
    if (c.metaKey || c.ctrlKey || c.shiftKey || c.altKey) return false;
    if (c.target && c.target !== "_self") return false;
    if (c.download || c.noSwap) return false;
    if (!c.sameOrigin || c.hashOnly) return false;
    if (!c.path || deniedTarget(c.path)) return false;
    return true;
  }

  // Pure: given a fetch outcome, swap or load whole? r is
  //   { ok, redirected, hasMain, autorefresh, missingScripts }
  // Returns "" when the swap may proceed, else a short reason.
  function checkSwapResponse(r) {
    if (!r || !r.ok) return "bad-status";
    if (r.redirected) return "redirected";
    if (!r.hasMain) return "no-main";
    if (r.autorefresh) return "autorefresh";
    if (r.missingScripts && r.missingScripts.length) return "missing-scripts";
    return "";
  }

  // Pure: one comparison form for a script src. Both the
  // fetched page's list and the live document's list run through
  // this, so a script present on both sides always cancels out —
  // including absolute cross-origin URLs, whose pathname form
  // would otherwise never equal the raw src.
  function scriptKey(src) {
    var raw = src || "";
    var base = "https://localhost/";
    if (typeof window !== "undefined" && window.location && window.location.href) {
      base = window.location.href;
    }
    try {
      return new URL(raw, base).pathname;
    } catch (e) {
      return raw.split("?")[0];
    }
  }

  // Pure: pathnames the fetched page needs that are not loaded now
  // and not part of the shell itself.
  function scriptsMissing(needed, loaded) {
    var seen = {};
    var i;
    for (i = 0; i < loaded.length; i++) seen[scriptKey(loaded[i])] = true;
    for (i = 0; i < SHELL_SCRIPTS.length; i++) seen[scriptKey(SHELL_SCRIPTS[i])] = true;
    var out = [];
    for (i = 0; i < needed.length; i++) {
      var key = scriptKey(needed[i]);
      // Edge-injected scripts (Rocket Loader, the challenge
      // platform) ride along in the fetched HTML but are never
      // in the live document — the loader takes its own tag back
      // out once it has run — and they are not the page's to
      // load. Counting them here failed every swap.
      if (key.indexOf("/cdn-cgi/") === 0) continue;
      if (!seen[key] && out.indexOf(key) === -1) out.push(key);
    }
    return out;
  }

  // Pure: pick the sidebar link to mark active for path. Exact
  // matches win; otherwise the longest href that is a whole path
  // segment prefix of the target.
  function bestNavHref(hrefs, path) {
    var want = normalizePath(path);
    var i, h;
    for (i = 0; i < hrefs.length; i++) {
      if (normalizePath(hrefs[i]) === want) return hrefs[i];
    }
    var best = null;
    for (i = 0; i < hrefs.length; i++) {
      h = normalizePath(hrefs[i]);
      if (h === "/") continue;
      if (want.indexOf(h) === 0 && (want.length === h.length || want.charAt(h.length) === "/")) {
        if (!best || h.length > normalizePath(best).length) best = hrefs[i];
      }
    }
    return best;
  }

  // Pure: skip prefetching on metered or very slow connections.
  function isSlowConnection(conn) {
    if (!conn) return false;
    if (conn.saveData) return true;
    var t = conn.effectiveType || "";
    return t === "slow-2g" || t === "2g";
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      shouldSwapClick: shouldSwapClick,
      checkSwapResponse: checkSwapResponse,
      scriptKey: scriptKey,
      scriptsMissing: scriptsMissing,
      bestNavHref: bestNavHref,
      normalizePath: normalizePath,
      isSlowConnection: isSlowConnection,
      deniedTarget: deniedTarget,
      deniedPrefetch: deniedPrefetch
    };
  }

  if (typeof document === "undefined" || typeof window === "undefined") return;
  if (!window.fetch || !window.DOMParser || !window.history || !window.history.pushState) return;
  var main = document.getElementById("main-content");
  if (!main) return;
  main.setAttribute("tabindex", "-1");

  // The document the shell is currently showing, fragment aside.
  // A fragment-only history move (an in-page anchor such as a
  // "jump to bottom" link, or back/forward across one) is the
  // browser's own business: the content is already on screen, and
  // swapping it in again would repaint the page and throw the
  // scroll back to the top.
  var docURL = window.location.href.split("#")[0];

  // Modal backdrops are position: fixed, and a backdrop-filter
  // ancestor (the glass page wrapper) becomes their containing
  // block, shrinking them from the viewport to the wrapper. They
  // live at body level instead, where fixed means the viewport:
  // hoisted on first load and after every swap (the incoming
  // page's backdrops arrive inside the swapped markup), and the
  // outgoing page's hoisted backdrops are dropped as it leaves.
  function hoistModals(root) {
    var found = root.querySelectorAll(".modal-backdrop");
    for (var i = 0; i < found.length; i++) {
      var el = found[i];
      if (el.parentNode !== document.body) {
        el.setAttribute("data-hoisted", "1");
        document.body.appendChild(el);
      }
    }
  }
  function dropHoistedModals() {
    var stale = document.querySelectorAll("body > .modal-backdrop[data-hoisted]");
    for (var i = 0; i < stale.length; i++) {
      stale[i].parentNode.removeChild(stale[i]);
    }
  }
  hoistModals(document);
  // The shell owns scroll: the browser's own restoration would
  // fight the swap (a second jump after the paint), so it stays
  // off while the swapper is active.
  try {
    window.history.scrollRestoration = "manual";
  } catch (e) { /* older engines: the swap still lands */ }

  var reducedMotion = window.matchMedia &&
    window.matchMedia("(prefers-reduced-motion: reduce)").matches;

  // Fetched-page cache: prefetch fills it, history revisits read
  // it. Small and short-lived: at most 24 entries, 60 seconds each.
  var pageCache = {};
  var pageCacheOrder = [];
  function cacheGet(url) {
    var hit = pageCache[url];
    if (!hit) return null;
    if (Date.now() - hit.at > 60000) {
      delete pageCache[url];
      return null;
    }
    return hit;
  }
  function cachePut(url, entry) {
    if (!pageCache[url]) {
      pageCacheOrder.push(url);
      while (pageCacheOrder.length > 24) {
        delete pageCache[pageCacheOrder.shift()];
      }
    }
    entry.at = Date.now();
    pageCache[url] = entry;
  }

  var inflight = {}; // url -> true while a prefetch is running
  var navController = null; // AbortController for the live navigation

  function scriptPathnames(doc) {
    var out = [];
    var tags = doc.querySelectorAll("script[src]");
    for (var i = 0; i < tags.length; i++) {
      out.push(scriptKey(tags[i].getAttribute("src") || ""));
    }
    return out;
  }

  function loadedScriptPathnames() {
    var out = [];
    var tags = document.querySelectorAll("script[src]");
    for (var i = 0; i < tags.length; i++) {
      out.push(scriptKey(tags[i].getAttribute("src") || ""));
    }
    return out;
  }

  function fetchPage(url, signal, prefetching) {
    var headers = { "Accept": "text/html" };
    // A prefetch says so: the server then counts no view, marks no
    // guess or foresight target opened, and queues nothing at
    // viewed priority. Only a committed navigation counts.
    if (prefetching) headers["X-Swap-Prefetch"] = "1";
    var init = {
      credentials: "same-origin",
      headers: headers
    };
    if (signal) init.signal = signal;
    return window.fetch(url, init).then(function (resp) {
      var text = function () { return resp.text(); };
      if (!resp.ok) {
        return text().then(function () {
          return { reason: checkSwapResponse({ ok: false }) };
        });
      }
      if (resp.redirected) {
        return { reason: checkSwapResponse({ ok: true, redirected: true }) };
      }
      return text().then(function (html) {
        var doc;
        try {
          doc = new DOMParser().parseFromString(html, "text/html");
        } catch (e) {
          return { reason: "unparseable" };
        }
        var next = doc.getElementById("main-content");
        var body = doc.body;
        var missing = scriptsMissing(scriptPathnames(doc), loadedScriptPathnames());
        var reason = checkSwapResponse({
          ok: true,
          redirected: false,
          hasMain: !!next,
          autorefresh: !!(body && body.getAttribute("data-autorefresh")),
          missingScripts: missing
        });
        if (reason) return { reason: reason };
        var titleEl = doc.querySelector("title");
        return {
          main: next.innerHTML,
          title: titleEl ? titleEl.textContent : ""
        };
      });
    });
  }

  function syncActiveNav(path) {
    var sidebar = document.getElementById("site-nav");
    if (!sidebar) return;
    var links = sidebar.querySelectorAll(".sidenav a[href]");
    var hrefs = [];
    for (var i = 0; i < links.length; i++) hrefs.push(links[i].getAttribute("href") || "");
    var match = bestNavHref(hrefs, path);
    for (var j = 0; j < links.length; j++) {
      links[j].classList.remove("active");
    }
    var branches = sidebar.querySelectorAll(".sidenav details.branch");
    for (var k = 0; k < branches.length; k++) branches[k].classList.remove("active");
    if (!match) return;
    for (var m = 0; m < links.length; m++) {
      if (links[m].getAttribute("href") === match) {
        links[m].classList.add("active");
        var branch = links[m].closest ? links[m].closest("details.branch") : null;
        if (branch) {
          branch.classList.add("active");
          if (document.documentElement.getAttribute("data-nav") !== "rail") {
            branch.open = true;
          }
        }
        break;
      }
    }
  }

  function applySwapped(url, entry, push) {
    docURL = String(url).split("#")[0];
    document.dispatchEvent(new CustomEvent("evesynapse:content-will-swap", {
      detail: { url: url }
    }));
    // The nav drawer belongs to the page being left: a full load
    // used to close it for free, so a swap closes it too. app.js
    // syncs its controls off the toggle's change event.
    var drawer = document.getElementById("nav-drawer-toggle");
    if (drawer && drawer.checked) {
      drawer.checked = false;
      drawer.dispatchEvent(new CustomEvent("change", { bubbles: true }));
    }
    // The scroll lands with the paint — inside the transition
    // update callback, or immediately without one — so back and
    // forward arrive at the saved offset with no second jump.
    var paint = function () {
      dropHoistedModals();
      main.innerHTML = entry.main;
      hoistModals(main);
      if (entry.title) document.title = entry.title;
      syncActiveNav(new URL(url, window.location.href).pathname);
      if (push === "top") {
        window.scrollTo(0, 0);
      } else if (typeof push === "number") {
        window.scrollTo(0, push);
      }
    };
    var after = function () {
      try {
        main.focus({ preventScroll: true });
      } catch (e) {
        main.focus();
      }
      document.dispatchEvent(new CustomEvent("evesynapse:content-ready", {
        detail: { root: main, url: url }
      }));
    };
    if (!reducedMotion && document.startViewTransition) {
      // The transition name is armed only for the swap itself: a
      // standing view-transition-name on <main> makes it a backdrop
      // root, which silently disables backdrop-filter for every
      // glass surface inside it. While the transition runs the
      // content is a snapshot anyway, so the name costs nothing
      // there; the moment it finishes, the name comes off and the
      // glass blurs again.
      main.style.viewTransitionName = "main-content";
      var transition = document.startViewTransition(function () { paint(); });
      var disarm = function () {
        main.style.viewTransitionName = "";
        after();
      };
      transition.finished.then(disarm, disarm);
    } else {
      paint();
      after();
    }
  }

  function fullNav(url) {
    window.location.href = url;
  }

  function commitView(url) {
    try {
      window.fetch(url, {
        credentials: "same-origin",
        headers: { "Accept": "text/html", "X-Swap-Commit": "1" },
        keepalive: true
      }).catch(function () {});
    } catch (e) { /* the page still shows */ }
  }

  function navigate(url, opts) {
    opts = opts || {};
    if (navController && navController.abort) navController.abort();
    var hit = cacheGet(url);
    if (hit) {
      if (hit.prefetched) {
        // Opening a prefetched page is the view: tell the server
        // once, with a plain request whose answer is not needed.
        hit.prefetched = false;
        commitView(url);
      }
      if (opts.push !== false) {
        try {
          window.history.replaceState(
            { esSwap: 1, scroll: window.scrollY }, "", window.location.href);
          window.history.pushState({ esSwap: 1, scroll: 0 }, "", url);
        } catch (e) { /* private mode: the swap still lands */ }
      }
      applySwapped(url, hit, opts.push === false ? opts.scroll : "top");
      return;
    }
    var controller = null;
    if (window.AbortController) {
      controller = new AbortController();
      navController = controller;
    }
    fetchPage(url, controller ? controller.signal : null).then(function (page) {
      if (controller && navController !== controller) return; // superseded
      if (page.reason) {
        fullNav(url);
        return;
      }
      cachePut(url, page);
      if (opts.push !== false) {
        try {
          window.history.replaceState(
            { esSwap: 1, scroll: window.scrollY }, "", window.location.href);
          window.history.pushState({ esSwap: 1, scroll: 0 }, "", url);
        } catch (e) { /* the swap still lands */ }
      }
      applySwapped(url, page, opts.push === false ? opts.scroll : "top");
    }).catch(function (err) {
      if (err && err.name === "AbortError") return;
      fullNav(url);
    });
  }

  function prefetch(url) {
    if (cacheGet(url) || inflight[url]) return;
    if (isSlowConnection(window.navigator && window.navigator.connection)) return;
    inflight[url] = true;
    fetchPage(url, null, true).then(function (page) {
      delete inflight[url];
      if (page && !page.reason) {
        page.prefetched = true;
        cachePut(url, page);
      }
    }).catch(function () {
      delete inflight[url];
    });
  }

  function describeAnchor(a, ev) {
    var href = a.getAttribute("href") || "";
    var url;
    try {
      url = new URL(href, window.location.href);
    } catch (e) {
      return null;
    }
    var sameDoc = url.href.split("#")[0] === window.location.href.split("#")[0];
    return {
      url: url,
      verdict: shouldSwapClick({
        button: ev.button,
        metaKey: ev.metaKey,
        ctrlKey: ev.ctrlKey,
        shiftKey: ev.shiftKey,
        altKey: ev.altKey,
        target: a.getAttribute("target"),
        download: a.hasAttribute("download"),
        noSwap: a.hasAttribute("data-no-swap"),
        sameOrigin: url.origin === window.location.origin,
        hashOnly: sameDoc,
        path: url.pathname
      })
    };
  }

  document.addEventListener("click", function (ev) {
    if (ev.defaultPrevented) return;
    var a = ev.target && ev.target.closest ? ev.target.closest("a[href]") : null;
    if (!a || !document.contains(a)) return;
    var d = describeAnchor(a, ev);
    if (!d || !d.verdict) return;
    ev.preventDefault();
    navigate(d.url.href, { push: true });
  });

  function prefetchFromEvent(ev) {
    var t = ev.target;
    var a = null;
    if (t && t.closest) a = t.closest("a[href]");
    if (!a) return;
    var d = describeAnchor(a, { button: 0 });
    if (!d || !d.verdict || deniedPrefetch(d.url.pathname)) return;
    prefetch(d.url.href);
  }
  document.addEventListener("mouseover", prefetchFromEvent);
  document.addEventListener("focusin", prefetchFromEvent);

  // Remember where this shell started: popping back past the first
  // swap restores it from here instead of reloading.
  cachePut(window.location.href, {
    main: main.innerHTML,
    title: document.title
  });
  try {
    window.history.replaceState({ esSwap: 1, scroll: window.scrollY }, "", window.location.href);
  } catch (e) { /* private mode: back button still fetches */ }

  window.addEventListener("popstate", function (ev) {
    var url = window.location.href;
    // Fragment-only move: the same document with a different (or
    // no) hash. The browser has already scrolled (or will); there
    // is nothing to swap.
    if (url.split("#")[0] === docURL) return;
    var st = ev.state || {};
    var scroll = typeof st.scroll === "number" ? st.scroll : 0;
    var same = false;
    try {
      same = new URL(url).origin === window.location.origin;
    } catch (e) { same = false; }
    if (!same) return;
    var hit = cacheGet(url);
    if (hit) {
      applySwapped(url, hit, scroll);
      return;
    }
    navigate(url, { push: false, scroll: scroll });
  });
})();
