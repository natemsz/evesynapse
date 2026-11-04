// EveSynapse: the battle report's timeline (battle.html).
//
// Every moment on the timeline, and the panel's previous and next
// controls, is a plain link to the report with ?m=<seq>; without
// script the server renders that moment. With it, a pick loads the
// moment's panel in place (its fragment URL rides data-moment),
// lights the event on the lanes, moves the chart's band over the
// moment's window (data-band-x / data-band-w, in chart units), and
// keeps the address bar on the moment so a reload or a shared link
// opens the same one. Arrow keys step through the moments while the
// panel or a lane event has focus.
(function () {
  "use strict";

  function init(root) {
    var page = (root || document).querySelector("[data-battle]");
    if (!page || page.getAttribute("data-battle-bound")) return;
    page.setAttribute("data-battle-bound", "1");
    var panel = page.querySelector("#bt-moment");
    var band = page.querySelector("#bt-band");
    if (!panel) return;
    var pending = 0;

    function light(seq) {
      var events = page.querySelectorAll(".bt-ev");
      for (var i = 0; i < events.length; i++) {
        var ev = events[i];
        var on = /\/moment\/(\d+)\/$/.exec(ev.getAttribute("data-moment") || "");
        var mine = on && on[1] === seq;
        ev.classList.toggle("on", !!mine);
        if (mine) {
          ev.setAttribute("aria-current", "true");
          if (band) {
            band.setAttribute("x", ev.getAttribute("data-band-x"));
            band.setAttribute("width", ev.getAttribute("data-band-w"));
            band.removeAttribute("hidden");
          }
        } else {
          ev.removeAttribute("aria-current");
        }
      }
    }

    function open(fragment, href, focusPanel) {
      var ticket = ++pending;
      panel.setAttribute("aria-busy", "true");
      fetch(fragment, { credentials: "same-origin", headers: { Accept: "text/html" } })
        .then(function (res) {
          if (!res.ok) throw new Error(res.status);
          return res.text();
        })
        .then(function (html) {
          if (ticket !== pending) return;
          panel.innerHTML = html;
          panel.removeAttribute("aria-busy");
          var head = panel.querySelector("[data-moment-seq]");
          if (head) light(head.getAttribute("data-moment-seq"));
          try { history.replaceState(history.state, "", href); } catch (e) { /* a sandboxed frame */ }
          if (focusPanel) {
            var h = panel.querySelector("#bt-moment-h");
            if (h) { h.setAttribute("tabindex", "-1"); h.focus({ preventScroll: true }); }
          }
          var r = panel.getBoundingClientRect();
          if (r.top < 0 || r.top > window.innerHeight * 0.6) {
            panel.scrollIntoView({ block: "start", behavior: matchMedia("(prefers-reduced-motion: reduce)").matches ? "auto" : "smooth" });
          }
        })
        .catch(function () {
          // The fragment failed: the link still works whole.
          window.location.href = href;
        });
    }

    page.addEventListener("click", function (ev) {
      if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
      var a = ev.target.closest ? ev.target.closest("a[data-moment]") : null;
      if (!a || !page.contains(a)) return;
      ev.preventDefault();
      open(a.getAttribute("data-moment"), a.getAttribute("href"), a.closest(".bt-mnav") !== null);
    });

    page.addEventListener("keydown", function (ev) {
      if (ev.key !== "ArrowLeft" && ev.key !== "ArrowRight") return;
      var t = ev.target;
      if (!t || !(t.closest(".bt-lanes") || t.closest("#bt-moment"))) return;
      if (t.matches && t.matches("input, textarea, select")) return;
      var link = panel.querySelector(ev.key === "ArrowLeft" ? ".bt-mnav [rel=prev]" : ".bt-mnav [rel=next]");
      if (!link) return;
      ev.preventDefault();
      open(link.getAttribute("data-moment"), link.getAttribute("href"), true);
    });
  }

  // This script loads only with the report, after the shell's first
  // content-ready: run for the document as it stands, then for every
  // swap that brings a report back.
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { init(document); });
  } else {
    init(document);
  }
  if (window.EveSynapseContent && window.EveSynapseContent.onReady) {
    window.EveSynapseContent.onReady(init);
  }
})();
