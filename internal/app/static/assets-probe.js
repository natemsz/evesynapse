// When the asset CDN can't be reached, the page returns to the
// wallpapers and landing stills built into the app.
(function () {
  var meta = document.querySelector('meta[name="es-assets"]');
  if (!meta) return;
  var base = meta.getAttribute("content");
  function local(value) { return value.split(base).join(""); }
  function restore() {
    var media = document.querySelectorAll("img, video");
    for (var i = 0; i < media.length; i++) {
      var el = media[i];
      var attrs = ["src", "srcset", "poster", "data-full-poster"];
      for (var j = 0; j < attrs.length; j++) {
        var value = el.getAttribute(attrs[j]);
        if (value && value.indexOf(base) !== -1) el.setAttribute(attrs[j], local(value));
      }
      if (el.tagName === "VIDEO" && el.hasAttribute("data-gal-vid")) {
        el.removeAttribute("src");
        el.load();
      }
    }
  }
  var probe = new Image();
  probe.onerror = function () {
    document.documentElement.setAttribute("data-assets-off", "");
    if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", restore);
    else restore();
  };
  probe.src = base + "/static/bg-calm-muted-thumb.jpg";
})();
