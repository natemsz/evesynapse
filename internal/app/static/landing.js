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

