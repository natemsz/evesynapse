// Damage at range: the chart behind the workbench's DPS sparkline.
// The server works out every figure (fitview.FitRange): the stats
// block carries them in data-fit-range, a distance axis and, per
// target, the total and each weapon group's damage per second.
// This file only draws. The chart is a manual popover in the top
// layer, built once and kept on the body, so a re-simulate that
// replaces the stats block leaves it open; fit.js calls
// FitRange.refresh() after each one to redraw from the fresh data.
var FitRange = (function () {
  var SVGNS = "http://www.w3.org/2000/svg";
  var dlg = null, plot = null, readout = null, legend = null, table = null, picks = null, opener = null;
  var data = null, target = "any", at = 0;

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
  function km(v) { return (Math.round(v * 10) / 10).toLocaleString() + " km"; }

  function build() {
    dlg = el("dialog", "fit-range");
    dlg.setAttribute("popover", "manual");
    dlg.setAttribute("aria-labelledby", "fit-range-title");
    var head = el("div", "fit-info-head");
    var close = el("button", "fit-info-close", "Close");
    close.type = "button";
    close.addEventListener("click", hide);
    var names = el("div", "fit-info-names");
    var h = el("h2", null, "Damage at range");
    h.id = "fit-range-title";
    names.appendChild(h);
    names.appendChild(el("span", "fit-info-sub", "Expected damage per second by distance from your ship"));
    head.appendChild(close);
    head.appendChild(names);
    picks = el("div", "fit-range-targets");
    picks.setAttribute("role", "group");
    picks.setAttribute("aria-label", "Target");
    readout = el("p", "fit-range-readout");
    readout.setAttribute("aria-live", "polite");
    plot = el("div", "fit-range-plot");
    legend = el("ul", "fit-range-legend");
    var details = el("details", "fit-range-tablewrap");
    details.appendChild(el("summary", null, "Table"));
    table = el("table", "fit-range-table");
    details.appendChild(table);
    var body = el("div", "fit-range-body");
    body.appendChild(picks);
    body.appendChild(readout);
    body.appendChild(plot);
    body.appendChild(legend);
    body.appendChild(details);
    dlg.appendChild(head);
    dlg.appendChild(body);
    document.body.appendChild(dlg);
    if (window.ResizeObserver) new ResizeObserver(function () { if (isOpen()) draw(); }).observe(plot);
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
    var stats = document.querySelector(".fit-stats[data-fit-range]");
    if (!stats) return null;
    try { return JSON.parse(stats.getAttribute("data-fit-range")); } catch (e) { return null; }
  }

  function open(from) {
    data = read();
    if (!data) return;
    if (!dlg) build();
    if (!dlg.showPopover) return;
    opener = from || document.activeElement;
    if (!isOpen()) dlg.showPopover();
    render();
    var first = picks.querySelector("[aria-pressed=true]");
    if (first) try { first.focus({ preventScroll: true }); } catch (e) {}
  }

  // A fresh simulation: redraw from its data, or close when the
  // fit no longer does any damage.
  function refresh() {
    if (!isOpen()) return;
    data = read();
    if (!data) { hide(); return; }
    render();
  }

  function curves() { return (data.curves && data.curves[target]) || data.curves.any; }

  function render() {
    if (!data.curves[target]) target = "any";
    picks.textContent = "";
    data.targets.forEach(function (t) {
      var b = el("button", "fit-range-target");
      b.type = "button";
      b.setAttribute("aria-pressed", t.key === target ? "true" : "false");
      b.appendChild(el("b", null, t.label));
      b.appendChild(el("small", null, t.note));
      b.addEventListener("click", function () { target = t.key; render(); });
      picks.appendChild(b);
    });
    if (at >= data.km.length) at = 0;
    legendRows();
    tableRows();
    draw();
  }

  function series() {
    var out = [{ label: "Total", reach: "", cls: "fr-total" }];
    data.groups.forEach(function (g, i) { out.push({ label: g.label, reach: g.reach, cls: "fr-s" + (i + 1) }); });
    return out;
  }

  function lineKey(cls) {
    var k = svg("svg", { "class": "fit-range-key", viewBox: "0 0 16 4", "aria-hidden": "true" });
    k.appendChild(svg("line", { x1: 0, y1: 2, x2: 16, y2: 2, "class": cls }));
    return k;
  }

  function legendRows() {
    legend.textContent = "";
    var rows = series();
    if (rows.length === 2) rows = rows.slice(1); // one weapon group: its line is the total
    rows.forEach(function (s) {
      var li = el("li");
      li.appendChild(lineKey(s.cls));
      li.appendChild(el("span", "fit-range-name", s.label));
      if (s.reach) li.appendChild(el("small", null, s.reach));
      legend.appendChild(li);
    });
  }

  function tableRows() {
    table.textContent = "";
    var rows = series(), c = curves();
    var thead = el("thead"), tr = el("tr");
    tr.appendChild(el("th", null, "Distance"));
    rows.forEach(function (s) { tr.appendChild(el("th", null, s.label)); });
    thead.appendChild(tr);
    table.appendChild(thead);
    var tbody = el("tbody");
    var step = data.end / 10;
    for (var t = 0; t <= 10; t++) {
      var i = nearest(t * step);
      var r = el("tr");
      r.appendChild(el("td", null, km(data.km[i])));
      c.forEach(function (line) { r.appendChild(el("td", null, fmt(line[i]))); });
      tbody.appendChild(r);
    }
    table.appendChild(tbody);
  }

  function nearest(k) {
    var best = 0;
    for (var i = 1; i < data.km.length; i++) {
      if (Math.abs(data.km[i] - k) < Math.abs(data.km[best] - k)) best = i;
    }
    return best;
  }

  // niceStep rounds a span's quarter up to 1, 2, 2.5 or 5 times a
  // power of ten, for gridlines.
  function niceStep(span) {
    var raw = span / 4, p = Math.pow(10, Math.floor(Math.log10(raw)));
    var ms = [1, 2, 2.5, 5, 10];
    for (var i = 0; i < ms.length; i++) if (raw <= ms[i] * p) return ms[i] * p;
    return 10 * p;
  }

  function draw() {
    plot.textContent = "";
    var W = Math.max(plot.clientWidth, 260), H = W < 520 ? 220 : 300;
    var L = 48, R = 14, T = 12, B = 30;
    var c = curves();
    // One scale for every target, so switching target shows what
    // the fit loses rather than rescaling it away.
    var peak = 0;
    data.curves.any[0].forEach(function (v) { peak = Math.max(peak, v); });
    var yStep = niceStep(peak || 1), yMax = Math.ceil((peak || 1) / yStep) * yStep;
    function X(k) { return L + (W - L - R) * k / data.end; }
    function Y(v) { return T + (H - T - B) * (1 - v / yMax); }
    var root = svg("svg", { viewBox: "0 0 " + W + " " + H, width: W, height: H, "class": "fit-range-svg", role: "img", tabindex: "0",
      "aria-label": "Damage per second by distance, 0 to " + km(data.end) + ". Arrow keys move the reading." });
    for (var v = 0; v <= yMax + 1e-9; v += yStep) {
      root.appendChild(svg("line", { x1: L, x2: W - R, y1: Y(v), y2: Y(v), "class": "fit-range-grid" }));
      var ty = svg("text", { x: L - 6, y: Y(v) + 4, "class": "fit-range-tick", "text-anchor": "end" });
      ty.textContent = fmt(v);
      root.appendChild(ty);
    }
    var xStep = niceStep(data.end);
    for (var k = 0; k <= data.end + 1e-9; k += xStep) {
      var tx = svg("text", { x: X(k), y: H - 10, "class": "fit-range-tick", "text-anchor": k === 0 ? "start" : "middle" });
      tx.textContent = (Math.round(k * 10) / 10) + (k === 0 ? " km" : "");
      root.appendChild(tx);
    }
    // Weapon lines first, the total on top.
    var rows = series();
    for (var s = c.length - 1; s >= 0; s--) {
      if (s > 0 && c.length === 2) continue; // one group: draw only the total
      var pts = data.km.map(function (km, i) { return X(km).toFixed(1) + "," + Y(c[s][i]).toFixed(1); }).join(" ");
      root.appendChild(svg("polyline", { points: pts, "class": "fit-range-line " + rows[s].cls }));
    }
    var cross = svg("line", { y1: T, y2: H - B, "class": "fit-range-cross" });
    root.appendChild(cross);
    var hit = svg("rect", { x: L, y: T, width: W - L - R, height: H - T - B, "class": "fit-range-hit" });
    root.appendChild(hit);
    function place(i) {
      at = i;
      var x = X(data.km[i]);
      cross.setAttribute("x1", x);
      cross.setAttribute("x2", x);
      reading();
    }
    hit.addEventListener("pointermove", function (ev) {
      var box = root.getBoundingClientRect();
      var k = (ev.clientX - box.left) / box.width * W;
      place(nearest((k - L) / (W - L - R) * data.end));
    });
    root.addEventListener("keydown", function (ev) {
      if (ev.key === "ArrowRight" || ev.key === "ArrowLeft") {
        ev.preventDefault();
        place(Math.max(0, Math.min(data.km.length - 1, at + (ev.key === "ArrowRight" ? 1 : -1))));
      }
    });
    plot.appendChild(root);
    place(at);
  }

  // The readout: every line's value at the crosshair, values first.
  function reading() {
    var c = curves(), rows = series();
    readout.textContent = "";
    readout.appendChild(el("span", "fit-range-at", "At " + km(data.km[at])));
    rows.forEach(function (s, i) {
      if (i > 0 && c.length === 2) return;
      var item = el("span", "fit-range-val");
      item.appendChild(lineKey(s.cls));
      item.appendChild(el("b", null, fmt(c[i][at])));
      item.appendChild(document.createTextNode(" " + s.label));
      readout.appendChild(item);
    });
  }

  document.addEventListener("click", function (ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest("[data-fit-range-open]") : null;
    if (!t) return;
    ev.preventDefault();
    open(t);
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape" && isOpen()) hide();
  });
  return { open: open, refresh: refresh, close: hide };
})();
