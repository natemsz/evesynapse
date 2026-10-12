// plan.js — the build planner's client side. It fetches the plan
// JSON (?format=json), renders the four tabs, and recomputes on
// make/buy toggles without a round trip.
//
// The trust rule: this file duplicates no formula. Every figure
// it shows is a server figure or a sum of server figures, and the
// payload carries the server's canonical preset totals. One
// shared compute() path serves presets and custom masks alike;
// presets are verified against the server exactly (integer
// cents, whole seconds), and any mismatch locks the page to
// server totals with a visible flag. A client bug can only fall
// back, never invent a number.
//
// Markup rules (CSP): no inline styles or handlers anywhere.
// Geometry rides data-w / data-left attributes that app.js
// applies; all listeners attach here.

(function () {
"use strict";

var section = document.getElementById("plan");
if (!section) return;

function $(id) { return document.getElementById(id); }

// --- formatting (mirrors Go: iskCompactCents, humanDuration) ---
function iskC(cents) {
  var neg = cents < 0;
  var f = Math.abs(cents) / 100;
  var s;
  if (f >= 1e9) s = (f / 1e9).toFixed(2) + "B";
  else if (f >= 1e6) s = (f / 1e6).toFixed(2) + "M";
  else if (f >= 1e3) s = (f / 1e3).toFixed(2) + "K";
  else s = f.toFixed(2);
  return (neg ? "-" : "") + s + " ISK";
}

function intF(n) { return n.toLocaleString("en-US"); }

function dur(sec) {
  if (sec < 60) return "<1m";
  var days = Math.floor(sec / 86400);
  var hours = Math.floor((sec % 86400) / 3600);
  var mins = Math.floor((sec % 3600) / 60);
  if (days > 0) return days + "d " + hours + "h";
  if (hours > 0) return hours + "h " + mins + "m";
  return mins + "m";
}

function esc(s) {
  return String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;")
    .replace(/>/g, "&gt;").replace(/"/g, "&quot;");
}

// --- state ---
var P = null;             // payload
var preset = "balanced";  // balanced | full | buycomp
var overrides = {};       // path -> true (buy) | false (build)
var collapsed = {};       // path -> true
var selected = "0";       // path
var serverMode = false;   // cross-check failed: server figures only
var judgeDocWired = false; // one outside-click closer for every reboot

function planURL() {
  var sp = new URLSearchParams(window.location.search);
  sp.set("format", "json");
  return window.location.pathname + "?" + sp.toString();
}

// Assign stable index paths (root "0", kids "0.0"...) for masks.
function assignPaths(node, path) {
  node.path = path;
  (node.kids || []).forEach(function (k, i) { assignPaths(k, path + "." + i); });
}

function findNode(path) {
  var parts = path.split(".").slice(1).map(Number);
  var n = P.root;
  for (var i = 0; i < parts.length; i++) {
    n = (n.kids || [])[parts[i]];
    if (!n) return null;
  }
  return n;
}

// --- the one compute path (mirrors Go planRollup) ---
function decide(n, isRoot, mask) {
  if (n.short <= 0) return "have";
  if (!n.build) return "buy";
  if (mask.overrides.hasOwnProperty(n.path))
    return mask.overrides[n.path] ? "buy" : "build";
  if (mask.preset === "full") return "build";
  if (mask.preset === "buycomp") return isRoot ? "build" : "buy";
  // balanced
  return (n.deltaOn && n.delta < 0 && n.known) ? "buy" : "build";
}

function compute(mask) {
  var t = { materials: 0, fees: 0, feesKnown: true, complete: true,
    unpriced: 0, buyValue: 0, bought: 0, built: 0 };
  var shop = {};
  var jobs = [];
  function add(n, isRoot, built) {
    if (n.short <= 0) return null;
    var d = decide(n, isRoot, mask);
    if (d === "buy") {
      // Mirrors the server: the finished product never lands in
      // the shopping aggregation (its outright cost still
      // accrues to the money totals below).
      if (!isRoot) shop[n.id] = (shop[n.id] || 0) + n.short;
      if (n.known) {
        var c = n.build ? n.buy : n.materials;
        t.materials += c;
        t.buyValue += c;
        if (n.build || !built) t.bought += c;
        else t.built += c;
      } else { t.complete = false; t.unpriced++; }
      return null;
    }
    var job = { n: n, dur: n.timeSec, deps: [] };
    if (n.jobKnown) t.fees += n.job;
    else t.feesKnown = false;
    (n.kids || []).forEach(function (k) {
      var dep = add(k, false, true);
      if (dep) job.deps.push(dep);
    });
    jobs.push(job);
    return job;
  }
  add(P.root, true, false);
  t.landed = t.materials + t.fees;
  // product value and margin
  t.value = 0; t.known = false; t.profit = 0; t.profKnown = false; t.marginPct = 0;
  if (P.root.known) {
    t.value = P.root.unit * P.root.produced;
    t.known = true;
    if (t.complete && t.feesKnown && t.value > 0) {
      t.profit = t.value - t.landed;
      t.profKnown = true;
      t.marginPct = t.profit / t.value * 100;
    }
  }
  var sched = schedule(jobs, P.slots);
  t.jobs = sched.spans;
  t.elapsed = sched.elapsed;
  // shopping lines, name-sorted like the server
  t.shopping = Object.keys(shop).map(function (id) {
    return { id: Number(id), qty: shop[id] };
  });
  t.shopping.forEach(function (L) {
    var n = findFirst(L.id);
    L.name = n ? n.name : ("Type " + L.id);
    L.unit = n && n.known ? n.unit : 0;
    L.known = !!(n && n.known);
    L.cost = L.known ? L.unit * L.qty : 0;
  });
  t.shopping.sort(function (a, b) { return a.name < b.name ? -1 : a.name > b.name ? 1 : 0; });
  return t;
}

var byIdCache = {};
// Shopping lines price from the tree, but a covered leaf carries
// no price (the engine prices shortfalls only), so the first
// same-type node can be unpriced while a deeper twin is priced.
// Prefer a priced node: the server prices the same price map, so
// both sides then agree exactly and the cross-check holds.
function findFirst(id) {
  if (byIdCache[id]) return byIdCache[id];
  var hit = null, priced = null;
  (function walk(n) {
    if (!n) return;
    if (n.id === id) {
      if (!hit) hit = n;
      if (n.known) { priced = n; return; }
    }
    (n.kids || []).forEach(walk);
  })(P.root);
  byIdCache[id] = priced || hit;
  return priced || hit;
}

// Deterministic list scheduling (mirrors Go planSchedule).
function schedule(jobs, slots) {
  if (slots < 1) slots = 1;
  var ordered = jobs.slice().sort(function (a, b) {
    if (a.n.depth !== b.n.depth) return b.n.depth - a.n.depth;
    return a.n.id - b.n.id;
  });
  var free = [], end = new Map();
  for (var i = 0; i < slots; i++) free.push(0);
  var spans = [], makespan = 0;
  var at = new Map();
  ordered.forEach(function (job) {
    var ready = 0;
    job.deps.forEach(function (d) { ready = Math.max(ready, end.get(d) || 0); });
    var slot = 0;
    for (var s = 1; s < slots; s++) if (free[s] < free[slot]) slot = s;
    var start = Math.max(ready, free[slot]);
    var finish = start + job.dur;
    free[slot] = finish;
    end.set(job, finish);
    if (finish > makespan) makespan = finish;
    at.set(job, spans.length);
    spans.push({ id: job.n.id, name: job.n.name, start: start,
      end: finish, slot: slot, critical: false });
  });
  // critical path: longest chain back from the depth-0 job
  var longest = new Map();
  function chain(job) {
    if (longest.has(job)) return longest.get(job);
    var best = 0;
    job.deps.forEach(function (d) { best = Math.max(best, chain(d)); });
    longest.set(job, job.dur + best);
    return job.dur + best;
  }
  if (ordered.length) {
    var rootJob = null;
    ordered.forEach(function (job) { if (!rootJob && job.n.depth === 0) rootJob = job; });
    if (!rootJob) rootJob = ordered[0];
    chain(rootJob);
    var cur = rootJob;
    while (cur) {
      spans[at.get(cur)].critical = true;
      var next = null, best = -1;
      cur.deps.forEach(function (d) {
        if (longest.get(d) > best) { best = longest.get(d); next = d; }
      });
      cur = next;
    }
  }
  spans.sort(function (a, b) {
    return a.start !== b.start ? a.start - b.start : a.slot - b.slot;
  });
  return { spans: spans, elapsed: makespan };
}

// --- cross-check: presets must equal the server exactly ---
function sameLines(a, b) {
  if (a.length !== b.length) return false;
  for (var i = 0; i < a.length; i++) {
    var x = a[i], y = b[i];
    if (x.id !== y.id || x.qty !== y.qty || x.unit !== y.unit ||
        x.cost !== y.cost || x.known !== y.known) return false;
  }
  return true;
}

function sameSpans(a, b) {
  if (a.length !== b.length) return false;
  for (var i = 0; i < a.length; i++) {
    var x = a[i], y = b[i];
    if (x.id !== y.id || x.start !== y.start || x.end !== y.end ||
        x.slot !== y.slot || x.critical !== y.critical) return false;
  }
  return true;
}

function checkPreset(name, server) {
  var c = compute({ preset: name, overrides: {} });
  var keys = ["materials", "fees", "landed", "bought", "built",
    "unpriced", "value", "profit", "elapsed", "buyValue"];
  for (var i = 0; i < keys.length; i++) {
    if (c[keys[i]] !== server[keys[i]]) return false;
  }
  var flags = [["feesKnown", "feesKnown"], ["complete", "complete"],
    ["known", "valueKnown"], ["profKnown", "profKnown"]];
  for (var j = 0; j < flags.length; j++) {
    if (!!c[flags[j][0]] !== !!server[flags[j][1]]) return false;
  }
  if (!sameLines(c.shopping, server.shopping)) return false;
  if (!sameSpans(c.jobs, server.jobs)) return false;
  return true;
}

function crossCheck() {
  var ok = checkPreset("full", P.full) &&
    checkPreset("balanced", P.balanced) &&
    checkPreset("buycomp", P.buyComp);
  if (!ok) {
    serverMode = true;
    var flag = $("plan-serverflag");
    if (flag) flag.hidden = false;
  }
  return ok;
}

// --- current view: server structs in serverMode, compute() else ---
function serverTotals(name) {
  return name === "full" ? P.full : name === "buycomp" ? P.buyComp : P.balanced;
}

function currentMask() { return { preset: preset, overrides: overrides }; }

function hasOverrides() {
  for (var k in overrides) if (overrides.hasOwnProperty(k)) return true;
  return false;
}

function currentTotals() {
  if (serverMode || !hasOverrides()) return serverTotals(preset);
  return compute(currentMask());
}

// --- header ---
function setText(id, s) { var el = $(id); if (el) el.textContent = s; }

function renderHeader(t) {
  var landedSub = "materials";
  if (t.feesKnown && t.fees > 0) landedSub = "materials + fees";
  else if (!t.feesKnown) landedSub = "materials (fees unknown)";
  setText("plan-landed", t.complete ? iskC(t.landed) : "partial");
  setText("plan-landed-sub", t.complete ? landedSub : landedSub + " · some prices missing");
  setText("plan-value", t.known ? iskC(t.value) : "—");
  if (t.profKnown) {
    setText("plan-margin", (t.profit > 0 ? "+" : "") + iskC(t.profit));
    var sub = "";
    if (t.value > 0) sub = t.marginPct.toFixed(1) + "% of market value";
    if (t.elapsed > 0) {
      var perHour = t.profit / 100 / (t.elapsed / 3600);
      sub += (sub ? " · " : "") + iskC(Math.round(perHour * 100)) + "/hour";
    }
    setText("plan-margin-sub", sub);
  } else {
    setText("plan-margin", "—");
    setText("plan-margin-sub", "needs full prices");
  }
  setText("plan-elapsed", dur(t.elapsed));
  setText("plan-elapsed-sub", "critical path · " + P.slots + " slots");
}

// --- tabs and presets ---
function showTab(name) {
  ["tree", "time", "buy", "cost"].forEach(function (n) {
    var panel = $("plan-tab-" + n);
    if (panel) panel.hidden = n !== name;
  });
  section.querySelectorAll("[data-plan-tab]").forEach(function (btn) {
    var on = btn.getAttribute("data-plan-tab") === name;
    btn.setAttribute("aria-selected", on ? "true" : "false");
    if (on) btn.setAttribute("data-active", "1");
    else btn.removeAttribute("data-active");
  });
}

function setPreset(name) {
  preset = name;
  overrides = {};
  syncPresetButtons();
  renderAll();
}

// --- decision map ---
// Bottleneck: a built branch whose own job runs long — over six
// hours or over a sixth of the current elapsed time.
function bottleneck(n, elapsed) {
  if (!n.build || n.short <= 0) return false;
  return n.timeSec >= 6 * 3600 || (elapsed > 0 && n.timeSec >= elapsed / 6);
}

function nodeClass(n, mask, elapsed) {
  var d = decide(n, n.path === "0", mask);
  var cls = "plan-node";
  if (d === "have") cls += " plan-have";
  else if (d === "buy" || !n.build) cls += " plan-buy";
  else cls += " plan-build";
  if (d === "build" && bottleneck(n, elapsed)) cls += " plan-risk";
  if (n.path === selected) cls += " plan-sel";
  return cls;
}

function nodeTitle(n) {
  var parts = [n.name];
  if (n.build && n.short > 0) {
    parts.push("build " + iskC(n.materials) + " + " + iskC(n.job) + " fees");
    parts.push(n.runs + " runs · " + dur(n.timeSec));
    parts.push("ME" + n.me + "/TE" + n.te + " (" + n.meSrc + ")");
    if (n.deltaOn) parts.push(n.delta >= 0 ? "building saves " + iskC(n.delta) : "buying saves " + iskC(-n.delta));
    if (n.saveKnown && n.save > 0 && n.me < 10)
      parts.push("research to ME10 saves " + iskC(n.save));
  } else if (n.short > 0) {
    parts.push("buy " + intF(n.short) + (n.known ? " × " + iskC(n.unit) : ""));
  } else {
    parts.push("covered by stock");
  }
  return parts.join(" · ");
}

function nodeCost(n, mask) {
  var d = decide(n, n.path === "0", mask);
  if (d === "have") return 0;
  var buying = !n.build || d === "buy";
  if (buying && n.build) return n.known ? n.buy : n.materials;
  return n.build ? n.materials + n.job : n.materials;
}

// Hidden descendants under a collapsed branch: the +N label.
function subtreeSize(n) {
  var c = 0;
  (n.kids || []).forEach(function (k) { c += 1 + subtreeSize(k); });
  return c;
}

function nodeCard(n, mask, elapsed, maxCost) {
  var d = decide(n, n.path === "0", mask);
  var buying = !n.build || d === "buy";
  var label = d === "have" ? "HAVE" : buying ? "BUY" : "BUILD";
  var cost = nodeCost(n, mask);
  var pct = maxCost > 0 ? Math.round(cost / maxCost * 1000) / 10 : 0;
  var fold = "";
  // The root card is the tree itself, so collapsing it underneath
  // itself is meaningless. A fold button on the root would be dead.
  if (n.path !== "0" && n.build && n.short > 0 && (n.kids || []).length) {
    fold = ' <button type="button" class="plan-fold" data-plan-fold="' + esc(n.path) +
      '" aria-label="' + (collapsed[n.path] ? "expand" : "collapse") + " " + esc(n.name) + '">' +
      (collapsed[n.path] ? "+" + subtreeSize(n) : "–") + "</button>";
  }
  // Tree-table row: the label column carries the depth indent
  // (data-pad-left, applied by app.js through the CSSOM) while the
  // cost bar lives in a fixed-width column pinned to the right, so
  // every row's bar starts at the same x whatever its depth.
  var pad = (n.depth || 0) * 22;
  return '<div class="' + nodeClass(n, mask, elapsed) + '" data-path="' + esc(n.path) +
    '" title="' + esc(nodeTitle(n)) + '" role="button" tabindex="0">' +
    '<div class="plan-trow"><div class="plan-tlabel" data-pad-left="' + pad + '">' +
    '<div class="plan-node-top"><strong>' + esc(n.name) + "</strong>" +
    "<span>" + esc(label) + fold + "</span></div>" +
    "<small>" + iskC(cost) + "</small></div>" +
    '<div class="plan-tbar"><div class="plan-bar"><div class="plan-fill' + (buying ? " plan-buy" : "") +
    '" data-w="' + pct + '"></div></div></div></div></div>';
}

// Only the root starts expanded: every branch with children
// starts collapsed, and the global buttons flip the whole map.
function defaultCollapse() {
  collapsed = {};
  (function walk(n) {
    (n.kids || []).forEach(function (k) {
      if ((k.kids || []).length) collapsed[k.path] = true;
      walk(k);
    });
  })(P.root);
}

function setAllCollapsed(on) {
  if (!P) return;
  if (on) defaultCollapse();
  else collapsed = {};
  renderMap(currentTotals());
}

// One row per node: children follow as siblings, never nested
// inside the parent card, so every row spans the same width and
// the bar column lines up at the same x at every depth. Depth
// reads from the label indent. Bars scale against siblings, so a
// branch reads on its own terms.
function renderNode(n, mask, elapsed, maxCost) {
  var kidsHTML = "";
  var kids = n.kids || [];
  if (kids.length && !collapsed[n.path]) {
    var m = 0;
    kids.forEach(function (k) { m = Math.max(m, nodeCost(k, mask)); });
    kidsHTML = '<div class="plan-kids">' + kids.map(function (k) {
      return renderNode(k, mask, elapsed, m || 1);
    }).join("") + "</div>";
  }
  return nodeCard(n, mask, elapsed, maxCost) + kidsHTML;
}

function renderMap(t) {
  var host = $("plan-map");
  if (!host || !P) return;
  var mask = currentMask();
  var root = P.root;
  host.innerHTML = renderNode(root, mask, t.elapsed, root.materials + root.job || 1);
  host.querySelectorAll("[data-plan-fold]").forEach(function (btn) {
    btn.addEventListener("click", function (ev) {
      ev.stopPropagation();
      var path = btn.getAttribute("data-plan-fold");
      if (collapsed[path]) delete collapsed[path];
      else collapsed[path] = true;
      renderMap(currentTotals());
    });
  });
  host.querySelectorAll(".plan-node").forEach(function (el) {
    function pick(ev) {
      // Rows are siblings, so the closest card is always this
      // row's own; the guard below keeps that true if markup
      // ever nests again.
      var t = ev && ev.target && ev.target.closest ? ev.target.closest("[data-path]") : el;
      if (t && t !== el) return;
      selected = el.getAttribute("data-path");
      // Selecting a branch also opens it, same as its fold
      // button; an open or childless row is unaffected.
      delete collapsed[selected];
      renderMap(currentTotals());
      renderInspector(currentTotals());
    }
    el.addEventListener("click", pick);
    el.addEventListener("keydown", function (ev) {
      if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); pick(ev); }
    });
  });
}

// --- inspector ---
function renderInspector(t) {
  var host = $("plan-inspector");
  if (!host) return;
  var n = findNode(selected) || P.root;
  var mask = currentMask();
  var d = decide(n, n.path === "0", mask);
  var html = "<h4>Node inspector</h4>";
  html += "<p><strong>" + esc(n.name) + "</strong><br><small>" +
    esc(n.path === "0" ? "final assembly" : "depth " + n.depth) + "</small></p>";
  if (n.build && n.short > 0 && n.path !== "0" && !serverMode) {
    var buying = d === "buy";
    html += '<div class="plan-pick" role="group" aria-label="Make or buy">' +
      '<button type="button" data-plan-make="build"' + (buying ? "" : ' data-active="1"') + ">Build</button>" +
      '<button type="button" data-plan-make="buy"' + (buying ? ' data-active="1"' : "") + ">Buy</button></div>";
  }
  var rows = [];
  if (n.build && n.short > 0) {
    rows.push(["Branch cost", iskC(n.materials)]);
    rows.push(["Fees", n.jobKnown ? iskC(n.job) : "unknown"]);
    rows.push(["Duration", dur(n.timeSec)]);
    rows.push(["Blueprint", "ME" + n.me + " / TE" + n.te + " (" + esc(n.meSrc) + ")"]);
    if (n.saveKnown && n.save > 0 && n.me < 10)
      rows.push(["Blueprint opportunity", "Research to ME10 saves " + iskC(n.save) + " on this build."]);
    else if (n.me >= 10)
      rows.push(["Blueprint opportunity", "Already at ME10: nothing left to research here."]);
    if (n.deltaOn)
      rows.push(["Buy vs build", n.delta >= 0 ? "Building saves " + iskC(n.delta) : "Buying saves " + iskC(-n.delta)]);
  } else if (n.short > 0) {
    rows.push(["To buy", intF(n.short) + (n.known ? " × " + iskC(n.unit) + " = " + iskC(n.materials) : " · no price data")]);
  } else {
    rows.push(["Stock", "Covered by stock on hand."]);
  }
  html += '<table class="stat-table"><tbody>' + rows.map(function (r) {
    return "<tr><td>" + esc(r[0]) + "</td><td>" + esc(r[1]) + "</td></tr>";
  }).join("") + "</tbody></table>";
  host.innerHTML = html;
  host.querySelectorAll("[data-plan-make]").forEach(function (btn) {
    btn.addEventListener("click", function () {
      if (serverMode) return;
      overrides[n.path] = btn.getAttribute("data-plan-make") === "buy";
      renderAll();
    });
  });
}

// --- timeline ---
function renderGantt(t) {
  var host = $("plan-gantt");
  if (!host) return;
  if (!t.jobs.length) {
    host.innerHTML = "<p><small>No build jobs under this scenario: everything is bought or on hand.</small></p>";
    return;
  }
  var span = t.elapsed > 0 ? t.elapsed : 1;
  var weeks = Math.max(1, Math.ceil(t.elapsed / 604800));
  var head = '<div class="plan-weeks">';
  for (var w = 1; w <= Math.min(weeks, 26); w++) head += "<span>Week " + w + "</span>";
  head += "</div>";
  var rows = t.jobs.map(function (j) {
    var left = Math.round(j.start / span * 1000) / 10;
    var width = Math.max(Math.round((j.end - j.start) / span * 1000) / 10, j.end > j.start ? 0.5 : 0);
    return '<div class="plan-grow"><span class="plan-gname">' + esc(j.name) + "</span>" +
      '<div class="plan-gtrack"><div class="plan-gbar' + (j.critical ? " plan-crit" : "") +
      '" data-left="' + left + '" data-w="' + width +
      '" title="' + esc(j.name + " · " + dur(j.end - j.start) + (j.critical ? " · critical path" : "")) + '">' +
      "<small>" + esc(j.name) + "</small></div></div></div>";
  }).join("");
  var busy = {};
  t.jobs.forEach(function (j) { busy[j.slot] = true; });
  host.innerHTML = head + rows +
    "<p><small>Earliest completion " + esc(dur(t.elapsed)) +
    " · Busy slots " + Object.keys(busy).length + " / " + P.slots + "</small></p>";
}

// --- buy list and hub split ---
function renderBuy(t) {
  var list = $("plan-buylist"), hub = $("plan-hubsplit");
  if (!list || !hub) return;
  if (!t.shopping.length) {
    // Empty for two honest reasons: stock covers everything, or
    // the scenario buys the finished product outright (the root
    // is deliberately never a shopping line).
    var rootBought = P.root.short > 0 && decide(P.root, true, currentMask()) === "buy";
    list.innerHTML = "<p><small>" + (rootBought ?
      "This scenario buys the finished product outright; see the header verdict." :
      "Nothing to buy: stock covers the plan.") + "</small></p>";
  } else {
    list.innerHTML = "<ul>" + t.shopping.map(function (L) {
      return "<li><strong>" + esc(L.name) + "</strong> — " + intF(L.qty) +
        (L.known ? " × " + iskC(L.unit) + " = " + iskC(L.cost) : " · no price data") + "</li>";
    }).join("") + "</ul>" +
    '<p><button type="button" id="plan-copy">Copy multibuy list</button></p>';
  }
  var buy = t.buyValue, stock = P.stockVal, tot = buy + stock;
  var html = "<h4>Hub split</h4>";
  if (tot > 0) {
    html += "<p><small>" + P.marketName + " buy " + Math.round(buy / tot * 100) + "% · " +
      "Local stock " + Math.round(stock / tot * 100) + "%</small></p>";
    html += '<div class="plan-bar"><div class="plan-fill plan-buy" data-w="' +
      Math.round(buy / tot * 1000) / 10 + '"></div></div>';
  } else {
    html += "<p><small>No priced value to split yet.</small></p>";
  }
  if (!t.complete) html += "<p><small>" + t.unpriced + " line(s) have no price data.</small></p>";
  hub.innerHTML = html;
  var copy = $("plan-copy");
  if (copy) copy.addEventListener("click", copyMultibuy);
}

function copyMultibuy() {
  var t = currentTotals();
  var lines = t.shopping.filter(function (L) { return L.known; })
    .map(function (L) { return L.name + " x" + L.qty; });
  var text = lines.join("\n");
  function done(btn, s) {
    if (btn) btn.textContent = s;
  }
  var btn = $("plan-copy");
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).then(function () { done(btn, "Copied"); },
      function () { done(btn, "Copy failed"); });
  } else {
    var ta = document.createElement("textarea");
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    try {
      done(btn, document.execCommand("copy") ? "Copied" : "Copy failed");
    } catch (e) { done(btn, "Copy failed"); }
    document.body.removeChild(ta);
  }
}

// --- waterfall ---
function renderFall(t) {
  var host = $("plan-fall");
  if (!host) return;
  var bought = t.bought;
  var added = t.built;
  var fees = t.feesKnown ? t.fees : 0;
  var segs = [
    { name: "Purchased inputs", value: bought, cls: "plan-buy" },
    { name: "Build materials", value: added, cls: "plan-build" },
    { name: "Installation + logistics", value: fees,
      cls: "plan-fee", unknown: !t.feesKnown }
  ];
  var base = bought + added + fees;
  var scale = (t.known && t.value > base ? t.value : base) || 1;
  var html = "";
  var at = 0;
  segs.forEach(function (s) {
    var left = Math.round(at / scale * 1000) / 10;
    var width = Math.round(s.value / scale * 1000) / 10;
    html += '<div class="plan-frow"><span class="plan-fname">' + esc(s.name) + "</span>" +
      '<div class="plan-ftrack"><div class="plan-fbar ' + s.cls +
      '" data-left="' + left + '" data-w="' + width + '"></div></div>' +
      "<span>" + (s.unknown ? "unknown" : iskC(s.value)) + "</span></div>";
    at += s.value;
  });
  if (t.profKnown) {
    var left = Math.round(at / scale * 1000) / 10;
    var width = Math.max(Math.round(t.profit / scale * 1000) / 10, 0);
    html += '<div class="plan-frow"><span class="plan-fname">Market margin</span>' +
      '<div class="plan-ftrack"><div class="plan-fbar plan-crit' +
      '" data-left="' + left + '" data-w="' + width + '"></div></div>' +
      "<span>" + (t.profit > 0 ? "+" : "") + iskC(t.profit) + "</span></div>";
  } else {
    html += "<p><small>Margin needs full prices.</small></p>";
  }
  if (!t.complete) html += "<p><small>Partial: " + t.unpriced + " line(s) lack prices.</small></p>";
  host.innerHTML = html;
}

// --- render all and init ---
function renderAll() {
  var t = currentTotals();
  renderHeader(t);
  renderMap(t);
  renderInspector(t);
  renderGantt(t);
  renderBuy(t);
  renderFall(t);
}

function fail(note) {
  ["plan-map", "plan-gantt", "plan-buylist", "plan-fall"].forEach(function (id) {
    var el = $(id);
    if (el) el.innerHTML = "<p><small>" + esc(note) + "</small></p>";
  });
}

function setPayload(payload) {
  P = payload;
  if (!P || !P.root) throw new Error("empty plan");
  assignPaths(P.root, "0");
  byIdCache = {};
  // Server structs name it valueKnown; compute() names it
  // known. Normalize once so every renderer reads one shape.
  ["full", "balanced", "buyComp"].forEach(function (k) {
    if (P[k]) P[k].known = !!P[k].valueKnown;
  });
  // A fresh tree invalidates view state; the preset choice
  // survives, its buttons re-synced below.
  overrides = {};
  selected = "0";
  serverMode = false;
  defaultCollapse();
  crossCheck();
  renderAll();
}

function loadJSON() {
  return fetch(planURL(), { headers: { Accept: "application/json" } }).then(function (r) {
    if (!r.ok) throw new Error("plan data unavailable");
    return r.json();
  }).then(setPayload);
}

function syncPresetButtons() {
  section.querySelectorAll("[data-plan-preset]").forEach(function (btn) {
    var on = btn.getAttribute("data-plan-preset") === preset;
    btn.setAttribute("aria-pressed", on ? "true" : "false");
    if (on) btn.setAttribute("data-active", "1");
    else btn.removeAttribute("data-active");
  });
}

function wireME() {
  var tog = $("me-toggle"), fields = $("me-fields");
  if (tog && fields) tog.addEventListener("click", function () {
    var open = fields.hidden;
    fields.hidden = !open;
    tog.setAttribute("aria-expanded", open ? "true" : "false");
    var hint = $("me-hint");
    if (hint) hint.textContent = open ? "[Collapse]" : "[Expand]";
  });
  var apply = $("me-apply"), setall = $("me-setall");
  if (apply && setall) apply.addEventListener("click", function () {
    var v = parseInt(setall.value, 10);
    if (isNaN(v)) return;
    v = Math.max(0, Math.min(10, v));
    document.querySelectorAll('#me-fields input[name^="me_"]').forEach(function (el) { el.value = v; });
  });
}

// Recalculate without a page reload: the server re-renders the
// whole plan region (header, tabs, static list, verdict) and the
// client reboots its interactive state on top. Anything fails,
// the form submits natively and the server render stands alone.
function onRecalc(ev) {
  ev.preventDefault();
  var form = document.getElementById("plan-form");
  if (!form) return;
  var sp = new URLSearchParams(new FormData(form));
  // new FormData never carries buttons, so a judge pick (a
  // submit button with the scope as its value) must come from
  // the submitter; without it the pick would silently re-apply
  // the current scope.
  var sub = ev.submitter;
  if (sub && sub.name) sp.set(sub.name, sub.value);
  sp.delete("format");
  var url = window.location.pathname + "?" + sp.toString();
  var cur = "tree";
  section.querySelectorAll("[data-plan-tab]").forEach(function (b) {
    if (b.getAttribute("aria-selected") === "true") cur = b.getAttribute("data-plan-tab");
  });
  fetch(url, { headers: { Accept: "text/html" } }).then(function (r) {
    if (!r.ok) throw new Error("recalc unavailable");
    return r.text();
  }).then(function (html) {
    var doc = new DOMParser().parseFromString(html, "text/html");
    var fresh = doc.getElementById("plan-wrap");
    var host = document.getElementById("plan-wrap");
    if (!fresh || !host) throw new Error("recalc unavailable");
    host.innerHTML = fresh.innerHTML;
    window.history.replaceState(null, "", url);
    section = document.getElementById("plan");
    if (!section) throw new Error("recalc unavailable");
    wireStatic();
    return loadJSON();
  }).then(function () {
    showTab(cur);
    var btn = document.querySelector('#plan-form button[type="submit"]');
    if (btn) btn.focus({ preventScroll: true });
  }).catch(function () {
    var f = document.getElementById("plan-form");
    if (f) f.submit();
  });
}

function wireStatic() {
  section.querySelectorAll("[data-plan-preset]").forEach(function (btn) {
    btn.addEventListener("click", function () { setPreset(btn.getAttribute("data-plan-preset")); });
  });
  syncPresetButtons();
  section.querySelectorAll("[data-plan-tab]").forEach(function (btn) {
    btn.addEventListener("click", function () { showTab(btn.getAttribute("data-plan-tab")); });
  });
  section.querySelectorAll("[data-plan-expand-all]").forEach(function (btn) {
    btn.addEventListener("click", function () { setAllCollapsed(false); });
  });
  section.querySelectorAll("[data-plan-collapse-all]").forEach(function (btn) {
    btn.addEventListener("click", function () { setAllCollapsed(true); });
  });
  var form = document.getElementById("plan-form");
  if (form) form.addEventListener("submit", onRecalc);
  wireME();
  wireJudge();
}

// Judge-as picker: the same filterable dropdown as the
// character selectors elsewhere. Picks are submit buttons,
// so choosing one flows through the normal recalculate path
// (dynamic, or native without JS); typing only filters.
function wireJudge() {
  var input = document.getElementById("judge-q"), list = document.getElementById("judge-suggest");
  if (!input || !list) return;
  var current = input.getAttribute("data-judge-current") || "";
  function filter() {
    var q = input.value.toLowerCase().trim();
    list.querySelectorAll("[data-judge-pick]").forEach(function (b) {
      var t = (b.textContent || "").toLowerCase();
      b.hidden = !!q && t.indexOf(q) === -1;
    });
  }
  input.addEventListener("focus", function () {
    input.value = "";
    filter();
    list.hidden = false;
    input.setAttribute("aria-expanded", "true");
  });
  input.addEventListener("input", function () {
    filter();
    list.hidden = false;
  });
  input.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") {
      list.hidden = true;
      input.setAttribute("aria-expanded", "false");
      input.blur();
      if (!input.value && current) input.value = current;
    }
  });
  // One document listener for every reboot: it always reads
  // the live DOM, so swapped-in pickers keep working.
  if (!judgeDocWired) {
    judgeDocWired = true;
    document.addEventListener("click", function (ev) {
      var live = document.getElementById("judge-q"), liveList = document.getElementById("judge-suggest");
      if (!live || !liveList) return;
      if (!live.parentElement.contains(ev.target)) {
        liveList.hidden = true;
        live.setAttribute("aria-expanded", "false");
        var cur = live.getAttribute("data-judge-current") || "";
        if (!live.value && cur) live.value = cur;
      }
    });
  }
}

function init() {
  wireStatic();
  loadJSON().catch(function () {
    fail("The interactive views need the plan data; the figures above are current.");
  });
}

if (document.readyState === "loading") {
  document.addEventListener("DOMContentLoaded", init);
} else {
  init();
}

})();
