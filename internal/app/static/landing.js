// The landing page's script: the feature explorer's tab
// switcher, the public-tools modal, the back-to-top button, and
// the starfield scatter. Every section works with the script
// off — the default explorer panel is rendered active in the
// HTML, the modal's links are plain anchors in the markup, and
// the stars are decoration — so the script only moves state
// between shapes the page already holds.

// Explorer tabs: move the active marker between tabs and panels.
(function () {
  var tabs = document.querySelectorAll(".ld-xtab");
  var panels = document.querySelectorAll(".ld-xpanel");
  if (!tabs.length || !panels.length) return;
  tabs.forEach(function (tab) {
    tab.addEventListener("click", function () {
      tabs.forEach(function (t) {
        var on = t === tab;
        t.classList.toggle("active", on);
        t.setAttribute("aria-selected", on ? "true" : "false");
      });
      panels.forEach(function (p) {
        p.classList.toggle("active", p.getAttribute("data-xpanel") === tab.getAttribute("data-xtab"));
      });
    });
  });
})();

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

// Starfield: dots, four-point sparkles, and plus glints over the
// whole page depth, scattered once everything has loaded so the
// measured height is the final one. Static after placement.
(function () {
  function scatter() {
    var host = document.getElementById("ldStars");
    if (!host || host.dataset.done) return;
    host.dataset.done = "1";
    var H = Math.max(document.documentElement.scrollHeight, document.body.scrollHeight, 3200);
    var FADE = 900;
    function rnd(a, b) {
      return a + Math.random() * (b - a);
    }
    function placeLow(el, bias) {
      var span = Math.max(H - FADE, 400);
      el.style.left = rnd(0.5, 99) + "%";
      el.style.top = FADE + Math.pow(Math.random(), bias) * span + "px";
      host.appendChild(el);
    }
    function placeTop(el, bias) {
      el.style.left = rnd(0.5, 99) + "%";
      el.style.top = Math.pow(Math.random(), bias) * H + "px";
      host.appendChild(el);
    }
    for (var i = 0; i < 300; i++) {
      var d = document.createElement("i");
      var sz = rnd(0.7, 2.2);
      d.style.width = sz + "px";
      d.style.height = sz + "px";
      d.style.opacity = rnd(0.25, 0.9).toFixed(2);
      placeLow(d, 1.15);
    }
    for (var j = 0; j < 40; j++) {
      var d2 = document.createElement("i");
      var s2 = rnd(0.7, 1.6);
      d2.style.width = s2 + "px";
      d2.style.height = s2 + "px";
      d2.style.opacity = rnd(0.12, 0.4).toFixed(2);
      placeTop(d2, 1.4);
    }
    for (var k = 0; k < 26; k++) {
      var sp = document.createElement("b");
      var sz2 = rnd(7, 15);
      sp.style.width = sz2 + "px";
      sp.style.height = sz2 + "px";
      sp.style.opacity = rnd(0.4, 0.95).toFixed(2);
      placeLow(sp, 1.2);
    }
    for (var m = 0; m < 18; m++) {
      var pl = document.createElement("s");
      pl.textContent = "+";
      pl.style.fontSize = rnd(9, 14) + "px";
      placeLow(pl, 1.2);
    }
  }
  if (document.readyState === "complete") scatter();
  else window.addEventListener("load", scatter);
})();
