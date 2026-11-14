// Figures over time: the chart behind the workbench's DPS tile when
// the fit carries a module that ramps up cycle by cycle (entropic
// disintegrators, mutadaptive remote armor repairers). The server
// works out every figure (fitview.FitRamp): the stats block carries
// them in data-fit-ramp, a time axis and, per output, its rate at
// each moment of continuous firing and its average since the first
// cycle. This file only draws, in the damage-at-range chart's popover
// look; fit.js calls FitRamp.refresh() after each re-simulate.
var FitRamp = (function () {
  var SVGNS = "http://www.w3.org/2000/svg";
  var dlg = null, body = null, opener = null, data = null, at = 0;

  function el(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function svg(tag, attrs) {
    var e = document.createElementNS(SVGNS, tag);
    for (var k in attrs) e.setAttribute(k, attrs[k]);
    return e;
  }
  function fmt(v) { return v >= 100 ? Math.round(v).toLocaleString() : (Math.round(v * 10) / 10).toLocaleString(); }
  function secs(v) {
    v = Math.round(v);
    if (v < 60) return v + "s";
    var s = v % 60;
    return Math.floor(v / 60) + "m " + (s < 10 ? "0" : "") + s + "s";
  }

  function build() {
    dlg = el("dialog", "fit-range fit-ramp");
    dlg.setAttribute("popover", "manual");
    dlg.setAttribute("aria-labelledby", "fit-ramp-title");
    var head = el("div", "fit-info-head");
    var close = el("button", "fit-info-close", "Close");
    close.type = "button";
    close.addEventListener("click", hide);
    var names = el("div", "fit-info-names");
    var h = el("h2", null, "Output over time");
    h.id = "fit-ramp-title";
    names.appendChild(h);
    names.appendChild(el("span", "fit-info-sub", "From the first cycle, firing without a break"));
    head.appendChild(close);
    head.appendChild(names);
    body = el("div", "fit-range-body");
    dlg.appendChild(head);
    dlg.appendChild(body);
    document.body.appendChild(dlg);
    if (window.ResizeObserver) new ResizeObserver(function () { if (isOpen()) render(); }).observe(body);
  }

  function isOpen() { return !!dlg && dlg.matches(":popover-open"); }
  function hide() {
    if (!isOpen()) return;
    dlg.hidePopover();
    if (opener && opener.isConnected && opener.focus) {
      try { opener.focus({ preventScroll: true }); } catch (e) {}
    }
    opener = null;
  }
  function read() {
    var stats = document.querySelector(".fit-stats[data-fit-ramp]");
    if (!stats) return null;
    try { return JSON.parse(stats.getAttribute("data-fit-ramp")); } catch (e) { return null; }
  }
  function open(from) {
    data = read();
    if (!data) return;
    if (!dlg) build();
    if (!dlg.showPopover) return;
    opener = from || document.activeElement;
    if (!isOpen()) dlg.showPopover();
    render();
    var first = body.querySelector(".fit-range-svg");
    if (first) try { first.focus({ preventScroll: true }); } catch (e) {}
  }
  function refresh() {
    if (!isOpen()) return;
    data = read();
    if (!data) { hide(); return; }
    render();
  }

  function niceStep(span) {
    var raw = span / 4, p = Math.pow(10, Math.floor(Math.log10(raw)));
    var ms = [1, 2, 2.5, 5, 10];
    for (var i = 0; i < ms.length; i++) if (raw <= ms[i] * p) return ms[i] * p;
    return 10 * p;
  }
  // timeStep is the time axis's gridline step: seconds and minutes in
  // the steps a clock reads (the server ends the axis on one).
  var TIME_STEPS = [5, 10, 15, 30, 60, 120, 300, 600];
  function timeStep(span) {
    for (var i = 0; i < TIME_STEPS.length; i++) if (span / 4 <= TIME_STEPS[i]) return TIME_STEPS[i];
    return 600;
  }
  function nearest(t) {
    var best = 0;
    for (var i = 1; i < data.t.length; i++) {
      if (Math.abs(data.t[i] - t) < Math.abs(data.t[best] - t)) best = i;
    }
    return best;
  }
  function lineKey(cls) {
    var k = svg("svg", { "class": "fit-range-key", viewBox: "0 0 16 4", "aria-hidden": "true" });
    k.appendChild(svg("line", { x1: 0, y1: 2, x2: 16, y2: 2, "class": cls }));
    return k;
  }

  var LINES = [{ key: "now", label: "now", cls: "fr-s1" }, { key: "avg", label: "average so far", cls: "fr-total" }];

  function render() {
    body.textContent = "";
    var readouts = [], single = data.charts.length === 1;
    dlg.querySelector("#fit-ramp-title").textContent = single ? data.charts[0].title : "Output over time";
    data.charts.forEach(function (c) {
      var sec = el("section", "fit-ramp-chart");
      if (!single) sec.appendChild(el("h3", "fit-ramp-title", c.title));
      sec.appendChild(el("p", "fit-ramp-sum", fmt(c.start) + " " + c.unit + " at the start, " + fmt(c.full) + " " + c.unit +
        " at full bonus after " + secs(c.fullAt) + " of firing."));
      var readout = el("p", "fit-range-readout");
      readout.setAttribute("aria-live", "polite");
      var plot = el("div", "fit-range-plot");
      sec.appendChild(readout);
      sec.appendChild(plot);
      var legend = el("ul", "fit-range-legend");
      LINES.forEach(function (l) {
        var li = el("li");
        li.appendChild(lineKey(l.cls));
        li.appendChild(el("span", "fit-range-name", c.unit + " " + l.label));
        legend.appendChild(li);
      });
      sec.appendChild(legend);
      body.appendChild(sec);
      readouts.push(draw(c, plot, readout));
    });
    function placeAll(i) { readouts.forEach(function (f) { f(i); }); }
    placeAll(Math.min(at, data.t.length - 1));
    body.__place = placeAll;
  }

  // draw renders one chart and returns its crosshair setter; every
  // chart shares the reading, so the crosshair moves on all of them.
  function draw(c, plot, readout) {
    var W = Math.max(body.clientWidth - 8, 260), H = W < 520 ? 200 : 260;
    var L = 48, R = 14, T = 12, B = 30;
    var peak = 0;
    c.now.forEach(function (v) { peak = Math.max(peak, v); });
    var yStep = niceStep(peak || 1), yMax = Math.ceil((peak || 1) / yStep) * yStep;
    function X(t) { return L + (W - L - R) * t / data.end; }
    function Y(v) { return T + (H - T - B) * (1 - v / yMax); }
    var root = svg("svg", { viewBox: "0 0 " + W + " " + H, width: W, height: H, "class": "fit-range-svg", role: "img", tabindex: "0",
      "aria-label": c.title + ", 0 to " + secs(data.end) + ". Arrow keys move the reading." });
    for (var v = 0; v <= yMax + 1e-9; v += yStep) {
      root.appendChild(svg("line", { x1: L, x2: W - R, y1: Y(v), y2: Y(v), "class": "fit-range-grid" }));
      var ty = svg("text", { x: L - 6, y: Y(v) + 4, "class": "fit-range-tick", "text-anchor": "end" });
      ty.textContent = fmt(v);
      root.appendChild(ty);
    }
    var xStep = timeStep(data.end);
    for (var s = 0; s <= data.end + 1e-9; s += xStep) {
      var last = s + xStep > data.end + 1e-9;
      var tx = svg("text", { x: X(s), y: H - 10, "class": "fit-range-tick", "text-anchor": s === 0 ? "start" : (last ? "end" : "middle") });
      tx.textContent = s === 0 ? "0 s" : secs(s);
      root.appendChild(tx);
    }
    var full = svg("line", { x1: X(c.fullAt), x2: X(c.fullAt), y1: T, y2: H - B, "class": "fit-ramp-full" });
    root.appendChild(full);
    for (var li = LINES.length - 1; li >= 0; li--) {
      var ys = c[LINES[li].key];
      var pts = data.t.map(function (t, i) { return X(t).toFixed(1) + "," + Y(ys[i]).toFixed(1); }).join(" ");
      root.appendChild(svg("polyline", { points: pts, "class": "fit-range-line " + LINES[li].cls }));
    }
    var cross = svg("line", { y1: T, y2: H - B, "class": "fit-range-cross" });
    root.appendChild(cross);
    var hit = svg("rect", { x: L, y: T, width: W - L - R, height: H - T - B, "class": "fit-range-hit" });
    root.appendChild(hit);
    function place(i) {
      var x = X(data.t[i]);
      cross.setAttribute("x1", x);
      cross.setAttribute("x2", x);
      readout.textContent = "";
      readout.appendChild(el("span", "fit-range-at", "At " + secs(data.t[i])));
      LINES.forEach(function (l) {
        var item = el("span", "fit-range-val");
        item.appendChild(lineKey(l.cls));
        item.appendChild(el("b", null, fmt(c[l.key][i])));
        item.appendChild(document.createTextNode(" " + c.unit + " " + l.label));
        readout.appendChild(item);
      });
    }
    function move(i) { at = i; if (body.__place) body.__place(i); else place(i); }
    hit.addEventListener("pointermove", function (ev) {
      var box = root.getBoundingClientRect();
      var px = (ev.clientX - box.left) / box.width * W;
      move(nearest((px - L) / (W - L - R) * data.end));
    });
    root.addEventListener("keydown", function (ev) {
      if (ev.key === "ArrowRight" || ev.key === "ArrowLeft") {
        ev.preventDefault();
        move(Math.max(0, Math.min(data.t.length - 1, at + (ev.key === "ArrowRight" ? 1 : -1))));
      }
    });
    plot.appendChild(root);
    return place;
  }

  document.addEventListener("click", function (ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest("[data-fit-ramp-open]") : null;
    if (!t) return;
    ev.preventDefault();
    open(t);
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape" && isOpen()) hide();
  });
  return { open: open, refresh: refresh, close: hide };
})();
