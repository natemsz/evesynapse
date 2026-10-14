// pi.js — PI Planner map interactions. Server renders the plan;
// this file only moves the highlight and navigates picks. Amounts,
// factories and taxes always come from the server render. One
// formula is mirrored, on purpose: the coverage verdict and tinting
// recompute piplan.CoverageFor (internal/piplan/piplan.go) live, so
// ticking planets never leaves the page arguing with itself. A
// change to either side must change both. Recalculate remains the
// source of truth for the money.
(function () {
  "use strict";
  var graphEl = document.getElementById("pi-graph-data");
  var graphRoot = document.getElementById("pi-graph");
  if (!graphRoot) return;
  var graph = {};
  try { graph = JSON.parse(graphEl.dataset.graph || "{}"); } catch (e) { graph = {}; }

  function chainOf(target) {
    var seen = {};
    (function walk(name) {
      if (seen[name]) return;
      seen[name] = true;
      (graph[name] && graph[name].inputs || []).forEach(walk);
    })(target);
    return seen;
  }
  function downstreamOf(target) {
    var out = {};
    Object.keys(graph).forEach(function (name) {
      if (name === target) return;
      if (chainOf(name)[target]) out[name] = true;
    });
    return out;
  }

  function highlight(target) {
    var chain = chainOf(target);
    var down = downstreamOf(target);
    graphRoot.querySelectorAll("[data-pi-node]").forEach(function (el) {
      var name = el.getAttribute("data-pi-node");
      var tier = el.getAttribute("data-pi-tier");
      el.classList.remove("selected", "input", "output", "dimmed");
      if (tier === "Planet") return; // planet tint is checkbox-driven
      if (name === target) el.classList.add("selected");
      else if (chain[name]) el.classList.add("input");
      else if (down[name]) el.classList.add("output");
      else el.classList.add("dimmed");
    });
    var sel = document.getElementById("pi-product");
    if (sel) sel.value = target;
  }

  graphRoot.addEventListener("click", function (ev) {
    var btn = ev.target.closest("[data-pi-node]");
    if (!btn) return;
    var name = btn.getAttribute("data-pi-node");
    var tier = btn.getAttribute("data-pi-tier");
    if (tier === "Planet") {
      // Toggle the matching checkbox and let the form state lead.
      var box = document.querySelector('input[name="planets"][value="' + name + '"]');
      if (box) { box.checked = !box.checked; box.dispatchEvent(new Event("change", { bubbles: true })); }
      return;
    }
    if (tier === "R0") { highlight(name); return; } // raws highlight only
    // P1-P4: navigate with current simulator settings preserved.
    var form = document.getElementById("pi-form");
    var params = new URLSearchParams(window.location.search);
    if (form) {
      new FormData(form).forEach(function (v, k) {
        if (k === "planets") return;
        params.set(k, v);
      });
      params.delete("planets");
      form.querySelectorAll('input[name="planets"]:checked').forEach(function (b) {
        params.append("planets", b.value);
      });
    }
    params.set("product", name);
    window.location.search = params.toString();
  });

  // Live coverage: selected planets glow, chain tiles your
  // planets cannot supply flag, and the verdict line recomputes —
  // all without a round trip, mirroring the server's check.
  var tierOf = {};
  var rawPlanets = {};
  graphRoot.querySelectorAll("[data-pi-node]").forEach(function (el) {
    var name = el.getAttribute("data-pi-node");
    tierOf[name] = el.getAttribute("data-pi-tier");
    if (tierOf[name] === "R0") {
      var ps = {};
      (el.getAttribute("data-pi-planets") || "").split(",").forEach(function (s) {
        if (s.trim()) ps[s.trim()] = true;
      });
      rawPlanets[name] = ps;
    }
  });

  function currentTarget() {
    var sel = graphRoot.querySelector(".pi-node.selected");
    if (sel) return sel.getAttribute("data-pi-node");
    var prod = document.getElementById("pi-product");
    return prod ? prod.value : "";
  }

  function retintCoverage() {
    var checked = {};
    document.querySelectorAll('input[name="planets"]:checked').forEach(function (b) { checked[b.value] = true; });
    var nChecked = Object.keys(checked).length;
    graphRoot.querySelectorAll('[data-pi-tier="Planet"]').forEach(function (el) {
      var on = !!checked[el.getAttribute("data-pi-node")];
      el.classList.toggle("covered", on);
      el.classList.toggle("dimmed", !on);
    });
    var target = currentTarget();
    if (!target) return;
    var chain = chainOf(target);
    var raws = Object.keys(chain).filter(function (n) { return tierOf[n] === "R0"; });
    var rawCovered = {};
    var missing = [];
    raws.forEach(function (r) {
      var ok = nChecked > 0 && Object.keys(rawPlanets[r] || {}).some(function (p) { return checked[p]; });
      rawCovered[r] = ok;
      if (nChecked > 0 && !ok) missing.push(r);
    });
    // Tint: raws flag when unsupplied; a P1 flags when its raw is.
    graphRoot.querySelectorAll("[data-pi-node]").forEach(function (el) {
      var name = el.getAttribute("data-pi-node");
      if (!chain[name]) return;
      if (tierOf[name] === "R0") {
        el.classList.toggle("missing", nChecked > 0 && !rawCovered[name]);
      } else if (tierOf[name] === "P1") {
        var ins = (graph[name] && graph[name].inputs) || [];
        el.classList.toggle("missing", nChecked > 0 && ins.length > 0 && !rawCovered[ins[0]]);
      }
    });
    // The verdict line itself, in the server's own wording.
    var box = document.getElementById("pi-coverage");
    if (!box) return;
    if (nChecked === 0) {
      box.textContent = "No planet types selected — tick planets above to check coverage.";
      box.className = "pi-coverage";
    } else if (missing.length === 0) {
      box.textContent = "Covered — your " + nChecked + " selected planet type(s) supply all " +
        raws.length + " raw resources.";
      box.className = "pi-coverage pos";
    } else {
      missing.sort();
      box.textContent = "Not covered — " + (raws.length - missing.length) + " of " + raws.length +
        " raw resources available. Missing: " + missing.join(", ");
      box.className = "pi-coverage neg";
    }
  }
  document.querySelectorAll('input[name="planets"]').forEach(function (b) {
    b.addEventListener("change", retintCoverage);
  });
  retintCoverage();
})();
