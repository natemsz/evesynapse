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
      // The viewer left the fleet: reload so the page stops showing it.
      if (resp.status === 403 || resp.status === 404) {
        window.location.reload();
        throw new Error("stop");
      }
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
      // The fleet is no longer open to this viewer, or the request was
      // refused: stop asking.
      if (err && err.message === "stop") return;
      schedule();
    });
  }

  // The demo's sample fleet: a Save asks the server to redraw the fleet
  // with every change made on this page. Nothing is stored, so leaving
  // the page drops them all. Without JavaScript the save is refused with
  // the demo's usual notice.
  var tried = {};
  var triedOn = null;

  function trySave(event) {
    var form = event.target;
    var el = document.getElementById("fl-live");
    var url = el && el.getAttribute("data-fleet-try");
    if (!url || !form.id || form.id.indexOf("fl-p-") !== 0 || !el.contains(form)) return;
    event.preventDefault();
    if (el !== triedOn) {
      tried = {};
      triedOn = el;
    }
    var fields = new FormData(form);
    var id = fields.get("character");
    tried[id] = { role: fields.get("role") || "", tags: fields.get("tags") || "" };
    var body = new URLSearchParams();
    body.set("edits", JSON.stringify(tried));
    body.set("page", fields.get("page") || "1");
    window.fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-Requested-With": "XMLHttpRequest" },
      body: body
    }).then(function (resp) {
      if (!resp.ok) throw new Error("refused");
      return resp.text();
    }).then(function (html) {
      if (!document.contains(el)) return;
      var holder = document.createElement("template");
      holder.innerHTML = html;
      var fresh = holder.content.getElementById("fl-live");
      if (!fresh) return;
      el.replaceWith(fresh);
      triedOn = fresh;
      var said = document.getElementById("fl-live-said");
      if (said) said.textContent = "Change shown. It resets when you leave this page.";
      var again = document.querySelector("#fl-p-" + CSS.escape(id) + " button");
      if (again) again.focus();
      document.dispatchEvent(new CustomEvent("evesynapse:content-ready", {
        detail: { root: fresh, url: window.location.href }
      }));
    }).catch(function () {
      var said = document.getElementById("fl-live-said");
      if (said) said.textContent = "That change could not be shown. Reload the page and try again.";
    });
  }

  document.addEventListener("submit", trySave, true);
  document.addEventListener("evesynapse:content-ready", schedule);
  document.addEventListener("visibilitychange", function () {
    if (document.visibilityState === "visible" && region()) {
      if (timer !== null) window.clearTimeout(timer);
      poll();
    }
  });
  schedule();
})();

// The planner's start is written in EVE time (UTC), while the browser
// shows the box in its own format: say when that is for the reader.
(function () {
  "use strict";
  function bind() {
    var input = document.getElementById("fl-start");
    var out = document.getElementById("fl-start-local");
    if (!input || !out || input.getAttribute("data-local-bound")) return;
    input.setAttribute("data-local-bound", "1");
    function render() {
      var v = input.value || "";
      if (v.length === 16) v += ":00";
      var d = new Date(v + "Z");
      if (!v || isNaN(d.getTime())) {
        out.textContent = "In EVE time, which is UTC.";
        return;
      }
      out.textContent = "In EVE time (UTC). That is " + d.toLocaleString(undefined, {
        weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit"
      }) + " your time.";
    }
    input.addEventListener("input", render);
    render();
  }
  bind();
  document.addEventListener("evesynapse:content-ready", bind);
})();
