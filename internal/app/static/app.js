// EveSynapse progressive enhancements. Everything here only ever
// *adds* behavior: with JavaScript disabled the pages render and
// work exactly as before (the nav is <details>-driven, the market
// form submits, panels are simply unfolded).
(function () {
  "use strict";

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

  var foldables = document.querySelectorAll(".foldable");
  for (var i = 0; i < foldables.length; i++) {
    // data-fold="never": keep the foldable section styling but
    // don't offer a fold button (Issue 17).
    if (foldables[i].getAttribute("data-fold") === "never") continue;
    addFold(foldables[i], foldables[i].querySelector(":scope > h2, :scope > h3"));
  }

  var cards = document.querySelectorAll(".card");
  for (var i = 0; i < cards.length; i++) {
    addFold(cards[i], cards[i].querySelector(":scope > h2, :scope > h3"));
  }

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
  var countdowns = document.querySelectorAll("[data-finish]");
  if (countdowns.length) {
    var fmtLeft = function (ms) {
      if (ms <= 0) return "done";
      var s = Math.floor(ms / 1000);
      var d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600),
          m = Math.floor((s % 3600) / 60), sec = s % 60;
      if (d > 0) return "in " + d + "d " + h + "h " + m + "m";
      if (h > 0) return "in " + h + "h " + m + "m";
      if (m > 0) return "in " + m + "m " + sec + "s";
      return "in " + sec + "s";
    };
    var tickCountdowns = function () {
      for (var i = 0; i < countdowns.length; i++) {
        var t = Date.parse(countdowns[i].getAttribute("data-finish"));
        if (!isNaN(t)) countdowns[i].textContent = fmtLeft(t - Date.now());
      }
    };
    tickCountdowns();
    setInterval(tickCountdowns, 1000);
  }

  // --- Fleet filter (home overview) ---------------------------
  // The fleet table renders every character server-side; this
  // just hides rows that don't match the typed text or the
  // active tag chip. No JS: the full list is simply visible.
  var fleetFilter = document.getElementById("fleet-filter");
  var fleetRows = document.querySelectorAll(".fleet-row");
  if (fleetFilter && fleetRows.length) {
    var noMatch = document.querySelector(".fleet-nomatch");
    var chips = document.querySelectorAll(".chip[data-tag]");
    var activeTag = "";
    var applyFleetFilter = function () {
      var q = fleetFilter.value.trim().toLowerCase();
      var shown = 0;
      for (var i = 0; i < fleetRows.length; i++) {
        var hay = fleetRows[i].getAttribute("data-search") || "";
        var ok = (!q || hay.indexOf(q) !== -1) && (!activeTag || hay.indexOf(activeTag) !== -1);
        fleetRows[i].style.display = ok ? "" : "none";
        if (ok) shown++;
      }
      if (noMatch) noMatch.hidden = shown > 0;
    };
    fleetFilter.addEventListener("input", applyFleetFilter);
    for (var c = 0; c < chips.length; c++) {
      chips[c].addEventListener("click", function () {
        var tag = (this.getAttribute("data-tag") || "").toLowerCase();
        activeTag = activeTag === tag ? "" : tag;
        for (var j = 0; j < chips.length; j++) {
          chips[j].classList.toggle("on", (chips[j].getAttribute("data-tag") || "").toLowerCase() === activeTag);
        }
        applyFleetFilter();
      });
    }
  }

  // --- Widget settings (orders scope picker) ------------------
  // A change applies at once: the form IS the plain submit (the
  // no-JS path), this just skips the button press. In customize
  // mode the page reloads so the live card re-renders in place.
  var scopeForms = document.querySelectorAll("form.widget-scope");
  for (var s = 0; s < scopeForms.length; s++) {
    scopeForms[s].addEventListener("change", function () {
      this.submit();
    });
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
  var homeGrid = document.getElementById("home-grid");
  if (homeGrid && homeGrid.getAttribute("data-customize") === "1" && window.fetch) {
    initHomeCustomize(homeGrid);
  }

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

    function widgetIDs() {
      var ids = [];
      var cards = grid.querySelectorAll(".card[data-widget]");
      for (var i = 0; i < cards.length; i++) {
        ids.push(cards[i].getAttribute("data-widget"));
      }
      return ids;
    }

    function closeModal() { if (modal) modal.hidden = true; }

    // --- the grid engine, mirrored ----------------------------
    // solveHomeSpans is the exact mirror of the Go solver in
    // home_overview.go: walk the order once — a full module, or a
    // flex module marked wide, owns its row; a flex module
    // takes half a row and pairs with the next flex module that
    // fits; a flex module left alone in its row stretches full.
    // The server renders these spans as span3/span6 classes; the
    // mirror re-solves live (drag, resize toggles), so what is
    // under the finger is always the final arrangement.
    function solveHomeSpans(descs, cols) {
      var spans = [];
      var k;
      if (cols < 2) {
        for (k = 0; k < descs.length; k++) spans.push(1);
        return spans;
      }
      var flex = Math.floor(cols / 2);
      var rowUsed = 0, rowStart = 0;
      function closeRow(next) {
        if (next - rowStart === 1 && spans[rowStart] < cols) spans[rowStart] = cols;
        rowStart = next;
        rowUsed = 0;
      }
      for (var i = 0; i < descs.length; i++) {
        var span = (descs[i].wide || descs[i].full) ? cols : flex;
        if (rowUsed > 0 && rowUsed + span > cols) closeRow(i);
        spans[i] = span;
        rowUsed += span;
        if (rowUsed === cols) closeRow(i + 1);
      }
      if (rowUsed > 0) closeRow(descs.length);
      return spans;
    }

    function cardDescriptor(el) {
      return {
        wide: el.getAttribute("data-span") === "wide",
        full: el.getAttribute("data-size") === "full"
      };
    }

    function applySpanClass(el, span) {
      el.classList.remove("span3");
      el.classList.remove("span6");
      el.classList.remove("spanall");
      el.classList.add(span >= 6 ? "span6" : "span3");
    }

    // Re-solve the grid and hand every card its span class. Mid
    // drag the dragged card is out of flow and the placeholder
    // stands in for it — pass both; otherwise pass nulls.
    function respan(placeholder, draggedCard) {
      var nodes = [], descs = [];
      var kids = grid.children;
      for (var i = 0; i < kids.length; i++) {
        var el = kids[i];
        if (draggedCard && el === draggedCard) continue;
        if (placeholder && el === placeholder) {
          nodes.push(el);
          descs.push(cardDescriptor(draggedCard));
        } else if (el.classList && el.classList.contains("card") && el.hasAttribute("data-widget")) {
          nodes.push(el);
          descs.push(cardDescriptor(el));
        }
      }
      var spans = solveHomeSpans(descs, 6);
      for (var j = 0; j < nodes.length; j++) applySpanClass(nodes[j], spans[j]);
    }

    // Give one card its drag handle, resize toggle and × button.
    // Cards added from the pop-up pass through here too.
    function enhanceCard(card) {
      var heading = card.querySelector(":scope > h2, :scope > h3");
      if (!heading || card.querySelector(".draghandle")) return;
      var id = card.getAttribute("data-widget");
      // The fold button (+ / –) already lives in the heading;
      // keep its glyph out of the accessible name.
      var title = heading.textContent.replace(/[+–-]\s*$/, "").trim();

      var handle = document.createElement("button");
      handle.type = "button";
      handle.className = "draghandle";
      handle.textContent = "⠿";
      handle.setAttribute("aria-label", "Drag to reorder " + title);
      heading.insertBefore(handle, heading.firstChild);
      handle.addEventListener("pointerdown", function (ev) {
        startDrag(ev, card, handle);
      });

      // Flex modules get the half/full resize toggle: flip the
      // stored span preference, then re-solve the grid in place.
      if (card.getAttribute("data-size") === "flex") {
        card.classList.add("has-spanbtn");
        var spanBtn = document.createElement("button");
        spanBtn.type = "button";
        spanBtn.className = "cardspan";
        spanBtn.textContent = "⤢";
        var paintSpanBtn = function () {
          var wide = card.getAttribute("data-span") === "wide";
          spanBtn.setAttribute("aria-pressed", wide ? "true" : "false");
          spanBtn.setAttribute("aria-label", wide
            ? "Show " + title + " at half width"
            : "Show " + title + " full width");
        };
        paintSpanBtn();
        spanBtn.addEventListener("click", function () {
          var next = card.getAttribute("data-span") === "wide" ? "auto" : "wide";
          spanBtn.disabled = true;
          postLayout({ action: "span", widget: id, span: next }).then(function (ok) {
            spanBtn.disabled = false;
            if (!ok) return;
            card.setAttribute("data-span", next === "wide" ? "wide" : "");
            paintSpanBtn();
            flipSiblings(function () { respan(null, null); });
          });
        });
        heading.appendChild(spanBtn);
      }

      var remove = document.createElement("button");
      remove.type = "button";
      remove.className = "cardremove";
      remove.textContent = "×";
      remove.setAttribute("aria-label", "Remove " + title + " from your home");
      remove.addEventListener("click", function () {
        remove.disabled = true;
        postLayout({ action: "toggle", widget: id }).then(function (ok) {
          if (ok) {
            flipSiblings(function () {
              card.remove();
              respan(null, null);
            });
          } else {
            remove.disabled = false;
          }
        });
      });
      heading.appendChild(remove);
    }

    // --- dragging ---------------------------------------------
    // The model: lifting a card swaps a placeholder into its
    // slot, wearing the span class the solver assigns the card
    // at the hovered position, so the layout never collapses
    // mid-drag; the card itself goes position:fixed and follows
    // the pointer, fully opaque with its real panel colors.
    // When the pointer crosses a sibling's center (plus a
    // hysteresis margin) the placeholder hops slots, everything
    // re-solves, and the displaced siblings FLIP-animate into
    // their new cells — position and size — instead of
    // teleporting. Holding the card near a viewport edge
    // auto-scrolls the page, so long moves on a short screen
    // don't need grab-drop-grab. On drop the card animates into
    // the placeholder's slot and swaps places with it; a
    // cancelled drag or a failed save restores the pre-drag
    // order from a DOM snapshot taken at lift-off.
    var reducedMotion = window.matchMedia &&
      window.matchMedia("(prefers-reduced-motion: reduce)").matches;

    // Midpoint hysteresis, in pixels: the pointer must push
    // this far past the neighbor cell's center before the solve
    // may change, so a boundary never flickers under the finger.
    var hyst = 10;

    function siblingCards() {
      return grid.querySelectorAll(".card[data-widget]:not(.dragging)");
    }

    // FLIP-animate the sibling cards around a DOM mutation:
    // measure (position and size), mutate, then invert each
    // moved card back onto its old spot — a translate, plus a
    // brief scale for cards whose solved span changed size —
    // and let a transition play it into the new cell. Cleanup
    // removes every inline property again.
    function flipSiblings(mutate) {
      var cards = siblingCards();
      var first = [];
      for (var i = 0; i < cards.length; i++) {
        first.push(cards[i].getBoundingClientRect());
      }
      mutate();
      if (reducedMotion) return;
      for (var j = 0; j < cards.length; j++) {
        (function (el, before) {
          var now = el.getBoundingClientRect();
          var dx = before.left - now.left, dy = before.top - now.top;
          var sx = now.width ? before.width / now.width : 1;
          var sy = now.height ? before.height / now.height : 1;
          if (!dx && !dy && sx === 1 && sy === 1) return;
          el.style.transformOrigin = "0 0";
          el.style.transform = "translate(" + dx + "px," + dy + "px) scale(" + sx + "," + sy + ")";
          el.style.transition = "none";
          // Reading offsetWidth commits the inverted state
          // before the transition is armed.
          void el.offsetWidth;
          el.style.transition = "transform 180ms ease";
          el.style.transform = "";
          var done = function () {
            el.style.transition = "";
            el.style.transform = "";
            el.style.transformOrigin = "";
            el.removeEventListener("transitionend", done);
          };
          el.addEventListener("transitionend", done);
          window.setTimeout(done, 260);
        })(cards[j], first[j]);
      }
    }

    // The insertion index a pointer at (x, y) asks for, counted
    // in sibling cards before the placeholder, row-major: a card
    // counts as passed once the pointer crosses its middle —
    // below its center, or right of its center within its row.
    function insertionIndex(x, y) {
      var cards = siblingCards();
      for (var i = 0; i < cards.length; i++) {
        var r = cards[i].getBoundingClientRect();
        if (y < r.top + r.height / 2) return i;
        if (y <= r.bottom && x < r.left + r.width / 2) return i;
      }
      return cards.length;
    }

    // The index the drag may actually use: the natural index,
    // damped by midpoint hysteresis against the placeholder's
    // current neighbor, so the solve never flip-flops while the
    // pointer hovers on a boundary.
    function desiredIndex(x, y, current) {
      var want = insertionIndex(x, y);
      if (want === current) return current;
      var cards = siblingCards();
      var boundary = want > current ? cards[current] : cards[current - 1];
      if (!boundary) return want;
      var r = boundary.getBoundingClientRect();
      var cx = r.left + r.width / 2, cy = r.top + r.height / 2;
      var inRow = y >= r.top && y <= r.bottom;
      if (want > current) {
        return (inRow ? x > cx + hyst : y > cy + hyst) ? want : current;
      }
      return (inRow ? x < cx - hyst : y < cy - hyst) ? want : current;
    }

    function startDrag(ev, card, handle) {
      if (ev.pointerType === "mouse" && ev.button !== 0) return;
      ev.preventDefault();

      var startX = ev.clientX, startY = ev.clientY;
      var lastX = startX, lastY = startY;
      var active = false;   // pointer passed the grab threshold
      var lifted = false;   // card out of flow, placeholder in
      var placeholder = null;
      var phIndex = 0;      // sibling cards ahead of the placeholder
      var grabDX = 0, grabDY = 0;
      var savedNodes = null;
      var rafId = 0;

      if (handle.setPointerCapture) {
        try { handle.setPointerCapture(ev.pointerId); } catch (err) { /* capture is a nicety */ }
      }

      function lift() {
        lifted = true;
        savedNodes = [];
        var all = grid.querySelectorAll(".card[data-widget]");
        for (var i = 0; i < all.length; i++) savedNodes.push(all[i]);
        phIndex = 0;
        for (var j = 0; j < all.length; j++) {
          if (all[j] === card) { phIndex = j; break; }
        }
        var rect = card.getBoundingClientRect();
        grabDX = lastX - rect.left;
        grabDY = lastY - rect.top;
        // The placeholder takes the card's slot wearing the span
        // the solver gives it there and the card's own height,
        // so the grid underneath is the solved final layout for
        // the whole drag.
        placeholder = document.createElement("section");
        placeholder.className = "card drag-placeholder";
        applySpanClass(placeholder, card.classList.contains("span6") ? 6 : 3);
        placeholder.style.height = rect.height + "px";
        placeholder.setAttribute("aria-hidden", "true");
        grid.insertBefore(placeholder, card.nextSibling);
        card.classList.add("dragging");
        card.style.width = rect.width + "px";
        card.style.height = rect.height + "px";
        card.style.left = rect.left + "px";
        card.style.top = rect.top + "px";
        respan(placeholder, card);
        document.body.classList.add("drag-active");
        rafId = requestAnimationFrame(tick);
      }

      // One loop drives the follow-the-pointer positioning, the
      // edge auto-scroll and the placeholder hop, so a burst of
      // pointermove events costs one frame of work.
      function tick() {
        rafId = 0;
        if (!lifted) return;
        position();
        // Edge auto-scroll: the closer the pointer rides to a
        // viewport edge, the faster the page rolls under it.
        var edge = 76, maxStep = 14, speed = 0;
        if (lastY < edge) {
          speed = -Math.min(maxStep, Math.ceil((edge - lastY) / 5));
        } else if (lastY > window.innerHeight - edge) {
          speed = Math.min(maxStep, Math.ceil((lastY - (window.innerHeight - edge)) / 5));
        }
        if (speed !== 0) window.scrollBy(0, speed);
        movePlaceholder();
        rafId = requestAnimationFrame(tick);
      }

      function position() {
        card.style.left = (lastX - grabDX) + "px";
        card.style.top = (lastY - grabDY) + "px";
      }

      // Hop the placeholder only when the pointer has carried
      // the insertion past the current neighbor's center (plus
      // hysteresis); the grid re-solves around the hop and the
      // siblings FLIP into their new cells, so nothing under
      // the finger teleports.
      function movePlaceholder() {
        var idx = desiredIndex(lastX, lastY, phIndex);
        if (idx === phIndex) return;
        var cards = siblingCards();
        var target = idx < cards.length ? cards[idx] : null;
        flipSiblings(function () {
          if (target) grid.insertBefore(placeholder, target);
          else grid.appendChild(placeholder);
          respan(placeholder, card);
        });
        phIndex = idx;
      }

      function onMove(e) {
        lastX = e.clientX;
        lastY = e.clientY;
        if (!active) {
          if (Math.abs(lastX - startX) + Math.abs(lastY - startY) < 5) return;
          active = true;
          lift();
        }
      }

      function detach() {
        handle.removeEventListener("pointermove", onMove);
        handle.removeEventListener("pointerup", onUp);
        handle.removeEventListener("pointercancel", onCancel);
        if (rafId) cancelAnimationFrame(rafId);
        rafId = 0;
      }

      // Every exit path funnels through here: no placeholder,
      // no lifted card, no inline style survives a drag.
      function cleanup() {
        if (placeholder) { placeholder.remove(); placeholder = null; }
        card.classList.remove("dragging");
        card.classList.remove("drag-settle");
        card.style.cssText = "";
        document.body.classList.remove("drag-active");
      }

      function restoreOrder() {
        if (!savedNodes) return;
        for (var i = 0; i < savedNodes.length; i++) {
          grid.appendChild(savedNodes[i]);
        }
      }

      function onUp() {
        detach();
        if (!lifted) return;
        // Settle: animate the card into the placeholder's solved
        // slot (gliding to its position and size), swap the two
        // in the DOM, then save. A failed save restores the
        // pre-drag arrangement.
        var slot = placeholder.getBoundingClientRect();
        card.classList.add("drag-settle");
        card.style.left = slot.left + "px";
        card.style.top = slot.top + "px";
        card.style.width = slot.width + "px";
        card.style.height = slot.height + "px";
        card.style.transform = "scale(1)";
        var swapIn = function () {
          grid.replaceChild(card, placeholder);
          placeholder = null;
          cleanup();
          respan(null, null);
          postLayout({ action: "order", ids: widgetIDs().join(",") }).then(function (ok) {
            if (!ok) {
              restoreOrder();
              respan(null, null);
            }
            savedNodes = null;
          });
        };
        if (reducedMotion) swapIn();
        else window.setTimeout(swapIn, 190);
      }

      function onCancel() {
        detach();
        if (!lifted) return;
        cleanup();
        restoreOrder();
        respan(null, null);
        savedNodes = null;
      }

      handle.addEventListener("pointermove", onMove);
      handle.addEventListener("pointerup", onUp);
      handle.addEventListener("pointercancel", onCancel);
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
          flipSiblings(function () {
            grid.appendChild(node);
            respan(null, null);
          });
          addFold(node, node.querySelector(":scope > h2, :scope > h3"));
          enhanceCard(node);
          closeModal();
        }, function () { window.location.reload(); });
      });
    }

    var cards = grid.querySelectorAll(".card[data-widget]");
    for (var i = 0; i < cards.length; i++) enhanceCard(cards[i]);
    // The server solved and rendered these spans already; one
    // mirror pass here keeps the two solvers in lockstep.
    respan(null, null);

    // Add-module pop-up. The full catalog is rendered into it;
    // each open re-filters against what is on the home right now,
    // so removes and adds are reflected immediately.
    var addBtn = document.getElementById("add-module-btn");
    if (addBtn && modal) {
      var emptyNote = modal.querySelector(".modal-empty");
      var addItems = modal.querySelectorAll("[data-add-widget]");
      var refreshModal = function () {
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
      };
      addBtn.addEventListener("click", function (ev) {
        ev.preventDefault();
        refreshModal();
        modal.hidden = false;
      });
      modal.addEventListener("click", function (ev) {
        if (ev.target === modal) closeModal();
      });
      var closers = modal.querySelectorAll("[data-close-modal]");
      for (var c = 0; c < closers.length; c++) {
        closers[c].addEventListener("click", closeModal);
      }
      document.addEventListener("keydown", function (ev) {
        if (ev.key === "Escape" && !modal.hidden) closeModal();
      });
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
          lab.textContent = it.label;
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

  // Market: a pick jumps straight to that item's market page,
  // keeping the region the page is showing.
  (function () {
    var regionField = document.getElementById("market-region");
    attachSuggest(
      document.getElementById("market-q"),
      document.getElementById("market-suggest"),
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
    var only = document.getElementById("assets-only");
    attachSuggest(
      document.getElementById("assets-q"),
      document.getElementById("assets-suggest"),
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
    document.getElementById("sync-character-q"),
    document.getElementById("sync-character-suggest"),
    suggestURL("/admin/suggest", null),
    function (it) { window.location.href = "/sync/?character=" + encodeURIComponent(it.id); });
  attachSuggest(
    document.getElementById("admin-q"),
    document.getElementById("admin-suggest"),
    suggestURL("/admin/suggest", null),
    function (it) { window.location.href = "/admin/?q=" + encodeURIComponent(it.id); });

  // Watchlist finder: a pick fills the box with the exact name
  // and runs the same find the button would.
  (function () {
    var input = document.getElementById("watch-q");
    attachSuggest(input, document.getElementById("watch-suggest"),
      suggestURL("/items/search.json", "market"),
      function (it, input) {
        input.value = it.name;
        if (input.form) input.form.submit();
      });
  })();

  // Planner: a pick opens that product's plan.
  attachSuggest(
    document.getElementById("planner-q"),
    document.getElementById("planner-suggest"),
    suggestURL("/items/search.json", "planner"),
    function (it) {
      window.location.href = "/planner/?product=" + encodeURIComponent(it.id);
    });

  // Skill-plan add box: a pick fills the exact skill name and
  // runs the search, which offers the level picker as usual.
  (function () {
    var input = document.getElementById("skill-q");
    attachSuggest(input, document.getElementById("skill-suggest"),
      suggestURL("/items/search.json", "skills"),
      function (it, input) {
        input.value = it.name;
        if (input.form) input.form.submit();
      });
  })();

  // Items DB search: a pick opens the item's details page.
  attachSuggest(
    document.getElementById("items-q"),
    document.getElementById("items-suggest"),
    suggestURL("/items/search.json", "all"),
    function (it) {
      window.location.href = "/items/type/" + encodeURIComponent(it.id) + "/";
    });

  // Banner global search: items, your characters, pilots and
  // organizations the app already knows, each to its own page.
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
  var regions = document.querySelectorAll("[data-live-region]");
  if (!regions.length || !window.fetch) return;
  var kicks = [];

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

  for (var i = 0; i < regions.length; i++) {
    arm(regions[i]);
  }
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
  var page = window.location.pathname + window.location.search;
  var polls = 0;
  var timer = null;
  var stopped = false;

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

  function stop() {
    stopped = true;
    if (timer !== null) {
      window.clearTimeout(timer);
      timer = null;
    }
  }
  function schedule() {
    if (stopped) return;
    // ~1.5s for the first couple of minutes, then a steady
    // ~5s while the page is still waiting.
    timer = window.setTimeout(poll, polls <= 80 ? 1500 : 5000);
  }
  function poll() {
    timer = null;
    if (stopped) return;
    polls += 1;
    window.fetch("/sync/page-status?page=" + encodeURIComponent(page), {
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
        stop();
      }
    }).catch(function () {
      // A failed status poll is not news; the next one retries
      // at the same cadence.
      schedule();
    });
  }
  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState !== "visible" || stopped) return;
    if (timer !== null) {
      window.clearTimeout(timer);
      timer = null;
    }
    poll();
  });
  schedule();
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
    // the next page load and it never paints expanded first (see
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
    if (openBranch && !openBranch.contains(e.target)) {
      closeCategoryBranches(null);
    }
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
  if (currentState() === "rail") {
    closeCategoryBranches(null);
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
    { name: "Skill plans", url: "/skills/plans" },
    { name: "Ship Workbench", url: "/fittings/" },
    { name: "Ship Loadouts", url: "/fittings/saved/" },
    { name: "Killmails", url: "/killmails/" },
    { name: "Mail", url: "/mail/" },
    { name: "Calendar", url: "/calendar/" },
    { name: "Contacts", url: "/contacts/" },
    { name: "Assets", url: "/assets/" },
    { name: "Industry", url: "/industry/" },
    { name: "Build Planner", url: "/planner/" },
    { name: "Planetary Industry", url: "/planets/" },
    { name: "Market", url: "/market/" },
    { name: "Spread scanner", url: "/market/scanner/" },
    { name: "Tradefinder", url: "/market/tradefinder/" },
    { name: "Top stations", url: "/market/leaderboard/" },
    { name: "Restock planner", url: "/market/restock/" },
    { name: "Item Browser", url: "/items/" },
    { name: "Wallet", url: "/wallet/" },
    { name: "Orders", url: "/orders/" },
    { name: "Contracts", url: "/contracts/" },
    { name: "Corporation Overview", url: "/corporations/" },
    { name: "Corporation Members", url: "/corporations/members/" },
    { name: "Corporation Wallets", url: "/corporations/wallets/" },
    { name: "Corporation Orders", url: "/corporations/orders/" },
    { name: "Corporation Assets", url: "/corporations/assets/" },
    { name: "Corporation Structures", url: "/corporations/structures/" },
    { name: "Corporation Killmails", url: "/corporations/killmails/" },
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
      default: return "/market/?type=" + encodeURIComponent(hit.id);
    }
  }

  var entries = [];   // flat, selectable rows in display order
  var selected = -1;
  var lastFocus = null;
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
            lab.textContent = hit.label;
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

  function openPalette() {
    if (isOpen()) return;
    lastFocus = document.activeElement;
    overlay.hidden = false;
    document.body.classList.add("quickjump-open");
    input.value = "";
    ask("");
    input.focus();
    input.select();
  }

  function closePalette() {
    if (!isOpen()) return;
    overlay.hidden = true;
    document.body.classList.remove("quickjump-open");
    querySerial++; // an answer in flight no longer matters
    if (timer) window.clearTimeout(timer);
    timer = null;
    if (lastFocus && lastFocus.focus) lastFocus.focus();
    lastFocus = null;
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

  // A click on the dimmed surround closes; clicks inside the
  // panel (on the input, on gaps between rows) do not.
  overlay.addEventListener("mousedown", function (ev) {
    if (ev.target === overlay) closePalette();
  });
})();


// --- Theme toggle (dark/light/factions) ------------------------------
// A stored choice in localStorage wins and is written onto
// document.documentElement.dataset.theme; with no stored
// choice the attribute stays unset so the stylesheet's
// prefers-color-scheme media query applies. The button only
// renders while logged in (see base.html), so everything here
// is guarded: no button, no work.
// v0.3.34: simple dark/light toggle (faction themes deferred).
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
  // wins; otherwise dark is the default (the app's classic palette).
  // The OS preference is intentionally ignored — dark is our best face.
  // Icon visibility is CSS-driven (html[data-theme] selectors); no JS needed.
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

// v0.3.32: character selector dropdown with search/filter.
(function() {
  document.querySelectorAll('[data-charselector]').forEach(function(root) {
    var input = root.querySelector('.charselector-input');
    var dropdown = root.querySelector('.charselector-dropdown');
    var options = Array.prototype.slice.call(root.querySelectorAll('.charselector-option'));
    if (!input || !dropdown) return;

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
    // Close on outside click.
    document.addEventListener('click', function(e) {
      if (!root.contains(e.target)) {
        dropdown.hidden = true;
        // Restore active name if input was cleared.
        if (!input.value && active) input.value = active.getAttribute('data-name') || '';
      }
    });
    // Close on Escape.
    input.addEventListener('keydown', function(e) {
      if (e.key === 'Escape') {
        dropdown.hidden = true;
        input.blur();
      }
    });
  });

  // --- CSV export links ---------------------------------------
  // Market/planner pages carry an "Export CSV" link whose href is
  // the current URL plus format=csv, set here (one shared place;
  // the pages used to repeat it as inline scripts, which the
  // content security policy no longer allows).
  var csvExportLink = document.getElementById("csv-export");
  if (csvExportLink) {
    csvExportLink.href = window.location.pathname + window.location.search +
      (window.location.search ? "&" : "?") + "format=csv";
  }

  // --- Client-side table filters ------------------------------
  // Industry search boxes filter their table's rows by a
  // data-search attribute (moved here from inline scripts, same
  // behavior):
  //   <input data-table-filter="bp-table" data-row-class="bp-row"
  //          data-header-class="bp-cat-header">
  // Rows whose data-search misses hide; category headers with no
  // visible rows hide with them. data-header-class is optional.
  var tableFilterBoxes = document.querySelectorAll("input[data-table-filter]");
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

  // --- Auto-refresh pages -------------------------------------
  // The Sync page reloads itself every few seconds
  // (body[data-autorefresh]); with JavaScript off, a <noscript>
  // meta-refresh does the same. The reload waits out typing in a
  // field and hidden tabs, and never runs for readers who asked
  // for reduced motion — they get the page as it loaded, plus the
  // Sync page's own manual controls.
  var autoRefreshSeconds = parseInt(document.body.getAttribute("data-autorefresh") || "", 10);
  if (autoRefreshSeconds > 0 &&
      !(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches)) {
    (function scheduleRefresh() {
      window.setTimeout(function () {
        var active = document.activeElement;
        var typing = active && /^(INPUT|TEXTAREA|SELECT)$/.test(active.tagName || "");
        if (!typing && !document.hidden) {
          window.location.reload();
          return;
        }
        scheduleRefresh();
      }, autoRefreshSeconds * 1000);
    })();
  }


  // --- Filters that apply as they change -----------------------
  // A control marked data-autosubmit (the Item Database's "Market
  // items only" box and its category and group pickers) submits its
  // form the moment it changes, so a filter applies at once and the
  // result keeps its Category › Group layout; the form's own button
  // stays for readers without JavaScript. Picking a different
  // category clears the group first: the old group would otherwise
  // pull the category back.
  document.addEventListener("change", function (ev) {
    var control = ev.target && ev.target.closest ? ev.target.closest("[data-autosubmit]") : null;
    if (!control || !control.form) return;
    if (control.name === "category" && control.form.elements.group) {
      control.form.elements.group.value = "";
    }
    if (control.form.requestSubmit) control.form.requestSubmit();
    else control.form.submit();
  });

  // --- Buttons that navigate -----------------------------------
  // <button data-href="/path"> goes to that path on click: what an
  // inline click handler on the button used to do, which the content
  // security policy no longer allows. Only same-site paths count.
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
  (function () {
    var picker = document.querySelector("[data-plan-picker]");
    if (!picker) return;
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
  })();

  // --- Data-driven geometry ------------------------------------
  // The content security policy forbids inline style attributes, so
  // the numbers the server used to write into inline styles ride
  // data-* attributes instead, and are applied here through the
  // CSSOM (which the policy allows):
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

// Notification settings: under each kind, the characters it is on for
// (a [data-checkselector]: the character selector's look, with a tick
// box per character). The field shows the count and filters the list
// as you type; the list stays open while boxes are ticked and closes
// on a click elsewhere or Escape. The boxes are ordinary form fields,
// so saving works the same with or without this.
(function () {
  "use strict";
  var selectors = Array.prototype.slice.call(document.querySelectorAll("[data-checkselector]"));
  if (!selectors.length) return;

  selectors.forEach(function (root) {
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
      selectors.forEach(function (other) { if (other !== root && other.closeList) other.closeList(); });
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

  document.addEventListener("click", function (ev) {
    selectors.forEach(function (root) {
      if (!root.contains(ev.target) && root.closeList) root.closeList();
    });
  });
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
