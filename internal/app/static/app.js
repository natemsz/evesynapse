// EveSynapse progressive enhancements. Everything here only ever
// *adds* behavior: with JavaScript disabled the pages render and
// work exactly as before (the nav is <details>-driven, the market
// form submits, panels are simply unfolded).
(function () {
  "use strict";

  // --- Collapsible panels -------------------------------------
  // The base template wraps every page in one .panel; grid pages
  // add .card widgets inside it. Each gets a fold button in its
  // header area (the panel's h1 bar, a card's first heading).
  // Content starts open; the button folds it away and back.
  function addFold(container, heading) {
    if (!container || !heading) return;
    var btn = document.createElement("button");
    btn.type = "button";
    btn.className = "foldbtn";
    btn.textContent = "–"; // en dash: fold; becomes + when folded
    btn.setAttribute("aria-expanded", "true");
    btn.setAttribute("aria-label", "Collapse section");
    btn.addEventListener("click", function () {
      var folded = container.classList.toggle("folded");
      btn.textContent = folded ? "+" : "–";
      btn.setAttribute("aria-expanded", folded ? "false" : "true");
      btn.setAttribute("aria-label", folded ? "Expand section" : "Collapse section");
    });
    heading.appendChild(btn);
    container.classList.add("has-fold");
  }

  var panel = document.querySelector("main > .panel");
  if (panel) addFold(panel, panel.querySelector(":scope > h1"));

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
