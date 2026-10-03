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

    // The card the pointer is currently asking to displace:
    // first sibling (row-major) whose top is below the pointer,
    // or same-row sibling whose center is right of it.
    function dragAfter(x, y) {
      var sibs = grid.querySelectorAll(".card[data-widget]:not(.dragging)");
      for (var i = 0; i < sibs.length; i++) {
        var r = sibs[i].getBoundingClientRect();
        if (y < r.top || (y <= r.bottom && x < r.left + r.width / 2)) return sibs[i];
      }
      return null;
    }

    function startDrag(ev, card, handle) {
      if (ev.pointerType === "mouse" && ev.button !== 0) return;
      ev.preventDefault();
      var startX = ev.clientX, startY = ev.clientY;
      var moved = false;
      card.classList.add("dragging");
      if (handle.setPointerCapture) {
        try { handle.setPointerCapture(ev.pointerId); } catch (err) { /* capture is a nicety */ }
      }

      // The card's layout position with the follow-the-pointer
      // transform momentarily cleared, so a DOM reorder can
      // compensate its start point and the card never jumps.
      function layoutRect() {
        var t = card.style.transform;
        card.style.transform = "";
        var r = card.getBoundingClientRect();
        card.style.transform = t;
        return r;
      }

      function onMove(e) {
        var dx = e.clientX - startX, dy = e.clientY - startY;
        if (!moved && Math.abs(dx) + Math.abs(dy) < 3) return;
        moved = true;
        card.style.transform = "translate(" + dx + "px," + dy + "px)";
        var before = layoutRect();
        var after = dragAfter(e.clientX, e.clientY);
        if (after) grid.insertBefore(card, after);
        else grid.appendChild(card);
        var now = layoutRect();
        startX += now.left - before.left;
        startY += now.top - before.top;
        card.style.transform = "translate(" + (e.clientX - startX) + "px," + (e.clientY - startY) + "px)";
      }

      function onUp() {
        handle.removeEventListener("pointermove", onMove);
        handle.removeEventListener("pointerup", onUp);
        handle.removeEventListener("pointercancel", onUp);
        card.classList.remove("dragging");
        card.style.transform = "";
        if (moved) {
          postLayout({ action: "order", ids: widgetIDs().join(",") });
        }
      }

      handle.addEventListener("pointermove", onMove);
      handle.addEventListener("pointerup", onUp);
      handle.addEventListener("pointercancel", onUp);
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
