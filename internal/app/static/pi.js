// pi.js — PI Planner map interactions. Server renders the plan;
// this file moves the highlight and recalculates in place. Amounts,
// factories and taxes always come from the server render: a pick or
// a Recalculate fetches the page for the new settings and swaps the
// planner region (#pi-wrap), so nothing on the page ever reloads.
// One formula is mirrored, on purpose: the coverage verdict and
// tinting recompute piplan.CoverageFor (internal/piplan/piplan.go)
// live, so ticking planets never leaves the page arguing with
// itself. A change to either side must change both.
(function () {
  "use strict";

  // Fetches the planner for these settings and swaps its region in
  // place. Only a failed fetch navigates, so the page still works.
  function recalc(params) {
    var url = "/industry/pi/?" + params.toString();
    window.fetch(url, { credentials: "same-origin", headers: { Accept: "text/html" } }).then(function (r) {
      if (!r.ok) throw new Error("recalc unavailable");
      return r.text();
    }).then(function (html) {
      var doc = new DOMParser().parseFromString(html, "text/html");
      var fresh = doc.getElementById("pi-wrap");
      var host = document.getElementById("pi-wrap");
      if (!fresh || !host) throw new Error("recalc unavailable");
      host.innerHTML = fresh.innerHTML;
      // The address keeps the settings, not the one-off "cover"
      // request: a reload shows the planets now ticked.
      params.delete("cover");
      if (window.history && window.history.replaceState) {
        window.history.replaceState(null, "", "/industry/pi/?" + params.toString());
      }
      boot();
    }).catch(function () {
      window.location.href = url;
    });
  }

  // The form's current settings, planets as ticked.
  function formParams() {
    var params = new URLSearchParams();
    var form = document.getElementById("pi-form");
    if (!form) return params;
    new FormData(form).forEach(function (v, k) {
      if (k !== "planets") params.set(k, v);
    });
    var any = false;
    form.querySelectorAll('input[name="planets"]:checked').forEach(function (b) {
      params.append("planets", b.value);
      any = true;
    });
    // Present but empty means "none ticked" to the server.
    if (!any) params.set("planets", "");
    return params;
  }

  // Picking a product drives the planet selection: the server ticks
  // the smallest set of planet types that supplies its chain.
  function pickProduct(name) {
    var params = formParams();
    params.delete("planets");
    params.set("product", name);
    params.set("cover", "1");
    recalc(params);
  }

  var booted = null; // the #pi-graph last wired
  function boot() {
    var graphEl = document.getElementById("pi-graph-data");
    var graphRoot = document.getElementById("pi-graph");
    if (!graphRoot || graphRoot === booted) return;
    booted = graphRoot;
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
      // P1-P4: plan it here, simulator settings kept.
      highlight(name);
      pickProduct(name);
    });

    var form = document.getElementById("pi-form");
    if (form) form.addEventListener("submit", function (ev) {
      ev.preventDefault();
      recalc(formParams());
    });
    var product = document.getElementById("pi-product");
    if (product) product.addEventListener("change", function () {
      pickProduct(product.value);
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
  }

  boot();
  // An in-page swap onto the planner does not re-run this script.
  document.addEventListener("evesynapse:content-ready", boot);
})();
