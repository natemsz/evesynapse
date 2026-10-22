// The landing page's script: the feature explorer's tab
// switcher, the public-tools modal, and the back-to-top button.
// Every section works with the script off — the default explorer
// panel is rendered active in the HTML and the modal's links
// are plain anchors in the markup — so the script only moves
// state between shapes the page already holds.

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

