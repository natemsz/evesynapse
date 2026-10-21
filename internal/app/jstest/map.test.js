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
