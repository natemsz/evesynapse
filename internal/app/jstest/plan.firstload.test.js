"use strict";
// First-load harness for plan.js: the interactive views must
// render from the fetched plan JSON with no click, at
// capital scale, and one view's failure must not blank the
// others. plan.js has no exports, so the harness stubs the DOM
// and fetch and drives init through require like the page does.
const assert = require("node:assert");
const test = require("node:test");

function makeEl() {
  return {
    innerHTML: "",
    textContent: "",
    hidden: false,
    setAttribute: function () {},
    getAttribute: function () { return null; },
    addEventListener: function () {},
    querySelectorAll: function () { return []; },
    focus: function () {}
  };
}

function install(payload, opts) {
  opts = opts || {};
  const els = {};
  const doc = {
    readyState: "complete",
    title: "probe",
    getElementById: function (id) {
      if (!els[id]) {
        els[id] = makeEl();
        if (opts.throwOnMap && id === "plan-map") {
          els[id].querySelectorAll = function () { throw new Error("map bind blew up"); };
        }
      }
      return els[id];
    },
    querySelectorAll: function () { return []; },
    addEventListener: function () {}
  };
  const win = {
    location: { href: "https://x.test/planner/?product=1", search: "?product=1", pathname: "/planner/" },
    history: {},
    matchMedia: function () { return { matches: false }; },
    scrollTo: function () {}
  };
  global.document = doc;
  global.window = win;
  global.fetch = function () {
    return Promise.resolve({ ok: true, json: function () { return Promise.resolve(payload); } });
  };
  const resolved = require.resolve("../static/plan.js");
  delete require.cache[resolved];
  require("../static/plan.js");
  return els;
}

function waitFor(cond, what) {
  const deadline = Date.now() + 15000;
  return new Promise(function (resolve, reject) {
    (function poll() {
      let ok = false;
      try { ok = cond(); } catch (e) { return reject(e); }
      if (ok) return resolve();
      if (Date.now() > deadline) return reject(new Error("timed out waiting for " + what));
      setTimeout(poll, 25);
    })();
  });
}

// Capital-scale fixture: 30 components x 12 sub-assemblies x 6
// leaves (2551 nodes), DTO-shaped like the server payload.
function leafNode(id) {
  return {
    id: id, name: "Leaf " + id, depth: 3, build: false,
    need: 10, have: 0, short: 10, runs: 0, produced: 0, surplus: 0,
    me: 0, te: 0, meSrc: "", timeSec: 0, materials: 50, complete: true,
    unit: 5, known: true, buy: 50, job: 0, jobKnown: true,
    save: 0, saveKnown: true, delta: -10, deltaOn: true,
    cycle: false, capped: false
  };
}

function buildNode(id, name, depth, kids, timeSec) {
  return {
    id: id, name: name, depth: depth, build: true,
    need: 1, have: 0, short: 1, runs: 1, produced: 1, surplus: 0,
    me: 0, te: 0, meSrc: "", timeSec: timeSec, materials: 500, complete: true,
    unit: 5000, known: true, buy: 9000, job: 100, jobKnown: true,
    save: 0, saveKnown: true, delta: 100, deltaOn: true,
    cycle: false, capped: false, kids: kids
  };
}

function capitalPayload() {
  let next = 100000;
  const leaves = [];
  const subs = [];
  const comps = [];
  for (let c = 0; c < 30; c++) {
    const ckids = [];
    for (let s = 0; s < 12; s++) {
      const skids = [];
      for (let l = 0; l < 6; l++) skids.push(leafNode(next++));
      const sub = buildNode(next++, "Sub " + c + "." + s, 2, skids, 60);
      subs.push(sub);
      ckids.push(sub);
    }
    comps.push(buildNode(20000 + c, "Comp " + c, 1, ckids, 600));
  }
  const root = buildNode(10001, "Cap Hull", 0, comps, 3600);
  root.materials = 200000;
  root.job = 5000;
  const totals = {
    materials: 150000, fees: 5000, feesKnown: true, complete: true,
    unpriced: 0, buyValue: 150000, bought: 90000, built: 60000,
    landed: 155000, value: 200000, known: true, profit: 45000,
    profKnown: true, marginPct: 22.5, elapsed: 7200,
    jobs: [{ id: 10001, name: "Cap Hull", start: 0, end: 3600, slot: 1, critical: true }],
    shopping: [{ id: 90001, name: "Cap Leaf", qty: 60, unit: 5, known: true, cost: 300 }]
  };
  return {
    product: 10001, slots: 5, marketName: "Jita", stockVal: 0,
    root: root, full: totals, optimized: totals, buyComp: totals
  };
}

const rendered = {
  "plan-map": "plan-node",
  "plan-gantt": "plan-gbar",
  "plan-buylist": "plan-copy",
  "plan-fall": "plan-frow",
  "plan-inspector": "Node inspector"
};

test("capital-scale plan renders every view on first load", async function () {
  const els = install(capitalPayload());
  await waitFor(function () {
    return els["plan-map"] && els["plan-map"].innerHTML.indexOf("plan-node") !== -1;
  }, "map render");
  for (const id of Object.keys(rendered)) {
    assert.ok(els[id] && els[id].innerHTML.indexOf(rendered[id]) !== -1, id + " rendered");
    assert.ok(els[id].innerHTML.indexOf("need the plan data") === -1, id + " has no fail note");
  }
});

test("one throwing view does not blank the others", async function () {
  const els = install(capitalPayload(), { throwOnMap: true });
  await waitFor(function () {
    return els["plan-gantt"] && els["plan-gantt"].innerHTML.indexOf("plan-gbar") !== -1;
  }, "gantt render despite map failure");
  assert.ok(els["plan-map"].innerHTML.indexOf("failed to render") !== -1, "map shows its failure note");
  for (const id of ["plan-gantt", "plan-buylist", "plan-fall", "plan-inspector"]) {
    assert.ok(els[id] && els[id].innerHTML.indexOf(rendered[id]) !== -1, id + " still rendered");
  }
});
