/* scans.js -- a scan's reading (scan.html).
 *
 * Two small jobs: bars take their widths from data-w (the policy
 * keeps style attributes out of the markup), and "Copy link" copies
 * the scan's full address. Runs at load and on every page swap that
 * brings a scan back.
 */
(function () {
  "use strict";

  function init(root) {
    if (!root.querySelectorAll) return;
    root.querySelectorAll("[data-w]").forEach(function (b) {
      b.style.setProperty("--w", Math.max(0, Math.min(100, +b.getAttribute("data-w") || 0)) + "%");
    });
    root.querySelectorAll("[data-n]").forEach(function (b) {
      b.style.setProperty("--n", Math.max(0, +b.getAttribute("data-n") || 0));
    });
    var link = root.querySelector("[data-scan-link]");
    var btn = root.querySelector("[data-scan-copy]");
    if (!link || !btn || btn.getAttribute("data-bound")) return;
    btn.setAttribute("data-bound", "1");
    link.value = location.origin + link.value;
    link.addEventListener("focus", function () { link.select(); });
    btn.addEventListener("click", function () {
      function done() {
        btn.textContent = "Copied";
        setTimeout(function () { btn.textContent = "Copy link"; }, 1600);
      }
      if (navigator.clipboard && navigator.clipboard.writeText) {
        navigator.clipboard.writeText(link.value).then(done, function () { link.select(); });
      } else {
        link.select();
        try { if (document.execCommand("copy")) done(); } catch (e) { /* select is the fallback */ }
      }
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { init(document); });
  } else {
    init(document);
  }
  if (window.EveSynapseContent && window.EveSynapseContent.onReady) {
    window.EveSynapseContent.onReady(init);
  }
})();
