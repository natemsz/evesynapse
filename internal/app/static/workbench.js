// Intel Workbench: view tabs, d-scan grouping and grid filters,
// local grouping, standing and flag filters, the hull and pilot
// drawers, and the paste drawer. Everything renders server-side;
// this only shows, hides and posts. Loaded only on the
// workbench page (base.html).
(function () {
  "use strict";
  var root = document.querySelector(".wb");
  if (!root) return;

  // Stacked-bar widths ride data attributes; the CSP keeps
  // style attributes out of the markup (the scans.js pattern).
  root.querySelectorAll("[data-w]").forEach(function (b) {
    b.style.setProperty("--w", Math.max(0, Math.min(100, +b.getAttribute("data-w") || 0)) + "%");
  });

  function qsa(sel, el) { return Array.prototype.slice.call((el || root).querySelectorAll(sel)); }

  // --- Views ------------------------------------------------------------
  function showView(name) {
    qsa("[data-wb-view]").forEach(function (s) { s.hidden = s.getAttribute("data-wb-view") !== name; });
    qsa(".wb-tab").forEach(function (t) { t.classList.toggle("is-active", t.getAttribute("data-wb-tab") === name); });
  }
  qsa("[data-wb-tab]").forEach(function (b) {
    b.addEventListener("click", function () { showView(b.getAttribute("data-wb-tab")); });
  });

  // --- Filters ------------------------------------------------------------
  var gridFilter = "all", standingFilter = "all", flagFilter = "";
  var filterNames = { on: "On grid", off: "Off grid", dep: "Structures & probes",
    hostile: "Hostile", neutral: "Neutral", friendly: "Friendly",
    young: "Young alts (<90d)", recent: "Corp join <7d" };

  function applyFilters() {
    // D-scan rows, in whichever grouping panel is showing.
    qsa("[data-wb-dpanel]").forEach(function (panel) {
      var visible = 0;
      qsa("tr[data-on]", panel).forEach(function (row) {
        var on = +row.getAttribute("data-on") || 0, n = +row.getAttribute("data-n") || 0;
        var show = gridFilter === "all" ||
          (gridFilter === "on" && on > 0) ||
          (gridFilter === "off" && on < n) ||
          (gridFilter === "dep" && row.getAttribute("data-dep") === "1");
        row.hidden = !show;
        if (show) visible++;
      });
      panel.setAttribute("data-wb-visible", String(visible));
    });
    var dActive = root.querySelector("[data-wb-dpanel]:not([hidden])");
    var dNone = root.querySelector("[data-wb-dscan-none]");
    if (dNone) dNone.hidden = !(dActive && dActive.getAttribute("data-wb-visible") === "0");

    // Local rows in both groupings; an alliance section with no
    // visible pilots goes with them.
    var localVisible = 0;
    qsa(".wb-pilot").forEach(function (row) {
      var show = (standingFilter === "all" || row.getAttribute("data-bucket") === standingFilter) &&
        (!flagFilter || row.getAttribute("data-" + flagFilter) === "1");
      row.hidden = !show;
      if (show) localVisible++;
    });
    qsa("[data-wb-alli]").forEach(function (sec) {
      sec.hidden = !qsa(".wb-pilot", sec).some(function (r) { return !r.hidden; });
    });
    var lNone = root.querySelector("[data-wb-local-none]");
    if (lNone) lNone.hidden = localVisible > 0 || !root.querySelector(".wb-pilot");

    // The active-filters line.
    var active = [];
    if (gridFilter !== "all") active.push(filterNames[gridFilter]);
    if (standingFilter !== "all") active.push(filterNames[standingFilter]);
    if (flagFilter) active.push(filterNames[flagFilter]);
    var bar = root.querySelector("[data-wb-filters]");
    if (bar) {
      bar.hidden = active.length === 0;
      var list = bar.querySelector("[data-wb-filter-list]");
      if (list) list.textContent = active.join(" · ");
    }
  }

  function setMode(attr, btn, panelsAttr) {
    var name = btn.getAttribute(attr);
    qsa("[" + attr + "]").forEach(function (b) {
      if (b.classList.contains("wb-mode")) b.classList.toggle("is-active", b === btn);
    });
    qsa("[" + panelsAttr + "]").forEach(function (p) { p.hidden = p.getAttribute(panelsAttr) !== name; });
    applyFilters();
    return name;
  }
  qsa("[data-wb-dmode]").forEach(function (b) {
    b.addEventListener("click", function () { setMode("data-wb-dmode", b, "data-wb-dpanel"); });
  });
  qsa("[data-wb-lmode]").forEach(function (b) {
    b.addEventListener("click", function () { setMode("data-wb-lmode", b, "data-wb-lpanel"); });
  });
  qsa("[data-wb-grid]").forEach(function (b) {
    b.addEventListener("click", function () {
      gridFilter = b.getAttribute("data-wb-grid");
      qsa("[data-wb-grid].wb-mode").forEach(function (m) { m.classList.toggle("is-active", m === b); });
      applyFilters();
    });
  });
  qsa("[data-wb-standing]").forEach(function (b) {
    b.addEventListener("click", function () {
      standingFilter = b.getAttribute("data-wb-standing");
      qsa("[data-wb-standing]").forEach(function (m) { m.classList.toggle("is-active", m === b); });
      applyFilters();
    });
  });
  qsa("[data-wb-flag]").forEach(function (b) {
    b.addEventListener("click", function () {
      var f = b.getAttribute("data-wb-flag");
      flagFilter = flagFilter === f ? "" : f;
      applyFilters();
    });
  });
  var clearBtn = root.querySelector("[data-wb-clear-filters]");
  if (clearBtn) clearBtn.addEventListener("click", function () {
    gridFilter = "all"; standingFilter = "all"; flagFilter = "";
    qsa("[data-wb-grid].wb-mode").forEach(function (m) { m.classList.toggle("is-active", m.getAttribute("data-wb-grid") === "all"); });
    qsa("[data-wb-standing]").forEach(function (m) { m.classList.toggle("is-active", m.getAttribute("data-wb-standing") === "all"); });
    applyFilters();
  });

  // --- Drawers ------------------------------------------------------------
  // The shades are .modal-backdrop elements: the shell lifts
  // them to the body, out of the page glass, so they are looked
  // up on the document, not under the page root.
  function shades() { return qsa(".wb-shade", document); }
  function openDrawer(el) {
    closeDrawers();
    if (!el) return;
    el.hidden = false;
  }
  function closeDrawers() {
    shades().forEach(function (d) { d.hidden = true; });
  }
  shades().forEach(function (shade) {
    shade.addEventListener("click", function (e) { if (e.target === shade) closeDrawers(); });
  });
  qsa("[data-wb-open-hull]").forEach(function (row) {
    function open() { openDrawer(document.querySelector('[data-wb-hull-drawer="' + row.getAttribute("data-wb-open-hull") + '"]')); }
    row.addEventListener("click", open);
    row.addEventListener("keydown", function (e) { if (e.key === "Enter") open(); });
  });
  qsa("[data-wb-open-pilot]").forEach(function (row) {
    function open() { openDrawer(document.querySelector('[data-wb-pilot-drawer="' + row.getAttribute("data-wb-open-pilot") + '"]')); }
    row.addEventListener("click", open);
    row.addEventListener("keydown", function (e) { if (e.key === "Enter") open(); });
  });
  qsa("[data-wb-close]", document).forEach(function (b) { b.addEventListener("click", closeDrawers); });
  document.addEventListener("keydown", function (e) { if (e.key === "Escape") closeDrawers(); });

  // --- Paste drawer ---------------------------------------------------------
  var parser = document.querySelector("[data-wb-parser]");
  var openBtn = root.querySelector(".wb-open-parser");
  if (parser && openBtn) openBtn.addEventListener("click", function () { openDrawer(parser); });
  if (parser) {
    var pasteTa = parser.querySelector("[data-wb-paste]");
    var msg = parser.querySelector("[data-wb-parser-msg]");
    function showMsg(text) { if (msg) { msg.textContent = text; msg.hidden = !text; } }
    var clearInputs = parser.querySelector("[data-wb-clear-inputs]");
    if (clearInputs) clearInputs.addEventListener("click", function () {
      pasteTa.value = ""; showMsg("");
    });
    var parseBtn = parser.querySelector("[data-wb-parse]");
    parseBtn.addEventListener("click", function () {
      // One box, unsorted: the ingest classifies the paste
      // itself — d-scan rows, a local list, probe results,
      // or a mix — exactly as a channel paste is classified.
      var paste = pasteTa.value;
      if (!paste.trim()) { showMsg("Paste a scan first."); return; }
      if (parser.getAttribute("data-mode") === "scan") {
        // A plain form post: the scan tool's own submit, which
        // lands on the new scan's page.
        var form = document.createElement("form");
        form.method = "post";
        form.action = "/intel/scan/";
        var scanField = document.createElement("textarea");
        scanField.name = "scan"; scanField.value = paste; form.appendChild(scanField);
        var sysField = document.createElement("input");
        sysField.name = "system"; sysField.value = parser.getAttribute("data-system-name") || "";
        form.appendChild(sysField);
        document.body.appendChild(form);
        form.submit();
        return;
      }
      // Channel mode: post the paste as one report on this
      // snapshot's system, then open the report it made.
      parseBtn.disabled = true;
      showMsg("Posting…");
      var body = new URLSearchParams();
      body.set("text", paste);
      body.set("system_id", parser.getAttribute("data-system-id") || "");
      fetch(parser.getAttribute("data-post"), {
        method: "POST",
        credentials: "same-origin",
        headers: { "X-Requested-With": "XMLHttpRequest", "Content-Type": "application/x-www-form-urlencoded" },
        body: body.toString()
      }).then(function (r) { return r.json(); }).then(function (j) {
        if (j.reportId) { window.location.assign("/intel/workbench/" + j.reportId + "/"); return; }
        parseBtn.disabled = false;
        showMsg(j.message || "That did not go through.");
        if (j.ok) window.location.reload();
      }).catch(function () {
        parseBtn.disabled = false;
        showMsg("That did not go through. Please try again.");
      });
    });
  }

  // Deep links: a hash names the view or drawer to open with
  // the page (#diff, #system, #parser, #hull-<type>, #pilot-<id>).
  var hash = window.location.hash || "";
  if (hash === "#diff" || hash === "#system") showView(hash.slice(1));
  if (hash === "#parser" && parser) openDrawer(parser);
  var hullMatch = hash.match(/^#hull-(\d+)$/);
  if (hullMatch) openDrawer(document.querySelector('[data-wb-hull-drawer="' + hullMatch[1] + '"]'));
  var pilotMatch = hash.match(/^#pilot-(\d+)$/);
  if (pilotMatch) openDrawer(document.querySelector('[data-wb-pilot-drawer="' + pilotMatch[1] + '"]'));

  applyFilters();
})();
