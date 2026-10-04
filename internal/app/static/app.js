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
    var lastQuery = "";

    function close() {
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
      if (!it) return;
      close();
      onPick(it, input);
    }

    function query(q) {
      fetch(endpoint(q), {
        headers: { "Accept": "application/json" }
      }).then(function (resp) {
        return resp.ok ? resp.json() : [];
      }).then(function (rows) {
        if (input.value.trim() !== q) return; // typed past this result
        items = Array.isArray(rows) ? rows : [];
        selected = items.length ? 0 : -1;
        paint();
      }).catch(function () { close(); });
    }

    input.addEventListener("input", function () {
      var q = input.value.trim();
      if (q === lastQuery) return;
      lastQuery = q;
      if (timer) window.clearTimeout(timer);
      if (q.length < 2) { close(); return; }
      timer = window.setTimeout(function () { query(q); }, 150);
    });

    input.addEventListener("keydown", function (ev) {
      if (list.hidden) return;
      if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
        ev.preventDefault();
        if (!items.length) return;
        selected = ev.key === "ArrowDown"
          ? (selected + 1) % items.length
          : (selected - 1 + items.length) % items.length;
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

  // Banner global search: items, your characters, and pilots the
  // app already knows, each to its own page.
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
// fragment settles, and gives up quietly after ~40 tries,
// leaving the pending copy standing.
(function () {
  var regions = document.querySelectorAll("[data-live-region]");
  if (!regions.length || !window.fetch) return;

  function arm(region) {
    var url = region.getAttribute("data-poll-url");
    if (!url) return;
    var attempts = 0;
    var timer = window.setInterval(function () {
      attempts += 1;
      if (attempts > 40) {
        window.clearInterval(timer);
        return;
      }
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
          window.clearInterval(timer);
          window.location.reload();
          return;
        }
        var state = fresh.getAttribute("data-poll-state");
        if (state !== "pending" && state !== "loading") {
          window.clearInterval(timer);
          region.outerHTML = html;
        }
      }).catch(function () {
        // A failed poll is not news; the next tick retries
        // until the attempt cap.
      });
    }, 1500);
  }

  for (var i = 0; i < regions.length; i++) {
    arm(regions[i]);
  }
})();

// Page-sync indicator: while this page still has data on the
// way (names, descriptions, price history it asked for), the
// top banner shows a small ember ring with the count. It polls
// a cache-only status endpoint — never the data itself — and
// hides the moment the page has everything. Background sync in
// general is not reported here.
(function () {
  var indicator = document.getElementById("page-sync-indicator");
  if (!indicator || !window.fetch) return;
  var label = indicator.querySelector("[data-page-sync-label]");
  var page = window.location.pathname + window.location.search;
  var attempts = 0;
  var maxAttempts = 80; // ~2 minutes at 1.5s, then leave the page as-is

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

  var timer = window.setInterval(function () {
    attempts += 1;
    if (attempts > maxAttempts) {
      window.clearInterval(timer);
      indicator.hidden = true;
      return;
    }
    window.fetch("/sync/page-status?page=" + encodeURIComponent(page), {
      credentials: "same-origin",
      headers: { "X-Requested-With": "XMLHttpRequest" }
    }).then(function (resp) {
      return resp.json();
    }).then(function (status) {
      var pending = status && typeof status.pending === "number" ? status.pending : 0;
      if (pending > 0 || unresolvedRegions() > 0) {
        show(pending);
      } else {
        indicator.hidden = true;
        window.clearInterval(timer);
      }
    }).catch(function () {
      // A failed status poll is not news; the next tick retries
      // until the attempt cap.
    });
  }, 1500);
})();
