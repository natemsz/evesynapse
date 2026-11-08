// Contract search page behavior: the specialized-specs
// drawer slot, region chips, buyout cap chips, slider pairs,
// ISK input hints, the results accordion, and the notepad
// copy buttons. Plain form semantics underneath throughout:
// every control is a real form field, and everything the JS
// arranges still submits through the one search form.
(function () {
  "use strict";

  function applyMeters(root) {
    root.querySelectorAll(".meter > span[data-w], .cs-telbar > span[data-w]").forEach(function (el) {
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
  // ---- abyssal telemetry picker ----------------------------------
  // A contract with several abyssal lines carries one telemetry
  // card per line (all shown without JS). With JS one shows at a
  // time: the first, until a line's roll tag picks another. The
  // drawer and the contract page share it (data-ab-scope).
  function showAbTel(scope, id) {
    scope.querySelectorAll("[data-ab-tel]").forEach(function (c) {
      c.hidden = c.getAttribute("data-ab-tel") !== id;
    });
    scope.querySelectorAll("[data-ab-pick]").forEach(function (b) {
      var on = b.getAttribute("data-ab-pick") === id;
      b.setAttribute("aria-pressed", on ? "true" : "false");
      var line = b.closest(".cs-aline");
      if (line) line.classList.toggle("ab-on", on);
    });
  }
  function initAbPick(root) {
    var cards = root ? root.querySelectorAll("[data-ab-tel]") : [];
    if (cards.length < 2) return;
    showAbTel(root, cards[0].getAttribute("data-ab-tel"));
  }
  document.addEventListener("click", function (e) {
    var b = e.target.closest ? e.target.closest("[data-ab-pick]") : null;
    if (!b) return;
    var scope = b.closest(".cs-panel, [data-ab-scope]");
    if (!scope) return;
    e.preventDefault();
    showAbTel(scope, b.getAttribute("data-ab-pick"));
  });
  document.querySelectorAll("[data-ab-scope]").forEach(initAbPick);
  // A swapped-in page (swap.js) never re-runs this script, so
  // its cards arrive visible all at once; collapse them to the
  // first when the shell announces the new content, the same
  // moment init() re-wires the search form below.
  document.addEventListener("evesynapse:content-ready", function () {
    document.querySelectorAll("[data-ab-scope]").forEach(initAbPick);
  });

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
          el.value = el.getAttribute("data-slider-rest") || "0";
        } else {
          el.value = "";
        }
      });
      // A cleared abyssal drawer has no family pick and no
      // bands left: re-derive which roll sliders show.
      if (btn.getAttribute("data-reset-drawer") === "abyssal") {
        if (familySelect) familySelect.value = "";
        if (sizeSelect) sizeSelect.value = "";
        if (sourceSelect) sourceSelect.value = "";
        syncAbyssalState(false);
      }
    });
  });

  // ---- auto-submit toggles -----------------------------------------
  // Type pills and the deals switch filter the moment they
  // flip, like the mockup's live filter state.
  if (form) {
    form.querySelectorAll(".cs-auto").forEach(function (el) {
      el.addEventListener("change", function () {
        // Inside the phone filters sheet the reader is still
        // choosing; the sheet's own submit applies them together.
        if (el.closest(".as-sheet")) return;
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

  // ---- roll band catalog and dynamic sliders -----------------------------
  // The drawer carries roll sliders per rollable attribute.
  // The catalog carries every family's rollable attributes,
  // value ranges, and meta variant marks as JSON (#cs-roll-catalog).
  // Track gradients fill dynamically between min and max handles,
  // readouts show actual rolled values with units, and clicking
  // a meta variant mark sets the min bound to that variant.
  var catalogByFamily = {};
  var rollCatalog = {};
  var rollMetaByFamily = {};
  var rollCatalogEl = document.getElementById("cs-roll-catalog");
  if (rollCatalogEl) {
    try {
      JSON.parse(rollCatalogEl.textContent).forEach(function (g) {
        catalogByFamily[g.family] = g;
        rollCatalog[g.family] = g.attrs || [];
        if (g.meta && g.meta.length) {
          var mMap = {};
          g.meta.forEach(function (m) { mMap[m.id] = m; });
          rollMetaByFamily[g.family] = mMap;
        }
      });
    } catch (e) { /* a bad catalog leaves the server split in place */ }
  }

  function formatRollStat(v, unit) {
    var s;
    if (v === Math.round(v)) {
      s = String(v);
    } else if (Math.abs(v) < 10) {
      s = (Math.round(v * 100) / 100).toFixed(2).replace(/\.?0+$/, "");
    } else {
      s = (Math.round(v * 10) / 10).toFixed(1).replace(/\.?0+$/, "");
    }
    if (unit === "x") return s + "x";
    if (!unit) return s;
    if (unit === "%") return s + "%";
    return s + " " + unit;
  }

  var familySelect = form.querySelector('select[name="family"]');
  var sizeSelect = form.querySelector('select[name="module_size"]');
  var sourceSelect = form.querySelector('select[name="source_q"]');
  var sizeWrap = form.querySelector("[data-ab-size-wrap]");
  var sourceWrap = form.querySelector("[data-ab-source-wrap]");
  var valuationWrap = form.querySelector("[data-ab-valuation]");
  var slidersWrap = form.querySelector("[data-ab-sliders]");
  var rollGrid = form.querySelector(".cs-rollgrid");
  var rollHint = form.querySelector("[data-roll-hint]");

  // If styled tooltips are able to be shown, suppress native browser title tooltips
  // so dual tooltips do not appear. Use title as a fallback only when styled tips are absent.
  function auditMarkTooltips(container) {
    var root = container || form;
    if (!root) return;
    root.querySelectorAll(".cs-roll-mark").forEach(function (mark) {
      if (mark.querySelector(".cs-roll-mark-tip")) {
        if (mark.hasAttribute("title")) {
          mark.setAttribute("data-tooltip", mark.getAttribute("title"));
          mark.removeAttribute("title");
        }
      } else if (mark.hasAttribute("data-tooltip") && !mark.hasAttribute("title")) {
        mark.setAttribute("title", mark.getAttribute("data-tooltip"));
      }
    });
  }
  auditMarkTooltips(form);

  function currentAttrMeta(attrId) {
    var fam = familySelect ? familySelect.value : "";
    var g = catalogByFamily[fam];
    if (g) {
      var sz = sizeSelect ? sizeSelect.value : "";
      if (sz && g.meta_by_size && g.meta_by_size[sz]) {
        for (var i = 0; i < g.meta_by_size[sz].length; i++) {
          if (g.meta_by_size[sz][i].id === attrId) return g.meta_by_size[sz][i];
        }
      }
      if (g.meta) {
        for (var j = 0; j < g.meta.length; j++) {
          if (g.meta[j].id === attrId) return g.meta[j];
        }
      }
    }
    for (var f in rollMetaByFamily) {
      if (rollMetaByFamily[f] && rollMetaByFamily[f][attrId]) {
        return rollMetaByFamily[f][attrId];
      }
    }
    return null;
  }

  function updateTrackContainer(container) {
    if (!container) return;
    var field = container.closest("[data-roll-field]");
    if (!field) return;
    var attrId = Number(field.getAttribute("data-roll-field"));
    var minInput = container.querySelector(".cs-rollslider-min");
    var maxInput = container.querySelector(".cs-rollslider-max");
    var minNum = document.getElementById("cs-rollmin-" + attrId);
    var maxNum = document.getElementById("cs-rollmax-" + attrId);
    var minReadout = container.querySelector('[data-readout-for="cs-rollmin-' + attrId + '"]');
    var maxReadout = container.querySelector('[data-readout-for="cs-rollmax-' + attrId + '"]');

    var mn = minNum && minNum.value !== "" ? Number(minNum.value) : (minInput ? Number(minInput.value) : 0);
    var mx = maxNum && maxNum.value !== "" ? Number(maxNum.value) : (maxInput ? Number(maxInput.value) : 100);
    if (isNaN(mn)) mn = 0;
    if (isNaN(mx)) mx = 100;

    container.style.setProperty("--min", mn + "%");
    container.style.setProperty("--max", mx + "%");

    var meta = currentAttrMeta(attrId);
    if (minReadout) {
      if (minNum && minNum.value === "") {
        minReadout.textContent = "";
      } else if (meta && meta.worst !== undefined && meta.best !== undefined) {
        var vMin = meta.worst + (meta.best - meta.worst) * (mn / 100);
        minReadout.textContent = formatRollStat(vMin, meta.unit);
      } else {
        minReadout.textContent = mn + "%";
      }
    }
    if (maxReadout) {
      if (maxNum && maxNum.value === "") {
        maxReadout.textContent = "";
      } else if (meta && meta.worst !== undefined && meta.best !== undefined) {
        var vMax = meta.worst + (meta.best - meta.worst) * (mx / 100);
        maxReadout.textContent = formatRollStat(vMax, meta.unit);
      } else {
        maxReadout.textContent = mx + "%";
      }
    }

    container.querySelectorAll(".cs-roll-mark").forEach(function (mark) {
      var pct = Number(mark.getAttribute("data-pct"));
      var inRange = pct >= mn && pct <= mx;
      var pip = mark.querySelector(".cs-roll-mark-pip");
      var wick = mark.querySelector(".cs-roll-mark-wick");
      if (pip) {
        pip.style.borderColor = inRange ? "#ffd27a" : "var(--box-edge)";
        pip.style.background = inRange ? "#ffd27a" : "#2a2018";
      }
      if (wick) {
        wick.style.background = inRange ? "color-mix(in srgb, #ffd27a 65%, rgba(255, 255, 255, 0.4))" : "color-mix(in srgb, var(--accent) 45%, rgba(255, 255, 255, 0.35))";
        wick.style.opacity = inRange ? "0.85" : "0.45";
      }
    });
  }

  function bindTrackMarkClicks(container) {
    if (!container) return;
    var field = container.closest("[data-roll-field]");
    if (!field) return;
    var attrId = Number(field.getAttribute("data-roll-field"));
    var minInput = container.querySelector(".cs-rollslider-min");
    var minNum = document.getElementById("cs-rollmin-" + attrId);

    container.querySelectorAll(".cs-roll-mark").forEach(function (mark) {
      if (mark._bound) return;
      mark._bound = true;
      mark.addEventListener("click", function (e) {
        e.preventDefault();
        var pct = mark.getAttribute("data-pct");
        if (minInput) minInput.value = pct;
        if (minNum) minNum.value = pct;
        updateTrackContainer(container);
      });
    });
  }

  // Bind slider pairs with dynamic gradient & readout updates.
  document.querySelectorAll("input[data-slider-for]").forEach(function (slider) {
    var num = document.getElementById(slider.getAttribute("data-slider-for"));
    var container = slider.closest(".cs-roll-track-container");
    if (num) {
      slider.addEventListener("input", function () {
        num.value = slider.value;
        if (container) updateTrackContainer(container);
      });
      num.addEventListener("input", function () {
        slider.value = num.value === "" ? (slider.getAttribute("data-slider-rest") || "0") : num.value;
        if (container) updateTrackContainer(container);
      });
    }
    if (container) {
      bindTrackMarkClicks(container);
      updateTrackContainer(container);
    }
  });

  function rollFieldBanded(field) {
    var set = false;
    field.querySelectorAll('input[type="number"]').forEach(function (n) {
      if (n.value !== "") set = true;
    });
    return set;
  }

  function applyRollFamily() {
    if (!rollGrid || !familySelect) return;
    var fam = familySelect.value;
    var order = rollCatalog[fam] || [];
    var inFamily = {};
    order.forEach(function (id) { inFamily[id] = true; });
    var lead = [], extra = [], rest = [];
    rollGrid.querySelectorAll("[data-roll-field]").forEach(function (field) {
      var id = Number(field.getAttribute("data-roll-field"));
      if (inFamily[id]) lead.push(field);
      else if (rollFieldBanded(field)) extra.push(field);
      else rest.push(field);
    });
    lead.sort(function (a, b) {
      return order.indexOf(Number(a.getAttribute("data-roll-field"))) -
        order.indexOf(Number(b.getAttribute("data-roll-field")));
    });
    extra.sort(function (a, b) {
      return Number(a.getAttribute("data-roll-field")) - Number(b.getAttribute("data-roll-field"));
    });
    lead.concat(extra).forEach(function (field) {
      field.hidden = false;
      field.querySelectorAll("input").forEach(function (i) { i.disabled = false; });
      rollGrid.appendChild(field);
    });
    rest.forEach(function (field) {
      field.hidden = true;
      field.querySelectorAll("input").forEach(function (i) { i.disabled = true; });
      rollGrid.appendChild(field);
    });
    if (rollHint) rollHint.hidden = fam !== "";

    // Refresh range badges, marks, and track readouts for the active family and size.
    rollGrid.querySelectorAll("[data-roll-field]").forEach(function (field) {
      var id = Number(field.getAttribute("data-roll-field"));
      var meta = currentAttrMeta(id);
      var rangeBadge = field.querySelector('[data-roll-range="' + id + '"]');
      if (rangeBadge && meta) {
        rangeBadge.textContent = meta.range_text || "";
      }
      var marksContainer = field.querySelector('[data-marks-for="' + id + '"]');
      if (marksContainer && meta && meta.marks) {
        marksContainer.innerHTML = "";
        meta.marks.forEach(function (m) {
          var btn = document.createElement("button");
          btn.type = "button";
          btn.className = "cs-roll-mark";
          btn.style.left = m.pct + "%";
          btn.setAttribute("data-pct", m.pct);
          btn.setAttribute("data-val", m.val);
          btn.setAttribute("data-fmt", m.fmt);
          btn.setAttribute("data-tooltip", m.label + ": " + m.fmt);
          btn.setAttribute("aria-label", m.label + ": " + m.fmt);

          var wick = document.createElement("span");
          wick.className = "cs-roll-mark-wick";
          var pip = document.createElement("span");
          pip.className = "cs-roll-mark-pip";
          var tip = document.createElement("span");
          tip.className = "cs-roll-mark-tip";
          tip.textContent = m.label + ": " + m.fmt;

          btn.appendChild(wick);
          btn.appendChild(pip);
          btn.appendChild(tip);
          marksContainer.appendChild(btn);
        });
        auditMarkTooltips(marksContainer);
      }
      var container = field.querySelector(".cs-roll-track-container");
      if (container) {
        bindTrackMarkClicks(container);
        updateTrackContainer(container);
      }
    });
  }

  function syncAbyssalState(sourceChange) {
    var fam = familySelect ? familySelect.value : "";
    var g = catalogByFamily[fam];
    var hasFamily = Boolean(fam && g);
    var hasSizes = Boolean(hasFamily && g.sizes && g.sizes.length > 0);

    // 1. Size dropdown
    if (sizeWrap && sizeSelect) {
      if (hasSizes) {
        sizeWrap.hidden = false;
        var prevSize = sizeSelect.value;
        var optionsValues = Array.prototype.map.call(sizeSelect.options, function (o) { return o.value; });
        var neededValues = [""].concat(g.sizes);
        if (optionsValues.join("|") !== neededValues.join("|")) {
          sizeSelect.innerHTML = '<option value="">Select size...</option>';
          g.sizes.forEach(function (sz) {
            var opt = document.createElement("option");
            opt.value = sz;
            opt.textContent = sz;
            if (sz === prevSize) opt.selected = true;
            sizeSelect.appendChild(opt);
          });
        }
      } else {
        sizeWrap.hidden = true;
        sizeSelect.value = "";
      }
    }

    // 2. Source module dropdown
    if (sourceWrap && sourceSelect) {
      if (hasFamily) {
        sourceWrap.hidden = false;
        var prevSource = sourceSelect.value;
        var sz = sizeSelect ? sizeSelect.value : "";
        var list = [];
        if (g.modules) {
          if (sz && g.modules[sz]) {
            list = g.modules[sz];
          } else if (!hasSizes && g.modules[""]) {
            list = g.modules[""];
          } else {
            for (var k in g.modules) {
              list = list.concat(g.modules[k]);
            }
            list.sort();
          }
        }
        var curSrcVals = Array.prototype.map.call(sourceSelect.options, function (o) { return o.value; });
        var targetSrcVals = [""].concat(list);
        if (curSrcVals.join("|") !== targetSrcVals.join("|")) {
          sourceSelect.innerHTML = '<option value="">Any original module</option>';
          list.forEach(function (mName) {
            var opt = document.createElement("option");
            opt.value = mName;
            opt.textContent = mName;
            if (mName === prevSource) opt.selected = true;
            sourceSelect.appendChild(opt);
          });
        }
      } else {
        sourceWrap.hidden = true;
        sourceSelect.value = "";
      }
    }

    // 3. Unlock criteria:
    // Only once Size or Original module is selected should attribute selectors appear.
    var chosenSize = sizeSelect ? sizeSelect.value : "";
    var chosenSource = sourceSelect ? sourceSelect.value : "";
    var unlocked = Boolean(chosenSize || chosenSource);

    if (valuationWrap) valuationWrap.hidden = !unlocked;
    if (slidersWrap) slidersWrap.hidden = !unlocked;

    applyRollFamily();
  }

  function detectModuleSize(name) {
    if (!name) return "";
    var lower = name.toLowerCase();
    if (lower.indexOf("x-large") !== -1 || lower.indexOf("xlarge") !== -1 || lower.indexOf("xl ") !== -1) return "X-Large";
    if (lower.indexOf("capital") !== -1 || lower.indexOf("10000mn") !== -1 || lower.indexOf("50000mn") !== -1) return "Capital";
    if (lower.indexOf("large") !== -1 || lower.indexOf("heavy") !== -1 ||
        lower.indexOf("100mn") !== -1 || lower.indexOf("500mn") !== -1 ||
        lower.indexOf("800mm") !== -1 || lower.indexOf("1600mm") !== -1 ||
        lower.indexOf("corpus ") === 0 || lower.indexOf("gist ") === 0 ||
        lower.indexOf("core x-type") !== -1 || lower.indexOf("core a-type") !== -1 ||
        lower.indexOf("core b-type") !== -1 || lower.indexOf("core c-type") !== -1) return "Large";
    if (lower.indexOf("medium") !== -1 || lower.indexOf("10mn") !== -1 ||
        lower.indexOf("50mn") !== -1 || lower.indexOf("200mm") !== -1 ||
        lower.indexOf("400mm") !== -1 || lower.indexOf("corpum ") === 0 ||
        lower.indexOf("gistum ") === 0 || lower.indexOf("corelum ") === 0) return "Medium";
    if (lower.indexOf("small") !== -1 || lower.indexOf("micro") !== -1 ||
        lower.indexOf("1mn") !== -1 || lower.indexOf("5mn") !== -1 ||
        lower.indexOf("50mm") !== -1 || lower.indexOf("100mm") !== -1 ||
        lower.indexOf("corpii ") === 0 || lower.indexOf("gistii ") === 0 ||
        lower.indexOf("coreli ") === 0) return "Small";
    return "";
  }

  if (familySelect) {
    familySelect.addEventListener("change", function () {
      if (sizeSelect) sizeSelect.value = "";
      if (sourceSelect) sourceSelect.value = "";
      syncAbyssalState(false);
    });
  }
  if (sizeSelect) {
    sizeSelect.addEventListener("change", function () {
      if (sourceSelect) sourceSelect.value = "";
      syncAbyssalState(false);
    });
  }
  if (sourceSelect) {
    sourceSelect.addEventListener("change", function () {
      var chosenSource = sourceSelect.value;
      if (chosenSource && sizeSelect) {
        var detectedSize = "";
        var fam = familySelect ? familySelect.value : "";
        var g = catalogByFamily[fam];
        if (g && g.modules) {
          for (var sz in g.modules) {
            if (g.modules[sz] && g.modules[sz].indexOf(chosenSource) !== -1) {
              detectedSize = sz;
              break;
            }
          }
        }
        if (!detectedSize) {
          detectedSize = detectModuleSize(chosenSource);
        }
        if (detectedSize && sizeSelect.value !== detectedSize) {
          sizeSelect.value = detectedSize;
        }
      }
      syncAbyssalState(true);
    });
  }
  if (familySelect) {
    syncAbyssalState(false);
  }

  // ---- security floor readout -------------------------------------------
  // The floor prints beside its slider with one decimal;
  // the slider itself is the submitted value, so the
  // readout is display only.
  document.querySelectorAll(".cs-secslider").forEach(function (slider) {
    var pair = slider.closest(".cs-secpair");
    var out = pair ? pair.querySelector("[data-sec-val]") : null;
    if (!out) return;
    slider.addEventListener("input", function () {
      out.textContent = Number(slider.value).toFixed(1);
    });
  });

  // ---- results accordion -------------------------------------------------
  // Exactly one row open at a time; clicking the open row
  // closes it. The expansion lives in the drawer row right
  // under the data row; panels load lazily, one contract
  // per fetch, and are cached in the drawer for the session.
  var openRow = null;
  function drawerSlot(row) {
    var drawer = row.nextElementSibling;
    return drawer ? drawer.querySelector(".cs-panel-slot") : null;
  }
  function syncMoreToggle(row) {
    var btn = row.querySelector("[data-more-toggle]");
    if (btn) btn.setAttribute("aria-expanded", row.getAttribute("aria-expanded"));
  }
  function closeRow(row) {
    var slot = drawerSlot(row);
    if (slot) slot.hidden = true;
    row.classList.remove("open");
    row.setAttribute("aria-expanded", "false");
    syncMoreToggle(row);
    if (openRow === row) openRow = null;
  }
  function openRowPanel(row) {
    var slot = drawerSlot(row);
    if (!slot) return;
    if (openRow && openRow !== row) closeRow(openRow);
    openRow = row;
    row.classList.add("open");
    row.setAttribute("aria-expanded", "true");
    syncMoreToggle(row);
    if (slot.dataset.loaded) {
      slot.hidden = false;
      return;
    }
    slot.hidden = false;
    slot.innerHTML = '<p class="muted cs-panel-loading">Loading appraisal…</p>';
    // The search's own query rides along so the drawer
    // can pin the line the search matched; the panel
    // handler reads only the item context out of it.
    fetch("/contracts/search/" + row.getAttribute("data-contract") + "/panel" + window.location.search, {
      headers: { "X-Requested-With": "fetch" }
    }).then(function (resp) {
      if (!resp.ok) throw new Error("panel failed");
      return resp.text();
    }).then(function (html) {
      slot.innerHTML = html;
      slot.dataset.loaded = "1";
      applyMeters(slot);
      initAbPick(slot);
    }).catch(function () {
      slot.innerHTML = '<p class="muted cs-panel-loading">Could not load the appraisal. The contract page carries the same detail.</p>';
    });
  }
  document.querySelectorAll(".cs-row").forEach(function (row) {
    row.addEventListener("click", function (e) {
      if (e.target.closest("a, button, input, select, textarea, label")) return;
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
  // The contents cell caps at five lines: the "And N
  // more items" line under the head never reveals lines
  // in the cell -- it toggles the row's drawer, where
  // the full contents list, and collapses it again.
  // It is a button, so the row's own click guard already
  // keeps the click from double-toggling; it runs the
  // same open/close pair a row click runs.
  document.querySelectorAll("[data-more-toggle]").forEach(function (btn) {
    btn.addEventListener("click", function (e) {
      e.stopPropagation();
      var row = btn.closest(".cs-row");
      if (!row) return;
      if (openRow === row) closeRow(row);
      else openRowPanel(row);
    });
  });
  }
  init();
  document.addEventListener("evesynapse:content-ready", init);
})();
