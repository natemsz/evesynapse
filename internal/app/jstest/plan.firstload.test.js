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
    querySelector: function () { return null; },
    querySelectorAll: function () { return []; },
    focus: function () {}
  };
}

let fetchCalls = 0;

function install(payload, opts) {
  opts = opts || {};
  const els = {};
  fetchCalls = 0;
  if (opts.embed) {
    // The page as the server renders it: the plan data rides in
    // a JSON data block.
    els["plan-data"] = makeEl();
    els["plan-data"].textContent = JSON.stringify(payload);
  }
  const doc = {
    readyState: "complete",
    title: "probe",
    getElementById: function (id) {
      // A page that lacks an element answers null, as a browser
      // does. Without this the stub conjures every element asked
      // for and can never show what a missing one breaks.
      if (opts.missing && opts.missing.indexOf(id) !== -1) return null;
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
    matchMedia: opts.matchMedia || function () { return { matches: false }; },
    scrollTo: function () {}
  };
  global.document = doc;
  global.window = win;
  global.fetch = function () {
    fetchCalls++;
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
    landed: 155000, value: 200000, valueKnown: true, profit: 45000,
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
  // The buy list is a real table with a grand total, not a
  // wrapped run-on list.
  assert.ok(els["plan-buylist"].innerHTML.indexOf("plan-buytable") !== -1, "buylist is a table");
  assert.ok(els["plan-buylist"].innerHTML.indexOf("<tfoot>") !== -1, "buylist has a total row");
  // Decision-map cards: state chip on the face, and the root
  // (1h of a 2h elapsed) trips the bottleneck flag.
  assert.ok(els["plan-map"].innerHTML.indexOf("plan-chip") !== -1, "map cards carry a state chip");
  assert.ok(els["plan-map"].innerHTML.indexOf("BOTTLENECK") !== -1, "map flags the bottleneck");
});

test("embedded plan data renders every view without a second request", async function () {
  const els = install(capitalPayload(), { embed: true });
  await waitFor(function () {
    return els["plan-map"] && els["plan-map"].innerHTML.indexOf("plan-node") !== -1;
  }, "map render");
  for (const id of Object.keys(rendered)) {
    assert.ok(els[id] && els[id].innerHTML.indexOf(rendered[id]) !== -1, id + " rendered");
  }
  assert.strictEqual(fetchCalls, 0, "the plan is not requested again");
});

test("a page without the data block falls back to the JSON route", async function () {
  const els = install(capitalPayload());
  await waitFor(function () {
    return els["plan-map"] && els["plan-map"].innerHTML.indexOf("plan-node") !== -1;
  }, "map render");
  assert.strictEqual(fetchCalls, 1, "one request for the plan");
});

test("decision map renders as stage columns by depth", async function () {
  const els = install(capitalPayload());
  await waitFor(function () {
    return els["plan-map"] && els["plan-map"].innerHTML.indexOf("plan-node") !== -1;
  }, "map render");
  const map = els["plan-map"].innerHTML;
  // Columnar, like the mockup: one stage per depth, root first.
  assert.ok(map.indexOf("plan-map-cols") !== -1, "map is columnar");
  const s0 = map.indexOf('data-depth="0"');
  const s1 = map.indexOf('data-depth="1"');
  assert.ok(s0 !== -1 && map.indexOf("Final assembly") !== -1, "root stage labeled Final assembly");
  assert.ok(s1 !== -1 && map.indexOf("Components") !== -1, "second stage labeled Components");
  assert.ok(s0 < s1, "stages run shallow to deep, left to right");
  // Branches start collapsed: depth 2 exists in the payload but
  // must not render until its parent is expanded.
  assert.ok(map.indexOf('data-depth="2"') === -1, "no third stage before any expansion");
  assert.ok(map.indexOf("Sub 0.0") === -1, "collapsed subs are not rendered");
  // Cards know their parent (the map's connective tissue), and a
  // crowded stage switches to the dense card size.
  assert.ok(map.indexOf('data-parent="0"') !== -1, "component cards name the root as parent");
  assert.ok(map.indexOf("plan-stage-dense") !== -1, "the 30-card stage renders dense");
});

test("narrow screens keep the classic indented card stack", async function () {
  const els = install(capitalPayload(), {
    matchMedia: function () { return { matches: true, addEventListener: function () {} }; }
  });
  await waitFor(function () {
    return els["plan-map"] && els["plan-map"].innerHTML.indexOf("plan-node") !== -1;
  }, "map render");
  const map = els["plan-map"].innerHTML;
  assert.ok(map.indexOf("plan-map-cols") === -1, "no stage columns on a narrow screen");
  assert.ok(map.indexOf("plan-stage") === -1, "no stage markup in list mode");
  assert.ok(map.indexOf("plan-kids") !== -1, "children nest in the indenting wrapper in list mode");
  assert.ok(map.indexOf("data-pad-left") === -1, "no content padding carries depth in list mode");
  assert.ok(map.indexOf("plan-chip") !== -1, "list cards keep their state chips");
});

test("PI leaf cards carry the origin line and the PI planner link", async function () {
  const payload = capitalPayload();
  const leaf = payload.root.kids[0];
  leaf.build = false;
  leaf.origin = "pi";
  leaf.originTier = "P2";
  const els = install(payload);
  await waitFor(function () {
    return els["plan-map"] && els["plan-map"].innerHTML.indexOf("plan-node") !== -1;
  }, "map render");
  const map = els["plan-map"].innerHTML;
  assert.ok(map.indexOf("Planetary interaction") !== -1, "origin line names PI");
  assert.ok(map.indexOf("P2") !== -1, "origin line carries the tier");
  assert.ok(map.indexOf("/industry/pi/?target=") !== -1, "origin line links to the PI planner recipe");
});

test("a plan that buys the product says so, with no saving line", async function () {
  const payload = capitalPayload();
  // Optimized buys the hull outright: its cost is the market
  // price, a margin of exactly zero.
  payload.root.delta = -500;
  payload.optimized = Object.assign({}, payload.optimized, {
    rootBought: true, materials: 200000, fees: 0, landed: 200000,
    bought: 200000, built: 0, buyValue: 200000, profit: 0, marginPct: 0,
    elapsed: 0, jobs: [], shopping: []
  });
  const els = install(payload);
  await waitFor(function () {
    return els["plan-map"] && els["plan-map"].innerHTML.indexOf("plan-node") !== -1;
  }, "map render");
  assert.strictEqual(els["plan-verdict"].textContent, "Buy it on the market: building costs more");
  assert.strictEqual(els["plan-verdict"].className, "plan-verdict buy");
  assert.strictEqual(els["plan-verdict"].hidden, false);
  assert.ok(els["plan-verdict"].textContent.indexOf("saves") === -1, "no saving line for a buy decision");
});

// The failure class this harness exists for: the script reaches
// for an element the page does not have, throws, and takes every
// view down with it.
test("missing optional elements leave every view rendered", async function () {
  const missing = [
    "plan-hubsplit", "plan-inspector", "plan-verdict", "plan-serverflag",
    "plan-landed-sub", "plan-margin", "plan-margin-sub", "plan-data",
    "plan-form", "me-toggle", "me-modal", "me-apply",
    "plan-settings-toggle", "plan-settings-modal",
    "judge-q", "judge-suggest", "system-q", "market-q", "market-value"
  ];
  const els = install(capitalPayload(), { missing: missing });
  await waitFor(function () {
    return els["plan-map"] && els["plan-map"].innerHTML.indexOf("plan-node") !== -1;
  }, "map render");
  for (const id of ["plan-map", "plan-gantt", "plan-buylist", "plan-fall"]) {
    assert.ok(els[id] && els[id].innerHTML.indexOf(rendered[id]) !== -1, id + " rendered");
    assert.ok(els[id].innerHTML.indexOf("need the plan data") === -1, id + " has no fail note");
    assert.ok(els[id].innerHTML.indexOf("failed to render") === -1, id + " did not throw");
  }
  // The buy list does not depend on the hub split beside it.
  assert.ok(els["plan-buylist"].innerHTML.indexOf("plan-buytable") !== -1, "buy list renders without the hub split");
  for (const id of missing) assert.ok(!els[id], id + " stayed missing");
});

test("a missing view container leaves the other views rendered", async function () {
  const views = ["plan-map", "plan-gantt", "plan-buylist", "plan-fall"];
  for (const gone of views) {
    const els = install(capitalPayload(), { missing: [gone] });
    // Views render in one pass, the cost flow last (the timeline
    // stands in when the cost flow is the one missing).
    const last = gone === "plan-fall" ? "plan-gantt" : "plan-fall";
    await waitFor(function () {
      return els[last] && els[last].innerHTML.indexOf(rendered[last]) !== -1;
    }, last + " render without " + gone);
    for (const id of views) {
      if (id === gone) continue;
      assert.ok(els[id] && els[id].innerHTML.indexOf(rendered[id]) !== -1, id + " rendered without " + gone);
      assert.ok(els[id].innerHTML.indexOf("failed to render") === -1, id + " did not throw without " + gone);
    }
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

test("verdict line, margin color and node chip toggles render", async function () {
  const els = install(capitalPayload());
  await waitFor(function () {
    return els["plan-map"] && els["plan-map"].innerHTML.indexOf("plan-node") !== -1;
  }, "map render");
  // The verdict above the presets: the optimized profit worded
  // as a saving, classed for the build color.
  assert.strictEqual(els["plan-verdict"].textContent, "Building saves 450.00 ISK");
  assert.strictEqual(els["plan-verdict"].className, "plan-verdict build");
  // Positive profit colors the margin (and its ISK/hour sub).
  assert.strictEqual(els["plan-margin"].className, "pos");
  assert.strictEqual(els["plan-margin-sub"].className, "pos");
  // Buildable non-root nodes get a chip BUTTON that swaps the
  // node's build/buy decision; the root keeps a plain chip.
  const map = els["plan-map"].innerHTML;
  assert.ok(map.indexOf('data-plan-toggle="0.0"') !== -1, "component chip is a toggle");
  assert.ok(map.indexOf('data-plan-toggle="0"') === -1, "root chip is not a toggle");
});

test("null preset lists (empty shopping/jobs marshal) still render first load", async function () {
  // Go marshals a nil slice as null, not []: a preset whose mask
  // leaves nothing to buy (or nothing to build) arrives with
  // shopping: null, and first load must degrade to the honest
  // empty states — not die into the fail note. Regression pin
  // for the capital first-load bug (crossCheck read .length on
  // the null inside setPayload, before any view could paint).
  const p = capitalPayload();
  p.optimized = Object.assign({}, p.optimized, { shopping: null, jobs: null });
  const els = install(p);
  await waitFor(function () {
    return els["plan-map"] && els["plan-map"].innerHTML.indexOf("plan-node") !== -1;
  }, "map render");
  for (const id of ["plan-map", "plan-gantt", "plan-buylist", "plan-fall", "plan-inspector"]) {
    assert.ok(els[id] && els[id].innerHTML.length > 0, id + " painted something");
    assert.ok(els[id].innerHTML.indexOf("need the plan data") === -1, id + " has no fail note");
  }
});
