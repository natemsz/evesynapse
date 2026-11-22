// The live fleet: refreshes the Composition tab from stored data while
// the fleet runs. The server answers 204 when nothing changed. A refresh
// waits while a commander is editing a pilot, so it never takes their
// typing away. Without JavaScript the page is complete as loaded.
(function () {
  "use strict";

  var timer = null;

  function region() {
    var el = document.getElementById("fl-live");
    return el && el.getAttribute("data-fleet-live") ? el : null;
  }

  function editing(el) {
    var active = document.activeElement;
    if (active && el.contains(active) && active.matches("input, select, textarea")) return true;
    var fields = el.querySelectorAll("input[type=text]");
    for (var i = 0; i < fields.length; i++) {
      if (fields[i].value !== fields[i].defaultValue) return true;
    }
    var picks = el.querySelectorAll("select");
    for (var j = 0; j < picks.length; j++) {
      var options = picks[j].options;
      for (var k = 0; k < options.length; k++) {
        if (options[k].selected !== options[k].defaultSelected) {
          // A select with nothing marked shows its first option.
          if (k === 0 && !picks[j].querySelector("option[selected]")) continue;
          return true;
        }
      }
    }
    return false;
  }

  function schedule() {
    if (timer !== null) window.clearTimeout(timer);
    timer = null;
    var el = region();
    if (!el) return;
    var every = parseInt(el.getAttribute("data-every"), 10) || 20;
    timer = window.setTimeout(poll, every * 1000);
  }

  function poll() {
    timer = null;
    var el = region();
    if (!el) return;
    if (document.visibilityState !== "visible" || editing(el)) {
      schedule();
      return;
    }
    var url = new URL(el.getAttribute("data-fleet-live"), window.location.href);
    url.searchParams.set("rev", el.getAttribute("data-revision") || "0");
    window.fetch(url, {
      credentials: "same-origin",
      headers: { "X-Requested-With": "XMLHttpRequest" }
    }).then(function (resp) {
      if (resp.status === 204) return null;
      if (!resp.ok) throw new Error("stop");
      return resp.text();
    }).then(function (html) {
      if (html !== null && document.contains(el) && !editing(el)) {
        var holder = document.createElement("template");
        holder.innerHTML = html;
        var fresh = holder.content.getElementById("fl-live");
        if (fresh) {
          el.replaceWith(fresh);
          var said = document.getElementById("fl-live-said");
          if (said) said.textContent = "Fleet updated.";
        }
      }
      schedule();
    }).catch(function (err) {
      // The fleet is no longer open to this viewer: stop asking.
      if (err && err.message === "stop") return;
      schedule();
    });
  }

  document.addEventListener("evesynapse:content-ready", schedule);
  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "visible" && region()) {
      if (timer !== null) window.clearTimeout(timer);
      poll();
    }
  });
  schedule();
})();
