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
//
// A type with variations, implant levels or booster strengths (the
// sheet's compare flag) gets a second tab, Compare: the priced table
// from /items/compare/{id}.json, each figure marked against the
// opened type. A page that edits a fit registers a swapper
// (FitSheet.setSwapper): given the element the drawer was opened
// from, it answers whether that is a fitted item it can replace, and
// then each other row offers to fit that one in its place. After a
// swap the drawer follows the new type, so variants can be tried in
// turn.
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
  // The compare table carries prices, so it is fetched on each open
  // (the browser keeps it a few minutes).
  function getCompare(id) {
    return fetch("/items/compare/" + id + ".json", { headers: { "Accept": "application/json" } })
      .then(function (r) { return r.ok ? r.json() : null; })
      .catch(function () { return null; });
  }
  var drawer = null, title = null, sub = null, link = null, tabs = null, body = null, cmp = null;
  var opener = null, want = "", swapper = null, swap = null, tab = "details", cmpFor = "";
  function tabButton(key, label) {
    var b = document.createElement("button");
    b.type = "button";
    b.setAttribute("role", "tab");
    b.setAttribute("data-fit-info-tab", key);
    b.textContent = label;
    b.addEventListener("click", function () { showTab(key); });
    return b;
  }
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
    tabs = document.createElement("div");
    tabs.className = "fit-info-tabs";
    tabs.setAttribute("role", "tablist");
    tabs.setAttribute("aria-label", "Item details");
    tabs.appendChild(tabButton("details", "Details"));
    tabs.appendChild(tabButton("compare", "Compare"));
    tabs.hidden = true;
    var bar = document.createElement("div");
    bar.className = "fit-info-bar";
    bar.appendChild(head);
    bar.appendChild(tabs);
    body = document.createElement("div");
    body.className = "fit-info-body";
    body.setAttribute("role", "tabpanel");
    cmp = document.createElement("div");
    cmp.className = "fit-info-body fit-info-cmp";
    cmp.setAttribute("role", "tabpanel");
    cmp.hidden = true;
    drawer.appendChild(bar);
    drawer.appendChild(body);
    drawer.appendChild(cmp);
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
    swap = null;
  }
  function showTab(key) {
    tab = key === "compare" && !tabs.hidden ? "compare" : "details";
    var bs = tabs.querySelectorAll("[data-fit-info-tab]");
    for (var i = 0; i < bs.length; i++) {
      var on = bs[i].getAttribute("data-fit-info-tab") === tab;
      bs[i].classList.toggle("on", on);
      bs[i].setAttribute("aria-selected", on ? "true" : "false");
    }
    drawer.classList.toggle("is-wide", tab === "compare");
    body.hidden = tab !== "details";
    cmp.hidden = tab !== "compare";
    drawer.scrollTop = 0;
    if (tab === "compare" && cmpFor !== want) loadCompare(want);
  }
  function note(msg) {
    var p = cmp.querySelector(".fit-info-msg");
    if (!p) {
      p = document.createElement("p");
      p.className = "fit-info-msg";
      p.setAttribute("role", "status");
      cmp.insertBefore(p, cmp.firstChild);
    }
    p.textContent = msg;
  }
  function loadCompare(id) {
    cmpFor = id;
    cmp.textContent = "";
    var wait = document.createElement("p");
    wait.className = "fit-info-wait";
    wait.textContent = "Loading the comparison";
    cmp.appendChild(wait);
    getCompare(id).then(function (d) {
      if (cmpFor !== id || want !== id) return; // a later open won
      cmp.textContent = "";
      if (!d) {
        cmpFor = "";
        var p = document.createElement("p");
        p.className = "fit-info-wait";
        p.textContent = "The comparison couldn't be loaded. Try again in a moment.";
        cmp.appendChild(p);
        return;
      }
      var panel = document.createElement("section");
      panel.className = "l-panel is-vars";
      var h = document.createElement("h3");
      h.textContent = (swap ? "Swap or compare" : "Compare") + " ";
      var n = document.createElement("small");
      n.textContent = String(d.rows);
      h.appendChild(n);
      panel.appendChild(h);
      var wrap = document.createElement("div");
      // The server's own escaped template output (itemsheet.html).
      wrap.innerHTML = d.html;
      while (wrap.firstChild) panel.appendChild(wrap.firstChild);
      if (swap) addSwaps(panel);
      cmp.appendChild(panel);
    });
  }
  // addSwaps puts a fit control on every row but the opened type's.
  function addSwaps(panel) {
    var rows = panel.querySelectorAll("tr[data-type-id]");
    for (var i = 0; i < rows.length; i++) {
      var tr = rows[i];
      if (tr.classList.contains("is-cur")) continue;
      var cell = tr.querySelector("th");
      var a = cell && cell.querySelector("a");
      if (!a) continue;
      var b = document.createElement("button");
      b.type = "button";
      b.className = "fit-info-swap";
      b.textContent = swap.label;
      b.setAttribute("aria-label", swap.label + " " + a.textContent + " instead");
      (function (to, name) {
        b.addEventListener("click", function () {
          if (!swap) return;
          var refused = swap.apply(swap.from, to, name);
          if (refused) { note(refused); return; }
          swap.from = to;
          open(to, null, true);
        });
      })(tr.getAttribute("data-type-id"), a.textContent);
      cell.appendChild(b);
    }
  }
  // open shows a type's sheet. keepSwap carries the swap in progress
  // over to the type just fitted, on the Compare tab.
  function open(id, from, keepSwap) {
    if (!drawer) build();
    if (!drawer.showPopover) return;
    want = String(id);
    cmpFor = "";
    if (!keepSwap) {
      opener = from || document.activeElement;
      swap = null;
      tab = "details";
      if (swapper && opener) {
        var s = null;
        try { s = swapper(opener); } catch (e) { s = null; }
        if (s && s.apply) swap = { from: want, label: s.label || "Fit", apply: s.apply };
      }
    }
    title.textContent = "Loading";
    sub.textContent = "";
    link.removeAttribute("href");
    link.hidden = true;
    tabs.hidden = true;
    body.textContent = "";
    cmp.textContent = "";
    showTab("details");
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
      tabs.hidden = !d.compare;
      showTab(keepSwap && d.compare ? "compare" : "details");
      try { drawer.querySelector(keepSwap && d.compare ? '[data-fit-info-tab="compare"]' : ".fit-info-close").focus({ preventScroll: true }); } catch (e) {}
    });
  }
  // setSwapper registers the page's swapper: fn(opener) returns null,
  // or { label, apply(fromID, toID, name) } where apply fits toID in
  // fromID's place and returns "" or the reason it could not.
  function setSwapper(fn) { swapper = typeof fn === "function" ? fn : null; }
  document.addEventListener("click", function (ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest("[data-fit-info]") : null;
    if (!t) return;
    ev.preventDefault();
    open(t.getAttribute("data-fit-info"), t);
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape" && isOpen()) hide();
  });
  return { get: get, open: function (id, from) { open(id, from, false); }, close: hide, setSwapper: setSwapper };
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
  // the kind. The drawings are the shared fg-* symbols in base.html,
  // the same ones the workbench's templates use; the kind of each
  // row comes from the server (fitview.FitGlyphKind).
  var SVGNS = "http://www.w3.org/2000/svg";
  function glyph(kind) {
    var svg = document.createElementNS(SVGNS, "svg");
    svg.setAttribute("viewBox", "0 0 16 16");
    svg.setAttribute("aria-hidden", "true");
    svg.setAttribute("class", "fit-tip-g");
    if (kind) {
      var use = document.createElementNS(SVGNS, "use");
      use.setAttribute("href", "#fg-" + kind);
      svg.appendChild(use);
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
        rows.appendChild(lineRow(f[0], f[1], f[3] || "", f[2]));
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
  return { build: build, place: place, glyph: glyph };
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
