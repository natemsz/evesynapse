// The landing page's script: the public-tools modal and the
// back-to-top button. Every section works with the script
// off — the modal's links are plain anchors in the markup —
// so the script only moves state between shapes the page
// already holds.

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

// The header bar's liquid glass: generate the displacement map
// for the bar's exact size so the refraction layer in
// landing.css can bend the backdrop at the rim. The bar is a
// pill, so the corner radius is half its height. The map is
// neutral in the interior; only the rim band shifts, strongest
// at the edge. Regenerated when the bar changes size. If the
// map never lands, or the browser has no backdrop-filter url()
// support, the bar keeps its frost and rim light un-displaced.
(function () {
  var bar = document.querySelector(".ld-hbar");
  var fe = document.getElementById("ld-liquid-map");
  if (!bar || !fe) return;
  var RIM = 16, MAXOFF = 9;
  var lastW = 0, lastH = 0;
  function build() {
    var W = bar.offsetWidth, H = bar.offsetHeight;
    if (!W || !H || (W === lastW && H === lastH)) return;
    lastW = W; lastH = H;
    var R = H / 2;
    var cv = document.createElement("canvas");
    cv.width = W; cv.height = H;
    var ctx = cv.getContext("2d");
    var img = ctx.createImageData(W, H);
    var data = img.data;
    // Signed distance to the pill's border: negative inside.
    function sd(x, y) {
      var qx = Math.abs(x - W / 2) - (W / 2 - R);
      var qy = Math.abs(y - H / 2) - (H / 2 - R);
      var ax = Math.max(qx, 0), ay = Math.max(qy, 0);
      return Math.sqrt(ax * ax + ay * ay) + Math.min(Math.max(qx, qy), 0) - R;
    }
    var EPS = 1.25;
    for (var y = 0; y < H; y++) {
      for (var x = 0; x < W; x++) {
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
        var i = (y * W + x) * 4;
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
  build();
  var timer = null;
  window.addEventListener("resize", function () {
    if (timer) clearTimeout(timer);
    timer = setTimeout(build, 150);
  });
})();

