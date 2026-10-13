// pi.js — PI Planner map interactions. Server renders the plan;
// this file only moves the highlight and navigates picks. It
// duplicates no money or factory formula: amounts, factories and
// taxes always come from the server render. Coverage tinting
// mirrors the server's raw-availability check for instant feedback
// while ticking planets; Recalculate remains the source of truth.
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

  // Live planet tint: selected planets glow; chain raws your
  // planets cannot supply flag without a round trip.
  function retintPlanets() {
    var checked = {};
    document.querySelectorAll('input[name="planets"]:checked').forEach(function (b) { checked[b.value] = true; });
    graphRoot.querySelectorAll('[data-pi-tier="Planet"]').forEach(function (el) {
      var on = !!checked[el.getAttribute("data-pi-node")];
      el.classList.toggle("covered", on);
      el.classList.toggle("dimmed", !on);
    });
  }
  document.querySelectorAll('input[name="planets"]').forEach(function (b) {
    b.addEventListener("change", retintPlanets);
  });
  retintPlanets();
})();
