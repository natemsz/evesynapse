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
      var again = document.querySelector("#fl-e-" + CSS.escape(id) + " > summary") || document.querySelector("#fl-p-" + CSS.escape(id) + " button");
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

// The planner: its start is written in EVE time (UTC) while the
// browser shows the box in its own format, so say when that is for
// the reader; preset buttons fill the start and length; and the side
// preview follows the form as it is filled in.
(function () {
  "use strict";
  function pad(n) { return (n < 10 ? "0" : "") + n; }
  function stamp(d) {
    return d.getUTCFullYear() + "-" + pad(d.getUTCMonth() + 1) + "-" + pad(d.getUTCDate()) + "T" + pad(d.getUTCHours()) + ":" + pad(d.getUTCMinutes());
  }
  function parse(v) {
    if (!v) return null;
    if (v.length === 16) v += ":00";
    var d = new Date(v + "Z");
    return isNaN(d.getTime()) ? null : d;
  }
  function checkedLabel(form, name) {
    var input = form.querySelector('input[name="' + name + '"]:checked');
    var span = input && input.parentElement ? input.parentElement.querySelector("span") : null;
    if (!span) return "";
    var first = span.firstChild;
    return first && first.nodeType === 3 ? first.textContent.trim() : span.textContent.trim();
  }
  function bind() {
    var form = document.getElementById("fl-plan-form");
    var start = document.getElementById("fl-start");
    var out = document.getElementById("fl-start-local");
    if (!form || !start || form.getAttribute("data-plan-bound")) return;
    form.setAttribute("data-plan-bound", "1");
    var length = document.getElementById("fl-length");
    var preview = document.querySelector("[data-fl-preview]");
    function set(key, text) {
      if (!preview) return;
      var el = preview.querySelector('[data-pv="' + key + '"]');
      if (el && text) el.textContent = text;
    }
    function render() {
      var d = parse(start.value);
      var local = d ? d.toLocaleString(undefined, { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }) : "";
      if (out) out.textContent = d ? "In EVE time (UTC). That is " + local + " your time." : "In EVE time, which is UTC.";
      if (d) {
        set("clock", pad(d.getUTCHours()) + ":" + pad(d.getUTCMinutes()));
        set("day", ["Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"][d.getUTCDay()] + " " +
          ["Jan", "Feb", "Mar", "Apr", "May", "Jun", "Jul", "Aug", "Sep", "Oct", "Nov", "Dec"][d.getUTCMonth()] + " " + d.getUTCDate());
        set("local", local);
      }
      var title = document.getElementById("fl-title");
      set("title", title && title.value.trim() ? title.value.trim() : "Your fleet's title");
      if (length && length.value) set("length", length.value + " min");
      set("fly", checkedLabel(form, "doctrine_mode"));
      set("see", checkedLabel(form, "visibility"));
      set("join", checkedLabel(form, "join_gate"));
      set("signin", checkedLabel(form, "sign_in"));
      var names = form.querySelector('input[name="roster_public"]');
      if (names) set("names", names.checked ? "Shown to everyone who can open it" : "Shown to those who can sign up");
    }
    form.addEventListener("input", render);
    form.addEventListener("change", render);
    var presets = form.querySelectorAll("[data-fl-presets]");
    for (var i = 0; i < presets.length; i++) presets[i].hidden = false;
    form.addEventListener("click", function (ev) {
      var b = ev.target && ev.target.closest ? ev.target.closest("[data-start], [data-length]") : null;
      if (!b) return;
      ev.preventDefault();
      if (b.hasAttribute("data-length") && length) {
        length.value = b.getAttribute("data-length");
      } else {
        var spec = b.getAttribute("data-start") || "";
        var now = new Date();
        var d;
        if (spec.charAt(0) === "+") {
          d = new Date(now.getTime() + parseInt(spec.slice(1), 10) * 60000);
          d.setUTCMinutes(Math.ceil(d.getUTCMinutes() / 5) * 5, 0, 0);
        } else {
          var hm = spec.split(":");
          d = new Date(Date.UTC(now.getUTCFullYear(), now.getUTCMonth(), now.getUTCDate(), parseInt(hm[0], 10), parseInt(hm[1], 10)));
          if (d.getTime() <= now.getTime()) d = new Date(d.getTime() + 86400000);
        }
        start.value = stamp(d);
      }
      render();
    });
    render();
  }
  bind();
  document.addEventListener("evesynapse:content-ready", bind);
})();
