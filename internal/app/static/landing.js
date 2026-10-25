// The landing page's script: the public-tools modal, the
// Tools drop-down menu, and the back-to-top button. Every
// section works with the script off: the modal's links are
// plain anchors in the markup, and the menu's links are the
// same anchors one hover or tap away, so the script only
// moves state between shapes the page already holds.

// Public tools modal: open from any data-open-tools hook, close
// on the backdrop, the x, or Escape; the page behind stays put.
(function () {
  var modal = document.getElementById("ldToolsModal");
  if (!modal) return;
  function open(e) {
    if (e) e.preventDefault();
    modal.hidden = false;
    document.body.style.overflow = "hidden";
  }
  function close() {
    modal.hidden = true;
    document.body.style.overflow = "";
  }
  document.querySelectorAll("[data-open-tools]").forEach(function (el) {
    el.addEventListener("click", open);
  });
  modal.querySelectorAll("[data-close-tools]").forEach(function (el) {
    el.addEventListener("click", close);
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && !modal.hidden) close();
  });
})();

// Tools drop-down: opens on hover where a hover exists, on
// click or tap everywhere, and stays open while focus is
// inside it, so its links are reachable by keyboard alone.
// A click pins it open until a second click, an outside
// click, or Escape puts it away.
//
// The panel is hoisted out of the bar to the end of the body
// at init: a backdrop-filter surface nested inside the bar,
// which carries its own backdrop-filter, gets no backdrop in
// some engines, so the panel stands as a top-level surface
// and is placed under the Tools button from the button's own
// rect each time it opens. For hover, the button, the bridge
// over the gap beneath it, and the panel are one region: the
// panel closes only once the pointer has left all three, and
// a short grace covers the travel between them.
(function () {
  var wrap = document.querySelector("[data-tools-menu]");
  if (!wrap) return;
  var btn = wrap.querySelector("[data-open-tools-menu]");
  var menu = wrap.querySelector(".ld-menu");
  if (!btn || !menu) return;
  document.body.appendChild(menu);
  var bridge = document.createElement("div");
  bridge.className = "ld-menu-bridge";
  bridge.hidden = true;
  document.body.appendChild(bridge);
  var pinned = false;
  var closeTimer = null;
  var CLOSE_GRACE = 280;
  var phone = window.matchMedia("(max-width: 560px)");

  // Places the panel under the Tools button (the sheet pins
  // it by rule at phone widths instead) and sizes the bridge
  // over the gap between the button and the panel's top edge.
  function place() {
    var wr = wrap.getBoundingClientRect();
    var br = btn.getBoundingClientRect();
    var menuTop;
    if (phone.matches) {
      menu.style.top = "";
      menu.style.left = "";
      menuTop = 64;
    } else {
      menuTop = wr.bottom + 14;
      menu.style.top = menuTop + "px";
      menu.style.left = (wr.left - 14) + "px";
    }
    var bLeft = Math.min(br.left, phone.matches ? 8 : wr.left - 14);
    bridge.style.top = br.bottom + "px";
    bridge.style.left = bLeft + "px";
    bridge.style.width = (br.right - bLeft + 6) + "px";
    bridge.style.height = Math.max(0, menuTop - br.bottom) + "px";
  }
  function cancelClose() {
    if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
  }
  function focusInside() {
    var a = document.activeElement;
    return !!a && (wrap.contains(a) || menu.contains(a));
  }
  function setOpen(v) {
    cancelClose();
    if (menu.hidden === !v) return;
    if (v) place();
    menu.hidden = !v;
    bridge.hidden = !v;
    btn.setAttribute("aria-expanded", v ? "true" : "false");
  }
  function isOpen() { return !menu.hidden; }
  btn.addEventListener("click", function () {
    pinned = !isOpen();
    setOpen(pinned);
  });
  if (window.matchMedia("(hover: hover)").matches) {
    [btn, bridge, menu].forEach(function (el) {
      el.addEventListener("pointerenter", function () { setOpen(true); });
      el.addEventListener("pointerleave", function () {
        cancelClose();
        closeTimer = setTimeout(function () {
          closeTimer = null;
          if (!pinned && !focusInside()) setOpen(false);
        }, CLOSE_GRACE);
      });
    });
  }
  window.addEventListener("resize", function () { if (isOpen()) place(); });
  window.addEventListener("scroll", function () { if (isOpen()) place(); }, { passive: true });
  wrap.addEventListener("focusin", function () { setOpen(true); });
  menu.addEventListener("focusin", function () { setOpen(true); });
  function onFocusOut(e) {
    if (!pinned && !wrap.contains(e.relatedTarget) && !menu.contains(e.relatedTarget)) setOpen(false);
  }
  wrap.addEventListener("focusout", onFocusOut);
  menu.addEventListener("focusout", onFocusOut);
  document.addEventListener("click", function (e) {
    if (!wrap.contains(e.target) && !menu.contains(e.target) && !bridge.contains(e.target)) { pinned = false; setOpen(false); }
  });
  document.addEventListener("keydown", function (e) {
    if (e.key === "Escape" && isOpen()) {
      pinned = false;
      setOpen(false);
      btn.focus();
    }
  });
})();

// Back to top: the button rides the floating cluster and only
// shows once the reader is deep enough to need it.
(function () {
  var btn = document.getElementById("ldTop");
  if (!btn) return;
  function sync() {
    btn.hidden = window.scrollY < 600;
  }
  window.addEventListener("scroll", sync, { passive: true });
  sync();
  btn.addEventListener("click", function () {
    window.scrollTo({ top: 0, behavior: "smooth" });
  });
})();

// The liquid glass surface: the header bar gets a
// displacement map generated for its exact size, so the
// refraction layer in landing.css can bend the backdrop at
// its rim. The map is neutral in the interior; only the rim
// band shifts, strongest at the edge. Maps are regenerated
// when the bar changes size. If a map never lands, or the
// browser has no backdrop-filter url() support, the bar
// keeps its frost and rim light un-displaced.
//
// The Tools menu is not a liquid surface: its frost stands
// on the menu's own backdrop-filter alone, with no filter
// layer over it, so no filter machinery can take it down.
(function () {
  function liquid(el, fe, opts) {
    if (!el || !fe) return function () {};
    var lastW = 0, lastH = 0;
    function build() {
      var W = el.offsetWidth, H = el.offsetHeight;
      if (!W || !H || (W === lastW && H === lastH)) return;
      lastW = W; lastH = H;
      var scale = opts.scale || 1;
      var w = Math.max(2, Math.round(W * scale)), h = Math.max(2, Math.round(H * scale));
      var R = opts.radius * scale, RIM = opts.rim * scale, MAXOFF = opts.maxoff;
      var cv = document.createElement("canvas");
      cv.width = w; cv.height = h;
      var ctx = cv.getContext("2d");
      var img = ctx.createImageData(w, h);
      var data = img.data;
      // Signed distance to the surface's border: negative inside.
      function sd(x, y) {
        var qx = Math.abs(x - w / 2) - (w / 2 - R);
        var qy = Math.abs(y - h / 2) - (h / 2 - R);
        var ax = Math.max(qx, 0), ay = Math.max(qy, 0);
        return Math.sqrt(ax * ax + ay * ay) + Math.min(Math.max(qx, qy), 0) - R;
      }
      var EPS = 1.25;
      for (var y = 0; y < h; y++) {
        for (var x = 0; x < w; x++) {
          var px = x + 0.5, py = y + 0.5;
          var depth = -sd(px, py); // px inside the shape
          var t = Math.max(0, 1 - depth / RIM);
          var s = t * t * (3 - 2 * t); // smoothstep falloff
          var gx = sd(px + EPS, py) - sd(px - EPS, py);
          var gy = sd(px, py + EPS) - sd(px, py - EPS);
          var gl = Math.sqrt(gx * gx + gy * gy) || 1;
          // The gradient points outward; push the sample outward.
          var ox = (gx / gl) * s * MAXOFF;
          var oy = (gy / gl) * s * MAXOFF;
          var i = (y * w + x) * 4;
          data[i] = Math.max(0, Math.min(255, 128 + ox * (127 / MAXOFF)));
          data[i + 1] = Math.max(0, Math.min(255, 128 + oy * (127 / MAXOFF)));
          data[i + 2] = 128;
          data[i + 3] = 255;
        }
      }
      ctx.putImageData(img, 0, 0);
      fe.setAttribute("href", cv.toDataURL());
      fe.setAttribute("width", W);
      fe.setAttribute("height", H);
    }
    return build;
  }

  var buildBar = liquid(document.querySelector(".ld-hbar"), document.getElementById("ld-liquid-map"),
    { radius: 0, rim: 16, maxoff: 9 });
  buildBar();
  var timer = null;
  window.addEventListener("resize", function () {
    if (timer) clearTimeout(timer);
    timer = setTimeout(buildBar, 150);
  });
})();

