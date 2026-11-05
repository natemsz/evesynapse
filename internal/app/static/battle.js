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

    bindCrosshair(page);

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

  // --- Chart crosshair ------------------------------------------
  // The chart carries one stored point per column (#bt-hover-data,
  // battleHoverPt in battle_correlate.go: t the time; o, i, r, g, c
  // the rates of damage out and in, repairs in and out, and energy
  // drained; ti, to, tr who hit the pilot hardest, whom the pilot hit
  // hardest and who repaired the pilot most in that 10-second slice,
  // each {n name, h hull, s ship type, a amount}; m the mails that
  // landed there, each {s ship type, l a loss, t text}). Anywhere
  // over the chart, the column under the pointer shows in the shared
  // hover layer, the same card the site's hover sparklines use; a
  // finger scrubs sideways while the page still scrolls up and down.
  // Names come from players' logs, so the card is built from text
  // nodes, never markup.
  function layerEl() {
    var l = document.getElementById("spark-layer");
    if (!l) {
      l = document.createElement("div");
      l.id = "spark-layer";
      l.hidden = true;
      document.body.appendChild(l);
    }
    return l;
  }
  function node(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text !== undefined) e.textContent = text;
    return e;
  }
  function fmt(n) { return Math.round(n || 0).toLocaleString("en-US"); }
  function icon(typeID, size) {
    var id = parseInt(typeID, 10);
    if (!(id > 0)) return node("span", "btc-noicon");
    var img = node("img");
    img.src = "https://images.evetech.net/types/" + id + "/icon?size=" + (size > 32 ? 64 : 32);
    img.alt = "";
    img.width = size;
    img.height = size;
    return img;
  }
  function who(card, label, p, unit) {
    if (!p) return;
    card.appendChild(node("span", "btc-sub", label));
    var row = node("span", "btc-w");
    row.appendChild(icon(p.s, 22));
    var name = node("span", "btc-wn", p.n);
    if (p.h && p.h !== p.n) {
      name.appendChild(document.createTextNode(" "));
      name.appendChild(node("span", "btc-s", p.h));
    }
    row.appendChild(name);
    row.appendChild(node("b", "", fmt(p.a) + (unit || "")));
    card.appendChild(row);
  }
  function fill(l, pt) {
    l.className = "bt-card bt-card-chart";
    l.textContent = "";
    var top = node("span", "btc-top");
    top.appendChild(node("span", "btc-time", pt.t));
    top.appendChild(node("span", "btc-s", "a second, 10-second average"));
    l.appendChild(top);
    var rows = node("span", "btc-rows");
    var lines = [["Damage out", pt.o, "btc-out", ""], ["Damage in", pt.i, "btc-in", ""], ["Repairs in", pt.r, "btc-rep", ""], ["Repairs out", pt.g, "btc-rep", ""], ["Energy drained", pt.c, "btc-cap", " GJ"]];
    for (var i = 0; i < lines.length; i++) {
      if (i > 1 && !lines[i][1]) continue;
      var row = node("span", "btc-row " + lines[i][2]);
      row.appendChild(node("span", "", lines[i][0]));
      row.appendChild(node("b", "", fmt(lines[i][1]) + lines[i][3]));
      rows.appendChild(row);
    }
    l.appendChild(rows);
    who(l, "Hitting you hardest", pt.ti);
    who(l, "Your main target", pt.to);
    who(l, "Repairing you most", pt.tr);
    if (pt.m && pt.m.length) {
      l.appendChild(node("span", "btc-sub", "Killmails"));
      for (var m = 0; m < pt.m.length; m++) {
        var mail = node("span", "btc-w " + (pt.m[m].l ? "btc-loss" : "btc-kill"));
        mail.appendChild(icon(pt.m[m].s, 22));
        mail.appendChild(node("span", "btc-wn", pt.m[m].t));
        l.appendChild(mail);
      }
    }
  }
  function bindCrosshair(page) {
    var data = page.querySelector("#bt-hover-data");
    var svg = page.querySelector(".bt-chart");
    var cursor = page.querySelector("#bt-cursor");
    if (!data || !svg || !cursor) return;
    var pts = [];
    try { pts = JSON.parse(data.textContent) || []; } catch (e) { pts = []; }
    if (!pts.length) return;
    var last = -1;
    function at(ev) {
      var r = svg.getBoundingClientRect();
      if (!r.width) return;
      var frac = Math.min(Math.max((ev.clientX - r.left) / r.width, 0), 1);
      var idx = pts.length > 1 ? Math.round(frac * (pts.length - 1)) : 0;
      var x = pts.length > 1 ? idx / (pts.length - 1) * 600 : 0;
      cursor.setAttribute("x1", x);
      cursor.setAttribute("x2", x);
      cursor.removeAttribute("hidden");
      var l = layerEl();
      if (idx !== last || l.hidden) {
        fill(l, pts[idx]);
        last = idx;
      }
      l.hidden = false;
      var lw = l.offsetWidth, lh = l.offsetHeight;
      var touch = ev.pointerType === "touch" || ev.pointerType === "pen";
      var px = ev.clientX + 16;
      if (px + lw > window.innerWidth - 8) px = ev.clientX - lw - 16;
      px = Math.min(Math.max(8, px), window.innerWidth - lw - 8);
      var py = touch ? ev.clientY - lh - 28 : ev.clientY - lh / 2;
      if (py < 8) py = touch ? ev.clientY + 28 : 8;
      py = Math.min(py, window.innerHeight - lh - 8);
      l.style.left = px + "px";
      l.style.top = py + "px";
    }
    function off() {
      cursor.setAttribute("hidden", "");
      var l = document.getElementById("spark-layer");
      if (l && l.classList.contains("bt-card-chart")) l.hidden = true;
      last = -1;
    }
    svg.addEventListener("pointermove", at);
    svg.addEventListener("pointerdown", at);
    // A mouse leaving the chart closes the card; a lifted finger
    // leaves it up to be read, until a tap anywhere else or a scroll.
    svg.addEventListener("pointerleave", function (ev) { if (ev.pointerType === "mouse") off(); });
    function away(ev) { if (!svg.contains(ev.target)) off(); }
    function scrolled() { off(); }
    document.addEventListener("pointerdown", away);
    window.addEventListener("scroll", scrolled, true);
    document.addEventListener("evesynapse:content-will-swap", function () {
      off();
      document.removeEventListener("pointerdown", away);
      window.removeEventListener("scroll", scrolled, true);
    }, { once: true });
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
