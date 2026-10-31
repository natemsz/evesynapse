// Shared tooltip: the builder every item tooltip in the app
// renders through (FitTipUI below: the fitting workbench, the
// read-only fit pages, requirement icons), loaded before
// fit.js on fitting pages and on its own by pages that only
// need the tooltips. The requirement icon wiring at the end is
// delegated from the document, so content swaps cannot detach
// it; its tooltip is appended to the body, clear of any
// scrolling pane.

// --- shared module tooltip ------------------------------------
// One tooltip builder for every surface that shows a fitted
// module: the workbench editor and the read-only workbenches
// (a public fit's page). The content is identical everywhere —
// name, group, meta, the server's computed stat rows
// (data-tip-lines: "label|value|class" rows, the class good or
// bad when the effective value beats or trails the module's
// base value), and the loaded charge. State buttons render
// only when the caller passes a state handler AND the element
// carries states; read-only surfaces do neither, so their
// tooltips are information-only by construction.
var FitTipUI = (function () {
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
        var r = document.createElement("div");
        r.className = "fit-tip-line";
        var k = document.createElement("span");
        k.className = "fit-tip-k";
        k.textContent = f[0];
        var v = document.createElement("span");
        v.className = "fit-tip-v" + (f[2] ? " " + f[2] : "");
        v.textContent = f[1];
        r.appendChild(k);
        r.appendChild(v);
        rows.appendChild(r);
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
  // ring — right of a slot on the right, above one at the top,
  // diagonally out from one in between — the way the game places
  // it: never over the neighboring slots, never in the middle. It
  // may run past the workbench over the panels beside it, as in
  // game; it is only kept inside the window's width. A list chip's sits
  // beside its row, inside the workbench. Positions are relative to
  // the workbench, the offset parent (position: relative).
  function place(tipEl, slot, wb) {
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
