// EveSynapse progressive enhancements. Everything here only ever
// *adds* behavior: with JavaScript disabled the pages render and
// work exactly as before (the nav is <details>-driven, the market
// form submits, panels are simply unfolded).
(function () {
  "use strict";

  // --- Content-ready bus --------------------------------------
  // The persistent shell (swap.js) replaces #main-content without
  // a page load. Page widgets subscribe through EveSynapseContent
  // and initialize the fresh markup; the same initializer runs
  // once for the first paint. Shell-level code (sidebar, top bar,
  // palette, notifications) binds once and never re-runs. A widget
  // that binds document-level listeners or timers must guard them
  // itself: element bindings are safe (old nodes are discarded
  // with the swap), global ones are not.
  window.EveSynapseContent = {
    onReady: function (fn) {
      document.addEventListener("evesynapse:content-ready", function (ev) {
        fn((ev.detail && ev.detail.root) || document);
      });
    }
  };
  function onContentReady(fn) {
    window.EveSynapseContent.onReady(fn);
  }

  // --- Modal dialogs ---------------------------------------------
  // One implementation behind every pop-up — add a module, the fit
  // picker, the planner's ME and settings dialogs, quick jump: a
  // .modal-backdrop around a .modal, opened and closed through
  // EveSynapseModal. open() shows the backdrop in the browser's top
  // layer (a manual popover), so its blur covers the whole page —
  // top bar and sidebar included — whatever stacking the content
  // sits in, without moving the dialog out of its form. It
  // remembers the focus to give back and moves focus in; close()
  // hides it and gives the focus back. A click on the backdrop
  // itself, on any [data-close-modal] inside, or Escape closes the
  // topmost one; opts.onClose runs on every close, however it came.
  var modalStack = [];
  function modalIndex(el) {
    for (var i = modalStack.length - 1; i >= 0; i--) if (modalStack[i].el === el) return i;
    return -1;
  }
  function topLayer(el, show) {
    if (!el.showPopover) return;
    if (!el.hasAttribute("popover")) el.setAttribute("popover", "manual");
    try {
      if (show) el.showPopover(); else el.hidePopover();
    } catch (err) { /* already in that state */ }
  }
  var EveSynapseModal = window.EveSynapseModal = {
    open: function (el, opts) {
      if (!el || modalIndex(el) >= 0) return;
      opts = opts || {};
      modalStack.push({ el: el, back: document.activeElement, onClose: opts.onClose });
      el.hidden = false;
      topLayer(el, true);
      document.documentElement.classList.add("modal-open");
      if (opts.focus !== false) {
        var first = el.querySelector("[autofocus], input:not([type=hidden]), select, textarea, .modal-item, button:not([data-close-modal])") ||
          el.querySelector("[data-close-modal], button");
        if (first && first.focus) first.focus({ preventScroll: true });
      }
    },
    close: function (el) {
      if (!el) return;
      var i = modalIndex(el);
      el.hidden = true;
      topLayer(el, false);
      if (i < 0) return;
      var entry = modalStack.splice(i, 1)[0];
      if (!modalStack.length) document.documentElement.classList.remove("modal-open");
      if (entry.onClose) entry.onClose();
      if (entry.back && entry.back.focus && document.contains(entry.back)) entry.back.focus({ preventScroll: true });
    },
    isOpen: function (el) { return modalIndex(el) >= 0; }
  };
  document.addEventListener("click", function (ev) {
    var t = ev.target;
    if (!t || !t.classList) return;
    if (t.classList.contains("modal-backdrop")) { EveSynapseModal.close(t); return; }
    var closer = t.closest && t.closest("[data-close-modal]");
    var box = closer && closer.closest(".modal-backdrop");
    if (box) EveSynapseModal.close(box);
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Escape" || !modalStack.length || ev.defaultPrevented) return;
    ev.preventDefault();
    EveSynapseModal.close(modalStack[modalStack.length - 1].el);
  });
  // A dialog in content that is being swapped out closes with it.
  document.addEventListener("evesynapse:content-will-swap", function () {
    var main = document.getElementById("main-content");
    for (var i = modalStack.length - 1; i >= 0; i--) {
      if (main && main.contains(modalStack[i].el)) EveSynapseModal.close(modalStack[i].el);
    }
  });

  // --- Section tabs --------------------------------------------
  // Below the fit width the tab strip (and a grouped section's
  // sub-row) is a sideways-scrolling rail; start with the current
  // page's tab in view rather than off the right edge.
  function revealSectionTab(root) {
    var strips = root.querySelectorAll(".section-tabs, .section-subtabs");
    for (var i = 0; i < strips.length; i++) {
      var strip = strips[i], on = strip.querySelector("a.active");
      if (!on || strip.scrollWidth <= strip.clientWidth) continue;
      strip.scrollLeft = on.offsetLeft - strip.offsetLeft - (strip.clientWidth - on.offsetWidth) / 2;
    }
  }
  onContentReady(revealSectionTab);
  revealSectionTab(document);

  // --- Collapsible sections -----------------------------------
  // Folding lives at the smallest sensible unit on each page:
  //   - .foldable sections (the h2/h3 blocks inside a page panel)
  //     fold independently, section by section;
  //   - .card widgets fold whole (Home widgets, Sync cards);
  //   - the page .panel itself folds whole only when it wraps no
  //     .foldable section and no cards — single-block pages like
  //     Killmails or Wars, where the whole window IS the section.
  // Sections marked data-fold="closed" start folded; that state
  // is applied here, on load, so with JavaScript disabled every
  // section is simply visible — folding is pure enhancement and
  // nothing is ever hidden in the raw markup.
  function setFolded(container, btn, folded) {
    container.classList.toggle("folded", folded);
    btn.textContent = folded ? "+" : "–"; // + when folded, en dash to fold
    btn.setAttribute("aria-expanded", folded ? "false" : "true");
    btn.setAttribute("aria-label", folded ? "Expand section" : "Collapse section");
  }

  function addFold(container, heading) {
    if (!container || !heading) return;
    if (heading.querySelector(":scope > .foldbtn")) return;
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "foldbtn";
    btn.addEventListener("click", function () {
      setFolded(container, btn, !container.classList.contains("folded"));
    });
    heading.appendChild(btn);
    container.classList.add("has-fold");
    setFolded(container, btn, container.getAttribute("data-fold") === "closed");
  }

  // Fold buttons are added per node, so re-running over fresh
  // swapped markup is safe; the old nodes (and their buttons)
  // are discarded with the swap.
  function initFolds(root) {
    var foldables = root.querySelectorAll(".foldable");
    for (var i = 0; i < foldables.length; i++) {
      // data-fold="never": keep the foldable section styling but
      // don't offer a fold button (Issue 17).
      if (foldables[i].getAttribute("data-fold") === "never") continue;
      addFold(foldables[i], foldables[i].querySelector(":scope > h2, :scope > h3"));
    }

    var cards = root.querySelectorAll(".card");
    for (var j = 0; j < cards.length; j++) {
      // Home modules expand instead of folding (see Expanders).
      if (cards[j].closest(".home-grid")) continue;
      // data-fold="never" on a card: one window that is the page (the
      // sign-up question), nothing to fold.
      if (cards[j].getAttribute("data-fold") === "never") continue;
      addFold(cards[j], cards[j].querySelector(":scope > h2, :scope > h3"));
    }
  }
  onContentReady(initFolds);
  initFolds(document);

  // --- Confirmed forms ------------------------------------------
  // A form carrying data-confirm asks before it submits (CSP rules
  // out inline handlers). Delegated, so swapped pages need nothing.
  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    var q = form && form.getAttribute ? form.getAttribute("data-confirm") : null;
    if (q && !window.confirm(q)) ev.preventDefault();
  }, true);

  // --- Tabs ----------------------------------------------------
  // A [data-tabs] tablist renders hidden, its panels all showing
  // under their own headings: that is the page without JavaScript.
  // Here the list appears, one panel shows at a time, and the
  // container gets .tabs-on so the CSS can drop the per-panel
  // headings the tabs now carry. Arrow keys move along the list; a
  // URL hash naming a panel opens it.
  function initTabs(root) {
    var lists = root.querySelectorAll("[data-tabs]");
    for (var i = 0; i < lists.length; i++) setupTabs(lists[i]);
  }
  function setupTabs(list) {
    if (list.getAttribute("data-tabs-ready")) return;
    list.setAttribute("data-tabs-ready", "1");
    var tabs = Array.prototype.slice.call(list.querySelectorAll('[role="tab"]'));
    if (!tabs.length) return;
    function panelOf(tab) { return document.getElementById(tab.getAttribute("aria-controls")); }
    function select(tab, focus) {
      tabs.forEach(function (t) {
        var on = t === tab;
        t.setAttribute("aria-selected", on ? "true" : "false");
        t.tabIndex = on ? 0 : -1;
        var p = panelOf(t);
        if (p) p.hidden = !on;
      });
      if (focus) tab.focus();
    }
    tabs.forEach(function (t, n) {
      t.addEventListener("click", function () { select(t, false); });
      t.addEventListener("keydown", function (ev) {
        var k = ev.key, to = -1;
        if (k === "ArrowRight") to = (n + 1) % tabs.length;
        else if (k === "ArrowLeft") to = (n - 1 + tabs.length) % tabs.length;
        else if (k === "Home") to = 0;
        else if (k === "End") to = tabs.length - 1;
        if (to < 0) return;
        ev.preventDefault();
        select(tabs[to], true);
      });
    });
    var start = tabs[0];
    var hash = window.location.hash ? window.location.hash.slice(1) : "";
    tabs.forEach(function (t) {
      if (t.getAttribute("aria-controls") === hash) start = t;
    });
    list.hidden = false;
    if (list.parentElement) list.parentElement.classList.add("tabs-on");
    select(start, false);
  }
  onContentReady(initTabs);
  initTabs(document);

  // --- Expanders -------------------------------------------------
  // One mechanism for every summary/detail pair. A detail block
  // carries data-wmore; its control carries data-expand naming the
  // block's id. Home modules get theirs here: a module whose markup
  // has a detail block rests on its summary, with a More/Less
  // toggle in its heading, and takes its whole grid row while
  // open. Table rows with a detail row of their own (the fleet
  // overview) render their control server-side and use the same
  // switch. Without JavaScript nothing is hidden: every detail
  // block simply renders open. Which modules are open is a
  // per-browser convenience, remembered in localStorage.
  var EXPANDED_KEY = "evesynapse.home.expanded";
  function readExpanded() {
    try {
      var v = JSON.parse(window.localStorage.getItem(EXPANDED_KEY) || "[]");
      return Array.isArray(v) ? v : [];
    } catch (e) { return []; }
  }
  function rememberExpanded(id, open) {
    var ids = readExpanded().filter(function (x) { return x !== id; });
    if (open) ids.push(id);
    try { window.localStorage.setItem(EXPANDED_KEY, JSON.stringify(ids)); } catch (e) { /* private mode */ }
  }
  var expandSeq = 0;
  function setExpanded(ctrl, panels, open) {
    for (var p = 0; p < panels.length; p++) panels[p].hidden = !open;
    ctrl.setAttribute("aria-expanded", open ? "true" : "false");
    if (ctrl.classList.contains("cardexpand")) {
      ctrl.textContent = open ? "Less" : "More";
    }
    var owner = ctrl.getAttribute("data-expand-owner");
    var ownerEl = owner ? document.getElementById(owner) : null;
    if (ownerEl) ownerEl.classList.toggle("expanded", open);
    var card = ctrl.closest(".card[data-widget]");
    if (card && ctrl.classList.contains("cardexpand")) {
      card.classList.toggle("expanded", open);
      rememberExpanded(card.getAttribute("data-widget"), open);
    }
  }
  function bindExpander(ctrl, panels, open) {
    if (ctrl.hasAttribute("data-expand-bound")) return;
    ctrl.setAttribute("data-expand-bound", "1");
    var ids = [];
    for (var p = 0; p < panels.length; p++) {
      if (!panels[p].id) panels[p].id = "wmore-" + (++expandSeq);
      ids.push(panels[p].id);
    }
    ctrl.setAttribute("aria-controls", ids.join(" "));
    setExpanded(ctrl, panels, open);
    ctrl.addEventListener("click", function (ev) {
      ev.stopPropagation();
      setExpanded(ctrl, panels, panels[0].hidden);
    });
  }
  function initExpanders(root) {
    // Customize shows every module whole: it is about arranging.
    var grid = root.querySelector ? root.querySelector(".home-grid:not(.home-custom)") : null;
    var remembered = readExpanded();
    if (grid) {
      var cards = grid.querySelectorAll(".card[data-widget]");
      for (var i = 0; i < cards.length; i++) {
        var card = cards[i];
        // Every detail block in the module: whole blocks, and the
        // rows or items past the summary, each marked on its own.
        var panels = card.querySelectorAll("[data-wmore]");
        var heading = card.querySelector(":scope > h2, :scope > h3");
        if (!panels.length || !heading || heading.querySelector(".cardexpand")) continue;
        var btn = document.createElement("button");
        btn.type = "button";
        btn.className = "cardexpand";
        heading.appendChild(btn);
        card.classList.add("has-expand");
        bindExpander(btn, panels, remembered.indexOf(card.getAttribute("data-widget")) !== -1);
      }
    }
    var ctrls = root.querySelectorAll ? root.querySelectorAll("[data-expand]") : [];
    for (var j = 0; j < ctrls.length; j++) {
      var target = document.getElementById(ctrls[j].getAttribute("data-expand"));
      if (target) bindExpander(ctrls[j], [target], false);
    }
  }
  onContentReady(initExpanders);
  initExpanders(document);

  // --- Nav: Escape folds an open branch back up -----------------
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Escape") return;
    // (The character switcher in the top bar closes itself, with its
    // motion: see the end of this file.)
    var open = document.querySelectorAll("details.branch[open]:not(.topbar-switcher)");
    for (var i = 0; i < open.length; i++) open[i].removeAttribute("open");
  });

  // --- Live countdowns ---------------------------------------
  // Any element carrying data-finish (RFC3339) counts down to
  // that moment (home fleet widget, character page training
  // line). Without JS the server-rendered remainder stays.
  // One ticker at a time: the will-swap event stops it before
  // the old nodes go away, and the ready event starts it for the
  // fresh ones. A ticking-over-detached-nodes interval would
  // otherwise pile up behind every swap.
  var countdownTimer = null;
  function fmtLeft(ms) {
    if (ms <= 0) return "done";
    var s = Math.floor(ms / 1000);
    var d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600),
        m = Math.floor((s % 3600) / 60), sec = s % 60;
    if (d > 0) return "in " + d + "d " + h + "h " + m + "m";
    if (h > 0) return "in " + h + "h " + m + "m";
    if (m > 0) return "in " + m + "m " + sec + "s";
    return "in " + sec + "s";
  }
  function stopCountdowns() {
    if (countdownTimer !== null) {
      window.clearInterval(countdownTimer);
      countdownTimer = null;
    }
  }
  function initCountdowns(root) {
    var countdowns = root.querySelectorAll("[data-finish]");
    if (!countdowns.length) return;
    var tickCountdowns = function () {
      for (var i = 0; i < countdowns.length; i++) {
        var t = Date.parse(countdowns[i].getAttribute("data-finish"));
        if (!isNaN(t)) countdowns[i].textContent = fmtLeft(t - Date.now());
      }
    };
    tickCountdowns();
    stopCountdowns();
    countdownTimer = window.setInterval(tickCountdowns, 1000);
  }
  document.addEventListener("evesynapse:content-will-swap", stopCountdowns);
  onContentReady(initCountdowns);
  initCountdowns(document);

  // --- Fleet filter (home overview) ---------------------------
  // The fleet table renders every character server-side; this
  // just hides rows that don't match the typed text or the
  // active chip: All, a tag, or Not training. A row's detail row
  // goes with it, and a click anywhere on a row (not on a link)
  // opens its detail, like its chevron. No JS: the full list is
  // simply visible, details and all.
  function initFleetFilter(root) {
    var fleetFilter = root.querySelector("#fleet-filter");
    var fleetRows = root.querySelectorAll(".fleet-row");
    if (!fleetFilter || !fleetRows.length) return;
    var noMatch = root.querySelector(".fleet-nomatch");
    var shownEl = root.querySelector("[data-fleet-shown]");
    var chips = root.querySelectorAll(".fleet-tags .chip");
    var activeTag = "";
    var idleOnly = false;
    var detailOf = function (row) {
      var btn = row.querySelector("[data-expand]");
      return btn ? document.getElementById(btn.getAttribute("data-expand")) : null;
    };
    var applyFleetFilter = function () {
      var q = fleetFilter.value.trim().toLowerCase();
      var shown = 0;
      for (var i = 0; i < fleetRows.length; i++) {
        var hay = fleetRows[i].getAttribute("data-search") || "";
        var ok = (!q || hay.indexOf(q) !== -1) && (!activeTag || hay.indexOf(activeTag) !== -1) &&
          (!idleOnly || fleetRows[i].hasAttribute("data-idle"));
        fleetRows[i].style.display = ok ? "" : "none";
        var detail = detailOf(fleetRows[i]);
        if (detail) detail.style.display = ok ? "" : "none";
        if (ok) shown++;
      }
      if (noMatch) noMatch.hidden = shown > 0;
      if (shownEl) shownEl.textContent = String(shown);
    };
    var pick = function (chip) {
      activeTag = (chip.getAttribute("data-tag") || "").toLowerCase();
      idleOnly = chip.hasAttribute("data-idle-chip");
      for (var j = 0; j < chips.length; j++) {
        var on = chips[j] === chip;
        chips[j].classList.toggle("on", on);
        chips[j].setAttribute("aria-pressed", on ? "true" : "false");
      }
      applyFleetFilter();
    };
    fleetFilter.addEventListener("input", applyFleetFilter);
    for (var c = 0; c < chips.length; c++) {
      chips[c].addEventListener("click", function () {
        // A second press on the active chip goes back to All.
        if (this.classList.contains("on") && chips[0] !== this) pick(chips[0]);
        else pick(this);
      });
    }
    for (var r = 0; r < fleetRows.length; r++) {
      fleetRows[r].addEventListener("click", function (ev) {
        if (ev.target.closest("a, button, input, select")) return;
        var btn = this.querySelector("[data-expand]");
        if (btn) btn.click();
      });
    }
    // The KPI band's idle count ("N idle — show them") is a link to
    // the Characters page; with the Fleet Overview on the page it
    // filters the fleet to the idle pilots and brings the module into
    // view instead.
    var idleLink = root.querySelector("[data-fleet-idle]");
    var idleChip = root.querySelector(".fleet-tags [data-idle-chip]");
    var fleetCard = fleetFilter.closest(".card");
    if (idleLink && idleChip && fleetCard) {
      idleLink.addEventListener("click", function (ev) {
        ev.preventDefault();
        if (!idleChip.classList.contains("on")) pick(idleChip);
        fleetCard.scrollIntoView({ block: "start", behavior: "smooth" });
      });
    }
  }
  onContentReady(initFleetFilter);
  initFleetFilter(document);

  // --- Briefing severity tabs ------------------------------------
  // All / Critical / Warning / Info swap which cards show; the
  // module's More/Less still decides how far down the list goes.
  function initBriefTabs(root) {
    var tabs = root.querySelectorAll("[data-brief]");
    if (!tabs.length) return;
    var cards = root.querySelectorAll(".brief-card");
    for (var i = 0; i < tabs.length; i++) {
      tabs[i].addEventListener("click", function () {
        var sev = this.getAttribute("data-brief");
        for (var j = 0; j < tabs.length; j++) {
          var on = tabs[j] === this;
          tabs[j].classList.toggle("on", on);
          tabs[j].setAttribute("aria-pressed", on ? "true" : "false");
        }
        for (var k = 0; k < cards.length; k++) {
          cards[k].classList.toggle("brief-off", sev !== "all" && cards[k].getAttribute("data-sev") !== sev);
        }
      });
    }
  }
  onContentReady(initBriefTabs);
  initBriefTabs(document);

  // --- Widget settings (orders scope picker) ------------------
  // A change applies at once: the form IS the plain submit (the
  // no-JS path), this just skips the button press. In customize
  // mode the page reloads so the live card re-renders in place.
  function initScopeForms(root) {
    var scopeForms = root.querySelectorAll("form.widget-scope");
    for (var s = 0; s < scopeForms.length; s++) {
      scopeForms[s].addEventListener("change", function () {
        this.submit();
      });
    }
  }
  onContentReady(initScopeForms);
  initScopeForms(document);

  // The size picker's outside-click close binds once and reads the
  // current grid each time; binding it per grid would pile a dead
  // handler behind every swapped customize view.
  var homePickCloseBound = false;

  // Card control glyphs: one stroke family, drawn at one size and
  // centered in identical square buttons.
  var CARD_ICONS = {
    move: '<path d="M12 3v18M3 12h18"/><path d="m9 6 3-3 3 3M9 18l3 3 3-3M6 9l-3 3 3 3M18 9l3 3-3 3"/>',
    size: '<path d="M14 4h6v6M10 20H4v-6M20 4l-6.5 6.5M4 20l6.5-6.5"/>',
    remove: '<path d="M6 6l12 12M18 6 6 18"/>'
  };
  function cardIcon(name) {
    return '<svg class="cardicon" viewBox="0 0 24 24" aria-hidden="true" focusable="false">' +
      '<g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">' +
      CARD_ICONS[name] + "</g></svg>";
  }

  // --- Home customize: drag, resize, remove, add module --------
  // Layered over the plain customize forms, never replacing them:
  // this runs only on the customize view (the data-customize grid)
  // and stores through the same /home/layout endpoint — a drag
  // saves once, on drop, and the server stays the normalizer of
  // whatever order it is handed. The grid itself is solved, not
  // stretched: the mirror solver below recomputes every module's
  // span live so the arrangement under the finger is the final
  // one. Without JS the up/down, half/full and add/remove forms
  // in the module list do the same job.
  // Keyboard: \ on the Overview opens Customize, and in Customize
  // finishes it (every change already saved, so it is Done). Bound
  // once, reading the current page each time. Typing in a field,
  // modifier chords and a drag in progress are left alone.
  document.addEventListener("keydown", function (ev) {
    if (ev.ctrlKey || ev.metaKey || ev.altKey || ev.defaultPrevented) return;
    var t = ev.target;
    if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
    var grid = document.querySelector(".grid.home-grid");
    if (!grid) return;
    var custom = grid.classList.contains("home-custom");
    var go = function (href) {
      // Through a real link when the page has one, so the shell's
      // swap navigation handles it; a plain navigation otherwise.
      var link = document.querySelector('.home-bar a[href="' + href + '"]');
      if (link) link.click(); else window.location.assign(href);
    };
    if (ev.key !== "\\" || document.body.classList.contains("drag-active")) return;
    ev.preventDefault();
    go(custom ? "/" : "/?customize=1");
  });

  function initHomeGrid(root) {
    var homeGrid = root.querySelector("#home-grid");
    if (homeGrid && homeGrid.getAttribute("data-customize") === "1" && window.fetch) {
      initHomeCustomize(homeGrid);
    }
  }
  onContentReady(initHomeGrid);
  initHomeGrid(document);

  function initHomeCustomize(grid) {
    var modal = document.getElementById("add-module-modal");
    grid.classList.add("has-controls");

    function postLayout(params) {
      var body = new URLSearchParams();
      for (var key in params) body.set(key, params[key]);
      return fetch("/home/layout", {
        method: "POST",
        headers: { "X-Requested-With": "XMLHttpRequest" },
        body: body
      }).then(function (resp) { return resp.ok; }, function () { return false; });
    }

    function moduleCards() {
      return grid.querySelectorAll(":scope > .card[data-widget]");
    }
    function widgetIDs() {
      var ids = [];
      var cards = moduleCards();
      for (var i = 0; i < cards.length; i++) ids.push(cards[i].getAttribute("data-widget"));
      return ids;
    }

    // --- motion --------------------------------------------------
    // Every rearrangement goes through flip(): measure where each
    // module is drawn right now (mid-animation included), stop
    // whatever it was doing, apply the change, measure the new
    // layout, and play each module from where it was drawn to its
    // new cell. Moves are translate-only — scaling a module to
    // fake its size change squashes its text — and every module
    // runs at most one animation, cancelled before the next change
    // is measured, so overlapping changes never fight, jump back or
    // replay. The measured layout (page coordinates, no transforms)
    // is kept for the drag's hit-testing, so decisions follow where
    // modules are going, not where an animation happens to be.
    var reducedMotion = window.matchMedia &&
      window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    var canAnimate = !reducedMotion && !!Element.prototype.animate;
    var MOVE_MS = 260;
    var EASE = "cubic-bezier(0.2, 0, 0, 1)";
    var layout = new Map();

    function movers() {
      return grid.querySelectorAll(":scope > .card:not(.dragging)");
    }
    function stopMotion(el) {
      if (el._motion) { el._motion.cancel(); el._motion = null; }
    }
    function pageBox(r) {
      var sx = window.scrollX, sy = window.scrollY;
      return { l: r.left + sx, t: r.top + sy, r: r.right + sx, b: r.bottom + sy };
    }
    function measure() {
      layout = new Map();
      var els = movers();
      for (var i = 0; i < els.length; i++) {
        // A running move is a transform; the box under it is the
        // layout. Strip the move to read it.
        var el = els[i], r = el.getBoundingClientRect(), b = pageBox(r);
        if (el._motion) {
          var m = new DOMMatrixReadOnly(window.getComputedStyle(el).transform);
          b = { l: b.l - m.m41, t: b.t - m.m42, r: b.r - m.m41, b: b.b - m.m42 };
        }
        layout.set(el, b);
      }
    }
    function flip(mutate, still) {
      var before = new Map();
      var els = movers();
      for (var i = 0; i < els.length; i++) before.set(els[i], els[i].getBoundingClientRect());
      for (var j = 0; j < els.length; j++) stopMotion(els[j]);
      mutate();
      var now = movers();
      layout = new Map();
      for (var k = 0; k < now.length; k++) {
        var el = now[k], r = el.getBoundingClientRect(), was = before.get(el);
        layout.set(el, pageBox(r));
        if (!canAnimate || el === still) continue;
        if (!was) {
          // Arriving: a soft fade and rise into its cell.
          el._motion = el.animate([
            { opacity: 0, transform: "translateY(10px) scale(0.98)" },
            { opacity: 1, transform: "none" }
          ], { duration: MOVE_MS, easing: EASE });
        } else {
          var dx = was.left - r.left, dy = was.top - r.top;
          if (Math.abs(dx) < 0.5 && Math.abs(dy) < 0.5) continue;
          el._motion = el.animate([
            { transform: "translate(" + dx + "px," + dy + "px)" },
            { transform: "none" }
          ], { duration: MOVE_MS, easing: EASE });
        }
        el._motion.onfinish = (function (e, a) {
          return function () { if (e._motion === a) e._motion = null; };
        })(el, el._motion);
      }
    }
    // A module leaving: fade it down, then close the gap.
    function removeCard(card) {
      var gone = false;
      var go = function () {
        if (gone) return;
        gone = true;
        flip(function () { card.remove(); });
      };
      if (!canAnimate) { go(); return; }
      stopMotion(card);
      card.style.pointerEvents = "none";
      card.animate([
        { opacity: 1, transform: "none" },
        { opacity: 0, transform: "scale(0.96)" }
      ], { duration: 160, easing: "ease-in", fill: "forwards" }).onfinish = go;
      // A page whose animations are paused still loses the card.
      window.setTimeout(go, 220);
    }
    // A brief red pulse on whatever refused a change.
    function refuse(el) {
      el.classList.remove("refused");
      void el.offsetWidth;
      el.classList.add("refused");
      window.setTimeout(function () { el.classList.remove("refused"); }, 650);
    }

    // --- sizes ---------------------------------------------------
    // A module's size is its gw/gh classes (grid cells across and
    // down, 1-3); the CSS lays the grid out from them and every cell
    // is a fixed size, so a module is exactly as big as its cells.
    // Some modules have a floor (data-minw / data-minh): asked for
    // less, the cells blink red and the module keeps its size.
    function sizeOf(el) {
      return {
        w: parseInt(el.getAttribute("data-gw"), 10) || 1,
        h: parseInt(el.getAttribute("data-gh"), 10) || 1
      };
    }
    function minOf(el) {
      return {
        w: parseInt(el.getAttribute("data-minw"), 10) || 1,
        h: parseInt(el.getAttribute("data-minh"), 10) || 1
      };
    }
    function sizeCard(el, w, h) {
      ["gw1", "gw2", "gw3", "gh1", "gh2", "gh3"].forEach(function (c) { el.classList.remove(c); });
      el.classList.add("gw" + w);
      el.classList.add("gh" + h);
      el.setAttribute("data-gw", String(w));
      el.setAttribute("data-gh", String(h));
    }
    // One resize, wherever it came from (picker or grip): apply it at
    // once, save it, and put the old size back if the save fails.
    function resizeCard(card, w, h) {
      var was = sizeOf(card);
      if (was.w === w && was.h === h) return;
      flip(function () { sizeCard(card, w, h); });
      postLayout({ action: "size", widget: card.getAttribute("data-widget"), w: w, h: h }).then(function (ok) {
        if (!ok) flip(function () { sizeCard(card, was.w, was.h); });
      });
    }
    function saveOrder() {
      return postLayout({ action: "order", ids: widgetIDs().join(",") });
    }

    // Give one card its move handle, resize toggle and × button.
    // Cards added from the pop-up pass through here too.
    function enhanceCard(card) {
      var heading = card.querySelector(":scope > h2, :scope > h3");
      if (!heading || card.querySelector(".draghandle")) return;
      var id = card.getAttribute("data-widget");
      var title = heading.textContent.trim();

      var handle = document.createElement("button");
      handle.type = "button";
      handle.className = "cardbtn draghandle";
      handle.innerHTML = cardIcon("move");
      handle.title = "Drag to move";
      handle.setAttribute("aria-label", "Move " + title + " (drag, or use the arrow keys)");
      heading.insertBefore(handle, heading.firstChild);
      handle.addEventListener("pointerdown", function (ev) { startDrag(ev, card, handle); });
      handle.addEventListener("keydown", function (ev) { keyMove(ev, card, handle); });

      // The card's own controls sit in its top-right corner: the
      // size picker (a 3x3 grid of cells, the way a table size is
      // picked) and remove. The grip in the bottom-right corner
      // resizes by dragging, snapping to whole cells.
      var ctl = document.createElement("div");
      ctl.className = "cardctl";
      var sizeBtn = document.createElement("button");
      sizeBtn.type = "button";
      sizeBtn.className = "cardbtn cardsize";
      sizeBtn.innerHTML = cardIcon("size");
      sizeBtn.title = "Resize";
      sizeBtn.setAttribute("aria-haspopup", "true");
      sizeBtn.setAttribute("aria-expanded", "false");
      sizeBtn.setAttribute("aria-label", "Resize " + title);
      var picker = document.createElement("div");
      picker.className = "sizepick";
      picker.hidden = true;
      picker.setAttribute("role", "group");
      picker.setAttribute("aria-label", "Size of " + title + ", in grid cells");
      var cells = [];
      for (var ph = 1; ph <= 3; ph++) {
        for (var pw = 1; pw <= 3; pw++) {
          (function (w, h) {
            var cell = document.createElement("button");
            cell.type = "button";
            cell.className = "sizecell";
            cell.setAttribute("data-gw", String(w));
            cell.setAttribute("data-gh", String(h));
            cell.setAttribute("aria-label", w + " wide by " + h + " tall");
            cell.title = w + " × " + h;
            cell.addEventListener("mouseenter", function () { paintPicker(w, h); });
            cell.addEventListener("focus", function () { paintPicker(w, h); });
            cell.addEventListener("click", function () {
              var min = minOf(card);
              if (w < min.w || h < min.h) {
                // Under the module's floor: blink what was asked
                // for, then show the size it keeps.
                paintPicker(w, h);
                cells.forEach(function (c) { if (c.classList.contains("on")) refuse(c); });
                window.setTimeout(function () { var s = sizeOf(card); paintPicker(s.w, s.h); }, 650);
                return;
              }
              resizeCard(card, w, h);
              closePicker();
            });
            cells.push(cell);
            picker.appendChild(cell);
          })(pw, ph);
        }
      }
      function paintPicker(w, h) {
        var min = minOf(card);
        cells.forEach(function (c) {
          var cw = +c.getAttribute("data-gw"), ch = +c.getAttribute("data-gh");
          c.classList.toggle("on", cw <= w && ch <= h);
          c.classList.toggle("floor", cw <= min.w && ch <= min.h);
        });
      }
      function closePicker() {
        picker.hidden = true;
        sizeBtn.setAttribute("aria-expanded", "false");
      }
      picker.addEventListener("mouseleave", function () { var s = sizeOf(card); paintPicker(s.w, s.h); });
      sizeBtn.addEventListener("click", function () {
        var open = picker.hidden;
        var others = grid.querySelectorAll(".sizepick");
        for (var o = 0; o < others.length; o++) others[o].hidden = true;
        picker.hidden = !open;
        sizeBtn.setAttribute("aria-expanded", open ? "true" : "false");
        if (open) {
          var s = sizeOf(card);
          paintPicker(s.w, s.h);
          cells[(s.h - 1) * 3 + (s.w - 1)].focus();
        }
      });
      picker.addEventListener("keydown", function (ev) {
        // Escape closes the picker only; it does not also leave Customize.
        if (ev.key === "Escape") { ev.preventDefault(); closePicker(); sizeBtn.focus(); }
      });

      var remove = document.createElement("button");
      remove.type = "button";
      remove.className = "cardbtn cardremove";
      remove.innerHTML = cardIcon("remove");
      remove.title = "Remove";
      remove.setAttribute("aria-label", "Remove " + title + " from your home");
      remove.addEventListener("click", function () {
        remove.disabled = true;
        postLayout({ action: "toggle", widget: id }).then(function (ok) {
          if (ok) removeCard(card);
          else remove.disabled = false;
        });
      });
      ctl.appendChild(sizeBtn);
      ctl.appendChild(remove);
      ctl.appendChild(picker);
      card.appendChild(ctl);

      var grip = document.createElement("span");
      grip.className = "cardgrip";
      grip.setAttribute("aria-hidden", "true");
      grip.addEventListener("pointerdown", function (ev) { startResize(ev, card, grip); });
      card.appendChild(grip);
    }

    // Keyboard move: the arrow keys on a module's move handle step it
    // one place earlier or later and save.
    function keyMove(ev, card, handle) {
      var back = ev.key === "ArrowUp" || ev.key === "ArrowLeft";
      var fwd = ev.key === "ArrowDown" || ev.key === "ArrowRight";
      if (!back && !fwd) return;
      ev.preventDefault();
      var all = Array.prototype.slice.call(moduleCards());
      var i = all.indexOf(card), j = back ? i - 1 : i + 1;
      if (j < 0 || j >= all.length) return;
      flip(function () {
        grid.insertBefore(card, back ? all[j] : all[j].nextSibling);
      });
      handle.focus();
      saveOrder();
    }

    // --- grip resize ---------------------------------------------
    // The grip drags a frame that follows the pointer exactly; the
    // module underneath snaps to whole cells as the frame crosses
    // into them (with a little hysteresis, so a boundary never
    // flickers), and the modules after it glide out of the way. A
    // module grows only rightwards to the grid's edge from the
    // column it starts in, so resizing never makes it jump rows.
    // Dragged below the module's floor, the frame turns red and the
    // module stays at its floor.
    function startResize(ev, card, grip) {
      if (ev.pointerType === "mouse" && ev.button !== 0) return;
      ev.preventDefault();
      stopMotion(card);
      var start = sizeOf(card), cur = start, min = minOf(card);
      var r0 = card.getBoundingClientRect();
      var gcs = window.getComputedStyle(grid);
      var gapX = parseFloat(gcs.columnGap) || 0, gapY = parseFloat(gcs.rowGap) || 0;
      var cellW = (r0.width - (start.w - 1) * gapX) / start.w + gapX;
      // A row is the grid's row floor (--home-row): modules can stand
      // taller than their cells when their content needs it, so the
      // card's own height is not the measure.
      var rowPx = parseFloat(gcs.getPropertyValue("--home-row")) *
        parseFloat(window.getComputedStyle(document.documentElement).fontSize);
      var cellH = (rowPx || 176) + gapY;
      var cols = (gcs.gridTemplateColumns || "").split(" ").filter(Boolean).length || 1;
      var gridLeft = grid.getBoundingClientRect().left;
      var colStart = Math.round((r0.left - gridLeft) / cellW);
      var maxW = Math.max(1, Math.min(3, cols - colStart));
      var lastX = ev.clientX, lastY = ev.clientY, under = false;
      var startY = ev.clientY;

      var frame = document.createElement("div");
      frame.className = "resize-frame";
      document.body.appendChild(frame);
      try { grip.setPointerCapture(ev.pointerId); } catch (err) { /* nicety */ }
      card.classList.add("resizing");
      document.body.classList.add("resize-active");

      function snap(raw, was, hi) {
        var n = Math.round(raw);
        if (n !== was && Math.abs(raw - was) < 0.62) n = was;
        return Math.max(1, Math.min(hi, n));
      }
      // Runs on every pointer move, straight from the event: the
      // frame tracks the pointer with no frame of lag.
      function frameTick() {
        var r = card.getBoundingClientRect();
        // Width from the pointer's offset (a module is exactly its
        // columns wide). Height from how far the pointer has moved
        // since the press, added to the rows the module had: a module
        // standing taller than its rows (its content needs the room)
        // must not jump to that height the moment the grip is taken.
        var relX = lastX - r.left;
        var relY = start.h * cellH - gapY + (lastY - startY);
        var w = snap((relX + gapX) / cellW, cur.w, maxW);
        var h = snap((relY + gapY) / cellH, cur.h, 3);
        under = w < min.w || h < min.h;
        frame.classList.toggle("under", under);
        frame.style.left = r.left + "px";
        frame.style.top = r.top + "px";
        frame.style.width = Math.max(cellW * 0.5, Math.min(relX, maxW * cellW - gapX)) + "px";
        frame.style.height = Math.max(cellH * 0.5, Math.min(relY, 3 * cellH - gapY)) + "px";
        w = Math.max(w, min.w);
        h = Math.max(h, min.h);
        if (w !== cur.w || h !== cur.h) {
          cur = { w: w, h: h };
          flip(function () { sizeCard(card, w, h); }, card);
        }
      }
      function onMove(e) {
        lastX = e.clientX;
        lastY = e.clientY;
        frameTick();
      }
      function onUp() {
        grip.removeEventListener("pointermove", onMove);
        grip.removeEventListener("pointerup", onUp);
        grip.removeEventListener("pointercancel", onUp);
        card.classList.remove("resizing");
        document.body.classList.remove("resize-active");
        frame.remove();
        if (under) refuse(card);
        if (cur.w === start.w && cur.h === start.h) return;
        // The module already stands at its new size; save it, and a
        // failed save puts back the size it started at.
        postLayout({ action: "size", widget: card.getAttribute("data-widget"), w: cur.w, h: cur.h }).then(function (ok) {
          if (!ok) flip(function () { sizeCard(card, start.w, start.h); });
        });
      }
      grip.addEventListener("pointermove", onMove);
      grip.addEventListener("pointerup", onUp);
      grip.addEventListener("pointercancel", onUp);
      frameTick();
    }

    // --- dragging --------------------------------------------------
    // Lifting a card puts a glowing slot into its place, with its
    // size, and takes the card out of the grid (position: fixed) at
    // exactly the spot it occupied — nothing under the pointer moves
    // at lift-off. The card then follows the pointer by transform
    // alone, so it tracks without lag. The slot moves when the
    // pointer is over another module: before it if the pointer is in
    // that module's first half (left half within the same row, top
    // half across rows), after it otherwise; the module the slot
    // just passed is ignored until the pointer leaves it, so a
    // boundary never flip-flops. Past the last module the slot goes
    // to the end. Holding the card near a viewport edge scrolls the
    // page. On drop the card glides into the slot and takes its
    // place; Escape or a cancelled pointer puts everything back.
    // The grid child beside el (dir -1 before, 1 after), skipping
    // text and the lifted card, which sits out of the flow.
    function sibling(el, dir) {
      var n = dir < 0 ? el.previousElementSibling : el.nextElementSibling;
      while (n && n.classList.contains("dragging")) n = dir < 0 ? n.previousElementSibling : n.nextElementSibling;
      return n;
    }
    function inside(b, x, y) {
      return !!b && x >= b.l && x <= b.r && y >= b.t && y <= b.b;
    }

    function startDrag(ev, card, handle) {
      if (ev.pointerType === "mouse" && ev.button !== 0) return;
      ev.preventDefault();

      var startX = ev.clientX, startY = ev.clientY;
      var lastX = startX, lastY = startY;
      var lifted = false, slot = null, origin = null, savedNodes = null;
      var raf = 0, lock = null, holdUntil = 0, dirty = true;

      try { handle.setPointerCapture(ev.pointerId); } catch (err) { /* nicety */ }

      function lift() {
        lifted = true;
        savedNodes = Array.prototype.slice.call(moduleCards());
        var open = grid.querySelectorAll(".sizepick:not([hidden])");
        for (var i = 0; i < open.length; i++) open[i].hidden = true;
        stopMotion(card);
        origin = card.getBoundingClientRect();
        slot = document.createElement("div");
        slot.className = "card drag-placeholder";
        slot.setAttribute("aria-hidden", "true");
        var s = sizeOf(card);
        sizeCard(slot, s.w, s.h);
        grid.insertBefore(slot, card);
        card.style.left = origin.left + "px";
        card.style.top = origin.top + "px";
        card.style.width = origin.width + "px";
        card.style.height = origin.height + "px";
        card.style.transformOrigin = (startX - origin.left) + "px " + (startY - origin.top) + "px";
        card.classList.add("dragging");
        // A grouped ancestor (the view-transition name on <main>, or
        // any transform/filter on a wrapper) is the containing block
        // for a fixed element, so viewport coordinates land offset
        // by that ancestor's origin. Measure the miss and correct
        // it: the card stays exactly under the pointer either way.
        var placed = card.getBoundingClientRect();
        var missX = placed.left - origin.left, missY = placed.top - origin.top;
        if (missX || missY) {
          card.style.left = (origin.left - missX) + "px";
          card.style.top = (origin.top - missY) + "px";
        }
        document.body.classList.add("drag-active");
        measure();
        raf = requestAnimationFrame(tick);
      }

      // The card is placed on every pointer move, straight from the
      // event (a transform: no layout), so it never trails the
      // pointer by a frame.
      function follow() {
        card.style.transform = "translate3d(" + (lastX - startX) + "px," + (lastY - startY) + "px,0)";
      }

      // The frame loop only scrolls: the closer the pointer rides to
      // a viewport edge, the faster the page rolls under it.
      function tick() {
        raf = 0;
        if (!lifted) return;
        var edge = 72, speed = 0;
        if (lastY < edge) speed = -Math.min(16, Math.ceil((edge - lastY) / 4));
        else if (lastY > window.innerHeight - edge) speed = Math.min(16, Math.ceil((lastY - (window.innerHeight - edge)) / 4));
        if (speed) { window.scrollBy(0, speed); dirty = true; }
        if (dirty) retarget();
        raf = requestAnimationFrame(tick);
      }

      function retarget() {
        if (!lifted || performance.now() < holdUntil) return;
        dirty = false;
        var px = lastX + window.scrollX, py = lastY + window.scrollY;
        if (lock && !inside(layout.get(lock), px, py)) lock = null;
        var sb = layout.get(slot);
        if (inside(sb, px, py)) { lock = null; return; }
        var kids = Array.prototype.slice.call(movers());
        var target = null, bottom = 0;
        for (var i = 0; i < kids.length; i++) {
          var b = layout.get(kids[i]);
          if (b && b.b > bottom) bottom = b.b;
          if (kids[i] !== slot && !target && inside(b, px, py)) target = kids[i];
        }
        if (!target) {
          if (py > bottom && kids[kids.length - 1] !== slot) moveSlot(null, null);
          return;
        }
        if (target === lock) return;
        var tb = layout.get(target);
        var sameRow = sb && sb.t < tb.b && tb.t < sb.b;
        var first = sameRow ? px < (tb.l + tb.r) / 2 : py < (tb.t + tb.b) / 2;
        // Already on that side of it: nothing to do.
        if ((first ? sibling(target, -1) : sibling(target, 1)) === slot) return;
        moveSlot(first ? target : target.nextSibling, target);
      }

      function moveSlot(ref, passed) {
        flip(function () { grid.insertBefore(slot, ref); });
        lock = passed;
        // Let the hop's layout settle under the pointer before the
        // next decision.
        holdUntil = performance.now() + 90;
        dirty = true;
      }

      function onMove(e) {
        lastX = e.clientX;
        lastY = e.clientY;
        dirty = true;
        if (!lifted && Math.abs(lastX - startX) + Math.abs(lastY - startY) >= 5) lift();
        if (lifted) { follow(); retarget(); }
      }

      function onKey(e) {
        if (e.key === "Escape") { e.preventDefault(); onCancel(); }
      }

      function detach() {
        handle.removeEventListener("pointermove", onMove);
        handle.removeEventListener("pointerup", onUp);
        handle.removeEventListener("pointercancel", onCancel);
        document.removeEventListener("keydown", onKey, true);
        if (raf) cancelAnimationFrame(raf);
        raf = 0;
      }

      // Every exit path ends here: no slot, no lifted card, no
      // inline style survives a drag.
      function settle() {
        if (slot && slot.parentNode) grid.replaceChild(card, slot);
        slot = null;
        card.classList.remove("dragging");
        card.style.left = card.style.top = card.style.width = card.style.height = "";
        card.style.transform = card.style.transformOrigin = "";
        document.body.classList.remove("drag-active");
        lifted = false;
      }

      // Glide the lifted card onto the slot's cell, then let it take
      // the slot's place.
      function landThen(done) {
        var b = layout.get(slot);
        if (!canAnimate || !b) { done(); return; }
        var dx = b.l - window.scrollX - origin.left, dy = b.t - window.scrollY - origin.top;
        var a = card.animate([
          { transform: card.style.transform || "none" },
          { transform: "translate3d(" + dx + "px," + dy + "px,0)" }
        ], { duration: 200, easing: EASE, fill: "forwards" });
        card.classList.add("landing");
        // Whichever comes first: the glide's end, or a timer for a
        // page whose animations are paused (a hidden tab).
        var finished = false;
        var finish = function () {
          if (finished) return;
          finished = true;
          card.classList.remove("landing");
          done();
          a.cancel();
        };
        a.onfinish = finish;
        window.setTimeout(finish, 260);
      }

      function onUp() {
        detach();
        if (!lifted) return;
        lifted = false;
        landThen(function () {
          settle();
          var moved = moduleCards();
          var changed = moved.length !== savedNodes.length;
          for (var i = 0; !changed && i < moved.length; i++) changed = moved[i] !== savedNodes[i];
          if (!changed) return;
          saveOrder().then(function (ok) {
            if (!ok) flip(function () { for (var j = 0; j < savedNodes.length; j++) grid.appendChild(savedNodes[j]); });
          });
        });
      }

      function onCancel() {
        detach();
        if (!lifted) return;
        lifted = false;
        // Back to where it started: the slot returns to the card's
        // old place, the card lands there.
        var at = savedNodes.indexOf(card);
        var next = savedNodes[at + 1] || null;
        flip(function () { grid.insertBefore(slot, next); });
        landThen(settle);
      }

      handle.addEventListener("pointermove", onMove);
      handle.addEventListener("pointerup", onUp);
      handle.addEventListener("pointercancel", onCancel);
      document.addEventListener("keydown", onKey, true);
    }

    function addModule(id, btn) {
      btn.disabled = true;
      postLayout({ action: "toggle", widget: id }).then(function (ok) {
        if (!ok) { btn.disabled = false; return; }
        // Widget markup is server-rendered; lift the new card
        // out of a fresh copy of this page rather than rebuilding
        // it client-side. If anything about that fails, a reload
        // lands in the same place — the add already saved.
        fetch("/?customize=1", { headers: { "Accept": "text/html" } }).then(function (resp) {
          return resp.ok ? resp.text() : "";
        }).then(function (html) {
          var doc = new DOMParser().parseFromString(html, "text/html");
          var fresh = doc.querySelector('.card[data-widget="' + id + '"]');
          if (!fresh) { window.location.reload(); return; }
          var node = document.importNode(fresh, true);
          // Never two cards for one module: if a stale copy is
          // somehow still in the grid, it goes before the fresh
          // one lands.
          var stale = grid.querySelector('.card[data-widget="' + id + '"]');
          if (stale) stale.remove();
          enhanceCard(node);
          window.EveSynapseModal.close(modal);
          flip(function () { grid.appendChild(node); });
          node.scrollIntoView({ behavior: reducedMotion ? "auto" : "smooth", block: "nearest" });
        }, function () { window.location.reload(); });
      });
    }

    var cards = moduleCards();
    for (var i = 0; i < cards.length; i++) enhanceCard(cards[i]);
    // A size picker closes on a click anywhere outside its card's
    // controls. Bound once; it reads the current grid each time.
    if (!homePickCloseBound) {
      homePickCloseBound = true;
      document.addEventListener("click", function (ev) {
        if (ev.target.closest && ev.target.closest(".cardctl")) return;
        var open = document.querySelectorAll(".sizepick:not([hidden])");
        for (var p = 0; p < open.length; p++) {
          open[p].hidden = true;
          var btn = open[p].parentNode.querySelector(".cardsize");
          if (btn) btn.setAttribute("aria-expanded", "false");
        }
      });
    }

    // Add-module pop-up. The full catalog is rendered into it;
    // each open re-filters against what is on the home right now,
    // so removes and adds are reflected immediately. Opening,
    // closing, Escape and the backdrop are the shared modal's.
    var addBtn = document.getElementById("add-module-btn");
    if (addBtn && modal) {
      var emptyNote = modal.querySelector(".modal-empty");
      var addItems = modal.querySelectorAll("[data-add-widget]");
      var openAdd = function () {
        var onHome = {};
        var ids = widgetIDs();
        for (var i = 0; i < ids.length; i++) onHome[ids[i]] = true;
        var available = 0;
        for (var j = 0; j < addItems.length; j++) {
          var off = !!onHome[addItems[j].getAttribute("data-add-widget")];
          addItems[j].parentElement.hidden = off;
          addItems[j].disabled = false;
          if (!off) available++;
        }
        if (emptyNote) emptyNote.hidden = available > 0;
        window.EveSynapseModal.open(modal);
      };
      addBtn.addEventListener("click", function (ev) {
        ev.preventDefault();
        var menu = addBtn.closest("details");
        if (menu) menu.open = false;
        openAdd();
      });
      // The Overview menu's "Add a module" lands here from the home
      // with ?add=1: open the pop-up straight away.
      if (/[?&]add=1\b/.test(window.location.search)) openAdd();
      for (var a = 0; a < addItems.length; a++) {
        addItems[a].addEventListener("click", function () {
          addModule(this.getAttribute("data-add-widget"), this);
        });
      }
    }
  }

  // --- Search suggestions -------------------------------------
  // One autocomplete widget behind every search box: the market
  // and watchlist finders, the planner, the skill-plan add box,
  // the Items DB search, and the banner's global search. Each
  // box names its feed (a pool of the one suggestion endpoint)
  // and what a pick does; without one of them the plain form
  // still submits exactly as before.
  // hitLabel fills a result's type line: a system's security status
  // in its color, then the label text.
  function hitLabel(el, it) {
    if (it.sec) {
      var sec = document.createElement("span");
      sec.className = "sec sec-" + (it.secTier || 0);
      sec.textContent = it.sec;
      el.appendChild(sec);
    }
    el.appendChild(document.createTextNode(it.label || ""));
  }
  window.EveSynapseHitLabel = hitLabel;

  function attachSuggest(input, list, endpoint, onPick) {
    if (!input || !list || !window.fetch) return;
    var items = [];
    var selected = -1;
    var timer = null;
    var retryTimer = null;
    var pendingRetries = 0;
    var lastQuery = "";

    function isPending(it) {
      return !!(it && it.kind === "pilot-pending");
    }
    function firstSelectable() {
      for (var i = 0; i < items.length; i++) {
        if (!isPending(items[i])) return i;
      }
      return -1;
    }
    function nextSelectable(from, delta) {
      if (!items.length) return -1;
      var idx = from;
      for (var step = 0; step < items.length; step++) {
        idx = (idx + delta + items.length) % items.length;
        if (!isPending(items[idx])) return idx;
      }
      return -1;
    }

    function close() {
      if (retryTimer) window.clearTimeout(retryTimer);
      retryTimer = null;
      list.hidden = true;
      items = [];
      selected = -1;
      input.setAttribute("aria-expanded", "false");
      input.removeAttribute("aria-activedescendant");
    }

    function paint() {
      list.innerHTML = "";
      items.forEach(function (it, idx) {
        var li = document.createElement("li");
        li.id = list.id + "-" + idx;
        li.setAttribute("role", "option");
        var name = document.createElement("span");
        name.textContent = it.name;
        li.appendChild(name);
        if (it.label) {
          var lab = document.createElement("span");
          lab.className = "sug-label";
          hitLabel(lab, it);
          li.appendChild(lab);
        }
        if (idx === selected) {
          li.className = "sel";
          input.setAttribute("aria-activedescendant", li.id);
        }
        if (isPending(it)) {
          // A name search still warming: shown, never a pick.
          li.className = (li.className ? li.className + " " : "") + "pending";
          li.setAttribute("aria-disabled", "true");
          list.appendChild(li);
          return;
        }
        // mousedown, not click: it fires before the input's blur,
        // so the pick lands before anything can close the list.
        li.addEventListener("mousedown", function (ev) {
          ev.preventDefault();
          pick(idx);
        });
        // Touch: tap selects without stealing the scroll.
        li.addEventListener("touchstart", function (ev) {
          ev.preventDefault();
          pick(idx);
        }, { passive: false });
        list.appendChild(li);
      });
      list.hidden = items.length === 0;
      input.setAttribute("aria-expanded", items.length ? "true" : "false");
    }

    function pick(idx) {
      var it = items[idx];
      if (!it || isPending(it)) return;
      close();
      onPick(it, input);
    }

    function query(q) {
      fetch(endpoint(q), {
        cache: "no-store",
        headers: { "Accept": "application/json" }
      }).then(function (resp) {
        return resp.ok ? resp.json() : [];
      }).then(function (rows) {
        if (input.value.trim() !== q) return; // typed past this result
        items = Array.isArray(rows) ? rows : [];
        selected = firstSelectable();
        paint();
        // A pilot-name search still warming re-asks on a short
        // fuse; the same query returns the real suggestion once
        // the record lands. Bounded, and only while this exact
        // text is still in the box.
        if (retryTimer) window.clearTimeout(retryTimer);
        retryTimer = null;
        var hasPending = false;
        for (var i = 0; i < items.length; i++) {
          if (isPending(items[i])) { hasPending = true; break; }
        }
        if (hasPending && pendingRetries < 8) {
          pendingRetries++;
          retryTimer = window.setTimeout(function () { query(q); }, 2500);
        } else if (!hasPending) {
          pendingRetries = 0;
        }
      }).catch(function () { close(); });
    }

    input.addEventListener("input", function () {
      var q = input.value.trim();
      if (q === lastQuery) return;
      lastQuery = q;
      if (timer) window.clearTimeout(timer);
      if (retryTimer) window.clearTimeout(retryTimer);
      retryTimer = null;
      pendingRetries = 0;
      if (q.length < 2) { close(); return; }
      timer = window.setTimeout(function () { query(q); }, 150);
    });

    input.addEventListener("keydown", function (ev) {
      if (list.hidden) return;
      if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
        ev.preventDefault();
        if (!items.length) return;
        selected = nextSelectable(selected, ev.key === "ArrowDown" ? 1 : -1);
        paint();
      } else if (ev.key === "Enter") {
        if (selected >= 0) {
          ev.preventDefault();
          pick(selected);
        }
        // No selection: let the form submit as it always has.
      } else if (ev.key === "Escape") {
        close();
      }
    });

    input.addEventListener("blur", function () {
      // Let a mousedown pick win the race, then close.
      window.setTimeout(close, 120);
    });
    var form = input.form;
    if (form) form.addEventListener("submit", function () { close(); });
  }

  function suggestURL(base, pool) {
    return function (q) {
      return base + "?q=" + encodeURIComponent(q) + (pool ? "&pool=" + pool : "");
    };
  }

  // Page search boxes re-attach over fresh swapped markup;
  // each attachSuggest binds the input in front of it, and the old
  // inputs leave with the old page. The banner search below lives
  // in the shell and binds exactly once.
  function initPageSuggest(root) {
    // Market: a pick jumps straight to that item's market page,
    // keeping the region the page is showing.
    (function () {
      var regionField = root.querySelector("#market-region");
      attachSuggest(
        root.querySelector("#market-q"),
        root.querySelector("#market-suggest"),
        suggestURL("/market/suggest", null),
        function (it) {
          var url = "/market/?type=" + encodeURIComponent(it.id);
          if (regionField && regionField.value) {
            url += "&region=" + encodeURIComponent(regionField.value);
          }
          window.location.href = url;
        });
    })();

    // Assets: suggestions come from what the account actually owns
    // (each says whose it is), narrowed to the acting character when
    // the box is ticked. A pick fills the box and runs the search.
    (function () {
      var only = root.querySelector("#assets-only");
      attachSuggest(
        root.querySelector("#assets-q"),
        root.querySelector("#assets-suggest"),
        function (q) {
          return "/assets/suggest?q=" + encodeURIComponent(q) + (only && only.checked ? "&only=1" : "");
        },
        function (it, input) {
          input.value = it.name;
          if (input.form) input.form.submit();
        });
    })();

    // Sync and Admin (administrators): find one character among all of
    // them. A pick goes straight to that character's data, or to the
    // account it belongs to; typing a name and pressing Enter does the
    // same through the plain form.
    attachSuggest(
      root.querySelector("#sync-character-q"),
      root.querySelector("#sync-character-suggest"),
      suggestURL("/admin/suggest", null),
      function (it) { window.location.href = "/sync/?character=" + encodeURIComponent(it.id); });
    attachSuggest(
      root.querySelector("#admin-q"),
      root.querySelector("#admin-suggest"),
      suggestURL("/admin/suggest", null),
      function (it) { window.location.href = "/admin/?q=" + encodeURIComponent(it.id); });

    // Search boxes that name their own feed (data-suggest): the
    // doctrine pages and the fit browsers. A suggestion that is a
    // link is followed; any other fills the box, and where the box
    // names a hidden field (data-suggest-target) that takes what
    // was picked, until the box is typed in again.
    Array.prototype.forEach.call(root.querySelectorAll("input[data-suggest]"), function (input) {
      var base = input.getAttribute("data-suggest");
      var target = document.getElementById(input.getAttribute("data-suggest-target") || "");
      if (target) input.addEventListener("input", function () { target.value = ""; });
      attachSuggest(input, document.getElementById(input.getAttribute("aria-controls")),
        function (q) { return base + (base.indexOf("?") === -1 ? "?" : "&") + "q=" + encodeURIComponent(q); },
        function (it, input) {
          if (it.url) { window.location.href = it.url; return; }
          input.value = it.name;
          if (target) target.value = it.value || it.id;
        });
    });

    // Watchlist finder: a pick watches that exact item at once (the
    // same post the Watch buttons make); the keyword matches only list
    // when the box is submitted without a pick.
    (function () {
      var input = root.querySelector("#watch-q");
      attachSuggest(input, root.querySelector("#watch-suggest"),
        suggestURL("/items/search.json", "market"),
        function (it, input) {
          input.value = it.name;
          var form = document.createElement("form");
          form.method = "post";
          form.action = "/market/watch";
          form.hidden = true;
          var fields = {
            action: "add", type: String(it.id),
            region: input.getAttribute("data-watch-region") || "",
            threshold: "5", next: "/market/"
          };
          Object.keys(fields).forEach(function (name) {
            var field = document.createElement("input");
            field.type = "hidden";
            field.name = name;
            field.value = fields[name];
            form.appendChild(field);
          });
          document.body.appendChild(form);
          form.submit();
        });
    })();

    // Planner: a pick opens that product's plan.
    attachSuggest(
      root.querySelector("#planner-q"),
      root.querySelector("#planner-suggest"),
      suggestURL("/items/search.json", "planner"),
      function (it) {
        window.location.href = "/planner/?product=" + encodeURIComponent(it.id);
      });

    // Skill-plan add box: a pick fills the exact skill name and
    // runs the search, which offers the level picker as usual.
    (function () {
      var input = root.querySelector("#skill-q");
      attachSuggest(input, root.querySelector("#skill-suggest"),
        suggestURL("/items/search.json", "skills"),
        function (it, input) {
          input.value = it.name;
          if (input.form) input.form.submit();
        });
    })();

    // Items DB search: a pick opens the item's details page.
    attachSuggest(
      root.querySelector("#items-q"),
      root.querySelector("#items-suggest"),
      suggestURL("/items/search.json", "all"),
      function (it) {
        window.location.href = "/items/type/" + encodeURIComponent(it.id) + "/";
      });
  }
  onContentReady(initPageSuggest);
  initPageSuggest(document);

  // Banner global search: items, your characters, systems, regions,
  // pilots and organizations the app already knows, each to its own
  // page.
  attachSuggest(
    document.getElementById("topbar-q"),
    document.getElementById("topbar-suggest"),
    function (q) { return "/search.json?q=" + encodeURIComponent(q); },
    function (it) {
      var url;
      if (it.kind === "character") {
        url = "/character/?character=" + encodeURIComponent(it.id);
      } else if (it.kind === "pilot") {
        url = "/pilot/?character=" + encodeURIComponent(it.id);
      } else if (it.kind === "corporation") {
        url = "/corporation/?corporation=" + encodeURIComponent(it.id);
      } else if (it.kind === "alliance") {
        url = "/alliance/?alliance=" + encodeURIComponent(it.id);
      } else if (it.kind === "system") {
        url = "/system/?system=" + encodeURIComponent(it.id);
      } else if (it.kind === "region") {
        url = "/region/?region=" + encodeURIComponent(it.id);
      } else {
        url = "/items/type/" + encodeURIComponent(it.id) + "/";
      }
      window.location.href = url;
    });
})();

// Live regions: a section rendered while its data is still
// warming polls its fragment endpoint and swaps itself in when
// the state leaves pending. The server renders the section again
// from stored rows only; polling stops as soon as the returned
// fragment settles. It never gives up while the fragment keeps
// answering pending: a fast cadence at first, then a slower
// steady one, so a slow first warm (a cold worker catching up
// after a restart) still fills the page in on its own instead
// of stranding the pending copy until a manual refresh. When
// the tab becomes visible again the next poll happens at once
// instead of whenever a throttled timer gets round to it.
(function () {
  if (!window.fetch) return;
  var kicks = [];

  // Arm the pending regions in fresh markup. Old regions leave
  // with the swap (their polls notice the detach and stop), and
  // the kick list resets so visibility wakes only live ones.
  function armRegions(root) {
    var regions = root.querySelectorAll("[data-live-region]");
    for (var i = 0; i < regions.length; i++) {
      arm(regions[i]);
    }
  }
  window.EveSynapseContent.onReady(armRegions);
  document.addEventListener("evesynapse:content-will-swap", function () {
    kicks = [];
  });

  function arm(region) {
    var url = region.getAttribute("data-poll-url");
    if (!url) return;
    var polls = 0;
    var timer = null;
    var stopped = false;

    function stop() {
      stopped = true;
      if (timer !== null) {
        window.clearTimeout(timer);
        timer = null;
      }
    }
    function schedule() {
      if (stopped) return;
      // ~1.5s while the wait is young (the first minute or
      // so), then a steady ~5s for as long as it takes.
      timer = window.setTimeout(poll, polls < 40 ? 1500 : 5000);
    }
    function poll() {
      timer = null;
      if (stopped) return;
      if (!document.contains(region)) {
        stop();
        return;
      }
      polls += 1;
      window.fetch(url, {
        credentials: "same-origin",
        headers: { "X-Requested-With": "XMLHttpRequest" }
      }).then(function (resp) {
        return resp.text();
      }).then(function (html) {
        var probe = document.createElement("div");
        probe.innerHTML = html;
        var fresh = probe.querySelector("[data-poll-state]");
        if (!fresh) {
          // Not a fragment (a redirect to a full page, an
          // error): stop polling and let a reload show it.
          stop();
          window.location.reload();
          return;
        }
        var state = fresh.getAttribute("data-poll-state");
        if (state !== "pending" && state !== "loading") {
          stop();
          // The page title renders above the panel (base.html's
          // pagetitle block), outside this region, so a title
          // that depended on the loaded record would never catch
          // up. The fragment carries it as data-title-text; sync
          // the heading before the region itself is replaced.
          var titleText = fresh.getAttribute("data-title-text");
          if (titleText) {
            var h1 = document.querySelector("#main-content > h1");
            if (h1) {
              var gt = h1.querySelector(".gt");
              (gt || h1).textContent = titleText;
            }
          }
          region.outerHTML = html;
        } else {
          schedule();
        }
      }).catch(function () {
        // A failed poll is not news; the next one is already
        // scheduled and retries at the same cadence.
        schedule();
      });
    }
    kicks.push(function () {
      if (stopped) return;
      if (timer !== null) {
        window.clearTimeout(timer);
        timer = null;
      }
      poll();
    });
    schedule();
  }

  armRegions(document);
  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState !== "visible") return;
    for (var i = 0; i < kicks.length; i++) {
      kicks[i]();
    }
  });
})();

// --- Market history chart: day-point tooltips ---------------
// Hover (mouse) or focus (keyboard) follows the point; a tap
// pins the tip so it can be read, and the next tap — the same
// point again, or anywhere else — lets it go. Pressing and
// dragging across the chart scrubs to the nearest recorded day
// while the tip follows the pointer. Delegation on document
// throughout, so the live-region fragment swap that replaces
// the chart body keeps working untouched.
(function () {
  var pinned = null; // the .cdot holding the tip open by tap
  var scrub = null; // active pointer drag across a chart
  var suppressClickChart = null;
  var suppressClickUntil = 0;

  function tipOf(dot) {
    var chart = dot.closest ? dot.closest(".pchart") : null;
    return chart ? chart.querySelector(".ctip") : null;
  }

  function fill(tip, dot) {
    tip.innerHTML = "";
    var date = document.createElement("div");
    date.className = "ctip-date";
    date.textContent = dot.getAttribute("data-date");
    tip.appendChild(date);
    var rows = [];
    // Balance charts carry one figure; the Net worth chart breaks
    // its total into ISK and assets; market history dots carry the
    // full day. Render whichever the dot brought.
    var balance = dot.getAttribute("data-balance");
    if (dot.getAttribute("data-isk") !== null) {
      rows.push(["ISK", dot.getAttribute("data-isk") + " ISK"]);
      rows.push(["Assets & orders", dot.getAttribute("data-assets") + " ISK"]);
      rows.push(["Total", balance + " ISK"]);
    } else if (balance) {
      rows.push(["Balance", balance + " ISK"]);
    }
    if (dot.getAttribute("data-avg")) {
      rows.push(["Average", dot.getAttribute("data-avg") + " ISK"]);
      rows.push(["High", dot.getAttribute("data-high") + " ISK"]);
      rows.push(["Low", dot.getAttribute("data-low") + " ISK"]);
      rows.push(["Volume", dot.getAttribute("data-vol")]);
    }
    for (var i = 0; i < rows.length; i++) {
      var line = document.createElement("div");
      line.textContent = rows[i][0] + ": " + rows[i][1];
      tip.appendChild(line);
    }
  }

  function markSelected(dot) {
    var chart = dot.closest ? dot.closest(".pchart") : null;
    if (!chart) return;
    var selected = chart.querySelectorAll(".cdot.is-selected");
    for (var i = 0; i < selected.length; i++) {
      if (selected[i] !== dot) selected[i].classList.remove("is-selected");
    }
    dot.classList.add("is-selected");
  }

  function show(dot, clientX) {
    var tip = tipOf(dot);
    if (!tip) return;
    markSelected(dot);
    fill(tip, dot);
    tip.hidden = false;
    var chart = tip.parentElement;
    var crect = chart.getBoundingClientRect();
    var drect = dot.getBoundingClientRect();
    var left = (typeof clientX === "number")
      ? clientX - crect.left
      : drect.left - crect.left + drect.width / 2;
    var half = tip.offsetWidth / 2;
    if (left < half) left = half;
    if (left > crect.width - half) left = crect.width - half;
    tip.style.left = left + "px";
    tip.style.top = (drect.top - crect.top) + "px";
  }

  function hide(dot) {
    var tip = tipOf(dot);
    if (tip) tip.hidden = true;
    if (dot) dot.classList.remove("is-selected");
  }

  function asDot(ev) {
    var t = ev.target;
    return t && t.closest ? t.closest(".cdot") : null;
  }

  function chartOf(target) {
    return target && target.closest ? target.closest(".pchart") : null;
  }

  function nearestDot(chart, clientX) {
    var dots = chart.querySelectorAll(".cdot");
    var best = null;
    var bestDistance = Infinity;
    for (var i = 0; i < dots.length; i++) {
      var rect = dots[i].getBoundingClientRect();
      var distance = Math.abs(clientX - (rect.left + rect.width / 2));
      if (distance < bestDistance) {
        bestDistance = distance;
        best = dots[i];
      }
    }
    return best;
  }

  document.addEventListener("pointerdown", function (ev) {
    if (ev.button !== undefined && ev.button !== 0) return;
    var chart = chartOf(ev.target);
    if (!chart || !chart.hasAttribute("data-chart-scrub")) return;
    if (!ev.target || !ev.target.closest || !ev.target.closest("svg")) return;
    var dot = nearestDot(chart, ev.clientX);
    if (!dot) return;
    scrub = { chart: chart, pointerId: ev.pointerId, dot: dot, moved: false };
    show(dot, ev.clientX);
  });
  document.addEventListener("pointermove", function (ev) {
    if (!scrub || ev.pointerId !== scrub.pointerId) return;
    var dot = nearestDot(scrub.chart, ev.clientX);
    if (!dot) return;
    scrub.moved = true;
    scrub.dot = dot;
    show(dot, ev.clientX);
  });
  function endScrub(ev) {
    if (!scrub || ev.pointerId !== scrub.pointerId) return;
    if (scrub.moved && ev.type !== "pointercancel") {
      if (pinned && pinned !== scrub.dot) hide(pinned);
      pinned = scrub.dot;
      show(scrub.dot, ev.clientX);
      suppressClickChart = scrub.chart;
      suppressClickUntil = Date.now() + 500;
    }
    scrub = null;
  }
  document.addEventListener("pointerup", endScrub);
  document.addEventListener("pointercancel", endScrub);

  document.addEventListener("mouseover", function (ev) {
    var dot = asDot(ev);
    if (dot && pinned !== dot) show(dot);
  });
  document.addEventListener("mouseout", function (ev) {
    var dot = asDot(ev);
    if (dot && pinned !== dot) hide(dot);
  });
  document.addEventListener("focusin", function (ev) {
    var dot = asDot(ev);
    if (dot) show(dot);
  });
  document.addEventListener("focusout", function (ev) {
    var dot = asDot(ev);
    if (dot && pinned !== dot) hide(dot);
  });
  document.addEventListener("click", function (ev) {
    var clickChart = chartOf(ev.target);
    if (clickChart && clickChart === suppressClickChart && Date.now() <= suppressClickUntil) {
      suppressClickChart = null;
      return;
    }
    var dot = asDot(ev);
    if (dot) {
      if (pinned === dot) {
        pinned = null;
        hide(dot);
      } else {
        if (pinned) hide(pinned);
        pinned = dot;
        show(dot);
      }
    } else if (pinned) {
      hide(pinned);
      pinned = null;
    }
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape" && pinned) {
      hide(pinned);
      pinned = null;
    }
  });
})();

// Page-sync indicator: while this page still has data on the
// way (names, descriptions, price history it asked for), the
// top banner shows a small ember ring with the count. It polls
// a cache-only status endpoint — never the data itself — and
// hides the moment the page has everything. Polling never
// gives up while the page is still waiting: fast at first,
// then a slower steady cadence for as long as the status says
// pending, so the ring never sits spinning over a page nobody
// is checking on any more (a cold worker can take minutes to
// warm a first name; the page still fills itself in). A tab
// coming back to the front re-checks immediately. Background
// sync in general is not reported here.
(function () {
  var indicator = document.getElementById("page-sync-indicator");
  if (!indicator || !window.fetch) return;
  var label = indicator.querySelector("[data-page-sync-label]");
  // The poll in flight, if any. A swap stops it (the URL it asks
  // about belongs to the old page) and starts one for the new
  // page; the visibility handler below always wakes the current.
  var active = null;

  function unresolvedRegions() {
    return document.querySelectorAll(
      "[data-live-region][data-poll-state='pending'], [data-live-region][data-poll-state='loading']"
    ).length;
  }

  function show(pending) {
    indicator.hidden = false;
    if (label) {
      label.textContent = pending > 0
        ? "Loading page data · " + pending
        : "Loading page data…";
    }
  }

  function stopPageSync() {
    if (active) {
      active.stopped = true;
      if (active.timer !== null) window.clearTimeout(active.timer);
      active = null;
    }
  }

  function startPageSync() {
    stopPageSync();
    var job = {
      page: window.location.pathname + window.location.search,
      polls: 0,
      timer: null,
      stopped: false
    };
    active = job;
    function schedule() {
      if (job.stopped) return;
      // ~1.5s for the first couple of minutes, then a steady
      // ~5s while the page is still waiting.
      job.timer = window.setTimeout(poll, job.polls <= 80 ? 1500 : 5000);
    }
    function poll() {
      job.timer = null;
      if (job.stopped) return;
      job.polls += 1;
      window.fetch("/sync/page-status?page=" + encodeURIComponent(job.page), {
        credentials: "same-origin",
        headers: { "X-Requested-With": "XMLHttpRequest" }
      }).then(function (resp) {
        return resp.json();
      }).then(function (status) {
        var pending = status && typeof status.pending === "number" ? status.pending : 0;
        if (pending > 0 || unresolvedRegions() > 0) {
          show(pending);
          schedule();
        } else {
          indicator.hidden = true;
          if (active === job) active = null;
          job.stopped = true;
        }
      }).catch(function () {
        // A failed status poll is not news; the next one retries
        // at the same cadence.
        schedule();
      });
    }
    job.kick = function () {
      if (job.stopped) return;
      if (job.timer !== null) {
        window.clearTimeout(job.timer);
        job.timer = null;
      }
      poll();
    };
    schedule();
  }

  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState !== "visible" || !active) return;
    active.kick();
  });
  document.addEventListener("evesynapse:content-will-swap", stopPageSync);
  document.addEventListener("evesynapse:content-ready", startPageSync);
  startPageSync();
})();

// Navigation state: the wide sidebar can be expanded, collapsed
// to an icon rail, or hidden entirely; the choice persists in
// local storage. On small screens the same sidebar is an
// off-canvas drawer driven by the checkbox in the page markup,
// so it still opens and closes with scripts disabled.
(function () {
  "use strict";
  var root = document.documentElement;
  var sidebar = document.getElementById("site-nav");
  var drawerToggle = document.getElementById("nav-drawer-toggle");
  var hamburger = document.querySelector(".nav-hamburger");
  var collapseButton = document.getElementById("nav-collapse");
  var hideButton = document.getElementById("nav-hide");
  var reopenButton = document.getElementById("nav-reopen");
  var drawerClose = document.querySelector(".nav-drawer-close");
  var expandAllButton = document.getElementById("nav-expand-all");
  var categoryBranches = sidebar ? sidebar.querySelectorAll(".sidenav details.branch") : [];
  var drawerQuery = window.matchMedia ? window.matchMedia("(max-width: 860px)") : { matches: false };
  var storageKey = "evesynapse-nav";

  function currentState() {
    return root.getAttribute("data-nav") || "expanded";
  }
  function storeState(state) {
    try {
      window.localStorage.setItem(storageKey, state);
    } catch (e) { /* private mode: this page still works */ }
    // Also in a cookie, so the server renders the saved layout on
    // the next page load and it never paints the default first (see
    // ui_navstate.go). Without storage, a cookie alone still keeps it.
    document.cookie = storageKey + "=" + state + "; path=/; max-age=31536000; samesite=lax" +
      (window.location.protocol === "https:" ? "; secure" : "");
  }
  function allCategoriesOpen() {
    if (!categoryBranches.length) {
      return false;
    }
    for (var i = 0; i < categoryBranches.length; i++) {
      if (!categoryBranches[i].open) {
        return false;
      }
    }
    return true;
  }
  function syncExpandAllButton() {
    if (!expandAllButton) {
      return;
    }
    var allOpen = allCategoriesOpen();
    expandAllButton.setAttribute("aria-expanded", allOpen ? "true" : "false");
    expandAllButton.setAttribute("aria-label", allOpen ? "Collapse all navigation categories" : "Expand all navigation categories");
    expandAllButton.setAttribute("title", allOpen ? "Collapse all navigation categories" : "Expand all navigation categories");
  }
  // The category of the page being shown stays open while the menu
  // shows its labels.
  function openActiveCategory() {
    var active = sidebar ? sidebar.querySelector(".sidenav details.branch.active") : null;
    if (active) active.open = true;
  }
  function closeCategoryBranches(except) {
    for (var i = 0; i < categoryBranches.length; i++) {
      if (categoryBranches[i] !== except) {
        categoryBranches[i].open = false;
      }
    }
  }
  function railActive() {
    return currentState() === "rail" && !drawerQuery.matches;
  }
  function syncNavigationControls() {
    var drawerOpen = !!(drawerToggle && drawerToggle.checked && drawerQuery.matches);
    if (hamburger) {
      hamburger.setAttribute("aria-expanded", drawerOpen ? "true" : "false");
    }
    if (reopenButton) {
      reopenButton.setAttribute("aria-expanded", currentState() === "hidden" ? "false" : "true");
    }
    if (collapseButton) {
      var rail = currentState() === "rail";
      collapseButton.setAttribute("aria-label", rail ? "Expand navigation" : "Collapse navigation");
      collapseButton.setAttribute("title", rail ? "Expand navigation" : "Collapse navigation");
    }
    syncExpandAllButton();
  }
  function applyNavigationState(state) {
    if (state === "rail") {
      // Arrive in the rail clean: categories opened in the
      // wide sidebar keep their <details open> state across
      // the switch, and each one would paint its own tall
      // flyout in the rail column — a merged stack under a
      // row of lit-up icons. Fold them all; from here the
      // rail opens one flyout at a time.
      closeCategoryBranches(null);
    }
    if (state === "expanded") {
      root.removeAttribute("data-nav");
      openActiveCategory();
    } else {
      root.setAttribute("data-nav", state);
    }
    storeState(state);
    syncNavigationControls();
  }
  function closeDrawer(returnFocus) {
    if (drawerToggle && drawerToggle.checked) {
      drawerToggle.checked = false;
      drawerToggle.dispatchEvent(new Event("change", { bubbles: true }));
    }
    if (returnFocus && hamburger) {
      hamburger.focus();
    }
  }
  if (expandAllButton) {
    expandAllButton.addEventListener("click", function () {
      var shouldOpen = !allCategoriesOpen();
      for (var i = 0; i < categoryBranches.length; i++) {
        categoryBranches[i].open = shouldOpen;
      }
      syncExpandAllButton();
    });
  }
  for (var categoryIndex = 0; categoryIndex < categoryBranches.length; categoryIndex++) {
    categoryBranches[categoryIndex].addEventListener("toggle", function (event) {
      // In the rail one flyout at a time: opening a category
      // folds whichever flyout is already out. The wide and
      // drawer presentations keep their many-open behavior.
      if (event.target.open && railActive()) {
        closeCategoryBranches(event.target);
      }
      syncExpandAllButton();
    });
  }
  // In the rail the flyouts also follow the pointer: hovering a
  // category brings its flyout up, and the flyout folds when the
  // pointer leaves — no waiting for a click somewhere else.
  // Click keeps working too (touch, and the plain toggle); the
  // one adjustment is that a pointer click on a flyout the
  // pointer itself opened does not toggle it shut under the
  // cursor — in the rail, leaving is what closes it. Keyboard
  // activation (a click with detail 0) keeps the native toggle
  // in both directions, and Escape still folds a branch (the
  // handler near the top of this file). The wide sidebar and
  // the drawer never see any of this: every handler checks
  // railActive() first, so their menus open and stay by click
  // exactly as before.
  var railHoverCloseMs = 150; // crossing the rail→flyout gap must not flicker the flyout shut
  function cancelRailHoverClose(branch) {
    if (branch._railHoverTimer) {
      window.clearTimeout(branch._railHoverTimer);
      branch._railHoverTimer = null;
    }
  }
  function scheduleRailHoverClose(branch) {
    cancelRailHoverClose(branch);
    branch._railHoverTimer = window.setTimeout(function () {
      branch._railHoverTimer = null;
      if (!railActive() || !branch.open) {
        return;
      }
      // A keyboard user working the flyout's links keeps it:
      // focus inside is not the pointer leaving. (Focus resting
      // on the summary itself is the pointer's own click path,
      // so the summary does not hold the flyout open.)
      var summary = branch.querySelector("summary");
      if (document.activeElement && summary && branch.contains(document.activeElement) &&
          document.activeElement !== summary) {
        return;
      }
      branch.open = false;
      syncExpandAllButton();
    }, railHoverCloseMs);
  }
  for (var hoverIndex = 0; hoverIndex < categoryBranches.length; hoverIndex++) {
    (function (branch) {
      branch.addEventListener("mouseenter", function () {
        if (!railActive()) {
          return;
        }
        cancelRailHoverClose(branch);
        if (!branch.open) {
          closeCategoryBranches(branch);
          branch.open = true;
        }
      });
      branch.addEventListener("mouseleave", function () {
        if (!railActive()) {
          return;
        }
        scheduleRailHoverClose(branch);
      });
      branch.addEventListener("click", function (e) {
        if (!railActive() || e.detail === 0 || !branch.open) {
          return;
        }
        var summary = branch.querySelector("summary");
        if (summary && (e.target === summary || summary.contains(e.target))) {
          e.preventDefault();
        }
      });
    })(categoryBranches[hoverIndex]);
  }
  if (collapseButton) {
    collapseButton.addEventListener("click", function () {
      applyNavigationState(currentState() === "rail" ? "expanded" : "rail");
    });
  }
  if (hideButton) {
    hideButton.addEventListener("click", function () {
      applyNavigationState("hidden");
    });
  }
  if (reopenButton) {
    reopenButton.addEventListener("click", function () {
      applyNavigationState("expanded");
      reopenButton.setAttribute("aria-expanded", "true");
    });
  }
  if (drawerToggle) {
    drawerToggle.addEventListener("change", syncNavigationControls);
  }
  if (hamburger) {
    hamburger.addEventListener("keydown", function (e) {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        hamburger.click();
      }
    });
  }
  if (drawerClose) {
    drawerClose.addEventListener("keydown", function (e) {
      if (e.key === "Enter" || e.key === " ") {
        e.preventDefault();
        drawerClose.click();
      }
    });
  }
  if (sidebar) {
    sidebar.addEventListener("click", function (e) {
      if (!drawerQuery.matches || !drawerToggle || !drawerToggle.checked) {
        return;
      }
      var link = e.target && e.target.closest ? e.target.closest("a") : null;
      if (link) {
        closeDrawer(false);
      }
    });
  }
  document.addEventListener("click", function (e) {
    // Rail flyouts dismiss on an outside click, like the
    // drawer does; a click inside the open category (its
    // summary, its menu) is the category's own business.
    if (!railActive() || !sidebar) {
      return;
    }
    var openBranch = sidebar.querySelector(".sidenav details.branch[open]");
    if (!openBranch) return;
    // A link chosen in the flyout closes it at once: the page swap
    // that follows paints the new content over anything still out,
    // which left the flyout half covered behind the panel.
    var link = e.target.closest && e.target.closest(".menu a");
    if (!openBranch.contains(e.target) || (link && openBranch.contains(link))) {
      closeCategoryBranches(null);
    }
  });
  // However a page swap starts (a link, Back, a shortcut), no rail
  // flyout stays out over the incoming page.
  document.addEventListener("evesynapse:content-will-swap", function () {
    if (railActive()) closeCategoryBranches(null);
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && drawerQuery.matches) {
      closeDrawer(true);
    }
  });
  function handleDrawerBreakpoint(event) {
    // Entering small screens always starts with the drawer
    // closed; leaving them must not leave it hanging open either.
    closeDrawer(false);
    if (railActive()) {
      closeCategoryBranches(null);
    } else {
      openActiveCategory();
    }
    syncNavigationControls();
  }
  if (drawerQuery.addEventListener) {
    drawerQuery.addEventListener("change", handleDrawerBreakpoint);
  } else if (drawerQuery.addListener) {
    drawerQuery.addListener(handleDrawerBreakpoint);
  }
  // The drawer starts closed on small screens, period: a
  // restored or stale checkbox state must not pop it open.
  if (drawerQuery.matches) {
    closeDrawer(false);
  }
  // A page that loads straight into the rail (the saved choice
  // the head script applied, a restored page) starts with
  // every category folded too — no flyout stack waiting under
  // the icons.
  if (railActive()) {
    closeCategoryBranches(null);
  } else {
    openActiveCategory();
  }
  syncNavigationControls();
  // Enable width/visibility transitions only after the
  // state the head script restored has had frames to land:
  // user toggles glide, but a refresh or navigation into a
  // saved rail/hidden state opens already settled.
  function enableNavMotion() {
    root.classList.add("nav-motion-ready");
  }
  if (window.requestAnimationFrame) {
    window.requestAnimationFrame(function () {
      window.requestAnimationFrame(enableNavMotion);
    });
  } else {
    window.setTimeout(enableNavMotion, 0);
  }
})();

// --- Quick jump (Ctrl+K) --------------------------------------
// A keyboard-first palette over the same pool the banner search
// serves (/search.json): your characters, items, corporations,
// alliances and known pilots, each jumping to the page it links
// to everywhere else. The app's own pages ride along as a static
// group, so the palette is useful before a single letter is
// typed. Everything renders from text nodes; nothing here makes
// a request the banner search wouldn't.
(function () {
  "use strict";
  var overlay = document.getElementById("quickjump");
  var input = document.getElementById("quickjump-q");
  var list = document.getElementById("quickjump-results");
  var openBtn = document.getElementById("quickjump-open");
  if (!overlay || !input || !list || !window.fetch) return;

  // The palette's page group: every destination the sidebar
  // offers, in sidebar order. A test pins this list against
  // base.html's nav, so a new page joins both or neither.
  var quickJumpPages = [
    { name: "Home", url: "/" },
    { name: "Overview", url: "/character/" },
    { name: "Characters", url: "/characters/" },
    { name: "Skill Plans", url: "/skills/plans" },
    { name: "Ship Workbench", url: "/fittings/" },
    { name: "My Loadouts", url: "/fittings/saved/" },
    { name: "Public Loadouts Browser", url: "/fittings/public/" },
    { name: "Killmails", url: "/killmails/" },
    { name: "Mail", url: "/mail/" },
    { name: "Calendar", url: "/calendar/" },
    { name: "Contacts", url: "/contacts/" },
    { name: "Assets", url: "/assets/" },
    { name: "Blueprints", url: "/industry/" },
    { name: "My Jobs", url: "/industry/jobs/" },
    { name: "Mining Ledger", url: "/industry/mining/" },
    { name: "Build Planner", url: "/planner/" },
    { name: "My Colonies", url: "/planets/" },
    { name: "PI Planner", url: "/industry/pi/" },
    { name: "Market", url: "/market/" },
    { name: "Spread Scanner", url: "/market/scanner/" },
    { name: "Trade Ideas", url: "/market/ideas/" },
    { name: "Tradefinder", url: "/market/tradefinder/" },
    { name: "Top Stations", url: "/market/leaderboard/" },
    { name: "Restock Planner", url: "/market/restock/" },
    { name: "Item Browser", url: "/items/" },
    { name: "Appraisal", url: "/market/appraise/" },
    { name: "Wallet", url: "/wallet/" },
    { name: "My Orders", url: "/orders/" },
    { name: "My Contracts", url: "/contracts/" },
    { name: "Contract Search", url: "/contracts/search/" },
    { name: "Corporation Overview", url: "/corporations/" },
    { name: "Corporation Members", url: "/corporations/members/" },
    { name: "Corporation Wallets", url: "/corporations/wallets/" },
    { name: "Corporation Orders", url: "/corporations/orders/" },
    { name: "Corporation Assets", url: "/corporations/assets/" },
    { name: "Corporation Structures", url: "/corporations/structures/" },
    { name: "Corporation Killmails", url: "/corporations/killmails/" },
    { name: "Doctrines", url: "/doctrines/" },
    { name: "Corporate Loadouts", url: "/doctrines/fits/" },
    { name: "Ship Replacement (SRP)", url: "/srp/" },
    { name: "Discord Server", url: "/discord/servers" },
    { name: "Groups", url: "/groups/" },
    { name: "Corporation Settings", url: "/corporations/settings/" },
    { name: "Wars", url: "/intel/wars/" },
    { name: "Incursions", url: "/intel/incursions/" },
    { name: "Faction Warfare", url: "/intel/fw/" },
    { name: "Sync", url: "/sync/" },
    { name: "Admin", url: "/admin/" }
  ];

  // Sync and Admin are for administrators: the menu only has them for
  // one (base.html), and the palette offers them on the same terms.
  // The server refuses the pages to anyone else either way.
  var adminOnlyPages = { "/sync/": true, "/admin/": true };
  var isAdmin = !!document.querySelector(".sidenav-utility");

  // Result groups, in display order; server hits carry these
  // kinds already, pages are matched locally.
  var quickJumpGroups = [
    ["page", "Pages"],
    ["character", "Characters"],
    ["item", "Items"],
    ["system", "Systems"],
    ["region", "Regions"],
    ["corporation", "Corporations"],
    ["alliance", "Alliances"],
    ["pilot", "Pilots"]
  ];

  function jumpURL(hit) {
    switch (hit.kind) {
      case "page": return hit.url;
      case "character": return "/character/?character=" + encodeURIComponent(hit.id);
      case "pilot": return "/pilot/?character=" + encodeURIComponent(hit.id);
      case "corporation": return "/corporation/?corporation=" + encodeURIComponent(hit.id);
      case "alliance": return "/alliance/?alliance=" + encodeURIComponent(hit.id);
      case "system": return "/system/?system=" + encodeURIComponent(hit.id);
      case "region": return "/region/?region=" + encodeURIComponent(hit.id);
      default: return "/market/?type=" + encodeURIComponent(hit.id);
    }
  }

  var entries = [];   // flat, selectable rows in display order
  var selected = -1;
  var timer = null;
  var querySerial = 0;

  function isOpen() { return !overlay.hidden; }

  function paint(groups) {
    list.innerHTML = "";
    entries = [];
    selected = -1;
    for (var g = 0; g < groups.length; g++) {
      var hits = groups[g].hits;
      if (!hits.length) continue;
      var head = document.createElement("li");
      head.className = "qj-group";
      head.setAttribute("role", "presentation");
      head.textContent = groups[g].label;
      list.appendChild(head);
      for (var i = 0; i < hits.length; i++) {
        (function (hit) {
          var li = document.createElement("li");
          li.setAttribute("role", "option");
          li.id = "quickjump-result-" + entries.length;
          var name = document.createElement("span");
          name.textContent = hit.name;
          li.appendChild(name);
          if (hit.label) {
            var lab = document.createElement("span");
            lab.className = "qj-label";
            if (window.EveSynapseHitLabel) window.EveSynapseHitLabel(lab, hit);
            else lab.textContent = hit.label;
            li.appendChild(lab);
          }
          if (hit.kind === "pilot-pending") {
            li.className = "qj-pending";
            li.setAttribute("aria-disabled", "true");
            list.appendChild(li);
            return;
          }
          var idx = entries.length;
          entries.push({ url: jumpURL(hit), el: li });
          li.addEventListener("mousedown", function (ev) {
            ev.preventDefault();
            go(idx);
          });
          li.addEventListener("mousemove", function () { select(idx); });
          list.appendChild(li);
        })(hits[i]);
      }
    }
    if (entries.length) select(0);
  }

  function select(idx) {
    if (idx === selected) return;
    if (entries[selected]) entries[selected].el.classList.remove("sel");
    selected = idx;
    if (entries[selected]) {
      entries[selected].el.classList.add("sel");
      input.setAttribute("aria-activedescendant", entries[selected].el.id);
      if (entries[selected].el.scrollIntoView) {
        entries[selected].el.scrollIntoView({ block: "nearest" });
      }
    } else {
      input.removeAttribute("aria-activedescendant");
    }
  }

  function go(idx) {
    var entry = entries[idx];
    if (!entry) return;
    closePalette();
    window.location.href = entry.url;
  }

  function pageHits(q) {
    var out = [];
    for (var i = 0; i < quickJumpPages.length; i++) {
      if (adminOnlyPages[quickJumpPages[i].url] && !isAdmin) continue;
      if (!q || quickJumpPages[i].name.toLowerCase().indexOf(q) !== -1) {
        out.push({
          kind: "page", name: quickJumpPages[i].name,
          url: quickJumpPages[i].url, label: quickJumpPages[i].url
        });
      }
    }
    return out;
  }

  function render(hits, q) {
    var groups = [{ label: "Pages", hits: pageHits(q) }];
    var byKind = {};
    for (var i = 0; i < hits.length; i++) {
      (byKind[hits[i].kind] = byKind[hits[i].kind] || []).push(hits[i]);
    }
    for (var g = 1; g < quickJumpGroups.length; g++) {
      var kind = quickJumpGroups[g][0];
      var rows = byKind[kind] || [];
      if (kind === "pilot" && byKind["pilot-pending"]) {
        rows = rows.concat(byKind["pilot-pending"]);
      }
      groups.push({ label: quickJumpGroups[g][1], hits: rows });
    }
    paint(groups);
  }

  function ask(q) {
    var serial = ++querySerial;
    render([], q); // page matches land instantly
    if (q.length < 2) return;
    window.fetch("/search.json?q=" + encodeURIComponent(q), {
      cache: "no-store",
      headers: { "Accept": "application/json" }
    }).then(function (resp) {
      return resp.ok ? resp.json() : [];
    }).then(function (rows) {
      if (serial !== querySerial) return; // typed past this answer
      render(Array.isArray(rows) ? rows : [], q);
    }).catch(function () { /* page matches already stand */ });
  }

  // The palette is one of the shared modals (EveSynapseModal):
  // the backdrop, Escape-from-anywhere and the focus hand-back are
  // the shared ones; paletteClosed is what the palette adds.
  function openPalette() {
    if (isOpen()) return;
    document.body.classList.add("quickjump-open");
    input.value = "";
    ask("");
    window.EveSynapseModal.open(overlay, { focus: false, onClose: paletteClosed });
    input.focus();
    input.select();
  }

  function paletteClosed() {
    document.body.classList.remove("quickjump-open");
    querySerial++; // an answer in flight no longer matters
    if (timer) window.clearTimeout(timer);
    timer = null;
  }

  function closePalette() {
    if (!isOpen()) return;
    window.EveSynapseModal.close(overlay);
  }

  if (openBtn) {
    openBtn.addEventListener("click", function () {
      if (isOpen()) closePalette(); else openPalette();
    });
  }

  // Ctrl+K / Cmd+K from anywhere — including inside a form
  // field, which is the point — opens or closes the palette.
  // Plain typing never does; the modifiers are the trigger.
  document.addEventListener("keydown", function (ev) {
    if ((ev.ctrlKey || ev.metaKey) && !ev.altKey &&
        (ev.key === "k" || ev.key === "K")) {
      ev.preventDefault();
      if (isOpen()) closePalette(); else openPalette();
    }
  });

  input.addEventListener("input", function () {
    var q = input.value.trim().toLowerCase();
    if (timer) window.clearTimeout(timer);
    timer = window.setTimeout(function () { ask(q); }, 150);
  });

  input.addEventListener("keydown", function (ev) {
    if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
      ev.preventDefault();
      if (!entries.length) return;
      var delta = ev.key === "ArrowDown" ? 1 : -1;
      select((selected + delta + entries.length) % entries.length);
    } else if (ev.key === "Enter") {
      if (selected >= 0) {
        ev.preventDefault();
        go(selected);
      }
    } else if (ev.key === "Escape") {
      ev.preventDefault();
      ev.stopPropagation();
      closePalette();
    }
  });

})();


// --- Theme toggle (dark/light/factions) ------------------------------
// A stored choice in localStorage wins and is written onto
// document.documentElement.dataset.theme; with no stored
// choice the attribute stays unset so the stylesheet's
// prefers-color-scheme media query applies. The button only
// renders while logged in (see base.html), so everything here
// is guarded: no button, no work.
// Simple dark/light toggle (faction themes deferred).
(function () {
  "use strict";
  var btn = document.getElementById("theme-toggle");
  if (!btn) return;
  var root = document.documentElement;
  var storageKey = "evesynapse-theme";

  function readSaved() {
    try {
      var v = window.localStorage.getItem(storageKey);
      return (v === "dark" || v === "light") ? v : "";
    } catch (e) { return ""; }
  }
  function save(theme) {
    try {
      window.localStorage.setItem(storageKey, theme);
    } catch (e) { /* private mode: the click still works this visit */ }
  }
  // The theme in effect right now: a manually set attribute
  // wins; otherwise dark is the default, whatever the OS prefers.
  // Icon visibility is CSS-driven (html[data-theme] selectors).
  function effective() {
    var attr = root.getAttribute("data-theme");
    if (attr === "dark" || attr === "light") return attr;
    return "dark";
  }

  var saved = readSaved();
  if (saved) root.setAttribute("data-theme", saved);

  btn.addEventListener("click", function () {
    var next = effective() === "dark" ? "light" : "dark";
    root.setAttribute("data-theme", next);
    save(next);
  });
})();

// Character selector dropdown with search/filter.
(function() {
  // Live selectors: the outside-click handler below is bound once
  // and walks these, so swaps never pile handlers. Detached roots
  // prune themselves on the next click or swap.
  var liveSelectors = [];
  var outsideBound = false;
  function pruneSelectors() {
    liveSelectors = liveSelectors.filter(function (s) {
      return document.contains(s.root);
    });
  }
  function armOutsideClick() {
    if (outsideBound) return;
    outsideBound = true;
    document.addEventListener('click', function (e) {
      pruneSelectors();
      liveSelectors.forEach(function (s) {
        if (!s.root.contains(e.target)) {
          s.dropdown.hidden = true;
          // Restore active name if input was cleared.
          if (!s.input.value && s.active) s.input.value = s.active.getAttribute('data-name') || '';
        }
      });
    });
  }
  function initCharselector(root) {
    var input = root.querySelector('.charselector-input');
    var dropdown = root.querySelector('.charselector-dropdown');
    if (!input || !dropdown) return;
    if (root.getAttribute('data-charselector-live') === '1') return;
    root.setAttribute('data-charselector-live', '1');
    var options = Array.prototype.slice.call(root.querySelectorAll('.charselector-option'));

    // Show active character name in the input initially.
    var active = root.querySelector('.charselector-option.active');
    if (active) input.value = active.getAttribute('data-name') || '';

    function filter() {
      var q = input.value.toLowerCase().trim();
      options.forEach(function(opt) {
        var name = (opt.getAttribute('data-name') || '').toLowerCase();
        var tags = (opt.getAttribute('data-tags') || '').toLowerCase();
        var match = !q || name.indexOf(q) !== -1 || tags.indexOf(q) !== -1;
        opt.hidden = !match;
      });
    }

    input.addEventListener('focus', function() {
      input.value = '';
      filter();
      dropdown.hidden = false;
    });
    input.addEventListener('input', function() {
      filter();
      dropdown.hidden = false;
    });
    // Close on Escape.
    input.addEventListener('keydown', function(e) {
      if (e.key === 'Escape') {
        dropdown.hidden = true;
        input.blur();
      }
    });
    liveSelectors.push({ root: root, input: input, dropdown: dropdown, active: active });
    armOutsideClick();
  }
  function initCharselectors(scope) {
    var roots = scope.querySelectorAll('[data-charselector]');
    for (var i = 0; i < roots.length; i++) initCharselector(roots[i]);
  }
  window.EveSynapseContent.onReady(initCharselectors);
  document.addEventListener('evesynapse:content-will-swap', pruneSelectors);
  initCharselectors(document);

  // --- CSV export links ---------------------------------------
  // Market/planner pages carry an "Export CSV" link whose href is
  // the current URL plus format=csv, set here. The address bar
  // already shows the new page when this runs after a swap.
  function initCSVExport(root) {
    var csvExportLink = root.querySelector("#csv-export");
    if (csvExportLink) {
      csvExportLink.href = window.location.pathname + window.location.search +
        (window.location.search ? "&" : "?") + "format=csv";
    }
  }

  // --- Client-side table filters ------------------------------
  // Industry search boxes filter their table's rows by a
  // data-search attribute:
  //   <input data-table-filter="bp-table" data-row-class="bp-row"
  //          data-header-class="bp-cat-header">
  // Rows whose data-search misses hide; category headers with no
  // visible rows hide with them. data-header-class is optional.
  function initTableFilters(root) {
    var tableFilterBoxes = root.querySelectorAll("input[data-table-filter]");
    for (var filterIndex = 0; filterIndex < tableFilterBoxes.length; filterIndex++) {
      (function (filterInput) {
      var table = document.getElementById(filterInput.getAttribute("data-table-filter"));
      if (!table) return;
      var rowClass = filterInput.getAttribute("data-row-class") || "";
      if (!rowClass) return;
      var headerClass = filterInput.getAttribute("data-header-class") || "";
      filterInput.addEventListener("input", function () {
        var q = filterInput.value.toLowerCase();
        table.querySelectorAll("." + rowClass).forEach(function (r) {
          var match = !q || (r.getAttribute("data-search") || "").toLowerCase().indexOf(q) >= 0;
          r.style.display = match ? "" : "none";
        });
        if (!headerClass) return;
        table.querySelectorAll("." + headerClass).forEach(function (h) {
          var next = h.nextElementSibling;
          var hasVisible = false;
          while (next && !next.classList.contains(headerClass)) {
            if (next.style.display !== "none") { hasVisible = true; break; }
            next = next.nextElementSibling;
          }
          h.style.display = hasVisible ? "" : "none";
        });
      });
      })(tableFilterBoxes[filterIndex]);
    }
  }
  function initContentWidgets(root) {
    initCSVExport(root);
    initTableFilters(root);
  }
  window.EveSynapseContent.onReady(initContentWidgets);
  initContentWidgets(document);

  // --- Auto-refresh pages -------------------------------------
  // The Sync page refreshes itself every few seconds
  // (body[data-autorefresh]) in place: the page is fetched again and
  // only its content is swapped, so the reader keeps their scroll
  // position (a full reload went back to the top). With JavaScript
  // off, a <noscript> meta-refresh does the same. The refresh waits
  // out typing in a field and hidden tabs, and never runs for readers
  // who asked for reduced motion — they get the page as it loaded,
  // plus the Sync page's own manual controls. A swap away stops the chain:
  // without this the old page's timer would reload the new one.
  // (Swaps onto an auto-refresh page fall back to a full load, so
  // the chain only ever starts here, on first paint.)
  var autoRefreshStopped = false;
  document.addEventListener("evesynapse:content-will-swap", function () {
    autoRefreshStopped = true;
  });
  // refreshInPlace fetches this page again and swaps its main
  // content, leaving the scroll position alone, then lets the content
  // widgets set up the fresh markup. Anything unexpected is skipped
  // until the next tick rather than reloading.
  function refreshInPlace() {
    if (!window.fetch || !window.DOMParser) {
      window.location.reload();
      return Promise.resolve();
    }
    var url = window.location.pathname + window.location.search;
    return window.fetch(url, { credentials: "same-origin", cache: "no-store" })
      .then(function (resp) { return resp.ok && !resp.redirected ? resp.text() : ""; })
      .then(function (html) {
        if (!html || autoRefreshStopped) return;
        var active = document.activeElement;
        if (active && /^(INPUT|TEXTAREA|SELECT)$/.test(active.tagName || "")) return;
        var next = new DOMParser().parseFromString(html, "text/html").getElementById("main-content");
        var main = document.getElementById("main-content");
        if (!next || !main) return;
        main.innerHTML = next.innerHTML;
        document.dispatchEvent(new CustomEvent("evesynapse:content-ready", {
          detail: { root: main, url: url }
        }));
      });
  }
  (function () {
    var autoRefreshSeconds = parseInt(document.body.getAttribute("data-autorefresh") || "", 10);
    if (autoRefreshSeconds > 0 &&
        !(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches)) {
      (function scheduleRefresh() {
        window.setTimeout(function () {
          if (autoRefreshStopped) return;
          var active = document.activeElement;
          var typing = active && /^(INPUT|TEXTAREA|SELECT)$/.test(active.tagName || "");
          if (!typing && !document.hidden) {
            refreshInPlace().then(scheduleRefresh, scheduleRefresh);
            return;
          }
          scheduleRefresh();
        }, autoRefreshSeconds * 1000);
      })();
    }
  })();


  // --- Sortable tables ------------------------------------------
  // Any table under the page content sorts by a header click: again
  // reverses. A column whose cells all read as numbers (ISK, counts,
  // percentages, "1.2B") sorts numerically, else as text. A row's
  // detail rows (rows that are one wide cell, like the fleet table's)
  // move with it, and in a capped widget the rows past the cap
  // (data-wmore) are re-marked by position. data-nosort opts a table
  // out; empty headers do nothing.
  function sortKey(cell) {
    var raw = (cell ? (cell.getAttribute("data-sort") || cell.textContent) : "").trim();
    var m = raw.replace(/ISK|,|\s|%|\+|×/g, "").replace(/^[−–]/, "-").match(/^(-?\d+(?:\.\d+)?)([KMBT]?)$/i);
    if (m) {
      var mult = { "": 1, k: 1e3, m: 1e6, b: 1e9, t: 1e12 }[m[2].toLowerCase()];
      return { num: parseFloat(m[1]) * mult, text: raw };
    }
    return { num: null, text: raw.toLowerCase() };
  }
  document.addEventListener("click", function (ev) {
    var th = ev.target && ev.target.closest ? ev.target.closest("#main-content table > thead th") : null;
    if (!th || ev.target.closest("a, button, input, select")) return;
    var table = th.closest("table");
    if (!table || table.hasAttribute("data-nosort") || !th.textContent.trim()) return;
    var body = table.tBodies[0];
    if (!body || body.rows.length < 2) return;
    var col = th.cellIndex;
    // Rows with their attached detail rows.
    var groups = [];
    Array.prototype.forEach.call(body.rows, function (row) {
      var single = row.cells.length === 1 && row.cells[0].colSpan > 1;
      if (single && groups.length) groups[groups.length - 1].push(row);
      else groups.push([row]);
    });
    var dir = th.getAttribute("aria-sort") === "ascending" ? -1 : 1;
    var keys = groups.map(function (g) { return sortKey(g[0].cells[col]); });
    var numeric = keys.every(function (k) { return k.num !== null || k.text === "" || k.text === "—"; });
    var order = groups.map(function (g, i) { return i; });
    order.sort(function (a, b) {
      var ka = keys[a], kb = keys[b], r;
      if (numeric) {
        var na = ka.num === null ? -Infinity : ka.num, nb = kb.num === null ? -Infinity : kb.num;
        r = na - nb;
      } else {
        r = ka.text < kb.text ? -1 : ka.text > kb.text ? 1 : 0;
      }
      return r * dir || a - b;
    });
    var capped = body.querySelectorAll("tr[data-wmore]").length;
    var heads = [];
    order.forEach(function (i) {
      groups[i].forEach(function (row) { body.appendChild(row); });
      heads.push(groups[i][0]);
    });
    if (capped) {
      heads.forEach(function (row, i) {
        if (i >= heads.length - capped) row.setAttribute("data-wmore", "");
        else row.removeAttribute("data-wmore");
      });
    }
    Array.prototype.forEach.call(th.parentNode.cells, function (c) { c.removeAttribute("aria-sort"); });
    th.setAttribute("aria-sort", dir === 1 ? "ascending" : "descending");
  });

  // --- Filters that apply as they change -----------------------
  // A control marked data-autosubmit (the Item Database search's
  // category and group pickers and its published-only box) submits
  // its form the moment it changes, so a filter applies at once
  // and the result keeps its Category › Group layout; the form's
  // own button stays for readers without JavaScript. Picking a
  // different category clears the group first: the old group
  // would otherwise pull the category back.
  document.addEventListener("change", function (ev) {
    var control = ev.target && ev.target.closest ? ev.target.closest("[data-autosubmit]") : null;
    if (!control || !control.form) return;
    if (control.name === "category" && control.form.elements.group) {
      control.form.elements.group.value = "";
    }
    if (control.form.requestSubmit) control.form.requestSubmit();
    else control.form.submit();
  });

  // --- Item Database: published-only, applied in place --------
  // On the browse tree (the category list, a category's groups,
  // a group's types) the "Published items only" box carries
  // data-published-toggle instead: flipping it prunes the rows
  // already on the page rather than navigating. Branches with
  // no published types disappear, counts read as published
  // counts while it is on, unpublished type rows hide, and the
  // drill-down links pick the state up so it survives the next
  // click. Every figure rides in data attributes, so toggling
  // back restores the full tree exactly as it was.
  document.addEventListener("change", function (ev) {
    var box = ev.target && ev.target.closest ? ev.target.closest("[data-published-toggle]") : null;
    if (!box) return;
    var on = box.checked;
    Array.prototype.forEach.call(document.querySelectorAll("tr[data-pubcount]"), function (row) {
      row.hidden = on && row.getAttribute("data-pubcount") === "0";
    });
    Array.prototype.forEach.call(document.querySelectorAll("[data-count]"), function (cell) {
      cell.textContent = on ? cell.getAttribute("data-pub") : cell.getAttribute("data-total");
    });
    Array.prototype.forEach.call(document.querySelectorAll("tr[data-published]"), function (row) {
      row.hidden = on && row.getAttribute("data-published") === "0";
    });
    Array.prototype.forEach.call(document.querySelectorAll('a[href^="/items/"]'), function (a) {
      var href = a.getAttribute("href");
      var has = /[?&]published=1(&|$)/.test(href);
      if (on && !has) {
        a.setAttribute("href", href + (href.indexOf("?") >= 0 ? "&" : "?") + "published=1");
      } else if (!on && has) {
        a.setAttribute("href", href.replace("&published=1", "").replace("?published=1&", "?").replace(/\?published=1$/, ""));
      }
    });
  });

  // --- Buttons that navigate -----------------------------------
  // <button data-href="/path"> goes to that path on click (the content
  // security policy allows no inline handlers). Only same-site paths count.
  document.addEventListener("click", function (ev) {
    var button = ev.target && ev.target.closest ? ev.target.closest("button[data-href]") : null;
    if (!button) return;
    var href = button.getAttribute("data-href") || "";
    if (href.charAt(0) === "/" && href.charAt(1) !== "/") window.location.href = href;
  });

  // --- Skill planner: move and remove in place -------------------
  // A move or remove button in the plan table posts its form with
  // fetch and swaps the returned table (#plan-body) into the page, so
  // the search box, results and scroll position stay where they are.
  // The server answers with the reason in data-message when it refuses
  // a move (a prerequisite is in the way); that is read out through the
  // status region above the table. Without JavaScript the same forms
  // post normally and come back to the page with the reason flashed.
  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    if (!window.fetch || !form || !form.matches || !form.matches(".plan-move-form, .plan-remove-form")) return;
    var editor = form.closest("[data-plan-editor]");
    var body = form.closest("[data-plan-body]");
    if (!editor || !body) return;
    ev.preventDefault();

    var data = new URLSearchParams(new FormData(form));
    var dir = ev.submitter && ev.submitter.name === "dir" ? ev.submitter.value : "";
    if (dir) data.set("dir", dir);
    var row = form.closest("tr");
    var skill = row ? row.getAttribute("data-skill") : "";
    var removing = form.classList.contains("plan-remove-form");

    window.fetch(form.action, {
      method: "POST",
      credentials: "same-origin",
      headers: {
        "X-Requested-With": "XMLHttpRequest",
        "Content-Type": "application/x-www-form-urlencoded"
      },
      body: data.toString()
    }).then(function (resp) {
      return resp.text();
    }).then(function (html) {
      var probe = document.createElement("div");
      probe.innerHTML = html;
      var fresh = probe.querySelector("[data-plan-body]");
      if (!fresh) {
        // Not the table (signed out, an error): let the page show it.
        window.location.reload();
        return;
      }
      var live = editor.querySelector(".plan-live");
      var message = fresh.getAttribute("data-message") || "";
      if (live) {
        live.textContent = "";
        if (message) {
          var note = document.createElement("p");
          note.className = "notice";
          note.textContent = message;
          live.appendChild(note);
        }
      }
      body.replaceWith(fresh);
      // Keep the keyboard where it was: the same button on the same
      // skill, else the other move button; after a remove, the table.
      var target = null;
      if (!removing && skill) {
        var next = fresh.querySelector("tr[data-skill='" + skill + "']");
        if (next) {
          target = next.querySelector("button[name='dir'][value='" + dir + "']:not([aria-disabled='true'])") ||
            next.querySelector("button[name='dir']:not([aria-disabled='true'])") ||
            next.querySelector("button[name='dir']");
        }
      }
      (target || fresh).focus();
    }).catch(function () {
      // The request itself failed: fall back to the plain post.
      form.submit();
    });
  });

  // --- Skill planner: find and delete saved plans ----------------
  // The search above the list of plans narrows it as you type (Enter
  // opens the first match, the arrow keys walk the list, Escape clears
  // it); without JavaScript the same search is a form that reloads the
  // page with ?pq=. The delete button on a row asks in place — the row
  // turns into "Delete this plan? [Delete] [Keep]" — where a plain link
  // goes to the page's own confirmation.
  function initPlanPicker(root) {
    var picker = root.querySelector("[data-plan-picker]");
    if (!picker || picker.getAttribute("data-plan-live") === "1") return;
    picker.setAttribute("data-plan-live", "1");
    var input = picker.querySelector("[data-plan-filter]");
    var items = Array.prototype.slice.call(picker.querySelectorAll(".plan-item"));
    var none = picker.querySelector(".plan-none");

    function shown() {
      return items.filter(function (li) { return !li.hidden; });
    }
    function filter() {
      var q = input.value.trim().toLowerCase();
      items.forEach(function (li) {
        li.hidden = q !== "" && (li.getAttribute("data-name") || "").toLowerCase().indexOf(q) === -1;
      });
      if (none) none.hidden = shown().length > 0;
    }
    function openLink(li) {
      return li.querySelector(".plan-open");
    }

    if (input) {
      input.addEventListener("input", filter);
      input.addEventListener("keydown", function (ev) {
        var rows = shown();
        if (ev.key === "ArrowDown" && rows.length) {
          ev.preventDefault();
          openLink(rows[0]).focus();
        } else if (ev.key === "Escape" && input.value !== "") {
          ev.preventDefault();
          input.value = "";
          filter();
        } else if (ev.key === "Enter" && input.value.trim() !== "" && rows.length) {
          ev.preventDefault();
          window.location.href = openLink(rows[0]).href;
        }
      });
      if (input.value !== "") filter();
    }

    picker.addEventListener("keydown", function (ev) {
      var link = ev.target && ev.target.closest ? ev.target.closest(".plan-open") : null;
      if (link && (ev.key === "ArrowDown" || ev.key === "ArrowUp")) {
        var rows = shown();
        var at = rows.indexOf(link.closest(".plan-item"));
        var next = rows[at + (ev.key === "ArrowDown" ? 1 : -1)];
        ev.preventDefault();
        if (next) openLink(next).focus();
        else if (ev.key === "ArrowUp" && input) input.focus();
      } else if (ev.key === "Escape" && ev.target.closest && ev.target.closest(".plan-confirm")) {
        var keep = ev.target.closest(".plan-item").querySelector("[data-plan-keep]");
        if (keep) keep.click();
      }
    });

    picker.addEventListener("click", function (ev) {
      var target = ev.target && ev.target.closest ? ev.target : null;
      if (!target) return;
      var del = target.closest("[data-plan-delete]");
      if (del) {
        var row = del.closest(".plan-item");
        var form = row && row.querySelector(".plan-confirm");
        if (!form) return;
        ev.preventDefault();
        row.classList.add("is-confirming");
        form.hidden = false;
        form.querySelector("button[type='submit']").focus();
        return;
      }
      var keepBtn = target.closest("[data-plan-keep]");
      if (keepBtn) {
        var li = keepBtn.closest(".plan-item");
        li.classList.remove("is-confirming");
        li.querySelector(".plan-confirm").hidden = true;
        li.querySelector("[data-plan-delete]").focus();
      }
    });
  }
  window.EveSynapseContent.onReady(initPlanPicker);
  initPlanPicker(document);

  // --- Data-driven geometry ------------------------------------
  // The content security policy forbids inline style attributes, so
  // per-element numbers ride data-* attributes and are applied here
  // through the CSSOM (which the policy allows):
  //   data-w         fill width of a bar, percent
  //   data-left      absolute position from the left, percent
  //   data-top       absolute position from the top, percent
  //   data-rot       rotation in degrees; an element with data-rot
  //                  (the fitting separators) is also centered on
  //                  its left/top point, i.e.
  //                  transform: translate(-50%,-50%) rotate(<n>deg)
  //   data-pad-left  left padding (indent), px
  // applyGeometry is the rule for one element; the code below only
  // finds elements: at load, and again for markup injected later
  // (fit.js swaps whole server-rendered fragments into the page).
  // A missing, empty, or non-numeric value sets nothing (never
  // "NaNpx"). Widths clamp to 0–100: a bar over capacity arrives as
  // e.g. 123.4 and its track hides the overflow anyway. Positions
  // pass through as sent, as the inline styles did.
  function geometryNumber(el, name) {
    var raw = el.getAttribute(name);
    if (raw === null || raw.trim() === "") return null;
    var n = Number(raw);
    return isFinite(n) ? n : null;
  }

  function applyGeometry(el) {
    var w = geometryNumber(el, "data-w");
    if (w !== null) el.style.width = Math.min(Math.max(w, 0), 100) + "%";
    var left = geometryNumber(el, "data-left");
    if (left !== null) el.style.left = left + "%";
    var top = geometryNumber(el, "data-top");
    if (top !== null) el.style.top = top + "%";
    var rot = geometryNumber(el, "data-rot");
    if (rot !== null) el.style.transform = "translate(-50%,-50%) rotate(" + rot + "deg)";
    var pad = geometryNumber(el, "data-pad-left");
    if (pad !== null && pad >= 0) el.style.paddingLeft = pad + "px";
  }

  var GEOMETRY_SELECTOR = "[data-w],[data-left],[data-top],[data-rot],[data-pad-left]";

  function applyDataGeometry(root) {
    if (!root || root.nodeType !== 1) return;
    if (root.matches(GEOMETRY_SELECTOR)) applyGeometry(root);
    root.querySelectorAll(GEOMETRY_SELECTOR).forEach(applyGeometry);
  }

  applyDataGeometry(document.body);
  if (window.MutationObserver) {
    new MutationObserver(function (records) {
      records.forEach(function (record) {
        record.addedNodes.forEach(applyDataGeometry);
      });
    }).observe(document.body, { childList: true, subtree: true });
  }
})();

// The notification list in the top bar: it closes on a click or tap
// anywhere else and on Escape, and opens and closes with the
// navigation's motion (the CSS transition on .notify.is-open). The
// <details> works without this; this only adds the closing and the
// motion, and keeps the icon up to date while the page is open.
(function () {
  "use strict";
  var menu = document.getElementById("notify-menu");
  if (!menu) return;
  var waiting = null; // an update held back while the list is open
  function show(html, unread) {
    // The icon is rebuilt, so a reader who had tabbed to it is put
    // back on it.
    var focused = document.activeElement && menu.contains(document.activeElement);
    menu.innerHTML = html;
    menu.classList.toggle("has-unread", unread > 0);
    var summary = menu.querySelector("summary");
    if (focused && summary) summary.focus();
  }
  var closing = null;
  // As long as the transition in style.css, or no wait at all for a
  // reader who asked for less motion.
  var still = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var motion = still ? 0 : 180;

  function open() {
    window.clearTimeout(closing);
    closing = null;
    menu.open = true;
    // Reading the width makes the browser lay the list out in its
    // starting state first, so the transition has something to run
    // from.
    void menu.offsetWidth;
    menu.classList.add("is-open");
  }

  function close() {
    if (!menu.open || closing) return;
    menu.classList.remove("is-open");
    closing = window.setTimeout(function () {
      closing = null;
      menu.open = false;
      // An update that arrived while the list was open goes in now.
      if (waiting) {
        show(waiting.html, waiting.unread);
        waiting = null;
      }
    }, motion);
  }

  // The icon's contents are replaced when they change (below), so
  // the click is caught on the <details>, which stays.
  menu.addEventListener("click", function (ev) {
    if (!ev.target.closest || !ev.target.closest("summary")) return;
    ev.preventDefault();
    if (menu.open && !closing) { close(); } else { open(); }
  });
  document.addEventListener("click", function (ev) {
    if (!menu.contains(ev.target)) close();
  });
  // A swap replaces the page under an open list: fold it away
  // through the same path as an outside click.
  document.addEventListener("evesynapse:content-will-swap", function () {
    close();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Escape" || !menu.open) return;
    close();
    menu.querySelector("summary").focus();
  });

  // --- The live icon ---------------------------------------------
  // Every data-poll seconds (NOTIFY_POLL_SECONDS on the server) the
  // page asks whether the icon has changed, and swaps its contents
  // in when it has, so a new notification shows without a reload.
  // It only asks while the page is on screen, checks at once when
  // the page comes back into view or a push arrives, and leaves the
  // list alone while it is open under the reader's pointer.
  var every = parseInt(menu.getAttribute("data-poll"), 10);
  if (!(every > 0) || !window.fetch) return;
  var etag = "";
  var timer = null;
  var asking = false;
  var stopped = false;
  function check() {
    if (stopped || asking || document.hidden) return;
    asking = true;
    var headers = {};
    if (etag) headers["If-None-Match"] = etag;
    window.fetch("/notifications/badge", { credentials: "same-origin", headers: headers, cache: "no-store" }).then(function (res) {
      // Anything that is not the icon (the sign-in page after the
      // session ended, an error page) ends the checks: the next
      // page load starts them again.
      if (res.status !== 304 && !(res.ok && res.headers.get("X-Notify-Badge") === "1")) {
        stopped = true;
        return;
      }
      if (res.status === 304) return;
      var unread = parseInt(res.headers.get("X-Notify-Unread"), 10) || 0;
      var tag = res.headers.get("ETag") || "";
      return res.text().then(function (html) {
        etag = tag;
        if (menu.open) {
          waiting = { html: html, unread: unread };
        } else {
          show(html, unread);
        }
      });
    }).catch(function () {
      // Offline or a dropped request: try again at the next turn.
    }).then(function () { asking = false; });
  }

  function schedule() {
    window.clearInterval(timer);
    timer = window.setInterval(check, every * 1000);
  }

  schedule();
  document.addEventListener("visibilitychange", function () {
    if (!document.hidden) { check(); schedule(); }
  });
  if (navigator.serviceWorker) {
    navigator.serviceWorker.addEventListener("message", function (ev) {
      if (ev.data && ev.data.push === "received") check();
    });
  }
})();

// The first-run notification prompt: a small card under the bell.
// The server renders it open on the first view of a session for an
// account that has not answered (and never again once it has); this
// only gives it the bell list's motion, a way to close it without
// answering (the close is this visit only — answering is the form),
// and the bell list's "Notification choices" entry as a way back
// in. The form itself is an ordinary post and works without this.
(function () {
  "use strict";
  var card = document.getElementById("notify-prompt");
  if (!card) return;
  var still = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var hiding = null; // a close on its way; opening again cancels it

  function show() {
    window.clearTimeout(hiding);
    hiding = null;
    card.hidden = false;
    // Reading the width makes the browser lay the card out in its
    // starting state first, so the transition has something to run
    // from (the bell list's trick).
    void card.offsetWidth;
    card.classList.add("is-open");
  }

  function hide() {
    if (card.hidden || hiding) return;
    card.classList.remove("is-open");
    if (still) {
      card.hidden = true;
      return;
    }
    hiding = window.setTimeout(function () {
      hiding = null;
      card.hidden = true;
    }, 180);
  }

  if (!card.hidden) {
    // Rendered open: give it the drop.
    window.requestAnimationFrame(function () { card.classList.add("is-open"); });
  }

  document.addEventListener("click", function (ev) {
    if (!ev.target.closest) return;
    if (ev.target.closest("[data-notify-prompt-open]")) {
      var menu = document.getElementById("notify-menu");
      if (menu) {
        menu.open = false;
        menu.classList.remove("is-open");
      }
      show();
      return;
    }
    if (card.hidden) return;
    if (ev.target.closest("[data-notify-prompt-close]") || !card.contains(ev.target)) hide();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape" && !card.hidden) hide();
  });
  document.addEventListener("evesynapse:content-will-swap", function () {
    if (!card.hidden) hide();
  });
})();

// Notification settings: under each kind, the characters it is on for
// (a [data-checkselector]: the character selector's look, with a tick
// box per character). The field shows the count and filters the list
// as you type; the list stays open while boxes are ticked and closes
// on a click elsewhere or Escape. The boxes are ordinary form fields,
// so saving works the same with or without this.
(function () {
  "use strict";
  var docWired = false;
  function wireCheckSelectors() {
    var selectors = Array.prototype.slice.call(document.querySelectorAll("[data-checkselector]"));
    if (!selectors.length) return;

    selectors.forEach(function (root) {
      if (root.getAttribute("data-checkselector-wired") === "1") return;
      root.setAttribute("data-checkselector-wired", "1");
      var input = root.querySelector(".charselector-input");
      var dropdown = root.querySelector(".charselector-dropdown");
      var options = Array.prototype.slice.call(root.querySelectorAll(".charselector-option"));
      var boxes = Array.prototype.slice.call(root.querySelectorAll('input[type="checkbox"]'));
      if (!input || !dropdown) return;

      function summarize() {
        var on = boxes.filter(function (box) { return box.checked; }).length;
        // A list to pick additions from says how many are picked.
        if (root.getAttribute("data-summary") === "add") {
          input.placeholder = on ? on + " picked: press Add" : "Add characters…";
          return;
        }
        var total = options.length;
        var text = "On for " + on + " of " + total + (total === 1 ? " character" : " characters");
        if (boxes.length < total) {
          text = boxes.length ? text + " (" + (total - boxes.length) + " without access)"
            : "No access granted on " + total + (total === 1 ? " character" : " characters");
        }
        input.placeholder = text;
      }

      function filter() {
        var q = input.value.toLowerCase().trim();
        options.forEach(function (option) {
          var name = (option.getAttribute("data-name") || "").toLowerCase();
          option.hidden = !!q && name.indexOf(q) === -1;
        });
      }

      function close() {
        if (dropdown.hidden) return;
        dropdown.hidden = true;
        input.value = "";
        filter();
      }

      function open() {
        // One list at a time.
        var all = document.querySelectorAll("[data-checkselector]");
        for (var i = 0; i < all.length; i++) {
          if (all[i] !== root && all[i].closeList) all[i].closeList();
        }
        dropdown.hidden = false;
      }

      root.closeList = close;
      input.addEventListener("focus", open);
      input.addEventListener("click", open);
      input.addEventListener("input", function () { filter(); dropdown.hidden = false; });
      root.addEventListener("change", summarize);
      root.addEventListener("keydown", function (ev) {
        if (ev.key === "Escape") { close(); input.blur(); }
        // Enter in the search field would send the whole form.
        if (ev.key === "Enter" && ev.target === input) ev.preventDefault();
      });
      summarize();
    });

    if (!docWired) {
      docWired = true;
      document.addEventListener("click", function (ev) {
        var all = document.querySelectorAll("[data-checkselector]");
        for (var i = 0; i < all.length; i++) {
          if (!all[i].contains(ev.target) && all[i].closeList) all[i].closeList();
        }
      });
    }
  }
  if (window.EveSynapseContent && window.EveSynapseContent.onReady) {
    window.EveSynapseContent.onReady(wireCheckSelectors);
  }
  wireCheckSelectors();
})();

// The character switcher in the top bar: like the notification list,
// it closes on a click or tap anywhere else and on Escape, and opens
// and closes with the navigation's motion (the CSS transition on
// .topbar-switcher.is-open). The <details> works without this.
(function () {
  "use strict";
  var switcher = document.querySelector("details.topbar-switcher");
  if (!switcher) return;
  var summary = switcher.querySelector("summary");
  var closing = null;
  var still = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
  var motion = still ? 0 : 180; // as long as the transition in style.css

  function open() {
    window.clearTimeout(closing);
    closing = null;
    switcher.open = true;
    void switcher.offsetWidth; // lay it out closed first, so there is something to move from
    switcher.classList.add("is-open");
  }
  function close() {
    if (!switcher.open || closing) return;
    switcher.classList.remove("is-open");
    closing = window.setTimeout(function () {
      closing = null;
      switcher.open = false;
    }, motion);
  }

  summary.addEventListener("click", function (ev) {
    ev.preventDefault();
    if (switcher.open && !closing) { close(); } else { open(); }
  });
  document.addEventListener("click", function (ev) {
    if (!switcher.contains(ev.target)) close();
  });
  // A swap replaces the page under an open switcher: fold it
  // away through the same path as an outside click.
  document.addEventListener("evesynapse:content-will-swap", function () {
    close();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Escape" || !switcher.open) return;
    close();
    summary.focus();
  });
})();

// "Install app" in the sidebar. A browser that can install the site as
// an app (Chrome and Edge, on Android and on a computer) says so with
// a beforeinstallprompt event, and otherwise keeps the offer behind
// its own menu where few people find it. The event is held, the link
// is shown, and a click opens the browser's own install dialog.
// Nothing is offered unasked: the link only sits there until it is
// clicked. Where there is no such event (Safari, Firefox) or the app
// is already installed, the link is never shown.
(function () {
  "use strict";
  var btn = document.getElementById("install-app");
  if (!btn) return;
  var offer = null;
  window.addEventListener("beforeinstallprompt", function (e) {
    e.preventDefault(); // no banner of the browser's own; ours is the link
    offer = e;
    btn.hidden = false;
  });
  btn.addEventListener("click", function () {
    if (!offer) return;
    var held = offer;
    offer = null; // an offer can be used once; the browser sends another if it is declined
    btn.hidden = true;
    held.prompt();
  });
  window.addEventListener("appinstalled", function () {
    offer = null;
    btn.hidden = true;
  });
})();

// Copy buttons: [data-copy] copies its data-copy-text, or the
// text of [data-copy-from]; the label flips to Copied briefly.
// Server-rendered share blocks use it so no page needs its own
// clipboard code.
(function () {
  "use strict";
  function flash(btn) {
    var prior = btn.textContent;
    btn.textContent = "Copied";
    setTimeout(function () { btn.textContent = prior; }, 1500);
  }
  document.addEventListener("click", function (ev) {
    var btn = ev.target && ev.target.closest ? ev.target.closest("[data-copy]") : null;
    if (!btn) return;
    var text = btn.getAttribute("data-copy-text") || "";
    var from = btn.getAttribute("data-copy-from");
    if (from) {
      var el = document.getElementById(from);
      if (el) text = el.textContent || "";
    }
    if (!text) return;
    function done(ok) { if (ok) flash(btn); }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(function () { done(true); }, function () { done(false); });
    } else {
      var ta = document.createElement("textarea");
      ta.value = text;
      document.body.appendChild(ta);
      ta.select();
      try { done(document.execCommand("copy")); } catch (e) { done(false); }
      document.body.removeChild(ta);
    }
  });
})();

// The appraisal form's Clear button: empties the pastes and
// puts the basis back to the remembered defaults the page
// loaded with, then focuses the paste box.
(function () {
  "use strict";
  var items = document.getElementById("ap-items");
  if (!items || !items.form) return;
  var form = items.form;
  var remembered = {};
  ["hub", "mode", "pct"].forEach(function (name) {
    var el = form.querySelector('[name="' + name + '"]');
    if (el) remembered[name] = el.value;
  });
  document.addEventListener("click", function (ev) {
    var btn = ev.target && ev.target.closest ? ev.target.closest("[data-clear-appraise]") : null;
    if (!btn) return;
    items.value = "";
    var exclude = document.getElementById("ap-exclude");
    if (exclude) exclude.value = "";
    ["hub", "mode", "pct"].forEach(function (name) {
      var el = form.querySelector('[name="' + name + '"]');
      if (el && remembered[name] != null) el.value = remembered[name];
    });
    var mult = form.querySelector('[name="multiplier"]');
    if (mult) mult.value = "1";
    items.focus();
  });
})();

// The sign-up chooser (/auth/link): select all, tick every extra without
// leaving the place on the page, and a live count of the permissions.
(function () {
  "use strict";
  function recount(form) {
    var countEl = document.querySelector("[data-lk-count]");
    if (!form || !countEl) return;
    var req = {}, all = {};
    var feats = form.querySelectorAll('input[name="feature"]');
    for (var i = 0; i < feats.length; i++) {
      if (!feats[i].checked) continue;
      var sc = (feats[i].getAttribute("data-scopes") || "").split(/\s+/);
      for (var j = 0; j < sc.length; j++) if (sc[j]) { req[sc[j]] = true; all[sc[j]] = true; }
    }
    var extras = form.querySelectorAll('input[name="enhance"]');
    for (var k = 0; k < extras.length; k++) if (extras[k].checked) all[extras[k].value] = true;
    var n = Object.keys(all).length;
    countEl.textContent = String(n);
    var reqEl = document.querySelector("[data-lk-required]");
    if (reqEl) reqEl.textContent = String(Object.keys(req).length);
  }
  document.addEventListener("click", function (ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest("[data-tick-all],[data-select-all]") : null;
    if (!t) return;
    ev.preventDefault();
    if (t.hasAttribute("data-select-all")) {
      var cards = document.querySelectorAll(".lk-cards input[type=checkbox]");
      var every = true;
      for (var i = 0; i < cards.length; i++) if (!cards[i].checked) every = false;
      for (var j = 0; j < cards.length; j++) cards[j].checked = !every;
      return;
    }
    var form = t.closest("form");
    if (!form) return;
    var boxes = form.querySelectorAll('input[name="enhance"]');
    var allOn = true;
    for (var a = 0; a < boxes.length; a++) if (!boxes[a].checked) allOn = false;
    for (var b = 0; b < boxes.length; b++) boxes[b].checked = !allOn;
    recount(form);
  });
  document.addEventListener("change", function (ev) {
    var form = ev.target && ev.target.closest ? ev.target.closest("[data-lk-form]") : null;
    if (form) recount(form);
  });
})();
