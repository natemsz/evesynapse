// EveSynapse: the battle report's timeline (battle.html), and the
// game log form on the list (battles.html).
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
//
// The game log form works without script. While a log waits in
// the queue, the list asks how many still do and reloads once none
// do. With it, a chosen or
// dropped file shows its name, and is gzipped in the browser before
// it is sent (a game log packs to about a tenth), which keeps the
// upload inside the site's per-request limit.
(function () {
  "use strict";

  function initUpload(root) {
    var form = (root || document).querySelector("[data-bt-upload]");
    if (!form || form.getAttribute("data-bt-bound")) return;
    form.setAttribute("data-bt-bound", "1");
    var input = form.querySelector("input[type=file]");
    var drop = form.querySelector(".bt-drop");
    var label = form.querySelector("[data-bt-file]");
    var button = form.querySelector("button[type=submit]");
    var hint = label ? label.textContent : "";
    var idle = button ? button.textContent : "";
    if (!input || !drop) return;

    function show() {
      var f = input.files && input.files[0];
      if (label) label.textContent = f ? f.name + ", " + (f.size / 1048576).toFixed(1) + " MB" : hint;
      drop.classList.toggle("has-file", !!f);
    }
    function busy(on) {
      if (!button) return;
      button.disabled = on;
      button.textContent = on ? "Reading" : idle;
    }
    input.addEventListener("change", show);
    ["dragenter", "dragover"].forEach(function (name) {
      drop.addEventListener(name, function (ev) { ev.preventDefault(); drop.classList.add("over"); });
    });
    ["dragleave", "drop"].forEach(function (name) {
      drop.addEventListener(name, function () { drop.classList.remove("over"); });
    });
    drop.addEventListener("drop", function (ev) {
      ev.preventDefault();
      if (ev.dataTransfer && ev.dataTransfer.files.length) {
        input.files = ev.dataTransfer.files;
        show();
      }
    });
    // Back to the page from the history: the button is usable again.
    window.addEventListener("pageshow", function () { busy(false); });
    form.addEventListener("submit", function (ev) {
      var f = input.files && input.files[0];
      if (!f || /\.gz$/i.test(f.name) || !window.CompressionStream || !window.DataTransfer || !f.stream) {
        busy(true);
        return;
      }
      ev.preventDefault();
      busy(true);
      new Response(f.stream().pipeThrough(new CompressionStream("gzip"))).blob().then(function (blob) {
        var packed = new DataTransfer();
        packed.items.add(new File([blob], f.name + ".gz", { type: "application/gzip" }));
        input.files = packed.files;
        form.submit();
      }, function () {
        form.submit();
      });
    });
  }

  // While a game log waits in the queue, ask every few seconds how
  // many still do, and reload the list once none do.
  function initQueue(root) {
    var panel = (root || document).querySelector("[data-bt-queue]");
    if (!panel || panel.getAttribute("data-bt-bound")) return;
    panel.setAttribute("data-bt-bound", "1");
    var tries = 0;
    function ask() {
      if (!document.body.contains(panel) || ++tries > 200) return;
      fetch("/intel/battles/queue", { credentials: "same-origin", cache: "no-store" })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (q) {
          if (q && q.waiting === 0) { window.location.reload(); return; }
          setTimeout(ask, 3000);
        }, function () { setTimeout(ask, 10000); });
    }
    setTimeout(ask, 1500);
  }

  function init(root) {
    initUpload(root);
    initQueue(root);
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
    initZoom(page, panel);

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
      var raw = Math.min(Math.max((ev.clientX - r.left) / r.width, 0), 1);
      // A zoomed strip (initZoom) shows only its window of the battle.
      var za = parseFloat(svg.getAttribute("data-zoom-a")) || 0;
      var zb = parseFloat(svg.getAttribute("data-zoom-b"));
      if (!(zb > za)) zb = 1;
      var frac = za + raw * (zb - za);
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

  // --- Zoom (a fleet battle's strip) -----------------------------
  // A strip marked data-fleet-zoom zooms into any window of the battle:
  // the buttons halve, double or reset it, a drag across the chart
  // picks one, and "Zoom to moment" frames the open moment. Every row of
  // the strip follows: the chart and each pilot's lane through their
  // SVG viewBox (their lines keep their width), the marks on the lanes
  // and the rail by their stored place (data-at, data-span, percent of
  // the battle), and the time axis. The chart's height follows the
  // busiest second in the window, from #fl-zoom-data.
  function initZoom(page, panel) {
    var strip = page.querySelector("[data-fleet-zoom]");
    var dataEl = page.querySelector("#fl-zoom-data");
    if (!strip || !dataEl || strip.getAttribute("data-zoom-bound")) return;
    strip.setAttribute("data-zoom-bound", "1");
    var d;
    try { d = JSON.parse(dataEl.textContent); } catch (e) { return; }
    if (!d || !d.length || !d.out || !d.out.length) return;
    var chart = strip.querySelector(".bt-chart");
    var brush = strip.querySelector("[data-zoom-brush]");
    var controls = strip.querySelector("[data-zoom-controls]");
    var readout = strip.querySelector("[data-zoom-window]");
    var ticks = strip.querySelectorAll("[data-zoom-ticks] span");
    var ytop = strip.querySelector("[data-zoom-ytop]");
    var momentBtn = strip.querySelector('[data-zoom="moment"]');
    var allBtn = strip.querySelector('[data-zoom="all"]');
    var outBtn = strip.querySelector('[data-zoom="out"]');
    var hint = strip.querySelector("[data-zoom-hint]");
    if (controls) controls.hidden = false;
    if (hint) hint.hidden = false;
    var n = d.out.length;
    var full = 1;
    for (var i = 0; i < n; i++) full = Math.max(full, d.out[i], d["in"][i], d.reps[i]);
    var ceilFull = nice(full);
    var win = [0, d.length];

    function nice(v) {
      var p = Math.pow(10, Math.floor(Math.log10(Math.max(v, 1))));
      var m = v / p;
      return (m <= 1 ? 1 : m <= 2 ? 2 : m <= 5 ? 5 : 10) * p;
    }
    function clock(sec) {
      var t = new Date((d.start + Math.round(sec)) * 1000);
      var p2 = function (x) { return (x < 10 ? "0" : "") + x; };
      return p2(t.getUTCHours()) + ":" + p2(t.getUTCMinutes()) + ":" + p2(t.getUTCSeconds());
    }
    function span(sec) {
      var m = Math.floor(sec / 60), s2 = Math.round(sec % 60);
      return m ? m + " min" + (s2 ? " " + s2 + " s" : "") : s2 + " s";
    }
    function apply(a, b) {
      a = Math.max(0, Math.min(a, d.length - 10));
      b = Math.min(d.length, Math.max(b, a + 30));
      win = [a, b];
      var fa = a / d.length, fb = b / d.length;
      // The chart: x to the window, y to its busiest second.
      var lo = Math.floor(fa * (n - 1)), hi = Math.ceil(fb * (n - 1));
      var top = 1;
      for (var i = lo; i <= hi && i < n; i++) top = Math.max(top, d.out[i], d["in"][i], d.reps[i]);
      var h = Math.max(top * 1.08 / ceilFull * 160, 6);
      var svgs = strip.querySelectorAll("[data-zoom-svg]");
      for (var j = 0; j < svgs.length; j++) {
        var sv = svgs[j], flat = sv.hasAttribute("data-zoom-flat");
        var vh = flat ? 20 : h, vy = flat ? 0 : 160 - h;
        sv.setAttribute("viewBox", (fa * 600).toFixed(2) + " " + vy.toFixed(2) + " " + ((fb - fa) * 600).toFixed(2) + " " + vh.toFixed(2));
        sv.setAttribute("data-zoom-a", fa);
        sv.setAttribute("data-zoom-b", fb);
      }
      if (ytop) ytop.textContent = Math.round(h / 160 * ceilFull).toLocaleString("en-US");
      // The marks: placed again inside the window, hidden outside it.
      var marks = strip.querySelectorAll("[data-zoom-track] [data-at]");
      var w = (fb - fa) * 100;
      for (var k = 0; k < marks.length; k++) {
        var mk = marks[k];
        var at = parseFloat(mk.getAttribute("data-at")), sp = parseFloat(mk.getAttribute("data-span")) || 0;
        var left = (at - fa * 100) / w * 100, wide = sp / w * 100;
        var out = left + wide < -1 || left > 101;
        mk.hidden = out;
        if (!out) {
          // A window that starts before the view, or runs past it, is
          // cut at its edge rather than drawn over the lane's label.
          if (sp) {
            var l0 = Math.max(left, 0), r0 = Math.min(left + wide, 100);
            mk.style.left = l0 + "%";
            mk.style.width = Math.max(r0 - l0, 0.8) + "%";
          } else {
            mk.style.left = left + "%";
          }
        }
      }
      for (var t = 0; t < ticks.length; t++) {
        var lab = clock(a + (b - a) * t / (ticks.length - 1));
        ticks[t].textContent = (b - a) < 600 ? lab : lab.slice(0, 5);
      }
      var whole = a <= 0 && b >= d.length;
      if (readout) readout.textContent = whole ? "Whole battle, " + span(d.length) : clock(a) + " to " + clock(b) + " EVE, " + span(b - a);
      if (allBtn) allBtn.hidden = whole;
      if (outBtn) outBtn.disabled = whole;
    }
    function zoomTo(a, b) { apply(a, b); }

    strip.addEventListener("click", function (ev) {
      var btn = ev.target.closest ? ev.target.closest("[data-zoom]") : null;
      if (!btn || !strip.contains(btn)) return;
      var mid = (win[0] + win[1]) / 2, half = (win[1] - win[0]) / 2;
      switch (btn.getAttribute("data-zoom")) {
        case "in": zoomTo(mid - half / 2, mid + half / 2); break;
        case "out": zoomTo(mid - half * 2, mid + half * 2); break;
        case "all": zoomTo(0, d.length); break;
        case "moment":
          var m = panel && panel.querySelector("[data-moment-from]");
          if (m) {
            var f = parseFloat(m.getAttribute("data-moment-from")), to = parseFloat(m.getAttribute("data-moment-to"));
            var pad = Math.max(20, (to - f) / 2);
            zoomTo(f - pad, to + pad);
          }
          break;
      }
    });
    strip.addEventListener("keydown", function (ev) {
      if (ev.target.matches && ev.target.matches("input, textarea, select")) return;
      var b = null;
      if (ev.key === "+" || ev.key === "=") b = "in";
      else if (ev.key === "-") b = "out";
      else if (ev.key === "0") b = "all";
      if (!b) return;
      var btn = strip.querySelector('[data-zoom="' + b + '"]');
      if (btn) { ev.preventDefault(); btn.click(); }
    });

    // A drag across the chart picks a window.
    if (chart && brush) {
      var from = null;
      var secAt = function (ev) {
        var r = chart.getBoundingClientRect();
        var raw = Math.min(Math.max((ev.clientX - r.left) / r.width, 0), 1);
        return win[0] + raw * (win[1] - win[0]);
      };
      chart.addEventListener("pointerdown", function (ev) {
        if (ev.pointerType !== "mouse" || ev.button !== 0) return;
        from = secAt(ev);
      });
      chart.addEventListener("pointermove", function (ev) {
        if (from === null) return;
        var to = secAt(ev);
        var a = Math.min(from, to), b = Math.max(from, to);
        if (b - a < 4) { brush.setAttribute("hidden", ""); return; }
        brush.setAttribute("x", (a / d.length * 600).toFixed(2));
        brush.setAttribute("width", ((b - a) / d.length * 600).toFixed(2));
        var vb = chart.getAttribute("viewBox").split(" ");
        brush.setAttribute("y", vb[1]);
        brush.setAttribute("height", vb[3]);
        brush.removeAttribute("hidden");
      });
      var finish = function (ev) {
        if (from === null) return;
        var to = secAt(ev), a = Math.min(from, to), b = Math.max(from, to);
        from = null;
        brush.setAttribute("hidden", "");
        if (b - a >= 20) zoomTo(a, b);
      };
      chart.addEventListener("pointerup", finish);
      chart.addEventListener("pointerleave", function () { from = null; brush.setAttribute("hidden", ""); });
    }

    // The open moment can be framed.
    var showMomentBtn = function () {
      if (momentBtn) momentBtn.hidden = !(panel && panel.querySelector("[data-moment-from]"));
    };
    showMomentBtn();
    if (panel && window.MutationObserver) new MutationObserver(showMomentBtn).observe(panel, { childList: true });
    apply(0, d.length);
  }

  // This script loads only with the report and the list, after the shell's first
  // content-ready: run for the document as it stands, then for every
  // swap that brings one back.
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { init(document); });
  } else {
    init(document);
  }
  if (window.EveSynapseContent && window.EveSynapseContent.onReady) {
    window.EveSynapseContent.onReady(init);
  }
})();
