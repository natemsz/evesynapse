// The intel workbench as a dashboard (intel_workbench.html): the
// thread rows' live poll, a row's report opened in place below them,
// and the map that follows. The page works without this file: every
// Details is a plain link to the same page with that report open.
(function () {
  "use strict";

  /**
   * One row of /intel/threads.json (threadRow in intel_dashboard.go).
   * @typedef {{id: number, report: number, n: number, count?: number, noun?: string, clear?: boolean,
   *   sys: number, sysName: string, sec: string, tier: number,
   *   systems: number, kind: string, by?: string, chn?: string,
   *   at: number, ago: string}} ThreadRow
   */

  // --- Seen threads ---
  // Which report of each thread this browser has opened: a thread
  // whose newest report was viewed stops pinging. A per-browser
  // convenience; without storage every recent thread pings.
  var SEEN_KEY = "evesynapse.intel.seen";
  function readSeen() {
    try { return JSON.parse(window.localStorage.getItem(SEEN_KEY) || "{}") || {}; } catch (e) { return {}; }
  }
  function markSeen(thread, report) {
    var s = readSeen();
    s[thread] = report;
    var keys = Object.keys(s);
    // Bounded: the oldest threads leave first (ids grow).
    if (keys.length > 300) keys.sort(function (a, b) { return a - b; }).slice(0, keys.length - 300).forEach(function (k) { delete s[k]; });
    try { window.localStorage.setItem(SEEN_KEY, JSON.stringify(s)); } catch (e) { /* private mode */ }
  }

  // shouldPing is whether a thread's newest report still pings: it
  // is recent and this browser has not opened it.
  function shouldPing(row, seen, nowSec, recentSec) {
    return nowSec - row.at < recentSec && seen[row.id] !== row.report;
  }

  // --- Rows ---
  function text(doc, tag, cls, s) {
    var el = doc.createElement(tag);
    if (cls) el.className = cls;
    if (s) el.textContent = s;
    return el;
  }
  function whatText(r) {
    if (r.clear) return "Clear";
    if (r.n) return r.n + (r.n === 1 ? " hostile" : " hostiles");
    if (r.count && r.noun) return r.count + " " + r.noun + (r.count === 1 ? "" : "s");
    return "Hostiles";
  }
  function hhmm(sec) {
    var d = new Date(sec * 1000);
    return String(d.getUTCHours()).padStart(2, "0") + ":" + String(d.getUTCMinutes()).padStart(2, "0");
  }
  /** @param {Document} doc @param {ThreadRow} r */
  function rowEl(doc, r) {
    var li = text(doc, "li", "wbd-row");
    li.setAttribute("data-thread", String(r.id));
    li.setAttribute("data-report", String(r.report));
    li.setAttribute("data-sys", String(r.sys));
    li.setAttribute("data-at", String(r.at));
    var what = text(doc, "span", "wbd-what", whatText(r) + " in ");
    what.appendChild(text(doc, "b", "", r.sysName));
    if (r.systems > 1) {
      what.appendChild(doc.createTextNode(" "));
      what.appendChild(text(doc, "span", "muted", "· " + r.systems + " systems"));
    }
    li.appendChild(what);
    var sep = function () { var s = text(doc, "span", "wbd-sep", "|"); s.setAttribute("aria-hidden", "true"); return s; };
    li.appendChild(sep());
    var by = text(doc, "span", "wbd-by", r.kind + (r.by ? " by " + r.by : "") + " in ");
    by.appendChild(text(doc, "span", "sec sec-" + r.tier, r.sec));
    by.appendChild(doc.createTextNode(" " + r.sysName + " at "));
    var t = text(doc, "time", "", hhmm(r.at));
    t.dateTime = new Date(r.at * 1000).toISOString();
    by.appendChild(t);
    if (r.chn) {
      by.appendChild(doc.createTextNode(" "));
      by.appendChild(text(doc, "span", "muted", "· " + r.chn));
    }
    li.appendChild(by);
    li.appendChild(sep());
    var a = text(doc, "a", "wbd-details", "Details");
    a.href = "/intel/workbench/" + r.report + "/";
    a.setAttribute("data-wbd-open", "");
    li.appendChild(a);
    return li;
  }
  function rowOf(li) {
    return { id: +li.getAttribute("data-thread"), report: +li.getAttribute("data-report"),
      sys: +li.getAttribute("data-sys"), at: +li.getAttribute("data-at") };
  }

  // --- The dashboard ---
  function bind(root, wbd) {
    var doc = root.ownerDocument || root;
    var list = wbd.querySelector("[data-wbd-list]");
    var main = wbd.querySelector("[data-wbd-main]");
    var mapEl = wbd.querySelector("[data-map]");
    var url = wbd.getAttribute("data-wbd-threads-url");
    var recent = +wbd.getAttribute("data-wbd-recent") || 1800;

    // The map takes events once it has mounted; until then they wait.
    var queued = [];
    function toMap(name, detailObj) {
      if (!mapEl) return;
      if (mapEl.getAttribute("data-map-ready")) {
        mapEl.dispatchEvent(new CustomEvent(name, { detail: detailObj }));
      } else {
        queued.push([name, detailObj]);
      }
    }
    if (mapEl) {
      mapEl.addEventListener("evesynapse:map-ready", function () {
        queued.splice(0).forEach(function (q) { mapEl.dispatchEvent(new CustomEvent(q[0], { detail: q[1] })); });
      });
    }
    function ping(r, fresh) {
      toMap("evesynapse:map-ping", { system: r.sys, id: r.id, at: r.at, recent: recent, fresh: fresh });
    }

    // First paint: the recent, unopened threads breathe on the map,
    // and the map opens on the newest.
    var seen = readSeen(), nowSec = Date.now() / 1000;
    var rows = list ? list.querySelectorAll("li[data-thread]") : [];
    rows.forEach(function (li) {
      var r = rowOf(li);
      if (shouldPing(r, seen, nowSec, recent)) ping(r, false);
    });
    // The map opens on the open report's thread, else the newest.
    var first = list && (list.querySelector("li.is-open") || rows[0]);
    if (first) toMap("evesynapse:map-focus", { system: rowOf(first).sys });

    // --- Details, in place ---
    // A row's Details loads that report's page and puts its
    // breakdown where the open one stands, below the list: the
    // list, the map and the log stay put. The address follows (it
    // is the report's own page), replaced rather than pushed, so
    // Back leaves the workbench as it came.
    var opening = 0, openThread = 0;
    function markOpen(thread) {
      openThread = thread;
      if (!list) return;
      list.querySelectorAll("li.is-open").forEach(function (li) { li.classList.remove("is-open"); });
      var li = thread && list.querySelector('li[data-thread="' + thread + '"]');
      if (li) li.classList.add("is-open");
    }
    function viewed(r) {
      markSeen(r.id, r.report);
      toMap("evesynapse:map-viewed", { id: r.id });
      toMap("evesynapse:map-focus", { system: r.sys });
    }
    function open(href, r) {
      var n = ++opening;
      var cur = main.querySelector(".wb, .wbd-none");
      main.setAttribute("aria-busy", "true");
      markOpen(r.id);
      viewed(r);
      try { history.replaceState(history.state, "", href); } catch (e) { /* sandboxed */ }
      fetch(href, { credentials: "same-origin" }).then(function (resp) {
        return resp.ok ? resp.text() : Promise.reject(resp.status);
      }).then(function (html) {
        if (n !== opening || !main.isConnected) return;
        var wb = new DOMParser().parseFromString(html, "text/html").querySelector("[data-wbd-main] .wb");
        if (!wb) return Promise.reject(0);
        var fresh = doc.importNode(wb, true);
        if (cur && cur.parentNode === main) main.replaceChild(fresh, cur); else main.appendChild(fresh);
        main.removeAttribute("aria-busy");
        // The breakdown's own widgets (workbench.js) bind to it.
        doc.dispatchEvent(new CustomEvent("evesynapse:content-ready", { detail: { root: main } }));
        if (main.getBoundingClientRect().top > window.innerHeight * 0.6) main.scrollIntoView({ block: "start", behavior: "smooth" });
      }).catch(function () {
        // A failed fetch falls back to the plain page load.
        if (n === opening) location.href = href;
      });
    }
    wbd.addEventListener("click", function (e) {
      var a = e.target.closest && e.target.closest("a[data-wbd-open]");
      if (!a || e.button || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return; // a new tab is a plain link
      var li = a.closest("li[data-thread]");
      if (!li) return;
      e.preventDefault();
      open(a.getAttribute("href"), rowOf(li));
    }, true);
    // The report open on arrival counts as viewed.
    var openLi = list && list.querySelector("li.is-open");
    if (openLi) { openThread = rowOf(openLi).id; viewed(rowOf(openLi)); }

    // --- The live poll ---
    var polling = false;
    function poll() {
      if (polling || document.visibilityState !== "visible" || !wbd.isConnected || !url) return;
      polling = true;
      fetch(url, { credentials: "same-origin" }).then(function (r) {
        return r.ok ? r.json() : { threads: [] };
      }).then(function (d) {
        /** @type {ThreadRow[]} */
        var got = (d && d.threads) || [];
        if (!list || !got.length) return;
        var empty = list.querySelector("[data-wbd-empty]");
        if (empty) empty.remove();
        var seenNow = readSeen(), now = Date.now() / 1000;
        // Newest first from the server: the list is rebuilt in its
        // order, rows kept where they stand, new reports pinged.
        var frag = doc.createDocumentFragment();
        got.forEach(function (r) {
          var old = list.querySelector('li[data-thread="' + r.id + '"]');
          var changed = !old || +old.getAttribute("data-report") !== r.report;
          var li = changed ? rowEl(doc, r) : old;
          if (changed) {
            li.classList.add("new");
            if (shouldPing(r, seenNow, now, recent)) ping(r, true);
          }
          frag.appendChild(li);
        });
        list.textContent = "";
        list.appendChild(frag);
        markOpen(openThread);
      }).catch(function () { /* the next poll tries again */ })
        .then(function () { polling = false; });
    }
    var timer = setInterval(poll, 10000);
    function onVisible() { if (document.visibilityState === "visible") poll(); }
    doc.addEventListener("visibilitychange", onVisible);
    doc.addEventListener("evesynapse:content-will-swap", function stop() {
      clearInterval(timer);
      doc.removeEventListener("visibilitychange", onVisible);
      doc.removeEventListener("evesynapse:content-will-swap", stop);
    });
  }

  function init(root) {
    var wbd = root.querySelector && root.querySelector("[data-wbd]");
    if (!wbd || wbd.getAttribute("data-bound")) return;
    wbd.setAttribute("data-bound", "1");
    bind(root, wbd);
  }

  if (typeof window !== "undefined" && typeof document !== "undefined") {
    if (document.readyState === "loading") {
      document.addEventListener("DOMContentLoaded", function () { init(document); });
    } else {
      init(document);
    }
    if (window.EveSynapseContent && window.EveSynapseContent.onReady) {
      window.EveSynapseContent.onReady(init);
    }
  }

  // Exposed for the node tests only (jstest/dashboard_test.js).
  if (typeof module !== "undefined" && module.exports) {
    module.exports = { shouldPing: shouldPing, whatText: whatText, hhmm: hhmm };
  }
})();
