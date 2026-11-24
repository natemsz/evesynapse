// Home: the signed-in Overview's widgets and its Customize engine.
// Loaded only on the home page (base.html gates it); with
// JavaScript off, the grid lays out and every Customize action
// keeps its plain form.
(function () {
  "use strict";

  function onContentReady(fn) {
    window.EveSynapseContent.onReady(fn);
  }

  // --- Fleet filter (home overview) ---------------------------
  // The fleet table renders every character server-side; this
  // just hides rows that don't match the typed text or the
  // active chip: All, a tag, or Not training. A row's detail row
  // goes with it, and a click anywhere on a row (not on a link)
  // opens its detail, like its chevron. No JS: the full list is
  // simply visible, details and all.
  function initFleetFilter(root) {
    var fleetFilter = root.querySelector("[data-fleet-filter]");
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
    // The KPI band's idle count ("N idle, show them") is a link to
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
  // All / Critical / Warning / Info swap which cards show; a deck
  // whose cards are all filtered out hides with them.
  function initBriefTabs(root) {
    var tabs = root.querySelectorAll("[data-brief]");
    if (!tabs.length) return;
    var cards = root.querySelectorAll(".brief-cards .brief-flip-card");
    var decks = root.querySelectorAll(".brief-cards .brief-deck");
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
        for (var s = 0; s < decks.length; s++) {
          var vis = briefLayout(decks[s]);
          briefFitDeck(decks[s]);
          decks[s].classList.toggle("brief-off", !vis.length);
        }
      });
    }
  }
  onContentReady(initBriefTabs);
  initBriefTabs(document);

  // --- Briefing one-row pager --------------------------------------
  // The decks sit in one row; the edge buttons smooth-scroll the
  // row by a full view (the CSS scroll-behavior animates it, and
  // touch swipes scroll the row natively). Buttons show only
  // while the row actually overflows, and grey out at the ends.
  function initBriefPager(root) {
    var lists = root.querySelectorAll(".brief-pager .brief-cards");
    for (var n = 0; n < lists.length; n++) {
      (function (list) {
        if (list.dataset.briefPagerWired) return;
        list.dataset.briefPagerWired = "1";
        var pager = list.closest(".brief-pager");
        if (!pager) return;
        var prev = pager.querySelector("[data-brief-page=\"-1\"]");
        var next = pager.querySelector("[data-brief-page=\"1\"]");
        if (!prev || !next) return;
        var update = function () {
          var over = list.scrollWidth - list.clientWidth > 4;
          prev.hidden = !over;
          next.hidden = !over;
          if (!over) return;
          prev.disabled = list.scrollLeft <= 4;
          next.disabled = list.scrollLeft >= list.scrollWidth - list.clientWidth - 4;
        };
        var page = function (dir) {
          var reduce = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
          list.scrollBy({ left: dir * list.clientWidth, behavior: reduce ? "auto" : "smooth" });
        };
        prev.addEventListener("click", function () { page(-1); });
        next.addEventListener("click", function () { page(1); });
        list.addEventListener("scroll", update, { passive: true });
        window.addEventListener("resize", update);
        var tabs = root.querySelectorAll("[data-brief]");
        for (var t = 0; t < tabs.length; t++) {
          tabs[t].addEventListener("click", function () { setTimeout(update, 60); });
        }
        update();
        setTimeout(update, 300);
      })(lists[n]);
    }
  }
  onContentReady(initBriefPager);
  initBriefPager(document);

  // --- Briefing deck flip ------------------------------------------
  // Each kind is a deck with one painted card: the face, over two
  // empty shells that suggest the stack behind it. The more button
  // (bottom right) and horizontal swipes flip through the deck:
  // the face folds edge-on, swaps under the fold, and the next
  // card unfolds (briefFlip). The deck holds one height
  // throughout (briefFitDeck).
  function briefDeckVisible(deck) {
    var cards = deck.querySelectorAll(".brief-flip-card");
    var out = [];
    for (var i = 0; i < cards.length; i++) {
      if (!cards[i].classList.contains("brief-off")) out.push(cards[i]);
    }
    return out;
  }
  // Positions run from the face: 0 in front, 1 and 2 the sheets
  // behind, the rest hidden at the back. except keeps its current
  // position (the leaving face, mid-flip).
  function briefLayout(deck, except) {
    var vis = briefDeckVisible(deck);
    if (!vis.length) return vis;
    var face = deck._face && vis.indexOf(deck._face) >= 0 ? deck._face : vis[0];
    deck._face = face;
    var start = vis.indexOf(face);
    for (var i = 0; i < vis.length; i++) {
      if (vis[i] === except) continue;
      vis[i].setAttribute("data-pos", String((i - start + vis.length) % vis.length));
    }
    // The deck's empty shells (style.css ::before/::after) show
    // only while the face has company, and they wear the face's
    // colors: copy its tint and edge onto the deck.
    deck.classList.toggle("has-stack", vis.length > 1);
    try {
      var cs = getComputedStyle(face);
      var tint = cs.getPropertyValue("--brief-tint");
      if (tint) deck.style.setProperty("--brief-tint", tint.trim());
      if (cs.borderLeftColor) deck.style.setProperty("--brief-edge", cs.borderLeftColor);
    } catch (e) {}
    return vis;
  }
  function briefMeasure(card) {
    if (card.getAttribute("data-pos") === "0") return card.offsetHeight;
    var prev = card.style.cssText;
    card.style.cssText = prev + ";display:block;position:absolute;visibility:hidden;height:auto;max-height:none;overflow:visible;";
    var h = card.offsetHeight;
    card.style.cssText = prev;
    return h;
  }
  // A deck with a stack holds one height, its tallest visible
  // card's, so flipping never grows or shrinks the deck and the
  // sheets behind the face show the same amount under every card.
  function briefFitDeck(deck) {
    deck.style.height = "";
    var vis = briefDeckVisible(deck);
    if (vis.length < 2) return;
    var max = 0;
    for (var i = 0; i < vis.length; i++) {
      var h = briefMeasure(vis[i]);
      if (h > max) max = h;
    }
    if (max) deck.style.height = max + "px";
  }
  function briefFlip(deck, dir) {
    if (deck.classList.contains("flipping")) return;
    var vis = briefLayout(deck);
    if (vis.length < 2) return;
    var face = deck._face;
    var idx = vis.indexOf(face);
    var nxt = vis[(idx + dir + vis.length) % vis.length];
    var reduce = window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches;
    if (reduce || !deck.animate) { deck._face = nxt; briefLayout(deck); return; }
    deck.classList.add("flipping");
    // Midpoint flip: the face folds edge-on around its center,
    // the deck swaps the face under the fold, and the new face
    // unfolds. Only one card is ever painted (see style.css), so
    // nothing travels and nothing can bleed; the swap happens at
    // 90 degrees, where the card is a line.
    var fold = null, unfold = null;
    var settled = false;
    var done = function () {
      if (settled) return;
      settled = true;
      if (fold) fold.cancel();
      if (unfold) unfold.cancel();
      deck.classList.remove("flipping");
      deck._face = nxt;
      briefLayout(deck);
    };
    try {
      fold = face.animate([
        { transform: "rotateX(0deg)", opacity: "1" },
        { transform: "rotateX(90deg)", opacity: "0.55" }
      ], { duration: 160, easing: "cubic-bezier(.4,0,.7,.4)", fill: "forwards" });
    } catch (e) { done(); return; }
    fold.onfinish = function () {
      deck._face = nxt;
      briefLayout(deck);
      if (fold) fold.cancel();
      try {
        unfold = nxt.animate([
          { transform: "rotateX(90deg)", opacity: "0.55" },
          { transform: "rotateX(0deg)", opacity: "1" }
        ], { duration: 200, easing: "cubic-bezier(.2,.6,.3,1)", fill: "forwards" });
        unfold.onfinish = function () { done(); };
      } catch (e) { done(); }
    };
    // Safety: if a finish event is ever dropped, settle anyway.
    setTimeout(done, 600);
  }
  function initBriefFlip(root) {
    var buttons = root.querySelectorAll("[data-brief-flip]");
    for (var i = 0; i < buttons.length; i++) {
      buttons[i].addEventListener("click", function () {
        var deck = this.closest(".brief-deck");
        if (deck) briefFlip(deck, 1);
      });
    }
    var decks = root.querySelectorAll(".brief-cards .brief-deck");
    for (var c = 0; c < decks.length; c++) {
      (function (deck) {
        briefLayout(deck);
        briefFitDeck(deck);
        var sx = 0, sy = 0, tracking = false;
        deck.addEventListener("pointerdown", function (e) {
          if (e.button !== undefined && e.button !== 0) return;
          if (e.target && e.target.closest && e.target.closest("a, button, form, input")) return;
          sx = e.clientX; sy = e.clientY; tracking = true;
        });
        deck.addEventListener("pointerup", function (e) {
          if (!tracking) return;
          tracking = false;
          var dx = e.clientX - sx, dy = e.clientY - sy;
          if (Math.abs(dx) > 48 && Math.abs(dx) > Math.abs(dy) * 1.4) {
            deck._briefSwipedAt = Date.now();
            briefFlip(deck, dx < 0 ? 1 : -1);
          }
        });
        deck.addEventListener("click", function (e) {
          if (deck._briefSwipedAt && Date.now() - deck._briefSwipedAt < 350) {
            e.preventDefault();
            e.stopPropagation();
          }
        }, true);
      })(decks[c]);
    }
  }
  onContentReady(initBriefFlip);
  initBriefFlip(document);

  // Card heights ride the layout width and the loaded faces:
  // refit every deck when either settles.
  var briefRefitTimer = 0;
  function briefRefitAll() {
    var decks = document.querySelectorAll(".brief-cards .brief-deck");
    for (var i = 0; i < decks.length; i++) briefFitDeck(decks[i]);
  }
  window.addEventListener("resize", function () {
    clearTimeout(briefRefitTimer);
    briefRefitTimer = setTimeout(briefRefitAll, 150);
  });
  if (document.fonts && document.fonts.ready) document.fonts.ready.then(briefRefitAll);

  // --- Briefing dismissed list -------------------------------------
  // The Dismissed (n) button opens the modal of hidden lines.
  function initBriefDismissed(root) {
    var openers = root.querySelectorAll("[data-open-brief-dismissed]");
    for (var i = 0; i < openers.length; i++) {
      openers[i].addEventListener("click", function () {
        var modal = document.getElementById(this.getAttribute("data-open-brief-dismissed"));
        if (modal && window.EveSynapseModal) EveSynapseModal.open(modal);
      });
    }
  }
  onContentReady(initBriefDismissed);
  initBriefDismissed(document);

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

  // --- Home grid: two-across fill -------------------------------
  // Between the phone band and the desktop's three columns the
  // home grid runs two across, and module widths drawn for three
  // columns leave holes: a one-column module ahead of a wide one
  // (which must start a new row), or last on the page, stands
  // alone beside an empty cell. Rather than reorder anything (the
  // grid never backfills, so Customize stays honest), the module
  // left alone on its row widens to fill it (.gfill). Desktop's
  // three columns are what Customize draws and are left as drawn;
  // the phone's one column has nothing to fill.
  //
  // Pure: given each module's column span in page order and the
  // column count, the indexes of the modules that close a row
  // short, each widens to finish its row.
  function homeFillPlan(spans, cols) {
    var fill = [], used = 0, last = -1;
    for (var i = 0; i < spans.length; i++) {
      var s = Math.min(spans[i], cols);
      if (used > 0 && used + s > cols) {
        fill.push(last);
        used = 0;
      }
      used += s;
      last = i;
      if (used >= cols) used = 0;
    }
    if (used > 0 && last >= 0) fill.push(last);
    return fill;
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports.homeFillPlan = homeFillPlan;
  }

  // A size is offerable when it sits inside the module's floor and
  // ceiling both: below the floor the content breaks, above the
  // ceiling it would only draw empty cells.
  function homeSizeAllowed(w, h, min, max) {
    return w >= min.w && w <= max.w && h >= min.h && h <= max.h;
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports.homeSizeAllowed = homeSizeAllowed;
  }

  // The resize frame's size: it starts exactly over the module,
  // the module's real size, not the pointer's inset on the grip
  // and not its nominal cells (a module can stand taller than its
  // rows), and follows the pointer's movement from there, held
  // between half a cell and the module's ceiling, and never below
  // the size it started at: taking the grip must not draw a frame
  // smaller than the module being resized.
  function homeResizeFrame(w0, h0, dx, dy, capW, capH, cellW, cellH) {
    return {
      w: Math.min(Math.max(w0 + dx, cellW * 0.5), Math.max(w0, capW)),
      h: Math.min(Math.max(h0 + dy, cellH * 0.5), Math.max(h0, capH))
    };
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports.homeResizeFrame = homeResizeFrame;
  }

  // The height of one grid row where the resized module stands:
  // the step from its row's tops to the next row's, or from the
  // previous row when it stands in the last one, or the fallback
  // when the grid has a single row. Rows size by content, so the
  // smallest step anywhere on the page (the old measure) commits
  // cells the frame never covered whenever the module's own row
  // stands taller.
  function homeRowPitch(tops, ownTop, fallback) {
    var rows = [];
    for (var i = 0; i < tops.length; i++) {
      var t = Math.round(tops[i]);
      if (rows.indexOf(t) < 0) rows.push(t);
    }
    rows.sort(function (a, b) { return a - b; });
    var at = rows.indexOf(Math.round(ownTop));
    if (at >= 0 && at + 1 < rows.length) return rows[at + 1] - rows[at];
    if (at > 0) return rows[at] - rows[at - 1];
    var best = 0;
    for (var j = 1; j < rows.length; j++) {
      var d = rows[j] - rows[j - 1];
      if (d > 0 && (!best || d < best)) best = d;
    }
    return best || fallback;
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports.homeRowPitch = homeRowPitch;
  }

  // The cells a released resize commits: the start cells plus the
  // whole cells the pointer covered, rounded plainly. The drag's
  // hysteresis holds the card's cells steady mid-drag, but the
  // release commits exactly what the frame showed, so no cell is
  // lost or gained between frame, commit and stored layout.
  function homeSnapCommit(startCells, deltaPx, pitch, hi) {
    var n = Math.round(startCells + deltaPx / pitch);
    return Math.max(1, Math.min(hi, n));
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports.homeSnapCommit = homeSnapCommit;
  }

  var homeTwoUp = window.matchMedia ? window.matchMedia("(min-width: 45.0625rem) and (max-width: 70rem)") : null;

  function layoutHomeFill(grid) {
    // Mid-resize the card under the grip owns its width; widening
    // it from here too would fight the drag.
    if (grid.querySelector(":scope > .card.resizing")) return;
    var cards = [], spans = [], i;
    for (i = 0; i < grid.children.length; i++) {
      var c = grid.children[i];
      if (!c.classList.contains("card") || c.classList.contains("dragging")) continue;
      cards.push(c);
      spans.push(c.classList.contains("gw2") || c.classList.contains("gw3") ? 2 : 1);
    }
    var on = {};
    if (homeTwoUp && homeTwoUp.matches) {
      var plan = homeFillPlan(spans, 2);
      for (i = 0; i < plan.length; i++) on[plan[i]] = true;
    }
    for (i = 0; i < cards.length; i++) {
      // Write only on change, so the observer below settles.
      if (cards[i].classList.contains("gfill") !== !!on[i]) cards[i].classList.toggle("gfill", !!on[i]);
    }
  }

  function initHomeFill(root) {
    var grids = (root || document).querySelectorAll(".grid.home-grid");
    for (var i = 0; i < grids.length; i++) {
      var grid = grids[i];
      layoutHomeFill(grid);
      if (grid._fillObserver || !window.MutationObserver) continue;
      // Customize moves, resizes, adds and removes modules in
      // place; follow it. Observers on swapped-out grids die with
      // their nodes.
      grid._fillObserver = new MutationObserver(function (g) {
        return function () { layoutHomeFill(g); };
      }(grid));
      grid._fillObserver.observe(grid, { childList: true, subtree: false, attributes: true, attributeFilter: ["class"] });
    }
  }
  onContentReady(initHomeFill);
  initHomeFill(document);
  if (homeTwoUp && homeTwoUp.addEventListener) {
    homeTwoUp.addEventListener("change", function () { initHomeFill(document); });
  }

  // --- Home customize: drag, resize, remove, add module --------
  // Layered over the plain customize forms, never replacing them:
  // this runs only on the customize view (the data-customize grid)
  // and stores through the same /home/layout endpoint, a drag
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
      }).then(function (resp) {
        // The save changed what the home pages render; the
        // shell's fetched-page cache predates it and must not
        // swap a stale copy back in over the saved layout.
        if (resp.ok) document.dispatchEvent(new CustomEvent("evesynapse:content-mutated"));
        return resp.ok;
      }, function () { return false; });
    }

    // A size decides more than a card's outline: modules draw
    // different content at different sizes (Profit Pulse's chart
    // bands, Production Lines' rows). Once a size save lands, swap
    // the card for the server's fresh render of it at the saved
    // size, so the card on screen shows the saved form and no
    // feature appears or disappears across the save.
    function refreshCard(card) {
      var id = card.getAttribute("data-widget");
      return fetch("/?customize=1", {
        headers: { "X-Requested-With": "XMLHttpRequest" },
        cache: "no-store"
      }).then(function (resp) {
        return resp.ok ? resp.text() : "";
      }, function () { return ""; }).then(function (html) {
        if (!html || !card.parentNode) return;
        var box = document.createElement("div");
        box.innerHTML = html;
        var fresh = box.querySelector('.card[data-widget="' + id + '"]');
        if (!fresh) return;
        card.replaceWith(fresh);
        enhanceCard(fresh);
      });
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
    // new cell. Moves are translate-only, scaling a module to
    // fake its size change squashes its text, and every module
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
    function maxOf(el) {
      return {
        w: parseInt(el.getAttribute("data-maxw"), 10) || 3,
        h: parseInt(el.getAttribute("data-maxh"), 10) || 3
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
    // A landed save re-renders the card at its new size
    // (refreshCard), so what the user sees is the saved form.
    function resizeCard(card, w, h) {
      var was = sizeOf(card);
      if (was.w === w && was.h === h) return;
      flip(function () { sizeCard(card, w, h); });
      postLayout({ action: "size", widget: card.getAttribute("data-widget"), w: w, h: h }).then(function (ok) {
        if (!ok) flip(function () { sizeCard(card, was.w, was.h); });
        else refreshCard(card);
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
            if (!homeSizeAllowed(w, h, minOf(card), maxOf(card))) {
              // Past the ceiling: this size would only draw
              // empty cells, so it is not an option.
              cell.disabled = true;
              cell.classList.add("over");
              cell.setAttribute("aria-label", w + " wide by " + h + " tall (more than this module can use)");
            }
            cell.addEventListener("mouseenter", function () { paintPicker(w, h); });
            cell.addEventListener("focus", function () { paintPicker(w, h); });
            cell.addEventListener("click", function () {
              if (!homeSizeAllowed(w, h, minOf(card), maxOf(card))) return;
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
        var min = minOf(card), max = maxOf(card);
        cells.forEach(function (c) {
          var cw = +c.getAttribute("data-gw"), ch = +c.getAttribute("data-gh");
          c.classList.toggle("on", cw <= w && ch <= h && homeSizeAllowed(cw, ch, min, max));
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
    // The grip drags a frame that starts exactly over the module
    // and follows the pointer from there; the
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
      var start = sizeOf(card), cur = start, min = minOf(card), ceil = maxOf(card);
      var r0 = card.getBoundingClientRect();
      var gcs = window.getComputedStyle(grid);
      var gapX = parseFloat(gcs.columnGap) || 0, gapY = parseFloat(gcs.rowGap) || 0;
      var cols = (gcs.gridTemplateColumns || "").split(" ").filter(Boolean).length || 1;
      // The cell pitch is measured from the grid itself, never
      // from the card under the grip: a card widened to fill its
      // row stands a row wide whatever its drawn size says, and
      // rows size by content, so the row pitch is the step out of
      // the row the module stands in (homeRowPitch).
      var gridRect = grid.getBoundingClientRect();
      var cellW = (gridRect.width + gapX) / cols;
      var colStart = Math.max(0, Math.min(cols - 1, Math.round((r0.left - gridRect.left) / cellW)));
      var tops = [], els = grid.querySelectorAll(":scope > .card");
      for (var i = 0; i < els.length; i++) {
        if (els[i].classList.contains("dragging")) continue;
        tops.push(els[i].getBoundingClientRect().top);
      }
      var rowPx = parseFloat(gcs.getPropertyValue("--home-row")) *
        parseFloat(window.getComputedStyle(document.documentElement).fontSize);
      var cellH = homeRowPitch(tops, r0.top, (rowPx || 176) + gapY);
      if (!(cellH > 24)) cellH = (rowPx || 176) + gapY;
      var maxW = Math.max(1, Math.min(ceil.w, cols - colStart));
      var maxH = ceil.h;
      var lastX = ev.clientX, lastY = ev.clientY, under = false;
      var startX = ev.clientX, startY = ev.clientY;

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
        // The frame starts exactly over the module and follows the
        // pointer's movement from there (homeResizeFrame), so it is
        // never smaller than the module at the grab. The snapped
        // size still comes from the module's cells: width from its
        // real edge, height from how far the pointer has moved
        // since the press, added to the rows the module had, a
        // module standing taller than its rows (its content needs
        // the room) must not jump to that height the moment the
        // grip is taken.
        var relX = r0.width + (lastX - startX);
        var relY = start.h * cellH - gapY + (lastY - startY);
        var w = snap((relX + gapX) / cellW, cur.w, maxW);
        var h = snap((relY + gapY) / cellH, cur.h, maxH);
        under = w < min.w || h < min.h;
        frame.classList.toggle("under", under);
        frame.style.left = r.left + "px";
        frame.style.top = r.top + "px";
        var fs = homeResizeFrame(r0.width, r0.height, lastX - startX, lastY - startY,
          maxW * cellW - gapX, maxH * cellH - gapY, cellW, cellH);
        frame.style.width = fs.w + "px";
        frame.style.height = fs.h + "px";
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
        // The released size is the size the frame showed: the
        // start cells plus the whole cells the pointer covered,
        // without the drag's hysteresis, so the stored size is
        // the frame's final size.
        var rawW = homeSnapCommit(start.w, lastX - startX, cellW, maxW);
        var rawH = homeSnapCommit(start.h, lastY - startY, cellH, maxH);
        under = rawW < min.w || rawH < min.h;
        cur = { w: Math.max(rawW, min.w), h: Math.max(rawH, min.h) };
        if (under) refuse(card);
        var now = sizeOf(card);
        if (now.w !== cur.w || now.h !== cur.h) {
          flip(function () { sizeCard(card, cur.w, cur.h); }, card);
        }
        if (cur.w === start.w && cur.h === start.h) return;
        // The module already stands at its new size; save it, and a
        // failed save puts back the size it started at.
        postLayout({ action: "size", widget: card.getAttribute("data-widget"), w: cur.w, h: cur.h }).then(function (ok) {
          if (!ok) flip(function () { sizeCard(card, start.w, start.h); });
          else refreshCard(card);
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
    // exactly the spot it occupied, nothing under the pointer moves
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
        // lands in the same place, the add already saved.
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
})();
