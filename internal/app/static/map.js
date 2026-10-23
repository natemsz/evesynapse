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
 * Canvas 2D, drawn on demand: the gate network is a few Path2D
 * batches per view in world coordinates (in New Eden, by line
 * class: in-region jumps solid, region-border jumps dashed, each
 * in its lower end's security color); systems are batched by
 * security color; labels are culled against an occupancy grid so
 * they never pile up.
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
   * @property {{id:number,name:string,x:number,y:number,px:number,py:number,w:number,h:number}[]} regions
   *   x, y: label point in the universe frame; px, py: in the projected
   *   frame; w, h: region frame size.
   * @property {{id:number,name:string,r:number}[]} cons  r: region index.
   * @property {{id:number[],n:string[],x:number[],y:number[],px:number[],py:number[],s:number[],r:number[],c:number[],rx:number[],ry:number[],f:number[],p:number[]}} sys
   *   x, y: universe frame (0..10000); px, py: projected frame;
   *   rx, ry: region frame (0..1000);
   *   s: true security; r: region index; c: constellation index (-1 none);
   *   f: faction ID; p: label prominence (higher names first).
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

  // A gate line wears the security class of its lower-security
  // end -- the end that decides how dangerous the jump is: 0 high
  // (0.45 and up), 1 low (0.05 to 0.45), 2 null (below 0.05). A
  // jump that crosses a region border is drawn dashed; the draw
  // batch index is the class, plus 3 for a border crossing, so
  // the web strokes in at most six batches: three solid, three
  // dashed.
  function gateClass(sa, sb) {
    var m = Math.min(sa, sb);
    return m >= 0.45 ? 0 : m >= 0.05 ? 1 : 2;
  }
  function gateBatch(sa, sb, ra, rb) {
    return gateClass(sa, sb) + (ra !== rb ? 3 : 0);
  }

  // Region names thin out as system names take over, but never
  // vanish: the name stays readable (and clickable) however deep
  // the zoom, so it can always carry the reader into the region.
  function regionLabelAlpha(f) {
    return f > 6 ? Math.max(0.55, 1 - (f - 6) / 14) : 1;
  }
  // System names are rationed by zoom tier, f the multiple of
  // the fit zoom: at 0 the region names are the picture and no
  // system is named; at 1 a few regions fill the view and only
  // the prominent systems in view are named, capped so the tier
  // stays readable however dense the cluster; at 2 a single
  // region or less fills the view and every system in view is
  // named. A pure function of the view's scale: the tier changes
  // with the same redraw that settles the picture, never mid-way
  // through a separate pass.
  var LABEL_MID_CAP = 120;
  function labelTier(f) {
    return f <= 6 ? 0 : f <= 24 ? 1 : 2;
  }
  // midTierLabels picks the middle tier's names: the candidates
  // arrive in label precedence order (best first), so the cap
  // simply keeps the front of the line.
  function midTierLabels(cands, cap) {
    return cands.length > cap ? cands.slice(0, cap) : cands;
  }
  // boxesOverlap is the label collision rule: the box x, y, w, h
  // against a placed box [x, y, w, h]. Edges that merely touch
  // do not collide.
  function boxesOverlap(x, y, w, h, o) {
    return x < o[0] + o[2] && x + w > o[0] && y < o[1] + o[3] && y + h > o[1];
  }
  // Zooming in enters a region only under the preference; every
  // other way in (a name, the search, the index) is unconditional,
  // and zooming back out of a region always leaves it.
  function zoomEntryAllowed(prefOn) { return !!prefOn; }
  // The mousewheel always zooms about the cursor, never pans:
  // wheel up (negative delta) zooms in, wheel down zooms out. A
  // pinch (browsers send it as a wheel with ctrlKey set) zooms
  // faster. Panning stays on drag and the arrow/WASD keys.
  function wheelZoomFactor(dy, pinch) {
    return Math.exp(-dy * (pinch ? 0.012 : 0.0024));
  }
  // Zoomed-out picking: a system with something on it (interest
  // w, 0 to 1) reaches farther than a bare one, and at a similar
  // distance wins; a system right under the pointer (within dot)
  // still beats everything.
  function pickReach(w) { return 14 + 10 * w; }
  function pickScore(d, w, dot) {
    return d <= dot * dot ? d - 1e6 : (d + 4) / ((1 + 3 * w) * (1 + 3 * w));
  }
  // A shareable view: the camera centre (in whichever world the
  // map is showing) and its zoom as a multiple of the fit zoom.
  function formatView(x, y, f) {
    return Math.round(x) + "," + Math.round(y) + "," + Math.round(f * 1000) / 1000;
  }
  function parseView(s) {
    if (!s) return null;
    var p = String(s).split(",");
    if (p.length !== 3) return null;
    var x = +p[0], y = +p[1], f = +p[2];
    if (!isFinite(x) || !isFinite(y) || !isFinite(f) || f <= 0) return null;
    return { x: x, y: y, f: f };
  }
  // --- Sovereignty territory: field and outlines ---
  // territoryField: who holds each cell of a grid over bounds b
  // ([x0, y0, x1, y1], gw cells across). Every source (flat arrays:
  // owner o, position x/y, spread s) casts a Gaussian of its own
  // width; each cell keeps its strongest holder and the runner-up, so
  // borders can be traced where the two are equal.
  function territoryField(src, b, gw) {
    var cell = (b[2] - b[0]) / gw, gh = Math.max(1, Math.ceil((b[3] - b[1]) / cell)), N = gw * gh;
    var best = new Float32Array(N), second = new Float32Array(N);
    var who = new Int32Array(N).fill(-1), who2 = new Int32Array(N).fill(-1);
    var acc = new Float32Array(N), boxes = {};
    // The Gaussian as a table: the inner loop runs tens of millions
    // of times on a big map, and a table read beats Math.exp.
    var lut = new Float32Array(1025);
    for (var li = 0; li <= 1024; li++) lut[li] = Math.exp(-li / 128);
    var byOwner = {};
    for (var n = 0; n < src.o.length; n++) (byOwner[src.o[n]] = byOwner[src.o[n]] || []).push(n);
    Object.keys(byOwner).forEach(function (ok) {
      var o = +ok, list = byOwner[ok], x0 = gw, y0 = gh, x1 = -1, y1 = -1, x, y, q;
      list.forEach(function (n) {
        var sx = src.x[n], sy = src.y[n], s = src.s[n], lutK = 128 / (2 * s * s), reach = Math.ceil(2.4 * s / cell);
        var cx = Math.floor((sx - b[0]) / cell), cy = Math.floor((sy - b[1]) / cell);
        var ax = Math.max(0, cx - reach), bx = Math.min(gw - 1, cx + reach);
        var ay = Math.max(0, cy - reach), by = Math.min(gh - 1, cy + reach);
        if (ax > bx || ay > by) return;
        if (ax < x0) x0 = ax; if (ay < y0) y0 = ay; if (bx > x1) x1 = bx; if (by > y1) y1 = by;
        for (y = ay; y <= by; y++) {
          var dy = b[1] + (y + 0.5) * cell - sy, dy2 = dy * dy, row = y * gw;
          for (x = ax; x <= bx; x++) {
            var dx = b[0] + (x + 0.5) * cell - sx, l = ((dx * dx + dy2) * lutK) | 0;
            acc[row + x] += lut[l > 1024 ? 1024 : l];
          }
        }
      });
      if (x1 < 0) return;
      boxes[o] = [x0, y0, x1, y1];
      for (y = y0; y <= y1; y++) for (x = x0; x <= x1; x++) {
        q = y * gw + x;
        var v = acc[q];
        if (v > best[q]) { second[q] = best[q]; who2[q] = who[q]; best[q] = v; who[q] = o; }
        else if (v > second[q]) { second[q] = v; who2[q] = o; }
        acc[q] = 0;
      }
    });
    return { gw: gw, gh: gh, cell: cell, x0: b[0], y0: b[1], best: best, second: second, who: who, who2: who2, boxes: boxes };
  }
  // territoryOutlines: every holder's land as closed outlines in world
  // units, by holder. Traced (marching squares) where a holder's lead
  // over the runner-up, or its strength over floor, crosses zero, the
  // crossing placed between cell centres by the values, then rounded
  // once (Chaikin): smooth at any zoom, with no grid steps. One pass
  // over the grid serves every holder; a square is looked at only for
  // the holders of its corners, the only ones that can lead there.
  function territoryOutlines(F, floor) {
    var gw = F.gw, gh = F.gh, who = F.who, who2 = F.who2, best = F.best, second = F.second;
    // Edge ids: horizontal (x,y)-(x+1,y) is 2*((y+1)*W+(x+1)),
    // vertical (x,y)-(x,y+1) the same plus one; W spans the grid and
    // the ring of empty cells around it.
    var W = gw + 2, st = {};
    function g(q, o) {
      if (q < 0) return -1;
      var bv = best[q];
      if (who[q] === o) return Math.min(bv - floor, bv - second[q]);
      if (who2[q] === o) return -(bv - second[q]);
      return -Math.max(bv, 0.05);
    }
    function at(x, y) { return x >= 0 && y >= 0 && x < gw && y < gh ? y * gw + x : -1; }
    function edge(s, x, y, vert, ga, gb) {
      var id = 2 * ((y + 1) * W + (x + 1)) + (vert ? 1 : 0);
      if (!s.pts[id]) {
        var t = ga / (ga - gb);
        s.pts[id] = [F.x0 + (x + (vert ? 0 : t) + 0.5) * F.cell, F.y0 + (y + (vert ? t : 0) + 0.5) * F.cell];
      }
      return id;
    }
    function seg(s, a, b) {
      s.segs.push(a, b);
      var n = s.segs.length / 2 - 1;
      (s.ends[a] = s.ends[a] || []).push(n);
      (s.ends[b] = s.ends[b] || []).push(n);
    }
    function trace(o, x, y, qa, qb, qc, qd) {
      var a = g(qa, o), b = g(qb, o), c = g(qc, o), d = g(qd, o);
      var k = (a > 0 ? 8 : 0) | (b > 0 ? 4 : 0) | (c > 0 ? 2 : 0) | (d > 0 ? 1 : 0);
      if (k === 0 || k === 15) return;
      var s = st[o] || (st[o] = { pts: {}, segs: [], ends: {} });
      var mid = a + b + c + d > 0;
      // Top, right, bottom, left edges of the square, made on demand.
      var T = k === 3 || k === 12 || k === 1 || k === 14 || k === 2 || k === 13 ? -1 : edge(s, x, y, false, a, b);
      var R = k === 1 || k === 14 || k === 6 || k === 9 || k === 7 || k === 8 ? -1 : edge(s, x + 1, y, true, b, c);
      var B = k === 4 || k === 11 || k === 3 || k === 12 || k === 7 || k === 8 ? -1 : edge(s, x, y + 1, false, d, c);
      var L = k === 2 || k === 13 || k === 4 || k === 11 || k === 6 || k === 9 ? -1 : edge(s, x, y, true, a, d);
      switch (k) {
        case 1: case 14: seg(s, L, B); break;
        case 2: case 13: seg(s, B, R); break;
        case 3: case 12: seg(s, L, R); break;
        case 4: case 11: seg(s, T, R); break;
        case 6: case 9: seg(s, T, B); break;
        case 7: case 8: seg(s, L, T); break;
        case 5: if (mid) { seg(s, L, T); seg(s, B, R); } else { seg(s, T, R); seg(s, L, B); } break;
        case 10: if (mid) { seg(s, T, R); seg(s, L, B); } else { seg(s, L, T); seg(s, B, R); } break;
      }
    }
    for (var y = -1; y < gh; y++) {
      for (var x = -1; x < gw; x++) {
        var qa = at(x, y), qb = at(x + 1, y), qc = at(x + 1, y + 1), qd = at(x, y + 1);
        var wa = qa < 0 ? -1 : who[qa], wb = qb < 0 ? -1 : who[qb], wc = qc < 0 ? -1 : who[qc], wd = qd < 0 ? -1 : who[qd];
        if (wa < 0 && wb < 0 && wc < 0 && wd < 0) continue;
        if (wa >= 0) trace(wa, x, y, qa, qb, qc, qd);
        if (wb >= 0 && wb !== wa) trace(wb, x, y, qa, qb, qc, qd);
        if (wc >= 0 && wc !== wa && wc !== wb) trace(wc, x, y, qa, qb, qc, qd);
        if (wd >= 0 && wd !== wa && wd !== wb && wd !== wc) trace(wd, x, y, qa, qb, qc, qd);
      }
    }
    // Chain each holder's segments into closed loops, then round them.
    var out = {};
    Object.keys(st).forEach(function (ok) {
      var s = st[ok], segs = s.segs, nseg = segs.length / 2, used = new Uint8Array(nseg), loops = [];
      for (var s0 = 0; s0 < nseg; s0++) {
        if (used[s0]) continue;
        used[s0] = 1;
        var loop = [segs[2 * s0]], cur = segs[2 * s0 + 1], guard = nseg + 2;
        while (cur !== loop[0] && guard-- > 0) {
          loop.push(cur);
          var nx = -1, cand = s.ends[cur];
          for (var ci = 0; ci < cand.length; ci++) if (!used[cand[ci]]) { nx = cand[ci]; break; }
          if (nx < 0) break;
          used[nx] = 1;
          cur = segs[2 * nx] === cur ? segs[2 * nx + 1] : segs[2 * nx];
        }
        if (loop.length < 3) continue;
        var sm = [];
        for (var i = 0; i < loop.length; i++) {
          var p = s.pts[loop[i]], q = s.pts[loop[(i + 1) % loop.length]];
          sm.push([0.75 * p[0] + 0.25 * q[0], 0.75 * p[1] + 0.25 * q[1]], [0.25 * p[0] + 0.75 * q[0], 0.25 * p[1] + 0.75 * q[1]]);
        }
        loops.push(sm);
      }
      if (loops.length) out[ok] = loops;
    });
    return out;
  }
  // territoryLoops: one holder's outlines (territoryOutlines).
  function territoryLoops(F, o, floor) { return territoryOutlines(F, floor)[o] || []; }
  // viewFits: whether a shared view's centre lies within the bounds
  // being shown ([x0, y0, x1, y1]), a quarter of their size to spare.
  function viewFits(v, b) {
    var mx = (b[2] - b[0]) * 0.25, my = (b[3] - b[1]) * 0.25;
    return v.x >= b[0] - mx && v.x <= b[2] + mx && v.y >= b[1] - my && v.y <= b[3] + my;
  }
  // The zoom that fits a route's span (dx by dy world units) into
  // the free part of the canvas (fw by fh px), pad px to spare.
  function routeFitScale(dx, dy, fw, fh, pad) {
    return Math.min((fw - 2 * pad) / Math.max(dx, 1), (fh - 2 * pad) / Math.max(dy, 1));
  }

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
    // The projected frame, for payloads that carry one; older
    // payloads fall back to the true frame throughout.
    var px = S.px && S.px.length === n ? S.px : S.x;
    var py = S.py && S.py.length === n ? S.py : S.y;
    var R = P.regions.length;
    var members = [], bounds = [], pbounds = [], exits = [], regionByID = {};
    for (j = 0; j < R; j++) {
      members.push([]); exits.push([]);
      bounds.push([Infinity, Infinity, -Infinity, -Infinity]);
      pbounds.push([Infinity, Infinity, -Infinity, -Infinity]);
      regionByID[P.regions[j].id] = j;
    }
    for (i = 0; i < n; i++) {
      var r = S.r[i], bb = bounds[r], pb = pbounds[r];
      members[r].push(i);
      if (S.x[i] < bb[0]) bb[0] = S.x[i];
      if (S.y[i] < bb[1]) bb[1] = S.y[i];
      if (S.x[i] > bb[2]) bb[2] = S.x[i];
      if (S.y[i] > bb[3]) bb[3] = S.y[i];
      if (px[i] < pb[0]) pb[0] = px[i];
      if (py[i] < pb[1]) pb[1] = py[i];
      if (px[i] > pb[2]) pb[2] = px[i];
      if (py[i] > pb[3]) pb[3] = py[i];
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
    var ux = 0, uy = 0, pux = 0, puy = 0;
    for (i = 0; i < n; i++) {
      if (S.x[i] > ux) ux = S.x[i];
      if (S.y[i] > uy) uy = S.y[i];
      if (px[i] > pux) pux = px[i];
      if (py[i] > puy) puy = py[i];
    }
    var regionLower = P.regions.map(function (rg) { return rg.name.toLowerCase(); });
    // Label precedence: the payload's prominence first (hubs,
    // station systems, the best-connected), then by ID, so the
    // same names win wherever the map is panned. A payload older
    // than the prominence array falls back to gate degree.
    var prom = S.p && S.p.length === n ? S.p : null;
    function byPrio(a, b) { return (prom ? prom[b] - prom[a] : adj[b].length - adj[a].length) || S.id[a] - S.id[b]; }
    var prio = [];
    for (i = 0; i < n; i++) prio.push(i);
    prio.sort(byPrio);
    var membersPrio = members.map(function (m) { return m.slice().sort(byPrio); });
    P._g = {
      prio: prio, membersPrio: membersPrio,
      n: n, adj: adj, tier: tier, lower: lower, byID: byID, regionByID: regionByID,
      members: members, bounds: bounds, pbounds: pbounds, exits: exits, consU: cu, consR: cr,
      uniW: ux, uniH: uy, projW: pux, projH: puy, px: px, py: py, regionLower: regionLower
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
  // Hues for the holders actually on the map, assigned to stand
  // apart: largest first, each a golden-angle step (137.5 deg)
  // from the last, so neighbours never land on near-identical
  // hues the way hashed IDs can. Past a full turn of the wheel,
  // a lightness offset keeps repeats apart too. counts is each
  // alliance's held-system count; the result is indexed the
  // same way, [hue, lightnessOffset] per alliance.
  function sovPalette(counts) {
    var order = counts.map(function (c, i) { return i; });
    order.sort(function (a, b) { return counts[b] - counts[a] || a - b; });
    var out = [];
    order.forEach(function (a, idx) {
      out[a] = [Math.round(idx * 137.508) % 360, idx < 6 ? 0 : ((idx % 3) - 1) * 7];
    });
    return out;
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
  // Intel reports never price a route: a report is not danger (a
  // neutral, a friendly, a clear), so a route only names the reported
  // systems it crosses (routeSummary) and the pilot decides.
  function routeCost(G, LY, mode) {
    return function (j) {
      var c = 1;
      if (mode === "short") return c;
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
  // next, when given, lists a system's neighbours in place of its
  // gates (jump legs, gates plus jump bridges); cost is asked of each
  // step as cost(to, from).
  function planRoute(G, a, b, cost, next) {
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
      var nb = next ? next(u) : G.adj[u];
      for (var k = 0; k < nb.length; k++) {
        var v = nb[k], d = dist[u] + cost(v, u);
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

  // --- Jump drives: the pure part ---
  // /map/jump.json carries every known-space system's true position in
  // light years and every jump-capable ship's base range (map_jump.go).
  // A jump lands on a cyno: never in high-sec (security 0.5 and up),
  // Pochven, or Zarzakh; Jump Drive Calibration adds 20% range a level.
  var POCHVEN = 10000070, ZARZAKH = 30100000;
  function jumpRange(base, jdc) { return base * (1 + 0.2 * Math.max(0, Math.min(5, jdc | 0))); }
  function joinJump(G, J) {
    var n = G.n, x = new Float64Array(n).fill(NaN), y = new Float64Array(n), z = new Float64Array(n);
    var dest = new Uint8Array(n);
    for (var k = 0; k < J.id.length; k++) {
      var i = G.byID[J.id[k]];
      if (i === undefined) continue;
      x[i] = J.x[k]; y[i] = J.y[k]; z[i] = J.z[k];
      dest[i] = G.tier[i] < 5 && J.region[k] !== POCHVEN && J.id[k] !== ZARZAKH ? 1 : 0;
    }
    return { x: x, y: y, z: z, dest: dest, ships: J.ships || [], grids: {} };
  }
  function lyBetween(JI, a, b) {
    var dx = JI.x[a] - JI.x[b], dy = JI.y[a] - JI.y[b], dz = JI.z[a] - JI.z[b];
    return Math.sqrt(dx * dx + dy * dy + dz * dz);
  }
  // jumpNeighbours lists where a jump of range R can land from i:
  // systems that take a cyno within R light years, found through a
  // grid of R-sized cells (built once per range).
  function jumpNeighbours(JI, i, R) {
    var key = R.toFixed(3), g = JI.grids[key];
    if (!g) {
      g = { cells: {}, near: {} };
      for (var s = 0; s < JI.x.length; s++) {
        if (!JI.dest[s] || isNaN(JI.x[s])) continue;
        var c = Math.floor(JI.x[s] / R) + "," + Math.floor(JI.y[s] / R) + "," + Math.floor(JI.z[s] / R);
        (g.cells[c] = g.cells[c] || []).push(s);
      }
      JI.grids[key] = g;
    }
    if (g.near[i]) return g.near[i];
    var out = [];
    if (!isNaN(JI.x[i])) {
      var cx = Math.floor(JI.x[i] / R), cy = Math.floor(JI.y[i] / R), cz = Math.floor(JI.z[i] / R);
      for (var a = -1; a <= 1; a++) for (var b = -1; b <= 1; b++) for (var d = -1; d <= 1; d++) {
        var list = g.cells[(cx + a) + "," + (cy + b) + "," + (cz + d)];
        if (!list) continue;
        for (var m = 0; m < list.length; m++) {
          var t = list[m];
          if (t !== i && lyBetween(JI, i, t) <= R) out.push(t);
        }
      }
    }
    g.near[i] = out;
    return out;
  }
  // jumpFatigue estimates fatigue and waits along legs (light years),
  // as CCP has it: each jump's distance counts times the ship's
  // fatigue multiplier; the reactivation wait is (1 + that) minutes or
  // a tenth of the fatigue held, whichever is more, at most 30; the
  // fatigue becomes 10 x (1 + that) minutes, or what was held times
  // (1 + that) when that is more, at most 5 hours; it wears off in
  // real time, so a wait between jumps takes off as much.
  function jumpFatigue(legs, mult, start) {
    var f = start || 0, waits = 0, last = 0;
    legs.forEach(function (ly, at) {
      var eff = ly * mult;
      if (at > 0) { f = Math.max(0, f - last); waits += last; }
      last = Math.min(30, Math.max(1 + eff, f / 10));
      f = Math.min(300, Math.max(10 * (1 + eff), f * (1 + eff)));
    });
    return { fatigue: f, waits: waits };
  }

  // --- Intel channels: the pure part ---
  // joinIntel indexes /map/intel.json by system: each system's reports
  // newest first, its freshness (1 just now, 0 at 30 minutes), and a
  // weight for its fresh hostile reports, by which a route names the
  // reported systems it crosses (never steering around them). A clear
  // newer than a system's last hostile report clears it; the weight
  // fades with age and is more for a bigger count.
  var INTEL_LIVE = 1800, INTEL_PRICE = 200;
  function intelFresh(at, now) { return Math.max(0, 1 - (now - at) / INTEL_LIVE); }
  function joinIntel(G, D, now) {
    var I = { channels: D.channels || [], bySys: {}, systems: [], prices: new Float32Array(G.n), hostile: 0, clear: 0 };
    I.names = {};
    I.channels.forEach(function (c) { I.names[c.id] = c.name; });
    (D.reports || []).forEach(function (r) {
      var i = G.byID[r.sys];
      if (i === undefined || intelFresh(r.at, now) <= 0) return;
      // Chatter and questions are not reports of anything; only
      // friendlies named is calm, like a clear.
      if (r.kind === "chatter" || r.kind === "question") return;
      if (r.kind === "friendly") r.calm = true;
      if (!I.bySys[i]) { I.bySys[i] = []; I.systems.push(i); }
      I.bySys[i].push(r);
    });
    I.systems.forEach(function (i) {
      var list = I.bySys[i].sort(function (a, b) { return b.at - a.at || b.id - a.id; });
      var last = list[0];
      if (last.clear || last.calm) { I.clear++; return; }
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
    var colors = null, font = "sans-serif", fontDisplay = "\"Rajdhani\", \"Segoe UI\", sans-serif";
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
      // The universe web's line colors: the dot palette's own
      // color for each security class (the 1.0 blue, the 0.4
      // amber, the 0.0 magenta), muted -- structure, not a
      // rainbow. Border-crossing jumps stroke stronger than
      // in-region ones, as the old inter-region color did.
      function lineColor(tier, alpha) {
        var rgb = rgbOf(sec[tier]);
        return "rgba(" + rgb[0] + "," + rgb[1] + "," + rgb[2] + "," + alpha + ")";
      }
      colors = {
        sec: sec,
        gate: tok("--map-gate", "rgba(255,180,120,0.22)"),
        gateInter: tok("--map-gate-inter", "rgba(255,120,60,0.4)"),
        gateLine: [lineColor(10, 0.4), lineColor(4, 0.4), lineColor(0, 0.4)],
        gateLineX: [lineColor(10, 0.6), lineColor(4, 0.6), lineColor(0, 0.6)],
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
        data: {
          pilots: rgbOf(tok("--map-d-pilots", "#6ff29a")), fleet: rgbOf(tok("--map-d-fleet", "#6aa8ff")),
          clones: rgbOf(tok("--map-d-clones", "#c4a1ff")), market: rgbOf(tok("--map-d-market", "#ffd24a")),
          industry: rgbOf(tok("--map-d-industry", "#ffa45c")), structures: rgbOf(tok("--map-d-structures", "#5ce0d8")),
          posted: rgbOf(tok("--map-d-posted", "#e6dccb"))
        },
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
      fontDisplay = tok("--map-font-display", fontDisplay);
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
    // wx/wy are the map's one coordinate choice: the region
    // frame in a region, and in the universe the projected
    // frame when Improved Projection is on, the true frame
    // otherwise. Everything that draws, picks or measures in
    // the universe goes through them.
    function wx(i) { return mode === "universe" ? (projOn ? G.px[i] : S.x[i]) : S.rx[i]; }
    function wy(i) { return mode === "universe" ? (projOn ? G.py[i] : S.y[i]) : S.ry[i]; }
    // A region's bounds in the universe frame in use.
    function uBounds(r) { return projOn ? G.pbounds[r] : G.bounds[r]; }

    function viewBounds() {
      if (mode === "universe") return [0, 0, projOn ? G.projW : G.uniW, projOn ? G.projH : G.uniH];
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
      var more = false, moving = !!(anim || wheelZ);
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
      // The glide has settled: the address now says where it ended,
      // not where it began.
      if (moving && !anim && !wheelZ) syncURL();
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
      var key = mode === "universe" ? (projOn ? "up" : "u") : "r" + region;
      if (pathCache[key]) return pathCache[key];
      var intra = new Path2D(), inter = new Path2D(), stubs = new Path2D();
      var web = null;
      var i, a, b;
      if (mode === "universe") {
        // The web, batched by line class (gateBatch): solid
        // in-region jumps, then the dashed border crossings,
        // each in its lower end's security color.
        web = [new Path2D(), new Path2D(), new Path2D(), new Path2D(), new Path2D(), new Path2D()];
        for (i = 0; i < P.gates.length; i += 2) {
          a = P.gates[i]; b = P.gates[i + 1];
          var p = web[gateBatch(S.s[a], S.s[b], S.r[a], S.r[b])];
          p.moveTo(wx(a), wy(a));
          p.lineTo(wx(b), wy(b));
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
      pathCache[key] = { intra: intra, inter: inter, stubs: stubs, web: web };
      return pathCache[key];
    }

    function visible() { return mode === "universe" ? null : G.members[region]; }

    // --- Drawing ---
    function schedule() { if (!raf) raf = requestAnimationFrame(draw); }

    function textWidth(s, size) {
      var key = ctx.font + "|" + size + "|" + s;
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
      // In the projection at the widest tier the gate web is
      // texture behind the region names, not the picture: drawn
      // at reduced alpha until the reader zooms into it.
      if (mode === "universe" && projOn && labelTier(cam.k / fitK()) === 0) ctx.globalAlpha = 0.45;
      if (pp.web) {
        // Solid batches first, then the border crossings in the
        // same class colors, dashed at a constant screen size.
        for (i = 0; i < 3; i++) {
          ctx.strokeStyle = colors.gateLine[i];
          ctx.stroke(pp.web[i]);
        }
        ctx.setLineDash([5 / k, 4 / k]);
        for (i = 0; i < 3; i++) {
          ctx.strokeStyle = colors.gateLineX[i];
          ctx.stroke(pp.web[i + 3]);
        }
        ctx.setLineDash([]);
      } else {
        ctx.strokeStyle = colors.gate;
        ctx.stroke(pp.intra);
        ctx.strokeStyle = colors.gateInter;
        ctx.stroke(pp.inter);
      }
      ctx.globalAlpha = 1;
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
      if (dataHeat()) {
        drawDataHeat(k, ox, oy, r, list, n, pad);
      } else if (layer === "security" || !LY) {
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

      // Your data, then hover and selection rings over it.
      drawData(k, ox, oy, r);
      [hover, sel].forEach(function (s, idx) {
        if (s < 0 || (mode !== "universe" && S.r[s] !== region)) return;
        var hx = wx(s) * k + ox, hy = wy(s) * k + oy;
        ctx.beginPath();
        ctx.arc(hx, hy, r + (idx ? 4 : 3), 0, Math.PI * 2);
        ctx.lineWidth = idx ? 2 : 1.25;
        ctx.strokeStyle = idx ? colors.accent : colors.label;
        ctx.stroke();
      });
      drawJumpRange(k, ox, oy, r);
      drawIntel(k, ox, oy, r);
      drawRouteEnds(k, ox, oy, r);

      drawLabels(k, ox, oy, r);
    }

    // Labels never overlap: each claims cells of a coarse grid, and
    // one that would land on a claimed cell is skipped. The grid is
    // anchored to the map, not the screen, and labels claim in a
    // fixed order (most prominent systems first), so panning moves
    // labels and cells together and the same names win every frame.
    // Zooming moves overlaps every frame, so every name showing last
    // frame claims before any new one, whatever kind of name it is:
    // labels are gathered first and placed in three passes (always
    // shown, showing last frame, new), in their priority order within
    // each. A name holds its place until one already showing truly
    // covers it, so a new alliance name cannot push out a region name
    // that is showing (and lose it back the next frame): names stop
    // blinking in and out as the scale changes.
    var shownLabels = {};
    function drawLabels(k, ox, oy, r) {
      var nextShown = {}, queue = [];
      // Items in priority order; the passes do the rest.
      function inPlaceFirst(items, key, fn) {
        for (var i = 0; i < items.length; i++) fn(items[i]);
      }
      // Placed names as exact boxes, bucketed by screen area so a
      // check looks only at its neighbours. Exact geometry (not grid
      // cells, whose edges a gliding name keeps crossing) means a pair
      // of names moving apart or together changes once, not back and
      // forth.
      var BUCKET = 96, buckets = {};
      function claim(x, y, w, h) {
        if (x < -12 || y < -12 || x + w > cw + 12 || y + h > ch + 12) return false;
        var bx0 = Math.floor(x / BUCKET), by0 = Math.floor(y / BUCKET), bx1 = Math.floor((x + w) / BUCKET), by1 = Math.floor((y + h) / BUCKET);
        var a, b, list, n, o;
        for (b = by0; b <= by1; b++) for (a = bx0; a <= bx1; a++) {
          list = buckets[a + "," + b];
          if (!list) continue;
          for (n = 0; n < list.length; n++) {
            o = list[n];
            if (boxesOverlap(x, y, w, h, o)) return false;
          }
        }
        var box = [x, y, w, h];
        for (b = by0; b <= by1; b++) for (a = bx0; a <= bx1; a++) (buckets[a + "," + b] = buckets[a + "," + b] || []).push(box);
        return true;
      }
      // label queues a name; placed, when given, is told its box.
      function label(text, x, y, size, color, weight, force, key, face, placed) {
        queue.push({ text: text, x: x, y: y, size: size, color: color, weight: weight, force: force, key: key, face: face,
          placed: placed, alpha: ctx.globalAlpha, pass: force ? 0 : key && shownLabels[key] ? 1 : 2 });
      }
      function place(q) {
        ctx.font = (q.weight || "500") + " " + q.size + "px " + (q.face || font);
        var w = textWidth(q.text, q.size);
        var lx = q.x - w / 2, ly = q.y;
        // Geometric hysteresis: a name already showing claims a box
        // a little inside its text, a newcomer one a little outside,
        // so two names at the edge of touching don't trade places.
        var pad = q.pass === 1 ? -2 : 4;
        if (!claim(lx - pad, ly - pad, w + 2 * pad, q.size + 2 * pad) && !q.force) return;
        if (q.key) nextShown[q.key] = 1;
        ctx.globalAlpha = q.alpha;
        ctx.lineWidth = 3;
        ctx.strokeStyle = colors.halo;
        ctx.strokeText(q.text, lx, ly + q.size * 0.8);
        ctx.fillStyle = q.color;
        ctx.fillText(q.text, lx, ly + q.size * 0.8);
        if (q.placed) q.placed([lx, ly, w, q.size]);
      }
      function placeAll() {
        for (var pass = 0; pass < 3; pass++) {
          for (var n = 0; n < queue.length; n++) if (queue[n].pass === pass) place(queue[n]);
        }
        ctx.globalAlpha = 1;
        queue = [];
        shownLabels = nextShown;
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
        // In Territory mode the holders are the subject: their
        // names claim their cells first, at display size, and the
        // region names stay visible behind them -- smaller,
        // thinner, still there to click. Everywhere else region
        // names claim first, as before.
        var terrSov = layer === "sov" && sovView === "territory" && LY;
        if (terrSov) layerLabels(k, ox, oy, label, inPlaceFirst);
        ctx.globalAlpha = regionLabelAlpha(f) * (terrSov ? 0.5 : 1);
        var size = Math.min(12 + f * 0.8, 18) - (terrSov ? 3 : 0), regs = [];
        for (i = 0; i < P.regions.length; i++) if (G.members[i].length) regs.push(i);
        inPlaceFirst(regs, function (i) { return "R" + i; }, function (i) {
          var rg = P.regions[i];
          label(rg.name.toUpperCase(), (projOn && rg.px !== undefined ? rg.px : rg.x) * k + ox, (projOn && rg.py !== undefined ? rg.py : rg.y) * k + oy - size / 2, size, colors.region, "600", false, "R" + i, fontDisplay,
            function (box) { labelBoxes.push({ r: i, b: box }); });
        });
        ctx.globalAlpha = 1;
        if (!terrSov) layerLabels(k, ox, oy, label, inPlaceFirst);
        intelLabels(k, ox, oy, label, inPlaceFirst);
        dataLabels(k, ox, oy, label, inPlaceFirst);
        // System names by zoom tier (labelTier): none while the
        // region names are the picture, the prominent few in view
        // once it opens up, every system in view once a single
        // region fills it. Candidates queue in precedence order,
        // so the collision placement below keeps the best names.
        var ltier = labelTier(f);
        if (ltier > 0) {
          var vis = [];
          for (var o = 0; o < G.n; o++) {
            i = G.prio[o];
            var sx = wx(i) * k + ox, sy = wy(i) * k + oy;
            if (sx >= 0 && sy >= 0 && sx <= cw && sy <= ch) vis.push(i);
          }
          if (ltier === 1) vis = midTierLabels(vis, LABEL_MID_CAP);
          inPlaceFirst(vis, function (i) { return "s" + i; }, function (i) {
            label(S.n[i], wx(i) * k + ox, wy(i) * k + oy + r + 3, 11, colors.labelDim, null, false, "s" + i);
          });
        }
        placeAll();
        return;
      }

      // Tactical: the layer's own labels, every system, the exits.
      layerLabels(k, ox, oy, label, inPlaceFirst);
      intelLabels(k, ox, oy, label, inPlaceFirst);
      dataLabels(k, ox, oy, label, inPlaceFirst);
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
      placeAll();
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
      var top = DATA_HEAT[name] ? (MINE ? MINE.max[name] : 1) : LY.max[name];
      return Math.max(top * Math.pow(2, -g), name === "threat" ? 0.25 : 1);
    }
    // heatTop is the value the legend's scale ends at, as said.
    function heatTop(name) {
      var m = heatMax(name), full = DATA_HEAT[name] ? (MINE ? MINE.max[name] : 1) : LY.max[name];
      return (DATA_HEAT[name] && DATA_HEAT[name].isk ? iskShort(m) : Math.round(m).toLocaleString()) + (m < full ? "+" : "");
    }
    // Sovereignty can show as dots (each system its holder's color)
    // or as territory: the old influence-map look, each holder's
    // space filled and edged where it meets a neighbour's.
    var SOV_KEY = "evesynapse-map-sov", sovView = "systems", sovFields = {};
    try { if (localStorage.getItem(SOV_KEY) === "territory") sovView = "territory"; } catch (e) { /* private mode */ }
    if (startQuery.get("sov") === "territory") sovView = "territory";
    // NPC faction territory (the grey empires) can be hidden so
    // only alliance borders show; on unless turned off.
    var sovNPC = true;
    try { sovNPC = localStorage.getItem("evesynapse-map-sov-npc") !== "0"; } catch (e) { /* private mode */ }
    if (startQuery.get("npc") === "0") sovNPC = false;
    if (startQuery.get("npc") === "1") sovNPC = true;
    // Improved Projection: the universe in the game's own
    // authored 2D layout, the default view. A remembered
    // preference, on every visit until the reader changes it
    // (only an explicit off is remembered as off); a link's
    // proj= settles it for that visit only, without touching
    // the preference.
    var PROJ_KEY = "evesynapse-map-projection", projOn = true;
    try { projOn = window.localStorage.getItem(PROJ_KEY) !== "0"; } catch (e) { /* private mode */ }
    if (startQuery.get("proj") === "1") projOn = true;
    if (startQuery.get("proj") === "0") projOn = false;
    var projPref = el.querySelector("[data-map-proj]");

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
    var sovPal = null, sovPalLY = null;
    function allianceColor(ai) {
      var al = LY.alliances[ai];
      if (al && LY.allianceMembers) {
        if (sovPalLY !== LY) {
          sovPalLY = LY;
          sovPal = sovPalette(LY.allianceMembers.map(function (mem) { return mem.length; }));
        }
        var p = sovPal[ai];
        if (p) {
          var baseL = parseFloat(colors.sovL) || 62;
          var li = Math.max(32, Math.min(78, Math.round(baseL + p[1])));
          return "hsl(" + p[0] + ",68%," + li + "%)";
        }
      }
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
    // Each held nullsec system casts a soft influence for its holder
    // (an alliance, or the NPC faction of NPC null), wider where
    // systems sit far apart, so sparse space at the rim fills in. A
    // gate joining two systems of one holder carries influence along
    // it, and a long one a broad band, so a holding joined across
    // empty space (Period Basis and Paragon Soul) reads as one shape.
    // The field is built once per view and NPC setting at a modest
    // grid (territoryField), and each holder's border traced from it
    // as smooth outlines (territoryLoops). Every frame then only fills
    // and strokes those outlines: crisp at any zoom, nothing rebuilt
    // while zooming, and the other NPC setting built while idle so
    // the toggle is instant.
    var sovGW = 720, sovSettleTimer = 0, sovIdleTimer = 0;
    try {
      if ((navigator.hardwareConcurrency || 8) <= 2 || (navigator.deviceMemory || 8) <= 2) sovGW = 512;
    } catch (e) { /* no hardware hints */ }
    function sovMoving() { return !!(anim || wheelZ || gesture); }
    function sovKey(npc) { return (mode === "universe" ? "u" : "r" + region) + (npc ? ":n1" : ":n0"); }
    // Build the view's field once the map has settled, then redraw.
    function sovSettle() {
      clearTimeout(sovSettleTimer);
      sovSettleTimer = setTimeout(function () {
        sovSettleTimer = 0;
        if (!el.isConnected || !LY || layer !== "sov" || sovView !== "territory") return;
        if (sovMoving()) { sovSettle(); return; }
        sovBuild(sovNPC);
        schedule();
      }, 180);
    }
    // The other NPC setting, built when the map is idle.
    function sovPrebuild() {
      clearTimeout(sovIdleTimer);
      sovIdleTimer = setTimeout(function () {
        if (!el.isConnected || !LY || layer !== "sov" || sovView !== "territory") return;
        if (sovMoving()) { sovPrebuild(); return; }
        sovBuild(!sovNPC);
      }, 600);
    }
    function sovField() {
      var f = sovFields[sovKey(sovNPC)];
      if (f) return f;
      // Mid-drag or mid-zoom, never build: draw once it settles.
      if (sovMoving()) { sovSettle(); return null; }
      f = sovBuild(sovNPC);
      sovPrebuild();
      return f;
    }
    function sovBuild(npc) {
      var key = sovKey(npc);
      if (sovFields[key]) return sovFields[key];
      var list = mode === "universe" ? null : G.members[region];
      var n = list ? list.length : G.n, A = LY.alliances.length;
      var ownerOf = new Int32Array(G.n).fill(-1), npcIdx = {}, i, m, j;
      for (m = 0; m < n; m++) {
        i = list ? list[m] : m;
        if (LY.sovA[i] >= 0) ownerOf[i] = LY.sovA[i];
        else if (npc && LY.sovF[i] > 0 && G.tier[i] === 0) {
          if (npcIdx[LY.sovF[i]] === undefined) npcIdx[LY.sovF[i]] = A + Object.keys(npcIdx).length;
          ownerOf[i] = npcIdx[LY.sovF[i]];
        }
      }
      var b = viewBounds(), bw = b[2] - b[0], bh = b[3] - b[1];
      var sigma = 0.85 * Math.sqrt(bw * bh / Math.max(n, 1));
      var src = { o: [], x: [], y: [], s: [] };
      function add(o, x, y, s) { src.o.push(o); src.x.push(x); src.y.push(y); src.s.push(s); }
      for (m = 0; m < n; m++) {
        i = list ? list[m] : m;
        var o = ownerOf[i];
        if (o < 0) continue;
        var xa = wx(i), ya = wy(i), near = Infinity;
        for (var e = 0; e < G.adj[i].length; e++) {
          j = G.adj[i][e];
          if (list && S.r[j] !== region) continue;
          var d = Math.hypot(wx(j) - xa, wy(j) - ya);
          if (d < near) near = d;
          if (j <= i || ownerOf[j] !== o) continue;
          // Influence along a gate within one holding: points a
          // sigma apart on a short gate; a long one carries a broad
          // band, wider the longer the gap, that fills the empty
          // space around it rather than drawing a thin corridor.
          var xb = wx(j), yb = wy(j);
          var s2 = d > 2.5 * sigma ? Math.min(3 * sigma, Math.max(sigma, 0.25 * d)) : sigma;
          var steps = Math.min(40, Math.max(1, Math.ceil(d / (s2 * 0.6))));
          for (var t = 1; t < steps; t++) add(o, xa + (xb - xa) * t / steps, ya + (yb - ya) * t / steps, s2);
        }
        // A system far from its neighbours reaches farther.
        add(o, xa, ya, isFinite(near) ? Math.min(2.2 * sigma, Math.max(sigma, 0.8 * near)) : sigma);
      }
      // The field runs past the view's edge, so territory at the
      // map's rim rounds off instead of being cut straight.
      var padW = 3 * sigma;
      b = [b[0] - padW, b[1] - padW, b[2] + padW, b[3] + padW];
      var F = territoryField(src, b, mode === "universe" ? sovGW : Math.round(sovGW / 2));
      var shapes = [], outlines = territoryOutlines(F, 0.3);
      var bx0 = Infinity, by0 = Infinity, bx1 = -Infinity, by1 = -Infinity;
      Object.keys(outlines).forEach(function (ok) {
        var o = +ok, loops = outlines[ok];
        var path = new Path2D();
        loops.forEach(function (lp) {
          path.moveTo(lp[0][0], lp[0][1]);
          for (var q = 0; q < lp.length; q++) {
            if (q) path.lineTo(lp[q][0], lp[q][1]);
            if (lp[q][0] < bx0) bx0 = lp[q][0]; if (lp[q][0] > bx1) bx1 = lp[q][0];
            if (lp[q][1] < by0) by0 = lp[q][1]; if (lp[q][1] > by1) by1 = lp[q][1];
          }
          path.closePath();
        });
        var c = rgbOf(o < A ? allianceColor(o) : colors.npc);
        shapes.push({ path: path, fill: "rgba(" + c[0] + "," + c[1] + "," + c[2] + ",0.17)", line: "rgba(" + c[0] + "," + c[1] + "," + c[2] + ",0.9)" });
      });
      sovFields[key] = { shapes: shapes, bounds: [bx0, by0, bx1, by1], fitK: fitK() };
      return sovFields[key];
    }

    // Territory is painted into pictures and the pictures are drawn,
    // not the shapes: filling and outlining every holder's detailed
    // shape every frame is what made the territory view slow while
    // moving. A base picture holds the whole territory (up to 2048 px
    // across), painted once; a sharp picture holds the view and a
    // margin (up to 4096 px) at the zoom the map came to rest at, painted once each
    // time it settles. While the map moves, the GPU only moves and
    // scales a picture: the sharp one while it covers the view near
    // its zoom, else the base; once it settles, the view is painted
    // sharp again. Borders are drawn 1.6 px wide on screen at the
    // zoom each picture is painted for.
    function paintTerritory(f, w, h, scale, tx, ty, lineWidth) {
      var c = root.createElement("canvas");
      c.width = Math.max(1, Math.ceil(w));
      c.height = Math.max(1, Math.ceil(h));
      var g = c.getContext("2d");
      g.setTransform(scale, 0, 0, scale, tx, ty);
      g.lineJoin = "round";
      f.shapes.forEach(function (sh) {
        g.fillStyle = sh.fill;
        g.fill(sh.path, "evenodd");
      });
      g.lineWidth = lineWidth;
      f.shapes.forEach(function (sh) {
        g.strokeStyle = sh.line;
        g.stroke(sh.path);
      });
      return c;
    }
    function drawTerritory(k, ox, oy) {
      var f = sovField();
      if (!f) return;
      var b = f.bounds;
      if (!(b[2] > b[0])) return;
      // The view in world units.
      var vx0 = -ox / k, vy0 = -oy / k, vx1 = (cw - ox) / k, vy1 = (ch - oy) / k;
      var sharp = f.sharp;
      var covers = sharp && vx0 >= sharp.x0 && vy0 >= sharp.y0 && vx1 <= sharp.x1 && vy1 <= sharp.y1;
      if (!sovMoving() && !(covers && sharp.k === k)) {
        // At rest: paint the view, and a quarter of it each side, sharp.
        var mx = (vx1 - vx0) * 0.2, my = (vy1 - vy0) * 0.2;
        var x0 = vx0 - mx, y0 = vy0 - my, x1 = vx1 + mx, y1 = vy1 + my;
        // Never more than 4096 px on a side, so a big screen at a
        // high pixel ratio stays within a modest GPU's memory.
        var s = Math.min(k * dpr, 4096 / Math.max(x1 - x0, y1 - y0));
        sharp = f.sharp = { k: k, x0: x0, y0: y0, x1: x1, y1: y1,
          canvas: paintTerritory(f, (x1 - x0) * s, (y1 - y0) * s, s, -x0 * s, -y0 * s, 1.6 / k) };
        covers = true;
      }
      var r = sharp && sharp.k / k;
      if (covers && r > 0.5 && r < 2) {
        ctx.drawImage(sharp.canvas, sharp.x0 * k + ox, sharp.y0 * k + oy, (sharp.x1 - sharp.x0) * k, (sharp.y1 - sharp.y0) * k);
        return;
      }
      if (!f.base) {
        var bw = b[2] - b[0], bh = b[3] - b[1], bs = Math.min(2048 / bw, 2048 / bh);
        f.base = { s: bs, canvas: paintTerritory(f, bw * bs, bh * bs, bs, -b[0] * bs, -b[1] * bs, 1.6 / f.fitK) };
      }
      ctx.drawImage(f.base.canvas, b[0] * k + ox, b[1] * k + oy, (b[2] - b[0]) * k, (b[3] - b[1]) * k);
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
          var terr = sovView === "territory";
          label(name, h.x * k + ox, h.y * k + oy - 6, terr ? (mode === "universe" ? 14 : 15) : (mode === "universe" ? 11 : 12), allianceColor(h.a), "700", false, "A" + h.a, fontDisplay);
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
      // A data heat layer colors the systems; the picker's other
      // colorings wait until it is off.
      if (name !== "security" && dataHeat()) name = "security";
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
      var dk = dataHeat();
      secLegend.hidden = layer !== "security" || !!dk;
      box.hidden = layer === "security" && !dk;
      if (dk) { renderDataLegend(box, dk); return; }
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
        top.textContent = " " + heatTop(layer);
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
        if (sovView === "territory") {
          var npcRow = root.createElement("label");
          npcRow.className = "map-layer-ctl map-layer-npc";
          var npcCb = root.createElement("input");
          npcCb.type = "checkbox";
          npcCb.checked = sovNPC;
          npcCb.setAttribute("data-map-sov-npc", "");
          npcRow.appendChild(npcCb);
          npcRow.appendChild(root.createTextNode(" NPC faction borders"));
          box.appendChild(npcRow);
        }
        row("", "hsl(30,68%," + colors.sovL + ")", "Alliances");
        if (sovNPC || sovView !== "territory") row("", colors.npc, "NPC factions");
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
            rows.push(["Killfeed, last hour", LY.threatHour[i] + (LY.threatHour[i] === 1 ? " kill" : " kills"), killsHref(i)]);
            if (LY.threatLast[i]) rows.push(["Last kill", agoShort(LY.threatLast[i])]);
          } else if (LY.spill[i] >= THREAT_LEVELS[1].at) {
            rows.push(["Threat", "Quiet · danger next door"]);
          } else if (!brief) {
            rows.push(["Threat", "Quiet · no PvP kills lately"]);
          }
          if (LY.campOf[i] >= 0) {
            var c = LY.camps[LY.campOf[i]];
            rows.push(["Gate camp", (c.group ? c.group + " · " : "") + c.n + " kills since " + agoShort(c.since), c.gid ? "/intel/kills/?" + c.kind + "=" + c.gid : ""]);
          }
          break;
        }
        case "kills":
          if (brief && !LY.ship[i] && !LY.pod[i]) break;
          rows.push(["Game count, last hour", LY.ship[i] + (LY.ship[i] === 1 ? " ship" : " ships") + " · " + LY.pod[i] + (LY.pod[i] === 1 ? " pod" : " pods"), killsHref(i)]);
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
            rows.push(["Sovereignty", al.name || "Alliance " + al.id, "/alliance/?alliance=" + al.id]);
          } else if (LY.sovF[i] > 0) {
            rows.push(["Sovereignty", LY.factions[LY.sovF[i]] || "NPC faction"]);
          } else if (!brief) {
            rows.push(["Sovereignty", "Unclaimed"]);
          }
          break;
        case "fw":
          if (LY.fwState[i]) {
            rows.push(["Faction Warfare", (LY.factions[LY.fwOcc[i]] || "Occupied") + " · " +
              ["", "Uncontested", "Contested", "Vulnerable", "Captured"][LY.fwState[i]] + (LY.fwPct[i] ? " " + LY.fwPct[i] + "%" : ""), "/intel/fw/"]);
          } else if (!brief) {
            rows.push(["Faction Warfare", "Not a warzone system"]);
          }
          break;
        case "inc":
          if (LY.incOf[i] >= 0) {
            var x = LY.incs[LY.incOf[i]];
            rows.push(["Incursion", (LY.inc[i] === 2 ? "Staging · " : "Infested · ") + x.state + " · " + Math.round(x.inf * 100) + "% influence" + (x.boss ? " · boss" : ""), "/intel/incursions/"]);
          } else if (!brief) {
            rows.push(["Incursion", "None here"]);
          }
          break;
      }
      return rows;
    }
    function tipRows(i) { return dataRows(i).concat(intelRows(i, 2), layer === "security" || dataHeat() ? [] : layerRows(i, layer, false)); }
    function liveRows(i) {
      var rows = [];
      rows = dataRows(i).concat(intelRows(i, 3));
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

    // How much a system has on it right now, 0 to 1: a fresh intel
    // report, one of your pins, or a hot spot on the heat showing.
    function interest(i) {
      var w = 0, dk = dataHeat();
      if (IN && intelOn && IN.bySys[i] && intelFresh(IN.bySys[i][0].at, intelNow()) > 0) return 1;
      if (MINE) {
        for (var name in dataOn) if (dataOn[name] && !DATA_HEAT[name] && MINE[name] && MINE[name][i]) return 1;
        var H = dk && MINE.heat[dk];
        if (H && H.vals[i] > 0) w = 0.4 + 0.6 * heatT(H.vals[i], heatMax(dk));
      }
      if (!dk && LY && HEAT[layer]) {
        if (layer === "threat" && LY.campOf[i] >= 0) return 1;
        if (LY[layer][i] > 0) w = Math.max(w, 0.2 + 0.8 * heatT(LY[layer][i], heatMax(layer)));
      }
      return w;
    }
    // The system under the pointer. Zoomed out, systems crowd
    // together; one with something on it reaches a little farther and
    // wins over a bare neighbour at a similar distance, so the pointer
    // finds what the reader is most likely after. One right under the
    // pointer still wins.
    function pick(px, py) {
      var k = cam.k, ox = cw / 2 - cam.x * k, oy = ch / 2 - cam.y * k;
      var best = -1, bs = Infinity, list = visible(), n = list ? list.length : G.n;
      var dot = radius() + 1;
      for (var m = 0; m < n; m++) {
        var i = list ? list[m] : m;
        var dx = wx(i) * k + ox - px, dy = wy(i) * k + oy - py, d = dx * dx + dy * dy;
        if (d >= 24 * 24) continue;
        var w = interest(i), reach = pickReach(w);
        if (d >= reach * reach) continue;
        var s = pickScore(d, w, dot);
        if (s < bs) { bs = s; best = i; }
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
      var bb = uBounds(S.r[s]), ext = Math.max(bb[2] - bb[0], bb[3] - bb[1]) * cam.k;
      if (ext >= Math.min(cw, ch) * 1.25) enterRegion(S.r[s], s, true);
    }
    function nearestUniverse(x, y) {
      var best = -1, bd = Infinity;
      for (var i = 0; i < G.n; i++) {
        var dx = wx(i) - x, dy = wy(i) - y, d = dx * dx + dy * dy;
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
      var rg = P.regions[region], bb = uBounds(region);
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
      q.push("view=" + formatView(cam.x, cam.y, cam.k / fitK()));
      var shownLayer = LY ? layer : wantLayer || layer;
      if (shownLayer && shownLayer !== "security") q.push("layer=" + encodeURIComponent(shownLayer));
      if (shownLayer === "sov" && sovView === "territory") q.push("sov=territory");
      if (shownLayer === "sov" && sovView === "territory" && !sovNPC) q.push("npc=0");
      if (projOn) q.push("proj=1");
      if (routeFrom >= 0) q.push("from=" + S.id[routeFrom]);
      if (routeTo >= 0) q.push("to=" + S.id[routeTo]);
      if ((routeFrom >= 0 || routeTo >= 0) && routeMode !== "short") q.push("route=" + routeMode);
      var dk = Object.keys(dataOn).filter(function (k) { return dataOn[k]; });
      if (dk.length) q.push("data=" + dk.join(","));
      if ((routeFrom >= 0 || routeTo >= 0) && travel === "jump") {
        q.push("travel=jump");
        var sh = jumpShip();
        if (sh) q.push("ship=" + sh.t);
        var jl = /^jdc:(\d)$/.exec(jumpPilot);
        if (jl && jl[1] !== "5") q.push("jdc=" + jl[1]);
      }
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
        // A row with a source opens it: the killboard, the channel,
        // the alliance, the warzone or incursion page.
        if (rw[2]) {
          var a = root.createElement("a");
          a.href = rw[2];
          a.textContent = rw[1];
          dd.appendChild(a);
        } else {
          dd.textContent = rw[1];
        }
        d.appendChild(dt);
        d.appendChild(dd);
        live.appendChild(d);
      });
      card.querySelector('[data-route-set="from"]').hidden = i === routeFrom;
      card.querySelector('[data-route-set="to"]').hidden = i === routeTo;
      card.querySelector("[data-map-scan]").href = "/intel/scan/?system=" + S.id[i];
      cardScans(i);
      cardKills(i);
      cardReport(i);
    }

    function killsHref(i) { return "/intel/kills/?system=" + S.id[i]; }

    // The data behind the kills and threat layers for the card's
    // system (/map/system.json): its latest kills, each opening on the
    // killboard, and the camp report when one is on. Shown with those
    // layers, or wherever there is a camp or a kill in the last hour.
    var systemFor = {};
    function cardKills(i) {
      var kbox = card.querySelector(".map-card-kills"), cbox = card.querySelector(".map-card-camp");
      var hot = LY && (LY.campOf[i] >= 0 || LY.threatHour[i] > 0);
      if (!(hot || layer === "kills" || layer === "threat")) {
        kbox.hidden = cbox.hidden = true;
        return;
      }
      function killItem(k) {
        var li = root.createElement("li"), a = root.createElement("a");
        a.href = "/intel/kill/" + k.id + "/";
        a.textContent = k.ship + " · " + k.victim + (k.group ? " (" + k.group + ")" : "");
        li.appendChild(a);
        var m = root.createElement("small");
        m.className = "muted";
        m.textContent = " " + (k.value ? k.value + " · " : "") + agoShort(k.at) + (k.solo ? " · solo" : "") + (k.npc ? " · NPC" : "");
        li.appendChild(m);
        return li;
      }
      function show(d) {
        if (sel !== i) return;
        var ul = kbox.querySelector("ul");
        ul.textContent = "";
        d.kills.forEach(function (k) { ul.appendChild(killItem(k)); });
        kbox.hidden = !d.kills.length;
        var more = kbox.querySelector("[data-map-kills-more]");
        more.href = d.more;
        more.textContent = "All kills in " + S.n[i] + " on the killboard";
        cbox.hidden = !d.camp;
        if (!d.camp) return;
        var c = d.camp, who = cbox.querySelector("[data-map-camp-who]");
        who.textContent = "";
        var ga = root.createElement("a");
        ga.href = c.href;
        ga.textContent = c.group || "Unknown " + c.kind;
        who.appendChild(ga);
        who.appendChild(root.createTextNode(" · " + plural(c.n, "kill") + " since " + agoShort(c.since) + " · " + plural(c.pilots, "pilot")));
        cbox.querySelector("[data-map-camp-ships]").textContent = c.ships.length
          ? "Flying " + c.ships.map(function (s) { return s.name + (s.n > 1 ? " ×" + s.n : ""); }).join(", ")
          : "";
        var vl = cbox.querySelector("ul");
        vl.textContent = "";
        c.victims.forEach(function (k) { vl.appendChild(killItem(k)); });
      }
      var id = S.id[i], cached = systemFor[id];
      if (cached && Date.now() - cached.at < 60000) { show(cached.d); return; }
      kbox.hidden = cbox.hidden = true;
      fetch("/map/system.json?system=" + id, { credentials: "same-origin" }).then(function (r) {
        return r.ok ? r.json() : null;
      }).then(function (d) {
        if (!d || !el.isConnected) return;
        systemFor[id] = { at: Date.now(), d: d };
        show(d);
      }).catch(function () { /* the card stands without them */ });
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

    // After the reader sets or changes a route, the view flies to show
    // all of it (fitNext), in the part of the map the route panel
    // leaves free. Re-plans of their own (fresh intel, the layers
    // arriving) leave the view where it is.
    var fitNext = false;
    function routeKey() { return routePath ? routePath.join(",") : ""; }
    function fitRoute() {
      if (!routePath || routePath.length < 2 || !cw) return;
      if (mode !== "universe" && routePath.some(function (i) { return S.r[i] !== region; })) leaveRegion();
      var x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
      routePath.forEach(function (i) {
        x0 = Math.min(x0, wx(i)); x1 = Math.max(x1, wx(i));
        y0 = Math.min(y0, wy(i)); y1 = Math.max(y1, wy(i));
      });
      // The free part of the canvas: left of the side column, or
      // above it where it is a sheet rising from the bottom (phones).
      var fx0 = 0, fy0 = 0, fx1 = cw, fy1 = ch, side = el.querySelector(".map-side");
      if (side && !routeEl.hidden) {
        var cr = canvas.getBoundingClientRect(), sr = side.getBoundingClientRect();
        if (sr.top > cr.top + ch * 0.3) fy1 = Math.max(ch * 0.4, sr.top - cr.top);
        else fx1 = Math.max(cw * 0.4, sr.left - cr.left);
      }
      var pad = 48;
      var k = routeFitScale(x1 - x0, y1 - y0, fx1 - fx0, fy1 - fy0, pad);
      k = clampK(Math.min(k, fitK() * (mode === "universe" ? 12 : 3)));
      flyTo((x0 + x1) / 2 - ((fx0 + fx1) / 2 - cw / 2) / k, (y0 + y1) / 2 - ((fy0 + fy1) / 2 - ch / 2) / k, k, 420);
    }
    function setRouteEnd(which, i) {
      if (i < 0) return;
      fitNext = true;
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
      fitNext = false;
      routeFrom = routeTo = -1;
      routePath = routeSum = null;
      routeNote("");
      renderRoute();
      updateCard();
      syncURL();
      schedule();
    }
    // How to travel: "gates" (with the corporation's Ansiblex bridges
    // when known and wanted) or "jump" (a jump drive, cyno to cyno).
    // In jump mode the preferences read as fewest jumps (short), least
    // fatigue -- the shortest total distance (safe) -- and avoid danger
    // at the cyno points (calm). routeLegs holds each jump's light years.
    var travel = startQuery.get("travel") === "jump" ? "jump" : "gates";
    var JUMP = null, jumpLoading = false, routeLegs = null, bridgeLegs = 0;
    var jumpShipID = +startQuery.get("ship") || 0, jumpPilot = startQuery.get("jdc") ? "jdc:" + startQuery.get("jdc") : "";
    try {
      if (!jumpShipID) jumpShipID = +localStorage.getItem("evesynapse-map-jump-ship") || 0;
      if (!jumpPilot) jumpPilot = localStorage.getItem("evesynapse-map-jump-pilot") || "jdc:5";
    } catch (e) { if (!jumpPilot) jumpPilot = "jdc:5"; }
    var bridgesOn = true;
    function jumpShip() {
      if (!JUMP) return null;
      var pick = null;
      JUMP.ships.forEach(function (s) { if (s.t === jumpShipID) pick = s; });
      return pick || JUMP.ships.filter(function (s) { return s.g === "Jump Freighter"; })[0] || JUMP.ships[0] || null;
    }
    function jumpPilotInfo() {
      var m = /^p:(\d+)$/.exec(jumpPilot);
      if (m && MINE && MINE.jumpers) {
        for (var k = 0; k < MINE.jumpers.length; k++) if (MINE.jumpers[k].id === +m[1]) return MINE.jumpers[k];
      }
      var lv = /^jdc:(\d)$/.exec(jumpPilot);
      return { jdc: lv ? +lv[1] : 5 };
    }
    function jumpRangeNow() {
      var s = jumpShip();
      return s ? jumpRange(s.r, jumpPilotInfo().jdc) : 0;
    }
    function loadJump() {
      if (JUMP || jumpLoading) return;
      jumpLoading = true;
      fetch("/map/jump.json", { credentials: "same-origin" }).then(function (r) { return r.ok ? r.json() : null; }).then(function (J) {
        jumpLoading = false;
        if (!J || !el.isConnected) return;
        JUMP = joinJump(G, J);
        fillJumpControls();
        replan();
      }).catch(function () { jumpLoading = false; });
    }
    // The jump bridges known, as extra one-way edges by system index.
    function bridgeEdges() {
      if (!MINE || !MINE.bridges || !MINE.bridges.length) return null;
      var out = {};
      MINE.bridges.forEach(function (b) {
        var a = G.byID[b.from], c = G.byID[b.to];
        if (a === undefined || c === undefined) return;
        (out[a] = out[a] || []).push(c);
      });
      return out;
    }
    function replan() {
      if (signedIn && !MINE) fetchMine();
      var prices = IN && intelOn ? IN.prices : null;
      routeLegs = null;
      bridgeLegs = 0;
      routePath = null;
      if (routeFrom >= 0 && routeTo >= 0) {
        if (travel === "jump") {
          if (!JUMP) loadJump();
          var R = jumpRangeNow();
          if (JUMP && R > 0 && JUMP.dest[routeTo]) {
            var danger = routeCost(G, LY, "calm");
            var cost = routeMode === "safe" ? function (v, u) { return lyBetween(JUMP, u, v) + 0.01; }
              : routeMode === "calm" ? function (v) { return danger(v); }
              : function () { return 1; };
            routePath = planRoute(G, routeFrom, routeTo, cost, function (u) { return jumpNeighbours(JUMP, u, R); });
            if (routePath) {
              routeLegs = [];
              for (var m = 1; m < routePath.length; m++) routeLegs.push(lyBetween(JUMP, routePath[m - 1], routePath[m]));
            }
          }
        } else {
          var BR = bridgesOn ? bridgeEdges() : null;
          routePath = planRoute(G, routeFrom, routeTo, routeCost(G, LY, routeMode),
            BR ? function (u) { return BR[u] ? G.adj[u].concat(BR[u]) : G.adj[u]; } : null);
          if (routePath && BR) {
            for (var b = 1; b < routePath.length; b++) if (G.adj[routePath[b - 1]].indexOf(routePath[b]) < 0) bridgeLegs++;
          }
        }
      }
      routeSum = routePath ? routeSummary(G, LY, routePath, prices) : null;
      renderRoute();
      if (fitNext && routePath) { fitNext = false; fitRoute(); }
      updateCard();
      syncURL();
      schedule();
    }
    // The ship and pilot pickers, once the jump data (and the reader's
    // pilots) are in.
    function fillJumpControls() {
      var shipSel = routeEl.querySelector("[data-route-ship]"), pilotSel = routeEl.querySelector("[data-route-pilot]");
      if (JUMP && !shipSel.options.length) {
        var groups = {};
        JUMP.ships.forEach(function (s) {
          if (!groups[s.g]) {
            groups[s.g] = root.createElement("optgroup");
            groups[s.g].label = s.g;
            shipSel.appendChild(groups[s.g]);
          }
          var o = root.createElement("option");
          o.value = String(s.t);
          o.textContent = s.n + " · " + s.r.toFixed(2).replace(/\.?0+$/, "") + " LY base";
          groups[s.g].appendChild(o);
        });
      }
      var ship = jumpShip();
      if (ship) shipSel.value = String(ship.t);
      if (MINE && MINE.jumpers && MINE.jumpers.length && !pilotSel.querySelector("[data-pilots]")) {
        var og = root.createElement("optgroup");
        og.label = "Your pilots";
        og.setAttribute("data-pilots", "");
        MINE.jumpers.forEach(function (j) {
          var o = root.createElement("option");
          o.value = "p:" + j.id;
          o.textContent = j.name + (j.known ? " · JDC " + j.jdc : "");
          og.appendChild(o);
        });
        pilotSel.insertBefore(og, pilotSel.firstChild);
      }
      if ([].some.call(pilotSel.options, function (o) { return o.value === jumpPilot; })) pilotSel.value = jumpPilot;
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
        var mk = b.getAttribute("data-route-mode");
        b.setAttribute("aria-checked", mk === routeMode ? "true" : "false");
        b.textContent = (travel === "jump" ? { short: "Fewest jumps", safe: "Least fatigue", calm: "Avoid danger" }
          : { short: "Shortest", safe: "Safer", calm: "Avoid danger" })[mk];
      });
      routeEl.querySelectorAll("[data-route-travel]").forEach(function (b) {
        b.setAttribute("aria-checked", b.getAttribute("data-route-travel") === travel ? "true" : "false");
      });
      routeEl.querySelector(".map-route-jump").hidden = travel !== "jump";
      var nBridges = MINE && MINE.bridges ? MINE.bridges.length : 0, bridgesBox = routeEl.querySelector("[data-route-bridges]");
      bridgesBox.hidden = travel !== "gates" || !nBridges;
      bridgesBox.querySelector("input").checked = bridgesOn;
      bridgesBox.querySelector("[data-route-bridges-n]").textContent = nBridges ? "(" + nBridges + " known)" : "";
      if (travel === "jump") {
        fillJumpControls();
        var rr = jumpRangeNow(), ship = jumpShip();
        routeEl.querySelector("[data-route-range]").textContent = !JUMP ? "Loading jump data…"
          : ship ? ship.n + " jumps " + rr.toFixed(2) + " LY" : "No jump-capable ships in the data.";
      }
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
        sum.textContent = travel !== "jump" ? "No stargates join these systems."
          : !JUMP ? "Loading jump data…"
          : !JUMP.dest[routeTo] ? "A jump can't land in high-sec, Pochven or Zarzakh: pick a destination in low-sec or null."
          : "No chain of cynos within " + jumpRangeNow().toFixed(2) + " LY reaches there: try another ship, or more Jump Drive Calibration.";
      } else {
        var s = routeSum;
        var j = root.createElement("strong");
        j.textContent = s.jumps === 1 ? "1 jump" : s.jumps + " jumps";
        sum.appendChild(j);
        if (routeLegs) {
          // Jump drive: the distance, the longest leg, and the fatigue
          // it comes to (an estimate, by CCP's published rules).
          var total = routeLegs.reduce(function (a, b) { return a + b; }, 0), longest = Math.max.apply(null, routeLegs);
          var ly = root.createElement("span");
          ly.className = "map-route-band";
          ly.textContent = total.toFixed(1) + " LY · longest " + longest.toFixed(2);
          sum.appendChild(ly);
          var pi = jumpPilotInfo(), shipNow = jumpShip(), held = pi.fatigue ? Math.max(0, (pi.fatigue - Date.now() / 1000) / 60) : 0;
          var est = jumpFatigue(routeLegs, shipNow ? shipNow.f : 1, held);
          var fat = root.createElement("li");
          fat.className = "map-route-fatigue";
          fat.textContent = "Fatigue after ≈ " + minutesShort(est.fatigue) + (est.waits >= 1 ? " · waits between jumps ≈ " + minutesShort(est.waits) : "") +
            (held ? " · " + (pi.name || "Pilot") + " holds " + minutesShort(held) + " now" : "") + " (estimate)";
          warn.appendChild(fat);
        } else if (bridgeLegs) {
          var br = root.createElement("span");
          br.className = "map-route-band";
          br.textContent = plural(bridgeLegs, "jump bridge");
          sum.appendChild(br);
        }
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
        routePath.forEach(function (i, at) {
          var li = root.createElement("li");
          li.appendChild(sysButton(i));
          if (routeLegs && at > 0) {
            var legLY = root.createElement("span");
            legLY.className = "map-route-leg";
            legLY.textContent = " " + routeLegs[at - 1].toFixed(2) + " LY";
            li.appendChild(legLY);
          } else if (at > 0 && G.adj[routePath[at - 1]].indexOf(i) < 0) {
            var via = root.createElement("span");
            via.className = "map-route-leg";
            via.textContent = " bridge";
            li.appendChild(via);
          }
          if (LY && LY.campOf[i] >= 0) li.className = "camp";
          else if (LY && threatLevel(LY.threat[i]) >= 2) li.className = "hot";
          list.appendChild(li);
        });
      }
      var acts = routeEl.querySelector(".map-route-actions");
      acts.hidden = !signedIn;
      acts.querySelectorAll("[data-route-send]").forEach(function (b) {
        b.disabled = sending || !canSend || (b.getAttribute("data-route-send") === "route" ? !routePath || routeSum.jumps < 1 : routeTo < 0);
        // The autopilot follows gates: a cyno route is not one.
        if (b.getAttribute("data-route-send") === "route") b.hidden = travel === "jump";
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
        if (mode === "universe") { pa = [wx(a), wy(a)]; pb = [wx(b), wy(b)]; }
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
      // A jump leg is no gate: drawn dashed, cyno to cyno.
      if (routeLegs) ctx.setLineDash([9, 6]);
      ctx.stroke();
      ctx.setLineDash([]);
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
        var what = r.clear ? "Clear" : r.calm ? "Friendly" : r.n ? r.n + (r.n === 1 ? " hostile" : " hostiles") : "Hostiles";
        return [IN.names[r.ch] || "Intel", what + (r.text ? " · " + r.text : "") + " · " + agoShort(r.at - intelSkew), "/intel/channels/" + r.ch + "/"];
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
        var calm = last.clear || last.calm;
        var c = calm ? colors.intelClear : colors.intel;
        var rr = r + 6 + (calm ? 0 : Math.min(8, Math.log(1 + (last.n || 1)) * 3));
        ctx.beginPath();
        ctx.arc(x, y, rr, 0, Math.PI * 2);
        ctx.lineWidth = 2.2;
        ctx.strokeStyle = intelRGBA(c, 0.35 + 0.65 * f);
        ctx.stroke();
        if (!calm) {
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
        var text = last.clear ? "clear · " + m : last.calm ? "friendly · " + m : (last.n ? last.n + " · " : "") + m;
        label(text, wx(i) * k + ox, wy(i) * k + oy - r - 24, 11, intelRGBA(last.clear || last.calm ? colors.intelClear : colors.intel, 1), "700", false, "N" + i);
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

    // --- Your data (map_mine.go) ---
    // The Data menu turns on layers of the reader's own data. Pins
    // (pilots, fleet, clones, market, industry, structures, posted)
    // draw over everything and combine freely. Heat layers (assets,
    // PvP, mining) color the systems themselves, so one at a time, and
    // while one is on the layer picker's colorings stand down: the
    // menu greys out the other heats, the picker greys out its chips.
    var DATA_ALL = ["pilots", "fleet", "clones", "assets", "market", "industry", "structures", "pvp", "mining", "posted", "watched"];
    var DATA_HEAT = {
      assets: { title: "Your assets, estimated ISK", isk: true },
      pvp: { title: "Your kills and losses, 30 days" },
      mining: { title: "Your mining, 30 days, units" }
    };
    var DATA_KEY = "evesynapse-map-data";
    var dataOn = {}, MINE = null, mineTimer = 0;
    var dataBtn = el.querySelector("[data-map-data]"), dataMenu = el.querySelector(".map-data-menu");
    (function () {
      var want = startQuery.get("data");
      if (want === null) { try { want = localStorage.getItem(DATA_KEY) || ""; } catch (e) { want = ""; } }
      want.split(",").forEach(function (k) { if (DATA_ALL.indexOf(k) >= 0) dataOn[k] = true; });
      // One heat at most, whatever was stored.
      var heat = "";
      DATA_ALL.forEach(function (k) { if (DATA_HEAT[k] && dataOn[k]) { if (heat) delete dataOn[k]; else heat = k; } });
      if (!signedIn) dataOn = {};
    })();
    function dataHeat() {
      for (var k in DATA_HEAT) if (dataOn[k]) return k;
      return "";
    }
    function dataAny() { for (var k in dataOn) if (dataOn[k]) return true; return false; }

    function iskShort(v) {
      if (v >= 1e12) return (v / 1e12).toFixed(1) + "t";
      if (v >= 1e9) return (v / 1e9).toFixed(1) + "b";
      if (v >= 1e6) return (v / 1e6).toFixed(v >= 1e8 ? 0 : 1) + "m";
      if (v >= 1e3) return Math.round(v / 1e3) + "k";
      return String(Math.round(v));
    }
    function untilShort(unix) {
      var s = unix - Date.now() / 1000;
      if (s <= 0) return "now";
      if (s < 3600) return Math.ceil(s / 60) + " min";
      if (s < 172800) return Math.round(s / 3600) + " h";
      return Math.round(s / 86400) + " d";
    }

    // joinMine indexes /map/mine.json by system index.
    function joinMine(D) {
      var M = { pilots: {}, fleet: {}, clones: {}, market: {}, industry: {}, structures: {}, posted: {}, watched: {}, heat: {}, max: {}, fleets: D.fleets || [], jumpers: D.jumpers || [], bridges: D.bridges || [] };
      function at(map, sys) {
        var i = G.byID[sys];
        if (i === undefined) return null;
        return map[i] || (map[i] = []);
      }
      function one(map, sys, v) { var i = G.byID[sys]; if (i !== undefined) map[i] = v; }
      (D.pilots || []).forEach(function (p) { var a = at(M.pilots, p.sys); if (a) a.push(p); });
      M.fleets.forEach(function (f) {
        (f.members || []).forEach(function (m) { var a = at(M.fleet, m.sys); if (a) a.push(m); });
      });
      (D.clones || []).forEach(function (c) { var a = at(M.clones, c.sys); if (a) a.push(c); });
      (D.market || []).forEach(function (m) { one(M.market, m.sys, m); });
      (D.industry || []).forEach(function (m) { one(M.industry, m.sys, m); });
      (D.structures || []).forEach(function (s) { var a = at(M.structures, s.sys); if (a) a.push(s); });
      (D.posted || []).forEach(function (p) { one(M.posted, p.sys, p); });
      (D.watched || []).forEach(function (w) { var a = at(M.watched, w.sys); if (a) a.push(w); });
      ["assets", "pvp", "mining"].forEach(function (k) {
        var vals = new Float64Array(G.n), rows = {}, top = 0;
        (D[k] || []).forEach(function (h) {
          var i = G.byID[h.sys];
          if (i === undefined) return;
          vals[i] = h.v;
          rows[i] = h;
          if (h.v > top) top = h.v;
        });
        M.heat[k] = { vals: vals, rows: rows };
        M.max[k] = top || 1;
      });
      return M;
    }
    function fetchMine(fresh) {
      // Wanted by the data layers on, or by an open route (its jump
      // bridges, its pilots' jump skills).
      if (!signedIn || (!dataAny() && routeFrom < 0 && routeTo < 0) || mineLoading) return;
      mineLoading = true;
      fetch("/map/mine.json" + (fresh ? "?fresh=1" : ""), { credentials: "same-origin" }).then(function (r) {
        return r.ok ? r.json() : null;
      }).then(function (D) {
        mineLoading = false;
        if (!D || !el.isConnected) return;
        MINE = joinMine(D);
        dataHeatOrder = null;
        syncDataMenu();
        renderLegend();
        // Bridges and pilots may change the route.
        if (routeFrom >= 0 && routeTo >= 0) replan(); else updateCard();
        schedule();
      }).catch(function () { mineLoading = false; });
    }
    var mineLoading = false;

    function minutesShort(min) {
      min = Math.round(min);
      if (min < 60) return min + " min";
      return Math.floor(min / 60) + " h " + ("0" + (min % 60)).slice(-2) + " min";
    }
    // In jump mode, the systems a jump from the start can reach are
    // ringed, over whatever layer is showing.
    function drawJumpRange(k, ox, oy, r) {
      if (travel !== "jump" || !JUMP || routeFrom < 0) return;
      var R = jumpRangeNow();
      if (!(R > 0)) return;
      ctx.beginPath();
      jumpNeighbours(JUMP, routeFrom, R).forEach(function (i) {
        if (mode !== "universe" && S.r[i] !== region) return;
        var x = wx(i) * k + ox, y = wy(i) * k + oy;
        if (x < -20 || y < -20 || x > cw + 20 || y > ch + 20) return;
        ctx.moveTo(x + r + 3, y);
        ctx.arc(x, y, r + 3, 0, Math.PI * 2);
      });
      ctx.lineWidth = 1.2;
      ctx.strokeStyle = colors.route;
      ctx.globalAlpha = 0.55;
      ctx.stroke();
      ctx.globalAlpha = 1;
    }

    function dataRGBA(name, a) {
      var c = colors.data[name];
      return "rgba(" + c[0] + "," + c[1] + "," + c[2] + "," + a + ")";
    }
    // drawData: the pins, each kind its own shape and place around
    // the system so several read at once. Every pin is drawn solid
    // over a dark outline, and the map under the pins fades a little
    // while any are on, so they stay readable over any layer and
    // over each other.
    function drawData(k, ox, oy, r) {
      if (!MINE) return;
      var pins = false;
      for (var name in dataOn) if (dataOn[name] && !DATA_HEAT[name]) pins = true;
      if (!pins) return;
      ctx.fillStyle = colors.halo;
      ctx.globalAlpha = 0.25;
      ctx.fillRect(0, 0, cw, ch);
      ctx.globalAlpha = 1;
      function each(name, fn) {
        if (!dataOn[name]) return;
        var map = MINE[name];
        for (var key in map) {
          var i = +key;
          if (mode !== "universe" && S.r[i] !== region) continue;
          var x = wx(i) * k + ox, y = wy(i) * k + oy;
          if (x < -30 || y < -30 || x > cw + 30 || y > ch + 30) continue;
          fn(i, x, y, map[key]);
        }
      }
      // The current path, outlined and then drawn in its color.
      function stroke(color, w) {
        ctx.lineWidth = w + 2.5;
        ctx.strokeStyle = colors.halo;
        ctx.stroke();
        ctx.lineWidth = w;
        ctx.strokeStyle = color;
        ctx.stroke();
      }
      function fill(color) {
        ctx.lineWidth = 2;
        ctx.strokeStyle = colors.halo;
        ctx.stroke();
        ctx.fillStyle = color;
        ctx.fill();
      }
      each("posted", function (i, x, y) {
        ctx.beginPath();
        ctx.arc(x, y, r + 13, 0, Math.PI * 2);
        ctx.setLineDash([4, 3]);
        stroke(dataRGBA("posted", 1), 1.8);
        ctx.setLineDash([]);
      });
      each("industry", function (i, x, y, v) {
        ctx.beginPath();
        ctx.arc(x, y, r + 9, 0, Math.PI * 2);
        stroke(dataRGBA("industry", 1), v.ready ? 2.8 : 1.8);
      });
      each("clones", function (i, x, y) {
        var s = r + 5;
        ctx.beginPath();
        ctx.rect(x - s, y - s, 2 * s, 2 * s);
        stroke(dataRGBA("clones", 1), 1.8);
      });
      each("fleet", function (i, x, y, list) {
        ctx.beginPath();
        ctx.arc(x, y, r + 3 + Math.min(6, Math.log(1 + list.length) * 2.5), 0, Math.PI * 2);
        ctx.fillStyle = dataRGBA("fleet", 0.35);
        ctx.fill();
        stroke(dataRGBA("fleet", 1), 1.6);
      });
      // A watched hostile: a red crosshair.
      each("watched", function (i, x, y) {
        var s = r + 7;
        ctx.beginPath();
        ctx.arc(x, y, s, 0, Math.PI * 2);
        ctx.moveTo(x - s - 4, y); ctx.lineTo(x - s + 3, y);
        ctx.moveTo(x + s - 3, y); ctx.lineTo(x + s + 4, y);
        ctx.moveTo(x, y - s - 4); ctx.lineTo(x, y - s + 3);
        ctx.moveTo(x, y + s - 3); ctx.lineTo(x, y + s + 4);
        stroke(colors.camp, 2);
      });
      each("market", function (i, x, y) {
        ctx.beginPath();
        ctx.arc(x + r + 5, y - r - 5, 3.6, 0, Math.PI * 2);
        fill(dataRGBA("market", 1));
      });
      each("structures", function (i, x, y, list) {
        var low = list.some(function (s) { return (s.fuel && s.fuel - Date.now() / 1000 < 3 * 86400) || /reinforce|armor|hull/.test(s.state); });
        ctx.beginPath();
        ctx.rect(x - r - 9.5, y - r - 9.5, 7, 7);
        fill(dataRGBA("structures", 1));
        if (low) {
          ctx.beginPath();
          ctx.rect(x - r - 11.5, y - r - 11.5, 11, 11);
          stroke(colors.camp, 1.6);
        }
      });
      each("pilots", function (i, x, y, list) {
        var on = list.some(function (p) { return p.online; }), s = r + 4;
        if (on) {
          ctx.beginPath();
          ctx.arc(x, y, s + 4, 0, Math.PI * 2);
          stroke(dataRGBA("pilots", 0.8), 1.4);
        }
        ctx.beginPath();
        ctx.moveTo(x, y - s); ctx.lineTo(x + s, y); ctx.lineTo(x, y + s); ctx.lineTo(x - s, y); ctx.closePath();
        fill(dataRGBA("pilots", on ? 1 : 0.6));
      });
    }
    // Pilots are named where they are; a fleet's count shows up close.
    function dataLabels(k, ox, oy, label, inPlaceFirst) {
      if (!MINE) return;
      var r = radius(), close = mode !== "universe" || cam.k / fitK() >= 2.5;
      if (dataOn.pilots) {
        var keys = Object.keys(MINE.pilots).map(Number);
        inPlaceFirst(keys, function (i) { return "D" + i; }, function (i) {
          if (mode !== "universe" && S.r[i] !== region) return;
          var list = MINE.pilots[i], text = list[0].name + (list.length > 1 ? " +" + (list.length - 1) : "");
          label(text, wx(i) * k + ox, wy(i) * k + oy - r - 22, 11, dataRGBA("pilots", 1), "700", false, "D" + i);
        });
      }
      if (dataOn.watched) {
        var wk = Object.keys(MINE.watched).map(Number);
        inPlaceFirst(wk, function (i) { return "W" + i; }, function (i) {
          if (mode !== "universe" && S.r[i] !== region) return;
          var list = MINE.watched[i], text = list[0].name + (list.length > 1 ? " +" + (list.length - 1) : "");
          label(text, wx(i) * k + ox, wy(i) * k + oy + r + 14, 11, colors.camp, "700", false, "W" + i);
        });
      }
      if (dataOn.fleet && close) {
        var fk = Object.keys(MINE.fleet).map(Number);
        inPlaceFirst(fk, function (i) { return "F" + i; }, function (i) {
          if (mode !== "universe" && S.r[i] !== region) return;
          label(MINE.fleet[i].length + " in fleet", wx(i) * k + ox, wy(i) * k + oy + r + 16, 10.5, dataRGBA("fleet", 1), "700", false, "F" + i);
        });
      }
    }
    var dataHeatOrder = null;
    function drawDataHeat(k, ox, oy, r, list, n, pad) {
      var dk = dataHeat(), H = MINE && MINE.heat[dk];
      ctx.beginPath();
      for (var m = 0; m < n; m++) {
        var i = list ? list[m] : m;
        if (H && H.vals[i] > 0) continue;
        var sx = wx(i) * k + ox, sy = wy(i) * k + oy;
        if (sx < -pad || sy < -pad || sx > cw + pad || sy > ch + pad) continue;
        ctx.moveTo(sx + r * 0.8, sy);
        ctx.arc(sx, sy, r * 0.8, 0, Math.PI * 2);
      }
      ctx.fillStyle = colors.dim;
      ctx.fill();
      if (!H) return;
      if (!dataHeatOrder || dataHeatOrder.k !== dk) {
        var order = Object.keys(H.rows).map(Number).sort(function (a, b) { return H.vals[a] - H.vals[b]; });
        dataHeatOrder = { k: dk, order: order };
      }
      var max = heatMax(dk), grow = mode === "universe" ? Math.min(1 + (cam.k / fitK() - 1) * 0.15, 3) : 0.9;
      ctx.globalCompositeOperation = colors.heatBlend;
      dataHeatOrder.order.forEach(function (i) {
        if (mode !== "universe" && S.r[i] !== region) return;
        var sx = wx(i) * k + ox, sy = wy(i) * k + oy;
        if (sx < -pad || sy < -pad || sx > cw + pad || sy > ch + pad) return;
        var t = heatT(H.vals[i], max);
        ctx.beginPath();
        ctx.arc(sx, sy, r + (2 + 13 * t) * grow, 0, Math.PI * 2);
        ctx.fillStyle = heatColor(t, 0.18 + 0.3 * t);
        ctx.fill();
      });
      ctx.globalCompositeOperation = "source-over";
      dataHeatOrder.order.forEach(function (i) {
        if (mode !== "universe" && S.r[i] !== region) return;
        var sx = wx(i) * k + ox, sy = wy(i) * k + oy;
        ctx.beginPath();
        ctx.arc(sx, sy, r + 0.6, 0, Math.PI * 2);
        ctx.fillStyle = heatColor(heatT(H.vals[i], max), 1);
        ctx.fill();
      });
    }
    function renderDataLegend(box, dk) {
      box.textContent = "";
      var h = root.createElement("p");
      h.className = "map-layer-title";
      h.textContent = DATA_HEAT[dk].title;
      box.appendChild(h);
      var bar = root.createElement("p");
      bar.className = "map-heat";
      var g = root.createElement("span");
      g.className = "map-heat-bar";
      g.style.background = "linear-gradient(to right," + heatColor(0.05, 1) + "," + heatColor(0.75, 1) + "," + heatColor(1, 1) + ")";
      bar.appendChild(root.createTextNode(DATA_HEAT[dk].isk ? "0 " : "1 "));
      bar.appendChild(g);
      var top = root.createElement("span");
      top.className = "map-heat-top";
      top.textContent = " " + (MINE ? heatTop(dk) : "…");
      bar.appendChild(top);
      box.appendChild(bar);
      var sc = root.createElement("label");
      sc.className = "map-layer-ctl map-scale";
      var lo = root.createElement("span");
      lo.textContent = "Biggest";
      var rng = root.createElement("input");
      rng.type = "range";
      rng.min = "-3"; rng.max = "5"; rng.step = "0.5";
      rng.value = String(+heatGain[dk] || 0);
      rng.setAttribute("data-map-gain", "");
      rng.setAttribute("aria-label", "Layer scale");
      var hi = root.createElement("span");
      hi.textContent = "All of it";
      sc.appendChild(lo); sc.appendChild(rng); sc.appendChild(hi);
      box.appendChild(sc);
    }
    // dataRows: what the data layers on say about system i.
    function dataRows(i) {
      if (!MINE) return [];
      var rows = [];
      if (dataOn.pilots && MINE.pilots[i]) {
        rows.push(["Your pilots", MINE.pilots[i].map(function (p) {
          return p.name + (p.ship ? " (" + p.ship + ")" : "") + (p.online ? ", online" : "") + (Date.now() / 1000 - p.at > 3600 ? ", as of " + agoShort(p.at) : "");
        }).join(" · ")]);
      }
      if (dataOn.fleet && MINE.fleet[i]) {
        var fl = MINE.fleet[i];
        rows.push(["Fleet", fl.length + " here: " + fl.slice(0, 6).map(function (m) { return m.name + (m.ship ? " (" + m.ship + ")" : ""); }).join(", ") + (fl.length > 6 ? "…" : "")]);
      }
      if (dataOn.clones && MINE.clones[i]) {
        var cl = MINE.clones[i], homes = cl.filter(function (c) { return c.home; }), jumps = cl.filter(function (c) { return !c.home; });
        var parts = [];
        if (homes.length) parts.push("home of " + homes.map(function (c) { return c.char; }).join(", "));
        if (jumps.length) parts.push(plural(jumps.length, "jump clone") + " (" + jumps.map(function (c) { return c.char; }).join(", ") + ")");
        rows.push(["Clones", parts.join(" · ")]);
      }
      if (dataOn.market && MINE.market[i]) {
        var mk = MINE.market[i], mp = [];
        if (mk.sell) mp.push(mk.sell + " sell · " + iskShort(mk.iskSell));
        if (mk.buy) mp.push(mk.buy + " buy · " + iskShort(mk.iskBuy));
        if (mk.contracts) mp.push(plural(mk.contracts, "contract"));
        rows.push(["Orders", mp.join("; ")]);
      }
      if (dataOn.industry && MINE.industry[i]) {
        var ind = MINE.industry[i], ip = [];
        if (ind.jobs) ip.push(plural(ind.jobs, "job") + (ind.ready ? ", " + ind.ready + " ready" : "") + (ind.next ? " · next " + untilShort(ind.next) : ""));
        if (ind.colonies) ip.push(plural(ind.colonies, "colony").replace("colonys", "colonies"));
        rows.push(["Industry", ip.join("; ")]);
      }
      if (dataOn.structures && MINE.structures[i]) {
        MINE.structures[i].forEach(function (s) {
          rows.push([s.type || "Structure", s.name + " · " + s.state.replace(/_/g, " ") + (s.fuel ? " · fuel " + untilShort(s.fuel) : "") + (s.timer && s.timer > Date.now() / 1000 ? " · timer " + untilShort(s.timer) : "")]);
        });
      }
      var dk = dataHeat(), hr = dk && MINE.heat[dk].rows[i];
      if (hr) {
        if (dk === "assets") rows.push(["Your assets", "≈" + iskShort(hr.v) + " ISK"]);
        if (dk === "pvp") rows.push(["Your PvP, 30 days", plural(hr.a, "kill") + " · " + plural(hr.b, "loss").replace("losss", "losses") + " · " + iskShort(hr.c) + " ISK"]);
        if (dk === "mining") rows.push(["Your mining, 30 days", Math.round(hr.v).toLocaleString() + " units" + (hr.a ? " · " + iskShort(hr.a) + " ISK" : "")]);
      }
      if (dataOn.watched && MINE.watched[i]) {
        rows.push(["Watched", MINE.watched[i].map(function (w) {
          return w.name + (w.note ? " (" + w.note + ")" : "") + " · " + agoShort(w.at);
        }).join(" · ")]);
      }
      if (dataOn.posted && MINE.posted[i]) {
        var po = MINE.posted[i], pp = [];
        if (po.reports) pp.push(plural(po.reports, "report"));
        if (po.scans) pp.push(plural(po.scans, "scan"));
        rows.push(["You posted", pp.join(" · ") + " · " + agoShort(po.last)]);
      }
      return rows;
    }
    function plural(n, w) { return n === 1 ? "1 " + w : n + " " + w + "s"; }

    // The menu: what is on, what would conflict, and the badge.
    function syncDataMenu() {
      if (!dataMenu) return;
      var heat = dataHeat();
      dataMenu.querySelectorAll("input[type=checkbox]").forEach(function (cb) {
        var k = cb.value, lab = cb.closest("label");
        cb.checked = !!dataOn[k];
        var off = !!(DATA_HEAT[k] && heat && heat !== k);
        cb.disabled = off;
        lab.classList.toggle("off", off);
        lab.title = off ? "One heat layer at a time: turn off " + DATA_HEAT[heat].title.toLowerCase() + " first" : "";
      });
      var fleetSmall = dataMenu.querySelector('input[value="fleet"]').closest("label");
      if (MINE && dataOn.fleet) {
        var note = !MINE.fleets.length ? "Not in a fleet" : MINE.fleets.some(function (f) { return f.boss; }) ? "" : "Members show to the fleet boss only";
        fleetSmall.title = note;
      }
      var n = 0;
      for (var k2 in dataOn) if (dataOn[k2]) n++;
      dataBtn.querySelector("[data-map-data-n]").textContent = n ? String(n) : "";
      dataBtn.classList.toggle("on", n > 0);
      // The picker's colorings stand down under a data heat layer.
      el.querySelectorAll("[data-map-layer]").forEach(function (b) {
        var off = !!heat && b.getAttribute("data-map-layer") !== "security";
        b.classList.toggle("off", off);
        b.title = off ? "A data heat layer is on: turn it off in Data to use this layer" : "";
      });
    }
    // The layer a heat layer replaced, put back when the heat goes off.
    var layerBeforeHeat = "";
    function setData(k, on) {
      if (on && DATA_HEAT[k]) {
        var heat = dataHeat();
        if (heat && heat !== k) return;
        if (!heat) layerBeforeHeat = layer;
        if (layer !== "security") setLayer("security", true);
      }
      if (on) dataOn[k] = true; else delete dataOn[k];
      if (!on && DATA_HEAT[k] && layerBeforeHeat) {
        var back = layerBeforeHeat;
        layerBeforeHeat = "";
        if (back !== layer) setLayer(back, true);
      }
      try { localStorage.setItem(DATA_KEY, Object.keys(dataOn).join(",")); } catch (e) { /* private mode */ }
      dataHeatOrder = null;
      syncDataMenu();
      renderLegend();
      syncURL();
      clearInterval(mineTimer);
      if (dataAny()) {
        if (!MINE) fetchMine(); else { updateCard(); schedule(); }
        mineTimer = setInterval(function () { if (document.visibilityState === "visible") fetchMine(); }, 60000);
      } else {
        updateCard();
        schedule();
      }
    }
    function toggleDataMenu(open) {
      if (!dataMenu) return;
      var show = open === undefined ? dataMenu.hidden : open;
      dataMenu.hidden = !show;
      dataBtn.setAttribute("aria-expanded", show ? "true" : "false");
      // The map box clips what spills past it, so the menu opens
      // toward the side with room (leftward when its button sits at
      // the right end of the bar), and is never taller than the room
      // left below its button; it scrolls instead.
      if (show) {
        var box = el.getBoundingClientRect();
        dataMenu.style.left = "0";
        dataMenu.style.right = "auto";
        if (dataMenu.getBoundingClientRect().right > box.right - 8) {
          dataMenu.style.left = "auto";
          dataMenu.style.right = "0";
        }
        var room = Math.min(el.getBoundingClientRect().bottom, window.innerHeight) - dataMenu.getBoundingClientRect().top - 8;
        dataMenu.style.maxHeight = Math.max(160, room) + "px";
      }
    }
    function onDataOutside(e) {
      if (dataMenu && !dataMenu.hidden && !e.target.closest(".map-data")) toggleDataMenu(false);
    }
    // The route panel's pickers: ship, pilot, jump bridges.
    routeEl.addEventListener("change", function (e) {
      var tg = e.target;
      if (tg.hasAttribute("data-route-ship")) {
        jumpShipID = +tg.value;
        try { localStorage.setItem("evesynapse-map-jump-ship", String(jumpShipID)); } catch (err) { /* private mode */ }
      } else if (tg.hasAttribute("data-route-pilot")) {
        jumpPilot = tg.value;
        try { localStorage.setItem("evesynapse-map-jump-pilot", jumpPilot); } catch (err) { /* private mode */ }
      } else if (tg.closest("[data-route-bridges]")) {
        bridgesOn = tg.checked;
      } else {
        return;
      }
      routeNote("");
      replan();
    });
    if (dataMenu) {
      dataMenu.addEventListener("change", function (e) {
        if (e.target.type === "checkbox") setData(e.target.value, e.target.checked);
      });
      root.addEventListener("click", onDataOutside);
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
    // The wheel always zooms about the cursor (wheelZoomFactor);
    // it never pans. Guessing mouse-vs-touchpad from the delta
    // misread smooth-scroll mice as touchpads and slid the map
    // around instead of zooming, so there is no guess any more.
    function onWheel(e) {
      e.preventDefault();
      var p = pt(e);
      var dy = e.deltaY;
      if (e.deltaMode === 1) { dy *= 16; } else if (e.deltaMode === 2) { dy *= ch; }
      chaseZoom(p[0], p[1], wheelZoomFactor(dy, e.ctrlKey));
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
          var opts = [].filter.call(el.querySelectorAll("[data-map-layer]"), function (b) { return !b.disabled && !b.classList.contains("off"); })
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

    // The Share button: copy a link to the map exactly as it
    // stands -- layer and filters, route, selected system, and
    // the centre and zoom on screen (syncURL writes them all).
    // Nothing is stripped at copy time: data that is not public
    // is checked when the link is opened, as it already is (your
    // data and intel are only ever served to readers entitled to
    // them), so alliancemates opening the same link see their
    // own view of it while anyone else sees the public map.
    function shareMap() {
      syncURL();
      var url = location.href;
      function done() { hint("Map link copied."); }
      function fallback() {
        var ta = root.createElement("textarea");
        ta.value = url;
        ta.setAttribute("readonly", "");
        ta.style.position = "absolute";
        ta.style.opacity = "0";
        root.body.appendChild(ta);
        ta.select();
        var ok = false;
        try { ok = root.execCommand && root.execCommand("copy"); } catch (e) { /* no clipboard */ }
        ta.remove();
        if (ok) done(); else window.prompt("Copy this map link:", url);
      }
      if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(url).then(done, fallback);
      else fallback();
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
        fitNext = true;
        replan();
        return;
      }
      var tv = t.closest("[data-route-travel]");
      if (tv) {
        travel = tv.getAttribute("data-route-travel") === "jump" ? "jump" : "gates";
        if (travel === "jump") loadJump();
        routeNote("");
        fitNext = true;
        replan();
        return;
      }
      // A route choice that finds no better way says so, or the
      // button would seem to do nothing.
      var rm = t.closest("[data-route-mode]");
      if (rm && ROUTE_MODES[rm.getAttribute("data-route-mode")]) {
        var was = routeKey(), wasMode = routeMode;
        routeMode = rm.getAttribute("data-route-mode");
        routeNote("");
        fitNext = true;
        replan();
        if (routeMode !== wasMode && was && routeKey() === was) {
          routeNote("Same route: this is already the best way for that choice.");
        }
        return;
      }
      var rd = t.closest("[data-route-send]");
      if (rd && !rd.disabled) { sendRoute(rd.getAttribute("data-route-send")); return; }
      var npcT = t.closest("[data-map-sov-npc]");
      if (npcT) {
        sovNPC = npcT.checked;
        try { localStorage.setItem("evesynapse-map-sov-npc", sovNPC ? "1" : "0"); } catch (err) { /* private mode */ }
        // No cache clear: each NPC setting is its own cached
        // field (see sovKey), the other built while idle, so the
        // toggle is instant.
        syncURL();
        schedule();
        return;
      }
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
      if (t.closest("[data-map-data]")) { toggleDataMenu(); return; }
      if (t.closest(".map-data-menu")) return;
      if (t.closest("[data-map-shot]")) { takeShot(); return; }
      if (t.closest("[data-map-share]")) { shareMap(); return; }
      if (t.closest("[data-map-intel]")) {
        intelOn = !intelOn;
        try { localStorage.setItem("evesynapse-map-intel", intelOn ? "1" : "0"); } catch (err) { /* private mode */ }
        intelSync();
        return;
      }
      var lb = t.closest("[data-map-layer]");
      if (lb && lb.classList.contains("off")) { hint("A data heat layer is on: turn it off under Data to use this layer."); return; }
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
      // A shared view: the exact centre and zoom it was copied at.
      // A shared route that will fly to fit itself (below) wins.
      // One whose centre lies outside what is showing (a New Eden
      // view under a region's name) would open on empty space: it is
      // ignored, and the region or system opens as it would without.
      var vw = parseView(qs.get("view"));
      if (vw && viewFits(vw, viewBounds())) { stopMotion(); cam.x = vw.x; cam.y = vw.y; cam.k = clampK(fitK() * vw.f); }
      // A shared route: its ends and preference.
      var rf = G.byID[+qs.get("from")], rt = G.byID[+qs.get("to")];
      if (ROUTE_MODES[qs.get("route")]) routeMode = qs.get("route");
      if (rf !== undefined) routeFrom = rf;
      if (rt !== undefined && rt !== rf) routeTo = rt;
      // A shared route with no system asked for opens showing all of it.
      fitNext = sys === undefined && ri === undefined;
      if (routeFrom >= 0 || routeTo >= 0) replan();
      updateCard();
    }

    // --- Wiring and teardown ---
    readColors();
    statusEl.hidden = true;
    findInput.disabled = false;
    el.querySelectorAll("[data-map-zoom], [data-map-keys], [data-map-full], [data-map-shot], [data-map-share]").forEach(function (b) { b.disabled = false; });
    root.addEventListener("fullscreenchange", onFullChange);
    root.addEventListener("webkitfullscreenchange", onFullChange);
    // The layer scale: the map follows the slider as it moves; the
    // legend redraws (threat's level swatches) once it is let go.
    el.addEventListener("input", function (e) {
      if (!e.target.hasAttribute || !e.target.hasAttribute("data-map-gain")) return;
      var gk = dataHeat() || layer;
      if (!DATA_HEAT[gk] && !LY) return;
      heatGain[gk] = +e.target.value;
      try { localStorage.setItem(GAIN_KEY, JSON.stringify(heatGain)); } catch (err) { /* private mode */ }
      var top = el.querySelector(".map-heat-top");
      if (top) top.textContent = " " + heatTop(gk);
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
    if (projPref) {
      projPref.disabled = false;
      projPref.checked = projOn;
      projPref.addEventListener("change", function () {
        projOn = projPref.checked;
        // The one place the preference itself changes: it is
        // remembered from here, across sessions, until the
        // reader changes it again.
        try { window.localStorage.setItem(PROJ_KEY, projOn ? "1" : "0"); } catch (e) { /* private mode */ }
        syncURL();
        schedule();
        hint(projOn
          ? "Improved projection is on: the universe in the game's own 2D layout. It stays on until you turn it off."
          : "Improved projection is off: the universe at its true positions. It stays off until you turn it on.");
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
    // Other classes come and go on the root too (full screen, the
    // nav); only a real change of colors throws the territory away.
    function colorSig() { return [colors.npc, colors.sovL, colors.label, colors.halo].join("|"); }
    var lastColors = colorSig();
    var mo = new MutationObserver(function () {
      readColors();
      var sig = colorSig();
      if (sig === lastColors) return;
      lastColors = sig;
      sovFields = {};
      renderLegend();
      schedule();
    });
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
    // Your data: what the URL or last visit had on.
    syncDataMenu();
    if (signedIn && dataAny()) {
      fetchMine();
      mineTimer = setInterval(function () { if (document.visibilityState === "visible") fetchMine(); }, 60000);
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
      clearInterval(mineTimer);
      root.removeEventListener("click", onDataOutside);
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
    module.exports = { secRounded: secRounded, secTier: secTier, secText: secText, gateClass: gateClass, gateBatch: gateBatch, prepare: prepare, regionLabelAlpha: regionLabelAlpha, labelTier: labelTier, midTierLabels: midTierLabels, boxesOverlap: boxesOverlap, LABEL_MID_CAP: LABEL_MID_CAP, zoomEntryAllowed: zoomEntryAllowed, wheelZoomFactor: wheelZoomFactor, pickReach: pickReach, pickScore: pickScore, routeFitScale: routeFitScale, formatView: formatView, parseView: parseView, viewFits: viewFits, sovPalette: sovPalette, territoryField: territoryField, territoryLoops: territoryLoops,
      joinLayers: joinLayers, heatT: heatT, allianceHue: allianceHue, threatLevel: threatLevel,
      routeCost: routeCost, planRoute: planRoute, routeSummary: routeSummary, joinIntel: joinIntel,
      jumpRange: jumpRange, joinJump: joinJump, jumpNeighbours: jumpNeighbours, jumpFatigue: jumpFatigue };
  }
})();
