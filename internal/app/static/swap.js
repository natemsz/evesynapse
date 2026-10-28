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
// A swap adopts whatever the fetched page carries that the shell
// lacks — its page stylesheet (the fetched <head> does not survive
// a <main>-only swap, so its links are re-created in the shell's
// head and pruned again when a later page does not carry them) and
// its page scripts (loaded in document order against the
// swapped-in content, before the content-ready event a full load
// would follow them with). Pages that redirect (an expired session
// bounces to /), pages without a <main>, auto-refresh pages, and
// pages whose assets fail to load always fall back to a full
// navigation — a failed swap must never strand anyone on stale,
// half-styled or inert content.
(function () {
  "use strict";

  // Link targets that always load whole: the fitting simulator and
  // saved fits (fit.js holds the editor's state in its one-time
  // closure and never reboots on content-ready, so a swapped-in
  // editor would sit inert), the SSO round-trips, and the character
  // switch (the shell's account area changes identity with it).
  var SWAP_DENY_EXACT = ["/fittings/"];
  var SWAP_DENY_PREFIX = ["/fittings/saved", "/auth/", "/characters/switch"];

  // Prefetch skips more than clicks do: the auto-refresh page
  // always falls back to a full load, so rendering it ahead of
  // time is a wasted server render. Clicks on it still work
  // through the normal fallback. The character switch is here
  // too: it changes who the session acts as, so it must never
  // run speculatively on a hover.
  var PREFETCH_DENY_PREFIX = ["/admin/sync/", "/characters/switch"];

  // Scripts the shell always carries. A fetched page needing
  // anything else (plan.js, map.js, push.js) has it adopted by the
  // swap; the shell set is listed so adoption never asks for a
  // script that is definitionally already running.
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
  //   { ok, redirected, hasMain, autorefresh }
  // Returns "" when the swap may proceed, else a short reason.
  // The assets a page needs are not a fallback question: the swap
  // adopts them, and only an asset that fails to load falls back,
  // at load time.
  function checkSwapResponse(r) {
    if (!r || !r.ok) return "bad-status";
    if (r.redirected) return "redirected";
    if (!r.hasMain) return "no-main";
    if (r.autorefresh) return "autorefresh";
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

  // Pure: the stylesheets a fetched page carries that the live
  // document does not. needed entries are { path } objects (or
  // plain path strings); loaded is a list of hrefs/pathnames in
  // any form — both sides run through scriptKey, so the versioned
  // ?v= href and the bare pathname compare equal. Edge-injected
  // sheets are never a page requirement, mirroring scriptsMissing.
  function stylesMissing(needed, loaded) {
    var seen = {};
    var i;
    for (i = 0; i < loaded.length; i++) seen[scriptKey(loaded[i])] = true;
    var out = [];
    for (i = 0; i < needed.length; i++) {
      var item = needed[i];
      var key = scriptKey(item && item.path ? item.path : item);
      if (!key || key.indexOf("/cdn-cgi/") === 0) continue;
      if (seen[key]) continue;
      seen[key] = true;
      out.push(item);
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

  // An image carrying data-fallback-src trades its src for the
  // fallback, exactly once: the attribute comes off as the src
  // goes on, so a fallback that also fails cannot loop. The
  // argument is element-shaped (getAttribute / removeAttribute /
  // src), so the rule stays unit-testable without a DOM.
  function applyImgFallback(img) {
    if (!img || typeof img.getAttribute !== "function") return false;
    var fb = img.getAttribute("data-fallback-src");
    if (!fb) return false;
    img.removeAttribute("data-fallback-src");
    img.src = fb;
    return true;
  }

  // Pure: has this image already finished loading — and failed?
  // A completed image holding no pixels will never fire another
  // event worth waiting for, so it is swept rather than listened
  // for.
  function imgSettledFailed(img) {
    return !!img && img.complete === true && img.naturalWidth === 0;
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports = {
      shouldSwapClick: shouldSwapClick,
      checkSwapResponse: checkSwapResponse,
      scriptKey: scriptKey,
      scriptsMissing: scriptsMissing,
      stylesMissing: stylesMissing,
      bestNavHref: bestNavHref,
      normalizePath: normalizePath,
      isSlowConnection: isSlowConnection,
      applyImgFallback: applyImgFallback,
      imgSettledFailed: imgSettledFailed,
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

  // Image fallbacks: EVE's image server answers a render only
  // for types that have one, so a page leads with the render and
  // names the icon in data-fallback-src (the item page's art
  // tile). A resource error does not bubble, so the listener
  // sits in the capture phase; images that already failed
  // before this script ran are swept once here. Templates
  // cannot do this themselves — the CSP allows no inline event
  // handlers, which is why the swap lives here.
  document.addEventListener("error", function (ev) {
    var t = ev.target;
    if (t && t.tagName === "IMG") applyImgFallback(t);
  }, true);
  var fallbackImgs = document.querySelectorAll("img[data-fallback-src]");
  for (var fi = 0; fi < fallbackImgs.length; fi++) {
    if (imgSettledFailed(fallbackImgs[fi])) applyImgFallback(fallbackImgs[fi]);
  }

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

  // A page that changes server state without navigating (the
  // home customize saves post their layout and stay put) says
  // so here: every cached copy predates the change, so the
  // cache empties instead of a later swap painting the stale
  // page back over the saved one.
  document.addEventListener("evesynapse:content-mutated", function () {
    pageCache = {};
    pageCacheOrder = [];
  });

  var inflight = {}; // url -> true while a prefetch is running
  var navController = null; // AbortController for the live navigation
  var navSeq = 0; // bumped per navigation; stale swaps stop quietly

  // A document's external scripts as { path, src }, in document
  // order: path is the comparison form, src the literal attribute
  // an adopted <script> must carry (version query included).
  function pageScripts(doc) {
    var out = [];
    if (!doc || !doc.querySelectorAll) return out;
    var tags = doc.querySelectorAll("script[src]");
    for (var i = 0; i < tags.length; i++) {
      var src = (tags[i].getAttribute && tags[i].getAttribute("src")) || tags[i].src || "";
      var path = scriptKey(src);
      if (path) out.push({ path: path, src: src });
    }
    return out;
  }

  // A document's stylesheet links as { href, path, media }, in
  // document order — the page assets a <main>-only swap would
  // otherwise throw away with the fetched <head>.
  function pageStyles(doc) {
    var out = [];
    if (!doc || !doc.querySelectorAll) return out;
    var links = doc.querySelectorAll('link[rel="stylesheet"]');
    for (var i = 0; i < links.length; i++) {
      var href = (links[i].getAttribute && links[i].getAttribute("href")) || links[i].href || "";
      var path = scriptKey(href);
      if (!path || path.indexOf("/cdn-cgi/") === 0) continue;
      out.push({
        href: href, path: path,
        media: (links[i].getAttribute && links[i].getAttribute("media")) || ""
      });
    }
    return out;
  }

  function loadedScriptPathnames() {
    return pageScripts(document).map(function (s) { return s.path; });
  }

  // ---------- asset adoption ----------

  // A page asset the shell waits on gets this long; past it the
  // swap gives up and the browser loads the page in full.
  var ASSET_LOAD_TIMEOUT_MS = 6000;

  // The stylesheets the document booted with, captured lazily at
  // the first adoption, while the live document is still the boot
  // one. Together with the sheets the swap itself adopts, they
  // mark the links the swap may prune: a link that appears any
  // other way (an edge injection, another script's doing) is not
  // the swap's to remove.
  var bootStylePaths = null;
  function liveStyleLinks() {
    var head = document.head;
    if (!head || !head.querySelectorAll) return [];
    return head.querySelectorAll('link[rel="stylesheet"]');
  }
  function loadedStylePaths() {
    var out = [];
    var links = liveStyleLinks();
    for (var i = 0; i < links.length; i++) {
      var href = (links[i].getAttribute && links[i].getAttribute("href")) || links[i].href || "";
      var p = scriptKey(href);
      if (p) out.push(p);
    }
    return out;
  }
  function ensureBootStyles() {
    if (!bootStylePaths) bootStylePaths = loadedStylePaths();
    return bootStylePaths;
  }

  function loadStylesheet(style) {
    return new Promise(function (resolve, reject) {
      var el = document.createElement("link");
      el.rel = "stylesheet";
      el.href = style.href;
      if (style.media) el.media = style.media;
      el.setAttribute("data-swap-style", style.path);
      var done = false;
      var timer = setTimeout(function () {
        if (!done) { done = true; reject(new Error("stylesheet timeout")); }
      }, ASSET_LOAD_TIMEOUT_MS);
      el.onload = function () { if (!done) { done = true; clearTimeout(timer); resolve(); } };
      el.onerror = function () { if (!done) { done = true; clearTimeout(timer); reject(new Error("stylesheet failed")); } };
      // Page sheets sit before phone.css in a full load's head;
      // an adopted sheet takes the same seat, so the cascade
      // resolves exactly as the server rendered it.
      var phone = null;
      var links = liveStyleLinks();
      for (var i = 0; i < links.length; i++) {
        var href = (links[i].getAttribute && links[i].getAttribute("href")) || links[i].href || "";
        if (scriptKey(href) === "/static/phone.css") { phone = links[i]; break; }
      }
      if (phone && phone.parentNode) phone.parentNode.insertBefore(el, phone);
      else document.head.appendChild(el);
    });
  }

  // adoptStyles(styles): load every sheet the page carries that
  // the live document lacks, and wait for them — the swap paints
  // only once the new page's styles are in, so it never flashes
  // unstyled. Rejects when a sheet fails or stalls.
  function adoptStyles(styles) {
    ensureBootStyles();
    var missing = stylesMissing(styles || [], loadedStylePaths());
    if (!missing.length) return Promise.resolve();
    return Promise.all(missing.map(loadStylesheet));
  }

  // pruneStyles(styles): drop the sheets the incoming page does
  // not carry — adopted ones and the boot page's own alike. The
  // shell's sheets survive every prune because every page's head
  // lists them; a page sheet (the killboard's, the map's) leaves
  // with its page, so the shell does not accumulate every sheet
  // it has ever shown. Runs after the paint, so the outgoing page
  // keeps its styles through the transition that carries it away.
  function pruneStyles(styles) {
    var keep = {};
    var i, k;
    var wanted = styles || [];
    for (i = 0; i < wanted.length; i++) {
      k = scriptKey(wanted[i] && wanted[i].path ? wanted[i].path : wanted[i]);
      if (k) keep[k] = true;
    }
    var boot = {};
    var bootPaths = ensureBootStyles();
    for (i = 0; i < bootPaths.length; i++) boot[bootPaths[i]] = true;
    var links = liveStyleLinks();
    for (i = 0; i < links.length; i++) {
      var adopted = links[i].getAttribute ? links[i].getAttribute("data-swap-style") : null;
      var href = (links[i].getAttribute && links[i].getAttribute("href")) || links[i].href || "";
      var path = adopted || scriptKey(href);
      if (!path || keep[path]) continue;
      if (adopted || boot[path]) links[i].remove();
    }
  }

  function loadScript(item) {
    return new Promise(function (resolve, reject) {
      var el = document.createElement("script");
      el.src = item.src;
      el.async = false; // run in the order the page lists them
      var done = false;
      var timer = setTimeout(function () {
        if (!done) { done = true; reject(new Error("script timeout")); }
      }, ASSET_LOAD_TIMEOUT_MS);
      el.onload = function () { if (!done) { done = true; clearTimeout(timer); resolve(); } };
      el.onerror = function () { if (!done) { done = true; clearTimeout(timer); reject(new Error("script failed")); } };
      (document.body || document.head).appendChild(el);
    });
  }

  // adoptScripts(scripts): load the page scripts the shell has
  // not got, one at a time in document order. Runs after the
  // paint, against the swapped-in content: a page script's
  // one-time initialization sees its own page, exactly as on a
  // full load, and the content-ready dispatch that follows it
  // reboots the ones that listen for it. Rejects when a script
  // fails or stalls. Scripts are never unloaded — once adopted,
  // a script stays for the session and scriptsMissing simply
  // stops asking for it.
  function adoptScripts(scripts) {
    var list = scripts || [];
    var missing = scriptsMissing(
      list.map(function (s) { return s.path; }),
      loadedScriptPathnames());
    if (!missing.length) return Promise.resolve();
    var byPath = {};
    for (var i = 0; i < list.length; i++) {
      if (!byPath[list[i].path]) byPath[list[i].path] = list[i];
    }
    var chain = Promise.resolve();
    missing.forEach(function (path) {
      chain = chain.then(function () { return loadScript(byPath[path]); });
    });
    return chain;
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
        var reason = checkSwapResponse({
          ok: true,
          redirected: false,
          hasMain: !!next,
          autorefresh: !!(body && body.getAttribute("data-autorefresh"))
        });
        if (reason) return { reason: reason };
        var titleEl = doc.querySelector("title");
        return {
          main: next.innerHTML,
          title: titleEl ? titleEl.textContent : "",
          styles: pageStyles(doc),
          scripts: pageScripts(doc)
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
    // applySwapped resolves once the new content is painted and
    // settled. The content-ready dispatch is not its tail any
    // more: performSwap fires it after the page's scripts are
    // adopted, so a page script is listening (or has run) by the
    // time widgets reboot — the sequence a full load produces.
    return new Promise(function (resolve) {
      var after = function () {
        try {
          main.focus({ preventScroll: true });
        } catch (e) {
          main.focus();
        }
        resolve();
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
    });
  }

  // performSwap(url, entry, push, isCurrent) — one complete swap:
  // adopt the page's stylesheets, paint, adopt its missing
  // scripts, prune the sheets the new page does not carry, then
  // fire content-ready. An asset that fails or stalls hands the
  // navigation to the browser instead of stranding a half-styled
  // or inert page. isCurrent() reports whether a newer navigation
  // has superseded this one; a superseded swap stops quietly.
  // fontsSettled resolves once the document's fonts have loaded,
  // or after a short cap. The swap transition snapshots the new
  // content as it stands, so text caught mid font swap is baked
  // into the snapshot in the fallback face and pops when the real
  // face lands. The fonts are preloaded and cached, so ready is
  // normally already settled and this costs nothing; the cap
  // keeps a stalled font from holding a navigation hostage.
  function fontsSettled() {
    if (!document.fonts || !document.fonts.ready) return Promise.resolve();
    return Promise.race([
      document.fonts.ready,
      new Promise(function (resolve) { setTimeout(resolve, 120); })
    ]);
  }

  function performSwap(url, entry, push, isCurrent) {
    var current = function () { return !isCurrent || isCurrent(); };
    return adoptStyles(entry.styles).then(function () {
      if (!current()) return false;
      return fontsSettled();
    }).then(function () {
      if (!current()) return false;
      return applySwapped(url, entry, push);
    }).then(function (painted) {
      if (painted === false || !current()) return false;
      return adoptScripts(entry.scripts);
    }).then(function () {
      if (!current()) return false;
      pruneStyles(entry.styles);
      document.dispatchEvent(new CustomEvent("evesynapse:content-ready", {
        detail: { root: main, url: url }
      }));
      return true;
    }).catch(function () {
      if (!current()) return false;
      fullNav(url);
      return false;
    });
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
    var mySeq = ++navSeq;
    var isCurrent = function () { return mySeq === navSeq; };
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
      performSwap(url, hit, opts.push === false ? opts.scroll : "top", isCurrent);
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
      performSwap(url, page, opts.push === false ? opts.scroll : "top", isCurrent);
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
  // swap restores it from here instead of reloading. Its assets
  // ride along so the restore prunes and adopts like any swap.
  // The snapshot reads main as the server rendered it, modals
  // inside, the shape every fetched page's entry has: the first
  // hoist runs after it, because a snapshot taken once the modals
  // had left main would restore a page with no modals at all.
  cachePut(window.location.href, {
    main: main.innerHTML,
    title: document.title,
    styles: pageStyles(document),
    scripts: pageScripts(document)
  });
  hoistModals(document);
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
      // A cached restore still runs the full swap: the page's
      // sheet may have been pruned while it was away.
      var mySeq = ++navSeq;
      performSwap(url, hit, scroll, function () { return mySeq === navSeq; });
      return;
    }
    navigate(url, { push: false, scroll: scroll });
  });
})();
