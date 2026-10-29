// Global notice banners (notices.go): dismissing one, and opening
// one's detail post. Clicks are delegated from the document, bound
// once, so swapped-in banners work as well as the first page's. A
// dismissal is remembered in a cookie (the server leaves those
// banners out of the next render) and, for a signed-in visitor, on
// the account. Pages served from the swap cache can still carry a
// dismissed banner, so the cookie is applied again on every
// content-ready.
(function () {
  "use strict";

  var COOKIE = "evesynapse-notices-hidden";
  var MAX = 20;

  function hiddenIds() {
    var parts = document.cookie ? document.cookie.split(";") : [];
    for (var i = 0; i < parts.length; i++) {
      var kv = parts[i].trim();
      if (kv.indexOf(COOKIE + "=") === 0) {
        return kv.slice(COOKIE.length + 1).split(".").filter(function (s) { return /^\d+$/.test(s); });
      }
    }
    return [];
  }

  function remember(id) {
    var ids = hiddenIds().filter(function (s) { return s !== id; });
    ids.push(id);
    if (ids.length > MAX) ids = ids.slice(ids.length - MAX);
    var secure = window.location.protocol === "https:" ? "; secure" : "";
    document.cookie = COOKIE + "=" + ids.join(".") + "; path=/; max-age=7776000; samesite=lax" + secure;
  }

  function settle(wrap) {
    if (!wrap) return;
    var shown = wrap.querySelectorAll(".gn-banner:not([hidden])");
    wrap.hidden = shown.length === 0;
  }

  function apply(root) {
    var ids = hiddenIds();
    var wraps = root.querySelectorAll("[data-gn-banners]");
    for (var w = 0; w < wraps.length; w++) {
      var wrap = wraps[w];
      var banners = wrap.querySelectorAll("[data-gn-id]");
      for (var i = 0; i < banners.length; i++) {
        if (ids.indexOf(banners[i].getAttribute("data-gn-id")) >= 0) banners[i].hidden = true;
      }
      settle(wrap);
    }
  }

  // The shared modal lives in app.js; the landing page does not load
  // it, so a plain show and hide stands in there.
  function openPost(el) {
    if (!el) return;
    if (window.EveSynapseModal) { window.EveSynapseModal.open(el); return; }
    el.hidden = false;
    var close = el.querySelector("[data-close-modal]");
    if (close) close.focus();
  }
  function closeFallback(el) {
    if (el && !window.EveSynapseModal) el.hidden = true;
  }

  document.addEventListener("click", function (ev) {
    var t = ev.target;
    if (!t || !t.closest) return;
    var dismiss = t.closest("[data-gn-dismiss]");
    if (dismiss) {
      ev.preventDefault();
      var id = dismiss.getAttribute("data-gn-dismiss");
      var banner = dismiss.closest("[data-gn-id]");
      var wrap = dismiss.closest("[data-gn-banners]");
      remember(id);
      if (banner) banner.hidden = true;
      settle(wrap);
      if (wrap && wrap.hasAttribute("data-gn-account") && window.fetch) {
        var body = new URLSearchParams();
        body.set("id", id);
        fetch("/notices/dismiss", { method: "POST", body: body, credentials: "same-origin" }).catch(function () {});
      }
      return;
    }
    var opener = t.closest("[data-gn-open]");
    if (opener) {
      ev.preventDefault();
      openPost(document.getElementById(opener.getAttribute("data-gn-open")));
      return;
    }
    if (!window.EveSynapseModal) {
      var box = t.classList && t.classList.contains("modal-backdrop") ? t : null;
      var closer = t.closest("[data-close-modal]");
      if (closer) box = closer.closest(".modal-backdrop");
      if (box && box.id && box.id.indexOf("gn-post-") === 0) closeFallback(box);
    }
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Escape" || window.EveSynapseModal) return;
    var open = document.querySelectorAll(".modal-backdrop[id^='gn-post-']:not([hidden])");
    for (var i = 0; i < open.length; i++) closeFallback(open[i]);
  });

  function boot() { apply(document); }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
  document.addEventListener("evesynapse:content-ready", boot);
})();
