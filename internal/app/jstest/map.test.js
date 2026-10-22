"use strict";
// map.js's pure parts: security as EVE shows it (core_linkfuncs.go
// secRound, mirrored), and the graph the map derives from its payload
// -- adjacency, each region's exits to its neighbours, and region
// bounds -- on a three-region toy universe.
const assert = require("node:assert");
const test = require("node:test");
const path = require("node:path");

const map = require(path.join(__dirname, "..", "static", "map.js"));

test("security rounds and colors like the server", () => {
  const cases = [
    [1.0, "1.0", 10], [0.45, "0.5", 5], [0.449, "0.4", 4],
    [0.04, "0.1", 1], [0.0, "0.0", 0], [-0.04, "0.0", 0], [-0.46, "-0.5", 0]
  ];
  for (const [s, text, tier] of cases) {
    assert.strictEqual(map.secText(s), text, "text for " + s);
    assert.strictEqual(map.secTier(s), tier, "tier for " + s);
  }
});

test("prepare derives adjacency, exits and bounds", () => {
  const P = {
    v: 1,
    regions: [
      { id: 10, name: "Alpha", x: 0, y: 0, w: 1000, h: 1000 },
      { id: 20, name: "Beta", x: 0, y: 0, w: 1000, h: 1000 },
      { id: 30, name: "Gamma", x: 0, y: 0, w: 1000, h: 1000 }
    ],
    cons: [{ id: 1, name: "A1", r: 0 }, { id: 2, name: "B1", r: 1 }, { id: 3, name: "G1", r: 2 }],
    sys: {
      id: [101, 102, 201, 301], n: ["Aone", "Atwo", "Bone", "Gone"],
      x: [0, 100, 500, 900], y: [0, 50, 500, 900], s: [0.9, 0.5, -0.2, 0.3],
      r: [0, 0, 1, 2], c: [0, 0, 1, 2], rx: [0, 1000, 500, 500], ry: [0, 500, 500, 500],
      f: [0, 0, 0, 0]
    },
    gates: [0, 1, 1, 2, 2, 3]
  };
  const G = map.prepare(P);
  assert.deepStrictEqual(G.adj.map((a) => a.slice().sort()), [[1], [0, 2], [1, 3], [2]]);
  // Alpha leaves through Atwo to Bone; Beta both ways; Gamma back to Beta.
  assert.deepStrictEqual(G.exits[0], [[1, 2]]);
  assert.deepStrictEqual(G.exits[1].sort(), [[2, 1], [2, 3]]);
  assert.deepStrictEqual(G.exits[2], [[3, 2]]);
  assert.deepStrictEqual(G.bounds[0], [0, 0, 100, 50]);
  assert.strictEqual(G.byID[201], 2);
  assert.strictEqual(G.regionByID[30], 2);
  assert.strictEqual(map.prepare(P), G, "derived once per payload");
});

test("prepare carries the projected frame, falling back to the true one", () => {
  const base = {
    v: 2,
    regions: [{ id: 10, name: "Alpha", x: 50, y: 25, px: 60, py: 30, w: 1000, h: 1000 }],
    cons: [],
    sys: {
      id: [101, 102], n: ["One", "Two"], x: [0, 100], y: [0, 50],
      px: [10, 130], py: [5, 80],
      s: [0.9, 0.5], r: [0, 0], c: [-1, -1], rx: [0, 1000], ry: [0, 500], f: [0, 0]
    },
    gates: [0, 1]
  };
  const G = map.prepare(base);
  assert.deepStrictEqual(G.px, [10, 130]);
  assert.deepStrictEqual(G.py, [5, 80]);
  assert.deepStrictEqual(G.pbounds[0], [10, 5, 130, 80], "projected region bounds");
  assert.strictEqual(G.projW, 130);
  // An older payload without the projected frame: true positions stand in.
  const old = JSON.parse(JSON.stringify(base));
  delete old.sys.px; delete old.sys.py; delete old._g;
  const G2 = map.prepare(old);
  assert.deepStrictEqual(G2.px, [0, 100]);
  assert.deepStrictEqual(G2.pbounds[0], [0, 0, 100, 50]);
});

test("region names stay visible and zoom entry needs the preference", () => {
  // Full strength while region names are the picture, then a
  // floor: however deep the zoom, the name stays readable (and
  // clickable) instead of fading away as it used to.
  assert.strictEqual(map.regionLabelAlpha(1), 1);
  assert.strictEqual(map.regionLabelAlpha(6), 1);
  assert.ok(map.regionLabelAlpha(12) < 1 && map.regionLabelAlpha(12) >= 0.55,
    "thinned but present at f=12, got " + map.regionLabelAlpha(12));
  assert.strictEqual(map.regionLabelAlpha(60), 0.55, "floor at deep zoom");
  // Clicking a name is the default way in; zooming in enters a
  // region only when the preference is on.
  assert.strictEqual(map.zoomEntryAllowed(false), false);
  assert.strictEqual(map.zoomEntryAllowed(undefined), false);
  assert.strictEqual(map.zoomEntryAllowed(true), true);
});

test("system names are rationed by zoom tier", () => {
  // Tier 0: region names are the picture, no system names.
  assert.strictEqual(map.labelTier(1), 0);
  assert.strictEqual(map.labelTier(6), 0, "the widest tier runs to the old gate");
  // Tier 1: a few regions fill the view, prominent systems only.
  assert.strictEqual(map.labelTier(6.5), 1);
  assert.strictEqual(map.labelTier(24), 1);
  // Tier 2: a single region or less, every system in view named.
  assert.strictEqual(map.labelTier(24.5), 2);
  assert.strictEqual(map.labelTier(60), 2, "the deepest universe zoom");
});

test("the middle tier names the front of the precedence line, capped", () => {
  const line = Array.from({ length: 300 }, (_, i) => i);
  const picked = map.midTierLabels(line, map.LABEL_MID_CAP);
  assert.strictEqual(picked.length, 120, "the cap is the tier's bound");
  assert.deepStrictEqual(picked, line.slice(0, 120), "best first, order kept");
  assert.deepStrictEqual(map.midTierLabels([5, 4], map.LABEL_MID_CAP), [5, 4],
    "under the cap the line passes through");
});

test("label collision: greedy in precedence order, touching is not overlap", () => {
  assert.strictEqual(map.boxesOverlap(0, 0, 50, 10, [25, 5, 50, 10]), true, "crossing boxes collide");
  assert.strictEqual(map.boxesOverlap(0, 0, 50, 10, [10, 2, 10, 4]), true, "a box inside another collides");
  assert.strictEqual(map.boxesOverlap(0, 0, 50, 10, [50, 0, 50, 10]), false, "edges that touch do not collide");
  assert.strictEqual(map.boxesOverlap(0, 0, 50, 10, [60, 30, 50, 10]), false, "apart is apart");
  // The placement rule itself: walk in precedence order, a name
  // is skipped when its box would land on one already placed.
  const placed = [], skipped = [];
  const greedy = (boxes) => {
    const kept = [];
    for (const b of boxes) {
      if (kept.some((o) => map.boxesOverlap(b[0], b[1], b[2], b[3], o))) skipped.push(b);
      else { kept.push(b); placed.push(b); }
    }
    return kept;
  };
  const order = [[0, 0, 50, 10], [25, 5, 50, 10], [100, 0, 50, 10], [110, 2, 50, 10]];
  assert.deepStrictEqual(greedy(order), [[0, 0, 50, 10], [100, 0, 50, 10]],
    "the first in line wins its ground; the overlapping later names skip");
  assert.strictEqual(skipped.length, 2);
});

test("label precedence follows the payload's prominence, degree before it", () => {
  const base = {
    v: 3,
    regions: [{ id: 10, name: "Alpha", x: 0, y: 0, w: 1000, h: 1000 }],
    cons: [],
    sys: {
      id: [101, 102, 103], n: ["Hub", "Quiet", "Junction"],
      x: [0, 10, 20], y: [0, 0, 0], s: [0.9, 0.9, 0.9],
      r: [0, 0, 0], c: [-1, -1, -1], rx: [0, 500, 1000], ry: [0, 0, 0], f: [0, 0, 0]
    },
    gates: [0, 1, 0, 2]
  };
  // Degrees: Hub 2, Quiet 1, Junction 1 (Quiet first on the ID tie).
  assert.deepStrictEqual(map.prepare(base).prio, [0, 1, 2], "no prominence array: gate degree, then ID");
  const withProm = JSON.parse(JSON.stringify(base));
  withProm.sys.p = [0, 500, 130];
  delete withProm._g;
  assert.deepStrictEqual(map.prepare(withProm).prio, [1, 2, 0],
    "prominence outranks degree: the quiet station system leads, the bare hub of gates last");
});

test("live layers join onto the map's systems", () => {
  const P = {
    v: 1,
    regions: [{ id: 10, name: "Alpha", x: 0, y: 0, w: 1000, h: 1000 }],
    cons: [{ id: 1, name: "A1", r: 0 }],
    sys: {
      id: [101, 102, 103], n: ["One", "Two", "Three"], x: [0, 10, 20], y: [0, 0, 0],
      s: [0.9, -0.3, -0.5], r: [0, 0, 0], c: [0, 0, 0], rx: [0, 500, 1000], ry: [0, 0, 0], f: [0, 0, 0]
    },
    gates: [0, 1, 1, 2]
  };
  const L = {
    kills: { at: 1, id: [102, 999], ship: [3, 9], pod: [1, 9], npc: [0, 9] },
    jumps: { at: 1, id: [101], n: [500] },
    sov: { at: 1, id: [102, 103], a: [0, -1], f: [0, 500010] },
    alliances: [{ id: 99003581, name: "Fraternity." }],
    fw: { at: 1, id: [101], occ: [500001], own: [500004], state: ["vulnerable"], pct: [100] },
    inc: { at: 1, list: [{ cons: 1, staging: 103, sys: [102, 103], state: "established", inf: 0.5, boss: false, f: 500019 }] },
    factions: [{ id: 500001, name: "Caldari State" }]
  };
  const J = map.joinLayers(P, L);
  assert.deepStrictEqual(Array.from(J.kills), [0, 4, 0], "ship + pod, unknown systems dropped");
  assert.strictEqual(J.max.kills, 4);
  assert.strictEqual(J.max.jumps, 500);
  assert.deepStrictEqual(Array.from(J.sovA), [-1, 0, -1]);
  assert.strictEqual(J.sovF[2], 500010);
  assert.deepStrictEqual(J.allianceMembers, [[1]]);
  assert.strictEqual(J.fwState[0], 3, "vulnerable");
  assert.deepStrictEqual(Array.from(J.inc), [0, 1, 2], "infested, and staging wins");
  assert.strictEqual(J.factions[500001], "Caldari State");
  assert.strictEqual(map.heatT(0, 10), 0);
  assert.strictEqual(map.heatT(10, 10), 1);
  assert.ok(map.heatT(1, 1000) > 0 && map.heatT(1, 1000) < 0.2, "log scale keeps a lone kill visible but faint");
  assert.notStrictEqual(map.allianceHue(99003581), map.allianceHue(99003582), "neighbouring IDs, different hues");
});

test("threat joins with levels, camps and spillover along the gates", () => {
  const P = {
    v: 1,
    regions: [{ id: 10, name: "Alpha", x: 0, y: 0, w: 1000, h: 1000 }],
    cons: [{ id: 1, name: "A1", r: 0 }],
    sys: {
      id: [101, 102, 103], n: ["One", "Two", "Three"], x: [0, 10, 20], y: [0, 0, 0],
      s: [0.9, -0.3, -0.5], r: [0, 0, 0], c: [0, 0, 0], rx: [0, 500, 1000], ry: [0, 0, 0], f: [0, 0, 0]
    },
    gates: [0, 1, 1, 2]
  };
  const J = map.joinLayers(P, {
    threat: { at: 1, id: [102], s: [600], k1: [4], last: [1000],
      camps: [{ sys: 102, n: 3, kind: "alliance", gid: 99, group: "The Campers", since: 900 }] }
  });
  assert.strictEqual(J.threat[1], 6);
  assert.strictEqual(J.threatHour[1], 4);
  assert.strictEqual(J.camps.length, 1);
  assert.strictEqual(J.campOf[1], 0);
  assert.ok(Math.abs(J.spill[0] - 1.8) < 1e-6 && Math.abs(J.spill[2] - 1.8) < 1e-6, "neighbours carry 30%");
  assert.strictEqual(J.spill[1], 0, "the hot system's own neighbours are quiet");
  assert.deepStrictEqual([0, 0.4, 0.5, 2, 5, 10, 40].map(map.threatLevel), [0, 0, 1, 2, 3, 4, 4]);
});

test("routes plan shortest first and bend around danger when asked", () => {
  // A - B - C is the short way; A - D - E - C detours. B is camped.
  const P = {
    v: 1,
    regions: [{ id: 10, name: "Alpha", x: 0, y: 0, w: 1000, h: 1000 }],
    cons: [],
    sys: {
      id: [101, 102, 103, 104, 105], n: ["A", "B", "C", "D", "E"],
      x: [0, 10, 20, 0, 20], y: [0, 0, 0, 10, 10],
      s: [0.9, 0.9, 0.9, 0.9, 0.9], r: [0, 0, 0, 0, 0], c: [-1, -1, -1, -1, -1],
      rx: [0, 500, 1000, 0, 1000], ry: [0, 0, 0, 500, 500], f: [0, 0, 0, 0, 0]
    },
    gates: [0, 1, 1, 2, 0, 3, 3, 4, 4, 2]
  };
  const G = map.prepare(P);
  const LY = { threat: new Float64Array(5), spill: new Float64Array(5), campOf: new Int32Array(5).fill(-1), camps: [{}] };
  LY.campOf[1] = 0;

  assert.deepStrictEqual(map.planRoute(G, 0, 2, map.routeCost(G, LY, "short")), [0, 1, 2],
    "shortest takes the camped gate");
  assert.deepStrictEqual(map.planRoute(G, 0, 2, map.routeCost(G, LY, "calm")), [0, 3, 4, 2],
    "calm pays jumps to skirt the camp");
  assert.strictEqual(map.planRoute(G, 0, 2, map.routeCost(G, null, "short")).length, 3,
    "no layer loaded still plans");

  const sum = map.routeSummary(G, LY, [0, 1, 2]);
  assert.strictEqual(sum.jumps, 2);
  assert.deepStrictEqual(sum.camps, [1], "the camp on the way is named");
  const calm = map.routeSummary(G, LY, [0, 3, 4, 2]);
  assert.strictEqual(calm.jumps, 3);
  assert.deepStrictEqual(calm.camps, []);

  // Intel: hostiles reported in B a minute ago, D reported then
  // cleared, E's report past its half hour.
  const now = 100000;
  const I = map.joinIntel(G, {
    channels: [{ id: 1, name: "Home", post: true }],
    reports: [
      { id: 1, ch: 1, sys: 102, n: 4, at: now - 60 },
      { id: 2, ch: 1, sys: 104, n: 2, at: now - 600 },
      { id: 3, ch: 1, sys: 104, n: 0, clear: true, at: now - 120 },
      { id: 4, ch: 1, sys: 105, n: 9, at: now - 4000 },
      { id: 5, ch: 1, sys: 999, n: 1, at: now }
    ]
  }, now);
  assert.strictEqual(I.hostile, 1, "one system with live hostiles");
  assert.strictEqual(I.clear, 1, "a newer clear clears a system");
  assert.ok(I.prices[1] > 150 && I.prices[3] === 0 && I.prices[4] === 0, "only fresh hostiles cost");
  assert.deepStrictEqual(I.bySys[3].map((r) => r.id), [3, 2], "a system's reports newest first");
  assert.strictEqual(I.bySys[4], undefined, "a report past its half hour is gone");
  // A report is not danger: no route steers around it, but a route
  // through it names it.
  assert.deepStrictEqual(map.planRoute(G, 0, 2, map.routeCost(G, null, "calm")), [0, 1, 2], "calm does not skirt reported systems");
  assert.deepStrictEqual(map.planRoute(G, 0, 2, map.routeCost(G, null, "safe")), [0, 1, 2], "nor does safer");
  assert.deepStrictEqual(map.routeSummary(G, null, [0, 1, 2], I.prices).reported, [1], "the route names the reported system");
});

test("jump drives: range, cyno-legal landings, routes and fatigue", () => {
  // Five systems on a line, 3 LY apart: A (high-sec) B C D (null), E in Pochven.
  const P = {
    v: 1,
    regions: [{ id: 10, name: "Alpha", x: 0, y: 0, w: 1000, h: 1000 }, { id: 10000070, name: "Pochven", x: 0, y: 0, w: 1000, h: 1000 }],
    cons: [],
    sys: {
      id: [201, 202, 203, 204, 205], n: ["A", "B", "C", "D", "E"],
      x: [0, 10, 20, 30, 40], y: [0, 0, 0, 0, 0],
      s: [0.9, -0.2, -0.3, -0.4, -0.5], r: [0, 0, 0, 0, 1], c: [-1, -1, -1, -1, -1],
      rx: [0, 1, 2, 3, 4], ry: [0, 0, 0, 0, 0], f: [0, 0, 0, 0, 0]
    },
    gates: [0, 1, 1, 2, 2, 3]
  };
  const G = map.prepare(P);
  const J = map.joinJump(G, {
    ships: [{ t: 1, n: "Ark", g: "Jump Freighter", r: 5, f: 0.1 }],
    id: [201, 202, 203, 204, 205], region: [10, 10, 10, 10, 10000070],
    x: [0, 3, 6, 9, 12], y: [0, 0, 0, 0, 0], z: [0, 0, 0, 0, 0]
  });
  assert.strictEqual(map.jumpRange(5, 5), 10, "JDC V doubles the range");
  assert.strictEqual(map.jumpRange(3.5, 0), 3.5);
  assert.deepStrictEqual(Array.from(J.dest), [0, 1, 1, 1, 0], "no cyno in high-sec or Pochven");
  assert.deepStrictEqual(map.jumpNeighbours(J, 0, 6).sort(), [1, 2], "within 6 LY, landable");
  const hop = (R) => (u) => map.jumpNeighbours(J, u, R);
  const two = map.planRoute(G, 0, 3, () => 1, hop(6));
  assert.strictEqual(two.length, 3, "fewest jumps: two (A-B-D and A-C-D tie)");
  assert.ok(two[0] === 0 && two[2] === 3 && J.dest[two[1]] === 1, "through a cyno-legal system");
  assert.deepStrictEqual(map.planRoute(G, 0, 3, () => 1, hop(3)), [0, 1, 2, 3], "a shorter range takes more cynos");
  assert.strictEqual(map.planRoute(G, 0, 4, () => 1, hop(10)), null, "nothing lands in Pochven");
  // Gates plus a one-way bridge from A to D.
  const bridged = (u) => (u === 0 ? G.adj[0].concat([3]) : G.adj[u]);
  assert.deepStrictEqual(map.planRoute(G, 0, 3, () => 1, bridged), [0, 3], "a jump bridge is a shortcut");
  assert.deepStrictEqual(map.planRoute(G, 3, 0, () => 1, bridged), [3, 2, 1, 0], "and one-way");
  // Fatigue, by CCP's rules.
  assert.deepStrictEqual(map.jumpFatigue([5], 1, 0), { fatigue: 60, waits: 0 });
  assert.deepStrictEqual(map.jumpFatigue([5, 5], 1, 0), { fatigue: 300, waits: 6 }, "capped at 5 hours");
  assert.deepStrictEqual(map.jumpFatigue([10], 0.1, 0), { fatigue: 20, waits: 0 }, "a jump freighter counts a tenth of the distance");
});

test("zoomed-out picking prefers the system with something on it", () => {
  // A bare system keeps the old 14px reach; a hot one reaches 24px.
  assert.strictEqual(map.pickReach(0), 14);
  assert.strictEqual(map.pickReach(1), 24);
  const dot = 3;
  // At the same distance the hot system wins; a somewhat farther
  // hot system (12px) still beats a bare neighbour (10px).
  assert.ok(map.pickScore(100, 1, dot) < map.pickScore(100, 0, dot));
  assert.ok(map.pickScore(144, 1, dot) < map.pickScore(100, 0, dot));
  // A system right under the pointer beats a hot one nearby.
  assert.ok(map.pickScore(4, 0, dot) < map.pickScore(20, 1, dot));
});

test("route fit zooms to the route's span in the free canvas", () => {
  // 100x50 world span into 1000x500 free px with 48px pad: the
  // tighter (vertical) side decides.
  assert.strictEqual(map.routeFitScale(100, 50, 1000, 500, 48), (500 - 96) / 50);
  assert.strictEqual(map.routeFitScale(0, 0, 1000, 500, 48), 404, "a point route is not divided by zero");
});

test("sov palette keeps holders' colors apart", () => {
  const pal = map.sovPalette([300, 200, 100, 50, 10]);
  // Indexed like the input, largest holder first.
  assert.strictEqual(pal.length, 5);
  assert.strictEqual(pal[0][0], 0, "the largest holder takes the first hue");
  const hues = pal.map((p) => p[0]);
  for (let a = 0; a < hues.length; a++) for (let b = a + 1; b < hues.length; b++) {
    const d = Math.min(Math.abs(hues[a] - hues[b]), 360 - Math.abs(hues[a] - hues[b]));
    assert.ok(d >= 30, "hues " + hues[a] + " and " + hues[b] + " are " + d + "deg apart");
  }
  // A crowded map: no two of a dozen holders share a near hue,
  // and repeats at least differ in lightness.
  const many = map.sovPalette(Array.from({ length: 12 }, (_, i) => 100 - i));
  for (let a = 0; a < 12; a++) for (let b = a + 1; b < 12; b++) {
    const d = Math.min(Math.abs(many[a][0] - many[b][0]), 360 - Math.abs(many[a][0] - many[b][0]));
    assert.ok(d >= 15 || many[a][1] !== many[b][1], "holders " + a + " and " + b + " too similar");
  }
});

test("share views round-trip centre and zoom", () => {
  assert.strictEqual(map.formatView(1234.4, -56.6, 2.34567), "1234,-57,2.346");
  assert.deepStrictEqual(map.parseView("1234,-57,2.346"), { x: 1234, y: -57, f: 2.346 });
  assert.deepStrictEqual(map.parseView(map.formatView(0, 0, 1)), { x: 0, y: 0, f: 1 });
  assert.strictEqual(map.parseView(""), null);
  assert.strictEqual(map.parseView("1,2"), null);
  assert.strictEqual(map.parseView("a,b,c"), null);
  assert.strictEqual(map.parseView("1,2,0"), null, "no zoom, no view");
  assert.strictEqual(map.parseView(null), null);
});

test("a shared view outside what is showing is ignored", () => {
  var region = [-60, -60, 1060, 860];
  assert.ok(map.viewFits({ x: 500, y: 425, f: 1 }, region));
  assert.ok(map.viewFits({ x: -200, y: 900, f: 1 }, region), "a little past the edge is fine");
  assert.ok(!map.viewFits({ x: 4212, y: 5507, f: 4.221 }, region), "a New Eden view under a region's name");
});

test("mousewheel zooms, never pans", () => {
  // Wheel up zooms in, wheel down zooms out, whatever the notch
  // size -- including the small, fractional deltas a smooth-scroll
  // mouse sends, which used to be misread as a touchpad and panned.
  assert.ok(map.wheelZoomFactor(-120, false) > 1, "wheel up zooms in");
  assert.ok(map.wheelZoomFactor(-33.3, false) > 1, "small wheel tick zooms in");
  assert.ok(map.wheelZoomFactor(120, false) < 1, "wheel down zooms out");
  assert.ok(map.wheelZoomFactor(33.3, false) < 1, "small wheel tick zooms out");
  assert.strictEqual(map.wheelZoomFactor(0, false), 1, "no delta, no zoom");
  // A pinch (ctrlKey wheel) zooms faster than a plain tick.
  assert.ok(map.wheelZoomFactor(-10, true) > map.wheelZoomFactor(-10, false), "pinch zooms faster");
});

test("territory outlines: round, split between holders, joined by a band", () => {
  const src = (list) => ({ o: list.map((p) => p[0]), x: list.map((p) => p[1]), y: list.map((p) => p[2]), s: list.map((p) => p[3]) });
  const B = [0, 0, 200, 100];
  // One source: one closed outline, about 1.55 sigma from it (where
  // the Gaussian falls to the 0.3 floor).
  let F = map.territoryField(src([[0, 50, 50, 10]]), B, 400);
  let loops = map.territoryLoops(F, 0, 0.3);
  assert.strictEqual(loops.length, 1);
  const r = loops[0].map((p) => Math.hypot(p[0] - 50, p[1] - 50));
  assert.ok(Math.min(...r) > 14 && Math.max(...r) < 17, "radius " + Math.min(...r) + ".." + Math.max(...r));
  // Two holders side by side: the border runs down the middle.
  F = map.territoryField(src([[0, 80, 50, 15], [1, 120, 50, 15]]), B, 400);
  const left = map.territoryLoops(F, 0, 0.3)[0], right = map.territoryLoops(F, 1, 0.3)[0];
  const atMid = (lp) => lp.filter((p) => Math.abs(p[1] - 50) < 2).map((p) => p[0]);
  assert.ok(Math.abs(Math.max(...atMid(left)) - 100) < 1.5, "left holder ends at the middle");
  assert.ok(Math.abs(Math.min(...atMid(right)) - 100) < 1.5, "right holder starts at the middle");
  // Two far clusters of one holder: apart alone, one shape with a band.
  const far = [[0, 30, 50, 8], [0, 170, 50, 8]];
  assert.strictEqual(map.territoryLoops(map.territoryField(src(far), B, 400), 0, 0.3).length, 2);
  const band = far.slice();
  for (let t = 1; t < 10; t++) band.push([0, 30 + 14 * t, 50, 24]);
  assert.strictEqual(map.territoryLoops(map.territoryField(src(band), B, 400), 0, 0.3).length, 1);
});
