/* map.js -- the universe map (map.html).
 *
 * Draws New Eden's known space on one canvas from the stored map
 * payload (universe_map.go), in two views:
 *
 *   - New Eden: every system at its true top-down position. Region
 *     names label the picture; system names appear as you zoom in.
 *   - Tactical: one region alone, on its own decluttered layout
 *     (computed at import, never here), every system named, gates to
 *     neighbouring regions drawn as labelled stubs that lead there.
 *
 * Clicking a region name enters its tactical view; zooming back
 * out of one returns to New Eden at the same place. Zooming deep
 * into a region enters it too, but only when the "Zoom to
 * tactical" preference is on (it is off by default). The search,
 * the system card and the region index enter one directly. The
 * URL keeps ?region= and ?system=, so a view can be shared.
 *
 * Canvas 2D, drawn on demand: the gate network is one Path2D per
 * view in world coordinates; systems are batched by security color;
 * labels are culled against an occupancy grid so they never pile up.
 * Colors come from the page: the legend's .sec-N swatches and the
 * --map-* tokens in map.css, re-read when the theme changes.
 */
(function () {
  "use strict";

  /**
   * The stored payload (Go: universeMapPayload, universe_map.go).
   * Systems are columnar: index i is one system across every array.
   * @typedef {Object} UniversePayload
   * @property {number} v
   * @property {{id:number,name:string,x:number,y:number,w:number,h:number}[]} regions
   *   x, y: label point in the universe frame; w, h: region frame size.
   * @property {{id:number,name:string,r:number}[]} cons  r: region index.
   * @property {{id:number[],n:string[],x:number[],y:number[],s:number[],r:number[],c:number[],rx:number[],ry:number[],f:number[]}} sys
   *   x, y: universe frame (0..10000); rx, ry: region frame (0..1000);
   *   s: true security; r: region index; c: constellation index (-1 none);
   *   f: faction ID.
   * @property {number[]} gates  stargate pairs as flattened index pairs.
   */

  // One fetch per payload URL for the life of the page shell: the
  // URL is content-hashed, so a swap back to the map redraws from
  // memory.
  var payloads = {};
  function loadPayload(url) {
    if (!payloads[url]) {
      payloads[url] = fetch(url, { credentials: "same-origin" }).then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        return r.json();
      });
      payloads[url].catch(function () { delete payloads[url]; });
    }
    return payloads[url];
  }

  // --- Security, as EVE shows it (core_linkfuncs.go secRound) ---
  function secRounded(s) {
    var r = Math.round(s * 10) / 10;
    if (s > 0 && s < 0.05) r = 0.1;
    return r;
  }
  function secTier(s) {
    var r = secRounded(s);
    return r <= 0 ? 0 : Math.round(r * 10);
  }
  function secText(s) {
    var r = secRounded(s);
    return (r === 0 ? 0 : r).toFixed(1);
  }

  // Region names thin out as system names take over, but never
  // vanish: the name stays readable (and clickable) however deep
  // the zoom, so it can always carry the reader into the region.
  function regionLabelAlpha(f) {
    return f > 6 ? Math.max(0.55, 1 - (f - 6) / 14) : 1;
  }
  // Zooming in enters a region only under the preference; every
  // other way in (a name, the search, the index) is unconditional,
  // and zooming back out of a region always leaves it.
  function zoomEntryAllowed(prefOn) { return !!prefOn; }

  // --- Graph: derived once per payload ---
  function prepare(P) {
    if (P._g) return P._g;
    var S = P.sys, n = S.id.length, i, j, a, b;
    var adj = new Array(n), tier = new Uint8Array(n), lower = new Array(n);
    var byID = {};
    for (i = 0; i < n; i++) {
      adj[i] = [];
      tier[i] = secTier(S.s[i]);
      lower[i] = S.n[i].toLowerCase();
      byID[S.id[i]] = i;
    }
    for (i = 0; i < P.gates.length; i += 2) {
      a = P.gates[i]; b = P.gates[i + 1];
      adj[a].push(b); adj[b].push(a);
    }
    var R = P.regions.length;
    var members = [], bounds = [], exits = [], regionByID = {};
    for (j = 0; j < R; j++) {
      members.push([]); exits.push([]);
      bounds.push([Infinity, Infinity, -Infinity, -Infinity]);
      regionByID[P.regions[j].id] = j;
    }
    for (i = 0; i < n; i++) {
      var r = S.r[i], bb = bounds[r];
      members[r].push(i);
      if (S.x[i] < bb[0]) bb[0] = S.x[i];
      if (S.y[i] < bb[1]) bb[1] = S.y[i];
      if (S.x[i] > bb[2]) bb[2] = S.x[i];
      if (S.y[i] > bb[3]) bb[3] = S.y[i];
      for (j = 0; j < adj[i].length; j++) {
        if (S.r[adj[i][j]] !== r) exits[r].push([i, adj[i][j]]);
      }
    }
    // Constellation label points, in both frames.
    var C = P.cons.length, cu = [], cr = [], cn = new Array(C);
    for (j = 0; j < C; j++) { cu.push([0, 0]); cr.push([0, 0]); cn[j] = 0; }
    for (i = 0; i < n; i++) {
      var c = S.c[i];
      if (c < 0) continue;
      cu[c][0] += S.x[i]; cu[c][1] += S.y[i];
      cr[c][0] += S.rx[i]; cr[c][1] += S.ry[i];
      cn[c]++;
    }
    for (j = 0; j < C; j++) {
      if (!cn[j]) continue;
      cu[j][0] /= cn[j]; cu[j][1] /= cn[j];
      cr[j][0] /= cn[j]; cr[j][1] /= cn[j];
    }
    var ux = 0, uy = 0;
    for (i = 0; i < n; i++) {
      if (S.x[i] > ux) ux = S.x[i];
      if (S.y[i] > uy) uy = S.y[i];
    }
    var regionLower = P.regions.map(function (rg) { return rg.name.toLowerCase(); });
    // Label precedence: the best-connected systems first, then by
    // ID, so the same names win wherever the map is panned.
    function byPrio(a, b) { return adj[b].length - adj[a].length || S.id[a] - S.id[b]; }
    var prio = [];
    for (i = 0; i < n; i++) prio.push(i);
    prio.sort(byPrio);
    var membersPrio = members.map(function (m) { return m.slice().sort(byPrio); });
    P._g = {
      prio: prio, membersPrio: membersPrio,
      n: n, adj: adj, tier: tier, lower: lower, byID: byID, regionByID: regionByID,
      members: members, bounds: bounds, exits: exits, consU: cu, consR: cr,
      uniW: ux, uniH: uy, regionLower: regionLower
    };
    return P._g;
  }

  // Stub length past a region's edge system, in region-frame units.
  var STUB = 70;

  function mount(el, P) {
    var G = prepare(P), S = P.sys;
    var stage = el.querySelector(".map-stage");
    var canvas = el.querySelector(".map-canvas");
    var ctx = canvas.getContext("2d");
    var statusEl = el.querySelector(".map-status");
    var hintEl = el.querySelector(".map-hint");
    var card = el.querySelector(".map-card");
    var crumbSep = el.querySelector(".map-crumb-sep");
    var crumbRegion = el.querySelector(".map-crumb-region");
    var findInput = el.querySelector(".map-find-input");
    var findList = el.querySelector(".map-find-list");
    var zoomPref = el.querySelector("[data-map-zoom-tactical]");
    var root = el.ownerDocument;

    // The one preference: whether zooming deep into a region
    // enters its tactical view. Off by default -- clicking the
    // region's name is the way in -- and remembered per browser.
    var ZOOM_TACTICAL_KEY = "evesynapse-map-zoom-tactical";
    var zoomTactical = false;
    try { zoomTactical = window.localStorage.getItem(ZOOM_TACTICAL_KEY) === "1"; } catch (e) { /* private mode */ }

    var cw = 0, ch = 0, dpr = 1;
    var mode = "universe", region = -1;
    var cam = { x: 0, y: 0, k: 1 };
    var sel = -1, hover = -1;
    var raf = 0, hintTimer = 0, fitted = false;
    var colors = null, font = "sans-serif";
    var labelBoxes = []; // region labels hit-tested in the universe view
    var pathCache = {};
    var widthCache = {};

    // --- Colors from the page ---
    function readColors() {
      var cs = getComputedStyle(el);
      function tok(name, fallback) {
        var v = cs.getPropertyValue(name).trim();
        return v || fallback;
      }
      var sec = [];
      for (var t = 0; t <= 10; t++) {
        var sw = el.querySelector('.map-swatch[data-tier="' + t + '"]');
        sec[t] = sw ? getComputedStyle(sw).color : "#888";
      }
      colors = {
        sec: sec,
        gate: tok("--map-gate", "rgba(255,180,120,0.22)"),
        gateInter: tok("--map-gate-inter", "rgba(255,120,60,0.4)"),
        gateHot: tok("--map-gate-hot", "#ff6a1a"),
        label: tok("--map-label", "#faf3e6"),
        labelDim: tok("--map-label-dim", "#b39a7d"),
        region: tok("--map-region", "rgba(255,210,122,0.75)"),
        cons: tok("--map-cons", "rgba(201,160,106,0.45)"),
        accent: tok("--map-accent", "#ff6a1a"),
        halo: tok("--map-halo", "rgba(10,7,4,0.85)")
      };
      font = cs.fontFamily || font;
      widthCache = {};
    }

    // --- Views ---
    function wx(i) { return mode === "universe" ? S.x[i] : S.rx[i]; }
    function wy(i) { return mode === "universe" ? S.y[i] : S.ry[i]; }

    function viewBounds() {
      if (mode === "universe") return [0, 0, G.uniW, G.uniH];
      var rg = P.regions[region];
      return [-STUB - 30, -STUB - 30, rg.w + STUB + 30, rg.h + STUB + 30];
    }
    function fitK() {
      var b = viewBounds();
      var bw = Math.max(b[2] - b[0], 1), bh = Math.max(b[3] - b[1], 1);
      return Math.min(cw / bw, ch / bh) * 0.92;
    }
    function fit() {
      var b = viewBounds();
      cam.x = (b[0] + b[2]) / 2;
      cam.y = (b[1] + b[3]) / 2;
      cam.k = fitK();
    }
    function minK() { return fitK() * (mode === "universe" ? 0.6 : 0.5); }
    function maxK() { return fitK() * (mode === "universe" ? 60 : 8); }
    function clampK(k) { return Math.min(Math.max(k, minK()), maxK()); }

    // --- Motion ---
    // Every camera change but a drag glides: a "fly" eases position
    // and zoom (zoom in log space, so it feels even at any scale)
    // to a target; a "zoom" eases the scale about a fixed screen
    // point, so the point under the cursor stays put. Wheel and
    // touchpad zoom chase a running target instead, so a burst of
    // wheel ticks reads as one smooth glide. Any touch on the map
    // stops the motion where it is; reduced motion jumps.
    var anim = null, wheelZ = null;
    var still = !!(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
    function ease(t) { return 1 - Math.pow(1 - t, 3); }
    function flyTo(x, y, k, dur) {
      wheelZ = null;
      k = clampK(k);
      if (still || !cw) { anim = null; cam.x = x; cam.y = y; cam.k = k; schedule(); return; }
      anim = { kind: "fly", t0: performance.now(), dur: dur || 320,
        x0: cam.x, y0: cam.y, l0: Math.log(cam.k), x1: x, y1: y, l1: Math.log(k) };
      schedule();
    }
    function glideZoom(px, py, factor, dur) {
      wheelZ = null;
      var k = clampK((anim && anim.kind === "zoom" ? Math.exp(anim.l1) : cam.k) * factor);
      if (still) { setZoomAt(px, py, k); afterZoom(factor > 1 ? 1 : -1); schedule(); return; }
      anim = { kind: "zoom", t0: performance.now(), dur: dur || 260, px: px, py: py,
        l0: Math.log(cam.k), l1: Math.log(k) };
      schedule();
    }
    function stopMotion() { anim = null; wheelZ = null; }
    // Scale about a screen point: the world point under it stays.
    function setZoomAt(px, py, k) {
      var before = toWorld(px, py);
      cam.k = clampK(k);
      var after = toWorld(px, py);
      cam.x += before[0] - after[0];
      cam.y += before[1] - after[1];
    }
    // One frame of motion; true while more remains.
    function stepMotion(now) {
      var more = false;
      if (anim) {
        var t = Math.min(1, (now - anim.t0) / anim.dur), e = ease(t);
        if (anim.kind === "fly") {
          cam.x = anim.x0 + (anim.x1 - anim.x0) * e;
          cam.y = anim.y0 + (anim.y1 - anim.y0) * e;
          cam.k = Math.exp(anim.l0 + (anim.l1 - anim.l0) * e);
          if (t >= 1) anim = null; else more = true;
        } else {
          var a = anim;
          setZoomAt(a.px, a.py, Math.exp(a.l0 + (a.l1 - a.l0) * e));
          if (t >= 1) anim = null; else more = true;
          afterZoom(a.l1 > a.l0 ? 1 : -1);
        }
      }
      if (wheelZ) {
        var lk = Math.log(cam.k), lt = Math.log(wheelZ.k);
        var done = Math.abs(lt - lk) < 0.004;
        setZoomAt(wheelZ.px, wheelZ.py, Math.exp(done ? lt : lk + (lt - lk) * 0.3));
        var w = wheelZ;
        if (done) wheelZ = null; else more = true;
        afterZoom(lt > lk ? 1 : -1);
        if (wheelZ === w && !done) more = true;
      }
      return more;
    }
    // A wheel tick or touchpad pinch: move the running target.
    function chaseZoom(px, py, factor) {
      anim = null;
      var base = wheelZ ? wheelZ.k : cam.k;
      var k = clampK(base * factor);
      if (still) { setZoomAt(px, py, k); afterZoom(factor > 1 ? 1 : -1); schedule(); return; }
      wheelZ = { k: k, px: px, py: py };
      schedule();
    }

    // The ghost end of a gate leaving the region: past the edge
    // system, pointing the way the neighbour truly lies.
    function ghost(i, j) {
      var dx = S.x[j] - S.x[i], dy = S.y[j] - S.y[i];
      var d = Math.sqrt(dx * dx + dy * dy) || 1;
      return [S.rx[i] + dx / d * STUB, S.ry[i] + dy / d * STUB];
    }

    function paths() {
      var key = mode === "universe" ? "u" : "r" + region;
      if (pathCache[key]) return pathCache[key];
      var intra = new Path2D(), inter = new Path2D(), stubs = new Path2D();
      var i, a, b;
      if (mode === "universe") {
        for (i = 0; i < P.gates.length; i += 2) {
          a = P.gates[i]; b = P.gates[i + 1];
          var p = S.r[a] === S.r[b] ? intra : inter;
          p.moveTo(S.x[a], S.y[a]);
          p.lineTo(S.x[b], S.y[b]);
        }
      } else {
        var ms = G.members[region];
        for (i = 0; i < ms.length; i++) {
          a = ms[i];
          var nb = G.adj[a];
          for (var k = 0; k < nb.length; k++) {
            b = nb[k];
            if (S.r[b] === region) {
              if (a < b) {
                var q = S.c[a] === S.c[b] ? intra : inter;
                q.moveTo(S.rx[a], S.ry[a]);
                q.lineTo(S.rx[b], S.ry[b]);
              }
            } else {
              var g = ghost(a, b);
              stubs.moveTo(S.rx[a], S.ry[a]);
              stubs.lineTo(g[0], g[1]);
            }
          }
        }
      }
      pathCache[key] = { intra: intra, inter: inter, stubs: stubs };
      return pathCache[key];
    }

    function visible() { return mode === "universe" ? null : G.members[region]; }

    // --- Drawing ---
    function schedule() { if (!raf) raf = requestAnimationFrame(draw); }

    function textWidth(s, size) {
      var key = size + "|" + s;
      var w = widthCache[key];
      if (w === undefined) { w = ctx.measureText(s).width; widthCache[key] = w; }
      return w;
    }

    function radius() {
      var f = cam.k / fitK();
      if (mode === "universe") return Math.min(1.3 + (f - 1) * 0.35, 6);
      return Math.min(3.4 + (f - 1) * 1.2, 10);
    }

    function draw(now) {
      raf = 0;
      if (!cw || !ch) return;
      if (stepMotion(now || performance.now())) schedule();
      var k = cam.k, ox = cw / 2 - cam.x * k, oy = ch / 2 - cam.y * k;
      var i, t;
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      ctx.clearRect(0, 0, cw, ch);

      // Gates, in world coordinates.
      var pp = paths();
      ctx.setTransform(dpr * k, 0, 0, dpr * k, dpr * ox, dpr * oy);
      ctx.lineCap = "round";
      ctx.lineWidth = (mode === "universe" ? 0.7 : 1.4) / k;
      ctx.strokeStyle = colors.gate;
      ctx.stroke(pp.intra);
      ctx.strokeStyle = colors.gateInter;
      ctx.stroke(pp.inter);
      if (mode !== "universe") {
        ctx.setLineDash([4 / k, 4 / k]);
        ctx.strokeStyle = colors.gateInter;
        ctx.stroke(pp.stubs);
        ctx.setLineDash([]);
      }
      // The selected system's gates, lit.
      if (sel >= 0 && (mode === "universe" || S.r[sel] === region)) {
        ctx.beginPath();
        var nb = G.adj[sel];
        for (i = 0; i < nb.length; i++) {
          var b = nb[i];
          ctx.moveTo(wx(sel), wy(sel));
          if (mode === "universe" || S.r[b] === region) ctx.lineTo(wx(b), wy(b));
          else { var gh = ghost(sel, b); ctx.lineTo(gh[0], gh[1]); }
        }
        ctx.lineWidth = (mode === "universe" ? 1.6 : 2.4) / k;
        ctx.strokeStyle = colors.gateHot;
        ctx.stroke();
      }
      ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
      if (mode !== "universe") drawConstellations(k, ox, oy);

      // Systems, batched by security color.
      var r = radius(), list = visible(), n = list ? list.length : G.n;
      var pad = 40;
      for (t = 0; t <= 10; t++) {
        ctx.beginPath();
        for (var m = 0; m < n; m++) {
          i = list ? list[m] : m;
          if (G.tier[i] !== t) continue;
          var sx = wx(i) * k + ox, sy = wy(i) * k + oy;
          if (sx < -pad || sy < -pad || sx > cw + pad || sy > ch + pad) continue;
          ctx.moveTo(sx + r, sy);
          ctx.arc(sx, sy, r, 0, Math.PI * 2);
        }
        ctx.fillStyle = colors.sec[t];
        ctx.fill();
      }

      // Ghost ends of the region's exits.
      if (mode !== "universe") {
        ctx.fillStyle = colors.labelDim;
        ctx.beginPath();
        var ex = G.exits[region];
        for (i = 0; i < ex.length; i++) {
          var g = ghost(ex[i][0], ex[i][1]);
          var gx = g[0] * k + ox, gy = g[1] * k + oy;
          ctx.moveTo(gx + r * 0.7, gy);
          ctx.arc(gx, gy, r * 0.7, 0, Math.PI * 2);
        }
        ctx.fill();
      }

      // Hover and selection rings.
      [hover, sel].forEach(function (s, idx) {
        if (s < 0 || (mode !== "universe" && S.r[s] !== region)) return;
        var hx = wx(s) * k + ox, hy = wy(s) * k + oy;
        ctx.beginPath();
        ctx.arc(hx, hy, r + (idx ? 4 : 3), 0, Math.PI * 2);
        ctx.lineWidth = idx ? 2 : 1.25;
        ctx.strokeStyle = idx ? colors.accent : colors.label;
        ctx.stroke();
      });

      drawLabels(k, ox, oy, r);
    }

    // Labels never overlap: each claims cells of a coarse grid, and
    // one that would land on a claimed cell is skipped. The grid is
    // anchored to the map, not the screen, and labels claim in a
    // fixed order (best-connected systems first), so panning moves
    // labels and cells together and the same names win every frame.
    // Zooming moves overlaps every frame, so names placed last frame
    // claim before new ones (inPlaceFirst): a name holds its place
    // until one already placed truly covers it, and names stop
    // blinking in and out as the scale changes.
    var shownLabels = {};
    function drawLabels(k, ox, oy, r) {
      var nextShown = {};
      // Run fn over items (already in priority order) twice: the
      // names showing last frame first, then the rest.
      function inPlaceFirst(items, key, fn) {
        var i;
        for (i = 0; i < items.length; i++) if (shownLabels[key(items[i])]) fn(items[i]);
        for (i = 0; i < items.length; i++) if (!shownLabels[key(items[i])]) fn(items[i]);
      }
      var cell = 12, cols = Math.ceil(cw / cell) + 2, grid = new Uint8Array(cols * (Math.ceil(ch / cell) + 2));
      var gx = ((ox % cell) + cell) % cell, gy = ((oy % cell) + cell) % cell;
      function claim(x, y, w, h) {
        var x0 = Math.floor((x - gx) / cell) + 1, y0 = Math.floor((y - gy) / cell) + 1;
        var x1 = Math.floor((x + w - gx) / cell) + 1, y1 = Math.floor((y + h - gy) / cell) + 1;
        if (x0 < 0 || y0 < 0 || x1 >= cols || (y1 + 1) * cols > grid.length) return false;
        var a, b;
        for (b = y0; b <= y1; b++) for (a = x0; a <= x1; a++) if (grid[b * cols + a]) return false;
        for (b = y0; b <= y1; b++) for (a = x0; a <= x1; a++) grid[b * cols + a] = 1;
        return true;
      }
      function label(text, x, y, size, color, weight, force, key) {
        ctx.font = (weight || "500") + " " + size + "px " + font;
        var w = textWidth(text, size);
        var lx = x - w / 2, ly = y;
        if (!claim(lx - 2, ly - 2, w + 4, size + 4) && !force) return null;
        if (key) nextShown[key] = 1;
        ctx.lineWidth = 3;
        ctx.strokeStyle = colors.halo;
        ctx.strokeText(text, lx, ly + size * 0.8);
        ctx.fillStyle = color;
        ctx.fillText(text, lx, ly + size * 0.8);
        return [lx, ly, w, size];
      }
      ctx.textBaseline = "alphabetic";
      ctx.lineJoin = "round";
      var i, s, f = cam.k / fitK();

      // The selected and hovered systems are always named first.
      [sel, hover].forEach(function (s) {
        if (s < 0 || (mode !== "universe" && S.r[s] !== region)) return;
        label(S.n[s], wx(s) * k + ox, wy(s) * k + oy + r + 4, 13, colors.label, "700", true);
      });

      if (mode === "universe") {
        labelBoxes = [];
        // Region names first, at every zoom: they claim their
        // cells before system names fill in around them, and
        // they only thin out, never vanish (regionLabelAlpha),
        // so a zoomed-in region's name is always there to click.
        ctx.globalAlpha = regionLabelAlpha(f);
        var size = Math.min(12 + f * 0.8, 18);
        for (i = 0; i < P.regions.length; i++) {
          var rg = P.regions[i];
          if (!G.members[i].length) continue;
          var box = label(rg.name.toUpperCase(), rg.x * k + ox, rg.y * k + oy - size / 2, size, colors.region, "600");
          if (box) labelBoxes.push({ r: i, b: box });
        }
        ctx.globalAlpha = 1;
        // System names once the picture opens up.
        if (f > 6) {
          var vis = [];
          for (var o = 0; o < G.n; o++) {
            i = G.prio[o];
            var sx = S.x[i] * k + ox, sy = S.y[i] * k + oy;
            if (sx >= 0 && sy >= 0 && sx <= cw && sy <= ch) vis.push(i);
          }
          inPlaceFirst(vis, function (i) { return "s" + i; }, function (i) {
            label(S.n[i], S.x[i] * k + ox, S.y[i] * k + oy + r + 3, 11, colors.labelDim, null, false, "s" + i);
          });
        }
        shownLabels = nextShown;
        return;
      }

      // Tactical: every system, then the exits.
      inPlaceFirst(G.membersPrio[region], function (s) { return "t" + s; }, function (s) {
        label(S.n[s], S.rx[s] * k + ox, S.ry[s] * k + oy + r + 3, 12, colors.label, null, false, "t" + s);
      });
      var ex = G.exits[region], seen = {}, exits = [];
      for (i = 0; i < ex.length; i++) {
        if (seen[ex[i][1]]) continue;
        seen[ex[i][1]] = 1;
        exits.push(ex[i]);
      }
      inPlaceFirst(exits, function (e) { return "x" + e[1]; }, function (e) {
        var to = e[1], g = ghost(e[0], to);
        label(S.n[to] + " · " + P.regions[S.r[to]].name, g[0] * k + ox, g[1] * k + oy + r + 2, 10.5, colors.labelDim, null, false, "x" + to);
      });
      shownLabels = nextShown;
    }

    // Tactical constellation names: a faint backdrop drawn before
    // the systems, so they never take a system name's place.
    function drawConstellations(k, ox, oy) {
      var cr = G.consR, size = Math.min(13 + (cam.k / fitK() - 1) * 2, 20);
      ctx.font = "600 " + size + "px " + font;
      ctx.fillStyle = colors.cons;
      ctx.textAlign = "center";
      for (var i = 0; i < P.cons.length; i++) {
        if (P.cons[i].r !== region) continue;
        ctx.fillText(P.cons[i].name.toUpperCase(), cr[i][0] * k + ox, cr[i][1] * k + oy);
      }
      ctx.textAlign = "start";
    }

    // --- Picking ---
    function toWorld(px, py) { return [(px - cw / 2) / cam.k + cam.x, (py - ch / 2) / cam.k + cam.y]; }

    function pick(px, py) {
      var k = cam.k, ox = cw / 2 - cam.x * k, oy = ch / 2 - cam.y * k;
      var best = -1, bd = 14 * 14, list = visible(), n = list ? list.length : G.n;
      for (var m = 0; m < n; m++) {
        var i = list ? list[m] : m;
        var dx = wx(i) * k + ox - px, dy = wy(i) * k + oy - py, d = dx * dx + dy * dy;
        if (d < bd) { bd = d; best = i; }
      }
      return best;
    }
    function pickGhost(px, py) {
      if (mode === "universe") return -1;
      var k = cam.k, ox = cw / 2 - cam.x * k, oy = ch / 2 - cam.y * k;
      var best = -1, bd = 14 * 14, ex = G.exits[region];
      for (var i = 0; i < ex.length; i++) {
        var g = ghost(ex[i][0], ex[i][1]);
        var dx = g[0] * k + ox - px, dy = g[1] * k + oy - py, d = dx * dx + dy * dy;
        if (d < bd) { bd = d; best = ex[i][1]; }
      }
      return best;
    }
    function pickRegionLabel(px, py) {
      for (var i = 0; i < labelBoxes.length; i++) {
        var b = labelBoxes[i].b;
        if (px >= b[0] - 4 && px <= b[0] + b[2] + 4 && py >= b[1] - 4 && py <= b[1] + b[3] + 4) return labelBoxes[i].r;
      }
      return -1;
    }

    // --- Camera ---
    // Far out of a region always leaves it. Deep into one enters
    // it only under the "Zoom to tactical" preference; clicking
    // the region's name is the default way in. The entry and exit
    // thresholds sit apart, so the views never flicker.
    // dir is the zoom's direction (+1 in, -1 out): each threshold
    // fires only its own way, so zooming in while a region's fly-in
    // still sits below the exit scale never throws you back out.
    function afterZoom(dir) {
      if (mode !== "universe") {
        if (dir < 0 && cam.k < fitK() * 0.7) leaveRegion();
        return;
      }
      if (dir <= 0 || !zoomEntryAllowed(zoomTactical)) return;
      var c = toWorld(cw / 2, ch / 2), s = nearestUniverse(c[0], c[1]);
      if (s < 0) return;
      var bb = G.bounds[S.r[s]], ext = Math.max(bb[2] - bb[0], bb[3] - bb[1]) * cam.k;
      if (ext >= Math.min(cw, ch) * 1.25) enterRegion(S.r[s], s, true);
    }
    function nearestUniverse(x, y) {
      var best = -1, bd = Infinity;
      for (var i = 0; i < G.n; i++) {
        var dx = S.x[i] - x, dy = S.y[i] - y, d = dx * dx + dy * dy;
        if (d < bd) { bd = d; best = i; }
      }
      return best;
    }

    // Entering flies in: the region opens from a little farther out
    // (just past the exit threshold when the zoom brought us here,
    // so the glide carries on) to its fit, or to the focus system.
    function enterRegion(ri, focus, byZoom) {
      if (ri < 0) return;
      stopMotion();
      mode = "region";
      region = ri;
      fit();
      var tx = cam.x, ty = cam.y, tk = cam.k;
      if (focus >= 0 && S.r[focus] === ri && !byZoom) {
        tx = S.rx[focus];
        ty = S.ry[focus];
        tk = fitK() * 1.6;
      }
      cam.k = fitK() * (byZoom ? 0.75 : 0.55);
      flyTo(tx, ty, tk, byZoom ? 260 : 420);
      crumbs();
      if (byZoom) hint("Tactical view of " + P.regions[ri].name + ". Zoom out to return to New Eden.");
      syncURL();
      schedule();
    }
    function leaveRegion() {
      if (mode === "universe") return;
      var rg = P.regions[region], bb = G.bounds[region];
      stopMotion();
      mode = "universe";
      region = -1;
      // Out to just below the entry threshold, centred where we
      // were, gliding back from a little closer in. The start
      // stays below that threshold too (0.95 x 1.15 < 1.25), so
      // the next zoom step cannot re-enter the region at once.
      var tk = clampK(Math.min(cw, ch) * 0.95 / Math.max(bb[2] - bb[0], bb[3] - bb[1], 1));
      cam.x = (bb[0] + bb[2]) / 2;
      cam.y = (bb[1] + bb[3]) / 2;
      cam.k = tk * 1.15;
      flyTo(cam.x, cam.y, tk, 300);
      crumbs();
      hint(rg.name + " in New Eden.");
      syncURL();
      schedule();
    }

    // --- Chrome: crumbs, hint, card, URL ---
    function crumbs() {
      var inRegion = mode !== "universe";
      crumbSep.hidden = !inRegion;
      crumbRegion.hidden = !inRegion;
      crumbRegion.textContent = inRegion ? P.regions[region].name : "";
      el.classList.toggle("map-tactical", inRegion);
      updateCard();
    }
    function hint(text) {
      hintEl.textContent = text;
      hintEl.hidden = false;
      clearTimeout(hintTimer);
      hintTimer = setTimeout(function () { hintEl.hidden = true; }, 3200);
    }
    function syncURL() {
      if (!window.history || !history.replaceState) return;
      var q = [];
      if (mode !== "universe") q.push("region=" + P.regions[region].id);
      if (sel >= 0) q.push("system=" + S.id[sel]);
      var url = location.pathname + (q.length ? "?" + q.join("&") : "") + location.hash;
      try { history.replaceState(history.state, "", url); } catch (e) { /* sandboxed */ }
    }

    function select(i, follow) {
      sel = i;
      if (i >= 0 && follow && mode !== "universe" && S.r[i] !== region) {
        enterRegion(S.r[i], i);
      }
      updateCard();
      syncURL();
      schedule();
    }

    function updateCard() {
      if (sel < 0) { card.hidden = true; return; }
      var i = sel, rg = P.regions[S.r[i]];
      card.hidden = false;
      card.querySelector(".map-card-name").textContent = S.n[i];
      var sec = card.querySelector(".map-card-sec");
      sec.className = "map-card-sec sec sec-" + G.tier[i];
      sec.textContent = secText(S.s[i]);
      var c = S.c[i];
      card.querySelector(".map-card-path").textContent = (c >= 0 ? P.cons[c].name + " · " : "") + rg.name;
      var nb = G.adj[i].slice().sort(function (a, b) { return S.n[a] < S.n[b] ? -1 : 1; });
      card.querySelector(".map-card-gates").textContent = nb.length === 1 ? "1 stargate" : nb.length + " stargates";
      var ul = card.querySelector(".map-card-links");
      ul.textContent = "";
      nb.forEach(function (b) {
        var li = root.createElement("li");
        var btn = root.createElement("button");
        btn.type = "button";
        btn.className = "map-card-link";
        btn.setAttribute("data-map-sys", String(b));
        var s = root.createElement("span");
        s.className = "sec sec-" + G.tier[b];
        s.textContent = secText(S.s[b]);
        btn.appendChild(s);
        btn.appendChild(root.createTextNode(" " + S.n[b] + (S.r[b] !== S.r[i] ? " · " + P.regions[S.r[b]].name : "")));
        li.appendChild(btn);
        ul.appendChild(li);
      });
      card.querySelector("[data-map-system-page]").href = "/system/?system=" + S.id[i];
      card.querySelector("[data-map-enter]").hidden = mode !== "universe";
    }

    // --- Search ---
    var hits = [], hitAt = -1;
    function search(q) {
      q = q.trim().toLowerCase();
      hits = [];
      if (q.length < 2) return renderHits();
      var i, pre = [], mid = [];
      for (i = 0; i < P.regions.length; i++) {
        var rl = G.regionLower[i], p = rl.indexOf(q);
        if (p === 0) pre.push({ r: i }); else if (p > 0) mid.push({ r: i });
      }
      for (i = 0; i < G.n && pre.length + mid.length < 200; i++) {
        var at = G.lower[i].indexOf(q);
        if (at === 0) pre.push({ s: i }); else if (at > 0) mid.push({ s: i });
      }
      hits = pre.concat(mid).slice(0, 8);
      hitAt = hits.length ? 0 : -1;
      renderHits();
    }
    function renderHits() {
      findList.textContent = "";
      findList.hidden = !hits.length;
      findInput.setAttribute("aria-expanded", hits.length ? "true" : "false");
      hits.forEach(function (h, idx) {
        var li = root.createElement("li");
        li.setAttribute("role", "option");
        li.id = "map-find-opt-" + idx;
        li.setAttribute("data-hit", String(idx));
        if (idx === hitAt) { li.className = "active"; li.setAttribute("aria-selected", "true"); }
        if (h.r !== undefined) {
          li.appendChild(root.createTextNode(P.regions[h.r].name));
          var k = root.createElement("small");
          k.textContent = " Region";
          li.appendChild(k);
        } else {
          var s = root.createElement("span");
          s.className = "sec sec-" + G.tier[h.s];
          s.textContent = secText(S.s[h.s]);
          li.appendChild(s);
          li.appendChild(root.createTextNode(" " + S.n[h.s]));
          var sm = root.createElement("small");
          sm.textContent = " " + P.regions[S.r[h.s]].name;
          li.appendChild(sm);
        }
        findList.appendChild(li);
      });
      if (hitAt >= 0) findInput.setAttribute("aria-activedescendant", "map-find-opt-" + hitAt);
      else findInput.removeAttribute("aria-activedescendant");
    }
    function choose(h) {
      if (!h) return;
      hits = [];
      renderHits();
      findInput.value = "";
      if (h.r !== undefined) { sel = -1; enterRegion(h.r, -1); return; }
      sel = h.s;
      enterRegion(S.r[h.s], h.s);
    }

    // --- Input ---
    var pointers = {}, gesture = null, lastTap = null;
    function pt(e) { var b = canvas.getBoundingClientRect(); return [e.clientX - b.left, e.clientY - b.top]; }

    function onPointerDown(e) {
      stopMotion();
      canvas.setPointerCapture(e.pointerId);
      var p = pt(e);
      pointers[e.pointerId] = p;
      var ids = Object.keys(pointers);
      if (ids.length === 1) gesture = { start: p, last: p, t: Date.now(), moved: false };
      else if (ids.length === 2) {
        var a = pointers[ids[0]], b = pointers[ids[1]];
        gesture = { pinch: true, d: Math.hypot(a[0] - b[0], a[1] - b[1]), mid: [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2], moved: true };
      }
    }
    function onPointerMove(e) {
      var p = pt(e);
      if (!pointers[e.pointerId]) {
        if (e.pointerType === "mouse") {
          var h = pick(p[0], p[1]);
          if (h !== hover) { hover = h; schedule(); }
          canvas.style.cursor = h >= 0 || pickGhost(p[0], p[1]) >= 0 || (mode === "universe" && pickRegionLabel(p[0], p[1]) >= 0) ? "pointer" : "";
        }
        return;
      }
      pointers[e.pointerId] = p;
      var ids = Object.keys(pointers);
      if (gesture && gesture.pinch && ids.length >= 2) {
        var a = pointers[ids[0]], b = pointers[ids[1]];
        var d = Math.hypot(a[0] - b[0], a[1] - b[1]), mid = [(a[0] + b[0]) / 2, (a[1] + b[1]) / 2];
        // Fingers track exactly: no easing under a pinch.
        cam.x -= (mid[0] - gesture.mid[0]) / cam.k;
        cam.y -= (mid[1] - gesture.mid[1]) / cam.k;
        if (gesture.d > 0) { setZoomAt(mid[0], mid[1], cam.k * d / gesture.d); afterZoom(d > gesture.d ? 1 : -1); }
        gesture.d = d;
        gesture.mid = mid;
        schedule();
        return;
      }
      if (!gesture) return;
      var dx = p[0] - gesture.last[0], dy = p[1] - gesture.last[1];
      if (!gesture.moved && Math.hypot(p[0] - gesture.start[0], p[1] - gesture.start[1]) > 6) gesture.moved = true;
      if (gesture.moved) {
        cam.x -= dx / cam.k;
        cam.y -= dy / cam.k;
        canvas.classList.add("dragging");
        schedule();
      }
      gesture.last = p;
    }
    function onPointerUp(e) {
      var p = pt(e);
      var g = gesture;
      delete pointers[e.pointerId];
      canvas.classList.remove("dragging");
      if (Object.keys(pointers).length) return;
      gesture = null;
      if (!g || g.moved || g.pinch) return;
      // A tap: double-tap zooms, a single tap picks.
      var now = Date.now();
      if (lastTap && now - lastTap.t < 320 && Math.hypot(p[0] - lastTap.p[0], p[1] - lastTap.p[1]) < 24) {
        lastTap = null;
        glideZoom(p[0], p[1], 2);
        return;
      }
      lastTap = { t: now, p: p };
      tap(p[0], p[1]);
    }
    function onPointerCancel(e) {
      delete pointers[e.pointerId];
      if (!Object.keys(pointers).length) gesture = null;
      canvas.classList.remove("dragging");
    }
    function tap(px, py) {
      var s = pick(px, py);
      if (s >= 0) { select(s); return; }
      var g = pickGhost(px, py);
      if (g >= 0) { sel = g; enterRegion(S.r[g], g); return; }
      if (mode === "universe") {
        var r = pickRegionLabel(px, py);
        if (r >= 0) {
          enterRegion(r, -1);
          hint("Tactical view of " + P.regions[r].name + ". Zoom out to return to New Eden.");
          return;
        }
      }
      if (sel >= 0) select(-1);
    }
    // The wheel. A mouse wheel zooms about the cursor; a touchpad's
    // two-finger scroll pans; a touchpad pinch (which browsers send
    // as a wheel with ctrlKey set) zooms. A mouse wheel moves in
    // whole notches (deltaMode lines, or the legacy wheelDeltaY in
    // multiples of 120); a touchpad streams small pixel deltas,
    // often sideways too, and a stream keeps its first reading so
    // one gesture never switches halfway.
    var padUntil = 0;
    function isTouchpad(e, dx, dy) {
      if (e.deltaMode !== 0) return false;
      var now = performance.now();
      var pad = dx !== 0 ||
        (typeof e.wheelDeltaY === "number" && e.wheelDeltaY !== 0 ? e.wheelDeltaY === -3 * e.deltaY || Math.abs(e.wheelDeltaY) % 120 !== 0
          : Math.abs(dy) < 40 || dy % 1 !== 0);
      if (now < padUntil) pad = true;
      if (pad) padUntil = now + 90;
      return pad;
    }
    function onWheel(e) {
      e.preventDefault();
      var p = pt(e);
      var dx = e.deltaX, dy = e.deltaY;
      if (e.deltaMode === 1) { dx *= 16; dy *= 16; } else if (e.deltaMode === 2) { dx *= ch; dy *= ch; }
      if (e.ctrlKey) { chaseZoom(p[0], p[1], Math.exp(-dy * 0.012)); return; }
      if (isTouchpad(e, dx, dy)) {
        stopMotion();
        cam.x += dx / cam.k;
        cam.y += dy / cam.k;
        schedule();
        return;
      }
      chaseZoom(p[0], p[1], Math.exp(-dy * 0.0024));
    }
    // Safari sends its trackpad pinch as gesture events.
    var gestureScale = 1;
    function onGestureStart(e) { e.preventDefault(); gestureScale = 1; }
    function onGestureChange(e) {
      e.preventDefault();
      var p = typeof e.clientX === "number" ? pt(e) : [cw / 2, ch / 2];
      chaseZoom(p[0], p[1], e.scale / gestureScale);
      gestureScale = e.scale;
    }

    function flyFit() {
      var b = viewBounds();
      flyTo((b[0] + b[2]) / 2, (b[1] + b[3]) / 2, fitK(), 360);
    }
    function pan(dx, dy) {
      var base = anim && anim.kind === "fly" ? anim : null;
      flyTo((base ? base.x1 : cam.x) + dx / cam.k, (base ? base.y1 : cam.y) + dy / cam.k,
        base ? Math.exp(base.l1) : cam.k, 180);
    }
    // Keys work anywhere in the map frame but the search box.
    function onKey(e) {
      if (e.altKey || e.ctrlKey || e.metaKey) return;
      var t = e.target;
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA")) return;
      switch (e.key) {
        case "ArrowLeft": case "a": pan(-90, 0); break;
        case "ArrowRight": case "d": pan(90, 0); break;
        case "ArrowUp": case "w": pan(0, -90); break;
        case "ArrowDown": case "s": pan(0, 90); break;
        case "+": case "=": case "e": glideZoom(cw / 2, ch / 2, 1.5); break;
        case "-": case "_": case "q": glideZoom(cw / 2, ch / 2, 1 / 1.5); break;
        case "0": case "f": flyFit(); break;
        case "/": findInput.focus(); break;
        case "?": toggleKeys(); break;
        case "Enter":
          if (sel >= 0 && mode === "universe") enterRegion(S.r[sel], sel);
          else return;
          break;
        case "Escape":
          if (!keysPanel.hidden) toggleKeys(false);
          else if (sel >= 0) select(-1);
          else leaveRegion();
          break;
        default: return;
      }
      e.preventDefault();
    }
    var keysPanel = el.querySelector(".map-keys"), keysBtn = el.querySelector("[data-map-keys]");
    function toggleKeys(open) {
      keysPanel.hidden = open === undefined ? !keysPanel.hidden : !open;
      keysBtn.setAttribute("aria-expanded", keysPanel.hidden ? "false" : "true");
    }

    function onClick(e) {
      var t = e.target;
      var b = t.closest("[data-map-sys]");
      if (b && el.contains(b)) { select(+b.getAttribute("data-map-sys"), true); return; }
      if (t.closest("[data-map-close]")) { select(-1); return; }
      if (t.closest("[data-map-enter]")) { if (sel >= 0) enterRegion(S.r[sel], sel); return; }
      if (t.closest("[data-map-universe]")) { leaveRegion(); return; }
      var z = t.closest("[data-map-zoom]");
      if (z) {
        var how = z.getAttribute("data-map-zoom");
        if (how === "fit") flyFit(); else glideZoom(cw / 2, ch / 2, how === "in" ? 1.6 : 1 / 1.6);
        return;
      }
      if (t.closest("[data-map-keys]")) { toggleKeys(); return; }
      var hit = t.closest("[data-hit]");
      if (hit && findList.contains(hit)) { choose(hits[+hit.getAttribute("data-hit")]); }
    }
    // The region index sits outside the map frame.
    function onIndexClick(e) {
      var a = e.target.closest("[data-map-region]");
      if (!a) return;
      var ri = G.regionByID[+a.getAttribute("data-map-region")];
      if (ri === undefined) return;
      e.preventDefault();
      sel = -1;
      enterRegion(ri, -1);
      el.scrollIntoView({ block: "start", behavior: "smooth" });
    }
    function onFindKey(e) {
      if (e.key === "ArrowDown" || e.key === "ArrowUp") {
        if (!hits.length) return;
        hitAt = (hitAt + (e.key === "ArrowDown" ? 1 : hits.length - 1)) % hits.length;
        renderHits();
        e.preventDefault();
      } else if (e.key === "Enter") {
        if (hitAt >= 0) { choose(hits[hitAt]); e.preventDefault(); }
      } else if (e.key === "Escape") {
        hits = []; renderHits();
      }
    }

    // --- Size ---
    function resize() {
      var b = stage.getBoundingClientRect();
      var w = Math.max(1, Math.round(b.width)), h = Math.max(1, Math.round(b.height));
      dpr = Math.min(window.devicePixelRatio || 1, 2.5);
      if (w === cw && h === ch && canvas.width === Math.round(w * dpr)) return;
      var hadK = fitted ? cam.k / fitK() : 0;
      cw = w; ch = h;
      canvas.width = Math.round(w * dpr);
      canvas.height = Math.round(h * dpr);
      if (!fitted) { fit(); fitted = true; restore(); }
      else if (hadK) cam.k = fitK() * hadK;
      draw();
    }

    // The URL's ?region= / ?system= on first draw.
    function restore() {
      var qs = new URLSearchParams(location.search);
      var sys = G.byID[+qs.get("system")];
      var ri = G.regionByID[+qs.get("region")];
      if (sys !== undefined) sel = sys;
      if (ri !== undefined) enterRegion(ri, sys !== undefined ? sys : -1);
      else if (sys !== undefined) enterRegion(S.r[sys], sys);
      updateCard();
    }

    // --- Wiring and teardown ---
    readColors();
    statusEl.hidden = true;
    findInput.disabled = false;
    el.querySelectorAll("[data-map-zoom], [data-map-keys]").forEach(function (b) { b.disabled = false; });
    if (zoomPref) {
      zoomPref.disabled = false;
      zoomPref.checked = zoomTactical;
      zoomPref.addEventListener("change", function () {
        zoomTactical = zoomPref.checked;
        try { window.localStorage.setItem(ZOOM_TACTICAL_KEY, zoomTactical ? "1" : "0"); } catch (e) { /* private mode */ }
        hint(zoomTactical
          ? "Zoom to tactical is on: zooming deep into a region opens its tactical view."
          : "Zoom to tactical is off: click a region's name for its tactical view.");
      });
    }
    var index = root.querySelector(".map-index");
    canvas.addEventListener("pointerdown", onPointerDown);
    canvas.addEventListener("pointermove", onPointerMove);
    canvas.addEventListener("pointerup", onPointerUp);
    canvas.addEventListener("pointercancel", onPointerCancel);
    canvas.addEventListener("pointerleave", function () { if (hover >= 0) { hover = -1; schedule(); } });
    canvas.addEventListener("wheel", onWheel, { passive: false });
    canvas.addEventListener("gesturestart", onGestureStart);
    canvas.addEventListener("gesturechange", onGestureChange);
    el.addEventListener("keydown", onKey);
    el.addEventListener("click", onClick);
    findInput.addEventListener("input", function () { search(findInput.value); });
    findInput.addEventListener("keydown", onFindKey);
    if (index) index.addEventListener("click", onIndexClick);

    var ro = new ResizeObserver(resize);
    ro.observe(stage);
    // The theme toggle recolors the page; the canvas follows.
    var mo = new MutationObserver(function () { readColors(); schedule(); });
    mo.observe(root.documentElement, { attributes: true, attributeFilter: ["data-theme", "class"] });

    function teardown() {
      ro.disconnect();
      mo.disconnect();
      if (raf) cancelAnimationFrame(raf);
      clearTimeout(hintTimer);
      root.removeEventListener("evesynapse:content-will-swap", teardown);
    }
    root.addEventListener("evesynapse:content-will-swap", teardown);
  }

  function init(rootEl) {
    var el = rootEl.querySelector && rootEl.querySelector("[data-map]");
    if (!el || el.getAttribute("data-map-bound")) return;
    el.setAttribute("data-map-bound", "1");
    var src = el.getAttribute("data-src");
    if (!src || !window.fetch || !window.Path2D || !window.ResizeObserver) return;
    loadPayload(src).then(function (P) {
      if (!el.isConnected) return;
      mount(el, P);
    }).catch(function () {
      var st = el.querySelector(".map-status");
      if (st) st.textContent = "The map could not be loaded. Reload the page to try again.";
    });
  }

  // This script loads only with the map page, after the shell's
  // first content-ready has fired: run for the document as it
  // stands, then for every swap that brings a map back.
  if (typeof window !== "undefined") {
    if (document.readyState === "loading") {
      document.addEventListener("DOMContentLoaded", function () { init(document); });
    } else {
      init(document);
    }
    if (window.EveSynapseContent && window.EveSynapseContent.onReady) {
      window.EveSynapseContent.onReady(init);
    }
  }

  // Exposed for the node tests only (jstest/map_test.js).
  if (typeof module !== "undefined" && module.exports) {
    module.exports = { secRounded: secRounded, secTier: secTier, secText: secText, prepare: prepare, regionLabelAlpha: regionLabelAlpha, zoomEntryAllowed: zoomEntryAllowed };
  }
})();
