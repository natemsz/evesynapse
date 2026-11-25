// Home: the signed-in Overview's widgets and its Customize engine.
// Loaded only on the home page (base.html gates it); with
// JavaScript off, the grid lays out and every Customize action
// keeps its plain form.
(function () {
  "use strict";

  function onContentReady(fn) {
    window.EveSynapseContent.onReady(fn);
  }

  // once marks el as wired for key and reports whether this is the
  // first time. Every initializer here runs for the document when the
  // file loads and again on content-ready, and a swap that adopts
  // this file does both for the same markup.
  function once(el, key) {
    var mark = "_home_" + key;
    if (el[mark]) return false;
    el[mark] = true;
    return true;
  }

  // --- Fleet filter (home overview) ---------------------------
  // Each Fleet Overview roster (.fleet-box: the card's body, its
  // phone body, the Expand view) renders every pilot in its scope
  // server-side; this hides the rows that don't match the typed
  // text or the active chip: All, a tag, or Not training. A row's
  // detail row goes with it, and a click anywhere on a row (not on a
  // link) opens its detail, like its chevron. Each body is wired on
  // its own, so two on one page never share a filter. No JS: the
  // full list is simply visible, details and all.
  function initFleetBox(box) {
    var fleetFilter = box.querySelector(".fleet-filter");
    var fleetRows = box.querySelectorAll(".fleet-row");
    if (!fleetFilter || !fleetRows.length || !once(box, "fleet")) return;
    var noMatch = box.querySelector(".fleet-nomatch");
    var shownEl = box.querySelector("[data-fleet-shown]");
    var chips = box.querySelectorAll(".fleet-tags .chip");
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
    // The idle links elsewhere on the home ask the box to show its
    // idle pilots.
    var idleChip = box.querySelector(".fleet-tags [data-idle-chip]");
    box._fleetShowIdle = idleChip ? function () {
      if (!idleChip.classList.contains("on")) pick(idleChip);
    } : null;
  }
  // An idle count ("2 idle, show them") links to the Characters page;
  // with a Fleet Overview on the home that can filter to its idle
  // pilots, it does that and brings the card into view instead.
  function initFleetIdleLink(link) {
    if (!once(link, "fleetidle")) return;
    link.addEventListener("click", function (ev) {
      var boxes = document.querySelectorAll(".grid.home-grid > .card[data-widget='fleet'] .fleet-box");
      for (var i = 0; i < boxes.length; i++) {
        if (!boxes[i]._fleetShowIdle || !boxes[i].getClientRects().length) continue;
        ev.preventDefault();
        boxes[i]._fleetShowIdle();
        boxes[i].closest(".card").scrollIntoView({ block: "start", behavior: "smooth" });
        return;
      }
    });
  }
  function initFleetFilter(root) {
    var boxes = Array.prototype.slice.call(root.querySelectorAll(".fleet-box"));
    if (root.matches && root.matches(".fleet-box")) boxes.push(root);
    boxes.forEach(initFleetBox);
    Array.prototype.forEach.call(root.querySelectorAll("[data-fleet-idle]"), initFleetIdleLink);
  }
  onContentReady(initFleetFilter);
  initFleetFilter(document);

  // --- Briefing severity tabs ------------------------------------
  // All / Critical / Warning / Info swap which cards show; a deck
  // whose cards are all filtered out hides with them.
  function initBriefTabs(root) {
    var tabs = root.querySelectorAll("[data-brief]");
    if (!tabs.length || !once(tabs[0], "tabs")) return;
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
      if (!once(buttons[i], "flip")) continue;
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
        if (!once(deck, "deck")) return;
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
      if (!once(openers[i], "dismissed")) continue;
      openers[i].addEventListener("click", function () {
        var modal = document.getElementById(this.getAttribute("data-open-brief-dismissed"));
        if (modal && window.EveSynapseModal) EveSynapseModal.open(modal);
      });
    }
  }
  onContentReady(initBriefDismissed);
  initBriefDismissed(document);

  // --- Widget settings (orders scope picker) ------------------
  // The size picker's outside-click close binds once and reads the
  // current grid each time; binding it per grid would pile a dead
  // handler behind every swapped customize view.
  var homePickCloseBound = false;

  // Card control glyphs: one stroke family, drawn at one size and
  // centered in identical square buttons.
  var CARD_ICONS = {
    move: '<path d="M12 3v18M3 12h18"/><path d="m9 6 3-3 3 3M9 18l3 3 3-3M6 9l-3 3 3 3M18 9l3 3-3 3"/>',
    size: '<path d="M14 4h6v6M10 20H4v-6M20 4l-6.5 6.5M4 20l6.5-6.5"/>',
    remove: '<path d="M6 6l12 12M18 6 6 18"/>',
    // The site's settings cog (ico-settings in base.html), in the
    // card buttons' own stroke.
    settings: '<circle cx="12" cy="12" r="3.2"/><path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 1 1-4 0v-.09a1.65 1.65 0 0 0-1-1.51 1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.65 1.65 0 0 0 .33-1.82 1.65 1.65 0 0 0-1.51-1H3a2 2 0 1 1 0-4h.09a1.65 1.65 0 0 0 1.51-1 1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.65 1.65 0 0 0 1.82.33 1.65 1.65 0 0 0 1-1.51V3a2 2 0 1 1 4 0v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.65 1.65 0 0 0-.33 1.82 1.65 1.65 0 0 0 1.51 1H21a2 2 0 1 1 0 4h-.09a1.65 1.65 0 0 0-1.51 1z"/>'
  };
  function cardIcon(name) {
    return '<svg class="cardicon" viewBox="0 0 24 24" aria-hidden="true" focusable="false">' +
      '<g fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round">' +
      CARD_ICONS[name] + "</g></svg>";
  }

  // --- Home grid: placement ----------------------------------------
  // Pure: the cells each widget takes. items are the widgets in page
  // order, each { w, h } in cells; cols is the grid's column count.
  // Each widget goes at the first spot where it fits at or after the
  // spot of the widget before it, scanning left to right then down,
  // the rule CSS row placement follows: widgets keep their order and
  // none moves back up into a hole an earlier one left (the hole
  // stays until the reader fills it). A width past the column count
  // draws as the column count. Returns { x, y, w, h } per item (x the
  // column, y the row, from 0).
  function homePack(items, cols) {
    cols = Math.max(1, cols | 0);
    var taken = [];
    var free = function (x, y, w, h) {
      for (var r = y; r < y + h; r++) {
        var row = taken[r];
        if (!row) continue;
        for (var c = x; c < x + w; c++) if (row[c]) return false;
      }
      return true;
    };
    var out = [];
    var cx = 0, cy = 0; // the cursor: just after the previous widget
    for (var i = 0; i < items.length; i++) {
      var w = Math.max(1, Math.min(cols, items[i].w | 0 || 1));
      var h = Math.max(1, items[i].h | 0 || 1);
      var y = cy, x = 0, from = cx, placed = false;
      while (!placed) {
        for (x = from; x + w <= cols; x++) {
          if (free(x, y, w, h)) { placed = true; break; }
        }
        if (!placed) { y++; from = 0; }
      }
      cx = x + w;
      cy = y;
      for (var r = y; r < y + h; r++) {
        taken[r] = taken[r] || [];
        for (var c = x; c < x + w; c++) taken[r][c] = true;
      }
      out.push({ x: x, y: y, w: w, h: h });
    }
    return out;
  }

  // homeGridCells maps placed widgets onto a rows-by-cols table of
  // the index standing in each cell (-1 for a hole). rows is the
  // grid's height: the lowest widget's bottom row.
  function homeGridCells(placed, cols) {
    var rows = 0, i, r, c;
    for (i = 0; i < placed.length; i++) rows = Math.max(rows, placed[i].y + placed[i].h);
    var cells = [];
    for (r = 0; r < rows; r++) {
      cells.push([]);
      for (c = 0; c < cols; c++) cells[r].push(-1);
    }
    for (i = 0; i < placed.length; i++) {
      var p = placed[i];
      for (r = p.y; r < p.y + p.h; r++) for (c = p.x; c < p.x + p.w; c++) cells[r][c] = i;
    }
    return cells;
  }

  // Pure: closes the holes homePack leaves. placed is homePack's
  // result with each item's allowed sizes added (sizes: [{ w, h }]).
  // A hole beside a widget is closed by widening that widget (the one
  // on its left first, then the one on its right), a hole under one by
  // lengthening it, and only to a size the widget allows; a widget
  // grows by as much of the hole as an allowed size covers. Holes no
  // allowed size can close stay. Returns new { x, y, w, h } per item.
  function homeFillGaps(placed, cols) {
    var out = placed.map(function (p) { return { x: p.x, y: p.y, w: p.w, h: p.h }; });
    var allows = function (i, w, h) {
      var sizes = placed[i].sizes || [];
      for (var k = 0; k < sizes.length; k++) if (sizes[k].w === w && sizes[k].h === h) return true;
      return false;
    };
    var clear = function (cells, x, y, w, h) {
      if (x < 0 || x + w > cols || y < 0 || y + h > cells.length) return false;
      for (var r = y; r < y + h; r++) for (var c = x; c < x + w; c++) if (cells[r][c] !== -1) return false;
      return true;
    };
    var grow = function (cells, r, c) {
      var run = 0, k, i, p;
      while (c + run < cols && cells[r][c + run] === -1) run++;
      // The widget on the hole's left widens rightwards.
      if (c > 0 && (i = cells[r][c - 1]) >= 0) {
        p = out[i];
        for (k = run; k >= 1; k--) {
          if (allows(i, p.w + k, p.h) && clear(cells, p.x + p.w, p.y, k, p.h)) { p.w += k; return true; }
        }
      }
      // The widget on its right widens leftwards.
      if (c + run < cols && (i = cells[r][c + run]) >= 0) {
        p = out[i];
        for (k = run; k >= 1; k--) {
          if (allows(i, p.w + k, p.h) && clear(cells, p.x - k, p.y, k, p.h)) { p.x -= k; p.w += k; return true; }
        }
      }
      // The widget above it lengthens downwards.
      if (r > 0 && (i = cells[r - 1][c]) >= 0) {
        p = out[i];
        var down = 0;
        while (r + down < cells.length && clear(cells, p.x, r + down, p.w, 1)) down++;
        for (k = down; k >= 1; k--) {
          if (allows(i, p.w, p.h + k)) { p.h += k; return true; }
        }
      }
      return false;
    };
    var changed = true;
    while (changed) {
      changed = false;
      var cells = homeGridCells(out, cols);
      for (var r = 0; r < cells.length && !changed; r++) {
        for (var c = 0; c < cols && !changed; c++) {
          if (cells[r][c] === -1 && grow(cells, r, c)) changed = true;
        }
      }
    }
    return out;
  }

  // homeHoles lists the empty runs of cells inside the grid, row by
  // row: { x, y, w } each. Customize shows them as Add a widget cells.
  function homeHoles(placed, cols) {
    var cells = homeGridCells(placed, cols), out = [];
    for (var r = 0; r < cells.length; r++) {
      for (var c = 0; c < cols; c++) {
        if (cells[r][c] !== -1) continue;
        var w = 1;
        while (c + w < cols && cells[r][c + w] === -1) w++;
        out.push({ x: c, y: r, w: w });
        c += w - 1;
      }
    }
    return out;
  }

  // Pure: how many rows of a list fit. bottoms are the rows' bottom
  // edges in order, limit the lowest edge a row may reach. Rows count
  // from the first until one passes the limit; half a pixel of
  // rounding is forgiven.
  function homeFitCount(bottoms, limit) {
    var n = 0;
    while (n < bottoms.length && bottoms[n] <= limit + 0.5) n++;
    return n;
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports.homePack = homePack;
    module.exports.homeFillGaps = homeFillGaps;
    module.exports.homeFitCount = homeFitCount;
  }

  // homeParseSizes reads a card's data-sizes ("1x1 2x1 2x2") into
  // the sizes the widget declares, narrowest then shortest first.
  function homeParseSizes(attr) {
    var out = [];
    (attr || "").split(/\s+/).forEach(function (raw) {
      var m = /^(\d)x(\d)$/.exec(raw);
      if (m) out.push({ w: +m[1], h: +m[2] });
    });
    out.sort(function (a, b) { return a.w - b.w || a.h - b.h; });
    return out;
  }

  // homeSnapToAllowed is the declared size a wanted size lands on,
  // by the server's rule (homeWidgetDef.Snap): the size itself when
  // declared; else the same width at the nearest height; else the
  // smallest declared size that covers it; else the nearest declared
  // size. Ties go to the smaller size.
  function homeSnapToAllowed(sizes, w, h) {
    if (!sizes.length) return { w: w, h: h };
    var i, c, best = null;
    for (i = 0; i < sizes.length; i++) {
      if (sizes[i].w === w && sizes[i].h === h) return sizes[i];
    }
    for (i = 0; i < sizes.length; i++) {
      c = sizes[i];
      if (c.w !== w) continue;
      if (!best || Math.abs(c.h - h) < Math.abs(best.h - h)) best = c;
    }
    if (best) return best;
    for (i = 0; i < sizes.length; i++) {
      c = sizes[i];
      if (c.w < w || c.h < h) continue;
      if (!best || c.w * c.h < best.w * best.h || (c.w * c.h === best.w * best.h && c.w < best.w)) best = c;
    }
    if (best) return best;
    var dist = function (s) { return Math.abs(s.w - w) + Math.abs(s.h - h); };
    best = sizes[0];
    for (i = 1; i < sizes.length; i++) {
      c = sizes[i];
      if (dist(c) < dist(best) || (dist(c) === dist(best) && c.w * c.h < best.w * best.h)) best = c;
    }
    return best;
  }

  if (typeof module !== "undefined" && module.exports) {
    module.exports.homeParseSizes = homeParseSizes;
    module.exports.homeSnapToAllowed = homeSnapToAllowed;
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

  // --- Home grid: layout ------------------------------------------
  // home.css sets the columns from the page's width (--home-cols).
  // This writes each card's cells on it through the CSSOM
  // (grid-column, grid-row): homePack's spots. On the three columns
  // Customize draws, every card stands at the size its reader chose and
  // a hole stays a hole (Customize shows it as an Add a widget cell;
  // Fill gaps closes it on request). On a narrower grid, which the
  // layout was not made for, homeFillGaps closes the holes for the
  // reader, as live's two-column fill did. With one column the cards stack and
  // nothing is written. It runs on load, on content-ready, when the grid
  // changes width, and inside every Customize change (flip calls it,
  // so the motion plays to the new places).
  // homeDesignCols is the column count layouts are made on (Customize
  // and the server's presets, homeGridCols in Go).
  var homeDesignCols = 3;
  function homeCols(grid) {
    var n = parseInt(window.getComputedStyle(grid).getPropertyValue("--home-cols"), 10);
    return n >= 1 ? n : 1;
  }
  function homeLayout(grid) {
    var cols = homeCols(grid);
    var custom = grid.classList.contains("home-custom");
    var cards = [], items = [], i;
    for (i = 0; i < grid.children.length; i++) {
      var c = grid.children[i];
      if (c.classList.contains("card") && !c.classList.contains("dragging")) cards.push(c);
    }
    var old = grid.querySelectorAll(":scope > .w-hole");
    for (i = 0; i < old.length; i++) old[i].remove();
    grid._homeCols = cols;
    if (cols < 2) {
      for (i = 0; i < cards.length; i++) {
        cards[i].style.gridColumn = "";
        cards[i].style.gridRow = "";
        cards[i]._homeSpan = null;
      }
      return;
    }
    for (i = 0; i < cards.length; i++) {
      var w = parseInt(cards[i].getAttribute("data-gw"), 10) || 1;
      var h = parseInt(cards[i].getAttribute("data-gh"), 10) || 1;
      // The sizes the widget allows, as this grid draws them: a width
      // past the column count draws as the count.
      var drawn = [];
      homeParseSizes(cards[i].getAttribute("data-sizes")).forEach(function (sz) {
        drawn.push({ w: Math.min(sz.w, cols), h: sz.h });
      });
      items.push({ w: Math.min(w, cols), h: h, sizes: drawn });
    }
    var placed = homePack(items, cols);
    if (!custom && cols < homeDesignCols) {
      for (i = 0; i < placed.length; i++) placed[i].sizes = items[i].sizes;
      placed = homeFillGaps(placed, cols);
    }
    for (i = 0; i < cards.length; i++) {
      var p = placed[i];
      var col = (p.x + 1) + " / span " + p.w, row = (p.y + 1) + " / span " + p.h;
      if (cards[i].style.gridColumn !== col) cards[i].style.gridColumn = col;
      if (cards[i].style.gridRow !== row) cards[i].style.gridRow = row;
      cards[i]._homeSpan = p;
    }
    if (!custom) return;
    homeHoles(placed, cols).forEach(function (hole) {
      var a = document.createElement("a");
      a.className = "w-hole";
      a.href = "/?customize=1&add=1";
      a.textContent = "Add a widget";
      a.style.gridColumn = (hole.x + 1) + " / span " + hole.w;
      a.style.gridRow = String(hole.y + 1);
      a.addEventListener("click", function (ev) {
        var add = document.getElementById("add-module-btn");
        if (!add) return;
        ev.preventDefault();
        add.click();
      });
      grid.appendChild(a);
    });
  }

  // --- Home grid: lists that fit ----------------------------------
  // In fixed rows a card is exactly its cells, so a list in it
  // ([data-w-fit], whose children are its rows) shows the whole rows
  // that fit and hides the rest, and the line after it says how many
  // more there are, linking to the widget's page (the card's
  // data-href, named by data-href-name: "3 more on the Jobs page").
  // Where cards take their content's height (auto rows, one column)
  // the server's cap holds instead (rows marked data-w-over), with
  // the same line.
  // homeListBox is the block a list stands in: a table body's table
  // (and its scroller), else the list itself.
  function homeListBox(list) {
    return list.tagName === "TBODY" ? (list.closest(".tw") || list.closest("table")) : list;
  }
  function homeMoreLine(list, card) {
    var anchor = homeListBox(list);
    var line = anchor.nextElementSibling;
    if (line && line.classList.contains("w-more")) return line;
    line = document.createElement("p");
    line.className = "w-more";
    line.hidden = true;
    var small = document.createElement("small");
    var href = card.getAttribute("data-href");
    var text = href ? document.createElement("a") : document.createElement("span");
    if (href) text.href = href;
    text.setAttribute("data-page", card.getAttribute("data-href-name") || "");
    small.appendChild(text);
    line.appendChild(small);
    anchor.insertAdjacentElement("afterend", line);
    return line;
  }
  function homeSetMore(line, n) {
    var text = line.querySelector("a, span");
    var page = text.getAttribute("data-page");
    text.textContent = n + " more" + (page && text.tagName === "A" ? " on the " + page + " page" : "");
    line.hidden = n <= 0;
  }
  // homeCut hides a row the card has no room for, or shows it again.
  // The row takes the hidden attribute as well as .w-cut: a table drawn
  // as cards (cards-on) styles its rows by id and spares only hidden
  // ones.
  function homeCut(row, on) {
    if (on) {
      row.classList.add("w-cut");
      row.hidden = true;
    } else if (row.classList.contains("w-cut")) {
      row.classList.remove("w-cut");
      row.hidden = false;
    }
  }
  function homeOverflows(body) {
    return body.scrollHeight - body.clientHeight > 1;
  }
  // homeFitLists gives every list its rows back, then takes rows off
  // until the body fits. One list gives up its last rows at once.
  // With several, the row lowest on the card goes first: stacked
  // lists give up rows the last list first (what a card leads with
  // stays), and lists side by side give up rows in turn. A list
  // inside a part that is not drawn counts all its rows as more.
  function homeFitLists(body, lists, card) {
    var l, k;
    for (l = 0; l < lists.length; l++) {
      var kids = lists[l].children;
      if (homeListBox(lists[l]) !== lists[l]) homeCut(homeListBox(lists[l]), false);
      for (k = 0; k < kids.length; k++) homeCut(kids[k], false);
      homeSetMore(homeMoreLine(lists[l], card), lists[l].getClientRects().length ? 0 : kids.length);
    }
    if (!homeOverflows(body)) return;
    var live = Array.prototype.filter.call(lists, function (list) {
      return list.getClientRects().length && list.children.length;
    });
    var rows = live.map(function (list) { return Array.prototype.slice.call(list.children); });
    var lines = live.map(function (list) { return homeMoreLine(list, card); });
    var shown = rows.map(function (r) { return r.length; });
    if (live.length === 1) {
      homeSetMore(lines[0], rows[0].length);
      var over = body.scrollHeight - body.clientHeight;
      var bottoms = rows[0].map(function (r) { return r.getBoundingClientRect().bottom; });
      shown[0] = homeFitCount(bottoms, bottoms[bottoms.length - 1] - over);
      for (k = shown[0]; k < rows[0].length; k++) homeCut(rows[0][k], true);
      homeSetMore(lines[0], rows[0].length - shown[0]);
    }
    // A table can reflow once rows go: keep taking rows while the
    // card still overflows.
    while (homeOverflows(body)) {
      var pick = -1, low = -Infinity;
      for (l = 0; l < live.length; l++) {
        if (!shown[l]) continue;
        var bottom = rows[l][shown[l] - 1].getBoundingClientRect().bottom;
        if (bottom >= low) { low = bottom; pick = l; }
      }
      if (pick < 0) break;
      shown[pick]--;
      homeCut(rows[pick][shown[pick]], true);
      homeSetMore(lines[pick], rows[pick].length - shown[pick]);
    }
    // A table with no row left goes too: a header over nothing says
    // nothing the line after it does not.
    for (l = 0; l < live.length; l++) {
      if (!shown[l] && homeListBox(live[l]) !== live[l]) homeCut(homeListBox(live[l]), true);
    }
  }
  // homeFitCard fits one card's body to its box. fit is false where
  // cards take their content's height, and the server's cap holds
  // (its rows past the cap are cut the same way). Lists give up rows
  // first; when the card still overflows, its parts after the first
  // are not drawn, the last first, until it fits (.w-drop): a card
  // shows the whole parts that fit, never a part cut in half. A kit
  // body (.w-body) is the box its parts fit, the one drawn when the
  // card has a phone body beside it.
  function homeFitCard(card, fit) {
    var body = card.querySelector(":scope > .card-body");
    if (!body) return;
    var kits = body.querySelectorAll(":scope > .w-body");
    for (var b = 0; b < kits.length; b++) {
      if (kits[b].getClientRects().length) { body = kits[b]; break; }
    }
    var lists = body.querySelectorAll("[data-w-fit]");
    var i, k;
    var dropped = body.querySelectorAll(":scope > .w-drop");
    for (i = 0; i < dropped.length; i++) dropped[i].classList.remove("w-drop");
    for (i = 0; i < lists.length; i++) lists[i].classList.toggle("w-fitting", fit);
    if (!fit) {
      for (i = 0; i < lists.length; i++) {
        var kids = lists[i].children, capped = 0;
        if (homeListBox(lists[i]) !== lists[i]) homeCut(homeListBox(lists[i]), false);
        for (k = 0; k < kids.length; k++) {
          var capRow = kids[k].hasAttribute("data-w-over");
          homeCut(kids[k], false);
          if (capRow) {
            homeCut(kids[k], true);
            capped++;
          }
        }
        homeSetMore(homeMoreLine(lists[i], card), capped);
      }
      return;
    }
    // The parts in the body's flow, taken before the lists give up
    // rows (a list that gives up all of them is still the first
    // part); a part set beside it (the Briefing's filters, placed in
    // the heading band) takes no room.
    var parts = Array.prototype.filter.call(body.children, function (el) {
      if (el.classList.contains("w-more") || !el.getClientRects().length) return false;
      var pos = window.getComputedStyle(el).position;
      return pos !== "absolute" && pos !== "fixed";
    });
    homeFitLists(body, lists, card);
    if (!homeOverflows(body)) return;
    for (i = parts.length - 1; i >= 1 && homeOverflows(body); i--) {
      parts[i].classList.add("w-drop");
      homeFitLists(body, lists, card);
    }
  }
  function homeFit(grid) {
    var fit = homeCols(grid) >= 2;
    var cards = grid.querySelectorAll(":scope > .card[data-widget]");
    for (var i = 0; i < cards.length; i++) homeFitCard(cards[i], fit);
  }

  function initHomeLayout(root) {
    var grids = root.querySelectorAll ? Array.prototype.slice.call(root.querySelectorAll(".grid.home-grid")) : [];
    if (!grids.length && root.closest) {
      var own = root.closest(".grid.home-grid");
      if (own) grids.push(own);
    }
    grids.forEach(function (grid) {
      homeLayout(grid);
      homeFit(grid);
      if (grid._homeRO || !window.ResizeObserver) return;
      // A new width can change the column count and, in fixed rows,
      // the row height: place and fit again. Observers on swapped-out
      // grids die with their nodes.
      grid._homeRO = new ResizeObserver(function () {
        if (grid.querySelector(":scope > .card.resizing, :scope > .card.dragging")) return;
        if (homeCols(grid) !== grid._homeCols) homeLayout(grid);
        homeFit(grid);
      });
      grid._homeRO.observe(grid);
    });
  }
  onContentReady(initHomeLayout);
  initHomeLayout(document);
  if (document.fonts && document.fonts.ready) {
    document.fonts.ready.then(function () {
      var grids = document.querySelectorAll(".grid.home-grid");
      for (var i = 0; i < grids.length; i++) homeFit(grids[i]);
    });
  }

  // --- Home grid: Expand ---------------------------------------------
  // Each card's heading carries an Expand button, rendered hidden and
  // shown here: it opens the widget's largest form in the shared
  // modal, fetched from the one-card route (view=expand) into a box
  // the form fills. Without JavaScript the card's title links to its
  // page, which has all of it.
  function initHomeExpand(root) {
    var btns = root.querySelectorAll ? root.querySelectorAll("[data-w-expand][hidden]") : [];
    for (var i = 0; i < btns.length; i++) btns[i].hidden = false;
  }
  onContentReady(initHomeExpand);
  initHomeExpand(document);

  var homeExpandSeq = 0;
  function homeExpandNote(box, text, href) {
    var p = document.createElement("p");
    p.className = "w-expand-note";
    p.textContent = text;
    if (href) {
      var a = document.createElement("a");
      a.href = href;
      a.textContent = "Open its page";
      p.appendChild(document.createTextNode(" "));
      p.appendChild(a);
    }
    box.replaceChildren(p);
  }
  function homeExpand(card, modal) {
    var key = card.getAttribute("data-key");
    var href = card.getAttribute("data-href");
    var heading = card.querySelector(":scope > h3");
    var name = heading ? (heading.querySelector(".w-title") || heading).textContent.trim() : key;
    var title = modal.querySelector("[data-w-expand-title]");
    var box = modal.querySelector("[data-w-expand-box]");
    var seq = ++homeExpandSeq;
    title.replaceChildren();
    if (href) {
      var link = document.createElement("a");
      link.href = href;
      link.textContent = name;
      title.appendChild(link);
    } else {
      title.textContent = name;
    }
    box.setAttribute("aria-busy", "true");
    homeExpandNote(box, "Loading " + name + ".");
    window.EveSynapseModal.open(modal, {
      onClose: function () {
        homeExpandSeq++;
        box.replaceChildren();
      }
    });
    fetch("/home/widget/" + encodeURIComponent(key) + "?view=expand" + homeSince("&"), {
      headers: { "X-Requested-With": "XMLHttpRequest" },
      credentials: "same-origin",
      cache: "no-store"
    }).then(function (resp) {
      if (resp.status === 204) return null;
      if (!resp.ok) throw new Error("card " + resp.status);
      return resp.text();
    }).then(function (html) {
      if (seq !== homeExpandSeq) return;
      box.removeAttribute("aria-busy");
      var tpl = document.createElement("template");
      tpl.innerHTML = (html || "").trim();
      var fresh = tpl.content.querySelector(".card[data-widget]");
      if (!fresh) {
        homeExpandNote(box, name + " has nothing to show right now.", href);
        return;
      }
      fresh = document.importNode(fresh, true);
      box.replaceChildren(fresh);
      document.dispatchEvent(new CustomEvent("evesynapse:content-ready", { detail: { root: fresh, part: true } }));
    }).catch(function () {
      if (seq !== homeExpandSeq) return;
      box.removeAttribute("aria-busy");
      homeExpandNote(box, name + " could not be loaded.", href);
    });
  }
  document.addEventListener("click", function (ev) {
    var btn = ev.target && ev.target.closest ? ev.target.closest("[data-w-expand]") : null;
    if (!btn) return;
    var card = btn.closest(".card[data-widget]");
    var modal = document.getElementById("w-expand-modal");
    if (!card || !modal || !window.EveSynapseModal || !window.fetch) return;
    ev.preventDefault();
    homeExpand(card, modal);
  });

  // The grid's most columns and rows (wSize in home_widget_kit.go).
  var HOME_COLS = 3, HOME_ROWS = 4;

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
    var homeGrid = root.querySelector("#home-grid") || root.querySelector(".grid.home-grid[data-home-page]");
    if (!homeGrid || !window.fetch) return;
    if (homeGrid.getAttribute("data-customize") === "1") {
      if (once(homeGrid, "customize")) initHomeCustomize(homeGrid);
    } else if (once(homeGrid, "refresh")) {
      initHomeRefresh(homeGrid);
    }
  }

  // --- Home grid: live refresh -----------------------------------
  // Outside Customize the cards redraw from the server every few
  // minutes while the page is in view (GET /home/widgets, every card
  // as it renders now), so countdowns, lists and figures follow the
  // worker without a reload. A card swaps only when its markup
  // changed; a card whose size or key no longer matches the page is
  // left for the next full load. Nothing refreshes while an Expand or
  // other dialog is open, or once the grid has left the page.
  var HOME_REFRESH_MS = 5 * 60 * 1000;

  // homeSince is the query part that keeps a fetched card's Briefing
  // on the window the page was drawn with ("" without a Briefing): the
  // page's own render moved the stored anchor, so without it the card
  // would lose the lines the reader is looking at.
  function homeSince(sep) {
    var el = document.querySelector("[data-briefing-since]");
    var v = el ? el.getAttribute("data-briefing-since") : "";
    return v ? sep + "since=" + encodeURIComponent(v) : "";
  }
  function initHomeRefresh(grid) {
    var last = Date.now(), busy = false, timer = 0;
    function due() {
      return document.visibilityState === "visible" && document.contains(grid) &&
        !document.querySelector(".modal-backdrop:not([hidden]), dialog[open]") && Date.now() - last >= HOME_REFRESH_MS;
    }
    function refresh() {
      if (busy || !due()) return;
      busy = true;
      last = Date.now();
      fetch("/home/widgets" + homeSince("?"), { headers: { "X-Requested-With": "XMLHttpRequest" }, credentials: "same-origin", cache: "no-store" })
        .then(function (resp) {
          if (!resp.ok) throw new Error("widgets " + resp.status);
          return resp.text();
        })
        .then(function (html) {
          if (!document.contains(grid)) return;
          var box = document.createElement("template");
          box.innerHTML = html.trim();
          var fresh = box.content.querySelectorAll(".card[data-key]");
          var swapped = false;
          for (var i = 0; i < fresh.length; i++) {
            var key = fresh[i].getAttribute("data-key");
            var live = grid.querySelector(':scope > .card[data-key="' + CSS.escape(key) + '"]');
            if (!live || live.getAttribute("data-gw") !== fresh[i].getAttribute("data-gw") ||
                live.getAttribute("data-gh") !== fresh[i].getAttribute("data-gh")) continue;
            if (live._homeSrc === fresh[i].innerHTML) continue;
            var node = document.importNode(fresh[i], true);
            node._homeSrc = fresh[i].innerHTML;
            node.style.gridColumn = live.style.gridColumn;
            node.style.gridRow = live.style.gridRow;
            live.replaceWith(node);
            document.dispatchEvent(new CustomEvent("evesynapse:content-ready", { detail: { root: node, part: true } }));
            swapped = true;
          }
          if (swapped) { homeLayout(grid); homeFit(grid); }
        })
        .catch(function () {})
        .then(function () { busy = false; });
    }
    timer = window.setInterval(function () {
      if (!document.contains(grid)) { window.clearInterval(timer); return; }
      refresh();
    }, 30 * 1000);
    document.addEventListener("visibilitychange", refresh);
  }
  onContentReady(initHomeGrid);
  initHomeGrid(document);

  // HOME_MAX_COPIES mirrors homeMaxCopies (home_widget_kit.go): the
  // most cards one widget may have on a home.
  var HOME_MAX_COPIES = 4;

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

    // postLayoutJSON is postLayout for a change whose answer carries
    // data (a copy's new key): the parsed reply, or null on failure.
    function postLayoutJSON(params) {
      var body = new URLSearchParams();
      for (var key in params) body.set(key, params[key]);
      return fetch("/home/layout", {
        method: "POST",
        headers: { "X-Requested-With": "XMLHttpRequest" },
        body: body
      }).then(function (resp) {
        if (!resp.ok) return null;
        document.dispatchEvent(new CustomEvent("evesynapse:content-mutated"));
        return resp.json();
      }).catch(function () { return null; });
    }

    // Undo a remove: one line above the grid names the card just
    // removed and offers it back for a while. Undo puts it back at
    // its old place and size, drawn fresh by the one-card route.
    var undoBar = null, undoTimer = 0;
    function offerUndo(name, params) {
      if (undoBar) undoBar.remove();
      window.clearTimeout(undoTimer);
      undoBar = document.createElement("p");
      undoBar.className = "w-undo";
      undoBar.appendChild(document.createTextNode("Removed " + (name || "the widget") + ". "));
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "btn btn-quiet";
      btn.textContent = "Undo";
      undoBar.appendChild(btn);
      grid.parentNode.insertBefore(undoBar, grid);
      var bar = undoBar;
      var close = function () {
        if (bar.parentNode) bar.remove();
        if (undoBar === bar) undoBar = null;
      };
      undoTimer = window.setTimeout(close, 15000);
      if (statusEl) statusEl.textContent = "Removed " + (name || "the widget") + ". Undo is above the grid.";
      btn.addEventListener("click", function () {
        btn.disabled = true;
        postLayout(params).then(function (ok) {
          if (!ok) { btn.disabled = false; return; }
          close();
          fetchCard(params.key).then(function (node) {
            if (!node) { window.location.reload(); return; }
            var cards = moduleCards();
            var before = cards[params.at] || null;
            flip(function () { grid.insertBefore(node, before); });
            placeCard(node);
            if (statusEl) statusEl.textContent = "Put back " + (name || "the widget") + ".";
            var h = node.querySelector(".draghandle");
            if (h) h.focus({ preventScroll: true });
          }, function () { window.location.reload(); });
        });
      });
    }

    // Fill gaps: give each card beside or above a hole the size that
    // closes it, as the home does outside Customize, and save those
    // sizes. Only a card that grows into a size it declares changes.
    var fillBtn = document.querySelector("[data-home-fill]");
    if (fillBtn && once(fillBtn, "fill")) {
      fillBtn.hidden = false;
      fillBtn.addEventListener("click", function () {
        var cols = homeCols(grid);
        if (cols < 2) return;
        var cards = Array.prototype.slice.call(moduleCards());
        var items = cards.map(function (c) {
          var s = sizeOf(c);
          return { w: Math.min(s.w, cols), h: s.h, sizes: sizesOf(c).map(function (z) { return { w: Math.min(z.w, cols), h: z.h }; }) };
        });
        var placed = homePack(items, cols);
        for (var i = 0; i < placed.length; i++) placed[i].sizes = items[i].sizes;
        placed = homeFillGaps(placed, cols);
        var pairs = [], grown = [];
        for (var j = 0; j < cards.length; j++) {
          var p = placed[j], s = sizeOf(cards[j]);
          if ((p.w > Math.min(s.w, cols) || p.h > s.h) && allows(cards[j], p.w, p.h)) {
            pairs.push(keyOf(cards[j]) + ":" + p.w + "x" + p.h);
            grown.push({ card: cards[j], w: p.w, h: p.h });
          }
        }
        if (!pairs.length) {
          if (statusEl) statusEl.textContent = "No gaps to fill.";
          return;
        }
        fillBtn.disabled = true;
        postLayout({ action: "sizes", sizes: pairs.join(",") }).then(function (ok) {
          fillBtn.disabled = false;
          if (!ok) return;
          flip(function () { grown.forEach(function (g) { sizeCard(g.card, g.w, g.h); }); });
          grown.forEach(function (g) { refreshCard(g.card); });
          if (statusEl) statusEl.textContent = "Filled the gaps: " + grown.length + " widget" + (grown.length === 1 ? "" : "s") + " grew.";
        });
      });
    }

    // fetchCard asks the one-card route for a widget's card as it
    // stands in this Customize view: the element, null when the
    // widget has nothing to show (204), or a rejection.
    function fetchCard(key) {
      return fetch("/home/widget/" + encodeURIComponent(key) + "?customize=1" + homeSince("&"), {
        headers: { "X-Requested-With": "XMLHttpRequest" },
        credentials: "same-origin",
        cache: "no-store"
      }).then(function (resp) {
        if (resp.status === 204) return null;
        if (!resp.ok) throw new Error("card " + resp.status);
        return resp.text().then(function (html) {
          var box = document.createElement("template");
          box.innerHTML = html.trim();
          var fresh = box.content.querySelector(".card[data-widget]");
          if (!fresh) throw new Error("no card");
          return document.importNode(fresh, true);
        });
      });
    }

    // placeCard enhances a card fresh from the server and runs the
    // page's initializers over it alone, as a swap does for a page.
    function placeCard(fresh) {
      enhanceCard(fresh);
      document.dispatchEvent(new CustomEvent("evesynapse:content-ready", { detail: { root: fresh, part: true } }));
    }

    // A size decides more than a card's outline: a widget draws a
    // different form at a different size. Once a size save lands,
    // the card is swapped for the server's render of it at the saved
    // size. The old render stays, dimmed, until the new one lands, so
    // nothing flashes or jumps. A render lands only on the card as it
    // stands: the live card for the key, not in the middle of a
    // resize, at the size the server drew. Otherwise a newer resize
    // is under way, and its own save (or settle, when it ends where
    // it began) brings the card up to date. Focus on one of the
    // card's controls moves to the same control on the new card, so a
    // keyboard resize carries on.
    var refreshSeq = {};
    function refreshCard(card) {
      var key = card.getAttribute("data-key") || card.getAttribute("data-widget");
      var seq = (refreshSeq[key] || 0) + 1;
      refreshSeq[key] = seq;
      card.classList.add("w-stale");
      return fetchCard(key).then(function (fresh) {
        if (refreshSeq[key] !== seq) return;
        var live = card.parentNode ? card : grid.querySelector(':scope > .card[data-key="' + CSS.escape(key) + '"]');
        if (!live || cardResizing(live)) return;
        if (!fresh) { removeCard(live); return; }
        if (live.getAttribute("data-gw") !== fresh.getAttribute("data-gw") ||
            live.getAttribute("data-gh") !== fresh.getAttribute("data-gh")) return;
        var control = focusedControl(live);
        flip(function () { live.replaceWith(fresh); }, fresh);
        placeCard(fresh);
        var again = control && fresh.querySelector(control);
        if (again) again.focus({ preventScroll: true });
      }, function () {
        if (refreshSeq[key] === seq) card.classList.remove("w-stale");
      });
    }
    // cardResizing: a grip drag or a run of arrow keys is under way.
    function cardResizing(card) {
      return card.classList.contains("resizing") || !!card._keyFrom;
    }
    // settle brings a card a resize left on an old render up to date;
    // a resize that ends without a save calls it.
    function settle(card) {
      if (card.parentNode && card.classList.contains("w-stale")) refreshCard(card);
    }
    // focusedControl names the card control that has focus, so the
    // same control on the card's new render can take it.
    var cardControls = [".cardgrip", ".cardsize", ".draghandle", ".cardsettings"];
    function focusedControl(card) {
      var a = document.activeElement;
      if (!a || !card.contains(a)) return "";
      for (var i = 0; i < cardControls.length; i++) {
        if (a.matches(cardControls[i])) return cardControls[i];
      }
      return "";
    }

    // Settings: the gear opens the card's own sheet (a plain form the
    // server rendered into the card) in the shared modal. Saving posts
    // it as the form would, then swaps in the card as the server now
    // draws it; a failed save keeps the sheet open and says so.
    var settingsModal = document.getElementById("w-settings-modal");
    function openSettings(card) {
      var tpl = card.querySelector(":scope > template[data-w-settings]");
      if (!tpl || !settingsModal || !window.EveSynapseModal) return;
      var box = settingsModal.querySelector("[data-w-settings-box]");
      var heading = settingsModal.querySelector("[data-w-settings-title]");
      var title = tpl.getAttribute("data-title") || titleOf(card);
      heading.textContent = title + " settings";
      box.replaceChildren(document.importNode(tpl.content, true));
      var form = box.querySelector("form");
      // A multi with a limit: once that many are picked, the other
      // boxes rest until one is unticked (the server refuses more).
      form.querySelectorAll("fieldset[data-max]").forEach(function (set) {
        var max = parseInt(set.getAttribute("data-max"), 10) || 0;
        function cap() {
          var picked = set.querySelectorAll('input[type="checkbox"]:checked').length;
          set.querySelectorAll('input[type="checkbox"]').forEach(function (b) {
            b.disabled = max > 0 && !b.checked && picked >= max;
          });
        }
        set.addEventListener("change", cap);
        cap();
      });
      form.addEventListener("submit", function (ev) {
        ev.preventDefault();
        var btn = form.querySelector('button[type="submit"]');
        if (btn) btn.disabled = true;
        var note = form.querySelector(".w-set-error");
        if (note) note.remove();
        fetch(form.action, {
          method: "POST",
          headers: { "X-Requested-With": "XMLHttpRequest" },
          credentials: "same-origin",
          body: new URLSearchParams(new FormData(form))
        }).then(function (resp) {
          if (!resp.ok) throw new Error("settings " + resp.status);
          document.dispatchEvent(new CustomEvent("evesynapse:content-mutated"));
          window.EveSynapseModal.close(settingsModal);
          var key = card.getAttribute("data-key");
          var live = grid.querySelector(':scope > .card[data-key="' + key + '"]') || card;
          refreshCard(live).then(function () {
            // The gear that opened the sheet went with the old card:
            // focus returns to the new card's gear.
            var a = document.activeElement;
            if (a && a !== document.body && document.contains(a)) return;
            var fresh = grid.querySelector(':scope > .card[data-key="' + key + '"] .cardsettings');
            if (fresh) fresh.focus({ preventScroll: true });
          });
          if (statusEl) statusEl.textContent = title + ": settings saved.";
        }).catch(function () {
          if (btn) btn.disabled = false;
          var p = document.createElement("p");
          p.className = "w-set-error";
          p.setAttribute("role", "alert");
          p.textContent = "The settings could not be saved. Check your picks and try again.";
          form.appendChild(p);
        });
      });
      window.EveSynapseModal.open(settingsModal, { onClose: function () { box.replaceChildren(); } });
    }

    // One status line for the whole Customize view, set once per
    // change: "Net Worth: 2 by 2, medium."
    var statusEl = document.querySelector("[data-home-status]");
    function announce(card, w, h) {
      if (!statusEl) return;
      var title = titleOf(card);
      var form = (sizeNamesOf(card)[w + "x" + h] || {}).form;
      statusEl.textContent = title + ": " + w + " by " + h + (form ? ", " + form : "") + ".";
    }
    function titleOf(card) {
      var heading = card.querySelector(":scope > h2, :scope > h3");
      if (!heading) return "";
      var copy = heading.cloneNode(true);
      var ctl = copy.querySelectorAll("button");
      for (var i = 0; i < ctl.length; i++) ctl[i].remove();
      return copy.textContent.trim();
    }

    function moduleCards() {
      return grid.querySelectorAll(":scope > .card[data-widget]");
    }
    // keyOf names a card in the layout: its key ("id#2" for a copy).
    function keyOf(card) {
      return card.getAttribute("data-key") || card.getAttribute("data-widget");
    }
    function widgetKeys() {
      var keys = [];
      var cards = moduleCards();
      for (var i = 0; i < cards.length; i++) keys.push(keyOf(cards[i]));
      return keys;
    }
    // widgetCounts is how many cards each widget has on the home.
    function widgetCounts() {
      var n = {};
      var cards = moduleCards();
      for (var i = 0; i < cards.length; i++) {
        var id = cards[i].getAttribute("data-widget");
        n[id] = (n[id] || 0) + 1;
      }
      return n;
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
      homeLayout(grid);
      homeFit(grid);
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
    // A widget's size is its gw/gh classes (grid cells across and
    // down); the CSS lays the grid out from them. The sizes it may
    // take are the ones its declaration lists (data-sizes), and each
    // draws a named form (data-size-names).
    function sizeOf(el) {
      return {
        w: parseInt(el.getAttribute("data-gw"), 10) || 1,
        h: parseInt(el.getAttribute("data-gh"), 10) || 1
      };
    }
    function sizesOf(el) {
      return homeParseSizes(el.getAttribute("data-sizes"));
    }
    function allows(el, w, h) {
      return sizesOf(el).some(function (s) { return s.w === w && s.h === h; });
    }
    function sizeNamesOf(el) {
      try { return JSON.parse(el.getAttribute("data-size-names") || "{}"); } catch (err) { return {}; }
    }
    function sizeLine(el, w, h) {
      var says = (sizeNamesOf(el)[w + "x" + h] || {}).says;
      return w + " by " + h + (says ? ": " + says : "");
    }
    function sizeCard(el, w, h) {
      ["gw1", "gw2", "gw3", "gh1", "gh2", "gh3", "gh4"].forEach(function (c) { el.classList.remove(c); });
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
      announce(card, w, h);
      saveSize(card, w, h, was);
    }
    // saveSize stores a size the card already shows; a failed save
    // puts back the size it had before.
    function saveSize(card, w, h, was) {
      postLayout({ action: "size", key: keyOf(card), w: w, h: h }).then(function (ok) {
        if (ok) { refreshCard(card); return; }
        flip(function () { sizeCard(card, was.w, was.h); });
        announce(card, was.w, was.h);
        refuse(card);
        settle(card);
      });
    }
    function saveOrder() {
      return postLayout({ action: "order", ids: widgetKeys().join(",") });
    }

    // Give one card its move handle, resize toggle and × button.
    // Cards added from the pop-up pass through here too.
    function enhanceCard(card) {
      var heading = card.querySelector(":scope > h2, :scope > h3");
      if (!heading || card.querySelector(".draghandle")) return;
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
      // size picker (three cells across by four down, the way a
      // table size is picked; only the sizes the widget declares can
      // be picked, each named for the form it draws) and remove. The
      // grip in the bottom-right corner resizes by dragging or by the
      // arrow keys, landing on declared sizes.
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
      for (var ph = 1; ph <= HOME_ROWS; ph++) {
        for (var pw = 1; pw <= HOME_COLS; pw++) {
          (function (w, h) {
            var cell = document.createElement("button");
            cell.type = "button";
            cell.className = "sizecell";
            cell.setAttribute("data-gw", String(w));
            cell.setAttribute("data-gh", String(h));
            var line = sizeLine(card, w, h);
            cell.setAttribute("aria-label", line);
            cell.title = line;
            if (!allows(card, w, h)) {
              // Not a size this widget has a form for.
              cell.disabled = true;
              cell.classList.add("over");
              cell.setAttribute("aria-label", w + " by " + h + ": not offered");
              cell.title = "";
            }
            cell.addEventListener("mouseenter", function () { paintPicker(w, h); });
            cell.addEventListener("focus", function () { paintPicker(w, h); });
            cell.addEventListener("click", function () {
              if (!allows(card, w, h)) return;
              resizeCard(card, w, h);
              closePicker();
            });
            cells.push(cell);
            picker.appendChild(cell);
          })(pw, ph);
        }
      }
      function paintPicker(w, h) {
        cells.forEach(function (c) {
          var cw = +c.getAttribute("data-gw"), ch = +c.getAttribute("data-gh");
          c.classList.toggle("on", cw <= w && ch <= h);
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
          cells[(s.h - 1) * HOME_COLS + (s.w - 1)].focus();
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
        var at = Array.prototype.indexOf.call(moduleCards(), card);
        var was = sizeOf(card), key = keyOf(card), name = titleOf(card);
        postLayout({ action: "toggle", key: key }).then(function (ok) {
          if (!ok) { remove.disabled = false; return; }
          removeCard(card);
          offerUndo(name, { action: "restore", key: key, at: at, w: was.w, h: was.h });
        });
      });
      var settingsTpl = card.querySelector(":scope > template[data-w-settings]");
      var gear = null;
      if (settingsTpl) {
        gear = document.createElement("button");
        gear.type = "button";
        gear.className = "cardbtn cardsettings";
        gear.innerHTML = cardIcon("settings");
        gear.title = "Settings";
        gear.setAttribute("aria-haspopup", "dialog");
        gear.setAttribute("aria-label", (settingsTpl.getAttribute("data-title") || title) + " settings");
        gear.addEventListener("click", function () { openSettings(card); });
      }
      ctl.appendChild(sizeBtn);
      if (gear) ctl.appendChild(gear);
      ctl.appendChild(remove);
      ctl.appendChild(picker);
      card.appendChild(ctl);

      var grip = document.createElement("button");
      grip.type = "button";
      grip.className = "cardgrip";
      grip.title = "Drag to resize";
      grip.setAttribute("aria-label", "Resize " + title + " (drag, or use the arrow keys)");
      grip.addEventListener("pointerdown", function (ev) { startResize(ev, card, grip); });
      grip.addEventListener("keydown", function (ev) { keyResize(ev, card); });
      card.appendChild(grip);
    }

    // Keyboard resize: the arrow keys on a card's grip step through
    // the sizes it declares (right and down to the next, left and up
    // to the one before). Each step shows at once; the save waits
    // until the keys rest.
    function keyResize(ev, card) {
      var fwd = ev.key === "ArrowRight" || ev.key === "ArrowDown";
      var back = ev.key === "ArrowLeft" || ev.key === "ArrowUp";
      if (!fwd && !back) return;
      ev.preventDefault();
      var sizes = sizesOf(card), now = sizeOf(card), at = -1;
      for (var i = 0; i < sizes.length; i++) {
        if (sizes[i].w === now.w && sizes[i].h === now.h) at = i;
      }
      var next = sizes[at + (fwd ? 1 : -1)];
      if (!next) return;
      if (!card._keyFrom) card._keyFrom = now;
      flip(function () { sizeCard(card, next.w, next.h); });
      announce(card, next.w, next.h);
      clearTimeout(card._keySave);
      card._keySave = setTimeout(function () {
        var from = card._keyFrom;
        card._keyFrom = null;
        var to = sizeOf(card);
        if (from.w !== to.w || from.h !== to.h) saveSize(card, to.w, to.h, from);
        else settle(card);
      }, 400);
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
      var start = sizeOf(card), cur = start, sizes = sizesOf(card);
      var r0 = card.getBoundingClientRect();
      var gcs = window.getComputedStyle(grid);
      var gapX = parseFloat(gcs.columnGap) || 0, gapY = parseFloat(gcs.rowGap) || 0;
      var cols = homeCols(grid);
      // The column pitch is measured from the grid itself, never from
      // the card under the grip: a card grown to close a hole stands
      // wider than its stored size says. The row pitch is the card's
      // own rows, evenly: exactly one row and its gap where rows are
      // fixed, and the rows the card stands in where they follow
      // their content.
      var gridRect = grid.getBoundingClientRect();
      var cellW = (gridRect.width + gapX) / cols;
      var colStart = Math.max(0, Math.min(cols - 1, Math.round((r0.left - gridRect.left) / cellW)));
      var spanH = card._homeSpan ? card._homeSpan.h : start.h;
      var cellH = (r0.height + gapY) / spanH;
      if (!(cellH > 24)) cellH = 176 + gapY;
      // Only the sizes that fit from the column the card starts in.
      var fits = sizes.filter(function (s) { return s.w <= cols - colStart; });
      if (!fits.length) fits = sizes;
      var maxW = 1, maxH = 1;
      fits.forEach(function (s) { maxW = Math.max(maxW, s.w); maxH = Math.max(maxH, s.h); });
      var lastX = ev.clientX, lastY = ev.clientY;
      var startX = ev.clientX, startY = ev.clientY;

      var frame = document.createElement("div");
      frame.className = "resize-frame";
      var frameLabel = document.createElement("span");
      frameLabel.className = "resize-frame-label";
      frame.appendChild(frameLabel);
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
        var raw = homeSnapToAllowed(fits, snap((relX + gapX) / cellW, cur.w, maxW), snap((relY + gapY) / cellH, cur.h, maxH));
        var w = raw.w, h = raw.h;
        frameLabel.textContent = sizeLine(card, w, h);
        frame.style.left = r.left + "px";
        frame.style.top = r.top + "px";
        var fs = homeResizeFrame(r0.width, r0.height, lastX - startX, lastY - startY,
          maxW * cellW - gapX, maxH * cellH - gapY, cellW, cellH);
        frame.style.width = fs.w + "px";
        frame.style.height = fs.h + "px";
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
        cur = homeSnapToAllowed(fits,
          homeSnapCommit(start.w, lastX - startX, cellW, maxW),
          homeSnapCommit(start.h, lastY - startY, cellH, maxH));
        var now = sizeOf(card);
        if (now.w !== cur.w || now.h !== cur.h) {
          flip(function () { sizeCard(card, cur.w, cur.h); }, card);
        }
        if (cur.w === start.w && cur.h === start.h) { settle(card); return; }
        // The widget already stands at its new size; save it, and a
        // failed save puts back the size it started at.
        announce(card, cur.w, cur.h);
        saveSize(card, cur.w, cur.h, start);
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
      // The lifted card is fixed, and the page's size container
      // (.l-page) is its containing block, so it moves with the page
      // when the page scrolls. fixX and fixY carry it back under the
      // pointer (onScroll).
      var fixX = 0, fixY = 0;
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
        homeLayout(grid);
        measure();
        window.addEventListener("scroll", onScroll, { passive: true });
        raf = requestAnimationFrame(tick);
      }

      function onScroll() {
        if (!lifted) return;
        var r = card.getBoundingClientRect();
        fixX += origin.left + (lastX - startX) - r.left;
        fixY += origin.top + (lastY - startY) - r.top;
        follow();
      }

      // The card is placed on every pointer move, straight from the
      // event (a transform: no layout), so it never trails the
      // pointer by a frame.
      function follow() {
        card.style.transform = "translate3d(" + (lastX - startX + fixX) + "px," + (lastY - startY + fixY) + "px,0)";
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
        window.removeEventListener("scroll", onScroll);
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
        homeLayout(grid);
        homeFit(grid);
      }

      // Glide the lifted card onto the slot's cell, then let it take
      // the slot's place.
      function landThen(done) {
        var b = layout.get(slot);
        if (!canAnimate || !b) { done(); return; }
        var dx = b.l - window.scrollX - origin.left + fixX, dy = b.t - window.scrollY - origin.top + fixY;
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
      // A widget already on the home that allows copies gets another
      // card under a new key; any other add is the plain toggle.
      var copy = !!widgetCounts()[id];
      var saved = copy
        ? postLayoutJSON({ action: "copy", widget: id }).then(function (r) { return r && r.ok ? r.key : null; })
        : postLayout({ action: "toggle", widget: id }).then(function (ok) { return ok ? id : null; });
      saved.then(function (key) {
        if (!key) { btn.disabled = false; return; }
        // Widget markup is server-rendered: the new card comes from
        // the one-card route. If that fails, a reload lands in the
        // same place, the add already saved.
        fetchCard(key).then(function (node) {
          if (!node) { window.location.reload(); return; }
          // Never two cards for one key: if a stale one is somehow
          // still in the grid, it goes before the fresh one lands.
          var stale = grid.querySelector('.card[data-key="' + CSS.escape(key) + '"]');
          if (stale) stale.remove();
          window.EveSynapseModal.close(modal);
          flip(function () { grid.appendChild(node); });
          placeCard(node);
          var s = sizeOf(node);
          announce(node, s.w, s.h);
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
        var counts = widgetCounts();
        var available = 0;
        for (var j = 0; j < addItems.length; j++) {
          // A widget already on the home stays listed, marked; one
          // that allows copies stays open until it has the most.
          var n = counts[addItems[j].getAttribute("data-add-widget")] || 0;
          var multi = addItems[j].hasAttribute("data-add-multi");
          var full = multi ? n >= HOME_MAX_COPIES : n > 0;
          addItems[j].disabled = full;
          var note = addItems[j].querySelector("[data-added-note]");
          if (note) note.hidden = n === 0;
          var again = addItems[j].querySelector("[data-add-again]");
          if (again) again.hidden = !(multi && n > 0 && !full);
          if (!full) available++;
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
