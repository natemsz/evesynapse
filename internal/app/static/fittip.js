// Shared tooltip: the builder every item tooltip in the app
// renders through (FitTipUI below: the fitting workbench, the
// read-only fit pages, requirement icons), loaded before
// fit.js on fitting pages and on its own by pages that only
// need the tooltips. The requirement icon wiring at the end is
// delegated from the document, so content swaps cannot detach
// it; its tooltip is appended to the body, clear of any
// scrolling pane.

// --- item sheets and the info drawer -----------------------------
// FitSheet fetches a type's static sheet (/items/sheet/{id}.json:
// the tooltip's rows and the full sheet's HTML) once per page and
// keeps it. FitSheet.open shows the full sheet in the info drawer, a
// manual popover in the top layer: a side panel, the whole screen
// on a phone, with its close control on top. Any element carrying
// data-fit-info="<type id>" opens it.
var FitSheet = (function () {
  var cache = {};
  function get(id) {
    id = String(id || "");
    if (!/^[0-9]+$/.test(id)) return Promise.resolve(null);
    if (!cache[id]) {
      cache[id] = fetch("/items/sheet/" + id + ".json", { headers: { "Accept": "application/json" } })
        .then(function (r) { return r.ok ? r.json() : null; })
        .catch(function () { return null; })
        .then(function (d) { if (!d) delete cache[id]; return d; });
    }
    return cache[id];
  }
  var drawer = null, title = null, sub = null, link = null, body = null, opener = null, want = "";
  function build() {
    drawer = document.createElement("aside");
    drawer.className = "fit-info";
    drawer.setAttribute("popover", "manual");
    drawer.setAttribute("aria-labelledby", "fit-info-title");
    var head = document.createElement("div");
    head.className = "fit-info-head";
    var close = document.createElement("button");
    close.type = "button";
    close.className = "fit-info-close";
    close.textContent = "Back";
    close.addEventListener("click", hide);
    var names = document.createElement("div");
    names.className = "fit-info-names";
    title = document.createElement("h2");
    title.id = "fit-info-title";
    sub = document.createElement("span");
    sub.className = "fit-info-sub";
    names.appendChild(title);
    names.appendChild(sub);
    link = document.createElement("a");
    link.className = "fit-info-link";
    link.textContent = "Item page";
    head.appendChild(close);
    head.appendChild(names);
    head.appendChild(link);
    body = document.createElement("div");
    body.className = "fit-info-body";
    drawer.appendChild(head);
    drawer.appendChild(body);
    document.body.appendChild(drawer);
  }
  function isOpen() { return !!drawer && drawer.matches(":popover-open"); }
  function hide() {
    if (!isOpen()) return;
    drawer.hidePopover();
    if (opener && opener.isConnected && opener.focus) {
      try { opener.focus({ preventScroll: true }); } catch (e) {}
    }
    opener = null;
  }
  function open(id, from) {
    if (!drawer) build();
    if (!drawer.showPopover) return;
    want = String(id);
    opener = from || document.activeElement;
    title.textContent = "Loading";
    sub.textContent = "";
    link.removeAttribute("href");
    link.hidden = true;
    body.textContent = "";
    if (!isOpen()) drawer.showPopover();
    drawer.scrollTop = 0;
    get(id).then(function (d) {
      if (want !== String(id)) return; // a later open won
      if (!d) {
        title.textContent = "Details unavailable";
        body.textContent = "This item's details couldn't be loaded. Try again in a moment.";
        return;
      }
      title.textContent = d.name;
      sub.textContent = d.group || "";
      link.href = d.url;
      link.hidden = false;
      // The server's own escaped template output (itemsheet.html).
      body.innerHTML = d.html;
      try { drawer.querySelector(".fit-info-close").focus({ preventScroll: true }); } catch (e) {}
    });
  }
  document.addEventListener("click", function (ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest("[data-fit-info]") : null;
    if (!t) return;
    ev.preventDefault();
    open(t.getAttribute("data-fit-info"), t);
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape" && isOpen()) hide();
  });
  return { get: get, open: open, close: hide };
})();

// --- shared module tooltip ------------------------------------
// One tooltip builder for every surface that shows a fitted
// module: the workbench editor and the read-only workbenches
// (a public fit's page). The content is identical everywhere,
// name, group, meta, the server's computed stat rows
// (data-tip-lines: "label|value|class" rows, the class good or
// bad when the effective value beats or trails the module's
// base value), and the loaded charge. State buttons render
// only when the caller passes a state handler AND the element
// carries states; read-only surfaces do neither, so their
// tooltips are information-only by construction.
var FitTipUI = (function () {
  // Glyphs: one small line drawing per kind of figure, coloured by
  // the kind (fitting.css), so a tooltip reads at a glance.
  var GLYPHS = {
    shield: "M8 1.5 13.5 3.5v4c0 3.3-2.3 5.8-5.5 7-3.2-1.2-5.5-3.7-5.5-7v-4z",
    armor: "M8 1.5l5.6 3.25v6.5L8 14.5l-5.6-3.25v-6.5zM8 5.2l2.6 1.5v3L8 11.2 5.4 9.7v-3z",
    structure: "M2.5 2.5h11v11h-11zM2.5 8h11M8 2.5v11",
    bonus: "M8 1.5 13.5 3.5v4c0 3.3-2.3 5.8-5.5 7-3.2-1.2-5.5-3.7-5.5-7v-4zM8 5v5M5.5 7.5h5",
    capacitor: "M9.5 1.5 3.5 9H8l-1.5 5.5L12.5 7H8z",
    targeting: "M8 3a5 5 0 1 0 0 10A5 5 0 1 0 8 3zM8 .5v4M8 11.5v4M.5 8h4M11.5 8h4",
    navigation: "M1.5 8h11M9 4.5 12.5 8 9 11.5M1.5 4.5h4M1.5 11.5h4",
    weapon: "M8 5.5a2.5 2.5 0 1 0 0 5 2.5 2.5 0 1 0 0-5zM8 .5v3M8 12.5v3M.5 8h3M12.5 8h3M3 3l2 2M11 11l2 2M13 3l-2 2M5 11l-2 2",
    drones: "M8 6a2 2 0 1 0 0 4 2 2 0 1 0 0-4zM3 2.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 1 0 0-3zM13 2.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 1 0 0-3zM3 10.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 1 0 0-3zM13 10.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 1 0 0-3zM4.3 4.3l2.3 2.3M11.7 4.3 9.4 6.6M4.3 11.7l2.3-2.3M11.7 11.7 9.4 9.4",
    fitting: "M4.5 4.5h7v7h-7zM6.5 1.5v3M9.5 1.5v3M6.5 11.5v3M9.5 11.5v3M1.5 6.5h3M1.5 9.5h3M11.5 6.5h3M11.5 9.5h3",
    miscellaneous: "M8 6.5a1.5 1.5 0 1 0 0 3 1.5 1.5 0 1 0 0-3z"
  };
  // The curated rows' labels (fitview.FitTipLines) by kind.
  var LABEL_KIND = {
    DPS: "weapon", Volley: "weapon", Optimal: "targeting", Falloff: "targeting", Tracking: "targeting",
    "Rate of fire": "weapon", Damage: "weapon",
    Neuts: "capacitor", Cap: "capacitor", Capacitor: "capacitor", "Cap recharge": "capacitor",
    "Armor HP": "armor", "Shield HP": "shield", "Shield recharge": "shield",
    Boost: "navigation", Strength: "navigation", Signature: "navigation",
    CPU: "fitting", PG: "fitting", "High slots": "fitting", "Mid slots": "fitting", "Low slots": "fitting",
    Turrets: "fitting", Launchers: "fitting"
  };
  var SVGNS = "http://www.w3.org/2000/svg";
  function glyph(kind) {
    var svg = document.createElementNS(SVGNS, "svg");
    svg.setAttribute("viewBox", "0 0 16 16");
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("class", "fit-tip-g");
    if (GLYPHS[kind]) {
      var path = document.createElementNS(SVGNS, "path");
      path.setAttribute("d", GLYPHS[kind]);
      svg.appendChild(path);
    }
    return svg;
  }
  // One label/figure row; kind picks its glyph and colour.
  function lineRow(label, value, kind, cls) {
    var r = document.createElement("div");
    r.className = "fit-tip-line" + (kind ? " k-" + kind : "");
    r.appendChild(glyph(kind));
    var k = document.createElement("span");
    k.className = "fit-tip-k";
    k.textContent = label;
    var v = document.createElement("span");
    v.className = "fit-tip-v" + (cls ? " " + cls : "");
    v.textContent = value;
    r.appendChild(k);
    r.appendChild(v);
    return r;
  }
  // A layer's resists: glyph, layer name, then four damage-type
  // cells, each filled to its figure.
  var RES_KINDS = ["em", "th", "kin", "exp"];
  var LAYER_NAMES = { shield: "Shield", armor: "Armor", structure: "Hull", bonus: "Resist bonus" };
  function resistRow(layer, figs) {
    var r = document.createElement("div");
    r.className = "fit-tip-res k-" + layer;
    r.appendChild(glyph(layer));
    var k = document.createElement("span");
    k.className = "fit-tip-k";
    k.textContent = LAYER_NAMES[layer] || layer;
    r.appendChild(k);
    for (var i = 0; i < 4; i++) {
      var raw = String(figs[i] || "0");
      var n = Math.abs(parseFloat(raw)) || 0;
      var c = document.createElement("span");
      c.className = "fit-tip-rc res-" + RES_KINDS[i];
      c.title = RES_KINDS[i].toUpperCase() + " " + raw + "%";
      var bar = document.createElement("span");
      bar.className = "fit-tip-rbar";
      bar.style.width = Math.min(100, n) + "%";
      var t = document.createElement("span");
      t.textContent = raw.charAt(0) === "+" ? raw : String(Math.round(n * 10) / 10);
      c.appendChild(bar);
      c.appendChild(t);
      r.appendChild(c);
    }
    return r;
  }
  function build(slot, opts) {
    opts = opts || {};
    var name = slot.getAttribute("data-tip-name") || "";
    if (!name) return null;
    var sub = slot.getAttribute("data-tip-sub") || "";
    var tipEl = document.createElement("div");
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
    var linesAttr = slot.getAttribute("data-tip-lines") || "";
    if (linesAttr) {
      var rows = document.createElement("div");
      rows.className = "fit-tip-lines";
      linesAttr.split(";").forEach(function (row) {
        var f = row.split("|");
        if (f.length < 2 || !f[0]) return;
        var kind = LABEL_KIND[f[0]] || (/\bres-/.test(f[2] || "") ? "bonus" : "");
        // Repairs take the layer they repair: shield boosters sit in
        // mid slots, armor and hull repairers in lows.
        if (f[0] === "Reps") kind = /^Mid/.test(sub) ? "shield" : "armor";
        rows.appendChild(lineRow(f[0], f[1], kind, f[2]));
      });
      tipEl.appendChild(rows);
    } else {
      var lines = [];
      var meta = slot.getAttribute("data-tip-meta") || "";
      var cpu = slot.getAttribute("data-tip-cpu") || "";
      var pg = slot.getAttribute("data-tip-pg") || "";
      var stat = slot.getAttribute("data-tip-stat") || "";
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
    }
    // The type's own sheet: its top rows when the curated rows say
    // only what the module costs (CPU, powergrid, capacitor) or there
    // are none (rigs, damage controls, implants, charges), and an
    // Info control that opens the full sheet. Curated rows that say
    // what the module does are the fitted, skill-adjusted figures,
    // so the sheet's base figures are not repeated under them.
    var typeID = slot.getAttribute("data-tip-type") || "";
    var costOnly = !linesAttr || linesAttr.split(";").every(function (row) {
      var label = row.split("|")[0];
      return !label || label === "CPU" || label === "PG" || label === "Cap";
    });
    if (typeID && costOnly) {
      var sheetRows = document.createElement("div");
      sheetRows.className = "fit-tip-lines";
      tipEl.appendChild(sheetRows);
      FitSheet.get(typeID).then(function (d) {
        if (!d || !d.top || !d.top.length || !tipEl.isConnected) return;
        d.top.forEach(function (row) {
          if (row.r && row.l) sheetRows.appendChild(resistRow(row.l, row.r));
          else sheetRows.appendChild(lineRow(row.k, row.v, row.s || "", ""));
        });
        if (tipEl._replace) tipEl._replace();
      });
    }
    var charge = slot.getAttribute("data-tip-charge") || "";
    if (charge) {
      var c = document.createElement("div");
      c.className = "fit-tip-stats";
      c.textContent = charge;
      tipEl.appendChild(c);
    }
    // Module states: the server lists only the states this
    // module can take; the current one is highlighted. Tapping
    // a state applies it through the caller's handler and
    // re-simulates; the tooltip re-opens on the new state
    // once the workbench re-renders.
    var tipStates = slot.getAttribute("data-tip-states") || "";
    var tipState = slot.getAttribute("data-tip-state") || "";
    var tipKey = slot.getAttribute("data-tip-key") || "";
    if (opts.onState && tipStates && tipKey) {
      var labels = { offline: "Offline", online: "Online", active: "Active", overheated: "Overheat" };
      var row = document.createElement("div");
      row.className = "fit-tip-states";
      row.setAttribute("role", "group");
      row.setAttribute("aria-label", "Module state");
      tipStates.split(",").forEach(function (s) {
        if (!labels[s]) return;
        var b = document.createElement("button");
        b.type = "button";
        b.className = "fit-tip-state" + (s === tipState ? " on" : "");
        b.textContent = labels[s];
        b.setAttribute("aria-pressed", s === tipState ? "true" : "false");
        (function (st, btn) {
          btn.addEventListener("click", function (ev) {
            ev.stopPropagation();
            // Optimistic highlight; the re-render confirms it.
            var sibs = row.querySelectorAll(".fit-tip-state");
            for (var k = 0; k < sibs.length; k++) {
              sibs[k].classList.remove("on");
              sibs[k].setAttribute("aria-pressed", "false");
            }
            btn.classList.add("on");
            btn.setAttribute("aria-pressed", "true");
            opts.onState(tipKey, st);
          });
        })(s, b);
        row.appendChild(b);
      });
      tipEl.appendChild(row);
    }
    if (typeID) {
      var info = document.createElement("button");
      info.type = "button";
      info.className = "fit-tip-info";
      info.textContent = "Info";
      info.setAttribute("aria-label", "Details of " + name);
      info.addEventListener("click", function (ev) {
        ev.stopPropagation();
        ev.preventDefault();
        if (opts.onClose) opts.onClose();
        FitSheet.open(typeID, slot);
      });
      tipEl.appendChild(info);
    }
    var x = document.createElement("button");
    x.className = "fit-tip-x";
    x.setAttribute("aria-label", "Close");
    x.textContent = "×";
    x.addEventListener("click", function (ev) { ev.stopPropagation(); if (opts.onClose) opts.onClose(); });
    tipEl.appendChild(x);
    return tipEl;
  }
  // Anchors the (already appended) tooltip. A ring slot's tooltip
  // sits beside the slot, offset outward by where the slot is on the
  // ring, right of a slot on the right, above one at the top,
  // diagonally out from one in between, the way the game places
  // it: never over the neighboring slots, never in the middle. It
  // may run past the workbench over the panels beside it, as in
  // game; it is only kept inside the window's width. A list chip's sits
  // beside its row, inside the workbench. Positions are relative to
  // the workbench, the offset parent (position: relative).
  function place(tipEl, slot, wb) {
    // Rows that arrive later (the sheet) re-place the tooltip.
    tipEl._replace = function () { if (tipEl.isConnected && slot.isConnected) place(tipEl, slot, wb); };
    var wr = wb.getBoundingClientRect();
    var sr = slot.getBoundingClientRect();
    var ring = slot.closest ? slot.closest(".fit-visual") : null;
    var tx, ty, tw, th;
    var gap = 8, edge = 8;
    function clamp() {
      if (tx + tw > wr.width - edge) tx = wr.width - tw - edge;
      if (tx < edge) tx = edge;
      if (ty + th > wr.height - edge) ty = wr.height - th - edge;
      if (ty < edge) ty = edge;
    }
    if (ring) {
      var vr = ring.getBoundingClientRect();
      var cx = vr.left + vr.width / 2, cy = vr.top + vr.height / 2;
      var sx = sr.left + sr.width / 2, sy = sr.top + sr.height / 2;
      var dx = sx - cx, dy = sy - cy;
      var len = Math.sqrt(dx * dx + dy * dy) || 1;
      var ux = dx / len, uy = dy / len;
      tw = tipEl.offsetWidth || 190;
      th = tipEl.offsetHeight || 110;
      // Outward on each axis that points outward; centred on the
      // slot along an axis that barely does.
      var lean = 0.38;
      var left, top;
      if (ux > lean) left = sr.right + gap;
      else if (ux < -lean) left = sr.left - gap - tw;
      else left = sx - tw / 2;
      if (uy > lean) top = sr.bottom + gap;
      else if (uy < -lean) top = sr.top - gap - th;
      else top = sy - th / 2;
      // Kept inside the window's width only: past a side it would be
      // cut off, while above or below the page simply scrolls, and a
      // vertical clamp would push it back onto the slots.
      var docEl = document.documentElement;
      var vw = (docEl && docEl.clientWidth) || window.innerWidth || Infinity;
      left = Math.max(edge, Math.min(left, vw - tw - edge));
      tipEl.style.left = Math.round(left - wr.left) + "px";
      tipEl.style.top = Math.round(top - wr.top) + "px";
      return;
    }
    {
      tw = tipEl.offsetWidth || 190;
      th = tipEl.offsetHeight || 110;
      var right = sr.right - wr.left + 10;
      var left = sr.left - wr.left - tw - 10;
      if (right + tw <= wr.width - edge) tx = right;
      else if (left >= edge) tx = left;
      else tx = sr.left - wr.left;
      ty = sr.top - wr.top;
      if (tx === sr.left - wr.left) {
        // No room on either side: under the row, the last resort.
        ty = sr.bottom - wr.top + 10;
      }
      clamp();
    }
    tipEl.style.left = Math.round(tx) + "px";
    tipEl.style.top = Math.round(ty) + "px";
  }
  return { build: build, place: place };
})();

// --- requirement icon tooltips -------------------------------
// Surfaces that render item requirements as icons (.req-icon,
// from the shared reqicons template partial) get the shared
// tooltip on hover or tap: the item's name, the quantity
// needed, and its average sell and buy. Mirrors the read-only
// workbench wiring's timing and close rules.
(function () {
  var tipEl = null;
  var hoverEl = null;
  var closeTimer = null;
  function closeTip() {
    if (tipEl && tipEl.parentNode) tipEl.parentNode.removeChild(tipEl);
    tipEl = null;
    hoverEl = null;
  }
  function showTip(el) {
    closeTip();
    tipEl = FitTipUI.build(el, { onClose: closeTip });
    if (!tipEl) return;
    hoverEl = el;
    tipEl.style.visibility = "hidden";
    document.body.appendChild(tipEl);
    FitTipUI.place(tipEl, el, document.body);
    tipEl.style.visibility = "";
  }
  function scheduleClose() {
    if (closeTimer) clearTimeout(closeTimer);
    closeTimer = setTimeout(function () {
      closeTimer = null;
      if (tipEl && !tipEl.matches(":hover")) closeTip();
    }, 150);
  }
  function iconOf(ev) {
    return ev.target && ev.target.closest ? ev.target.closest(".req-icon") : null;
  }
  function coarsePointer() {
    return !!(window.matchMedia && window.matchMedia("(hover: none)").matches);
  }
  document.addEventListener("mouseover", function (ev) {
    // A touch tap synthesizes a mouseover first; on coarse
    // pointers the click below is the opener instead, so the
    // first tap can pin the tooltip rather than navigate.
    if (coarsePointer()) return;
    var el = iconOf(ev);
    if (el) {
      if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
      if (el !== hoverEl) showTip(el);
      return;
    }
    if (tipEl && ev.target && ev.target.closest && ev.target.closest(".fit-tip")) {
      if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
    }
  });
  document.addEventListener("mouseout", function (ev) {
    var el = iconOf(ev);
    if (el && el === hoverEl) { scheduleClose(); return; }
    if (tipEl && ev.target && ev.target.closest && ev.target.closest(".fit-tip")) {
      var to = ev.relatedTarget;
      if (!to || !to.closest || !to.closest(".fit-tip")) scheduleClose();
    }
  });
  document.addEventListener("click", function (ev) {
    var el = iconOf(ev);
    if (el) {
      if (hoverEl === el && tipEl) { closeTip(); return; }
      if (coarsePointer()) {
        ev.preventDefault();
        showTip(el);
      }
      return;
    }
    if (tipEl && !(ev.target && ev.target.closest && ev.target.closest(".fit-tip"))) closeTip();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") closeTip();
  });
})();
