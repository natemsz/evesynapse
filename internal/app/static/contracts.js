// Contract search page behavior: the specialized-specs
// drawer slot, region chips, buyout cap chips, slider pairs,
// ISK input hints, the results accordion, and the notepad
// copy buttons. Plain form semantics underneath throughout:
// every control is a real form field, and everything the JS
// arranges still submits through the one search form.
(function () {
  "use strict";

  function applyMeters(root) {
    root.querySelectorAll(".meter > span[data-w]").forEach(function (el) {
      el.style.width = el.getAttribute("data-w") + "%";
    });
  }

  // ---- copy buttons ------------------------------------------------------
  // Document-level delegation, bound once: it serves the
  // current DOM and any swapped-in DOM alike.
  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-copy]");
    if (!btn) return;
    e.preventDefault();
    e.stopPropagation();
    var text = btn.getAttribute("data-copy");
    function done() {
      if (btn.dataset.copyBusy) return;
      btn.dataset.copyBusy = "1";
      var original = btn.textContent;
      btn.textContent = btn.classList.contains("cs-copy") && !btn.classList.contains("btn") ? "✓" : "Copied Link!";
      btn.classList.add("copied");
      setTimeout(function () {
        btn.textContent = original;
        btn.classList.remove("copied");
        delete btn.dataset.copyBusy;
      }, 2000);
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done, function () { fallback(); });
    } else {
      fallback();
    }
    function fallback() {
      var ta = document.createElement("textarea");
      ta.value = text;
      ta.style.position = "fixed";
      ta.style.opacity = "0";
      document.body.appendChild(ta);
      ta.select();
      try { document.execCommand("copy"); } catch (err) { /* clipboard unavailable */ }
      ta.remove();
      done();
    }
  });

  // ---- page wiring ---------------------------------------------------
  // Everything below binds to the search page's DOM. An
  // in-page swap replaces that DOM without re-running this
  // script, so the wiring re-runs when the shell announces
  // swapped-in content; the form-identity guard (the same
  // pattern pi.js boots with) keeps a repeat call for the
  // same DOM a no-op, and a page without the search form
  // (the contract detail page loads this script too) wires
  // nothing.
  var wiredForm = null;
  function init() {
  var form = document.getElementById("cs-form");
  if (!form || form === wiredForm) return;
  wiredForm = form;

  // ---- drawer slot -------------------------------------------------
  // One slot, three spec sets. The tabs control visibility
  // only: hidden drawers keep their values and still apply
  // on submit. Clicking the open tab closes the slot. A
  // hidden spec field carries the open drawer across the
  // submit, like the mockup's tab state.
  var specField = null;
  if (form) {
    specField = document.createElement("input");
    specField.type = "hidden";
    specField.name = "spec";
    form.appendChild(specField);
  }
  var tabs = Array.prototype.slice.call(document.querySelectorAll("[data-drawer-tab]"));
  var panels = {};
  document.querySelectorAll("[data-drawer-panel]").forEach(function (p) {
    panels[p.getAttribute("data-drawer-panel")] = p;
  });
  function openDrawer(name) {
    tabs.forEach(function (t) {
      var on = t.getAttribute("data-drawer-tab") === name;
      t.classList.toggle("on", on);
      t.setAttribute("aria-expanded", on ? "true" : "false");
    });
    Object.keys(panels).forEach(function (k) {
      panels[k].hidden = k !== name;
    });
    if (specField) {
      if (name) {
        specField.value = name;
      } else {
        specField.removeAttribute("value");
        specField.value = "";
      }
    }
  }
  tabs.forEach(function (t) {
    t.addEventListener("click", function () {
      var name = t.getAttribute("data-drawer-tab");
      var isOpen = panels[name] && !panels[name].hidden;
      openDrawer(isOpen ? "" : name);
    });
  });
  // Seed the spec field from the server-rendered open drawer.
  (function () {
    var open = tabs.filter(function (t) { return t.classList.contains("on"); })[0];
    if (open && specField) specField.value = open.getAttribute("data-drawer-tab");
  })();

  // Per-drawer resets clear only their own set.
  document.querySelectorAll("[data-reset-drawer]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var panel = panels[btn.getAttribute("data-reset-drawer")];
      if (!panel) return;
      panel.querySelectorAll("input, select, textarea").forEach(function (el) {
        if (el.type === "checkbox" || el.type === "radio") {
          el.checked = false;
        } else if (el.tagName === "SELECT") {
          el.selectedIndex = 0;
        } else if (el.type === "range") {
          el.value = "0";
        } else {
          el.value = "";
        }
      });
    });
  });

  // ---- auto-submit toggles -----------------------------------------
  // Type pills and the deals switch filter the moment they
  // flip, like the mockup's live filter state.
  if (form) {
    form.querySelectorAll(".cs-auto").forEach(function (el) {
      el.addEventListener("change", function () {
        form.requestSubmit();
      });
    });
  }

  // ---- regions -------------------------------------------------------
  var regionBoxes = Array.prototype.slice.call(document.querySelectorAll(".cs-region-cb"));
  var chipsBox = document.getElementById("cs-region-chips");
  function renderRegionChips() {
    if (!chipsBox) return;
    chipsBox.innerHTML = "";
    var selected = regionBoxes.filter(function (b) { return b.checked; });
    if (!selected.length) {
      var all = document.createElement("span");
      all.className = "cs-chips-all";
      all.textContent = "All regions";
      chipsBox.appendChild(all);
      return;
    }
    selected.forEach(function (b) {
      var chip = document.createElement("span");
      chip.className = "cs-chip";
      chip.textContent = b.getAttribute("data-region-name") + " ";
      var x = document.createElement("button");
      x.type = "button";
      x.className = "cs-chip-x";
      x.setAttribute("aria-label", "Remove region " + b.getAttribute("data-region-name"));
      x.textContent = "×";
      x.addEventListener("click", function () {
        b.checked = false;
        renderRegionChips();
      });
      chip.appendChild(x);
      chipsBox.appendChild(chip);
    });
  }
  regionBoxes.forEach(function (b) {
    b.addEventListener("change", renderRegionChips);
  });
  renderRegionChips();

  // The region dropdown is a search as well: typing in its
  // search field narrows the checkbox rows by name.
  var regionSearch = document.querySelector(".cs-region-search");
  if (regionSearch) {
    regionSearch.addEventListener("input", function () {
      var q = regionSearch.value.trim().toLowerCase();
      regionBoxes.forEach(function (b) {
        var row = b.closest(".cs-region-row");
        if (!row) return;
        var name = (b.getAttribute("data-region-name") || "").toLowerCase();
        row.style.display = !q || name.indexOf(q) >= 0 ? "" : "none";
      });
    });
  }

  var hubNames = ["The Forge", "Domain", "Sinq Laison", "Heimatar"];
  document.querySelectorAll("[data-region-quick]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var mode = btn.getAttribute("data-region-quick");
      regionBoxes.forEach(function (b) {
        var name = b.getAttribute("data-region-name");
        if (mode === "jita") b.checked = name === "The Forge";
        else if (mode === "hubs") b.checked = hubNames.indexOf(name) >= 0;
        else b.checked = false;
      });
      renderRegionChips();
    });
  });

  // ---- buyout cap chips ----------------------------------------------
  document.querySelectorAll("[data-buyout-cap]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var input = form && form.querySelector('input[name="buyout_max"]');
      if (!input) return;
      var v = parseInt(btn.getAttribute("data-buyout-cap"), 10);
      input.value = v >= 1e9 ? v / 1e9 + "b" : v / 1e6 + "m";
      input.dispatchEvent(new Event("input", { bubbles: true }));
      input.focus();
    });
  });

  // ---- ISK input hints -------------------------------------------------
  function parseISK(raw) {
    if (!raw) return null;
    var s = String(raw).replace(/[,\s]/g, "").toLowerCase();
    var mult = 1;
    if (s.endsWith("k")) { mult = 1e3; s = s.slice(0, -1); }
    else if (s.endsWith("m")) { mult = 1e6; s = s.slice(0, -1); }
    else if (s.endsWith("b")) { mult = 1e9; s = s.slice(0, -1); }
    if (!s || isNaN(Number(s))) return null;
    return Number(s) * mult;
  }
  document.querySelectorAll("input[data-isk]").forEach(function (input) {
    var hint = document.createElement("span");
    hint.className = "cs-isk-hint";
    input.insertAdjacentElement("afterend", hint);
    function update() {
      var v = parseISK(input.value);
      hint.textContent = v === null ? "" : "= " + Math.round(v).toLocaleString("en-US") + " ISK";
    }
    input.addEventListener("input", update);
    update();
  });

  // ---- slider pairs ----------------------------------------------------
  document.querySelectorAll("input[data-slider-for]").forEach(function (slider) {
    var num = document.getElementById(slider.getAttribute("data-slider-for"));
    if (!num) return;
    slider.addEventListener("input", function () {
      num.value = slider.value;
    });
    num.addEventListener("input", function () {
      slider.value = num.value === "" ? "0" : num.value;
    });
  });

  // ---- results accordion -------------------------------------------------
  // Exactly one row open at a time; clicking the open row
  // closes it. Panels load lazily, one contract per fetch,
  // and are cached in the row for the session.
  var openRow = null;
  function closeRow(row) {
    var slot = row.querySelector(".cs-panel-slot");
    if (slot) slot.hidden = true;
    row.classList.remove("open");
    row.setAttribute("aria-expanded", "false");
    if (openRow === row) openRow = null;
  }
  function openRowPanel(row) {
    var slot = row.querySelector(".cs-panel-slot");
    if (!slot) return;
    if (openRow && openRow !== row) closeRow(openRow);
    openRow = row;
    row.classList.add("open");
    row.setAttribute("aria-expanded", "true");
    if (slot.dataset.loaded) {
      slot.hidden = false;
      return;
    }
    slot.hidden = false;
    slot.innerHTML = '<p class="muted cs-panel-loading">Loading appraisal…</p>';
    fetch("/contracts/search/" + row.getAttribute("data-contract") + "/panel", {
      headers: { "X-Requested-With": "fetch" }
    }).then(function (resp) {
      if (!resp.ok) throw new Error("panel failed");
      return resp.text();
    }).then(function (html) {
      slot.innerHTML = html;
      slot.dataset.loaded = "1";
      applyMeters(slot);
    }).catch(function () {
      slot.innerHTML = '<p class="muted cs-panel-loading">Could not load the appraisal — the contract page carries the same detail.</p>';
    });
  }
  document.querySelectorAll(".cs-row").forEach(function (row) {
    row.addEventListener("click", function (e) {
      if (e.target.closest(".cs-panel-slot, a, button, input, select, textarea, label")) return;
      if (openRow === row) closeRow(row);
      else openRowPanel(row);
    });
    row.addEventListener("keydown", function (e) {
      if (e.key !== "Enter" && e.key !== " ") return;
      if (e.target !== row) return;
      e.preventDefault();
      if (openRow === row) closeRow(row);
      else openRowPanel(row);
    });
  });
  }
  init();
  document.addEventListener("evesynapse:content-ready", init);
})();
