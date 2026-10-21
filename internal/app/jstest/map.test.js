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
