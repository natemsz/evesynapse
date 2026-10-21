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

  // --- Live layers: the pure part ---

  /**
   * The live layers payload (Go: mapLayersPayload, map_layers.go).
   * Systems are keyed by ID; joinLayers maps them onto the universe
   * payload's indices. Every layer carries at: its fetch time (unix).
   * @typedef {Object} LayersPayload
   * @property {{at:number,id:number[],ship:number[],pod:number[],npc:number[]}} kills
   * @property {{at:number,id:number[],n:number[]}} jumps
   * @property {{at:number,id:number[],a:number[],f:number[]}} sov  a: alliance index (-1 none); f: NPC faction
   * @property {{id:number,name:string}[]} alliances
   * @property {{at:number,id:number[],occ:number[],own:number[],state:string[],pct:number[]}} fw
   * @property {{at:number,list:{cons:number,staging:number,sys:number[],state:string,inf:number,boss:boolean,f:number}[]}} inc
   * @property {{id:number,name:string}[]} factions
   */

  var FW_STATE = { uncontested: 1, contested: 2, vulnerable: 3, captured: 4 };

  // joinLayers maps a layers payload onto per-system arrays indexed
  // like the universe payload, with each heat layer's maximum.
  function joinLayers(P, L) {
    var G = prepare(P), n = G.n, i, j;
    var J = { at: {}, max: {}, factions: {}, alliances: L.alliances || [], incs: [] };
    function each(part, name, fn) {
      part = part || {};
      J.at[name] = part.at || 0;
      var ids = part.id || [];
      for (j = 0; j < ids.length; j++) {
        i = G.byID[ids[j]];
        if (i !== undefined) fn(i, j, part);
      }
    }
    J.ship = new Int32Array(n); J.pod = new Int32Array(n); J.npc = new Int32Array(n); J.kills = new Int32Array(n);
    each(L.kills, "kills", function (i, j, p) {
      J.ship[i] = p.ship[j]; J.pod[i] = p.pod[j]; J.npc[i] = p.npc[j];
      J.kills[i] = p.ship[j] + p.pod[j];
    });
    J.at.npc = J.at.kills;
    J.jumps = new Int32Array(n);
    each(L.jumps, "jumps", function (i, j, p) { J.jumps[i] = p.n[j]; });
    J.sovA = new Int16Array(n).fill(-1); J.sovF = new Float64Array(n);
    J.allianceMembers = J.alliances.map(function () { return []; });
    each(L.sov, "sov", function (i, j, p) {
      J.sovA[i] = p.a[j];
      J.sovF[i] = p.f[j];
      if (p.a[j] >= 0 && J.allianceMembers[p.a[j]]) J.allianceMembers[p.a[j]].push(i);
    });
    J.fwOcc = new Float64Array(n); J.fwOwn = new Float64Array(n); J.fwState = new Uint8Array(n); J.fwPct = new Uint8Array(n);
    each(L.fw, "fw", function (i, j, p) {
      J.fwOcc[i] = p.occ[j]; J.fwOwn[i] = p.own[j];
      J.fwState[i] = FW_STATE[p.state[j]] || 1; J.fwPct[i] = p.pct[j];
    });
    J.inc = new Uint8Array(n); J.incOf = new Int16Array(n).fill(-1);
    var inc = L.inc || {};
    J.at.inc = inc.at || 0;
    (inc.list || []).forEach(function (x, xi) {
      J.incs.push(x);
      (x.sys || []).forEach(function (id) {
        var s = G.byID[id];
        if (s !== undefined) { J.inc[s] = Math.max(J.inc[s], 1); J.incOf[s] = xi; }
      });
      var st = G.byID[x.staging];
      if (st !== undefined) { J.inc[st] = 2; J.incOf[st] = xi; }
    });
    (L.factions || []).forEach(function (f) { J.factions[f.id] = f.name; });
    ["kills", "npc", "jumps"].forEach(function (k) {
      var arr = J[k], m = 0;
      for (i = 0; i < n; i++) if (arr[i] > m) m = arr[i];
      J.max[k] = m;
    });
    // Threat: the decayed score on a fixed scale (THREAT_TOP is
    // "extreme"), kills in the last hour, the latest kill, camps,
    // and each system's spillover from its gate neighbours.
    J.threat = new Float32Array(n); J.threatHour = new Int32Array(n); J.threatLast = new Float64Array(n);
    each(L.threat, "threat", function (i, j, p) {
      J.threat[i] = p.s[j] / 100; J.threatHour[i] = p.k1[j]; J.threatLast[i] = p.last[j];
    });
    J.max.threat = THREAT_TOP;
    J.camps = []; J.campOf = new Int16Array(n).fill(-1);
    ((L.threat || {}).camps || []).forEach(function (c) {
      var s = G.byID[c.sys];
      if (s === undefined) return;
      J.campOf[s] = J.camps.length;
      J.camps.push({ sys: s, n: c.n, kind: c.kind, gid: c.gid, group: c.group, since: c.since });
    });
    J.spill = new Float32Array(n);
    for (i = 0; i < n; i++) {
      var nb = G.adj[i], m2 = 0;
      for (j = 0; j < nb.length; j++) if (J.threat[nb[j]] > m2) m2 = J.threat[nb[j]];
      J.spill[i] = m2 * THREAT_SPILL;
    }
    return J;
  }

  // The threat scale: the decayed score at which each level starts.
  // A fresh kill is worth 1 and halves every 45 minutes.
  var THREAT_LEVELS = [
    { at: 0, name: "Quiet" }, { at: 0.5, name: "Low" }, { at: 2, name: "Medium" },
    { at: 5, name: "High" }, { at: 10, name: "Extreme" }
  ];
  var THREAT_TOP = 10;
  var THREAT_SPILL = 0.3; // a neighbour's share of a system's threat
  function threatLevel(score) {
    var lv = 0;
    for (var i = 0; i < THREAT_LEVELS.length; i++) if (score >= THREAT_LEVELS[i].at) lv = i;
    return lv;
  }

  // heatT places a value on its layer's log scale, 0..1.
  function heatT(v, max) {
    return v <= 0 || max <= 0 ? 0 : Math.log(1 + v) / Math.log(1 + max);
  }

  // allianceHue gives every alliance its own stable hue: neighbours
  // with near IDs still land far apart on the wheel.
  function allianceHue(id) {
    return (Math.imul(id | 0, 2654435761) >>> 0) % 360;
  }

  // --- Routing: the pure part ---
  // Routes are planned here, over the payload's gates: Dijkstra with
  // a price for entering each system. "short" prices every jump the
  // same; "calm" adds the live threat (its level, gate camps, and
  // its spill onto gate neighbours), so the route bends around
  // trouble when a detour is cheap; "safe" does that and also
  // prices leaving high-sec steeply, as the game's own "safer" does.
  var ROUTE_MODES = { short: 1, safe: 1, calm: 1 };
  var THREAT_PRICE = [0, 3, 10, 40, 120];
  var CAMP_PRICE = 400, LOWSEC_PRICE = 50;
  // intel, when given, is the price of each system's fresh hostile
  // reports from the reader's intel channels (intelPrices).
  function routeCost(G, LY, mode, intel) {
    return function (j) {
      var c = 1;
      if (mode === "short") return c;
      if (intel) c += intel[j];
      if (mode === "safe" && G.tier[j] < 5) c += LOWSEC_PRICE;
      if (LY) {
        c += THREAT_PRICE[threatLevel(LY.threat[j])];
        if (LY.campOf[j] >= 0) c += CAMP_PRICE;
        if (threatLevel(LY.spill[j]) >= 2) c += 2;
      }
      return c;
    };
  }
  // planRoute: the cheapest path from a to b as system indices, a
  // and b included; null when no gates join them.
  function planRoute(G, a, b, cost) {
    var n = G.n, dist = new Float64Array(n).fill(Infinity), prev = new Int32Array(n).fill(-1);
    var heap = [], i;
    function push(d, v) {
      heap.push([d, v]);
      for (i = heap.length - 1; i > 0;) {
        var p = (i - 1) >> 1;
        if (heap[p][0] <= heap[i][0]) break;
        var t = heap[p]; heap[p] = heap[i]; heap[i] = t; i = p;
      }
    }
    function pop() {
      var top = heap[0], last = heap.pop();
      if (heap.length) {
        heap[0] = last;
        for (i = 0; ;) {
          var l = 2 * i + 1, r = l + 1, m = i;
          if (l < heap.length && heap[l][0] < heap[m][0]) m = l;
          if (r < heap.length && heap[r][0] < heap[m][0]) m = r;
          if (m === i) break;
          var t = heap[m]; heap[m] = heap[i]; heap[i] = t; i = m;
        }
      }
      return top;
    }
    dist[a] = 0;
    push(0, a);
    while (heap.length) {
      var top = pop(), u = top[1];
      if (top[0] > dist[u]) continue;
      if (u === b) break;
      var nb = G.adj[u];
      for (var k = 0; k < nb.length; k++) {
        var v = nb[k], d = dist[u] + cost(v);
        if (d < dist[v]) { dist[v] = d; prev[v] = u; push(d, v); }
      }
    }
    if (dist[b] === Infinity) return null;
    var path = [];
    for (var at = b; at !== -1; at = prev[at]) path.push(at);
    return path.reverse();
  }
  // routeSummary: the jumps by security band, the camps on the way,
  // and the systems hot enough to warn of (Medium threat and up),
  // hottest first.
  function routeSummary(G, LY, path, intel) {
    var s = { jumps: path.length - 1, high: 0, low: 0, nul: 0, camps: [], hot: [], top: 0, reported: [] };
    path.forEach(function (i, at) {
      if (at === 0) return; // where you start is not part of the trip
      var t = G.tier[i];
      if (t >= 5) s.high++; else if (t > 0) s.low++; else s.nul++;
      if (intel && intel[i] > 0) s.reported.push(i);
      if (!LY) return;
      if (LY.campOf[i] >= 0) s.camps.push(i);
      var lv = threatLevel(LY.threat[i]);
      if (lv > s.top) s.top = lv;
      if (lv >= 2) s.hot.push(i);
    });
    if (LY) s.hot.sort(function (x, y) { return LY.threat[y] - LY.threat[x]; });
    return s;
  }

  // --- Intel channels: the pure part ---
  // joinIntel indexes /map/intel.json by system: each system's reports
  // newest first, its freshness (1 just now, 0 at 30 minutes), and the
  // price a route pays to cross it. A clear newer than a system's last
  // hostile report clears it; hostiles cost up to INTEL_PRICE, fading
  // with age, more for a bigger count.
  var INTEL_LIVE = 1800, INTEL_PRICE = 200;
  function intelFresh(at, now) { return Math.max(0, 1 - (now - at) / INTEL_LIVE); }
  function joinIntel(G, D, now) {
    var I = { channels: D.channels || [], bySys: {}, systems: [], prices: new Float32Array(G.n), hostile: 0, clear: 0 };
    I.names = {};
    I.channels.forEach(function (c) { I.names[c.id] = c.name; });
    (D.reports || []).forEach(function (r) {
      var i = G.byID[r.sys];
      if (i === undefined || intelFresh(r.at, now) <= 0) return;
      if (!I.bySys[i]) { I.bySys[i] = []; I.systems.push(i); }
      I.bySys[i].push(r);
    });
    I.systems.forEach(function (i) {
      var list = I.bySys[i].sort(function (a, b) { return b.at - a.at || b.id - a.id; });
      var last = list[0];
      if (last.clear) { I.clear++; return; }
      I.hostile++;
      I.prices[i] = INTEL_PRICE * intelFresh(last.at, now) * (last.n > 5 ? 1.5 : 1);
    });
    return I;
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
        halo: tok("--map-halo", "rgba(10,7,4,0.85)"),
        // The live layers.
        dim: tok("--map-dim", "rgba(200,180,160,0.28)"),
        heatLo: rgbOf(tok("--map-heat-lo", "#7a2a0c")),
        heatHi: rgbOf(tok("--map-heat-hi", "#ff6a1a")),
        heatCore: rgbOf(tok("--map-heat-core", "#ffe2a8")),
        heatBlend: tok("--map-heat-blend", "lighter"),
        npc: tok("--map-npc", "#9a8f86"),
        unheld: tok("--map-unheld", "rgba(200,180,160,0.35)"),
        sovL: tok("--map-sov-l", "62%"),
        threatRamp: [rgbOf(tok("--map-threat-lo", "#e0a020")), rgbOf(tok("--map-threat-hi", "#e0301c")), rgbOf(tok("--map-threat-core", "#fff0e0"))],
        route: tok("--map-route", "#ffd27a"),
        routeHalo: tok("--map-route-halo", "rgba(255,210,122,0.25)"),
        intel: rgbOf(tok("--map-intel", "#3fd8ff")),
        intelClear: rgbOf(tok("--map-intel-clear", "#4fbf6a")),
        camp: tok("--map-camp", "#ff3b3b"),
        inc: tok("--map-inc", "#c64bd6"),
        incStage: tok("--map-inc-staging", "#ff4fd8"),
        faction: {
          500001: tok("--map-f-caldari", "#3d8bff"),
          500002: tok("--map-f-minmatar", "#d8452f"),
          500003: tok("--map-f-amarr", "#e8b54a"),
          500004: tok("--map-f-gallente", "#3fbf7a")
        }
      };
      font = cs.fontFamily || font;
      widthCache = {};
    }
    // rgbOf normalizes any CSS color to [r, g, b] through the
    // canvas's own parser.
    function rgbOf(c) {
      ctx.fillStyle = "#000";
      ctx.fillStyle = c;
      var v = ctx.fillStyle;
      if (v.charAt(0) === "#") return [parseInt(v.substr(1, 2), 16), parseInt(v.substr(3, 2), 16), parseInt(v.substr(5, 2), 16)];
      var m = v.match(/[\d.]+/g) || [0, 0, 0];
      return [+m[0], +m[1], +m[2]];
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

      // The planned route, under the systems it joins.
      drawRoute(k, ox, oy);

      // Systems: by security, or by the live layer showing.
      var r = radius(), list = visible(), n = list ? list.length : G.n;
      var pad = 40;
      if (layer === "security" || !LY) {
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
      } else {
        drawLayer(k, ox, oy, r, list, n, pad);
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
      drawIntel(k, ox, oy, r);
      drawRouteEnds(k, ox, oy, r);

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
        // Geometric hysteresis: a name already showing claims a box
        // a little inside its text, a newcomer one a little outside,
        // so two names at the edge of touching don't trade places.
        var pad = key && shownLabels[key] ? -1 : 3;
        if (!claim(lx - pad, ly - pad, w + 2 * pad, size + 2 * pad) && !force) return null;
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

      // The selected and hovered systems, and the route's ends, are
      // always named first.
      [sel, hover, routeFrom, routeTo].forEach(function (s, at, all) {
        if (s < 0 || all.indexOf(s) !== at || (mode !== "universe" && S.r[s] !== region)) return;
        label(S.n[s], wx(s) * k + ox, wy(s) * k + oy + r + 4, 13, colors.label, "700", true);
      });

      if (mode === "universe") {
        labelBoxes = [];
        // Region names first, at every zoom: they claim their
        // cells before system names fill in around them, and
        // they only thin out, never vanish (regionLabelAlpha),
        // so a zoomed-in region's name is always there to click.
        ctx.globalAlpha = regionLabelAlpha(f);
        var size = Math.min(12 + f * 0.8, 18), regs = [];
        for (i = 0; i < P.regions.length; i++) if (G.members[i].length) regs.push(i);
        inPlaceFirst(regs, function (i) { return "R" + i; }, function (i) {
          var rg = P.regions[i];
          var box = label(rg.name.toUpperCase(), rg.x * k + ox, rg.y * k + oy - size / 2, size, colors.region, "600", false, "R" + i);
          if (box) labelBoxes.push({ r: i, b: box });
        });
        ctx.globalAlpha = 1;
        layerLabels(k, ox, oy, label, inPlaceFirst);
        intelLabels(k, ox, oy, label, inPlaceFirst);
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

      // Tactical: the layer's own labels, every system, the exits.
      layerLabels(k, ox, oy, label, inPlaceFirst);
      intelLabels(k, ox, oy, label, inPlaceFirst);
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

    // --- Live layers ---
    // layer is what the systems are colored by; LY is the joined
    // layers payload (joinLayers), null until it loads. Heat layers
    // (kills, NPC kills, jumps) dim the map and glow each active
    // system by its share of the hour's busiest, on a log scale;
    // sovereignty colors by holder; Faction Warfare by occupier with
    // its contest progress as an arc; incursions mark infested
    // systems and ring the staging system.
    var layer = "security", LY = null;
    // The layer the URL asked for, read before anything rewrites the
    // URL, and kept in it until the layers payload arrives.
    var startQuery = new URLSearchParams(location.search);
    var wantLayer = startQuery.get("layer");
    var HEAT = { kills: 1, npc: 1, jumps: 1, threat: 1 };
    var heatOrder = {};
    // The heat layers' scale: a gain in powers of two on the value
    // that glows white-hot (the hour's busiest, or Extreme for
    // threat), set from the legend and remembered per browser. Up
    // brings quiet systems out; down picks out only the hottest.
    var GAIN_KEY = "evesynapse-map-gain", heatGain = {};
    try { heatGain = JSON.parse(localStorage.getItem(GAIN_KEY) || "{}") || {}; } catch (e) { heatGain = {}; }
    function heatMax(name) {
      var g = +heatGain[name] || 0;
      return Math.max(LY.max[name] * Math.pow(2, -g), name === "threat" ? 0.25 : 1);
    }
    // Sovereignty can show as dots (each system its holder's color)
    // or as territory: the old influence-map look, each holder's
    // space filled and edged where it meets a neighbour's.
    var SOV_KEY = "evesynapse-map-sov", sovView = "systems", sovFields = {};
    try { if (localStorage.getItem(SOV_KEY) === "territory") sovView = "territory"; } catch (e) { /* private mode */ }
    if (startQuery.get("sov") === "territory") sovView = "territory";

    // heatColor walks a three-stop ramp (the heat ramp unless one is
    // given: threat has its own, amber through red to white-hot).
    function heatColor(t, a, ramp) {
      ramp = ramp || [colors.heatLo, colors.heatHi, colors.heatCore];
      t = Math.min(Math.max(t, 0), 1);
      var lo = ramp[0], hi = ramp[1], core = ramp[2], c;
      if (t < 0.75) {
        var u = t / 0.75;
        c = [lo[0] + (hi[0] - lo[0]) * u, lo[1] + (hi[1] - lo[1]) * u, lo[2] + (hi[2] - lo[2]) * u];
      } else {
        var w = (t - 0.75) / 0.25;
        c = [hi[0] + (core[0] - hi[0]) * w, hi[1] + (core[1] - hi[1]) * w, hi[2] + (core[2] - hi[2]) * w];
      }
      return "rgba(" + Math.round(c[0]) + "," + Math.round(c[1]) + "," + Math.round(c[2]) + "," + a + ")";
    }
    function allianceColor(ai) {
      var al = LY.alliances[ai];
      return "hsl(" + allianceHue(al ? al.id : ai) + ",68%," + colors.sovL + ")";
    }

    function drawLayer(k, ox, oy, r, list, n, pad) {
      var i, m, sx, sy;
      function on(i) {
        sx = wx(i) * k + ox; sy = wy(i) * k + oy;
        return !(sx < -pad || sy < -pad || sx > cw + pad || sy > ch + pad);
      }
      function batch(color, rr, test) {
        ctx.beginPath();
        for (m = 0; m < n; m++) {
          i = list ? list[m] : m;
          if (!test(i) || !on(i)) continue;
          ctx.moveTo(sx + rr, sy);
          ctx.arc(sx, sy, rr, 0, Math.PI * 2);
        }
        ctx.fillStyle = color;
        ctx.fill();
      }
      if (HEAT[layer]) {
        var vals = LY[layer], max = heatMax(layer);
        var ramp = layer === "threat" ? colors.threatRamp : null;
        batch(colors.dim, r * 0.8, function (i) { return vals[i] <= 0; });
        // Threat spills along the gates: in a region's tactical view
        // a quiet system beside a dangerous one wears a faint ring.
        if (layer === "threat" && mode !== "universe") {
          ctx.setLineDash([3, 3]);
          ctx.lineWidth = 1.5;
          for (m = 0; m < n; m++) {
            i = list ? list[m] : m;
            if (LY.spill[i] < THREAT_LEVELS[1].at || vals[i] >= LY.spill[i] || !on(i)) continue;
            ctx.beginPath();
            ctx.arc(sx, sy, r + 5, 0, Math.PI * 2);
            ctx.strokeStyle = heatColor(heatT(LY.spill[i], max), 0.7, ramp);
            ctx.stroke();
          }
          ctx.setLineDash([]);
        }
        // Quietest first, so the hottest glow on top.
        var order = heatOrder[layer];
        if (!order) {
          order = [];
          for (i = 0; i < G.n; i++) if (vals[i] > 0) order.push(i);
          order.sort(function (a, b) { return vals[a] - vals[b]; });
          heatOrder[layer] = order;
        }
        // In a region's tactical view systems sit far apart and are
        // named; a modest glow keeps neighbours and their names clear.
        var grow = mode === "universe" ? Math.min(1 + (cam.k / fitK() - 1) * 0.15, 3) : 0.9;
        ctx.globalCompositeOperation = colors.heatBlend;
        for (m = 0; m < order.length; m++) {
          i = order[m];
          if (mode !== "universe" && S.r[i] !== region) continue;
          if (!on(i)) continue;
          var t = heatT(vals[i], max), rr = r + (2 + 13 * t) * grow;
          ctx.beginPath();
          ctx.arc(sx, sy, rr, 0, Math.PI * 2);
          ctx.fillStyle = heatColor(t, 0.18 + 0.3 * t, ramp);
          ctx.fill();
        }
        ctx.globalCompositeOperation = "source-over";
        for (m = 0; m < order.length; m++) {
          i = order[m];
          if (mode !== "universe" && S.r[i] !== region) continue;
          if (!on(i)) continue;
          ctx.beginPath();
          ctx.arc(sx, sy, r + 0.6, 0, Math.PI * 2);
          ctx.fillStyle = heatColor(heatT(vals[i], max), 1, ramp);
          ctx.fill();
        }
        // Camps: a ring around each.
        if (layer === "threat") {
          ctx.lineWidth = 2;
          ctx.strokeStyle = colors.camp;
          LY.camps.forEach(function (c) {
            if ((mode !== "universe" && S.r[c.sys] !== region) || !on(c.sys)) return;
            ctx.beginPath();
            ctx.arc(sx, sy, r + 9, 0, Math.PI * 2);
            ctx.stroke();
          });
        }
        return;
      }
      if (layer === "sov") {
        var land = sovView === "territory";
        if (land) drawTerritory(k, ox, oy);
        batch(colors.unheld, r * 0.8, function (i) { return LY.sovA[i] < 0 && (!LY.sovF[i] || (land && G.tier[i] > 0)); });
        batch(colors.npc, r, function (i) { return LY.sovA[i] < 0 && LY.sovF[i] > 0 && !(land && G.tier[i] > 0); });
        for (var a = 0; a < LY.alliances.length; a++) {
          var mem = LY.allianceMembers[a];
          if (!mem.length) continue;
          ctx.beginPath();
          for (m = 0; m < mem.length; m++) {
            i = mem[m];
            if ((mode !== "universe" && S.r[i] !== region) || !on(i)) continue;
            ctx.moveTo(sx + r * 1.15, sy);
            ctx.arc(sx, sy, r * 1.15, 0, Math.PI * 2);
          }
          ctx.fillStyle = allianceColor(a);
          ctx.fill();
        }
        return;
      }
      if (layer === "fw") {
        batch(colors.dim, r * 0.8, function (i) { return !LY.fwState[i]; });
        Object.keys(colors.faction).forEach(function (f) {
          batch(colors.faction[f], r * 1.2, function (i) { return LY.fwState[i] && LY.fwOcc[i] === +f; });
        });
        // Contest progress: an arc around each contested system.
        ctx.lineWidth = Math.max(1.5, r * 0.5);
        for (m = 0; m < n; m++) {
          i = list ? list[m] : m;
          if (!LY.fwState[i] || LY.fwPct[i] <= 0 || !on(i)) continue;
          ctx.beginPath();
          ctx.arc(sx, sy, r * 1.2 + 3, -Math.PI / 2, -Math.PI / 2 + Math.PI * 2 * LY.fwPct[i] / 100);
          ctx.strokeStyle = LY.fwState[i] === 3 ? colors.accent : colors.label;
          ctx.stroke();
        }
        return;
      }
      if (layer === "inc") {
        batch(colors.dim, r * 0.8, function (i) { return !LY.inc[i]; });
        batch(colors.inc, r * 1.4, function (i) { return LY.inc[i] === 1; });
        batch(colors.incStage, r * 1.8, function (i) { return LY.inc[i] === 2; });
        ctx.lineWidth = 2;
        ctx.strokeStyle = colors.incStage;
        for (m = 0; m < n; m++) {
          i = list ? list[m] : m;
          if (LY.inc[i] !== 2 || !on(i)) continue;
          ctx.beginPath();
          ctx.arc(sx, sy, r * 1.8 + 5, 0, Math.PI * 2);
          ctx.stroke();
        }
      }
    }

    // --- Sovereignty as territory ---
    // Each held nullsec system casts a soft influence (a Gaussian a
    // little wider than the space between systems) for its holder:
    // an alliance, or the NPC faction of NPC null. Every cell of a
    // coarse grid over the view goes to the strongest holder there,
    // filled faintly, and cells beside another holder's (or empty
    // space) are the border, drawn strong. The grid is built once
    // per view and layers payload, and drawn scaled and smoothed.
    function sovField() {
      var key = mode === "universe" ? "u" : "r" + region;
      if (sovFields[key]) return sovFields[key];
      var list = mode === "universe" ? null : G.members[region];
      var n = list ? list.length : G.n, A = LY.alliances.length;
      var owners = [], i, m, npcIdx = {};
      for (m = 0; m < n; m++) {
        i = list ? list[m] : m;
        if (LY.sovA[i] >= 0) owners.push([i, LY.sovA[i]]);
        else if (LY.sovF[i] > 0 && G.tier[i] === 0) {
          if (npcIdx[LY.sovF[i]] === undefined) npcIdx[LY.sovF[i]] = A + Object.keys(npcIdx).length;
          owners.push([i, npcIdx[LY.sovF[i]]]);
        }
      }
      var b = viewBounds(), bw = b[2] - b[0], bh = b[3] - b[1];
      var gw = mode === "universe" ? 640 : 320, cell = bw / gw, gh = Math.ceil(bh / cell);
      var sigma = 0.85 * Math.sqrt(bw * bh / Math.max(n, 1)), reach = Math.ceil(2.4 * sigma / cell);
      var best = new Float32Array(gw * gh), who = new Int16Array(gw * gh).fill(-1);
      // One holder at a time: sum its influence over its own box,
      // then keep the cells where it beats the strongest so far.
      var byOwner = {};
      owners.forEach(function (o) { (byOwner[o[1]] = byOwner[o[1]] || []).push(o[0]); });
      var acc = new Float32Array(gw * gh), s2 = 2 * sigma * sigma;
      Object.keys(byOwner).forEach(function (ok) {
        var sys = byOwner[ok], x0 = gw, y0 = gh, x1 = 0, y1 = 0, q, x, y;
        sys.forEach(function (s) {
          var cx = Math.floor((wx(s) - b[0]) / cell), cy = Math.floor((wy(s) - b[1]) / cell);
          var ax = Math.max(0, cx - reach), bx = Math.min(gw - 1, cx + reach);
          var ay = Math.max(0, cy - reach), by = Math.min(gh - 1, cy + reach);
          if (ax < x0) x0 = ax; if (ay < y0) y0 = ay; if (bx > x1) x1 = bx; if (by > y1) y1 = by;
          for (y = ay; y <= by; y++) {
            var dy = b[1] + (y + 0.5) * cell - wy(s);
            for (x = ax; x <= bx; x++) {
              var dx = b[0] + (x + 0.5) * cell - wx(s);
              acc[y * gw + x] += Math.exp(-(dx * dx + dy * dy) / s2);
            }
          }
        });
        for (y = y0; y <= y1; y++) for (x = x0; x <= x1; x++) {
          q = y * gw + x;
          if (acc[q] > best[q]) { best[q] = acc[q]; who[q] = +ok; }
          acc[q] = 0;
        }
      });
      var rgb = [];
      function ownerRGB(o) {
        if (!rgb[o]) rgb[o] = rgbOf(o < A ? allianceColor(o) : colors.npc);
        return rgb[o];
      }
      var off = root.createElement("canvas");
      off.width = gw; off.height = gh;
      var oc = off.getContext("2d"), img = oc.createImageData(gw, gh), px = img.data;
      var floor = 0.3;
      for (var y2 = 0; y2 < gh; y2++) for (var x2 = 0; x2 < gw; x2++) {
        var p = y2 * gw + x2, o = best[p] >= floor ? who[p] : -1;
        if (o < 0) continue;
        var edge = false;
        [[1, 0], [-1, 0], [0, 1], [0, -1]].forEach(function (d) {
          var nx = x2 + d[0], ny = y2 + d[1];
          if (nx < 0 || ny < 0 || nx >= gw || ny >= gh) return;
          var nq = ny * gw + nx;
          if ((best[nq] >= floor ? who[nq] : -1) !== o) edge = true;
        });
        var c = ownerRGB(o);
        px[p * 4] = c[0]; px[p * 4 + 1] = c[1]; px[p * 4 + 2] = c[2];
        px[p * 4 + 3] = edge ? 235 : Math.round(255 * Math.min(0.34, 0.14 + 0.08 * best[p]));
      }
      oc.putImageData(img, 0, 0);
      sovFields[key] = { canvas: off, x: b[0], y: b[1], w: gw * cell, h: gh * cell };
      return sovFields[key];
    }
    function drawTerritory(k, ox, oy) {
      var f = sovField();
      ctx.imageSmoothingEnabled = true;
      ctx.imageSmoothingQuality = "high";
      ctx.drawImage(f.canvas, f.x * k + ox, f.y * k + oy, f.w * k, f.h * k);
    }

    // The live layers' own labels, placed before system names:
    // alliance names over the space they hold, incursion staging
    // systems by name.
    function layerLabels(k, ox, oy, label, inPlaceFirst) {
      if (!LY) return;
      if (layer === "sov") {
        var holders = [];
        for (var a = 0; a < LY.alliances.length; a++) {
          var mem = LY.allianceMembers[a], cx = 0, cy = 0, c = 0;
          for (var m = 0; m < mem.length; m++) {
            var i = mem[m];
            if (mode !== "universe" && S.r[i] !== region) continue;
            cx += wx(i); cy += wy(i); c++;
          }
          if (c >= (mode === "universe" ? 4 : 2)) holders.push({ a: a, x: cx / c, y: cy / c, c: c });
        }
        holders.sort(function (p, q) { return q.c - p.c || p.a - q.a; });
        inPlaceFirst(holders, function (h) { return "A" + h.a; }, function (h) {
          var name = LY.alliances[h.a].name || "Alliance " + LY.alliances[h.a].id;
          label(name, h.x * k + ox, h.y * k + oy - 6, mode === "universe" ? 11 : 12, allianceColor(h.a), "700", false, "A" + h.a);
        });
      } else if (layer === "threat") {
        // A camp is named -- who, and how many kills -- once the
        // view is close enough to read it; New Eden shows the rings.
        if (mode === "universe" && cam.k / fitK() < 2.5) return;
        inPlaceFirst(LY.camps, function (c) { return "C" + c.sys; }, function (c) {
          if (mode !== "universe" && S.r[c.sys] !== region) return;
          label("Camp · " + c.group + " · " + c.n + " kills", wx(c.sys) * k + ox, wy(c.sys) * k + oy + radius() + 12,
            12, colors.camp, "700", false, "C" + c.sys);
        });
      } else if (layer === "inc") {
        LY.incs.forEach(function (x) {
          var s = G.byID[x.staging];
          if (s === undefined || (mode !== "universe" && S.r[s] !== region)) return;
          label("Staging · " + S.n[s], wx(s) * k + ox, wy(s) * k + oy + radius() * 1.8 + 8, 12, colors.incStage, "700", true, "I" + s);
        });
      }
    }

    function setLayer(name, quiet) {
      if (name !== "security" && !LY) name = "security";
      if (!el.querySelector('[data-map-layer="' + name + '"]')) name = "security";
      layer = name;
      showTip(-1);
      el.querySelectorAll("[data-map-layer]").forEach(function (b) {
        var on = b.getAttribute("data-map-layer") === name;
        b.setAttribute("aria-checked", on ? "true" : "false");
        // On a phone the picker scrolls sideways: keep the chosen
        // chip in view without moving the page.
        if (on && b.parentNode.scrollWidth > b.parentNode.clientWidth) {
          var row = b.parentNode;
          row.scrollLeft = Math.max(0, b.offsetLeft - (row.clientWidth - b.offsetWidth) / 2);
        }
      });
      el.classList.toggle("map-live", name !== "security");
      try { localStorage.setItem("evesynapse-map-layer", name); } catch (e) { /* private mode */ }
      renderLegend();
      updateCard();
      if (!quiet) syncURL();
      schedule();
    }

    function eveTime(unix) {
      if (!unix) return "";
      var d = new Date(unix * 1000);
      return ("0" + d.getUTCHours()).slice(-2) + ":" + ("0" + d.getUTCMinutes()).slice(-2) + " EVE";
    }
    var LAYER_TITLES = { kills: "Ship and pod kills, last hour", npc: "NPC kills, last hour", jumps: "Jumps, last hour",
      sov: "Sovereignty", fw: "Faction Warfare", inc: "Incursions", threat: "Threat now" };

    // The legend for the layer showing: the security swatches for
    // Security, otherwise a scale or key built here.
    function renderLegend() {
      var secLegend = el.querySelector(".map-legend"), box = el.querySelector(".map-layer-legend");
      secLegend.hidden = layer !== "security";
      box.hidden = layer === "security";
      if (layer === "security" || !LY) return;
      box.textContent = "";
      var h = root.createElement("p");
      h.className = "map-layer-title";
      h.textContent = LAYER_TITLES[layer];
      box.appendChild(h);
      function row(cls, color, text) {
        var p = root.createElement("p");
        p.className = "map-layer-row";
        var sw = root.createElement("span");
        sw.className = "map-layer-sw " + cls;
        sw.style.background = color;
        p.appendChild(sw);
        p.appendChild(root.createTextNode(text));
        box.appendChild(p);
      }
      if (layer === "threat") {
        // The levels, each in its color on the threat ramp (as the
        // scale has it).
        THREAT_LEVELS.forEach(function (lv, li) {
          if (!li) return;
          row("", heatColor(heatT(lv.at, heatMax("threat")), 1, colors.threatRamp), lv.name);
        });
        row("", colors.camp, LY.camps.length ? LY.camps.length + (LY.camps.length === 1 ? " gate camp" : " gate camps") : "No gate camps");
      } else if (HEAT[layer]) {
        var bar = root.createElement("p");
        bar.className = "map-heat";
        var g = root.createElement("span");
        g.className = "map-heat-bar";
        g.style.background = "linear-gradient(to right," + heatColor(0.05, 1) + "," + heatColor(0.75, 1) + "," + heatColor(1, 1) + ")";
        bar.appendChild(root.createTextNode("1 "));
        bar.appendChild(g);
        var top = root.createElement("span");
        top.className = "map-heat-top";
        top.textContent = " " + Math.round(heatMax(layer)).toLocaleString() + (heatMax(layer) < LY.max[layer] ? "+" : "");
        bar.appendChild(top);
        box.appendChild(bar);
      } else if (layer === "sov") {
        var view = root.createElement("div");
        view.className = "map-layer-ctl map-layer-seg";
        view.setAttribute("role", "radiogroup");
        view.setAttribute("aria-label", "Show sovereignty as");
        [["systems", "Systems"], ["territory", "Territory"]].forEach(function (v) {
          var b = root.createElement("button");
          b.type = "button";
          b.setAttribute("role", "radio");
          b.setAttribute("aria-checked", sovView === v[0] ? "true" : "false");
          b.setAttribute("data-map-sov", v[0]);
          b.textContent = v[1];
          view.appendChild(b);
        });
        box.appendChild(view);
        row("", "hsl(30,68%," + colors.sovL + ")", "Alliances");
        row("", colors.npc, "NPC factions");
        if (sovView === "systems") row("", colors.unheld, "Unclaimed");
      } else if (layer === "fw") {
        [[500003, "Amarr"], [500001, "Caldari"], [500004, "Gallente"], [500002, "Minmatar"]].forEach(function (f) {
          row("", colors.faction[f[0]], (LY.factions[f[0]] || f[1]).replace(/ (Empire|State|Federation|Republic)$/, ""));
        });
        var arc = root.createElement("p");
        arc.className = "map-layer-row muted";
        arc.textContent = "Arc: contest progress";
        box.appendChild(arc);
      } else if (layer === "inc") {
        row("", colors.inc, "Infested");
        row("", colors.incStage, "Staging");
        if (!LY.incs.length) row("", colors.dim, "No incursions right now");
      }
      if (HEAT[layer]) {
        // The scale: a slider, ends labelled by what they do.
        var sc = root.createElement("label");
        sc.className = "map-layer-ctl map-scale";
        var lo = root.createElement("span");
        lo.textContent = "Hottest";
        var rng = root.createElement("input");
        rng.type = "range";
        rng.min = "-3"; rng.max = "5"; rng.step = "0.5";
        rng.value = String(+heatGain[layer] || 0);
        rng.setAttribute("data-map-gain", "");
        rng.setAttribute("aria-label", "Layer scale");
        var hi = root.createElement("span");
        hi.textContent = "All activity";
        sc.appendChild(lo);
        sc.appendChild(rng);
        sc.appendChild(hi);
        box.appendChild(sc);
      }
      var at = root.createElement("p");
      at.className = "map-layer-at muted";
      at.textContent = LY.at[layer] ? "As of " + eveTime(LY.at[layer]) : "No data yet";
      box.appendChild(at);
    }

    // --- The hover popover ---
    // The system under the mouse: its name, security and region,
    // then only what the layer showing says about it. Placed beside
    // the cursor and flipped at the stage's edges.
    var tipEl = el.querySelector(".map-tip"), tipFor = -2;

    // layerRows: what one layer says about system i, as [label,
    // value] rows. The popover shows the rows of the layer showing;
    // the card shows every layer's, briefly (rows that only say
    // "nothing here" are left out), so the two always agree. The two
    // kill counts come from different places and say so: the game's
    // own hourly count (the Kills layer, up to an hour behind) and
    // our killfeed (the Threat layer, live).
    function layerRows(i, name, brief) {
      var rows = [];
      if (!LY || (!LY.at[name] && name !== "sov")) return rows;
      switch (name) {
        case "threat": {
          if (LY.threat[i] > 0 || LY.threatHour[i]) {
            rows.push(["Threat", THREAT_LEVELS[threatLevel(LY.threat[i])].name]);
            rows.push(["Killfeed, last hour", LY.threatHour[i] + (LY.threatHour[i] === 1 ? " kill" : " kills")]);
            if (LY.threatLast[i]) rows.push(["Last kill", agoShort(LY.threatLast[i])]);
          } else if (LY.spill[i] >= THREAT_LEVELS[1].at) {
            rows.push(["Threat", "Quiet · danger next door"]);
          } else if (!brief) {
            rows.push(["Threat", "Quiet · no PvP kills lately"]);
          }
          if (LY.campOf[i] >= 0) {
            var c = LY.camps[LY.campOf[i]];
            rows.push(["Gate camp", (c.group ? c.group + " · " : "") + c.n + " kills since " + agoShort(c.since)]);
          }
          break;
        }
        case "kills":
          if (brief && !LY.ship[i] && !LY.pod[i]) break;
          rows.push(["Game count, last hour", LY.ship[i] + (LY.ship[i] === 1 ? " ship" : " ships") + " · " + LY.pod[i] + (LY.pod[i] === 1 ? " pod" : " pods")]);
          break;
        case "npc":
          if (brief && !LY.npc[i]) break;
          rows.push(["NPC kills, last hour", LY.npc[i].toLocaleString()]);
          break;
        case "jumps":
          if (brief && !LY.jumps[i]) break;
          rows.push(["Jumps, last hour", LY.jumps[i].toLocaleString()]);
          break;
        case "sov":
          if (LY.sovA[i] >= 0) {
            var al = LY.alliances[LY.sovA[i]];
            rows.push(["Sovereignty", al.name || "Alliance " + al.id]);
          } else if (LY.sovF[i] > 0) {
            rows.push(["Sovereignty", LY.factions[LY.sovF[i]] || "NPC faction"]);
          } else if (!brief) {
            rows.push(["Sovereignty", "Unclaimed"]);
          }
          break;
        case "fw":
          if (LY.fwState[i]) {
            rows.push(["Faction Warfare", (LY.factions[LY.fwOcc[i]] || "Occupied") + " · " +
              ["", "Uncontested", "Contested", "Vulnerable", "Captured"][LY.fwState[i]] + (LY.fwPct[i] ? " " + LY.fwPct[i] + "%" : "")]);
          } else if (!brief) {
            rows.push(["Faction Warfare", "Not a warzone system"]);
          }
          break;
        case "inc":
          if (LY.incOf[i] >= 0) {
            var x = LY.incs[LY.incOf[i]];
            rows.push(["Incursion", (LY.inc[i] === 2 ? "Staging · " : "Infested · ") + x.state + " · " + Math.round(x.inf * 100) + "% influence" + (x.boss ? " · boss" : "")]);
          } else if (!brief) {
            rows.push(["Incursion", "None here"]);
          }
          break;
      }
      return rows;
    }
    function tipRows(i) { return intelRows(i, 2).concat(layer === "security" ? [] : layerRows(i, layer, false)); }
    function liveRows(i) {
      var rows = [];
      rows = intelRows(i, 3);
      ["threat", "kills", "npc", "jumps", "sov", "fw", "inc"].forEach(function (name) { rows = rows.concat(layerRows(i, name, true)); });
      return rows;
    }

    function showTip(i, px, py) {
      if (!tipEl) return;
      if (i < 0 || (mode !== "universe" && S.r[i] !== region)) {
        tipEl.hidden = true;
        tipFor = -2;
        return;
      }
      if (i !== tipFor) {
        tipFor = i;
        tipEl.textContent = "";
        var head = root.createElement("p");
        head.className = "map-tip-head";
        var sec = root.createElement("span");
        sec.className = "sec sec-" + G.tier[i];
        sec.textContent = secText(S.s[i]);
        head.appendChild(sec);
        var nm = root.createElement("strong");
        nm.textContent = " " + S.n[i];
        head.appendChild(nm);
        var rg = root.createElement("span");
        rg.className = "map-tip-region";
        rg.textContent = " " + P.regions[S.r[i]].name;
        head.appendChild(rg);
        tipEl.appendChild(head);
        tipRows(i).forEach(function (r) {
          var p = root.createElement("p");
          p.className = "map-tip-row";
          var k = root.createElement("span");
          k.textContent = r[0];
          var v = root.createElement("span");
          v.textContent = r[1];
          p.appendChild(k);
          p.appendChild(v);
          tipEl.appendChild(p);
        });
        tipEl.hidden = false;
      }
      // Beside the cursor, flipped to stay inside the stage.
      var w = tipEl.offsetWidth, h = tipEl.offsetHeight, gap = 14;
      var x = px + gap, y = py + gap;
      if (x + w > cw - 6) x = px - gap - w;
      if (y + h > ch - 6) y = py - gap - h;
      tipEl.style.transform = "translate(" + Math.max(6, x) + "px," + Math.max(6, y) + "px)";
    }

    function agoShort(unix) {
      var m = Math.max(0, Math.round((Date.now() / 1000 - unix) / 60));
      return m < 1 ? "just now" : m < 60 ? m + " min ago" : Math.round(m / 60) + " h ago";
    }

    // The card's live figures for one system, every layer at once.
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
      showTip(-1);
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
      // Not before the first draw has read the URL (restore): the
      // layers can arrive first, and would wipe what it asked for.
      if (!fitted || !window.history || !history.replaceState) return;
      var q = [];
      if (mode !== "universe") q.push("region=" + P.regions[region].id);
      if (sel >= 0) q.push("system=" + S.id[sel]);
      var shownLayer = LY ? layer : wantLayer || layer;
      if (shownLayer && shownLayer !== "security") q.push("layer=" + encodeURIComponent(shownLayer));
      if (shownLayer === "sov" && sovView === "territory") q.push("sov=territory");
      if (routeFrom >= 0) q.push("from=" + S.id[routeFrom]);
      if (routeTo >= 0) q.push("to=" + S.id[routeTo]);
      if ((routeFrom >= 0 || routeTo >= 0) && routeMode !== "short") q.push("route=" + routeMode);
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
      var live = card.querySelector(".map-card-live"), rows = liveRows(i);
      live.textContent = "";
      live.hidden = !rows.length;
      rows.forEach(function (rw) {
        var d = root.createElement("div"), dt = root.createElement("dt"), dd = root.createElement("dd");
        dt.textContent = rw[0];
        dd.textContent = rw[1];
        d.appendChild(dt);
        d.appendChild(dd);
        live.appendChild(d);
      });
      card.querySelector('[data-route-set="from"]').hidden = i === routeFrom;
      card.querySelector('[data-route-set="to"]').hidden = i === routeTo;
      card.querySelector("[data-map-scan]").href = "/intel/scan/?system=" + S.id[i];
      cardScans(i);
      cardReport(i);
    }

    // The reader's own scans of the card's system: fetched once per
    // system while the page lives (a new scan comes back through a
    // page load anyway), shown only when there are some.
    var scansFor = {};
    function cardScans(i) {
      var box = card.querySelector(".map-card-scans");
      function show(list) {
        if (sel !== i) return;
        var ul = box.querySelector("ul");
        ul.textContent = "";
        box.hidden = !list.length;
        list.slice(0, 5).forEach(function (s) {
          var li = root.createElement("li"), a = root.createElement("a");
          a.href = "/intel/scan/" + encodeURIComponent(s.id) + "/";
          a.textContent = s.title + " · " + agoShort(s.at);
          li.appendChild(a);
          ul.appendChild(li);
        });
      }
      var id = S.id[i];
      if (scansFor[id]) { show(scansFor[id]); return; }
      box.hidden = true;
      fetch("/map/scans.json?system=" + id, { credentials: "same-origin" }).then(function (r) {
        return r.ok ? r.json() : [];
      }).then(function (list) {
        scansFor[id] = Array.isArray(list) ? list : [];
        if (el.isConnected) show(scansFor[id]);
      }).catch(function () { /* the card stands without them */ });
    }

    // --- Routes ---
    // A route is two ends and a preference; the path is replanned
    // whenever one changes, and again when the threat layer
    // refreshes. Picking a destination with no start set starts
    // from where the reader's character last was, when we know.
    var routeEl = el.querySelector(".map-route");
    var routeFrom = -1, routeTo = -1, routeMode = "short", routePath = null, routeSum = null;
    var here = G.byID[+el.getAttribute("data-route-here")];
    if (here === undefined) here = -1;
    var canSend = el.hasAttribute("data-route-can-send"), signedIn = el.hasAttribute("data-route-signed-in");
    var sending = false;

    function setRouteEnd(which, i) {
      if (i < 0) return;
      if (which === "to") {
        routeTo = i;
        if (routeFrom < 0 && here >= 0) routeFrom = here;
        if (routeFrom === i) routeFrom = -1;
      } else {
        routeFrom = i;
        if (routeTo === i) routeTo = -1;
      }
      routeNote("");
      replan();
    }
    function clearRoute() {
      routeFrom = routeTo = -1;
      routePath = routeSum = null;
      routeNote("");
      renderRoute();
      updateCard();
      syncURL();
      schedule();
    }
    function replan() {
      routePath = routeFrom >= 0 && routeTo >= 0 ? planRoute(G, routeFrom, routeTo, routeCost(G, LY, routeMode, IN && intelOn ? IN.prices : null)) : null;
      routeSum = routePath ? routeSummary(G, LY, routePath, IN && intelOn ? IN.prices : null) : null;
      renderRoute();
      updateCard();
      syncURL();
      schedule();
    }
    function routeNote(text) { routeEl.querySelector(".map-route-note").textContent = text; }

    function secBadge(i) {
      var s = root.createElement("span");
      s.className = "sec sec-" + G.tier[i];
      s.textContent = secText(S.s[i]);
      return s;
    }
    function sysButton(i) {
      var b = root.createElement("button");
      b.type = "button";
      b.className = "map-route-sys";
      b.setAttribute("data-map-sys", String(i));
      b.appendChild(secBadge(i));
      b.appendChild(root.createTextNode(" " + S.n[i]));
      return b;
    }
    function renderRoute() {
      routeEl.hidden = routeFrom < 0 && routeTo < 0;
      el.classList.toggle("map-routing", !routeEl.hidden);
      if (routeEl.hidden) return;
      ["from", "to"].forEach(function (w) {
        var at = w === "from" ? routeFrom : routeTo, box = routeEl.querySelector('[data-route-end="' + w + '"]');
        box.textContent = "";
        if (at >= 0) box.appendChild(sysButton(at));
        else {
          var m = root.createElement("span");
          m.className = "muted";
          m.textContent = w === "from" ? "Pick a start" : "Pick a destination";
          box.appendChild(m);
        }
      });
      routeEl.querySelectorAll("[data-route-mode]").forEach(function (b) {
        b.setAttribute("aria-checked", b.getAttribute("data-route-mode") === routeMode ? "true" : "false");
      });
      var sum = routeEl.querySelector(".map-route-sum"), warn = routeEl.querySelector(".map-route-warn");
      var list = routeEl.querySelector(".map-route-list");
      var strip = routeEl.querySelector(".map-route-strip");
      warn.textContent = "";
      list.textContent = "";
      sum.textContent = "";
      strip.textContent = "";
      strip.hidden = true;
      routeEl.querySelector(".map-route-steps").hidden = !routePath;
      if (routeFrom < 0 || routeTo < 0) {
        sum.textContent = "Select a system, then “Route " + (routeFrom < 0 ? "from" : "to") + " here”.";
      } else if (!routePath) {
        sum.textContent = "No stargates join these systems.";
      } else {
        var s = routeSum;
        var j = root.createElement("strong");
        j.textContent = s.jumps === 1 ? "1 jump" : s.jumps + " jumps";
        sum.appendChild(j);
        [[s.high, "high", 10], [s.low, "low", 3], [s.nul, "null", 0]].forEach(function (b) {
          if (!b[0]) return;
          var sp = root.createElement("span");
          sp.className = "map-route-band sec sec-" + b[2];
          sp.textContent = b[0] + " " + b[1];
          sum.appendChild(sp);
        });
        // The route at a glance: one segment a system, in its
        // security color, camps and hot systems marked.
        strip.hidden = false;
        routePath.forEach(function (i, at) {
          if (!at) return;
          var seg = root.createElement("span");
          seg.className = "sec sec-" + G.tier[i] + (LY && LY.campOf[i] >= 0 ? " camp" : LY && threatLevel(LY.threat[i]) >= 2 ? " hot" : "");
          seg.title = S.n[i] + " " + secText(S.s[i]);
          strip.appendChild(seg);
        });
        s.camps.forEach(function (i) {
          var c = LY.camps[LY.campOf[i]], li = root.createElement("li");
          li.className = "map-route-camp";
          li.appendChild(root.createTextNode("Gate camp in "));
          li.appendChild(sysButton(i));
          li.appendChild(root.createTextNode(" · " + c.n + " kills" + (c.group ? " by " + c.group : "")));
          warn.appendChild(li);
        });
        s.reported.slice(0, 4).forEach(function (i) {
          var last = IN.bySys[i][0], li = root.createElement("li");
          li.className = "map-route-intel";
          li.appendChild(root.createTextNode((last.n ? last.n + " hostiles" : "Hostiles") + " reported in "));
          li.appendChild(sysButton(i));
          li.appendChild(root.createTextNode(" · " + agoShort(last.at - intelSkew)));
          warn.appendChild(li);
        });
        s.hot.slice(0, 3).forEach(function (i) {
          if (LY.campOf[i] >= 0) return;
          var li = root.createElement("li");
          li.appendChild(root.createTextNode(THREAT_LEVELS[threatLevel(LY.threat[i])].name + " threat in "));
          li.appendChild(sysButton(i));
          warn.appendChild(li);
        });
        routePath.forEach(function (i) {
          var li = root.createElement("li");
          li.appendChild(sysButton(i));
          if (LY && LY.campOf[i] >= 0) li.className = "camp";
          else if (LY && threatLevel(LY.threat[i]) >= 2) li.className = "hot";
          list.appendChild(li);
        });
      }
      var acts = routeEl.querySelector(".map-route-actions");
      acts.hidden = !signedIn;
      acts.querySelectorAll("[data-route-send]").forEach(function (b) {
        b.disabled = sending || !canSend || (b.getAttribute("data-route-send") === "route" ? !routePath || routeSum.jumps < 1 : routeTo < 0);
      });
      var note = routeEl.querySelector(".map-route-note");
      if (!note.textContent) {
        note.textContent = !signedIn ? "Sign in to send routes to your game client."
          : !canSend ? "To send routes to your game client, sign in again and allow EveSynapse to set autopilot waypoints." : "";
      }
    }
    function sendRoute(kind) {
      if (sending || routeTo < 0) return;
      var systems = kind === "dest" ? [S.id[routeTo]] : (routePath || []).slice(1).map(function (i) { return S.id[i]; });
      if (!systems.length) return;
      sending = true;
      routeNote(kind === "dest" ? "Setting your destination…" : "Sending " + systems.length + " waypoints…");
      renderRoute();
      fetch("/map/route", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/json", "X-Requested-With": "XMLHttpRequest" },
        body: JSON.stringify({ mode: kind, systems: systems })
      }).then(function (r) {
        return r.json().catch(function () { return { ok: false, message: "The server did not answer — try again." }; });
      }).then(function (res) {
        routeNote(res.message || (res.ok ? "Sent." : "That did not go through."));
      }).catch(function () {
        routeNote("The server could not be reached — try again.");
      }).then(function () {
        sending = false;
        if (el.isConnected) renderRoute();
      });
    }

    // drawRoute: the path as a lit line, in screen space, under the
    // systems; in a tactical view the legs that leave the region run
    // out along their stubs.
    function drawRoute(k, ox, oy) {
      if (!routePath || routePath.length < 2) return;
      ctx.beginPath();
      for (var m = 1; m < routePath.length; m++) {
        var a = routePath[m - 1], b = routePath[m], pa, pb;
        if (mode === "universe") { pa = [S.x[a], S.y[a]]; pb = [S.x[b], S.y[b]]; }
        else {
          var ina = S.r[a] === region, inb = S.r[b] === region;
          if (!ina && !inb) continue;
          pa = ina ? [S.rx[a], S.ry[a]] : ghost(b, a);
          pb = inb ? [S.rx[b], S.ry[b]] : ghost(a, b);
        }
        ctx.moveTo(pa[0] * k + ox, pa[1] * k + oy);
        ctx.lineTo(pb[0] * k + ox, pb[1] * k + oy);
      }
      ctx.lineCap = "round";
      ctx.lineJoin = "round";
      ctx.lineWidth = mode === "universe" ? 5 : 7;
      ctx.strokeStyle = colors.routeHalo;
      ctx.stroke();
      ctx.lineWidth = mode === "universe" ? 2.2 : 3;
      ctx.strokeStyle = colors.route;
      ctx.stroke();
    }
    function drawRouteEnds(k, ox, oy, r) {
      [routeFrom, routeTo].forEach(function (s, idx) {
        if (s < 0 || (mode !== "universe" && S.r[s] !== region)) return;
        var x = wx(s) * k + ox, y = wy(s) * k + oy;
        ctx.beginPath();
        ctx.arc(x, y, r + (idx ? 6 : 5), 0, Math.PI * 2);
        ctx.lineWidth = idx ? 2.5 : 1.5;
        ctx.strokeStyle = colors.route;
        ctx.stroke();
      });
    }

    // --- Intel channels ---
    // A signed-in reader's channels feed an overlay drawn over any
    // layer: every system with a report in the last 30 minutes wears
    // a cold ring (green for a clear), fading as the report ages. The
    // overlay polls every 20 seconds while the page is in view; the
    // card lists a system's latest reports and, where the reader may
    // post, reports straight from the map. Routes price fresh
    // hostiles (joinIntel).
    var IN = null, intelOn = true, intelTimer = 0, intelSkew = 0, intelKey = "";
    var intelBtn = el.querySelector("[data-map-intel]");
    var reportForm = card.querySelector("[data-map-report]");
    try { intelOn = localStorage.getItem("evesynapse-map-intel") !== "0"; } catch (e) { /* private mode */ }
    function intelNow() { return Date.now() / 1000 + intelSkew; }
    function fetchIntel(always) {
      if (!signedIn || (!always && document.visibilityState === "hidden")) return;
      fetch("/map/intel.json", { credentials: "same-origin" }).then(function (r) {
        return r.ok ? r.json() : null;
      }).then(function (D) {
        if (!D || !el.isConnected) return;
        if (D.now) intelSkew = D.now - Date.now() / 1000;
        IN = joinIntel(G, D, intelNow());
        intelSync();
      }).catch(function () { /* the map stands without intel */ });
    }
    function intelSync() {
      if (intelBtn) {
        intelBtn.hidden = !IN || !IN.channels.length;
        intelBtn.setAttribute("aria-pressed", intelOn ? "true" : "false");
        var n = intelBtn.querySelector("[data-map-intel-n]");
        if (n) n.textContent = IN && IN.hostile ? String(IN.hostile) : "";
      }
      if (routeFrom >= 0 && routeTo >= 0) replan();
      else { updateCard(); schedule(); }
    }
    function intelList(i) {
      if (!IN || !intelOn || !IN.bySys[i]) return [];
      var now = intelNow();
      return IN.bySys[i].filter(function (r) { return intelFresh(r.at, now) > 0; });
    }
    function intelRows(i, max) {
      return intelList(i).slice(0, max).map(function (r) {
        var what = r.clear ? "Clear" : r.n ? r.n + (r.n === 1 ? " hostile" : " hostiles") : "Hostiles";
        return [IN.names[r.ch] || "Intel", what + (r.text ? " · " + r.text : "") + " · " + agoShort(r.at - intelSkew)];
      });
    }
    function intelRGBA(c, a) { return "rgba(" + c[0] + "," + c[1] + "," + c[2] + "," + a.toFixed(3) + ")"; }
    function drawIntel(k, ox, oy, r) {
      if (!IN || !intelOn) return;
      var now = intelNow();
      IN.systems.forEach(function (i) {
        if (mode !== "universe" && S.r[i] !== region) return;
        var last = IN.bySys[i][0], f = intelFresh(last.at, now);
        if (f <= 0) return;
        var x = wx(i) * k + ox, y = wy(i) * k + oy;
        if (x < -40 || y < -40 || x > cw + 40 || y > ch + 40) return;
        var c = last.clear ? colors.intelClear : colors.intel;
        var rr = r + 6 + (last.clear ? 0 : Math.min(8, Math.log(1 + (last.n || 1)) * 3));
        ctx.beginPath();
        ctx.arc(x, y, rr, 0, Math.PI * 2);
        ctx.lineWidth = 2.2;
        ctx.strokeStyle = intelRGBA(c, 0.35 + 0.65 * f);
        ctx.stroke();
        if (!last.clear) {
          ctx.beginPath();
          ctx.arc(x, y, rr + 4, 0, Math.PI * 2);
          ctx.lineWidth = 1;
          ctx.strokeStyle = intelRGBA(c, 0.3 * f);
          ctx.stroke();
        }
      });
    }
    // A report's count and age beside its system, once the view is
    // close enough to read it.
    function intelLabels(k, ox, oy, label, inPlaceFirst) {
      if (!IN || !intelOn || (mode === "universe" && cam.k / fitK() < 2)) return;
      var now = intelNow(), r = radius();
      inPlaceFirst(IN.systems, function (i) { return "N" + i; }, function (i) {
        if (mode !== "universe" && S.r[i] !== region) return;
        var last = IN.bySys[i][0], f = intelFresh(last.at, now);
        if (f <= 0) return;
        var m = Math.max(1, Math.round((now - last.at) / 60)) + "m";
        var text = last.clear ? "clear · " + m : (last.n ? last.n + " · " : "") + m;
        label(text, wx(i) * k + ox, wy(i) * k + oy - r - 24, 11, intelRGBA(last.clear ? colors.intelClear : colors.intel, 1), "700", false, "N" + i);
      });
    }
    // The card's quick report: the channels the reader may post to,
    // the last one used chosen.
    function cardReport(i) {
      if (!reportForm) return;
      var posts = IN ? IN.channels.filter(function (c) { return c.post; }) : [];
      reportForm.hidden = !posts.length;
      if (!posts.length) return;
      var key = posts.map(function (c) { return c.id + ":" + c.name; }).join("|");
      var select = reportForm.querySelector("select");
      if (key !== intelKey) {
        intelKey = key;
        var want = "";
        try { want = localStorage.getItem("evesynapse-map-report-channel") || ""; } catch (e) { /* private mode */ }
        select.textContent = "";
        posts.forEach(function (c) {
          var o = root.createElement("option");
          o.value = String(c.id);
          o.textContent = c.name;
          if (String(c.id) === want) o.selected = true;
          select.appendChild(o);
        });
      }
      select.closest(".map-card-intel-ch").hidden = posts.length < 2;
      reportForm.setAttribute("data-sys", String(S.id[i]));
    }
    function onReport(e) {
      e.preventDefault();
      var f = reportForm, kind = e.submitter && e.submitter.value === "clear" ? "clear" : "hostile";
      var chID = f.querySelector("select").value, msg = f.querySelector(".map-card-intel-msg");
      var body = new URLSearchParams();
      body.set("system_id", f.getAttribute("data-sys"));
      body.set("text", f.elements.text.value);
      body.set("hostiles", f.elements.hostiles.value);
      if (kind === "clear") body.set("clear", "1");
      try { localStorage.setItem("evesynapse-map-report-channel", chID); } catch (err) { /* private mode */ }
      msg.textContent = "Reporting…";
      fetch("/intel/channels/" + encodeURIComponent(chID) + "/report", {
        method: "POST",
        credentials: "same-origin",
        headers: { "Content-Type": "application/x-www-form-urlencoded", "X-Requested-With": "XMLHttpRequest" },
        body: body.toString()
      }).then(function (r) { return r.json(); }).then(function (res) {
        msg.textContent = res.message || "";
        if (res.ok) { f.elements.text.value = ""; f.elements.hostiles.value = ""; fetchIntel(); }
      }).catch(function () { msg.textContent = "That did not go through — try again."; });
    }
    if (reportForm) reportForm.addEventListener("submit", onReport);

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
      showTip(-1);
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
          showTip(h, p[0], p[1]);
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
      if (t && (t.tagName === "INPUT" || t.tagName === "TEXTAREA" || t.tagName === "SELECT")) return;
      switch (e.key) {
        case "ArrowLeft": case "a": pan(-90, 0); break;
        case "ArrowRight": case "d": pan(90, 0); break;
        case "ArrowUp": case "w": pan(0, -90); break;
        case "ArrowDown": case "s": pan(0, 90); break;
        case "+": case "=": case "e": glideZoom(cw / 2, ch / 2, 1.5); break;
        case "-": case "_": case "q": glideZoom(cw / 2, ch / 2, 1 / 1.5); break;
        case "0": case "f": flyFit(); break;
        case "F": toggleFull(); break;
        case "/": findInput.focus(); break;
        case "?": toggleKeys(); break;
        case "l": case "L": {
          // Cycle through the layers the payload unlocked.
          var opts = [].filter.call(el.querySelectorAll("[data-map-layer]"), function (b) { return !b.disabled; })
            .map(function (b) { return b.getAttribute("data-map-layer"); });
          var at = opts.indexOf(layer);
          setLayer(opts[(at + (e.key === "L" ? opts.length - 1 : 1)) % opts.length]);
          break;
        }
        case "Enter":
          if (sel >= 0 && mode === "universe") enterRegion(S.r[sel], sel);
          else return;
          break;
        case "r":
          if (sel >= 0) setRouteEnd("to", sel); else return;
          break;
        case "R":
          if (sel >= 0) setRouteEnd("from", sel); else return;
          break;
        case "Escape":
          if (!keysPanel.hidden) toggleKeys(false);
          else if (routeFrom >= 0 || routeTo >= 0) clearRoute();
          else if (sel >= 0) select(-1);
          else if (mode !== "universe" || !el.classList.contains("map-full")) leaveRegion();
          else toggleFull(false);
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

    // --- Full screen ---
    // The map frame takes the whole screen: through the Fullscreen
    // API where the browser has it for an element, and as a fixed
    // frame over the page where it does not (iPhone Safari). Either
    // way the frame wears .map-full, and the canvas follows its size.
    var fullBtn = el.querySelector("[data-map-full]"), fullViaAPI = false;
    function fullscreenElement() { return root.fullscreenElement || root.webkitFullscreenElement || null; }
    function setFullClass(on) {
      el.classList.toggle("map-full", on);
      root.documentElement.classList.toggle("map-full-open", on);
      if (fullBtn) {
        fullBtn.setAttribute("aria-pressed", on ? "true" : "false");
        fullBtn.setAttribute("aria-label", on ? "Leave full screen" : "Full screen");
        fullBtn.title = on ? "Leave full screen (Shift F)" : "Full screen (Shift F)";
      }
    }
    function toggleFull(force) {
      var on = force === undefined ? !el.classList.contains("map-full") : force;
      setFullClass(on);
      if (on) {
        var req = el.requestFullscreen || el.webkitRequestFullscreen;
        if (req) {
          try {
            var pr = req.call(el);
            if (pr && pr.catch) pr.catch(function () { /* the fixed frame stands in */ });
          } catch (e) { /* the fixed frame stands in */ }
        }
      } else if (fullscreenElement() === el) {
        (root.exitFullscreen || root.webkitExitFullscreen).call(root);
      }
    }
    function onFullChange() {
      if (fullscreenElement() === el) { fullViaAPI = true; return; }
      if (fullViaAPI) { fullViaAPI = false; setFullClass(false); }
    }

    // --- Screenshot ---
    // The map as it is drawn, at the screen's full resolution, over
    // the stage's own background, with the brand in the bottom-right
    // corner and a caption (layer, place, EVE time) in the bottom-left.
    // Saved as a PNG; on a phone it goes to the share sheet when the
    // browser has one for files.
    var shotBtn = el.querySelector("[data-map-shot]");
    function loadImage(src) {
      return new Promise(function (ok, fail) {
        var img = new Image();
        img.onload = function () { ok(img); };
        img.onerror = fail;
        img.src = src;
      });
    }
    // The wordmark as a self-contained image: its letters' colors come
    // from the stylesheet, so they are written into the copy.
    function wordmarkImage() {
      var svg = el.querySelector(".map-mark .wordmark-svg");
      if (!svg) return Promise.reject(new Error("no wordmark"));
      var copy = svg.cloneNode(true);
      copy.setAttribute("xmlns", "http://www.w3.org/2000/svg");
      var from = svg.querySelectorAll("path"), to = copy.querySelectorAll("path");
      for (var i = 0; i < from.length; i++) to[i].setAttribute("fill", getComputedStyle(from[i]).fill);
      return loadImage("data:image/svg+xml;charset=utf-8," + encodeURIComponent(new XMLSerializer().serializeToString(copy)));
    }
    function shotName() {
      var d = new Date(), p = function (n) { return ("0" + n).slice(-2); };
      var where = mode === "universe" ? "new-eden" : P.regions[region].name.toLowerCase().replace(/[^a-z0-9]+/g, "-");
      return "evesynapse-map-" + where + "-" + d.getUTCFullYear() + p(d.getUTCMonth() + 1) + p(d.getUTCDate()) + "-" + p(d.getUTCHours()) + p(d.getUTCMinutes()) + ".png";
    }
    function takeShot() {
      if (!cw || shotBtn.classList.contains("busy")) return;
      shotBtn.classList.add("busy");
      draw();
      var out = root.createElement("canvas");
      out.width = canvas.width;
      out.height = canvas.height;
      var o = out.getContext("2d"), s = dpr;
      o.fillStyle = getComputedStyle(stage).backgroundColor || "#070504";
      o.fillRect(0, 0, out.width, out.height);
      o.drawImage(canvas, 0, 0);
      var glyph = el.querySelector(".map-mark-glyph");
      var fonts = root.fonts && root.fonts.load ? root.fonts.load("600 12px Rajdhani").catch(function () {}) : Promise.resolve();
      Promise.all([loadImage(glyph.src).catch(function () { return null; }), wordmarkImage().catch(function () { return null; }), fonts]).then(function (got) {
        var logo = got[0], word = got[1], pad = 14 * s, h = 18 * s;
        // The brand: glyph, wordmark, ".APP", right-aligned.
        o.save();
        o.globalAlpha = 0.6;
        o.font = "600 " + Math.round(h * 0.7) + "px Rajdhani, sans-serif";
        o.textBaseline = "alphabetic";
        var tld = ".APP", tw = o.measureText(tld).width;
        var ww = word ? h * (word.naturalWidth / word.naturalHeight || 5.9) : 0;
        var x = out.width - pad - tw, base = out.height - pad - h * 0.18;
        o.fillStyle = colors.label;
        o.fillText(tld, x, base);
        x -= ww + 2 * s;
        if (word) o.drawImage(word, x, out.height - pad - h, ww, h);
        if (logo) o.drawImage(logo, x - h * 1.35 - 6 * s, out.height - pad - h * 1.3, h * 1.35, h * 1.35);
        // The caption: what the map shows, where, when.
        var place = mode === "universe" ? "New Eden" : P.regions[region].name;
        var what = layer === "security" ? "Security" : LAYER_TITLES[layer] || layer;
        var d = new Date(), p2 = function (n) { return ("0" + n).slice(-2); };
        var when = d.getUTCFullYear() + "-" + p2(d.getUTCMonth() + 1) + "-" + p2(d.getUTCDate()) + " " + p2(d.getUTCHours()) + ":" + p2(d.getUTCMinutes()) + " EVE";
        o.globalAlpha = 0.75;
        o.font = "500 " + Math.round(12 * s) + "px " + font;
        o.fillStyle = colors.labelDim;
        o.fillText(place + " · " + what + " · " + when, pad, out.height - pad);
        o.restore();
        return new Promise(function (ok) { out.toBlob(ok, "image/png"); });
      }).then(function (blob) {
        if (!blob) throw new Error("no image");
        var name = shotName();
        var file = typeof File === "function" ? new File([blob], name, { type: "image/png" }) : null;
        var coarse = window.matchMedia && window.matchMedia("(pointer: coarse)").matches;
        if (coarse && file && navigator.canShare && navigator.canShare({ files: [file] })) {
          return navigator.share({ files: [file], title: "EveSynapse map" }).catch(function () { /* dismissed */ });
        }
        var a = root.createElement("a");
        a.href = URL.createObjectURL(blob);
        a.download = name;
        root.body.appendChild(a);
        a.click();
        a.remove();
        setTimeout(function () { URL.revokeObjectURL(a.href); }, 4000);
        hint("Screenshot saved.");
      }).catch(function () {
        hint("The screenshot could not be made.");
      }).then(function () { shotBtn.classList.remove("busy"); });
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
      var rs = t.closest("[data-route-set]");
      if (rs) { if (sel >= 0) setRouteEnd(rs.getAttribute("data-route-set"), sel); return; }
      if (t.closest("[data-route-close]")) { clearRoute(); return; }
      if (t.closest("[data-route-swap]")) {
        var tmp = routeFrom; routeFrom = routeTo; routeTo = tmp;
        routeNote("");
        replan();
        return;
      }
      var rm = t.closest("[data-route-mode]");
      if (rm && ROUTE_MODES[rm.getAttribute("data-route-mode")]) {
        routeMode = rm.getAttribute("data-route-mode");
        routeNote("");
        replan();
        return;
      }
      var rd = t.closest("[data-route-send]");
      if (rd && !rd.disabled) { sendRoute(rd.getAttribute("data-route-send")); return; }
      var sv = t.closest("[data-map-sov]");
      if (sv) {
        sovView = sv.getAttribute("data-map-sov");
        try { localStorage.setItem(SOV_KEY, sovView); } catch (err) { /* private mode */ }
        renderLegend();
        syncURL();
        schedule();
        return;
      }
      if (t.closest("[data-map-full]")) { toggleFull(); return; }
      if (t.closest("[data-map-shot]")) { takeShot(); return; }
      if (t.closest("[data-map-intel]")) {
        intelOn = !intelOn;
        try { localStorage.setItem("evesynapse-map-intel", intelOn ? "1" : "0"); } catch (err) { /* private mode */ }
        intelSync();
        return;
      }
      var lb = t.closest("[data-map-layer]");
      if (lb && !lb.disabled) { setLayer(lb.getAttribute("data-map-layer")); return; }
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
      var qs = startQuery;
      var sys = G.byID[+qs.get("system")];
      var ri = G.regionByID[+qs.get("region")];
      if (sys !== undefined) sel = sys;
      if (ri !== undefined) enterRegion(ri, sys !== undefined ? sys : -1);
      else if (sys !== undefined) enterRegion(S.r[sys], sys);
      // A shared route: its ends and preference.
      var rf = G.byID[+qs.get("from")], rt = G.byID[+qs.get("to")];
      if (ROUTE_MODES[qs.get("route")]) routeMode = qs.get("route");
      if (rf !== undefined) routeFrom = rf;
      if (rt !== undefined && rt !== rf) routeTo = rt;
      if (routeFrom >= 0 || routeTo >= 0) replan();
      updateCard();
    }

    // --- Wiring and teardown ---
    readColors();
    statusEl.hidden = true;
    findInput.disabled = false;
    el.querySelectorAll("[data-map-zoom], [data-map-keys], [data-map-full], [data-map-shot]").forEach(function (b) { b.disabled = false; });
    root.addEventListener("fullscreenchange", onFullChange);
    root.addEventListener("webkitfullscreenchange", onFullChange);
    // The layer scale: the map follows the slider as it moves; the
    // legend redraws (threat's level swatches) once it is let go.
    el.addEventListener("input", function (e) {
      if (!e.target.hasAttribute || !e.target.hasAttribute("data-map-gain") || !LY) return;
      heatGain[layer] = +e.target.value;
      try { localStorage.setItem(GAIN_KEY, JSON.stringify(heatGain)); } catch (err) { /* private mode */ }
      var top = el.querySelector(".map-heat-top");
      if (top) top.textContent = " " + Math.round(heatMax(layer)).toLocaleString() + (heatMax(layer) < LY.max[layer] ? "+" : "");
      schedule();
    });
    el.addEventListener("change", function (e) {
      if (e.target.hasAttribute && e.target.hasAttribute("data-map-gain") && layer === "threat") renderLegend();
    });
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
    canvas.addEventListener("pointerleave", function () { showTip(-1); if (hover >= 0) { hover = -1; schedule(); } });
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
    var mo = new MutationObserver(function () { readColors(); sovFields = {}; renderLegend(); schedule(); });
    mo.observe(root.documentElement, { attributes: true, attributeFilter: ["data-theme", "class"] });

    // --- Live layers: loading, the picker, refresh ---
    // The page names the payload it rendered with (hashed, cached for
    // good); every two minutes while the page is in view the map
    // asks for the current one, and redraws only when its ETag moved.
    var layersEtag = "", layersTimer = 0, firstLayers = true;
    function fetchLayers(url) {
      return fetch(url, { credentials: "same-origin" }).then(function (r) {
        if (!r.ok) throw new Error("HTTP " + r.status);
        var et = r.headers.get("ETag") || "";
        if (et && et === layersEtag) return null;
        layersEtag = et;
        return r.json();
      }).then(function (L) {
        if (!L || !el.isConnected) return;
        LY = joinLayers(P, L);
        heatOrder = {};
        sovFields = {};
        showTip(-1);
        el.querySelectorAll("[data-map-layer]").forEach(function (b) { b.disabled = false; });
        if (firstLayers) {
          firstLayers = false;
          var want = wantLayer;
          if (!want) { try { want = localStorage.getItem("evesynapse-map-layer"); } catch (e) { /* private mode */ } }
          setLayer(want || "security");
          if (routeFrom >= 0 || routeTo >= 0) replan();
        } else if (routeFrom >= 0 || routeTo >= 0) {
          // A calmer (or hotter) picture can bend the route.
          renderLegend();
          replan();
        } else {
          renderLegend();
          updateCard();
          schedule();
        }
      }).catch(function () { /* the map stands without its layers */ });
    }
    if (signedIn) {
      fetchIntel(true);
      intelTimer = setInterval(fetchIntel, 20000);
      root.addEventListener("visibilitychange", onIntelVisible);
    }
    // Back from another tab: catch up at once, not at the next tick.
    function onIntelVisible() { if (document.visibilityState === "visible") fetchIntel(); }
    var layersURL = el.getAttribute("data-layers");
    if (layersURL) {
      fetchLayers(layersURL);
      layersTimer = setInterval(function () {
        if (document.visibilityState === "visible") fetchLayers("/map/layers.json");
      }, 120000);
    }

    function teardown() {
      ro.disconnect();
      mo.disconnect();
      clearInterval(layersTimer);
      clearInterval(intelTimer);
      root.removeEventListener("visibilitychange", onIntelVisible);
      if (raf) cancelAnimationFrame(raf);
      clearTimeout(hintTimer);
      root.removeEventListener("fullscreenchange", onFullChange);
      root.removeEventListener("webkitfullscreenchange", onFullChange);
      if (el.classList.contains("map-full")) toggleFull(false);
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
    module.exports = { secRounded: secRounded, secTier: secTier, secText: secText, prepare: prepare, regionLabelAlpha: regionLabelAlpha, zoomEntryAllowed: zoomEntryAllowed,
      joinLayers: joinLayers, heatT: heatT, allianceHue: allianceHue, threatLevel: threatLevel,
      routeCost: routeCost, planRoute: planRoute, routeSummary: routeSummary, joinIntel: joinIntel };
  }
})();
