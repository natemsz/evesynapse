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

  var panel = document.querySelector("main > .panel");
  if (panel && !panel.querySelector(".foldable, .card")) {
    addFold(panel, panel.querySelector(":scope > h1"));
  }

  var foldables = document.querySelectorAll(".foldable");
  for (var i = 0; i < foldables.length; i++) {
    addFold(foldables[i], foldables[i].querySelector(":scope > h2, :scope > h3"));
  }

  var cards = document.querySelectorAll(".card");
  for (var i = 0; i < cards.length; i++) {
    addFold(cards[i], cards[i].querySelector(":scope > h2, :scope > h3"));
  }

  // --- Nav: Escape folds an open branch back up -----------------
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Escape") return;
    var open = document.querySelectorAll("details.branch[open]");
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
    // overview.go: walk the order once — a full module, or a
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
        card.appendChild(spanBtn);
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
      card.appendChild(remove);
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
    // Balance charts carry one figure; market history dots
    // carry the full day. Render whichever the dot brought.
    var balance = dot.getAttribute("data-balance");
    if (balance) {
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
    { name: "Character", url: "/character/" },
    { name: "Characters", url: "/characters/" },
    { name: "Skills", url: "/skills/" },
    { name: "Skill plans", url: "/skills/plans" },
    { name: "Fittings", url: "/fittings/" },
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
    { name: "Items", url: "/items/" },
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

// Fitting simulator editor (v0.3.21): the browser keeps the fit
// document (ship, items, charge choices) and posts it to
// /fittings/simulate/ on every change; the server answers with
// the re-rendered workbench fragment, which swaps in place. The
// canonical document rides back on the fragment root, so the
// editor state always matches what the server calculated.
(function () {
  var editor = document.getElementById("fit-editor");
  if (!editor || !window.fetch) return;

  var pilotSel = document.getElementById("fit-pilot");
  var nameInput = document.getElementById("fit-name");
  var shipInput = document.getElementById("fit-ship-search");
  var shipList = document.getElementById("fit-ship-suggest");
  var modInput = document.getElementById("fit-mod-search");
  var modList = document.getElementById("fit-mod-suggest");
  var famChips = document.getElementById("fit-famchips");
  var fittableChk = document.getElementById("fit-fittable");
  var usableChk = document.getElementById("fit-usable");
  var metaSel = document.getElementById("fit-meta");
  var picker = document.getElementById("fit-picker");
  var pickerHint = document.getElementById("fit-picker-hint");
  var pickerList = document.getElementById("fit-picker-list");
  var pickerFamily = document.getElementById("fit-picker-family");
  var saveBtn = document.getElementById("fit-save");
  var exportBtn = document.getElementById("fit-export-btn");
  var exportOut = document.getElementById("fit-eft-out");

  function workbench() { return editor.querySelector(".fit-wb"); }

  function readState() {
    var wb = workbench();
    if (!wb) return null;
    try { return JSON.parse(wb.getAttribute("data-fit-state") || "{}"); }
    catch (e) { return null; }
  }

  var state = readState() || {};
  state.name = state.name || "";
  state.shipTypeId = state.shipTypeId || 0;
  state.items = state.items || [];
  state.charges = state.charges || {};

  function pilotID() {
    return pilotSel ? (parseInt(pilotSel.value, 10) || 0) : 0;
  }

  var simSeq = 0;
  var simInFlight = false;
  var simQueued = false;
  // No silent taps: the last add/remove/charge change arms a flash
  // that fires on the re-rendered target once simulate() lands.
  var pendingFlash = null; // {type: "slot"|"group"|"charge", id: number, family: string}
  function simulate() {
    // Coalesce bursts: one request in flight at a time; a change
    // made mid-flight re-sends once with the latest state instead
    // of piling up stale POSTs.
    if (simInFlight) { simQueued = true; return; }
    simInFlight = true;
    state.name = nameInput ? nameInput.value : (state.name || "");
    var seq = ++simSeq;
    var payload = {
      name: state.name,
      shipTypeId: state.shipTypeId,
      items: state.items,
      charges: state.charges,
      pilot: pilotID()
    };
    function simDone() {
      simInFlight = false;
      if (simQueued) { simQueued = false; simulate(); }
    }
    fetch("/fittings/simulate/", {
      method: "POST",
      headers: { "Content-Type": "application/json", "Accept": "text/html" },
      body: JSON.stringify(payload)
    }).then(function (resp) {
      return resp.ok ? resp.text() : "";
    }).then(function (html) {
      if (!html || seq !== simSeq) { simDone(); return; }
      var tmp = document.createElement("div");
      tmp.innerHTML = html;
      var fresh = tmp.querySelector(".fit-wb");
      var current = workbench();
      if (!fresh || !current) { simDone(); return; }
      current.replaceWith(fresh);
      var next = readState();
      if (next) {
        next.items = next.items || [];
        next.charges = next.charges || {};
        state = next;
      }
      syncFilterUI();
      syncSubsystemUI();
      syncUndoButtons();
      runPendingFlash();
      simDone();
    }).catch(function () { simDone(); /* the static copy stays honest */ });
  }

  // Fires the armed add/remove flash on the re-rendered target.
  function runPendingFlash() {
    var f = pendingFlash;
    pendingFlash = null;
    if (!f) return;
    var el = null;
    if (f.type === "slot") {
      el = editor.querySelector('.fit-vslot[data-fit-vslot="' + f.id + '"]');
    } else if (f.type === "group") {
      el = editor.querySelector('.fit-group[data-fit-group="' + f.family + '"]');
    } else if (f.type === "charge") {
      el = editor.querySelector('select[data-fit-charge="' + f.id + '"]');
      if (!el) el = editor.querySelector('.fit-vslot[data-fit-vslot="' + f.id + '"] .fit-vammo');
    }
    if (!el) return;
    el.classList.add("flash");
    if (el.scrollIntoView) el.scrollIntoView({ block: "nearest" });
    window.setTimeout(function () { el.classList.remove("flash"); }, 750);
  }

  // --- filter controls: enabled states follow ship/pilot ----------
  function syncFilterUI() {
    var hasShip = !!(state && state.shipTypeId);
    if (fittableChk) {
      fittableChk.disabled = !hasShip;
      if (!hasShip) fittableChk.checked = false;
      var fl = fittableChk.closest("label");
      if (fl) fl.classList.toggle("disabled", !hasShip);
    }
    var allV = pilotID() === 0;
    if (usableChk) {
      usableChk.disabled = allV;
      if (allV) usableChk.checked = false;
      var ul = usableChk.closest("label");
      if (ul) ul.classList.toggle("disabled", allV);
      usableChk.title = allV ? "All V pilots can use everything" : "";
    }
  }

  function findItem(typeID) {
    for (var i = 0; i < state.items.length; i++) {
      if (state.items[i].typeId === typeID) return i;
    }
    return -1;
  }

  // --- undo / redo -------------------------------------------------
  // Client-side history of fit states (ship + modules + charges +
  // drones). Every mutation pushes the pre-change state; undo/redo
  // re-post the restored state through simulate(). Capped at 50.
  var past = [];
  var future = [];
  function snapState() {
    return {
      shipTypeId: state.shipTypeId,
      shipName: shipInput ? shipInput.value : "",
      items: JSON.parse(JSON.stringify(state.items || [])),
      charges: JSON.parse(JSON.stringify(state.charges || {}))
    };
  }
  function applySnap(s) {
    state.shipTypeId = s.shipTypeId;
    state.items = JSON.parse(JSON.stringify(s.items || []));
    state.charges = JSON.parse(JSON.stringify(s.charges || {}));
    if (shipInput) shipInput.value = s.shipName || "";
    markDirty();
    simulate();
    syncUndoButtons();
  }
  function pushHistory() {
    past.push(snapState());
    if (past.length > 50) past.shift();
    future = [];
    syncUndoButtons();
  }
  function undo() {
    if (!past.length) return;
    future.push(snapState());
    applySnap(past.pop());
    syncUndoButtons();
  }
  function redo() {
    if (!future.length) return;
    past.push(snapState());
    applySnap(future.pop());
    syncUndoButtons();
  }
  // Undo/redo ride the visual's corner and re-render with it, so
  // the buttons are re-queried every time (delegated clicks).
  function syncUndoButtons() {
    var ub = editor.querySelectorAll("[data-fit-undo]");
    var rb = editor.querySelectorAll("[data-fit-redo]");
    for (var i = 0; i < ub.length; i++) ub[i].disabled = !past.length;
    for (var j = 0; j < rb.length; j++) rb[j].disabled = !future.length;
  }
  syncUndoButtons();

  // --- autosave ----------------------------------------------------
  // Debounced ~1.5s after the last change, through the existing
  // save path. A named fit updates in place; an unsaved draft
  // keeps a single draft row (the server upserts it).
  var dirty = false;
  var autosaveTimer = null;
  var savedAtEl = document.getElementById("fit-saved-at");
  var descInput = document.getElementById("fit-desc");
  var tagsInput = document.getElementById("fit-tags");
  function markDirty() {
    dirty = true;
    if (autosaveTimer) clearTimeout(autosaveTimer);
    autosaveTimer = setTimeout(autosave, 1500);
  }
  function autosave() {
    autosaveTimer = null;
    if (!dirty || !state.shipTypeId) return;
    var localID = parseInt((document.getElementById("fit-local-id") || {}).value || "0", 10) || 0;
    var nm = nameInput ? nameInput.value : (state.name || "");
    var payload = {
      id: localID,
      name: nm,
      description: descInput ? descInput.value : "",
      tags: tagsInput ? tagsInput.value : "",
      fit: {
        name: nm,
        shipTypeId: state.shipTypeId,
        items: state.items,
        charges: state.charges
      }
    };
    fetch("/fittings/save/", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload)
    }).then(function (resp) {
      return resp.ok ? resp.json() : null;
    }).then(function (data) {
      if (!data || !data.id) return;
      dirty = false;
      var hid = document.getElementById("fit-local-id");
      if (hid) hid.value = String(data.id);
      if (savedAtEl && data.savedAt) {
        savedAtEl.textContent = "Saved ✓ " + data.savedAt;
        savedAtEl.hidden = false;
      }
    }).catch(function () { /* retry on the next change */ });
  }

  // --- ship-restricted modules -------------------------------------
  // The workbench carries data-fit-restricted: {typeID: "Dreadnoughts"}.
  function restrictionFor(typeID) {
    var wb = workbench();
    if (!wb) return "";
    try {
      var m = JSON.parse(wb.getAttribute("data-fit-restricted") || "{}");
      return m[String(typeID)] || "";
    } catch (e) { return ""; }
  }
  function restrictedSet() {
    var wb = workbench();
    if (!wb) return null;
    try {
      var m = JSON.parse(wb.getAttribute("data-fit-restricted") || "{}");
      return Object.keys(m).length ? m : null;
    } catch (e) { return null; }
  }
  // Plain-language refusal when the user tries to add a module
  // this hull cannot fit. Returns true when blocked.
  function checkRestricted(typeID, name) {
    var req = restrictionFor(typeID);
    if (req) {
      fitNotice((name || "That module") + " can only be fitted to " + req + ".", true, false);
      return true;
    }
    return false;
  }

  // Slot families with a fixed maximum: adding past it is a
  // plain-language refusal, never a silent no-op.
  var fitFamLabels = { high: "high", medium: "mid", low: "low", rig: "rig" };
  function addItem(typeID, qty) {
    var fam = familyOfType(typeID) || "";
    if (fitFamLabels[fam]) {
      var max = 0;
      var g = editor.querySelector('.fit-group[data-fit-group="' + fam + '"]');
      if (g) max = parseInt(g.getAttribute("data-fit-max") || "0", 10) || 0;
      var count = 0;
      for (var i = 0; i < state.items.length; i++) {
        if ((familyOfType(state.items[i].typeId) || "") === fam) count += state.items[i].qty || 1;
      }
      if (max > 0 && count >= max) {
        fitNotice("No free " + fitFamLabels[fam] + " slots — remove something first.", true, false);
        return;
      }
    }
    pushHistory();
    var i = findItem(typeID);
    if (i >= 0) state.items[i].qty += qty;
    else state.items.push({ typeId: typeID, qty: qty });
    markDirty();
    pendingFlash = fam === "drone" || fam === "cargo"
      ? { type: "group", family: fam }
      : { type: "slot", id: typeID };
    simulate();
  }
  // addItemAt inserts one module at a doc-order position (drag
  // move/swap), replacing the item list wholesale.
  function setItems(items) {
    pushHistory();
    state.items = items;
    markDirty();
    simulate();
  }
  function decItem(typeID) {
    pushHistory();
    var i = findItem(typeID);
    if (i < 0) return;
    var fam = familyOfType(typeID) || "";
    state.items[i].qty -= 1;
    if (state.items[i].qty <= 0) state.items.splice(i, 1);
    markDirty();
    // The removed module is gone; flash its group so the removal
    // reads as an action, not a glitch.
    pendingFlash = fam ? { type: "group", family: fam } : null;
    simulate();
  }
  function setItemQty(typeID, qty) {
    pushHistory();
    var i = findItem(typeID);
    if (qty <= 0) {
      if (i >= 0) state.items.splice(i, 1);
    } else if (i >= 0) {
      state.items[i].qty = qty;
    } else {
      state.items.push({ typeId: typeID, qty: qty });
    }
    markDirty();
    simulate();
  }
  function setShip(typeID, name) {
    pushHistory();
    state.shipTypeId = typeID;
    if (shipInput && typeof name === "string") shipInput.value = name;
    markDirty();
    simulate();
  }

  // --- workbench clicks (delegated: the workbench re-renders) --
  editor.addEventListener("click", function (ev) {
    var t = ev.target;
    if (!t || !t.getAttribute) return;
    var wb = workbench();
    if (wb && wb.contains(t)) {
      if (t.closest("[data-fit-undo]")) { undo(); return; }
      if (t.closest("[data-fit-redo]")) { redo(); return; }
      var dec = t.getAttribute("data-fit-dec");
      if (dec) { decItem(parseInt(dec, 10) || 0); return; }
      var inc = t.getAttribute("data-fit-inc");
      if (inc) { addItem(parseInt(inc, 10) || 0, 1); return; }
      var add = t.getAttribute("data-fit-add");
      if (add) {
        if (add === "drone") { focusModuleSearch("drone"); return; }
        openPicker(add);
        return;
      }
    }
  });
  editor.addEventListener("change", function (ev) {
    var t = ev.target;
    if (!t || !t.getAttribute) return;
    var chargeFor = t.getAttribute("data-fit-charge");
    if (chargeFor) {
      var weapon = parseInt(chargeFor, 10) || 0;
      var charge = parseInt(t.value, 10) || 0;
      if (charge > 0) state.charges[weapon] = charge;
      else delete state.charges[weapon];
      pendingFlash = { type: "charge", id: weapon };
      simulate();
      return;
    }
    var qtyFor = t.getAttribute("data-fit-qty");
    if (qtyFor) {
      setItemQty(parseInt(qtyFor, 10) || 0, parseInt(t.value, 10) || 0);
    }
  });

  if (pilotSel) pilotSel.addEventListener("change", function () {
    syncFilterUI();
    simulate();
  });
  if (nameInput) {
    nameInput.addEventListener("change", function () {
      state.name = nameInput.value;
    });
  }
  if (descInput) descInput.addEventListener("input", markDirty);
  if (tagsInput) tagsInput.addEventListener("input", markDirty);
  var pubChkInit = document.getElementById("fit-public");
  if (pubChkInit) pubChkInit.addEventListener("change", function () {
    // The public flag persists through the save path.
    var localID = parseInt((document.getElementById("fit-local-id") || {}).value || "0", 10) || 0;
    if (!localID || !state.shipTypeId) return;
    var nm = nameInput ? nameInput.value : (state.name || "");
    fetch("/fittings/save/", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        id: localID, name: nm,
        description: descInput ? descInput.value : "",
        tags: tagsInput ? tagsInput.value : "",
        isPublic: pubChkInit.checked, promote: false,
        fit: { name: nm, shipTypeId: state.shipTypeId, items: state.items, charges: state.charges }
      })
    }).catch(function () {});
  });
  syncFilterUI();
  syncSubsystemUI();

  // --- split searches: ships at the top, modules above the workbench --
  // Ship search: ships only. Module search: modules/drones/rigs/subs
  // only, with the kind-labeled suggestions and the filter row.
  var modFamily = "all";
  var kindOrder = ["high", "medium", "low", "rig", "subsystem", "drone", "charge"];
  var kindLabels = {
    high: "High slots", medium: "Mid slots", low: "Low slots",
    rig: "Rigs", subsystem: "Subsystems", drone: "Drones", charge: "Charges"
  };

  function filterQuery() {
    var meta = metaSel ? metaSel.value : "0";
    var usable = (usableChk && usableChk.checked && pilotID() > 0) ? "1" : "0";
    return "&meta=" + encodeURIComponent(meta) + "&usable=" + usable + "&pilot=" + pilotID();
  }
  // pickerURL always carries the module filters; the ship search
  // passes family=ship and ignores them server-side.
  function pickerURL(family, q) {
    return "/fittings/picker.json?family=" + encodeURIComponent(family) +
      "&q=" + encodeURIComponent(q) + filterQuery();
  }

  // Fittable-only: kinds with free capacity on the current hull,
  // from the slot-group Used/Max the workbench already renders.
  function fittableKinds() {
    if (!fittableChk || !fittableChk.checked) return null;
    var wb = workbench();
    if (!wb) return null;
    var groups = wb.querySelectorAll(".fit-group");
    if (!groups.length) return null;
    var ok = {};
    for (var i = 0; i < groups.length; i++) {
      var g = groups[i];
      var key = g.getAttribute("data-fit-group");
      var used = parseInt(g.getAttribute("data-fit-used") || "0", 10) || 0;
      var max = parseInt(g.getAttribute("data-fit-max") || "0", 10) || 0;
      if (key && max > used) ok[key] = true;
    }
    var bw = parseFloat(wb.getAttribute("data-fit-bandwidth") || "0") || 0;
    if (bw > 0) ok["drone"] = true;
    ok["ship"] = true;
    return ok;
  }
  function applyFittable(rows) {
    var ok = fittableKinds();
    var restricted = restrictedSet();
    if (!ok && !restricted) return rows;
    return (rows || []).filter(function (it) {
      // Ship-restricted modules never list under "Fittable only"
      // for an incompatible hull.
      if (restricted && restricted[it.id]) return false;
      return !ok || !!ok[it.kind || ""];
    });
  }

  // One suggestion engine drives both inputs. cfg: family() returns
  // the endpoint family; kinds limits the groups shown (null =
  // whatever the endpoint returned); dropShips strips ship rows
  // (the module search's "all" chip); pick handles the choice.
  function makeSuggester(input, list, cfg) {
    var timer = null;
    var rows = [];
    var active = -1;
    function close() {
      if (list) list.hidden = true;
      if (input) input.setAttribute("aria-expanded", "false");
      active = -1;
    }
    function icon(id) {
      var img = document.createElement("img");
      img.className = "sug-icon";
      img.alt = "";
      img.loading = "lazy";
      img.draggable = false;
      img.src = "https://images.evetech.net/types/" + id + "/icon?size=32";
      return img;
    }
    function query(force) {
      if (!input || !list) return;
      var q = input.value.trim();
      if (q.length < 2 && !force) { close(); return; }
      fetch(pickerURL(cfg.family(), q), {
        cache: "no-store", headers: { "Accept": "application/json" }
      }).then(function (resp) {
        return resp.ok ? resp.json() : [];
      }).then(function (data) {
        var filtered = applyFittable(data || []);
        if (cfg.dropShips) {
          filtered = filtered.filter(function (it) { return (it.kind || "") !== "ship"; });
        }
        render(filtered);
      }).catch(close);
    }
    function render(data) {
      if (!list) return;
      list.innerHTML = "";
      rows = [];
      active = -1;
      var groups = {};
      (data || []).forEach(function (it) {
        var k = it.kind || "";
        if (cfg.kinds && cfg.kinds.indexOf(k) < 0) return;
        if (!groups[k]) groups[k] = [];
        groups[k].push(it);
      });
      var order = cfg.kinds || kindOrder;
      var any = false;
      order.forEach(function (k) {
        var items = groups[k];
        if (!items || !items.length) return;
        any = true;
        var head = document.createElement("li");
        head.className = "sug-kind";
        head.textContent = (cfg.labels || kindLabels)[k] || k;
        list.appendChild(head);
        items.forEach(function (it) {
          var li = document.createElement("li");
          li.setAttribute("role", "option");
          li.appendChild(icon(it.id));
          var name = document.createElement("span");
          name.textContent = it.name;
          li.appendChild(name);
          if (it.label) {
            var lab = document.createElement("span");
            lab.className = "sug-label";
            lab.textContent = it.label;
            li.appendChild(lab);
          }
          rows.push({ it: it, li: li });
          li.addEventListener("mousedown", function (ev) {
            ev.preventDefault();
            cfg.pick(it);
          });
          list.appendChild(li);
        });
      });
      if (!any) {
        var li = document.createElement("li");
        li.className = "sug-empty";
        li.textContent = "Nothing found — try a different search.";
        list.appendChild(li);
      }
      input.setAttribute("aria-expanded", any ? "true" : "false");
      list.hidden = false;
    }
    function move(dir) {
      if (!rows.length) return;
      if (active >= 0 && rows[active]) rows[active].li.classList.remove("active");
      active = (active + dir + rows.length) % rows.length;
      var li = rows[active].li;
      li.classList.add("active");
      if (li.scrollIntoView) li.scrollIntoView({ block: "nearest" });
    }
    if (input) {
      input.addEventListener("input", function () {
        if (timer) window.clearTimeout(timer);
        timer = window.setTimeout(query, 150);
      });
      input.addEventListener("keydown", function (ev) {
        if (!list || list.hidden) return;
        if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
          ev.preventDefault();
          move(ev.key === "ArrowDown" ? 1 : -1);
        } else if (ev.key === "Enter") {
          if (active >= 0 && rows[active]) {
            ev.preventDefault();
            cfg.pick(rows[active].it);
          }
        } else if (ev.key === "Escape") {
          close();
        }
      });
      input.addEventListener("blur", function () {
        window.setTimeout(close, 120);
      });
    }
    return { query: query, close: close };
  }

  // Charges can't be fitted as items: set them on a fitted weapon's
  // ammunition select when one takes them, else fall back to the
  // picker so the user can browse.
  function pickCharge(it) {
    var sels = editor.querySelectorAll("select[data-fit-charge]");
    for (var i = 0; i < sels.length; i++) {
      var opts = sels[i].options;
      for (var j = 0; j < opts.length; j++) {
        if (String(opts[j].value) === String(it.id)) {
          sels[i].value = opts[j].value;
          var ev;
          try {
            ev = new Event("change", { bubbles: true });
          } catch (e) {
            ev = document.createEvent("Event");
            ev.initEvent("change", true, true);
          }
          sels[i].dispatchEvent(ev);
          if (modInput) modInput.value = "";
          return;
        }
      }
    }
    if (modInput) modInput.value = it.name;
    openPicker("any");
  }

  var shipSearch = makeSuggester(shipInput, shipList, {
    family: function () { return "ship"; },
    kinds: ["ship"],
    labels: { ship: "Ships" },
    pick: function (it) {
      pushHistory();
      state.shipTypeId = it.id;
      state.items = [];
      state.charges = {};
      if (shipInput) shipInput.value = it.name;
      markDirty();
      simulate();
      syncUndoButtons();
    }
  });
  var modSearch = makeSuggester(modInput, modList, {
    family: function () { return modFamily; },
    dropShips: true,
    pick: function (it) {
      var kind = it.kind || "";
      if (kind === "charge") { pickCharge(it); return; }
      if (kind === "high" || kind === "medium" || kind === "low" ||
          kind === "rig" || kind === "subsystem" || kind === "drone") {
        if (checkRestricted(it.id, it.name)) return;
        addItem(it.id, 1);
        return;
      }
      // Anything else (e.g. cargo items): jump the picker to it.
      if (modInput) modInput.value = it.name;
      openPicker("any");
    }
  });

  if (famChips) {
    famChips.addEventListener("click", function (ev) {
      var b = ev.target && ev.target.closest ? ev.target.closest("button[data-fam]") : null;
      if (!b) return;
      setFamChip(b.getAttribute("data-fam"));
      // The user took the wheel: a chip set by the drone-add flow
      // no longer restores.
      modSearchReturnFam = null;
    });
  }
  function setFamChip(fam) {
    modFamily = fam;
    if (famChips) {
      var btns = famChips.querySelectorAll("button[data-fam]");
      for (var i = 0; i < btns.length; i++) {
        btns[i].classList.toggle("on", btns[i].getAttribute("data-fam") === fam);
      }
    }
    modSearch.query();
  }
  // Drones flow: one search to rule them all. The "+ Add drones"
  // control focuses the module search with the Drones chip
  // pre-selected; drones are picked from the normal results. When
  // the user clears the search or picks another chip, the previous
  // filter state restores automatically.
  var modSearchReturnFam = null;
  function focusModuleSearch(fam) {
    if (modSearchReturnFam === null) modSearchReturnFam = modFamily;
    if (modInput) modInput.value = "";
    setFamChip(fam);
    modSearch.query(true); // empty query lists the whole family
    if (modInput) {
      if (modInput.scrollIntoView) modInput.scrollIntoView({ block: "center" });
      try { modInput.focus({ preventScroll: true }); } catch (e) { modInput.focus(); }
    }
  }
  if (modInput) {
    modInput.addEventListener("input", function () {
      if (modInput.value.trim() === "" && modSearchReturnFam !== null) {
        var back = modSearchReturnFam;
        modSearchReturnFam = null;
        setFamChip(back);
      }
    });
  }
  function filtersChanged() {
    modSearch.query();
    if (picker && !picker.hidden) queryPicker();
  }
  if (fittableChk) fittableChk.addEventListener("change", filtersChanged);
  if (usableChk) usableChk.addEventListener("change", filtersChanged);
  if (metaSel) metaSel.addEventListener("change", filtersChanged);

  // --- your-fits search --------------------------------------------
  // Finds one of the user's saved fits and loads it into the
  // editor. With "Include community fits" checked, other users'
  // public fits appear too (labeled with the author's character
  // name); loading one forks it as the user's own draft.
  var yourfitsInput = document.getElementById("fit-yourfits-search");
  var yourfitsList = document.getElementById("fit-yourfits-suggest");
  var communityChk = document.getElementById("fit-community");
  if (yourfitsInput && yourfitsList) {
    var yfTimer = null;
    var yfRows = [];
    var yfActive = -1;
    function yfClose() {
      yourfitsList.hidden = true;
      yourfitsInput.setAttribute("aria-expanded", "false");
      yfActive = -1;
    }
    function yfLoad(it) {
      yfClose();
      yourfitsInput.value = "";
      if (it.mine) {
        window.location.href = "/fittings/?local=" + it.id + "#fit-editor";
        return;
      }
      fetch("/fittings/fork/", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: it.id })
      }).then(function (resp) {
        return resp.ok ? resp.json() : null;
      }).then(function (data) {
        if (data && data.id) {
          window.location.href = "/fittings/?local=" + data.id + "#fit-editor";
        }
      }).catch(function () {});
    }
    function yfRender() {
      yourfitsList.innerHTML = "";
      yfActive = -1;
      if (!yfRows.length) { yfClose(); return; }
      yfRows.forEach(function (it, i) {
        var li = document.createElement("li");
        li.setAttribute("role", "option");
        li.id = "yf-sug-" + i;
        var name = document.createElement("span");
        name.textContent = it.name || "Unnamed fit";
        li.appendChild(name);
        var lab = document.createElement("span");
        lab.className = "sug-label";
        lab.textContent = (it.shipName || "") + (it.author ? " · by " + it.author : "") + (it.isPublic && !it.mine ? " · public" : "");
        li.appendChild(lab);
        li.addEventListener("mousedown", function (ev) {
          ev.preventDefault();
          yfLoad(it);
        });
        yourfitsList.appendChild(li);
      });
      yourfitsList.hidden = false;
      yourfitsInput.setAttribute("aria-expanded", "true");
    }
    function yfQuery() {
      var q = yourfitsInput.value.trim();
      if (q.length < 2) { yfClose(); return; }
      var url = "/fittings/mine.json?q=" + encodeURIComponent(q) +
        (communityChk && communityChk.checked ? "&community=1" : "");
      fetch(url, { headers: { "Accept": "application/json" } }).then(function (resp) {
        return resp.ok ? resp.json() : [];
      }).then(function (rows) {
        yfRows = rows || [];
        yfRender();
      }).catch(function () {});
    }
    yourfitsInput.addEventListener("input", function () {
      if (yfTimer) clearTimeout(yfTimer);
      yfTimer = setTimeout(yfQuery, 180);
    });
    yourfitsInput.addEventListener("keydown", function (ev) {
      var items = yourfitsList.querySelectorAll("li");
      if (ev.key === "Escape") { yfClose(); return; }
      if (!items.length) return;
      if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
        ev.preventDefault();
        yfActive = ev.key === "ArrowDown"
          ? (yfActive + 1) % items.length
          : (yfActive - 1 + items.length) % items.length;
        for (var i = 0; i < items.length; i++) {
          items[i].classList.toggle("active", i === yfActive);
          items[i].setAttribute("aria-selected", i === yfActive ? "true" : "false");
        }
        yourfitsInput.setAttribute("aria-activedescendant", "yf-sug-" + yfActive);
      } else if (ev.key === "Enter" && yfActive >= 0 && yfRows[yfActive]) {
        ev.preventDefault();
        yfLoad(yfRows[yfActive]);
      }
    });
    yourfitsInput.addEventListener("blur", function () {
      setTimeout(yfClose, 150);
    });
    if (communityChk) communityChk.addEventListener("change", yfQuery);
  }

  // --- your-fits list search + sort (anchor section) ---------------
  (function () {
    var q = document.getElementById("fit-local-q");
    var sortSel = document.getElementById("fit-local-sort");
    var list = document.getElementById("fit-local-list");
    if (!q || !sortSel || !list) return;
    function apply() {
      var term = q.value.trim().toLowerCase();
      var secs = Array.prototype.slice.call(list.querySelectorAll("section[data-fit-name]"));
      secs.forEach(function (s) {
        var name = (s.getAttribute("data-fit-name") || "").toLowerCase();
        var ship = (s.getAttribute("data-fit-ship") || "").toLowerCase();
        var hit = !term || name.indexOf(term) >= 0 || ship.indexOf(term) >= 0;
        s.style.display = hit ? "" : "none";
      });
      var mode = sortSel.value;
      var desc = mode.slice(-4) === "desc";
      var key = mode.split("-")[0];
      secs.sort(function (a, b) {
        var x = a.getAttribute("data-fit-" + key) || "";
        var y = b.getAttribute("data-fit-" + key) || "";
        var c = x.toLowerCase().localeCompare(y.toLowerCase());
        return desc ? -c : c;
      });
      secs.forEach(function (s) { list.appendChild(s); });
    }
    q.addEventListener("input", apply);
    sortSel.addEventListener("change", apply);
  })();

  // --- module picker -------------------------------------------
  function openPicker(family) {
    if (!picker) return;
    picker.hidden = false;
    if (pickerFamily && family) pickerFamily.value = family;
    queryPicker();
    picker.scrollIntoView({ block: "nearest" });
  }
  function closePicker() { if (picker) picker.hidden = true; }
  function queryPicker() {
    if (!pickerList) return;
    var family = pickerFamily ? pickerFamily.value : "any";
    var q = modInput ? modInput.value.trim() : "";
    fetch(pickerURL(family, q), {
      cache: "no-store", headers: { "Accept": "application/json" }
    }).then(function (resp) {
      return resp.ok ? resp.json() : [];
    }).then(function (rows) {
      pickerList.innerHTML = "";
      (applyFittable(rows) || []).forEach(function (it) {
        var li = document.createElement("li");
        li.setAttribute("data-fit-pick", String(it.id));
        // Tap adds (handled by the pointer controller); press-and-
        // drag moves it onto a slot. No mousedown+preventDefault
        // here — that kills the drag gesture before it starts.
        li.appendChild(suggestIcon(it.id));
        var name = document.createElement("span");
        name.textContent = it.name;
        li.appendChild(name);
        if (it.label) {
          var lab = document.createElement("span");
          lab.className = "sug-label";
          lab.textContent = it.label;
          li.appendChild(lab);
        }
        pickerList.appendChild(li);
      });
      if (!rows || rows.length === 0) {
        var li = document.createElement("li");
        li.textContent = "Nothing found — try a different search.";
        pickerList.appendChild(li);
      }
      if (pickerHint) {
        if (q.length >= 2) {
          pickerHint.textContent = "matching \u201c" + q + "\u201d";
          pickerHint.hidden = false;
        } else {
          pickerHint.hidden = true;
        }
      }
    }).catch(function () {});
  }
  if (pickerFamily) pickerFamily.addEventListener("change", queryPicker);
  var pickerClose = document.getElementById("fit-picker-close");
  if (pickerClose) pickerClose.addEventListener("click", closePicker);

  // --- pointer drag and drop --------------------------------------
  // In-game behavior, on mouse AND touch: tap shows the tooltip
  // (never deletes); drag moves between slots. Dropping on an
  // empty highlighted slot moves the module there; on an occupied
  // highlighted slot the two swap. Invalid drops cancel;
  // drag-off-to-delete still removes (the chip minus buttons stay
  // the explicit touch path).
  //
  // Why Pointer Events and not HTML5 drag-and-drop: mobile
  // browsers never fire dragstart/dragover/drop for touch, so the
  // old HTML5 wiring was dead on phones; and the picker's
  // mousedown+preventDefault killed the native drag on desktop
  // too. One pointer controller serves both. Slots carry
  // data-v-family / data-v-index (doc order); the reorder applies
  // against state.items directly.
  var DRAG_PX = 12;
  var dragState = null; // pending or active pointer drag
  var eatNextClick = false; // the synthetic click after a drag-source pointerup
  function dragSourceAt(ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot.filled,[data-fit-pick]") : null;
    return t;
  }
  function slotAtPoint(x, y) {
    var el = document.elementFromPoint ? document.elementFromPoint(x, y) : null;
    var t = el && el.closest ? el.closest(".fit-vslot") : null;
    return t;
  }
  function dragGhost(d) {
    var g = document.createElement("div");
    g.className = "fit-drag-ghost";
    var img = d.el.querySelector("img");
    if (img) {
      var c = document.createElement("img");
      c.src = img.src;
      c.alt = "";
      c.draggable = false;
      g.appendChild(c);
    } else {
      g.textContent = d.el.textContent;
    }
    document.body.appendChild(g);
    return g;
  }
  function moveGhost(d, x, y) {
    if (!d.ghost) return;
    d.ghost.style.left = (x - 22) + "px";
    d.ghost.style.top = (y - 22) + "px";
  }
  function clearDragMarks() {
    var marks = editor.querySelectorAll(".fit-vslot.drop-ok,.fit-vslot.drop-dim");
    for (var i = 0; i < marks.length; i++) {
      marks[i].classList.remove("drop-ok");
      marks[i].classList.remove("drop-dim");
    }
  }
  function highlightForDrag(d) {
    clearDragMarks();
    if (restrictionFor(d.typeID)) return; // lights up nothing
    var slots = editor.querySelectorAll(".fit-vslot");
    for (var i = 0; i < slots.length; i++) {
      var s = slots[i];
      var fam = s.getAttribute("data-v-family") || "";
      if (d.family && fam === d.family) s.classList.add("drop-ok");
      else s.classList.add("drop-dim");
    }
  }
  function startPointerDrag(ev, d) {
    if (restrictionFor(d.typeID)) { dragState = null; return; }
    d.active = true;
    closeTip();
    try { d.el.setPointerCapture(ev.pointerId); } catch (e) { /* not critical */ }
    d.ghost = dragGhost(d);
    moveGhost(d, ev.clientX, ev.clientY);
    d.el.classList.add("drag-src");
    highlightForDrag(d);
    movePointerDrag(ev, d);
  }
  function movePointerDrag(ev, d) {
    moveGhost(d, ev.clientX, ev.clientY);
    var slot = slotAtPoint(ev.clientX, ev.clientY);
    d.dropIndex = -1;
    if (slot) {
      var fam = slot.getAttribute("data-v-family") || "";
      if (d.family && fam === d.family && !restrictionFor(d.typeID)) {
        d.dropIndex = parseInt(slot.getAttribute("data-v-index") || "-1", 10);
        if (!(d.dropIndex >= 0)) d.dropIndex = -1;
      }
    }
  }
  function endPointerDrag(ev, d) {
    if (d.ghost && d.ghost.parentNode) d.ghost.parentNode.removeChild(d.ghost);
    d.el.classList.remove("drag-src");
    clearDragMarks();
    // The synthetic click lands right after pointerup; eat exactly it.
    eatNextClick = true;
    var typeID = d.typeID, fam = d.family, idx = d.dropIndex, fromFit = d.fromFit;
    if (idx >= 0) {
      if (fromFit) {
        pendingFlash = { type: "slot", id: typeID };
        moveItemTo(typeID, fam, idx);
      } else if (!checkRestricted(typeID, "")) {
        addItem(typeID, 1); // arms its own flash
      }
    } else if (fromFit) {
      // Dropped off the display: same as the minus button.
      decItem(typeID);
    }
    // A picker drag to nowhere simply cancels.
  }
  // Tap (press without drag): slot toggles the tooltip, the ammo
  // badge jumps to the charge picker, a picker row adds its item.
  function tapDragSource(ev, d) {
    var ammo = ev.target && ev.target.closest ? ev.target.closest("[data-fit-ammo]") : null;
    if (ammo) { jumpToCharge(ammo.getAttribute("data-fit-ammo")); return; }
    if (d.fromFit) {
      toggleTip(d.el);
      return;
    }
    if (!checkRestricted(d.typeID, "")) addItem(d.typeID, 1);
  }
  editor.addEventListener("pointerdown", function (ev) {
    if (ev.button !== undefined && ev.button > 0) return;
    if (dragState) return;
    var t = dragSourceAt(ev);
    if (!t) return;
    var vslot = t.getAttribute("data-fit-vslot");
    var pick = t.getAttribute("data-fit-pick");
    var typeID = parseInt(vslot || pick || "0", 10) || 0;
    if (!typeID) return;
    dragState = {
      el: t, typeID: typeID, fromFit: !!vslot,
      family: vslot ? (t.getAttribute("data-v-family") || "") : pickerDropFamily(),
      x0: ev.clientX, y0: ev.clientY, pid: ev.pointerId,
      active: false, dropIndex: -1, ghost: null,
      mayScroll: !vslot // picker rows live in a scrollable list
    };
  });
  // The bottom picker's family select is the drop family for picker
  // drags ("any" has no slot family, so those taps only).
  function pickerDropFamily() {
    var v = pickerFamily ? pickerFamily.value : "";
    return v === "any" ? "" : v;
  }
  editor.addEventListener("pointermove", function (ev) {
    var d = dragState;
    if (!d || ev.pointerId !== d.pid) return;
    var dx = ev.clientX - d.x0, dy = ev.clientY - d.y0;
    if (!d.active) {
      if (Math.sqrt(dx * dx + dy * dy) < DRAG_PX) return;
      // A mostly-vertical press on a picker row is a list scroll,
      // not a drag — hand it back to the browser.
      if (d.mayScroll && Math.abs(dy) > Math.abs(dx) * 1.4) { dragState = null; return; }
      startPointerDrag(ev, d);
      return;
    }
    movePointerDrag(ev, d);
  });
  function releasePointerDrag(ev) {
    var d = dragState;
    if (!d || ev.pointerId !== d.pid) return;
    dragState = null;
    // Eat the synthetic click after pointerup so a tap never
    // double-fires (show-then-dismiss) and a drag never leaks a
    // click into the workbench handlers. A fresh press clears it,
    // so a cancelled click can't eat a later real one.
    eatNextClick = true;
    if (d.active) endPointerDrag(ev, d);
    else tapDragSource(ev, d);
  }
  editor.addEventListener("pointerup", releasePointerDrag);
  editor.addEventListener("pointercancel", function (ev) {
    var d = dragState;
    if (!d || ev.pointerId !== d.pid) return;
    dragState = null;
    if (d.active) {
      if (d.ghost && d.ghost.parentNode) d.ghost.parentNode.removeChild(d.ghost);
      d.el.classList.remove("drag-src");
      clearDragMarks();
    }
  });
  // A real drag ends in a click; eat exactly that one so taps and
  // drags never double-fire. Clicks inside the tooltip itself
  // (its close button) always pass through. Any fresh press
  // clears the flag first, so a click that never arrives can't
  // eat a later genuine one.
  editor.addEventListener("pointerdown", function () { eatNextClick = false; }, true);
  editor.addEventListener("click", function (ev) {
    if (eatNextClick) {
      eatNextClick = false;
      if (ev.target && ev.target.closest && ev.target.closest(".fit-tip")) return;
      ev.stopPropagation();
      ev.preventDefault();
    }
  }, true);

  // A long-press must never pop the context menu mid-drag.
  editor.addEventListener("contextmenu", function (ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot,[data-fit-pick]") : null;
    if (t) ev.preventDefault();
  });

  // moveItemTo relocates one instance of typeID within its slot
  // family to doc-order position idx (counting only that
  // family's items). Dropping on an occupied slot swaps the two
  // modules; on an empty slot it moves there (or to the end of
  // the family when the slot is beyond the last item).
  function moveItemTo(typeID, fam, idx) {
    var famIdx = [];
    for (var i = 0; i < state.items.length; i++) {
      if ((familyOfType(state.items[i].typeId) || "") === fam) famIdx.push(i);
    }
    var fromPos = -1;
    for (var p = 0; p < famIdx.length; p++) {
      if (state.items[famIdx[p]].typeId === typeID) { fromPos = p; break; }
    }
    if (fromPos < 0) { addItem(typeID, 1); return; }
    // Clamp the target into the family's item range; an empty
    // slot beyond the last item means "end of family".
    if (idx > famIdx.length) idx = famIdx.length;
    if (idx === fromPos || (idx === famIdx.length && fromPos === famIdx.length - 1)) return;
    var items = state.items.slice();
    var moving = items.splice(famIdx[fromPos], 1)[0];
    // Recompute family positions after removal, then insert at
    // the target position (which may now be the end).
    var after = [];
    for (var j = 0; j < items.length; j++) {
      if ((familyOfType(items[j].typeId) || "") === fam) after.push(j);
    }
    var at = idx < after.length ? after[idx] : items.length;
    items.splice(at, 0, moving);
    setItems(items);
  }
  // familyOfType resolves a type's slot family client-side from
  // the rendered groups (the workbench carries data-fit-group).
  function familyOfType(typeID) {
    var wb = workbench();
    if (!wb) return "";
    var rows = wb.querySelectorAll("[data-fit-rowtype]");
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].getAttribute("data-fit-rowtype") === String(typeID)) {
        var g = rows[i].closest(".fit-group");
        return g ? (g.getAttribute("data-fit-group") || "") : "";
      }
    }
    return "";
  }
  // --- slot tooltips ----------------------------------------------
  // Hover (desktop) or tap (mobile) on a fitted icon shows the
  // quick-look tooltip: name, slot, meta, CPU/PG. Positioned
  // outward from the ship center so it never covers the render.
  // Tap never navigates or deletes. Dismissible via the close
  // button, tapping elsewhere, or Escape.
  var tipEl = null;
  function closeTip() {
    if (tipEl && tipEl.parentNode) tipEl.parentNode.removeChild(tipEl);
    tipEl = null;
  }
  function showTip(slot) {
    closeTip();
    var name = slot.getAttribute("data-tip-name") || "";
    var sub = slot.getAttribute("data-tip-sub") || "";
    var meta = slot.getAttribute("data-tip-meta") || "";
    var cpu = slot.getAttribute("data-tip-cpu") || "";
    var pg = slot.getAttribute("data-tip-pg") || "";
    var stat = slot.getAttribute("data-tip-stat") || "";
    var charge = slot.getAttribute("data-tip-charge") || "";
    var wb = workbench();
    if (!wb || !name) return;
    tipEl = document.createElement("div");
    tipEl.className = "fit-tip";
    tipEl.setAttribute("role", "tooltip");
    var h = document.createElement("div");
    h.className = "fit-tip-name";
    h.textContent = name;
    tipEl.appendChild(h);
    if (sub) {
      var s = document.createElement("div");
      s.className = "fit-tip-sub";
      s.textContent = sub;
      tipEl.appendChild(s);
    }
    var lines = [];
    if (meta) lines.push(meta);
    if (cpu) lines.push(cpu);
    if (pg) lines.push(pg);
    if (stat) lines.push(stat);
    if (lines.length) {
      var d = document.createElement("div");
      d.className = "fit-tip-stats";
      d.textContent = lines.join(" · ");
      tipEl.appendChild(d);
    }
    if (charge) {
      var c = document.createElement("div");
      c.className = "fit-tip-stats";
      c.textContent = charge;
      tipEl.appendChild(c);
    }
    var x = document.createElement("button");
    x.className = "fit-tip-x";
    x.setAttribute("aria-label", "Close");
    x.textContent = "×";
    x.addEventListener("click", function (ev) { ev.stopPropagation(); closeTip(); });
    tipEl.appendChild(x);
    // Anchor to the slot icon: below it when there is room,
    // otherwise above; clamped inside the workbench so it can
    // never cover the page header. The workbench is the offset
    // parent (position: relative).
    tipEl.style.visibility = "hidden";
    wb.appendChild(tipEl);
    var wr = wb.getBoundingClientRect();
    var sr = slot.getBoundingClientRect();
    var tw = tipEl.offsetWidth || 190, th = tipEl.offsetHeight || 110;
    var cx = sr.left - wr.left + sr.width / 2;
    var below = sr.bottom - wr.top + 10;
    var above = sr.top - wr.top - th - 10;
    var ty = (below + th <= wr.height) ? below : Math.max(8, above);
    var tx = cx - tw / 2;
    if (tx < 8) tx = 8;
    if (tx + tw > wr.width - 8) tx = wr.width - tw - 8;
    if (tx < 8) tx = 8;
    tipEl.style.left = Math.round(tx) + "px";
    tipEl.style.top = Math.round(ty) + "px";
    tipEl.style.visibility = "";
  }
  // Jumps to a weapon's ammunition select (from its slot badge).
  function jumpToCharge(weaponID) {
    var sel = editor.querySelector('select[data-fit-charge="' + weaponID + '"]');
    if (!sel) return;
    closeTip();
    if (sel.scrollIntoView) sel.scrollIntoView({ block: "center" });
    try { sel.focus({ preventScroll: true }); } catch (e) { try { sel.focus(); } catch (e2) {} }
    sel.classList.add("flash");
    window.setTimeout(function () { sel.classList.remove("flash"); }, 750);
  }
  var tipHoverSlot = null;
  editor.addEventListener("mouseover", function (ev) {
    if (dragState && dragState.active) return; // no tooltips mid-drag
    var slot = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot[data-tip-name]") : null;
    if (slot && slot !== tipHoverSlot) {
      tipHoverSlot = slot;
      showTip(slot);
    }
  });
  editor.addEventListener("mouseout", function (ev) {
    var slot = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot[data-tip-name]") : null;
    if (slot && slot === tipHoverSlot) {
      tipHoverSlot = null;
      closeTip();
    }
  });
  // Taps on slots are handled by the pointer controller (tap shows
  // the tooltip); a tap anywhere else dismisses it. Never deletes
  // or navigates. Keyboard: Enter/Space on a focused slot toggles
  // its tooltip the same way a tap does.
  function toggleTip(slot) {
    var key = slot.getAttribute("data-v-index") + ":" + (slot.getAttribute("data-v-family") || "");
    if (tipEl && tipEl.getAttribute("data-for") === key) {
      closeTip();
    } else {
      showTip(slot);
      if (tipEl) tipEl.setAttribute("data-for", key);
    }
  }
  editor.addEventListener("keydown", function (ev) {
    if (ev.key !== "Enter" && ev.key !== " ") return;
    var slot = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot.filled") : null;
    if (!slot) return;
    ev.preventDefault();
    toggleTip(slot);
  });
  editor.addEventListener("click", function (ev) {
    if (tipEl && !(ev.target && ev.target.closest && ev.target.closest(".fit-tip"))) closeTip();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") closeTip();
  });

  // --- subsystem UI gating ----------------------------------------
  // The subsystem ring/group/search-family only appear when the
  // hull supports subsystems (data-fit-subsystems on .fit-wb).
  function syncSubsystemUI() {
    var wb = workbench();
    var ok = wb && wb.getAttribute("data-fit-subsystems") === "1";
    var chip = document.querySelector('#fit-famchips button[data-fam="subsystem"]');
    if (chip) chip.hidden = !ok;
    var opt = document.querySelector('#fit-picker-family option[value="subsystem"]');
    if (opt) opt.hidden = !ok;
  }

  // --- save / export --------------------------------------------
  var saveEveBtn = document.getElementById("fit-save-eve");
  function fitNotice(msg, isErr, relink) {
    var old = editor.querySelector(".fit-notice");
    if (old) old.remove();
    var p = document.createElement("p");
    p.className = "fit-notice" + (isErr ? " err" : "");
    p.textContent = msg;
    if (relink) {
      p.appendChild(document.createTextNode(" "));
      var a = document.createElement("a");
      a.href = "/auth/eve";
      a.textContent = "Sign in again";
      p.appendChild(a);
    }
    var head = editor.querySelector(".fit-head");
    if (head && head.parentNode) head.parentNode.insertBefore(p, head.nextSibling);
    else editor.insertBefore(p, editor.firstChild);
    p.scrollIntoView({ block: "nearest" });
  }
  if (saveEveBtn) {
    saveEveBtn.addEventListener("click", function () {
      if (!state.shipTypeId) return;
      var charID = parseInt(saveEveBtn.getAttribute("data-fit-eve-char") || "0", 10) || 0;
      if (!charID) return;
      state.name = nameInput ? nameInput.value : state.name;
      saveEveBtn.disabled = true;
      fitNotice("Saving to EVE…", false, false);
      fetch("/fittings/save-to-eve/", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Accept": "application/json" },
        body: JSON.stringify({
          characterId: charID,
          name: state.name,
          shipTypeId: state.shipTypeId,
          items: state.items,
          charges: state.charges
        })
      }).then(function (resp) {
        return resp.ok ? resp.json() : null;
      }).then(function (row) {
        saveEveBtn.disabled = false;
        if (row && row.ok) {
          fitNotice("Saved \u201c" + row.name + "\u201d to EVE — loading your in-game fitting…", false, false);
          window.setTimeout(function () {
            window.location.href = "/fittings/?character=" + row.characterId + "&esi=" + row.fittingId + "#fit-editor";
          }, 1200);
        } else if (row) {
          fitNotice(row.error || "EVE refused the fitting.", true, !!row.relink);
        } else {
          fitNotice("Could not reach the server.", true, false);
        }
      }).catch(function () {
        saveEveBtn.disabled = false;
        fitNotice("Could not reach the server.", true, false);
      });
    });
  }
  if (saveBtn) {
    saveBtn.addEventListener("click", function () {
      if (!state.shipTypeId) return;
      state.name = nameInput ? nameInput.value : state.name;
      var pubChk = document.getElementById("fit-public");
      var payload = {
        id: parseInt(saveBtn.getAttribute("data-fit-local-id") || "0", 10) || 0,
        name: state.name,
        description: descInput ? descInput.value : "",
        tags: tagsInput ? tagsInput.value : "",
        isPublic: !!(pubChk && pubChk.checked),
        promote: true,
        fit: {
          name: state.name,
          shipTypeId: state.shipTypeId,
          items: state.items,
          charges: state.charges
        }
      };
      fetch("/fittings/save/", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Accept": "application/json" },
        body: JSON.stringify(payload)
      }).then(function (resp) {
        return resp.ok ? resp.json() : null;
      }).then(function (row) {
        if (row && row.id) {
          window.location.href = "/fittings/?local=" + row.id + "#fit-editor";
        }
      }).catch(function () {});
    });
  }
  if (exportBtn) {
    exportBtn.addEventListener("click", function () {
      state.name = nameInput ? nameInput.value : state.name;
      fetch("/fittings/export/", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Accept": "text/plain" },
        body: JSON.stringify({
          name: state.name,
          shipTypeId: state.shipTypeId,
          items: state.items,
          charges: state.charges,
          pilot: pilotID()
        })
      }).then(function (resp) {
        return resp.ok ? resp.text() : "";
      }).then(function (text) {
        if (exportOut) {
          exportOut.value = text;
          exportOut.focus();
          exportOut.select();
        }
      }).catch(function () {});
    });
  }
})();

// --- ESI fittings list: client-side search + sort ----------------
// (lists are small; the foldable sections now start collapsed)
(function () {
  var q = document.getElementById("fit-esi-q");
  var sortSel = document.getElementById("fit-esi-sort");
  var list = document.getElementById("fit-esi-list");
  if (!q || !sortSel || !list) return;
  function apply() {
    var term = q.value.trim().toLowerCase();
    var secs = Array.prototype.slice.call(list.querySelectorAll(":scope > section.foldable"));
    secs.forEach(function (s) {
      var name = (s.getAttribute("data-fit-name") || "").toLowerCase();
      var ship = (s.getAttribute("data-fit-ship") || "").toLowerCase();
      var hit = !term || name.indexOf(term) >= 0 || ship.indexOf(term) >= 0;
      s.style.display = hit ? "" : "none";
    });
    var mode = sortSel.value;
    var desc = mode.slice(-4) === "desc";
    var byShip = mode.indexOf("ship") === 0;
    secs.sort(function (a, b) {
      var x = (byShip ? a.getAttribute("data-fit-ship") : a.getAttribute("data-fit-name")) || "";
      var y = (byShip ? b.getAttribute("data-fit-ship") : b.getAttribute("data-fit-name")) || "";
      var c = x.toLowerCase().localeCompare(y.toLowerCase());
      return desc ? -c : c;
    });
    secs.forEach(function (s) { list.appendChild(s); });
  }
  q.addEventListener("input", apply);
  sortSel.addEventListener("change", apply);
})();
