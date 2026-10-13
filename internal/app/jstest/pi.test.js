"use strict";
// Live-coverage harness for pi.js: ticking planet checkboxes must
// recompute the coverage verdict and the missing-tile flags in
// place — the exact behavior whose absence left the server-rendered
// banner contradicting the map.
const assert = require("node:assert");
const test = require("node:test");

function makeTile(name, tier, planets, seedClasses) {
  const classes = new Set(seedClasses || []);
  return {
    classes,
    getAttribute(k) {
      if (k === "data-pi-node") return name;
      if (k === "data-pi-tier") return tier;
      if (k === "data-pi-planets") return planets || "";
      return null;
    },
    classList: {
      add(c) { classes.add(c); },
      remove(c) { classes.delete(c); },
      toggle(c, force) {
        if (force === undefined) force = !classes.has(c);
        if (force) classes.add(c); else classes.delete(c);
      },
      contains(c) { return classes.has(c); }
    }
  };
}

function makeBox(value, checked) {
  return {
    value, checked, handlers: {},
    addEventListener(ev, fn) { this.handlers[ev] = fn; },
    fire() { if (this.handlers.change) this.handlers.change(); }
  };
}

function install() {
  const tiles = [
    makeTile("Barren", "Planet"),
    makeTile("Temperate", "Planet"),
    makeTile("Autotrophs", "R0", "Temperate"),
    makeTile("Oxygen", "P1"),
    makeTile("Biocells", "P2"),
    makeTile("Broadcast Node", "P4", "", ["selected"])
  ];
  const byName = {};
  tiles.forEach(t => { byName[t.getAttribute("data-pi-node")] = t; });
  const boxes = [makeBox("Barren", true), makeBox("Temperate", false)];
  const verdict = { textContent: "", className: "pi-coverage neg" };
  const graph = {
    "Broadcast Node": { name: "Broadcast Node", inputs: ["Biocells"] },
    "Biocells": { name: "Biocells", inputs: ["Oxygen"] },
    "Oxygen": { name: "Oxygen", inputs: ["Autotrophs"] },
    "Autotrophs": { name: "Autotrophs", inputs: [] }
  };
  const graphData = { dataset: { graph: JSON.stringify(graph) } };
  const graphRoot = {
    addEventListener() {},
    querySelectorAll(sel) {
      if (sel === "[data-pi-node]") return tiles;
      if (sel === '[data-pi-tier="Planet"]') return tiles.filter(t => t.getAttribute("data-pi-tier") === "Planet");
      return [];
    },
    querySelector(sel) {
      if (sel === ".pi-node.selected") return tiles.find(t => t.classes.has("selected")) || null;
      return null;
    }
  };
  global.document = {
    getElementById(id) {
      if (id === "pi-graph-data") return graphData;
      if (id === "pi-graph") return graphRoot;
      if (id === "pi-product") return { value: "Broadcast Node" };
      if (id === "pi-coverage") return verdict;
      return null;
    },
    querySelectorAll(sel) {
      if (sel === 'input[name="planets"]') return boxes;
      if (sel === 'input[name="planets"]:checked') return boxes.filter(b => b.checked);
      return [];
    },
    querySelector() { return null; }
  };
  global.window = { location: { search: "" } };
  global.Event = class { constructor(type, opts) { this.type = type; this.opts = opts; } };
  const resolved = require.resolve("../static/pi.js");
  delete require.cache[resolved];
  require("../static/pi.js");
  return { tiles: byName, boxes, verdict };
}

test("coverage verdict and missing flags follow the checkboxes live", function () {
  const { tiles, boxes, verdict } = install();
  // Boot state: Barren ticked, Temperate not — Autotrophs (Temperate
  // only) is missing, and its P1 child flags with it.
  assert.match(verdict.textContent, /^Not covered — 0 of 1 raw resources available\. Missing: Autotrophs$/);
  assert.strictEqual(verdict.className, "pi-coverage neg");
  assert.ok(tiles["Autotrophs"].classes.has("missing"), "raw flags missing");
  assert.ok(tiles["Oxygen"].classes.has("missing"), "P1 flags with its raw");

  // Tick Temperate: verdict flips to covered, flags clear.
  boxes[1].checked = true;
  boxes[1].fire();
  assert.match(verdict.textContent, /^Covered — your 2 selected planet type\(s\) supply all 1 raw resources\.$/);
  assert.strictEqual(verdict.className, "pi-coverage pos");
  assert.ok(!tiles["Autotrophs"].classes.has("missing"), "raw flag clears");
  assert.ok(!tiles["Oxygen"].classes.has("missing"), "P1 flag clears");

  // Untick everything: neutral verdict, no flags.
  boxes[0].checked = false;
  boxes[1].checked = false;
  boxes[0].fire();
  assert.match(verdict.textContent, /^No planet types selected/);
  assert.strictEqual(verdict.className, "pi-coverage");
  assert.ok(!tiles["Autotrophs"].classes.has("missing"), "no flags with nothing selected");
});
