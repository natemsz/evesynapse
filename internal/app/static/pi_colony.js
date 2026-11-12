// pi_colony.js: the PI planner's colony view. A stored plan's colony
// opens on its planet, drawn on a canvas: structures, the links
// between them and the goods moving along them. Structures can be
// dragged, added, removed and retooled; every change goes to the
// server (POST /industry/pi/colony), which lays links and routes
// again and answers with the fit, the rates and the game template,
// so the view always shows the planner's own math. Colonies the
// player shaped can be kept while the rest of the plan is solved
// again around them.
(function () {
  "use strict";

  var TAU = Math.PI * 2;
  var KIND_GLYPH = { cc: "CC", ecu: "EX", basic: "B", advanced: "A", hightech: "HT", storage: "S", launchpad: "LP" };

  function css(el, name, fallback) {
    var v = getComputedStyle(el).getPropertyValue(name).trim();
    return v || fallback;
  }

  function clone(o) { return JSON.parse(JSON.stringify(o)); }

  function fmt(n) {
    return Math.round(n).toLocaleString("en-US");
  }

  // Deterministic value noise for the planet surface. The lattice wraps
  // at a period so the texture tiles without seams or mirrored shapes.
  function hash(x, y, seed) {
    var h = (x * 374761393 + y * 668265263 + seed * 2147483647) | 0;
    h = (h ^ (h >>> 13)) * 1274126177;
    return ((h ^ (h >>> 16)) >>> 0) / 4294967295;
  }
  function smooth(t) { return t * t * (3 - 2 * t); }
  function wrap(i, p) { return ((i % p) + p) % p; }
  function noise(x, y, seed, p) {
    var xi = Math.floor(x), yi = Math.floor(y);
    var xf = x - xi, yf = y - yi;
    var x0 = wrap(xi, p), x1 = wrap(xi + 1, p), y0 = wrap(yi, p), y1 = wrap(yi + 1, p);
    var a = hash(x0, y0, seed), b = hash(x1, y0, seed);
    var c = hash(x0, y1, seed), d = hash(x1, y1, seed);
    var u = smooth(xf), v = smooth(yf);
    return a + (b - a) * u + (c - a) * v + (a - b - c + d) * u * v;
  }
  // fbm over [0,1) x [0,1), tiling with base period p.
  function fbm(x, y, seed, p, ridged) {
    var s = 0, amp = 0.5, f = p, norm = 0;
    for (var i = 0; i < 5; i++) {
      var n = noise(x * f, y * f, seed + i * 31, f);
      if (ridged) n = 1 - Math.abs(2 * n - 1);
      s += amp * n; norm += amp; f *= 2; amp *= 0.5;
    }
    return s / norm;
  }
  function mix(a, b, t) {
    t = Math.max(0, Math.min(1, t));
    return [a[0] + (b[0] - a[0]) * t, a[1] + (b[1] - a[1]) * t, a[2] + (b[2] - a[2]) * t];
  }
  function mul(a, k) { return [a[0] * k, a[1] * k, a[2] * k]; }
  function band(t, lo, hi) { return Math.max(0, Math.min(1, (t - lo) / (hi - lo))); }
  // The colour of one surface texel: f is broad fbm, r ridged fbm (thin
  // lines where it peaks), v the vertical position for banded worlds.
  function shade(type, base, f, r, v) {
    switch (type) {
    case "lava":
      var crust = mix([34, 26, 24], mul(base, 0.7), f * 1.1);
      var glow = Math.pow(r, 9) * 1.3 + band(f, 0.66, 0.78) * 0.85;
      return mix(mix(crust, [225, 80, 20], glow), [255, 190, 80], Math.pow(r, 30));
    case "ice":
      var snow = mix(base, [225, 238, 246], 0.35 + f * 0.5);
      return mix(snow, mul(base, 0.75), Math.pow(r, 10) * 0.9);
    case "gas":
    case "storm":
      var warp = type === "storm" ? 2.6 : 1.4;
      var t = 0.5 + 0.5 * Math.sin((v * 5 + f * warp) * TAU);
      var c = mix(mul(base, 0.7), mul(base, 1.35), t);
      if (type === "storm") c = mix(c, [205, 220, 240], Math.pow(r, 12) * 0.8);
      return c;
    case "oceanic":
      var sea = mix(mul(base, 0.6), mul(base, 1.3), f * 1.2 - 0.1);
      return mix(sea, [150, 140, 95], band(f, 0.7, 0.74));
    case "temperate":
      if (f < 0.47) return mix([22, 52, 88], [45, 95, 130], band(f, 0.3, 0.47));
      var land = mix(mul(base, 1.15), mul(base, 0.75), band(f, 0.47, 0.62));
      return mix(land, [125, 112, 92], band(f, 0.64, 0.75));
    case "plasma":
      var dark = mul(base, 0.45 + f * 0.4);
      return mix(mix(dark, [215, 120, 235], Math.pow(r, 6)), [255, 225, 255], Math.pow(r, 16));
    default: // barren and anything unknown
      var stone = mul(base, 0.55 + f * 0.75);
      return mix(stone, mul(base, 0.45), Math.pow(r, 9) * 0.6);
    }
  }
  function parseColor(c) {
    var ctx = document.createElement("canvas").getContext("2d");
    ctx.fillStyle = c;
    var v = ctx.fillStyle;
    if (v.charAt(0) === "#") {
      return [parseInt(v.substr(1, 2), 16), parseInt(v.substr(3, 2), 16), parseInt(v.substr(5, 2), 16)];
    }
    var m = v.match(/\d+(\.\d+)?/g) || [0, 0, 0];
    return [+m[0], +m[1], +m[2]];
  }

  function View(root) {
    this.root = root;
    this.token = root.getAttribute("data-plan");
    this.panel = document.getElementById("pi-view");
    this.canvas = document.getElementById("pi-canvas");
    this.ctx = this.canvas.getContext("2d");
    this.tip = document.getElementById("pi-tip");
    this.grid = root.querySelector(".pi-plan-grid");
    this.edited = {};    // colony index -> edited colony
    this.answers = {};   // colony index -> last server answer
    this.peaks = {};     // colony index -> {raw: survey peak}
    this.sel = -1;
    this.hover = -1;
    this.index = -1;
    this.t0 = performance.now();
    this.textures = {};
    var self = this;
    fetch("/industry/pi/plan.json?plan=" + encodeURIComponent(this.token), { credentials: "same-origin" })
      .then(function (r) { if (!r.ok) throw new Error("plan"); return r.json(); })
      .then(function (j) {
        self.catalog = j.catalog;
        self.plan = j.plan;
        self.wire();
        var want = parseInt((location.hash.match(/colony-(\d+)/) || [])[1], 10);
        var wide = self.grid.clientWidth > 900;
        if (!isNaN(want)) self.open(want);
        else if (wide && self.plan.solution.colonies.length) self.open(self.plan.solution.colonies[0].index);
      })
      .catch(function () { /* the board stays usable without the view */ });
  }

  View.prototype.wire = function () {
    var self = this;
    this.root.addEventListener("click", function (ev) {
      var open = ev.target.closest("[data-open-colony]");
      if (open) { self.open(parseInt(open.getAttribute("data-open-colony"), 10)); return; }
      if (ev.target.closest("[data-colony-back]")) { self.close(); return; }
      var add = ev.target.closest("[data-add]");
      if (add) { self.addPin(add.getAttribute("data-add")); return; }
      if (ev.target.closest("[data-copy-template]")) { self.copyTemplate(); return; }
      if (ev.target.closest("[data-replan]")) { self.replan(); return; }
      if (ev.target.closest("[data-colony-reset]")) { self.reset(); return; }
      if (ev.target.closest("[data-arrange]")) { self.arrange(); return; }
      if (ev.target.closest("[data-jump-cc]")) { self.jumpCC(); return; }
      var rm = ev.target.closest("[data-remove-pin]");
      if (rm) { self.removePin(self.sel); return; }
    });
    document.getElementById("pi-view-cc").addEventListener("change", function (ev) {
      if (!self.colony) return;
      self.colony.ccLevel = parseInt(ev.target.value, 10);
      self.changed();
    });
    var c = this.canvas;
    this.pointers = {};
    c.addEventListener("pointerdown", function (ev) { self.down(ev); });
    c.addEventListener("pointermove", function (ev) { self.move(ev); });
    c.addEventListener("pointerup", function (ev) { self.up(ev); });
    c.addEventListener("pointercancel", function (ev) { self.up(ev); });
    c.addEventListener("pointerleave", function () { self.hover = -1; self.tip.hidden = true; });
    c.addEventListener("wheel", function (ev) {
      ev.preventDefault();
      var f = Math.exp(-ev.deltaY * 0.0015);
      self.zoomAt(ev.offsetX, ev.offsetY, f);
    }, { passive: false });
    c.tabIndex = 0;
    c.addEventListener("keydown", function (ev) { self.key(ev); });
    if (window.ResizeObserver) {
      new ResizeObserver(function () { self.resize(); }).observe(c);
    }
    var loop = function () {
      if (self.colony && !self.panel.hidden && document.contains(self.canvas)) self.draw();
      if (document.contains(self.canvas)) requestAnimationFrame(loop);
    };
    requestAnimationFrame(loop);
  };

  View.prototype.colonyByIndex = function (i) {
    var cols = this.plan.solution.colonies;
    for (var k = 0; k < cols.length; k++) if (cols[k].index === i) return cols[k];
    return null;
  };

  View.prototype.open = function (i) {
    var pc = this.colonyByIndex(i);
    if (!pc) return;
    this.index = i;
    this.pc = pc;
    this.colony = clone(this.edited[i] || pc.colony);
    this.sel = -1;
    this.panel.hidden = false;
    this.grid.classList.add("viewing");
    this.root.querySelectorAll(".pi-card").forEach(function (el) {
      el.classList.toggle("on", parseInt(el.getAttribute("data-colony"), 10) === i);
    });
    var cardName = this.root.querySelector('.pi-card[data-colony="' + i + '"] .pi-card-top b');
    var name = cardName ? cardName.textContent : pc.planetName;
    document.getElementById("pi-view-title").textContent = name + (pc.planetId ? " (" + pc.planetType + ")" : "");
    document.getElementById("pi-view-cc").value = String(this.colony.ccLevel);
    if (history.replaceState) history.replaceState(null, "", location.pathname + location.search + "#colony-" + i);
    this.resize();
    this.fitView();
    this.toolbar();
    this.inspect();
    if (this.answers[i]) this.show(this.answers[i]);
    else this.recompute();
    if (this.grid.clientWidth <= 900) this.panel.scrollIntoView({ block: "start" });
  };

  View.prototype.close = function () {
    this.panel.hidden = true;
    this.grid.classList.remove("viewing");
    var card = this.root.querySelector('.pi-card[data-colony="' + this.index + '"] .pi-card-open');
    if (card) card.focus();
  };

  View.prototype.toolbar = function () {
    var holds = (this.catalog.holds[this.colony.planetType] || []);
    this.root.querySelectorAll("[data-add]").forEach(function (b) {
      b.hidden = holds.indexOf(b.getAttribute("data-add")) < 0;
    });
  };

  // --- projection -------------------------------------------------------

  View.prototype.resize = function () {
    var c = this.canvas;
    var dpr = window.devicePixelRatio || 1;
    var w = c.clientWidth, h = c.clientHeight;
    if (!w || !h) return;
    if (c.width !== Math.round(w * dpr) || c.height !== Math.round(h * dpr)) {
      c.width = Math.round(w * dpr);
      c.height = Math.round(h * dpr);
    }
    this.dpr = dpr;
    this.w = w;
    this.h = h;
  };

  // fitView frames the working structures. The command center counts only
  // when nothing else is down: it often sits far from the cluster.
  View.prototype.fitView = function () {
    var all = this.colony.pins;
    this.ccSaved = null;
    this.ccButton();
    if (!all.length) { this.center = { lat: Math.PI / 2, lon: 0 }; this.sx = 1; this.scale = 4000; return; }
    var pins = all.filter(function (p) { return p.kind !== "cc"; });
    if (!pins.length) pins = all;
    var minLa = 9, maxLa = -9, minLo = 9, maxLo = -9;
    pins.forEach(function (p) {
      minLa = Math.min(minLa, p.lat); maxLa = Math.max(maxLa, p.lat);
      minLo = Math.min(minLo, p.lon); maxLo = Math.max(maxLo, p.lon);
    });
    this.center = { lat: (minLa + maxLa) / 2, lon: (minLo + maxLo) / 2 };
    // One east-west factor per colony, so panning never stretches the map.
    this.sx = Math.max(0.2, Math.sin(this.center.lat) || 1);
    var spanX = Math.max((maxLo - minLo) * this.sx, 0.04), spanY = Math.max(maxLa - minLa, 0.04);
    this.scale = Math.max(800, Math.min(60000, Math.min((this.w || 600) / (spanX * 1.5), (this.h || 400) / (spanY * 1.5))));
  };

  // jumpCC centres on the command center; pressed again it returns to
  // the view from before the jump.
  View.prototype.jumpCC = function () {
    if (this.ccSaved) {
      this.center = this.ccSaved.center;
      this.scale = this.ccSaved.scale;
      this.ccSaved = null;
    } else {
      var cc = null;
      this.colony.pins.forEach(function (p) { if (p.kind === "cc") cc = p; });
      if (!cc) { this.say("This colony has no command center yet."); return; }
      this.ccSaved = { center: { lat: this.center.lat, lon: this.center.lon }, scale: this.scale };
      this.center = { lat: cc.lat, lon: cc.lon };
    }
    this.ccButton();
  };

  View.prototype.ccButton = function () {
    var b = this.root.querySelector("[data-jump-cc]");
    if (!b) return;
    b.textContent = this.ccSaved ? "Back to the colony" : "Command center";
    b.setAttribute("aria-pressed", this.ccSaved ? "true" : "false");
  };

  View.prototype.toScreen = function (lat, lon) {
    var s = this.sx || 1;
    return {
      x: this.w / 2 + (lon - this.center.lon) * s * this.scale,
      y: this.h / 2 + (lat - this.center.lat) * this.scale
    };
  };

  View.prototype.toPlanet = function (x, y) {
    var s = this.sx || 1;
    return {
      lat: this.center.lat + (y - this.h / 2) / this.scale,
      lon: this.center.lon + (x - this.w / 2) / (s * this.scale)
    };
  };

  View.prototype.zoomAt = function (x, y, f) {
    var before = this.toPlanet(x, y);
    this.scale = Math.max(800, Math.min(60000, this.scale * f));
    var after = this.toPlanet(x, y);
    this.center.lat += before.lat - after.lat;
    this.center.lon += before.lon - after.lon;
  };

  View.prototype.pinRadius = function () {
    return Math.max(9, Math.min(22, this.scale * 0.0045));
  };

  View.prototype.pinAt = function (x, y) {
    var r = this.pinRadius() + 3;
    var pins = this.colony.pins;
    for (var i = pins.length - 1; i >= 0; i--) {
      var p = this.toScreen(pins[i].lat, pins[i].lon);
      if ((p.x - x) * (p.x - x) + (p.y - y) * (p.y - y) <= r * r) return i;
    }
    return -1;
  };

  // --- pointer ---------------------------------------------------------

  View.prototype.down = function (ev) {
    if (!this.colony) return;
    this.canvas.setPointerCapture(ev.pointerId);
    this.pointers[ev.pointerId] = { x: ev.offsetX, y: ev.offsetY };
    if (Object.keys(this.pointers).length >= 2) {
      // A second finger: pinch to zoom, both fingers pan. A structure
      // the first finger was dragging stays where it is.
      if (this.drag && this.drag.pin >= 0 && this.drag.moved) this.changed();
      else if (this.drag && this.drag.pin >= 0 && this.sel !== this.drag.was) { this.sel = this.drag.was; this.inspect(); }
      this.drag = null;
      this.pinch = this.pinchState();
      return;
    }
    var i = this.pinAt(ev.offsetX, ev.offsetY);
    this.drag = { x: ev.offsetX, y: ev.offsetY, pin: i, moved: false, was: this.sel, center: { lat: this.center.lat, lon: this.center.lon } };
    if (i >= 0) { this.sel = i; this.inspect(); }
  };

  // pinchState is the two first fingers' midpoint and spread.
  View.prototype.pinchState = function () {
    var ids = Object.keys(this.pointers).slice(0, 2);
    var a = this.pointers[ids[0]], b = this.pointers[ids[1]];
    return { x: (a.x + b.x) / 2, y: (a.y + b.y) / 2, dist: Math.max(1, Math.hypot(a.x - b.x, a.y - b.y)) };
  };

  View.prototype.move = function (ev) {
    if (!this.colony) return;
    if (this.pointers[ev.pointerId]) this.pointers[ev.pointerId] = { x: ev.offsetX, y: ev.offsetY };
    if (this.pinch) {
      var was = this.pinch, now = this.pinchState();
      var sx = this.sx || 1;
      this.center.lat -= (now.y - was.y) / this.scale;
      this.center.lon -= (now.x - was.x) / (sx * this.scale);
      this.zoomAt(now.x, now.y, now.dist / was.dist);
      this.pinch = now;
      return;
    }
    var d = this.drag;
    if (!d) {
      var h = this.pinAt(ev.offsetX, ev.offsetY);
      if (h !== this.hover) { this.hover = h; this.showTip(ev.offsetX, ev.offsetY); }
      this.canvas.style.cursor = h >= 0 ? "grab" : "default";
      return;
    }
    var dx = ev.offsetX - d.x, dy = ev.offsetY - d.y;
    if (Math.abs(dx) + Math.abs(dy) > 3) d.moved = true;
    if (!d.moved) return;
    if (d.pin >= 0) {
      var p = this.colony.pins[d.pin];
      if (p.change === "remove") return;
      var at = this.toPlanet(ev.offsetX, ev.offsetY);
      p.lat = at.lat;
      p.lon = at.lon;
      if (p.change === "keep" || p.change === "retool") p.change = p.change; // a moved pin keeps its building
      this.canvas.style.cursor = "grabbing";
    } else {
      var s = this.sx || 1;
      this.center.lat = d.center.lat - dy / this.scale;
      this.center.lon = d.center.lon - dx / (s * this.scale);
    }
  };

  View.prototype.up = function (ev) {
    if (ev && this.pointers) delete this.pointers[ev.pointerId];
    if (this.pinch) {
      // Lifting one finger of a pinch leaves the other panning, not
      // selecting what it rests on.
      var left = Object.keys(this.pointers || {});
      if (left.length >= 2) { this.pinch = this.pinchState(); return; }
      this.pinch = null;
      if (left.length === 1) {
        var q = this.pointers[left[0]];
        this.drag = { x: q.x, y: q.y, pin: -1, moved: true, center: { lat: this.center.lat, lon: this.center.lon } };
      }
      return;
    }
    var d = this.drag;
    this.drag = null;
    if (!d) return;
    if (d.pin >= 0 && d.moved) this.changed();
    else if (d.pin < 0 && !d.moved) { this.sel = -1; this.inspect(); }
  };

  View.prototype.key = function (ev) {
    if (!this.colony) return;
    var step = this.catalog.step / 4;
    var p = this.sel >= 0 ? this.colony.pins[this.sel] : null;
    var moves = { ArrowUp: [-step, 0], ArrowDown: [step, 0], ArrowLeft: [0, -step], ArrowRight: [0, step] };
    if (p && moves[ev.key]) {
      ev.preventDefault();
      p.lat += moves[ev.key][0];
      p.lon += moves[ev.key][1] / (Math.sin(p.lat) || 1);
      clearTimeout(this.keyTimer);
      var self = this;
      this.keyTimer = setTimeout(function () { self.changed(); }, 350);
    } else if (p && (ev.key === "Delete" || ev.key === "Backspace")) {
      ev.preventDefault();
      this.removePin(this.sel);
    } else if (ev.key === "Tab" && !ev.shiftKey && this.colony.pins.length) {
      // Tab walks the structures, then leaves the canvas.
      if (this.sel < this.colony.pins.length - 1) {
        ev.preventDefault();
        this.sel++;
        this.inspect();
      }
    } else if (ev.key === "+" || ev.key === "=") {
      this.zoomAt(this.w / 2, this.h / 2, 1.2);
    } else if (ev.key === "-") {
      this.zoomAt(this.w / 2, this.h / 2, 1 / 1.2);
    }
  };

  View.prototype.showTip = function (x, y) {
    if (this.hover < 0) { this.tip.hidden = true; return; }
    var p = this.colony.pins[this.hover];
    var label = this.catalog.labels[p.kind] || p.kind;
    var lines = [label];
    if (p.product) lines.push(p.product);
    if (p.kind === "ecu") lines.push(p.heads + " heads, " + Math.round((p.programHours || 24)) + " hour program");
    if (p.change && p.change !== "keep") lines.push({ add: "To build", remove: "To remove", retool: "To switch" }[p.change] || "");
    this.tip.textContent = lines.join(" · ");
    this.tip.hidden = false;
    this.tip.style.left = Math.min(x + 14, this.w - 200) + "px";
    this.tip.style.top = (y + 14) + "px";
  };

  // --- edits ------------------------------------------------------------

  View.prototype.addPin = function (kind) {
    if (!this.colony) return;
    var c = this.colony;
    var hub = null;
    c.pins.forEach(function (p) { if (!hub && p.kind === "launchpad" && p.change !== "remove") hub = p; });
    var origin = hub || c.pins[0] || { lat: Math.PI / 2, lon: 0 };
    var step = this.catalog.step;
    // The first free spot on rings around the hub.
    for (var ring = 1; ring < 12; ring++) {
      for (var k = 0; k < ring * 6; k++) {
        var a = (k / (ring * 6)) * TAU;
        var lat = origin.lat + Math.sin(a) * ring * step;
        var lon = origin.lon + Math.cos(a) * ring * step / (Math.sin(origin.lat) || 1);
        var free = c.pins.every(function (p) {
          var dl = p.lat - lat, dn = (p.lon - lon) * Math.sin(lat);
          return Math.sqrt(dl * dl + dn * dn) > step * 0.9;
        });
        if (free) {
          var pin = { kind: kind, lat: lat, lon: lon, change: "add" };
          if (kind === "ecu") {
            var raws = this.catalog.yields[c.planetType] || [];
            pin.product = raws[0] || "";
            pin.heads = 10;
            pin.programHours = 24;
          } else if (this.catalog.recipes[kind]) {
            pin.product = this.catalog.recipes[kind][0];
          }
          c.pins.push(pin);
          this.sel = c.pins.length - 1;
          this.changed();
          this.inspect();
          return;
        }
      }
    }
  };

  View.prototype.removePin = function (i) {
    if (i < 0 || !this.colony) return;
    var p = this.colony.pins[i];
    if (p.kind === "cc") { this.say("The command center stays."); return; }
    if (p.change === "add") this.colony.pins.splice(i, 1);
    else if (p.change === "remove") p.change = "keep";
    else if (p.change) p.change = "remove";
    else this.colony.pins.splice(i, 1);
    this.sel = -1;
    this.changed();
    this.inspect();
  };

  View.prototype.changed = function () {
    this.edited[this.index] = clone(this.colony);
    var card = this.root.querySelector('.pi-card[data-colony="' + this.index + '"]');
    if (card) card.classList.add("edited");
    clearTimeout(this.timer);
    var self = this;
    this.timer = setTimeout(function () { self.recompute(); }, 120);
  };

  // arrange asks the server to move the structures to where their
  // links cost the least, then shows the result as an edit.
  View.prototype.arrange = function () {
    if (!this.colony) return;
    clearTimeout(this.timer);
    this.edited[this.index] = clone(this.colony);
    var card = this.root.querySelector('.pi-card[data-colony="' + this.index + '"]');
    if (card) card.classList.add("edited");
    this.recompute(true);
  };

  View.prototype.recompute = function (arrange) {
    var self = this;
    var idx = this.index;
    var body = clone(this.colony);
    var peaks = this.peaks[idx] || {};
    body.pins.forEach(function (p) {
      if (p.kind === "ecu" && peaks[p.product]) { p.peak = peaks[p.product]; p.qtyPerCycle = 0; }
    });
    fetch("/industry/pi/colony" + (arrange ? "?arrange=1" : ""), {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest" },
      body: JSON.stringify(body)
    }).then(function (r) { return r.json(); }).then(function (ans) {
      if (ans.error && !ans.colony) { self.say(ans.error); return; }
      self.answers[idx] = ans;
      if (idx !== self.index) return;
      // Keep the player's own positions; take the server's links,
      // routes and levels.
      var drag = self.drag;
      if (!drag) {
        self.colony.links = ans.colony.links || [];
        self.colony.routes = ans.colony.routes || [];
        self.colony.pins = ans.colony.pins || [];
        if (self.edited[idx]) self.edited[idx] = clone(self.colony);
      }
      self.show(ans);
      if (arrange) self.say(ans.arranged ? "Layout optimized: structures moved to where their links cost the least." : "This layout already costs the least; nothing moved.");
    }).catch(function () { self.say("The colony could not be checked. Try again."); });
  };

  View.prototype.show = function (ans) {
    this.crowded = {};
    var self = this;
    (ans.crowded || []).forEach(function (i) { self.crowded[i] = true; });
    var f = ans.fit;
    var fitEl = document.getElementById("pi-view-fit");
    fitEl.textContent = "";
    var bar = function (label, used, cap, unit) {
      var wrap = document.createElement("div");
      wrap.className = "pi-fitbar" + (used > cap ? " over" : "");
      var l = document.createElement("span");
      l.className = "pi-meter-label";
      l.textContent = label;
      var track = document.createElement("span");
      track.className = "pi-fit-track";
      var fill = document.createElement("span");
      fill.className = "pi-fit-fill";
      fill.style.inlineSize = Math.min(100, cap ? used * 100 / cap : 0) + "%";
      track.appendChild(fill);
      var fig = document.createElement("span");
      fig.className = "pi-meter-fig";
      fig.textContent = fmt(used) + " / " + fmt(cap) + " " + unit;
      wrap.appendChild(l); wrap.appendChild(track); wrap.appendChild(fig);
      fitEl.appendChild(wrap);
    };
    bar("CPU", f.CPUUsed, f.CPUCap, "tf");
    bar("Power", f.PGUsed, f.PGCap, "MW");
    if (!ans.fits) {
      var warn = document.createElement("p");
      warn.className = "pi-warn";
      warn.textContent = "Over the command center's budget: raise its level, drop a structure or shorten links.";
      fitEl.appendChild(warn);
    }
    if (ans.crowded && ans.crowded.length) {
      var w2 = document.createElement("p");
      w2.className = "pi-warn";
      w2.textContent = ans.crowded.length + " structures sit closer than the game allows; drag them apart.";
      fitEl.appendChild(w2);
    }
    var rates = document.getElementById("pi-view-rates");
    rates.textContent = "";
    var list = function (title, m, cls) {
      var keys = Object.keys(m || {});
      if (!keys.length) return;
      var h = document.createElement("p");
      h.className = "pi-rates-h";
      h.textContent = title;
      rates.appendChild(h);
      keys.sort().forEach(function (k) {
        var s = document.createElement("span");
        s.className = "pi-rate " + cls;
        s.textContent = k + " " + fmt(m[k] * 24) + "/day";
        rates.appendChild(s);
      });
    };
    list("Sends out", ans.out, "out");
    list("Needs in", ans.in, "in");
    this.template = ans.template;
  };

  View.prototype.inspect = function () {
    var box = document.getElementById("pi-inspect");
    box.textContent = "";
    if (this.sel < 0 || !this.colony.pins[this.sel]) {
      var hint = document.createElement("p");
      hint.className = "pi-help";
      hint.textContent = "Select a structure to change it. Drag to move; arrow keys nudge the selected one.";
      box.appendChild(hint);
      return;
    }
    var self = this;
    var p = this.colony.pins[this.sel];
    var sel = this.sel;
    var live = function () { return self.colony.pins[sel] || p; };
    var title = document.createElement("p");
    title.className = "pi-inspect-h";
    title.textContent = (this.catalog.labels[p.kind] || p.kind) + (p.change === "add" ? " (to build)" : p.change === "remove" ? " (to remove)" : "");
    box.appendChild(title);
    var row = document.createElement("div");
    row.className = "pi-fields";
    box.appendChild(row);
    var field = function (label, input) {
      var l = document.createElement("label");
      l.className = "pi-field";
      var s = document.createElement("span");
      s.textContent = label;
      l.appendChild(s);
      l.appendChild(input);
      row.appendChild(l);
      return input;
    };
    var select = function (opts, value) {
      var sel = document.createElement("select");
      opts.forEach(function (o) {
        var op = document.createElement("option");
        op.value = o.value; op.textContent = o.label;
        if (String(o.value) === String(value)) op.selected = true;
        sel.appendChild(op);
      });
      return sel;
    };
    var products = p.kind === "ecu" ? (this.catalog.yields[this.colony.planetType] || []) : (this.catalog.recipes[p.kind] || []);
    if (products.length) {
      var ps = field(p.kind === "ecu" ? "Extracts" : "Makes", select(products.map(function (n) { return { value: n, label: n }; }), p.product));
      ps.addEventListener("change", function () {
        var q = live();
        if (q.change === "keep" && q.kind !== "ecu") q.change = "retool";
        q.product = ps.value;
        if (q.kind === "ecu") { q.qtyPerCycle = 0; q.headPos = null; }
        self.changed();
      });
    }
    if (p.kind === "ecu") {
      var heads = [];
      for (var h = 1; h <= this.catalog.maxHeads; h++) heads.push({ value: h, label: h + (h === 1 ? " head" : " heads") });
      var hs = field("Heads", select(heads, p.heads));
      hs.addEventListener("change", function () {
        var q = live();
        // A measured unit keeps its per-head rate when heads change.
        if (q.qtyPerCycle > 0 && q.heads > 0) { q.peak = q.qtyPerCycle * 4 / q.heads; q.qtyPerCycle = 0; }
        q.heads = parseInt(hs.value, 10);
        q.headPos = null;
        self.changed();
      });
      var progs = this.catalog.programs.map(function (v) { return { value: v, label: v < 48 ? v + " hours" : (v / 24) + " days" }; });
      var pg = field("Program", select(progs, p.programHours || 24));
      pg.addEventListener("change", function () { live().programHours = parseFloat(pg.value); self.changed(); });
      var peaks = this.peaks[this.index] || (this.peaks[this.index] = {});
      var inp = document.createElement("input");
      inp.type = "number"; inp.min = "50"; inp.step = "50"; inp.inputMode = "numeric";
      var measured = p.qtyPerCycle > 0 && p.heads > 0 ? Math.round(p.qtyPerCycle * 4 / p.heads) : 0;
      inp.placeholder = measured ? measured + " measured" : Math.round(p.peak || this.catalog.peakAssume) + " assumed";
      if (peaks[p.product]) inp.value = peaks[p.product];
      field("Survey yield per head per hour", inp);
      inp.addEventListener("change", function () {
        var v = parseFloat(inp.value);
        var q = live();
        if (v > 0) peaks[q.product] = v; else delete peaks[q.product];
        self.edited[self.index] = clone(self.colony);
        self.recompute();
      });
    }
    if (p.kind !== "cc") {
      var rm = document.createElement("button");
      rm.type = "button";
      rm.className = "btn btn-quiet";
      rm.setAttribute("data-remove-pin", "");
      rm.textContent = p.change === "remove" ? "Keep it" : "Remove";
      box.appendChild(rm);
    }
  };

  View.prototype.reset = function () {
    delete this.edited[this.index];
    delete this.answers[this.index];
    delete this.peaks[this.index];
    var card = this.root.querySelector('.pi-card[data-colony="' + this.index + '"]');
    if (card) card.classList.remove("edited");
    this.open(this.index);
  };

  View.prototype.copyTemplate = function () {
    var t = this.template;
    if (!t) {
      var ta = this.root.querySelector('[data-template="' + this.index + '"]');
      t = ta ? ta.value : "";
    }
    if (!t) { this.say("No template for this colony."); return; }
    var self = this;
    var done = function () { self.say("Copied. In game: Templates, Import, Load from clipboard."); };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(t).then(done, function () { self.fallbackCopy(t); done(); });
    } else { this.fallbackCopy(t); done(); }
  };

  View.prototype.fallbackCopy = function (t) {
    var ta = document.createElement("textarea");
    ta.value = t;
    ta.setAttribute("readonly", "");
    ta.className = "pi-copy-buffer";
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); } catch (e) { /* nothing more to try */ }
    ta.remove();
  };

  View.prototype.replan = function () {
    var self = this;
    var pins = [];
    Object.keys(this.edited).forEach(function (k) {
      var i = parseInt(k, 10);
      var pc = self.colonyByIndex(i);
      if (!pc) return;
      pins.push({
        colony: self.edited[k], charId: pc.charId || 0, peak: self.peaks[i] || null,
        system: pc.system || "", jumps: pc.jumps || 0, existing: !!pc.existing
      });
    });
    if (!pins.length) { this.say("Change a colony first; the plan as it stands is already solved."); return; }
    var body = new URLSearchParams();
    body.set("base", this.token);
    body.set("pins", JSON.stringify(pins));
    this.say("Planning around your colonies...");
    fetch("/industry/pi/solve", {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/x-www-form-urlencoded", "X-Requested-With": "XMLHttpRequest" },
      body: body.toString()
    }).then(function (r) { return r.json(); }).then(function (j) {
      if (j.url) window.location.assign(j.url);
      else self.say("The plan could not be made.");
    }).catch(function () { self.say("The plan could not be made. Try again."); });
  };

  View.prototype.say = function (msg) {
    var el = document.getElementById("pi-view-msg");
    el.textContent = msg;
    clearTimeout(this.sayTimer);
    this.sayTimer = setTimeout(function () { el.textContent = ""; }, 6000);
  };

  // --- drawing ----------------------------------------------------------

  // The surface is tiling layers fixed to the planet. The planet's
  // texture is drawn once at a size no zoom level can see repeat, then
  // again finer, faded in as the view closes on the colony, and a grey
  // detail layer is blended over the top. All move with the structures.
  var MACRO_N = 512, WIDE_RAD = 1.6, MACRO_RAD = 0.3, DETAIL_N = 256, DETAIL_RAD = 0.05;

  View.prototype.surface = function (type) {
    var key = "m|" + type;
    if (this.textures[key]) return this.textures[key];
    var n = MACRO_N;
    var off = document.createElement("canvas");
    off.width = n; off.height = n;
    var g = off.getContext("2d");
    var img = g.createImageData(n, n);
    var t = type.toLowerCase();
    var base = parseColor(css(this.panel, "--pi-surface-" + t, "#3a3f4a"));
    var seed = type.length * 97 + type.charCodeAt(0);
    for (var y = 0; y < n; y++) {
      for (var x = 0; x < n; x++) {
        var u = x / n, v = y / n;
        var f = fbm(u, v, seed, 6, false);
        var rr = fbm(u, v, seed + 7, 5, true);
        var c = shade(t, base, f, rr, v);
        var o = (y * n + x) * 4;
        img.data[o] = Math.min(255, c[0]);
        img.data[o + 1] = Math.min(255, c[1]);
        img.data[o + 2] = Math.min(255, c[2]);
        img.data[o + 3] = 255;
      }
    }
    g.putImageData(img, 0, 0);
    this.textures[key] = this.ctx.createPattern(off, "repeat");
    return this.textures[key];
  };

  View.prototype.detail = function () {
    if (this.textures.detail) return this.textures.detail;
    var n = DETAIL_N;
    var off = document.createElement("canvas");
    off.width = n; off.height = n;
    var g = off.getContext("2d");
    var img = g.createImageData(n, n);
    for (var y = 0; y < n; y++) {
      for (var x = 0; x < n; x++) {
        var k = 60 + fbm(x / n, y / n, 4242, 8, false) * 136;
        var o = (y * n + x) * 4;
        img.data[o] = img.data[o + 1] = img.data[o + 2] = k;
        img.data[o + 3] = 255;
      }
    }
    g.putImageData(img, 0, 0);
    this.textures.detail = this.ctx.createPattern(off, "repeat");
    return this.textures.detail;
  };

  // paintTile fills the canvas with pattern pat, one tile spanning rad
  // radians of the planet, anchored to a planet point near the view.
  View.prototype.paintTile = function (pat, n, rad) {
    // Tiles are square in surface distance: rad of longitude spans only
    // rad * sx of ground.
    var radLon = rad / (this.sx || 1);
    var la = Math.floor(this.center.lat / rad) * rad;
    var lo = Math.floor(this.center.lon / radLon) * radLon;
    var o = this.toScreen(la, lo);
    var k = rad * this.scale / n;
    if (pat.setTransform) pat.setTransform(new DOMMatrix().translate(o.x, o.y).scale(k, k));
    this.ctx.fillStyle = pat;
    this.ctx.fillRect(0, 0, this.w, this.h);
  };

  View.prototype.draw = function () {
    var ctx = this.ctx, dpr = this.dpr || 1;
    if (!this.w) this.resize();
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    var c = this.colony, self = this;
    var t = (performance.now() - this.t0) / 1000;

    // Surface, fixed to the planet.
    ctx.save();
    ctx.imageSmoothingQuality = "high";
    var pat = this.surface(c.planetType);
    this.paintTile(pat, MACRO_N, WIDE_RAD);
    var mid = Math.max(0, Math.min(1, (this.scale - 1500) / 3000));
    if (mid > 0) {
      ctx.globalAlpha = mid;
      this.paintTile(pat, MACRO_N, MACRO_RAD);
      ctx.globalAlpha = 1;
    }
    var fine = Math.max(0, Math.min(1, (this.scale - 2500) / 6000));
    if (fine > 0) {
      ctx.globalCompositeOperation = "overlay";
      ctx.globalAlpha = fine * 0.55;
      this.paintTile(this.detail(), DETAIL_N, DETAIL_RAD);
    }
    ctx.restore();
    // Vignette and grid.
    var vg = ctx.createRadialGradient(this.w / 2, this.h / 2, Math.min(this.w, this.h) * 0.2, this.w / 2, this.h / 2, Math.max(this.w, this.h) * 0.75);
    vg.addColorStop(0, "rgba(0,0,0,0)");
    vg.addColorStop(1, "rgba(0,0,0,0.55)");
    ctx.fillStyle = vg;
    ctx.fillRect(0, 0, this.w, this.h);
    ctx.strokeStyle = "rgba(255,255,255,0.05)";
    ctx.lineWidth = 1;
    // Grid lines at least 40 px apart at any zoom.
    var step = 0.01;
    while (step * this.scale < 40) step *= 2;
    var tl = this.toPlanet(0, 0), br = this.toPlanet(this.w, this.h);
    ctx.beginPath();
    for (var la = Math.floor(tl.lat / step) * step; la < br.lat; la += step) {
      var y = this.toScreen(la, 0).y; ctx.moveTo(0, y); ctx.lineTo(this.w, y);
    }
    for (var lo = Math.floor(tl.lon / step) * step; lo < br.lon; lo += step) {
      var x = this.toScreen(this.center.lat, lo).x; ctx.moveTo(x, 0); ctx.lineTo(x, this.h);
    }
    ctx.stroke();

    var pins = c.pins;
    var R = this.pinRadius();
    var pos = pins.map(function (p) { return self.toScreen(p.lat, p.lon); });

    // Extractor heads and their reach.
    pins.forEach(function (p, i) {
      if (p.kind !== "ecu" || p.change === "remove") return;
      var heads = p.headPos && p.headPos.length ? p.headPos : null;
      var n = p.heads || 0;
      ctx.strokeStyle = css(self.panel, "--pi-k-ecu", "#6fd3a6");
      ctx.globalAlpha = 0.5;
      for (var k = 0; k < n; k++) {
        var hp;
        if (heads && heads[k]) hp = self.toScreen(heads[k].lat, heads[k].lon);
        else {
          var a = k / Math.max(n, 1) * TAU + i;
          var rr = R * 2.6 + (k % 2) * R * 0.9;
          hp = { x: pos[i].x + Math.cos(a) * rr, y: pos[i].y + Math.sin(a) * rr };
        }
        ctx.beginPath(); ctx.moveTo(pos[i].x, pos[i].y); ctx.lineTo(hp.x, hp.y); ctx.stroke();
        ctx.beginPath(); ctx.arc(hp.x, hp.y, Math.max(3, R * 0.28), 0, TAU); ctx.stroke();
        var pulse = (Math.sin(t * 2 + k) + 1) / 2;
        ctx.fillStyle = css(self.panel, "--pi-k-ecu", "#6fd3a6");
        ctx.globalAlpha = 0.25 + pulse * 0.35;
        ctx.beginPath(); ctx.arc(hp.x, hp.y, Math.max(2, R * 0.16), 0, TAU); ctx.fill();
        ctx.globalAlpha = 0.5;
      }
      ctx.globalAlpha = 1;
    });

    // Links, thicker with level.
    var linkCol = css(this.panel, "--pi-link", "rgba(200,220,255,0.55)");
    (c.links || []).forEach(function (l) {
      var a = pos[l.a], b = pos[l.b];
      if (!a || !b) return;
      ctx.strokeStyle = linkCol;
      ctx.lineWidth = 1.5 + (l.level || 0) * 0.8;
      ctx.beginPath(); ctx.moveTo(a.x, a.y); ctx.lineTo(b.x, b.y); ctx.stroke();
    });

    // Goods moving along routes.
    (c.routes || []).forEach(function (r, ri) {
      var path = r.path || [];
      if (path.length < 2) return;
      var tier = (self.catalog.tiers[r.item] || "P1").toLowerCase();
      ctx.fillStyle = css(self.panel, "--pi-tier-" + tier, "#ffd27a");
      var segs = path.length - 1;
      var speed = 0.35;
      for (var k = 0; k < 2; k++) {
        var u = ((t * speed + k / 2 + ri * 0.137) % 1) * segs;
        var si = Math.floor(u), f = u - si;
        var a = pos[path[si]], b = pos[path[si + 1]];
        if (!a || !b) continue;
        var x = a.x + (b.x - a.x) * f, y = a.y + (b.y - a.y) * f;
        ctx.beginPath(); ctx.arc(x, y, 2.4, 0, TAU); ctx.fill();
      }
    });

    // Structures.
    var font = css(this.panel, "--font-display", "sans-serif");
    pins.forEach(function (p, i) {
      var q = pos[i];
      var col = css(self.panel, "--pi-k-" + p.kind, "#9ab");
      var removing = p.change === "remove";
      ctx.globalAlpha = removing ? 0.45 : 1;
      // Glow.
      var glow = ctx.createRadialGradient(q.x, q.y, R * 0.4, q.x, q.y, R * 1.9);
      glow.addColorStop(0, col);
      glow.addColorStop(1, "rgba(0,0,0,0)");
      ctx.globalAlpha = removing ? 0.15 : 0.35;
      ctx.fillStyle = glow;
      ctx.beginPath(); ctx.arc(q.x, q.y, R * 1.9, 0, TAU); ctx.fill();
      ctx.globalAlpha = removing ? 0.45 : 1;
      // Body: hexagon for factories, circle otherwise.
      ctx.fillStyle = "rgba(10,14,20,0.85)";
      ctx.strokeStyle = col;
      ctx.lineWidth = 2;
      ctx.beginPath();
      if (p.kind === "basic" || p.kind === "advanced" || p.kind === "hightech") {
        for (var k = 0; k < 6; k++) {
          var a = k / 6 * TAU + Math.PI / 6;
          var x = q.x + Math.cos(a) * R, y = q.y + Math.sin(a) * R;
          if (k) ctx.lineTo(x, y); else ctx.moveTo(x, y);
        }
        ctx.closePath();
      } else if (p.kind === "launchpad" || p.kind === "storage") {
        ctx.rect(q.x - R * 0.85, q.y - R * 0.85, R * 1.7, R * 1.7);
      } else {
        ctx.arc(q.x, q.y, R, 0, TAU);
      }
      ctx.fill();
      if (removing) ctx.setLineDash([4, 3]);
      ctx.stroke();
      ctx.setLineDash([]);
      ctx.fillStyle = col;
      ctx.font = "600 " + Math.round(R * 0.75) + "px " + font;
      ctx.textAlign = "center";
      ctx.textBaseline = "middle";
      ctx.fillText(KIND_GLYPH[p.kind] || "?", q.x, q.y + 1);
      // Change marks.
      if (p.change === "add" || p.change === "retool") {
        ctx.fillStyle = css(self.panel, p.change === "add" ? "--kept" : "--gold", "#7c7");
        ctx.beginPath(); ctx.arc(q.x + R * 0.8, q.y - R * 0.8, R * 0.3, 0, TAU); ctx.fill();
      }
      if (self.crowded && self.crowded[i]) {
        ctx.strokeStyle = css(self.panel, "--danger", "#f55");
        ctx.lineWidth = 2;
        ctx.beginPath(); ctx.arc(q.x, q.y, R * 1.35, 0, TAU); ctx.stroke();
      }
      if (i === self.sel || i === self.hover) {
        ctx.strokeStyle = css(self.panel, "--gold", "#ffd27a");
        ctx.lineWidth = i === self.sel ? 2.5 : 1.5;
        ctx.beginPath(); ctx.arc(q.x, q.y, R * 1.25 + (i === self.sel ? Math.sin(t * 4) * 1.5 : 0), 0, TAU); ctx.stroke();
      }
      // Product label when there is room.
      if (p.product && self.scale > 5000) {
        ctx.globalAlpha = removing ? 0.4 : 0.9;
        ctx.fillStyle = css(self.panel, "--text", "#e6e6e6");
        ctx.font = Math.round(Math.max(10, R * 0.55)) + "px " + css(self.panel, "--font-body", "sans-serif");
        ctx.fillText(p.product, q.x, q.y + R * 1.55);
      }
      ctx.globalAlpha = 1;
    });

    // Scale bar.
    var km = c.radiusKm || 5000;
    var target = 20;
    var px = target / km * this.scale;
    while (px > 160) { target /= 2; px /= 2; }
    while (px < 50) { target *= 2; px *= 2; }
    ctx.strokeStyle = css(this.panel, "--text", "#ddd");
    ctx.lineWidth = 1.5;
    ctx.beginPath(); ctx.moveTo(14, this.h - 16); ctx.lineTo(14 + px, this.h - 16); ctx.stroke();
    ctx.fillStyle = css(this.panel, "--text", "#ddd");
    ctx.font = "12px " + css(this.panel, "--font-num", "monospace");
    ctx.textAlign = "left";
    ctx.textBaseline = "bottom";
    ctx.fillText(Math.round(target) + " km", 14, this.h - 20);
  };

  // --- boot ---------------------------------------------------------------

  var booted = null;
  function boot() {
    var root = document.getElementById("pi-board");
    if (!root || root === booted || !root.getAttribute("data-plan")) return;
    booted = root;
    new View(root);
  }

  // The goal form's product moves the chain tabs to the same product.
  var goalBooted = null;
  function bootGoal() {
    var sel = document.getElementById("pi-goal-product");
    if (!sel || sel === goalBooted) return;
    goalBooted = sel;
    var form = document.getElementById("pi-goal");
    if (form) {
      // The search can take a few seconds: say it is working.
      form.addEventListener("submit", function () {
        var go = form.querySelector(".pi-goal-go");
        if (go) { go.textContent = "Planning..."; go.setAttribute("aria-busy", "true"); }
      });
    }
    sel.addEventListener("change", function () {
      var chain = document.getElementById("pi-product");
      if (chain && chain.value !== sel.value) {
        chain.value = sel.value;
        chain.dispatchEvent(new Event("change", { bubbles: true }));
      }
    });
  }

  function all() { boot(); bootGoal(); }
  all();
  document.addEventListener("evesynapse:content-ready", all);
  // A page restored from the back-forward cache keeps the busy label.
  window.addEventListener("pageshow", function () {
    document.querySelectorAll(".pi-goal-go[aria-busy]").forEach(function (b) {
      b.textContent = "Plan colonies";
      b.removeAttribute("aria-busy");
    });
  });
})();
