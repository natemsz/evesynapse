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

  // --- Home customize: drag, remove, add module -----------------
  // Layered over the plain customize forms, never replacing them:
  // this runs only on the customize view (the data-customize grid)
  // and stores through the same /home/layout endpoint — a drag
  // saves once, on drop, and the server stays the normalizer of
  // whatever order it is handed. Without JS the up/down +
  // add/remove forms in the module list do the same job.
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

    // Give one card its drag handle and × button. Cards added
    // from the pop-up pass through here too.
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

      var remove = document.createElement("button");
      remove.type = "button";
      remove.className = "cardremove";
      remove.textContent = "×";
      remove.setAttribute("aria-label", "Remove " + title + " from your home");
      remove.addEventListener("click", function () {
        remove.disabled = true;
        postLayout({ action: "toggle", widget: id }).then(function (ok) {
          if (ok) card.remove();
          else remove.disabled = false;
        });
      });
      card.appendChild(remove);
    }

    // --- dragging ---------------------------------------------
    // The model: lifting a card swaps a same-size placeholder
    // into its grid slot, so the layout never collapses mid-drag;
    // the card itself goes position:fixed and follows the pointer,
    // fully opaque with its real panel colors. When the pointer
    // crosses a sibling boundary the placeholder hops slots and
    // the displaced siblings FLIP-animate into their new cells
    // instead of teleporting. Holding the card near a viewport
    // edge auto-scrolls the page, so long moves on a short screen
    // don't need grab-drop-grab. On drop the card animates into
    // the placeholder's slot and swaps places with it; a
    // cancelled drag or a failed save restores the pre-drag order
    // from a DOM snapshot taken at lift-off.
    var reducedMotion = window.matchMedia &&
      window.matchMedia("(prefers-reduced-motion: reduce)").matches;

    function siblingCards() {
      return grid.querySelectorAll(".card[data-widget]:not(.dragging)");
    }

    // FLIP-animate the sibling cards around a DOM mutation:
    // measure, mutate, then invert each moved card back onto its
    // old spot and let a transition play it into the new one.
    // Cleanup removes both inline properties, on transitionend
    // or a fallback timer, whichever comes first.
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
          if (!dx && !dy) return;
          el.style.transform = "translate(" + dx + "px," + dy + "px)";
          el.style.transition = "none";
          // Reading offsetWidth commits the inverted state
          // before the transition is armed.
          void el.offsetWidth;
          el.style.transition = "transform 180ms ease";
          el.style.transform = "";
          var done = function () {
            el.style.transition = "";
            el.style.transform = "";
            el.removeEventListener("transitionend", done);
          };
          el.addEventListener("transitionend", done);
          window.setTimeout(done, 260);
        })(cards[j], first[j]);
      }
    }

    // The sibling card the placeholder should sit in front of for
    // a pointer at (x, y), row-major: a card counts as passed
    // once the pointer is below its middle, or right of its
    // middle within its row. Null: the placeholder goes last.
    function insertionTarget(x, y) {
      var cards = siblingCards();
      for (var i = 0; i < cards.length; i++) {
        var r = cards[i].getBoundingClientRect();
        if (y < r.top + r.height / 2) return cards[i];
        if (y <= r.bottom && x < r.left + r.width / 2) return cards[i];
      }
      return null;
    }

    function startDrag(ev, card, handle) {
      if (ev.pointerType === "mouse" && ev.button !== 0) return;
      ev.preventDefault();

      var startX = ev.clientX, startY = ev.clientY;
      var lastX = startX, lastY = startY;
      var active = false;   // pointer passed the grab threshold
      var lifted = false;   // card out of flow, placeholder in
      var placeholder = null;
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
        var rect = card.getBoundingClientRect();
        grabDX = lastX - rect.left;
        grabDY = lastY - rect.top;
        // The placeholder takes the card's exact footprint —
        // same span class, same height — so the grid underneath
        // is the final layout, undisturbed, for the whole drag.
        placeholder = document.createElement("section");
        placeholder.className = "card drag-placeholder" +
          (card.classList.contains("spanall") ? " spanall" : "");
        placeholder.style.height = rect.height + "px";
        placeholder.setAttribute("aria-hidden", "true");
        grid.insertBefore(placeholder, card.nextSibling);
        card.classList.add("dragging");
        card.style.width = rect.width + "px";
        card.style.height = rect.height + "px";
        card.style.left = rect.left + "px";
        card.style.top = rect.top + "px";
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

      // Hop the placeholder only when the pointer has actually
      // crossed into a different slot; the siblings FLIP around
      // the hop, so nothing under the finger teleports.
      function movePlaceholder() {
        var target = insertionTarget(lastX, lastY);
        var next = placeholder.nextElementSibling;
        while (next && (next === card || !next.classList.contains("card"))) {
          next = next.nextElementSibling;
        }
        if (next === target) return; // already in the asked-for slot
        flipSiblings(function () {
          if (target) grid.insertBefore(placeholder, target);
          else grid.appendChild(placeholder);
        });
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
        // Settle: animate the card into the placeholder's slot,
        // swap the two in the DOM, then save. A failed save
        // restores the pre-drag arrangement.
        var slot = placeholder.getBoundingClientRect();
        card.classList.add("drag-settle");
        card.style.left = slot.left + "px";
        card.style.top = slot.top + "px";
        card.style.transform = "scale(1)";
        var swapIn = function () {
          grid.replaceChild(card, placeholder);
          placeholder = null;
          cleanup();
          postLayout({ action: "order", ids: widgetIDs().join(",") }).then(function (ok) {
            if (!ok) restoreOrder();
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
          grid.appendChild(node);
          addFold(node, node.querySelector(":scope > h2, :scope > h3"));
          enhanceCard(node);
          closeModal();
        }, function () { window.location.reload(); });
      });
    }

    var cards = grid.querySelectorAll(".card[data-widget]");
    for (var i = 0; i < cards.length; i++) enhanceCard(cards[i]);

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

  // --- Market search suggestions --------------------------------
  var input = document.getElementById("market-q");
  var list = document.getElementById("market-suggest");
  if (!input || !list || !window.fetch) return;

  var form = document.getElementById("market-search");
  var regionField = document.getElementById("market-region");
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
      li.id = "market-suggest-" + idx;
      li.setAttribute("role", "option");
      li.textContent = it.name;
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
      list.appendChild(li);
    });
    list.hidden = items.length === 0;
    input.setAttribute("aria-expanded", items.length ? "true" : "false");
  }

  function pick(idx) {
    var it = items[idx];
    if (!it) return;
    var region = regionField ? regionField.value : "";
    var url = "/market/?type=" + encodeURIComponent(it.id);
    if (region) url += "&region=" + encodeURIComponent(region);
    window.location.href = url;
  }

  function query(q) {
    fetch("/market/suggest?q=" + encodeURIComponent(q), {
      headers: { "Accept": "application/json" }
    }).then(function (resp) {
      return resp.ok ? resp.json() : [];
    }).then(function (rows) {
      if (input.value.trim() !== q) return; // typed past this result
      items = Array.isArray(rows) ? rows.slice(0, 10) : [];
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
  if (form) form.addEventListener("submit", function () { close(); });
})();
