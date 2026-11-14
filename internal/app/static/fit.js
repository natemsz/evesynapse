// Fitting simulator editor: the browser keeps the fit
// document (ship, items, charge choices) and posts it to
// /fittings/simulate/ on every change; the server answers with
// the re-rendered workbench fragment, which swaps in place. The
// canonical document rides back on the fragment root, so the
// editor state always matches what the server calculated.
// The shared tooltip builder (FitTipUI) lives in fittip.js,
// which every page carrying this file loads first.

// --- phone views -------------------------------------------------
// Pure decisions for the fitting tool's phone layout (the editor
// below wires them to the DOM): which workbench sections each
// view shows, and the next state in the racks' tap-to-cycle. The
// cycle walks the states the module itself allows, in engagement
// order (online, active, overheated, offline); a line whose
// instances disagree ("" = mixed) enters at the first state.
var FitPhoneViews = (function () {
  var ORDER = ["online", "active", "overheated", "offline"];
  function sections(view) {
    return {
      center: view === "radial",
      slots: view === "racks" || view === "browse",
      bays: view === "racks",
      stats: view === "stats",
      browse: view === "browse"
    };
  }
  function nextState(validCSV, current) {
    var valid = (validCSV || "").split(",").filter(function (s) { return !!s; });
    var order = ORDER.filter(function (s) { return valid.indexOf(s) >= 0; });
    if (!order.length) return "";
    return order[(order.indexOf(current || "") + 1) % order.length];
  }
  // clickState is the desktop mouse's state change, after the
  // in-game module controls: a click switches a module on or off
  // (active and online for one that cycles, online and offline for
  // a passive one), Shift+click toggles overheat, Ctrl+click puts
  // it offline or back online. "" means the click changes nothing.
  function clickState(validCSV, current, shift, ctrl) {
    var valid = (validCSV || "").split(",").filter(function (s) { return !!s; });
    var has = function (s) { return valid.indexOf(s) >= 0; };
    var on = has("active") ? "active" : "online";
    var next = "";
    if (shift) {
      if (!has("overheated")) return "";
      next = current === "overheated" ? "active" : "overheated";
    } else if (ctrl) {
      next = current === "offline" ? on : "offline";
    } else if (has("active")) {
      next = (current === "active" || current === "overheated") ? "online" : "active";
    } else {
      next = current === "offline" ? "online" : "offline";
    }
    return has(next) && next !== current ? next : "";
  }
  return { sections: sections, nextState: nextState, clickState: clickState };
})();
if (typeof module !== "undefined" && module.exports) {
  module.exports = { FitPhoneViews: FitPhoneViews };
}

// --- drone gauges ------------------------------------------------
// The Drones card carries a micro-stat header: active bandwidth
// used / the ship's bandwidth, and bay volume used / capacity.
// The numbers already ride the workbench in the stats panel's
// "Drone bandwidth" / "Drone bay" rows ("25 / 50 Mbit/s"), which
// the server formats; the gauges parse the pair back out and
// restyle it as bars at the top of the drone group. Shared by the
// editor (re-applied after every re-render) and read-only
// workbenches (applied once on load).
var FitGauges = (function () {
  function parsePair(text) {
    var m = /([\d,]+(?:\.\d+)?)\s*\/\s*([\d,]+(?:\.\d+)?)/.exec(text || "");
    if (!m) return null;
    return {
      used: parseFloat(m[1].replace(/,/g, "")),
      max: parseFloat(m[2].replace(/,/g, "")),
      text: (text || "").replace(/\s+/g, " ").trim()
    };
  }
  function statPair(wb, label) {
    var dts = wb.querySelectorAll(".fit-stats dt");
    for (var i = 0; i < dts.length; i++) {
      if ((dts[i].textContent || "").replace(/\s+/g, " ").trim() !== label) continue;
      var wrap = dts[i].parentNode;
      if (!wrap || !wrap.children) return null;
      for (var j = 0; j < wrap.children.length; j++) {
        if (wrap.children[j].tagName === "DD") {
          return parsePair(wrap.children[j].textContent || "");
        }
      }
      return null;
    }
    return null;
  }
  function gaugeRow(label, pair) {
    var row = document.createElement("div");
    row.className = "fit-bar-row fit-gauge-row" + (pair.used > pair.max ? " over" : " good");
    var lab = document.createElement("span");
    lab.className = "fit-bar-label";
    lab.textContent = label;
    row.appendChild(lab);
    var bar = document.createElement("span");
    bar.className = "fit-bar";
    var fill = document.createElement("span");
    var pct = pair.max > 0 ? Math.min(100, (pair.used / pair.max) * 100) : 0;
    fill.setAttribute("data-w", pct.toFixed(1));
    bar.appendChild(fill);
    row.appendChild(bar);
    var nums = document.createElement("span");
    nums.className = "fit-bar-nums";
    nums.textContent = pair.text;
    row.appendChild(nums);
    return row;
  }
  function apply(wb) {
    if (!wb || !wb.querySelector) return;
    var group = wb.querySelector('.fit-group[data-fit-group="drone"]');
    if (!group) return;
    var old = group.querySelector(".fit-drone-gauges");
    if (old && old.parentNode) old.parentNode.removeChild(old);
    var bw = statPair(wb, "Drone bandwidth");
    var bay = statPair(wb, "Drone bay");
    if (!bw && !bay) return;
    var box = document.createElement("div");
    box.className = "fit-drone-gauges";
    if (bw) box.appendChild(gaugeRow("Bandwidth", bw));
    if (bay) box.appendChild(gaugeRow("Bay", bay));
    var h4 = group.querySelector("h4");
    if (h4 && h4.nextSibling) group.insertBefore(box, h4.nextSibling);
    else if (h4) group.appendChild(box);
    else if (group.firstChild) group.insertBefore(box, group.firstChild);
    else group.appendChild(box);
  }
  return { apply: apply, parsePair: parsePair };
})();

(function () {
  var editor = document.getElementById("fit-editor");
  if (!editor || !window.fetch) return;

  var pilotSel = document.getElementById("fit-pilot");
  var cloneSel = document.getElementById("fit-clone");
  var nameInput = document.getElementById("fit-name");
  var shipInput = document.getElementById("fit-ship-search");
  var shipList = document.getElementById("fit-ship-suggest");
  var modInput = document.getElementById("fit-mod-search");
  var modList = document.getElementById("fit-mod-suggest");
  var famChips = document.getElementById("fit-famchips");
  var fittableChk = document.getElementById("fit-fittable");
  var usableChk = document.getElementById("fit-usable");
  var metaSel = document.getElementById("fit-meta");
  var browse = document.getElementById("fit-browse");
  var browseTree = document.getElementById("fit-browse-tree");
  var browseHint = document.getElementById("fit-browse-hint");
  var saveBtn = document.getElementById("fit-save");
  var exportBtn = document.getElementById("fit-export-btn");
  var exportOut = document.getElementById("fit-eft-out"); // the copy popup's text box

  function workbench() { return editor.querySelector(".fit-wb"); }

  function readState() {
    var wb = workbench();
    if (!wb) return null;
    try { return JSON.parse(wb.getAttribute("data-fit-state") || "{}"); }
    catch (e) { return null; }
  }

  var state = readState() || {};
  state.name = state.name || "";
  state.shipTypeId = state.shipTypeId || 0;
  state.items = state.items || [];
  state.charges = state.charges || {};

  // Per-instance module states ride alongside qty: states[i] is
  // the i-th instance's state ("" = the type default). Kept the
  // same length as qty everywhere the document mutates.
  function normItems() {
    for (var i = 0; i < state.items.length; i++) {
      var it = state.items[i];
      it.qty = it.qty || 1;
      if (!it.states || !it.states.length) it.states = [];
      while (it.states.length < it.qty) it.states.push("");
      if (it.states.length > it.qty) it.states.length = it.qty;
    }
  }
  normItems();

  function pilotID() {
    return pilotSel ? (parseInt(pilotSel.value, 10) || 0) : 0;
  }

  function cloneID() {
    return cloneSel ? (parseInt(cloneSel.value, 10) || 0) : 0;
  }

  var simSeq = 0;
  var simInFlight = false;
  var simQueued = false;
  // No silent taps: the last add/remove/charge change arms a flash
  // that fires on the re-rendered target once simulate() lands.
  var pendingFlash = null; // {type: "slot"|"group"|"charge", id: number, family: string}
  // The head's arrangement follows the ship: with no ship picked
  // the editor carries fit-noship (the bar shows only the doors)
  // and the fit's working surfaces stay hidden behind the empty
  // state. Derived from state.shipTypeId, the editor's own truth;
  // called wherever a simulation is kicked off, which is every
  // path that changes the ship.
  function syncShipGate() {
    var hasShip = !!state.shipTypeId;
    editor.classList.toggle("fit-noship", !hasShip);
    var blocks = editor.querySelectorAll("[data-fit-shipblock]");
    for (var i = 0; i < blocks.length; i++) blocks[i].hidden = !hasShip;
    var empty = editor.querySelector("[data-fit-emptystate]");
    if (empty) empty.hidden = hasShip;
  }

  function simPayload(items) {
    return {
      name: state.name,
      shipTypeId: state.shipTypeId,
      items: items,
      charges: state.charges,
      implants: state.implants || {},
      boosts: state.boosts || [],
      pilot: pilotID(),
      clone: cloneID()
    };
  }

  function simulate() {
    syncShipGate();
    // Coalesce bursts: one request in flight at a time; a change
    // made mid-flight re-sends once with the latest state instead
    // of piling up stale POSTs.
    if (simInFlight) { simQueued = true; return; }
    simInFlight = true;
    state.name = nameInput ? nameInput.value : (state.name || "");
    var seq = ++simSeq;
    var payload = simPayload(state.items);
    endPreview();
    function simDone() {
      simInFlight = false;
      if (simQueued) { simQueued = false; simulate(); }
    }
    fetch("/fittings/simulate/", {
      method: "POST",
      headers: { "Content-Type": "application/json", "Accept": "text/html" },
      body: JSON.stringify(payload)
    }).then(function (resp) {
      return resp.ok ? resp.text() : "";
    }).then(function (html) {
      if (!html || seq !== simSeq) { simDone(); return; }
      var tmp = document.createElement("div");
      tmp.innerHTML = html;
      var fresh = tmp.querySelector(".fit-wb");
      var current = workbench();
      if (!fresh || !current) { simDone(); return; }
      var before = readFigures(current);
      var beforeShip = shipOf(current);
      current.replaceWith(fresh);
      // The fresh fragment's slot positions ride data-left /
      // data-top until app.js's geometry pass applies them; its
      // MutationObserver runs after this handler, and the
      // re-anchor below measures slot rects, so apply the pass
      // here first or every fresh slot still sits at the
      // visual's top-left corner when it is measured.
      if (window.EveSynapseContent && window.EveSynapseContent.applyDataGeometry) {
        window.EveSynapseContent.applyDataGeometry(fresh);
      }
      var next = readState();
      if (next) {
        next.items = next.items || [];
        next.charges = next.charges || {};
        state = next;
        normItems();
      }
      reanchorTip();
      if (window.FitRange) window.FitRange.refresh();
      if (window.FitRamp) window.FitRamp.refresh();
      syncFold();
      syncImpFold();
      syncDroneGauges();
      syncChargeButtons();
      syncSplitRows();
      syncFilterUI();
      syncSubsystemUI();
      syncUndoButtons();
      syncFullButton();
      if (beforeShip !== shipOf(fresh)) {
        // Rows answer for the hull they were fetched for.
        browseCache = {};
        if (browseTree) browseTree.innerHTML = "";
      }
      applyFitView();
      refreshBrowseRows();
      runPendingFlash();
      // Same hull: show what the edit moved. A new hull is a new
      // fit, so nothing is compared.
      if (before && beforeShip && beforeShip === shipOf(fresh)) {
        showDeltas(fresh, figureDeltas(before, readFigures(fresh)));
      }
      simDone();
    }).catch(function () { simDone(); /* the static copy stays honest */ });
  }

  // Fires the armed add/remove flash on the re-rendered target.
  function runPendingFlash() {
    var f = pendingFlash;
    pendingFlash = null;
    if (!f) return;
    var el = null;
    if (f.type === "slot") {
      el = editor.querySelector('.fit-vslot[data-fit-vslot="' + f.id + '"]');
    } else if (f.type === "group") {
      el = editor.querySelector('.fit-group[data-fit-group="' + f.family + '"]');
    } else if (f.type === "charge") {
      el = editor.querySelector('[data-fit-ammo-for="' + f.id + '"]');
      if (el && el.closest) el = el.closest(".fit-imp");
      if (!el) el = editor.querySelector('.fit-vslot[data-fit-vslot="' + f.id + '"] .fit-vammo');
    }
    if (f.undo && f.family) rackNote(f.family, "Removed " + f.undo + ".", false, true);
    if (!el) return;
    el.classList.add("flash");
    if (el.scrollIntoView) el.scrollIntoView({ block: "nearest" });
    window.setTimeout(function () { el.classList.remove("flash"); }, 750);
    if (f.landed) {
      // The new module lands in its slot and its rack row slides in.
      el.classList.add("landed");
      var row = editor.querySelector('.fit-chip[data-fit-rowtype="' + f.id + '"]');
      if (row) row.classList.add("landed");
      window.setTimeout(function () {
        el.classList.remove("landed");
        if (row) row.classList.remove("landed");
      }, 600);
    }
  }

  // The fitted module's name, read from the rendered rack.
  function typeNameOf(typeID) {
    var el = editor.querySelector('[data-tip-type="' + typeID + '"][data-tip-name]');
    return el ? el.getAttribute("data-tip-name") : "a module";
  }

  // A short line under a rack's heading: a refusal (err) or the
  // Undo offer after a removal. A re-render drops it; otherwise it
  // goes after a few seconds.
  function rackNote(fam, msg, isErr, withUndo) {
    var g = fam ? editor.querySelector('.fit-group[data-fit-group="' + fam + '"]') : null;
    if (!g) {
      if (isErr) fitNotice(msg, true, false);
      return null;
    }
    var old = g.querySelector(".fit-racknote");
    if (old) old.remove();
    var p = document.createElement("p");
    p.className = "fit-racknote" + (isErr ? " err" : "");
    p.setAttribute("role", isErr ? "alert" : "status");
    p.textContent = msg;
    if (withUndo) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "fit-racknote-undo";
      b.setAttribute("data-fit-undo", "");
      b.textContent = "Undo";
      p.appendChild(document.createTextNode(" "));
      p.appendChild(b);
    }
    var head = g.querySelector("h4");
    if (head) head.insertAdjacentElement("afterend", p);
    else g.insertBefore(p, g.firstChild);
    window.setTimeout(function () { if (p.isConnected) p.remove(); }, withUndo ? 8000 : 5000);
    return g;
  }
  // A refused add: the rack's heading shakes and says why.
  function rackRefuse(fam, msg) {
    var g = rackNote(fam, msg, true, false);
    if (!g) return;
    var head = g.querySelector("h4");
    if (!head) return;
    head.classList.remove("refused");
    void head.offsetWidth; // restart the animation on a repeat refusal
    head.classList.add("refused");
    if (head.scrollIntoView) head.scrollIntoView({ block: "nearest" });
  }

  // --- stat changes -------------------------------------------------
  // Every simulation carries its headline figures as numbers
  // (data-fit-figures). The editor compares two of them to show what
  // an edit moved, or what a resting search row would move.
  function shipOf(wb) {
    try {
      var st = JSON.parse(wb.getAttribute("data-fit-state") || "{}");
      return st.shipTypeId || 0;
    } catch (e) { return 0; }
  }
  function readFigures(root) {
    var el = root && root.querySelector("[data-fit-figures]");
    if (!el) return null;
    try {
      var list = JSON.parse(el.getAttribute("data-fit-figures") || "[]");
      var m = {};
      for (var i = 0; i < list.length; i++) m[list[i].k] = list[i];
      return m;
    } catch (e) { return null; }
  }
  function figureDeltas(a, b) {
    var out = [];
    if (!a || !b) return out;
    Object.keys(b).forEach(function (k) {
      if (!a[k]) return;
      var f = b[k];
      var d = f.v - a[k].v;
      if (Math.abs(d) < 0.5 * Math.pow(10, -f.d)) return;
      out.push({ k: k, d: d, f: f });
    });
    return out;
  }
  var deltaFmt = {};
  function deltaText(x) {
    var nf = deltaFmt[x.f.d] || (deltaFmt[x.f.d] = new Intl.NumberFormat("en-US", { maximumFractionDigits: x.f.d }));
    return (x.d > 0 ? "+" : "\u2212") + nf.format(Math.abs(x.d)) + (x.f.u ? " " + x.f.u : "");
  }
  // Chips beside each moved figure, green or red by whether the
  // change helps; they fade after about three seconds.
  function showDeltas(root, deltas) {
    var old = root.querySelectorAll(".fit-delta");
    for (var j = 0; j < old.length; j++) old[j].remove();
    deltas.forEach(function (x) {
      var anchors = root.querySelectorAll('[data-fit-fig="' + x.k + '"]');
      for (var i = 0; i < anchors.length; i++) {
        var chip = document.createElement("span");
        chip.className = "fit-delta " + ((x.d > 0) === x.f.hi ? "good" : "bad");
        chip.textContent = deltaText(x);
        anchors[i].appendChild(chip);
        (function (c) { window.setTimeout(function () { c.remove(); }, 3200); })(chip);
      }
    });
  }

  // --- hover preview ------------------------------------------------
  // Resting 300 ms on a search row shows what the fit would become:
  // one simulation of the fit plus that module, its changes shown as
  // chips in the row itself (the open list covers the stat panel).
  // Leaving the row cancels it. Desktop pointers only.
  var previewTimer = null;
  var previewAbort = null;
  var previewOK = !!(window.matchMedia && window.matchMedia("(hover: hover) and (pointer: fine)").matches);
  var previewKinds = { high: 1, medium: 1, low: 1, rig: 1, subsystem: 1, drone: 1, fighter: 1 };
  // Answers are kept per fit state, so sweeping back over a row costs
  // no second call; a throttled answer (429) pauses previews for a
  // minute so they never use up the edits' share.
  var previewCache = {};
  var previewCacheFor = "";
  var previewPausedUntil = 0;
  var previewRow = null;
  function showRowPreview(li, deltas) {
    var box = document.createElement("span");
    box.className = "sug-preview";
    if (!deltas.length) {
      box.textContent = "No change to the stats shown";
    }
    deltas.forEach(function (x) {
      var chip = document.createElement("span");
      chip.className = "fit-delta preview " + ((x.d > 0) === x.f.hi ? "good" : "bad");
      chip.textContent = deltaText(x);
      box.appendChild(chip);
    });
    var text = li.querySelector(".sug-text") || li;
    text.appendChild(box);
  }
  function startPreview(it, li) {
    endPreview();
    previewRow = li || null;
    if (!previewOK || !state.shipTypeId || !previewKinds[it.kind || ""]) return;
    if (restrictionFor(it.id) || Date.now() < previewPausedUntil) return;
    if (it.kind === "subsystem" && subsystemTaken(it.id)) return;
    var base = JSON.stringify(simPayload(state.items));
    if (base !== previewCacheFor) { previewCache = {}; previewCacheFor = base; }
    if (previewCache[it.id]) {
      if (li) showRowPreview(li, figureDeltas(readFigures(workbench()), previewCache[it.id]));
      return;
    }
    previewTimer = window.setTimeout(function () {
      previewTimer = null;
      var items = JSON.parse(JSON.stringify(state.items || []));
      var found = false;
      for (var i = 0; i < items.length; i++) {
        if (items[i].typeId === it.id && !items[i].cargo) { items[i].qty += 1; items[i].states.push(""); found = true; break; }
      }
      if (!found) items.push({ typeId: it.id, qty: 1, states: [] });
      var ctl = window.AbortController ? new AbortController() : null;
      previewAbort = ctl;
      fetch("/fittings/simulate/", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Accept": "text/html" },
        body: JSON.stringify(simPayload(items)),
        signal: ctl ? ctl.signal : undefined
      }).then(function (resp) {
        if (resp.status === 429) previewPausedUntil = Date.now() + 60000;
        return resp.ok ? resp.text() : "";
      }).then(function (html) {
        if (previewAbort !== ctl || !html) return;
        previewAbort = null;
        var tmp = document.createElement("div");
        tmp.innerHTML = html;
        var figs = readFigures(tmp);
        if (!figs) return;
        if (previewCacheFor === base) previewCache[it.id] = figs;
        if (li && previewRow === li && li.isConnected) {
          showRowPreview(li, figureDeltas(readFigures(workbench()), figs));
        }
      }).catch(function () { /* cancelled or offline: no preview */ });
    }, 300);
  }
  function endPreview() {
    if (previewTimer) { window.clearTimeout(previewTimer); previewTimer = null; }
    if (previewAbort) { previewAbort.abort(); previewAbort = null; }
    if (previewRow) {
      var old = previewRow.querySelectorAll(".sug-preview");
      for (var i = 0; i < old.length; i++) old[i].remove();
      previewRow = null;
    }
  }

  // --- filter controls: enabled states follow ship/pilot ----------
  function syncFilterUI() {
    var hasShip = !!(state && state.shipTypeId);
    if (fittableChk) {
      fittableChk.disabled = !hasShip;
      if (!hasShip) fittableChk.checked = false;
      var fl = fittableChk.closest("label");
      if (fl) fl.classList.toggle("disabled", !hasShip);
    }
    var allV = pilotID() === 0;
    if (usableChk) {
      usableChk.disabled = allV;
      if (allV) usableChk.checked = false;
      var ul = usableChk.closest("label");
      if (ul) ul.classList.toggle("disabled", allV);
      usableChk.title = allV ? "All V pilots can use everything" : "";
    }
  }

  // A line is a type and where it rides: on the hull (or in its
  // natural hold) or carried in cargo as a refit (cargo: true).
  function findItem(typeID, cargo) {
    for (var i = 0; i < state.items.length; i++) {
      if (state.items[i].typeId === typeID && !!state.items[i].cargo === !!cargo) return i;
    }
    return -1;
  }

  // --- undo / redo -------------------------------------------------
  // Client-side history of fit states (ship + modules + charges +
  // drones). Every mutation pushes the pre-change state; undo/redo
  // re-post the restored state through simulate(). Capped at 50.
  var past = [];
  var future = [];
  function snapState() {
    return {
      shipTypeId: state.shipTypeId,
      shipName: shipInput ? shipInput.value : "",
      items: JSON.parse(JSON.stringify(state.items || [])),
      charges: JSON.parse(JSON.stringify(state.charges || {})),
      implants: JSON.parse(JSON.stringify(state.implants || {})),
      boosts: JSON.parse(JSON.stringify(state.boosts || []))
    };
  }
  function applySnap(s) {
    state.shipTypeId = s.shipTypeId;
    state.items = JSON.parse(JSON.stringify(s.items || []));
    state.charges = JSON.parse(JSON.stringify(s.charges || {}));
    state.implants = JSON.parse(JSON.stringify(s.implants || {}));
    state.boosts = JSON.parse(JSON.stringify(s.boosts || []));
    if (shipInput) shipInput.value = s.shipName || "";
    markDirty();
    simulate();
    syncUndoButtons();
  }
  function pushHistory() {
    past.push(snapState());
    if (past.length > 50) past.shift();
    future = [];
    syncUndoButtons();
  }
  function undo() {
    if (!past.length) return;
    future.push(snapState());
    applySnap(past.pop());
    syncUndoButtons();
  }
  function redo() {
    if (!future.length) return;
    past.push(snapState());
    applySnap(future.pop());
    syncUndoButtons();
  }
  // Undo/redo ride the visual's corner and re-render with it, so
  // the buttons are re-queried every time (delegated clicks).
  function syncUndoButtons() {
    var ub = editor.querySelectorAll("[data-fit-undo]");
    var rb = editor.querySelectorAll("[data-fit-redo]");
    for (var i = 0; i < ub.length; i++) ub[i].disabled = !past.length;
    for (var j = 0; j < rb.length; j++) rb[j].disabled = !future.length;
  }
  syncUndoButtons();

  // --- explicit save (no autosave) --------------------------------
  // Edits only persist when the user presses the Save fit button.
  // markDirty() flags unsaved changes on the button itself
  // (is-dirty class + " •" marker); the Save button's success
  // handler clears the flag again via clearDirty().
  var dirty = false;
  var savedAtEl = document.getElementById("fit-saved-at");
  var descInput = document.getElementById("fit-desc");
  var tagsInput = document.getElementById("fit-tags");
  function markDirty() {
    dirty = true;
    if (saveBtn) {
      saveBtn.classList.add("is-dirty");
      if (!saveBtn.getAttribute("data-fit-label")) {
        saveBtn.setAttribute("data-fit-label", saveBtn.textContent);
      }
      saveBtn.textContent = saveBtn.getAttribute("data-fit-label") + " •";
    }
  }
  function clearDirty() {
    dirty = false;
    if (saveBtn) {
      saveBtn.classList.remove("is-dirty");
      var base = saveBtn.getAttribute("data-fit-label");
      if (base) saveBtn.textContent = base;
    }
  }

  // --- unsaved changes guard ---------------------------------------
  // While the fit has unsaved changes, anything on the page that
  // would drop them asks first in the page's own popup
  // (#fit-leave-modal): a link (caught here, before swap.js sees
  // it) and opening another fit (Your fits, the Import form, a
  // paste). Escape, the backdrop and Stay keep the fit. The
  // workbench always loads whole, so Back, Forward, reloading,
  // closing the tab or typing an address leave the document, and
  // browsers allow only their own prompt there (beforeunload).
  // Once the reader chooses to go, leaving is set and nothing asks
  // again.
  var leaveModal = document.getElementById("fit-leave-modal");
  var leaving = false;
  var leaveGo = null;
  var leaveCopy = {
    leave: ["Leave without saving?", "This fit has changes you haven't saved. Leaving now loses them.", "Leave without saving"],
    replace: ["Open another fit?", "This fit has changes you haven't saved. Opening another fit loses them.", "Open it anyway"]
  };
  function holdUnsaved() {
    return dirty && !leaving && !!state.shipTypeId && document.body.contains(editor);
  }
  function askLeave(kind, proceed) {
    if (!holdUnsaved()) { proceed(); return; }
    var copy = leaveCopy[kind] || leaveCopy.leave;
    if (!leaveModal || !window.EveSynapseModal) {
      if (window.confirm(copy[1])) { leaving = true; proceed(); }
      return;
    }
    leaveModal.querySelector("#fit-leave-title").textContent = copy[0];
    leaveModal.querySelector("#fit-leave-msg").textContent = copy[1];
    leaveModal.querySelector("[data-fit-leave-go]").textContent = copy[2];
    leaveGo = proceed;
    window.EveSynapseModal.open(leaveModal, { onClose: function () { leaveGo = null; } });
  }
  if (leaveModal) {
    leaveModal.querySelector("[data-fit-leave-stay]").addEventListener("click", function () {
      window.EveSynapseModal.close(leaveModal);
    });
    leaveModal.querySelector("[data-fit-leave-go]").addEventListener("click", function () {
      var go = leaveGo;
      leaving = true;
      window.EveSynapseModal.close(leaveModal);
      if (go) go();
    });
  }
  // Links: caught in the capture phase, ahead of the page swapper.
  // Going anyway clicks the link again, so it travels exactly as it
  // would have (swapped or loaded whole). A link to another tab, a
  // download, a modified click or a jump within this page passes.
  document.addEventListener("click", function (ev) {
    if (ev.defaultPrevented || ev.button !== 0 || ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.altKey) return;
    if (!holdUnsaved()) return;
    var a = ev.target && ev.target.closest ? ev.target.closest("a[href]") : null;
    if (!a || a.hasAttribute("download") || (a.target && a.target !== "_self")) return;
    var url;
    try { url = new URL(a.href, window.location.href); } catch (e) { return; }
    if (url.protocol !== "http:" && url.protocol !== "https:") return;
    if (url.origin === window.location.origin && url.pathname === window.location.pathname &&
        url.search === window.location.search) return;
    ev.preventDefault();
    askLeave("leave", function () { a.click(); });
  }, true);
  if (window.addEventListener) {
    window.addEventListener("beforeunload", function (ev) {
      if (!holdUnsaved()) return;
      ev.preventDefault();
      ev.returnValue = "";
    });
  }

  // --- ship-restricted modules -------------------------------------
  // The workbench carries data-fit-restricted for fitted modules:
  // {typeID: "Dreadnoughts"}. Search and Browse rows carry the same
  // for the hull they were asked about (it.only, it.subslot); rowSeen
  // keeps them so a pick or a drag from a row is checked before it
  // lands.
  var rowSeen = {};
  var rowSeenShip = 0;
  function noteRow(it) {
    if (!it || !it.id) return;
    if (rowSeenShip !== state.shipTypeId) { rowSeen = {}; rowSeenShip = state.shipTypeId; }
    rowSeen[it.id] = it;
  }
  function seenRow(typeID) {
    return rowSeenShip === state.shipTypeId ? rowSeen[typeID] || null : null;
  }
  function restrictionFor(typeID) {
    var wb = workbench();
    if (!wb) return "";
    try {
      var m = JSON.parse(wb.getAttribute("data-fit-restricted") || "{}");
      if (m[String(typeID)]) return m[String(typeID)];
    } catch (e) { /* fall through to the rows */ }
    var row = seenRow(typeID);
    return row && row.only ? row.only : "";
  }
  function restrictedSet() {
    var wb = workbench();
    if (!wb) return null;
    try {
      var m = JSON.parse(wb.getAttribute("data-fit-restricted") || "{}");
      return Object.keys(m).length ? m : null;
    } catch (e) { return null; }
  }
  // Plain-language refusal when the user tries to add a module
  // this hull cannot fit. Returns true when blocked.
  function checkRestricted(typeID, name, fam) {
    var req = restrictionFor(typeID);
    if (req) {
      rackRefuse(fam || familyOfType(typeID), (name || "That module") + " can only be fitted to " + req + ".");
      return true;
    }
    return false;
  }

  // Slot families with a fixed maximum: adding past it is a
  // plain-language refusal, never a silent no-op.
  var fitFamLabels = { high: "high", medium: "mid", low: "low", rig: "rig", subsystem: "subsystem" };
  // A hull takes one subsystem of each kind: the fitted subsystem of
  // the same kind as typeID, or null.
  function subsystemTaken(typeID) {
    var row = seenRow(typeID);
    var own = editor.querySelector('.fit-chip[data-fit-rowtype="' + typeID + '"][data-fit-subslot]');
    var kind = (row && row.subslot) || (own && own.getAttribute("data-fit-subslot"));
    if (!kind) return null;
    return editor.querySelector('.fit-chip[data-fit-subslot="' + kind + '"]');
  }
  function addItem(typeID, qty, famHint, cargo) {
    var fam = cargo ? "cargo" : (familyOfType(typeID) || famHint || "");
    if (fam === "subsystem") {
      var taken = subsystemTaken(typeID);
      if (taken) {
        rackRefuse(fam, "This hull already has " + (taken.getAttribute("data-tip-name") || "a subsystem of that kind") + ". Remove it first.");
        return;
      }
    }
    if (fitFamLabels[fam]) {
      var max = 0;
      var g = editor.querySelector('.fit-group[data-fit-group="' + fam + '"]');
      if (g) max = parseInt(g.getAttribute("data-fit-max") || "0", 10) || 0;
      var count = 0;
      for (var i = 0; i < state.items.length; i++) {
        if (state.items[i].cargo) continue;
        if ((familyOfType(state.items[i].typeId) || "") === fam) count += state.items[i].qty || 1;
      }
      if (max > 0 && count >= max) {
        rackRefuse(fam, "No free " + fitFamLabels[fam] + " slots. Remove something first.");
        return;
      }
    }
    pushHistory();
    var i = findItem(typeID, cargo);
    if (i >= 0) {
      state.items[i].qty += qty;
      for (var k = 0; k < qty; k++) state.items[i].states.push("");
    } else {
      var line = { typeId: typeID, qty: qty, states: [] };
      if (cargo) line.cargo = true;
      state.items.push(line);
    }
    normItems();
    markDirty();
    pendingFlash = fam === "drone" || fam === "fighter" || fam === "cargo"
      ? { type: "group", family: fam }
      : { type: "slot", id: typeID, landed: true };
    simulate();
  }
  // addItemAt inserts one module at a doc-order position (drag
  // move/swap), replacing the item list wholesale.
  function setItems(items) {
    pushHistory();
    state.items = items;
    markDirty();
    simulate();
  }
  function decItem(typeID, cargo) {
    var i = findItem(typeID, cargo);
    if (i < 0) return;
    pushHistory();
    var fam = cargo ? "cargo" : (familyOfType(typeID) || "");
    var name = typeNameOf(typeID);
    state.items[i].qty -= 1;
    state.items[i].states.pop();
    if (state.items[i].qty <= 0) state.items.splice(i, 1);
    markDirty();
    // The removed module is gone; flash its group so the removal
    // reads as an action, not a glitch, and offer to take it back.
    pendingFlash = fam ? { type: "group", family: fam, undo: name } : null;
    simulate();
  }
  // toggleFighterAbility switches one ability on a fighter line.
  // The line stores only what differs from the type's defaults
  // (abilitiesOn, abilitiesOff); a squadron runs one movement
  // ability at a time, so switching one on drops the others from
  // abilitiesOn (the server keeps the last one named).
  function toggleFighterAbility(typeID, btn) {
    var i = findItem(typeID);
    if (i < 0) return;
    var key = btn.getAttribute("data-fit-ability");
    var on = btn.getAttribute("aria-pressed") !== "true";
    var def = btn.getAttribute("data-fit-ability-default") === "1";
    pushHistory();
    var it = state.items[i];
    var drop = function (list, k) { return (list || []).filter(function (x) { return x !== k; }); };
    it.abilitiesOn = drop(it.abilitiesOn, key);
    it.abilitiesOff = drop(it.abilitiesOff, key);
    if (on && btn.getAttribute("data-fit-ability-prop") === "1") {
      var props = btn.closest(".fit-ftr-abil").querySelectorAll('[data-fit-ability-prop="1"]');
      for (var p = 0; p < props.length; p++) it.abilitiesOn = drop(it.abilitiesOn, props[p].getAttribute("data-fit-ability"));
    }
    if (on && !def) it.abilitiesOn.push(key);
    if (!on && def) it.abilitiesOff.push(key);
    if (!it.abilitiesOn.length) delete it.abilitiesOn;
    if (!it.abilitiesOff.length) delete it.abilitiesOff;
    markDirty();
    simulate();
  }
  function setItemQty(typeID, qty) {
    pushHistory();
    var i = findItem(typeID);
    if (qty <= 0) {
      if (i >= 0) {
        var fam = familyOfType(typeID) || "";
        if (fam) pendingFlash = { type: "group", family: fam, undo: typeNameOf(typeID) };
        state.items.splice(i, 1);
      }
    } else if (i >= 0) {
      state.items[i].qty = qty;
      normItems();
    } else {
      state.items.push({ typeId: typeID, qty: qty, states: [] });
      normItems();
    }
    markDirty();
    simulate();
  }

  // --- moves between the hull and the cargo hold --------------------
  // A rack row or bay row carried in cargo carries data-fit-cargo.
  function inCargo(el) {
    return !!(el && el.closest && el.closest("[data-fit-cargo]"));
  }
  // takeOne and putOne edit one instance of a line in a copy of the
  // items, so a move lands as one edit (one undo step, one simulate).
  function takeOne(items, typeID, cargo) {
    for (var i = 0; i < items.length; i++) {
      var it = items[i];
      if (it.typeId !== typeID || !!it.cargo !== !!cargo) continue;
      it.qty -= 1;
      if (it.states) it.states.pop();
      if (it.qty <= 0) items.splice(i, 1);
      return true;
    }
    return false;
  }
  function putOne(items, typeID, cargo) {
    for (var i = 0; i < items.length; i++) {
      var it = items[i];
      if (it.typeId !== typeID || !!it.cargo !== !!cargo) continue;
      it.qty += 1;
      if (!cargo) (it.states = it.states || []).push("");
      return;
    }
    var line = { typeId: typeID, qty: 1, states: cargo ? [] : [""] };
    if (cargo) line.cargo = true;
    items.push(line);
  }
  // roomIn says why a rack has no room for one more, or "" when it
  // has (bays and the hold always have room).
  function roomIn(fam) {
    if (!fitFamLabels[fam]) return "";
    var g = editor.querySelector('.fit-group[data-fit-group="' + fam + '"]');
    if (!g) return "This hull has no " + fitFamLabels[fam] + " slots.";
    var used = parseInt(g.getAttribute("data-fit-used") || "0", 10) || 0;
    var max = parseInt(g.getAttribute("data-fit-max") || "0", 10) || 0;
    return used < max ? "" : "No free " + fitFamLabels[fam] + " slots. Remove something first.";
  }
  // moveToCargo puts one fitted module (or bay item) in the hold.
  function moveToCargo(typeID) {
    var items = JSON.parse(JSON.stringify(state.items));
    if (!takeOne(items, typeID, false)) return;
    putOne(items, typeID, true);
    pendingFlash = { type: "group", family: "cargo" };
    setItems(items);
  }
  // moveFromCargo fits one module the hold carries, when its rack
  // has room and the hull takes it.
  function moveFromCargo(typeID, fam) {
    if (checkRestricted(typeID, typeNameOf(typeID), fam)) return;
    var full = roomIn(fam);
    if (full) { rackRefuse(fam, full); return; }
    if (fam === "subsystem" && subsystemTaken(typeID)) {
      rackRefuse(fam, "This hull already has a subsystem of that kind. Remove it first.");
      return;
    }
    var items = JSON.parse(JSON.stringify(state.items));
    if (!takeOne(items, typeID, true)) return;
    putOne(items, typeID, false);
    pendingFlash = fitFamLabels[fam] ? { type: "slot", id: typeID, landed: true } : { type: "group", family: fam };
    setItems(items);
  }
  // swapWithCargo: a module from the hold dropped on a filled slot
  // of its rack takes that slot; the fitted one goes to the hold.
  function swapWithCargo(cargoID, fittedID, fam) {
    if (cargoID === fittedID) return;
    if (checkRestricted(cargoID, typeNameOf(cargoID), fam)) return;
    if (fam === "subsystem") {
      var taken = subsystemTaken(cargoID);
      if (taken && taken.getAttribute("data-fit-rowtype") !== String(fittedID)) {
        rackRefuse(fam, "This hull already has a subsystem of that kind. Remove it first.");
        return;
      }
    }
    var items = JSON.parse(JSON.stringify(state.items));
    if (!takeOne(items, cargoID, true) || !takeOne(items, fittedID, false)) return;
    putOne(items, cargoID, false);
    putOne(items, fittedID, true);
    pendingFlash = { type: "slot", id: cargoID, landed: true };
    setItems(items);
  }
  // swapFitted trades the places of two fitted lines of one rack.
  function swapFitted(a, b) {
    var ia = findItem(a, false), ib = findItem(b, false);
    if (ia < 0 || ib < 0 || ia === ib) return;
    var items = state.items.slice();
    var t = items[ia];
    items[ia] = items[ib];
    items[ib] = t;
    pendingFlash = { type: "slot", id: a };
    setItems(items);
  }
  // swapType fits toID in fromID's place from the info drawer's
  // Compare tab: one fitted module (keeping its state and rack
  // position), or a whole drone or fighter line. Returns "" or the
  // reason it could not.
  function swapType(fromID, toID, name) {
    fromID = parseInt(fromID, 10) || 0;
    toID = parseInt(toID, 10) || 0;
    if (!fromID || !toID || fromID === toID) return "";
    var fam = familyOfType(fromID) || "";
    var req = restrictionFor(toID);
    if (req) return (name || "That module") + " can only be fitted to " + req + ".";
    var items = JSON.parse(JSON.stringify(state.items));
    var at = -1;
    for (var i = 0; i < items.length; i++) {
      if (items[i].typeId === fromID && !items[i].cargo) { at = i; break; }
    }
    if (at < 0) return "That item is no longer fitted.";
    var line = items[at];
    var whole = fam === "drone" || fam === "fighter";
    var n = whole ? (line.qty || 1) : 1;
    var moved = (line.states || []).slice(-n);
    while (moved.length < n) moved.push("");
    line.qty = (line.qty || 1) - n;
    if (line.states) line.states.length = Math.max(0, line.states.length - n);
    var gone = line.qty <= 0;
    if (gone) items.splice(at, 1);
    var into = -1;
    for (var j = 0; j < items.length; j++) {
      if (items[j].typeId === toID && !items[j].cargo) { into = j; break; }
    }
    if (into >= 0) {
      items[into].qty += n;
      items[into].states = (items[into].states || []).concat(moved);
    } else {
      items.splice(gone ? at : at + 1, 0, { typeId: toID, qty: n, states: moved });
    }
    pendingFlash = whole ? { type: "group", family: fam } : { type: "slot", id: toID, landed: true };
    setItems(items);
    return "";
  }
  // The info drawer's Compare tab swaps through these: a fitted
  // module or bay line, or a plugged implant (its chip's Info).
  if (window.FitSheet && window.FitSheet.setSwapper) {
    window.FitSheet.setSwapper(function (op) {
      if (!op || !op.isConnected || !editor.isConnected || !editor.contains(op)) return null;
      var imp = op.closest("[data-fit-imp-info]");
      if (imp) {
        var slot = imp.getAttribute("data-fit-imp-info");
        return { label: "Plug in", apply: function (from, to) { setImplant(slot, parseInt(to, 10) || 0); return ""; } };
      }
      var mod = op.closest(".fit-vslot.filled, .fit-chip[data-fit-rowtype]");
      if (!mod || inCargo(mod)) return null;
      if (!mod.classList.contains("fit-vslot") && !mod.closest(".fit-group")) return null;
      return { label: "Fit", apply: swapType };
    });
  }
  function setShip(typeID, name) {
    pushHistory();
    state.shipTypeId = typeID;
    if (shipInput && typeof name === "string") shipInput.value = name;
    markDirty();
    simulate();
  }

  // --- workbench clicks (delegated: the workbench re-renders) --
  editor.addEventListener("click", function (ev) {
    var t = ev.target;
    if (!t || !t.getAttribute) return;
    var wb = workbench();
    if (wb && wb.contains(t)) {
      // A module's name opens its details (fittip.js's drawer);
      // it neither toggles the row's tooltip nor picks anything.
      if (t.closest("[data-fit-info]")) { closeTip(); return; }
      if (t.closest("[data-fit-undo]")) { undo(); return; }
      var boostAdd = t.closest("[data-fit-boost-add]");
      if (boostAdd) { addBoost(parseInt(boostAdd.getAttribute("data-fit-boost-add"), 10) || 0); return; }
      var boostRemove = t.closest("[data-fit-boost-remove]");
      if (boostRemove) { removeBoost(parseInt(boostRemove.getAttribute("data-fit-boost-remove"), 10)); return; }
      if (t.closest("[data-fit-redo]")) { redo(); return; }
      var searchBtn = t.closest("[data-fit-search-btn]");
      if (searchBtn) { setSearchOpen(!searchOpen); return; }
      var viewBtn = t.closest("[data-fit-view]");
      if (viewBtn) { setFitView(viewBtn.getAttribute("data-fit-view")); return; }
      // Resolve the control from the nearest carrier, never the raw
      // target: a click on a slot's inner glyph must count as a
      // click on the slot (only the border used to respond).
      var ammoClear = t.closest("[data-fit-ammo-clear]");
      if (ammoClear) { setCharge(ammoClear.getAttribute("data-fit-ammo-clear"), 0); return; }
      var ammoReset = t.closest("[data-fit-ammo-reset]");
      if (ammoReset) {
        setChargeAt(ammoReset.getAttribute("data-fit-ammo-reset"), parseInt(ammoReset.getAttribute("data-fit-ammo-index"), 10), 0);
        return;
      }
      var ammoSplit = t.closest("[data-fit-ammo-split]");
      if (ammoSplit) {
        var sw = ammoSplit.getAttribute("data-fit-ammo-split");
        var box = editor.querySelector('[data-fit-ammo-each="' + sw + '"]');
        // A type whose modules load their own stays split until
        // the all-of-them row loads one charge for every module.
        if (box && box.getAttribute("data-fit-ammo-split-on") === "1") return;
        splitOpen[sw] = !splitOpen[sw];
        syncSplitRows();
        return;
      }
      var chargeBtn = t.closest("[data-fit-charge-for]");
      if (chargeBtn) { toggleChargeDrawer(chargeBtn); return; }
      var chargePick = t.closest("[data-fit-charge-pick]");
      if (chargePick) {
        var drawerEl = t.closest("[data-fit-drawer-for]");
        if (drawerEl) {
          setCharge(drawerEl.getAttribute("data-fit-drawer-for"),
            parseInt(chargePick.getAttribute("data-fit-charge-pick"), 10) || 0);
        }
        return;
      }
      var solveBtn = t.closest("[data-fit-solve]");
      if (solveBtn) {
        var solveWrap = solveBtn.closest(".fit-solve");
        var solveOut = solveWrap ? solveWrap.querySelector("[data-fit-solve-out]") : null;
        if (solveOut) runSolve(solveOut);
        return;
      }
      if (t.closest("[data-fit-solve-apply]")) { applySolvePicks(); return; }
      if (t.closest("[data-fit-imp-fold]")) { impOpen = !impOpen; syncImpFold(); return; }
      var fold = t.closest("[data-fit-fold]");
      if (fold) { foldOpen[fold.closest(".fit-group").getAttribute("data-fit-group")] = !fold.closest(".fit-group").classList.contains("open"); syncFold(); return; }
      var impReset = t.closest("[data-fit-imp-reset]");
      if (impReset) { setImplant(impReset.getAttribute("data-fit-imp-reset"), null); return; }
      var impClear = t.closest("[data-fit-imp-clear]");
      if (impClear) { setImplant(impClear.getAttribute("data-fit-imp-clear"), 0); return; }
      var abEl = t.closest("[data-fit-ability]");
      if (abEl) {
        var abRow = abEl.closest("[data-fit-rowtype]");
        if (abRow) toggleFighterAbility(parseInt(abRow.getAttribute("data-fit-rowtype"), 10) || 0, abEl);
        return;
      }
      var decEl = t.closest("[data-fit-dec]");
      if (decEl) { decItem(parseInt(decEl.getAttribute("data-fit-dec"), 10) || 0, inCargo(decEl)); return; }
      var incEl = t.closest("[data-fit-inc]");
      if (incEl) {
        var incID = parseInt(incEl.getAttribute("data-fit-inc"), 10) || 0;
        if (inCargo(incEl)) addItem(incID, 1, "", true);
        else if (!checkRestricted(incID, typeNameOf(incID), "")) addItem(incID, 1);
        return;
      }
      var addEl = t.closest("[data-fit-add]");
      var add = addEl ? addEl.getAttribute("data-fit-add") : "";
      if (add) {
        openBrowse(add === "med" ? "medium" : add, true);
        return;
      }
      // On the phone, in Slot Racks, a tap on a fitted module's
      // card cycles its state; the card's own controls (the
      // steppers above, form fields) keep their behavior, and
      // the state pill is part of the card, not a control.
      var cycChip = t.closest(".fit-chip[data-fit-states]");
      if (cycChip && !t.closest("button, input, select, textarea, a")) {
        if (phoneOn() && fitView === "racks") { cycleChipState(cycChip); return; }
        if (!phoneOn()) { closeTip(); queueStateClick(cycChip, ev); return; }
      }
      // A click anywhere else on a fitted module's row toggles
      // its tooltip, the same one hover shows; it never removes.
      // Removal is the stepper's minus button (handled above) or
      // dragging the ring slot off the display. Form fields
      // inside a row (the drone count) keep their own behavior.
      var chipEl = t.closest(".fit-chip[data-tip-name]");
      if (chipEl && !t.closest("input, select, textarea")) { toggleTip(chipEl); return; }
    }
  });
  editor.addEventListener("change", function (ev) {
    var t = ev.target;
    if (!t || !t.getAttribute) return;
    var qtyFor = t.getAttribute("data-fit-qty");
    if (qtyFor) {
      setItemQty(parseInt(qtyFor, 10) || 0, parseInt(t.value, 10) || 0);
    }
    var boostField = t.getAttribute("data-fit-boost-field");
    var boostRow = boostField && t.closest("[data-fit-boost]");
    if (boostRow) {
      setBoost(parseInt(boostRow.getAttribute("data-fit-boost"), 10), boostField, parseInt(t.value, 10) || 0);
    }
  });

  if (pilotSel) pilotSel.addEventListener("change", function () {
    syncFilterUI();
    refreshCloneOptions();
  });
  // The clone dropdown's options belong to the selected pilot;
  // refresh them on pilot change (back to Active clone), then
  // re-sim with the new pilot's implants.
  function refreshCloneOptions() {
    if (!cloneSel) { simulate(); return; }
    var pid = pilotID();
    function resetToActive() {
      cloneSel.innerHTML = "";
      var opt = document.createElement("option");
      opt.value = "0";
      opt.textContent = "Active clone";
      cloneSel.appendChild(opt);
      cloneSel.value = "0";
      cloneSel.disabled = true;
    }
    if (!pid) { resetToActive(); simulate(); return; }
    fetch("/fittings/clones.json?pilot=" + pid, { headers: { "Accept": "application/json" } })
      .then(function (resp) { return resp.ok ? resp.json() : null; })
      .then(function (data) {
        if (!data || !data.ok || !data.clones || !data.clones.length) { resetToActive(); }
        else {
          cloneSel.innerHTML = "";
          data.clones.forEach(function (c) {
            var opt = document.createElement("option");
            opt.value = String(c.id);
            opt.textContent = c.name;
            cloneSel.appendChild(opt);
          });
          cloneSel.value = "0";
          cloneSel.disabled = false;
        }
        simulate();
      })
      .catch(function () { resetToActive(); simulate(); });
  }
  if (cloneSel) cloneSel.addEventListener("change", function () {
    simulate();
  });
  if (nameInput) {
    nameInput.addEventListener("change", function () {
      state.name = nameInput.value;
    });
  }
  if (descInput) descInput.addEventListener("input", markDirty);
  if (tagsInput) tagsInput.addEventListener("input", markDirty);
  var pubChkInit = document.getElementById("fit-public");
  if (pubChkInit) pubChkInit.addEventListener("change", function () {
    // The public flag persists through the save path.
    var localID = parseInt((document.getElementById("fit-local-id") || {}).value || "0", 10) || 0;
    if (!localID || !state.shipTypeId) return;
    var nm = nameInput ? nameInput.value : (state.name || "");
    fetch("/fittings/save/", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        id: localID, name: nm,
        description: descInput ? descInput.value : "",
        tags: tagsInput ? tagsInput.value : "",
        isPublic: pubChkInit.checked, promote: false,
        fit: { name: nm, shipTypeId: state.shipTypeId, items: state.items, charges: state.charges, implants: state.implants || {} }
      })
    }).catch(function () {});
  });
  syncFilterUI();
  syncSubsystemUI();
  syncDroneGauges();
  syncChargeButtons();
  syncSplitRows();
  // Populate the clone dropdown for the initially-selected pilot
  // (Issue 19); pilot changes refresh it via the change handler.
  refreshCloneOptions();

  // --- split searches: ships at the top, modules above the workbench --
  // Ship search: ships only. Module search: modules/drones/rigs/subs
  // only, with the kind-labeled suggestions and the filter row.
  var modFamily = "all";
  var kindOrder = ["high", "medium", "low", "rig", "subsystem", "drone", "fighter", "charge", "cargo"];
  var kindLabels = {
    high: "High slots", medium: "Mid slots", low: "Low slots",
    rig: "Rigs", subsystem: "Subsystems", drone: "Drones", fighter: "Fighters", charge: "Charges",
    cargo: "Cargo"
  };

  function filterQuery() {
    var meta = metaSel ? metaSel.value : "0";
    var usable = (usableChk && usableChk.checked && pilotID() > 0) ? "1" : "0";
    return "&meta=" + encodeURIComponent(meta) + "&usable=" + usable + "&pilot=" + pilotID();
  }
  // pickerURL always carries the module filters; the ship search
  // passes family=ship and ignores them server-side.
  function pickerURL(family, q) {
    return "/fittings/picker.json?family=" + encodeURIComponent(family) +
      "&q=" + encodeURIComponent(q) + filterQuery() +
      (state.shipTypeId ? "&ship=" + state.shipTypeId : "");
  }

  // Fittable-only: kinds with free capacity on the current hull,
  // from the slot-group Used/Max the workbench already renders.
  function fittableKinds() {
    if (!fittableChk || !fittableChk.checked) return null;
    var wb = workbench();
    if (!wb) return null;
    var groups = wb.querySelectorAll(".fit-group");
    if (!groups.length) return null;
    var ok = {};
    for (var i = 0; i < groups.length; i++) {
      var g = groups[i];
      var key = g.getAttribute("data-fit-group");
      var used = parseInt(g.getAttribute("data-fit-used") || "0", 10) || 0;
      var max = parseInt(g.getAttribute("data-fit-max") || "0", 10) || 0;
      if (key && max > used) ok[key] = true;
    }
    var bw = parseFloat(wb.getAttribute("data-fit-bandwidth") || "0") || 0;
    if (bw > 0) ok["drone"] = true;
    var tubes = parseInt(wb.getAttribute("data-fit-tubes") || "0", 10) || 0;
    if (tubes > 0) ok["fighter"] = true;
    ok["ship"] = true;
    ok["charge"] = true;
    return ok;
  }
  // "Fittable only" hides what cannot go on this hull now; without
  // it every row lists, and rowReason says why one will not fit.
  function applyFittable(rows) {
    var ok = fittableKinds();
    if (!ok) return rows;
    var restricted = restrictedSet();
    return (rows || []).filter(function (it) {
      if ((restricted && restricted[it.id]) || it.only) return false;
      return !!ok[it.kind || ""];
    });
  }

  // rowReason is why a search row will not fit the loaded hull
  // right now, in words, or "" when it will (or no hull is loaded).
  // Over-budget modules can still be added, as in game; the stats
  // panel then shows what is over.
  var rackKinds = { high: 1, medium: 1, low: 1, rig: 1, subsystem: 1 };
  function rowReason(it) {
    var wb = workbench();
    if (!wb || !it || it.kind === "ship" || it.kind === "charge") return "";
    noteRow(it);
    var req = restrictionFor(it.id);
    if (req) return "Only fits " + req;
    if (it.kind === "subsystem" && subsystemTaken(it.id)) return "Its kind is already fitted";
    if (rackKinds[it.kind]) {
      var g = wb.querySelector('.fit-group[data-fit-group="' + it.kind + '"]');
      if (g) {
        var used = parseInt(g.getAttribute("data-fit-used") || "0", 10) || 0;
        var max = parseInt(g.getAttribute("data-fit-max") || "0", 10) || 0;
        if (max <= used) return max ? "No free slot" : "This hull has no slot for it";
      }
    }
    if (it.kind === "fighter" && !(parseInt(wb.getAttribute("data-fit-tubes") || "0", 10) > 0)) return "This hull has no fighter tubes";
    var left = (wb.getAttribute("data-fit-left") || "").split(",");
    var cpuLeft = parseFloat(left[0]), pgLeft = parseFloat(left[1]), calLeft = parseFloat(left[2]);
    if (it.cpu > 0 && !isNaN(cpuLeft) && it.cpu > cpuLeft + 1e-6) return "Needs " + fmtNum(it.cpu) + " tf CPU, " + fmtNum(Math.max(0, cpuLeft)) + " left";
    if (it.pg > 0 && !isNaN(pgLeft) && it.pg > pgLeft + 1e-6) return "Needs " + fmtNum(it.pg) + " MW, " + fmtNum(Math.max(0, pgLeft)) + " left";
    if (it.cal > 0 && !isNaN(calLeft) && it.cal > calLeft + 1e-6) return "Needs " + fmtNum(it.cal) + " calibration, " + fmtNum(Math.max(0, calLeft)) + " left";
    return "";
  }
  function fmtNum(v) {
    return Math.round(v) === v ? String(v) : v.toFixed(1);
  }
  // rowFacts is the right side of a search or Browse row: tier, CPU,
  // powergrid or calibration, the headline figure and the price,
  // then the Info control. It returns null for a row with nothing
  // to say.
  function rowFacts(it) {
    var box = document.createElement("span");
    box.className = "sug-facts";
    // kind is the figure's glyph (the shared fg-* symbols).
    function fact(text, cls, kind) {
      var f = document.createElement("span");
      f.className = "sug-fact" + (cls ? " " + cls : "") + (kind ? " k-" + kind : "");
      if (kind && window.FitTipUI) f.appendChild(FitTipUI.glyph(kind));
      f.appendChild(document.createTextNode(text));
      box.appendChild(f);
    }
    if (it.tier) fact(it.tier, "sug-tier");
    if (it.stat) fact(it.stat, "sug-stat", it.statk || "");
    if (it.cpu) fact(fmtNum(it.cpu) + " tf", "", "cpu");
    if (it.pg) fact(fmtNum(it.pg) + " MW", "", "powergrid");
    if (it.cal) fact(fmtNum(it.cal) + " cal", "", "calibration");
    if (it.price) fact(it.price + (it.est ? " est." : ""), "sug-price");
    if (it.id) {
      var info = document.createElement("button");
      info.type = "button";
      info.className = "sug-info";
      info.setAttribute("data-fit-info", String(it.id));
      info.setAttribute("aria-label", "Details of " + (it.name || "this item"));
      info.textContent = "Info";
      // A press on Info must not pick the row or start a drag.
      info.addEventListener("mousedown", function (ev) { ev.stopPropagation(); ev.preventDefault(); });
      info.addEventListener("pointerdown", function (ev) { ev.stopPropagation(); });
      box.appendChild(info);
    }
    return box;
  }
  function markReason(li, it) {
    var why = rowReason(it);
    if (!why) return null;
    li.classList.add("sug-wontfit");
    var r = document.createElement("span");
    r.className = "sug-why";
    r.textContent = why;
    return r;
  }

  // The type icon a suggestion or Browse row leads with.
  function suggestIcon(id) {
    var img = document.createElement("img");
    img.className = "sug-icon";
    img.alt = "";
    img.loading = "lazy";
    img.draggable = false;
    img.src = "https://images.evetech.net/types/" + id + "/icon?size=32";
    return img;
  }

  // One suggestion engine drives both inputs. cfg: family() returns
  // the endpoint family; kinds limits the groups shown (null =
  // whatever the endpoint returned); dropShips strips ship rows
  // (the module search's "all" chip); pick handles the choice.
  function makeSuggester(input, list, cfg) {
    var timer = null;
    var rows = [];
    var active = -1;
    function close() {
      if (list) list.hidden = true;
      if (input) input.setAttribute("aria-expanded", "false");
      active = -1;
      if (cfg.preview) cfg.preview(null);
    }
    function query(force) {
      if (!input || !list) return;
      var q = input.value.trim();
      if (cfg.elsewhere && cfg.elsewhere()) { close(); return; }
      if (q.length < 2 && !force) { close(); return; }
      fetch(cfg.url ? cfg.url(q) : pickerURL(cfg.family(), q), {
        cache: "no-store", headers: { "Accept": "application/json" }
      }).then(function (resp) {
        return resp.ok ? resp.json() : [];
      }).then(function (data) {
        var filtered = cfg.url ? (data || []) : applyFittable(data || []);
        if (cfg.dropShips) {
          filtered = filtered.filter(function (it) { return (it.kind || "") !== "ship"; });
        }
        render(filtered);
      }).catch(close);
    }
    function render(data) {
      if (!list) return;
      list.innerHTML = "";
      rows = [];
      active = -1;
      var groups = {};
      (data || []).forEach(function (it) {
        var k = it.kind || "";
        if (cfg.kinds && cfg.kinds.indexOf(k) < 0) return;
        // Anything that is not a slot family, drone, fighter or
        // charge is carried in the cargo hold.
        if (!cfg.kinds && kindOrder.indexOf(k) < 0) k = "cargo";
        if (!groups[k]) groups[k] = [];
        groups[k].push(it);
      });
      var order = cfg.kinds || kindOrder;
      var any = false;
      order.forEach(function (k) {
        var items = groups[k];
        if (!items || !items.length) return;
        any = true;
        var head = document.createElement("li");
        head.className = "sug-kind";
        head.textContent = (cfg.labels || kindLabels)[k] || k;
        list.appendChild(head);
        items.forEach(function (it) {
          var li = document.createElement("li");
          li.setAttribute("role", "option");
          li.appendChild(suggestIcon(it.id));
          var text = document.createElement("span");
          text.className = "sug-text";
          var name = document.createElement("span");
          name.textContent = it.name;
          if (it.missing) {
            li.classList.add("sug-missing");
            var miss = document.createElement("span");
            miss.className = "sug-missing-tag";
            miss.textContent = "Missing skills";
            name.appendChild(miss);
          }
          text.appendChild(name);
          if (it.label) {
            var lab = document.createElement("span");
            lab.className = "sug-label";
            lab.textContent = it.label;
            text.appendChild(lab);
          }
          var why = markReason(li, it);
          if (why) text.appendChild(why);
          li.appendChild(text);
          li.appendChild(rowFacts(it));
          rows.push({ it: it, li: li });
          if (cfg.dragRows && it.kind && it.kind !== "ship") {
            // A drag source too (the editor's pointer controller):
            // the press keeps focus in the field, the click picks.
            li.setAttribute("data-fit-pick", String(it.id));
            li.setAttribute("data-kind", browseRowKind(it));
            if (it.cpu) li.setAttribute("data-cpu", fmtNum(it.cpu));
            if (it.pg) li.setAttribute("data-pg", fmtNum(it.pg));
            li._fitRow = it;
            li.addEventListener("mousedown", function (ev) { ev.preventDefault(); });
            li.addEventListener("click", function (ev) { cfg.pick(it, ev.shiftKey); });
          } else {
            li.addEventListener("mousedown", function (ev) {
              ev.preventDefault();
              cfg.pick(it, ev.shiftKey);
            });
          }
          if (cfg.preview) {
            li.addEventListener("mouseenter", function () { cfg.preview(it, li); });
            li.addEventListener("mouseleave", function () { cfg.preview(null); });
          }
          list.appendChild(li);
        });
      });
      if (!any) {
        var li = document.createElement("li");
        li.className = "sug-empty";
        li.textContent = "Nothing found. Try a different search.";
        list.appendChild(li);
      }
      input.setAttribute("aria-expanded", any ? "true" : "false");
      list.hidden = false;
    }
    function move(dir) {
      if (!rows.length) return;
      if (active >= 0 && rows[active]) rows[active].li.classList.remove("active");
      active = (active + dir + rows.length) % rows.length;
      var li = rows[active].li;
      li.classList.add("active");
      if (li.scrollIntoView) li.scrollIntoView({ block: "nearest" });
    }
    if (input) {
      input.addEventListener("input", function () {
        if (timer) window.clearTimeout(timer);
        timer = window.setTimeout(query, 150);
      });
      input.addEventListener("keydown", function (ev) {
        if (!list || list.hidden) return;
        if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
          ev.preventDefault();
          move(ev.key === "ArrowDown" ? 1 : -1);
        } else if (ev.key === "Enter") {
          if (active >= 0 && rows[active]) {
            ev.preventDefault();
            cfg.pick(rows[active].it, ev.ctrlKey || ev.metaKey);
          }
        } else if (ev.key === "Escape") {
          close();
        }
      });
      input.addEventListener("blur", function () {
        window.setTimeout(close, 120);
      });
    }
    return { query: query, close: close };
  }

  // A charge loads into every fitted weapon that takes it; with no
  // such weapon it goes in the cargo hold.
  function pickCharge(it) {
    var rows = editor.querySelectorAll("[data-fit-ammo-for]");
    var weapons = [];
    for (var i = 0; i < rows.length; i++) {
      var ids = (rows[i].getAttribute("data-fit-ammo-ids") || "").split(",");
      if (ids.indexOf(String(it.id)) >= 0) weapons.push(rows[i].getAttribute("data-fit-ammo-for"));
    }
    if (!weapons.length) { addItem(it.id, 1, "cargo", true); return; }
    pushHistory();
    for (var w = 0; w < weapons.length; w++) state.charges[weapons[w]] = it.id;
    markDirty();
    pendingFlash = { type: "charge", id: weapons[0] };
    simulate();
  }

  var shipSearch = makeSuggester(shipInput, shipList, {
    family: function () { return "ship"; },
    kinds: ["ship"],
    labels: { ship: "Ships" },
    pick: function (it) {
      pushHistory();
      state.shipTypeId = it.id;
      state.items = [];
      state.charges = {};
      if (shipInput) shipInput.value = it.name;
      markDirty();
      simulate();
      syncUndoButtons();
      shipSearch.close();
    }
  });
  var modSearch = makeSuggester(modInput, modList, {
    family: function () { return modFamily; },
    // While the Browse pane is in view the search filters its
    // list in place instead of opening a dropdown over it.
    elsewhere: function () {
      if (!browseShown()) return false;
      renderBrowse();
      return true;
    },
    dropShips: true,
    dragRows: true,
    // A pick closes the list (and the phone search panel, a no-op
    // above the band) so the fit is in view; Shift+click or
    // Ctrl+Enter keeps it open to add several.
    pick: function (it, keep) {
      endPreview();
      pickModule(it);
      if (keep) return;
      modSearch.close();
      setSearchOpen(false, false);
    },
    preview: function (it, li) {
      if (it) startPreview(it, li); else endPreview();
    }
  });
  function pickModule(it) {
    var kind = it.kind || "";
    if (kind === "charge") { pickCharge(it); return; }
    if (kind === "high" || kind === "medium" || kind === "low" ||
        kind === "rig" || kind === "subsystem" || kind === "drone" || kind === "fighter") {
      if (checkRestricted(it.id, it.name, kind)) return;
      addItem(it.id, 1, kind);
      return;
    }
    // Anything else goes in the cargo hold.
    addItem(it.id, 1, "cargo", true);
  }

  if (famChips) {
    famChips.addEventListener("click", function (ev) {
      var b = ev.target && ev.target.closest ? ev.target.closest("button[data-fam]") : null;
      if (!b) return;
      setFamChip(b.getAttribute("data-fam"));
    });
  }
  // One kind filter drives the search and the Browse pane: All
  // lists every family (the search leaves out charges, which need
  // a weapon to land), Cargo searches any marketable item.
  function setFamChip(fam) {
    if (!famNames[fam]) fam = "all";
    modFamily = fam;
    if (famChips) {
      var btns = famChips.querySelectorAll("button[data-fam]");
      for (var i = 0; i < btns.length; i++) {
        var on = btns[i].getAttribute("data-fam") === fam;
        btns[i].classList.toggle("on", on);
        btns[i].setAttribute("aria-pressed", on ? "true" : "false");
      }
    }
    syncSearchPlaceholder();
    browseScroll = 0;
    if (browseShown()) renderBrowse(); else modSearch.query();
  }
  function filtersChanged() {
    modSearch.query();
    browseCache = {};
    if (browseTree) browseTree.innerHTML = "";
    if (browseShown()) renderBrowse();
  }
  // --- implants: one search per clone slot --------------------------
  // Each slot's box searches only that slot's implants; picks
  // override the clone there. id null = back to the clone's implant,
  // 0 = slot left empty.
  // Folded lists (cargo past five items) keep their open state
  // across re-renders.
  var foldOpen = {};
  // Implants fold to one line while they are the clone's own set
  // (the server folds them); Edit opens the slots, and they stay
  // open across re-renders until folded again.
  var impOpen = false;
  function syncImpFold() {
    var g = editor.querySelector(".fit-implant-group");
    if (!g) return;
    var btn = g.querySelector("[data-fit-imp-fold]");
    if (!btn) return; // edited sets render open
    g.classList.toggle("fit-imp-open", impOpen);
    btn.setAttribute("aria-expanded", impOpen ? "true" : "false");
    var act = btn.querySelector(".fit-imp-fold-act");
    if (act) act.textContent = impOpen ? "Done" : "Edit";
  }
  function syncFold() {
    var groups = editor.querySelectorAll(".fit-fold");
    for (var i = 0; i < groups.length; i++) {
      var open = !!foldOpen[groups[i].getAttribute("data-fit-group")];
      groups[i].classList.toggle("open", open);
      var btn = groups[i].querySelector("[data-fit-fold]");
      if (!btn) continue;
      btn.setAttribute("aria-expanded", open ? "true" : "false");
      var label = btn.querySelector("span");
      if (label) {
        if (!label.getAttribute("data-closed")) label.setAttribute("data-closed", label.textContent);
        label.textContent = open ? "show fewer" : label.getAttribute("data-closed");
      }
    }
  }
  function setImplant(slot, id) {
    pushHistory();
    state.implants = state.implants || {};
    if (id === null) delete state.implants[slot];
    else state.implants[slot] = id;
    pendingFlash = { type: "group", family: "implant" };
    markDirty();
    simulate();
  }
  // Fleet boosts: a booster's burst charge, hull, skill level and
  // mindlink acting on the fit. The server checks each choice.
  function addBoost(charge) {
    if (!charge) return;
    pushHistory();
    state.boosts = (state.boosts || []).concat([{ charge: charge, level: 5 }]);
    pendingFlash = { type: "group", family: "boosts" };
    markDirty();
    simulate();
  }
  function setBoost(index, field, value) {
    var list = state.boosts || [];
    if (!list[index]) return;
    pushHistory();
    var next = {};
    for (var k in list[index]) next[k] = list[index][k];
    next[field] = value;
    // A new charge may belong to another family: its hull and
    // mindlink choices start over.
    if (field === "charge") { delete next.hull; delete next.mindlink; }
    list = list.slice();
    list[index] = next;
    state.boosts = list;
    markDirty();
    simulate();
  }
  function removeBoost(index) {
    var list = (state.boosts || []).slice();
    if (!list[index]) return;
    pushHistory();
    list.splice(index, 1);
    state.boosts = list;
    markDirty();
    simulate();
  }
  // Weapon ammunition for every module of the type: id 0 unloads.
  // It replaces the charges single modules loaded of their own.
  function setCharge(weapon, id) {
    pushHistory();
    if (id > 0) state.charges[weapon] = id;
    else delete state.charges[weapon];
    weaponLines(weapon).forEach(function (it) { delete it.charges; });
    pendingFlash = { type: "charge", id: weapon };
    markDirty();
    simulate();
  }
  // weaponLines are the fitted document lines of a weapon type.
  function weaponLines(weapon) {
    return (state.items || []).filter(function (it) {
      return String(it.typeId) === String(weapon) && !it.cargo;
    });
  }
  // One module's own charge: index counts the type's fitted modules
  // from 0; id 0 has it load the same as the others. The server
  // folds a charge every module shares into the type's.
  function setChargeAt(weapon, index, id) {
    var line = weaponLines(weapon)[0];
    if (!line || !(index >= 0) || index >= (line.qty || 1)) return;
    pushHistory();
    var list = (line.charges || []).slice();
    while (list.length < (line.qty || 1)) list.push(0);
    list[index] = id > 0 ? id : 0;
    line.charges = list;
    splitOpen[weapon] = true;
    pendingFlash = { type: "charge", id: weapon };
    markDirty();
    simulate();
  }
  // splitOpen remembers which types show a charge row per module,
  // across re-renders; a type whose modules load their own always
  // shows them.
  var splitOpen = {};
  function syncSplitRows() {
    editor.querySelectorAll("[data-fit-ammo-each]").forEach(function (box) {
      var weapon = box.getAttribute("data-fit-ammo-each");
      var on = box.getAttribute("data-fit-ammo-split-on") === "1" || !!splitOpen[weapon];
      box.hidden = !on;
      var btn = editor.querySelector('[data-fit-ammo-split="' + weapon + '"]');
      if (btn) btn.setAttribute("aria-expanded", on ? "true" : "false");
    });
  }
  function splitShown(weapon) {
    var box = editor.querySelector('[data-fit-ammo-each="' + weapon + '"]');
    return !!box && !box.hidden;
  }
  // --- drone gauges: the shared FitGauges pass, on this editor's
  // workbench.
  function syncDroneGauges() {
    FitGauges.apply(workbench());
  }
  // --- inline charge drawer --------------------------------------
  // A fitted module that takes charges (it has an ammunition row)
  // grows a small charge button on its slot-row chip; the button
  // expands a drawer in place listing the charges it can load.
  // Picking one writes through setCharge, the same path the
  // ammunition section's search uses, so the fit document and the
  // engine see no difference. The drawer lives in the DOM the
  // workbench re-renders, so any re-render closes it.
  function syncChargeButtons() {
    var inputs = editor.querySelectorAll("[data-fit-ammo-for]");
    if (!inputs.length) return;
    var byWeapon = {};
    for (var i = 0; i < inputs.length; i++) {
      byWeapon[inputs[i].getAttribute("data-fit-ammo-for")] = inputs[i];
    }
    var chips = editor.querySelectorAll(".fit-chip[data-fit-rowtype]");
    for (var j = 0; j < chips.length; j++) {
      var chip = chips[j];
      var wid = chip.getAttribute("data-fit-rowtype");
      var input = byWeapon[wid];
      if (!input) continue;
      // Slot rows only: bay chips (drones, fighters, cargo) take no charges,
      // and the ammunition section's own chips hold the input.
      if (!chip.closest || !chip.closest(".fit-slots")) continue;
      if (chip.querySelector("[data-fit-charge-for]")) continue;
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "fit-chip-btn fit-charge-btn";
      btn.setAttribute("data-fit-charge-for", wid);
      var loaded = (state.charges || {})[wid];
      if (loaded) {
        var img = document.createElement("img");
        img.src = "https://images.evetech.net/types/" + loaded + "/icon?size=32";
        img.alt = "";
        img.setAttribute("draggable", "false");
        btn.appendChild(img);
        btn.title = "Change charge (loaded: " + (input.value || "charge") + ")";
      } else {
        btn.textContent = "charge";
        btn.title = "Load a charge";
        btn.classList.add("fit-charge-btn-empty");
      }
      // The button leads the row's controls, on one line with the
      // module state and the stepper.
      var host = chip.querySelector(".fit-chip-ctl");
      if (host) host.insertBefore(btn, host.firstChild); else chip.appendChild(btn);
    }
  }
  var chargeDrawerFor = "";
  function closeChargeDrawer() {
    var d = editor.querySelector(".fit-charge-drawer");
    if (d && d.parentNode) d.parentNode.removeChild(d);
    chargeDrawerFor = "";
  }
  function toggleChargeDrawer(btn) {
    var weapon = btn.getAttribute("data-fit-charge-for") || "";
    var chip = btn.closest ? btn.closest(".fit-chip") : null;
    if (!chip || !weapon) return;
    if (chargeDrawerFor === weapon) { closeChargeDrawer(); return; }
    closeChargeDrawer();
    chargeDrawerFor = weapon;
    var drawer = document.createElement("div");
    drawer.className = "fit-charge-drawer";
    drawer.setAttribute("data-fit-drawer-for", weapon);
    var head = document.createElement("div");
    head.className = "fit-charge-drawer-head";
    head.textContent = "Charges: " + (chip.getAttribute("data-tip-name") || "module");
    drawer.appendChild(head);
    var list = document.createElement("div");
    list.className = "fit-charge-list";
    list.textContent = "Loading…";
    drawer.appendChild(list);
    var parent = chip.parentNode;
    if (parent) {
      if (chip.nextSibling) parent.insertBefore(drawer, chip.nextSibling);
      else parent.appendChild(drawer);
    }
    fetch("/fittings/picker.json?family=charge&weapon=" + encodeURIComponent(weapon) + "&q=", {
      headers: { "Accept": "application/json" }
    }).then(function (resp) {
      return resp.ok ? resp.json() : null;
    }).then(function (rows) {
      fillChargeDrawer(drawer, weapon, rows);
    }).catch(function () { fillChargeDrawer(drawer, weapon, null); });
  }
  function fillChargeDrawer(drawer, weapon, rows) {
    var list = drawer.querySelector(".fit-charge-list");
    if (!list) return; // the drawer closed while loading
    list.textContent = "";
    var loaded = (state.charges || {})[weapon];
    if (!rows || !rows.length) {
      list.textContent = "No charges fit this module.";
      return;
    }
    rows.forEach(function (row) {
      var b = document.createElement("button");
      b.type = "button";
      b.className = "fit-charge-opt" + (String(row.id) === String(loaded || 0) ? " on" : "");
      b.setAttribute("data-fit-charge-pick", row.id);
      var img = document.createElement("img");
      img.src = "https://images.evetech.net/types/" + row.id + "/icon?size=32";
      img.alt = "";
      img.setAttribute("draggable", "false");
      b.appendChild(img);
      var nm = document.createElement("span");
      nm.textContent = row.name;
      b.appendChild(nm);
      list.appendChild(b);
    });
    if (loaded) {
      var un = document.createElement("button");
      un.type = "button";
      un.className = "fit-charge-opt fit-charge-unload";
      un.setAttribute("data-fit-charge-pick", "0");
      un.textContent = "Unload";
      list.appendChild(un);
    }
  }
  // --- implant solver --------------------------------------------
  // The Doesn't-fit ribbon's button asks the server which
  // hardwiring makes the fit legal; the answer renders in place,
  // and Apply writes the picks into the document's implant
  // overrides, a simulation overlay on the clone, cleared by the
  // same reset the implant chips already carry.
  var solvePicks = null;
  function fmtISK(v) {
    var s = String(Math.round(v));
    var out = "";
    while (s.length > 3) {
      out = "," + s.slice(-3) + out;
      s = s.slice(0, -3);
    }
    return s + out + " ISK";
  }
  function runSolve(out) {
    solvePicks = null;
    out.hidden = false;
    out.textContent = "Checking the implant ladder…";
    var payload = {
      name: state.name,
      shipTypeId: state.shipTypeId,
      items: state.items,
      charges: state.charges,
      implants: state.implants || {},
      boosts: state.boosts || [],
      pilot: pilotID(),
      clone: cloneID()
    };
    fetch("/fittings/solve-implants/", {
      method: "POST",
      headers: { "Content-Type": "application/json", "Accept": "application/json" },
      body: JSON.stringify(payload)
    }).then(function (resp) {
      return resp.ok ? resp.json() : null;
    }).then(function (data) {
      out.textContent = "";
      if (!data || !data.ok) {
        out.textContent = (data && data.error) || "Couldn't check implants just now.";
        return;
      }
      if (!data.solved) {
        out.textContent = data.note || "No implant makes this fit legal.";
        return;
      }
      solvePicks = data.picks || [];
      solvePicks.forEach(function (p) {
        var line = document.createElement("div");
        line.className = "fit-solve-pick";
        var nm = document.createElement("strong");
        nm.textContent = p.name;
        line.appendChild(nm);
        var detail = ", slot " + p.slot + " · +" + p.bonus + "% " + p.resource;
        if (p.priceKnown) detail += " · " + fmtISK(p.price);
        if (p.replacesName) detail += " · replaces your " + p.replacesName + " in this simulation";
        var detailEl = document.createElement("span");
        detailEl.textContent = detail;
        line.appendChild(detailEl);
        out.appendChild(line);
      });
      var apply = document.createElement("button");
      apply.type = "button";
      apply.className = "fit-solve-apply";
      apply.setAttribute("data-fit-solve-apply", "1");
      apply.textContent = solvePicks.length > 1 ? "Apply both implants" : "Apply implant";
      out.appendChild(apply);
      var note = document.createElement("div");
      note.className = "fit-solve-note";
      note.textContent = "Simulation only: your clone isn't changed.";
      out.appendChild(note);
    }).catch(function () {
      out.textContent = "Couldn't check implants just now.";
    });
  }
  function applySolvePicks() {
    if (!solvePicks || !solvePicks.length) return;
    pushHistory();
    state.implants = state.implants || {};
    solvePicks.forEach(function (p) { state.implants[p.slot] = p.typeId; });
    solvePicks = null;
    pendingFlash = { type: "group", family: "implant" };
    markDirty();
    simulate();
  }
  // Row searches are built on first focus (the workbench re-renders):
  // an implant slot lists that slot's implants, a weapon's ammunition
  // row the charges it takes.
  function rowSearchConfig(input) {
    var slot = input.getAttribute("data-fit-imp-slot");
    if (slot) {
      return {
        url: function (q) {
          return "/fittings/picker.json?family=implant&slot=" + encodeURIComponent(slot) +
            "&q=" + encodeURIComponent(q) + "&pilot=" + pilotID();
        },
        kinds: ["implant"],
        labels: { implant: "Slot " + slot + " implants" },
        pick: function (it) { setImplant(slot, it.id); }
      };
    }
    var one = input.getAttribute("data-fit-ammo-one");
    if (one) {
      var oneIndex = parseInt(input.getAttribute("data-fit-ammo-index"), 10);
      return {
        url: function (q) {
          return "/fittings/picker.json?family=charge&weapon=" + encodeURIComponent(one) +
            "&q=" + encodeURIComponent(q);
        },
        kinds: ["charge"],
        labels: { charge: (input.getAttribute("data-fit-ammo-weapon") || "Module") + " charges" },
        pick: function (it) { setChargeAt(one, oneIndex, it.id); }
      };
    }
    var weapon = input.getAttribute("data-fit-ammo-for");
    if (weapon) {
      return {
        url: function (q) {
          return "/fittings/picker.json?family=charge&weapon=" + encodeURIComponent(weapon) +
            "&q=" + encodeURIComponent(q);
        },
        kinds: ["charge"],
        labels: { charge: (input.getAttribute("data-fit-ammo-weapon") || "Module") + " charges" },
        pick: function (it) { setCharge(weapon, it.id); }
      };
    }
    return null;
  }
  editor.addEventListener("focusin", function (ev) {
    var input = ev.target;
    if (!input || !input.getAttribute) return;
    if (!input._fitSug) {
      var cfg = rowSearchConfig(input);
      if (!cfg) return;
      var list = document.createElement("ul");
      list.className = "suggest";
      list.setAttribute("role", "listbox");
      list.hidden = true;
      input.parentNode.appendChild(list);
      var pick = cfg.pick;
      cfg.pick = function (it) {
        input._fitSug.close();
        input.value = it.name;
        pick(it);
      };
      input._fitSug = makeSuggester(input, list, cfg);
      // A box left without a pick shows the slot's implant again.
      input.addEventListener("blur", function () {
        window.setTimeout(function () { input.value = input.defaultValue; }, 150);
      });
    }
    // Open on the whole slot; typing narrows it.
    input.value = "";
    input._fitSug.query(true);
  });

  if (fittableChk) fittableChk.addEventListener("change", filtersChanged);
  if (usableChk) usableChk.addEventListener("change", filtersChanged);
  if (metaSel) metaSel.addEventListener("change", filtersChanged);

  // --- your-fits search --------------------------------------------
  // Finds one of the user's saved fits and loads it into the
  // editor. With "Include community fits" checked, other users'
  // public fits appear too (labeled with the author's character
  // name); loading one forks it as the user's own draft.
  var yourfitsInput = document.getElementById("fit-yourfits-search");
  var yourfitsList = document.getElementById("fit-yourfits-suggest");
  var communityChk = document.getElementById("fit-community");
  if (yourfitsInput && yourfitsList) {
    var yfTimer = null;
    var yfRows = [];
    var yfActive = -1;
    function yfClose() {
      yourfitsList.hidden = true;
      yourfitsInput.setAttribute("aria-expanded", "false");
      yfActive = -1;
    }
    function yfLoad(it) {
      yfClose();
      yourfitsInput.value = "";
      askLeave("replace", function () { yfOpen(it); });
    }
    function yfOpen(it) {
      if (it.mine) {
        if (it.isESI) {
          // ESI fit: negative ID, load via ?esi= parameter
          window.location.href = "/fittings/?esi=" + (-it.id) + "#fit-editor";
        } else {
          window.location.href = "/fittings/?local=" + it.id + "#fit-editor";
        }
        return;
      }
      fetch("/fittings/fork/", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ id: it.id })
      }).then(function (resp) {
        return resp.ok ? resp.json() : null;
      }).then(function (data) {
        if (data && data.id) {
          window.location.href = "/fittings/?local=" + data.id + "#fit-editor";
        }
      }).catch(function () {});
    }
    function yfRender() {
      yourfitsList.innerHTML = "";
      yfActive = -1;
      if (!yfRows.length) { yfClose(); return; }
      yfRows.forEach(function (it, i) {
        var li = document.createElement("li");
        li.setAttribute("role", "option");
        li.id = "yf-sug-" + i;
        var name = document.createElement("span");
        name.textContent = it.name || "Unnamed fit";
        li.appendChild(name);
        var lab = document.createElement("span");
        lab.className = "sug-label";
        lab.textContent = (it.shipName || "") + (it.author ? " · by " + it.author : "") + (it.isPublic && !it.mine ? " · public" : "");
        li.appendChild(lab);
        li.addEventListener("mousedown", function (ev) {
          ev.preventDefault();
          yfLoad(it);
        });
        yourfitsList.appendChild(li);
      });
      yourfitsList.hidden = false;
      yourfitsInput.setAttribute("aria-expanded", "true");
    }
    function yfQuery() {
      var q = yourfitsInput.value.trim();
      if (q.length < 2) { yfClose(); return; }
      var url = "/fittings/mine.json?q=" + encodeURIComponent(q) +
        (communityChk && communityChk.checked ? "&community=1" : "");
      fetch(url, { headers: { "Accept": "application/json" } }).then(function (resp) {
        return resp.ok ? resp.json() : [];
      }).then(function (rows) {
        yfRows = rows || [];
        yfRender();
      }).catch(function () {});
    }
    yourfitsInput.addEventListener("input", function () {
      if (yfTimer) clearTimeout(yfTimer);
      yfTimer = setTimeout(yfQuery, 180);
    });
    yourfitsInput.addEventListener("keydown", function (ev) {
      var items = yourfitsList.querySelectorAll("li");
      if (ev.key === "Escape") { yfClose(); return; }
      if (!items.length) return;
      if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
        ev.preventDefault();
        yfActive = ev.key === "ArrowDown"
          ? (yfActive + 1) % items.length
          : (yfActive - 1 + items.length) % items.length;
        for (var i = 0; i < items.length; i++) {
          items[i].classList.toggle("active", i === yfActive);
          items[i].setAttribute("aria-selected", i === yfActive ? "true" : "false");
        }
        yourfitsInput.setAttribute("aria-activedescendant", "yf-sug-" + yfActive);
      } else if (ev.key === "Enter" && yfActive >= 0 && yfRows[yfActive]) {
        ev.preventDefault();
        yfLoad(yfRows[yfActive]);
      }
    });
    yourfitsInput.addEventListener("blur", function () {
      setTimeout(yfClose, 150);
    });
    if (communityChk) communityChk.addEventListener("change", yfQuery);
  }

  // --- your-fits list search + sort (anchor section) ---------------
  (function () {
    var q = document.getElementById("fit-local-q");
    var sortSel = document.getElementById("fit-local-sort");
    var list = document.getElementById("fit-local-list");
    if (!q || !sortSel || !list) return;
    function apply() {
      var term = q.value.trim().toLowerCase();
      var secs = Array.prototype.slice.call(list.querySelectorAll("section[data-fit-name]"));
      secs.forEach(function (s) {
        var name = (s.getAttribute("data-fit-name") || "").toLowerCase();
        var ship = (s.getAttribute("data-fit-ship") || "").toLowerCase();
        var hit = !term || name.indexOf(term) >= 0 || ship.indexOf(term) >= 0;
        s.style.display = hit ? "" : "none";
      });
      var mode = sortSel.value;
      var desc = mode.slice(-4) === "desc";
      var key = mode.split("-")[0];
      secs.sort(function (a, b) {
        var x = a.getAttribute("data-fit-" + key) || "";
        var y = b.getAttribute("data-fit-" + key) || "";
        var c = x.toLowerCase().localeCompare(y.toLowerCase());
        return desc ? -c : c;
      });
      secs.forEach(function (s) { list.appendChild(s); });
    }
    q.addEventListener("input", apply);
    sortSel.addEventListener("change", apply);
  })();

  // --- Browse pane ---------------------------------------------------
  // The left column's second tab (a fourth view on phones): the
  // market groups of one slot family as a tree, with a search on
  // top. The page has one panel; mountBrowse moves it into every
  // fresh workbench, so open branches and the search survive edits.
  // Rows that will not fit now list dimmed with the reason (the
  // module search's rowReason); "Fittable only" hides them.
  var leftPane = "fit";
  var browseCache = {};
  var browseOpen = {};
  var browseScroll = 0;
  var browseRackFams = { high: 1, medium: 1, low: 1, rig: 1, subsystem: 1, drone: 1, fighter: 1 };
  var famNames = {
    all: "modules, drones, rigs", high: "high slot modules", medium: "mid slot modules",
    low: "low slot modules", rig: "rigs", subsystem: "subsystems", drone: "drones",
    fighter: "fighters", charge: "ammunition and charges", any: "any item for the cargo hold"
  };
  function browseShown() {
    return phoneOn() ? fitView === "browse" : leftPane === "browse";
  }
  function mountBrowse() {
    var wb = workbench();
    if (!wb || !browse) return;
    var slot = wb.querySelector("[data-fit-browse-slot]");
    var racks = wb.querySelector(".fit-racks");
    var shown = browseShown();
    if (slot && browse.parentNode !== slot) {
      slot.appendChild(browse);
      if (browseTree) browseTree.scrollTop = browseScroll;
    }
    browse.hidden = false;
    if (slot) slot.hidden = !shown;
    if (racks) racks.hidden = shown;
    var tabs = wb.querySelectorAll("[data-fit-pane]");
    for (var i = 0; i < tabs.length; i++) {
      var on = tabs[i].getAttribute("data-fit-pane") === (shown ? "browse" : "fit");
      tabs[i].classList.toggle("on", on);
      tabs[i].setAttribute("aria-selected", on ? "true" : "false");
    }
    if (shown && browseTree && !browseTree.firstChild) renderBrowse();
  }
  // Switching panes hands the search over: Browse lists it in
  // place, Fit answers it with the dropdown again.
  function setLeftPane(pane) {
    var was = browseShown();
    leftPane = pane === "browse" ? "browse" : "fit";
    mountBrowse();
    if (was !== browseShown()) searchHandover();
  }
  // Outside Browse the search keeps its own wording; in Browse it
  // names what the list holds.
  function syncSearchPlaceholder() {
    if (!modInput) return;
    if (!modInput.hasAttribute("data-fit-ph")) modInput.setAttribute("data-fit-ph", modInput.placeholder);
    modInput.placeholder = browseShown() ? "Search " + famNames[modFamily] : modInput.getAttribute("data-fit-ph");
  }
  function searchHandover() {
    syncSearchPlaceholder();
    if (browseShown()) {
      modSearch.close();
      renderBrowse();
    } else if (modInput && modInput.value.trim().length >= 2) {
      if (browseTree) browseTree.innerHTML = "";
    }
  }
  // openBrowse shows the Browse pane on one family (the "+" of an
  // empty slot, a bay's add button).
  function openBrowse(fam, focus) {
    if (modInput) modInput.value = "";
    modSearch.close();
    if (phoneOn()) {
      setSearchOpen(false, false);
      setFitView("browse");
    } else {
      setLeftPane("browse");
    }
    setFamChip(fam);
    if (browse && browse.scrollIntoView) browse.scrollIntoView({ block: "nearest" });
    if (focus && !phoneOn() && modInput) {
      try { modInput.focus({ preventScroll: true }); } catch (e) { modInput.focus(); }
    }
  }
  function browseURL(group) {
    return "/fittings/browse.json?family=" + encodeURIComponent(modFamily) +
      "&group=" + group + filterQuery() +
      (state.shipTypeId ? "&ship=" + state.shipTypeId : "");
  }
  function fetchBrowse(group) {
    var url = browseURL(group);
    if (browseCache[url]) return Promise.resolve(browseCache[url]);
    return fetch(url, { headers: { "Accept": "application/json" } }).then(function (resp) {
      return resp.ok ? resp.json() : { groups: [], types: [] };
    }).then(function (data) {
      browseCache[url] = data;
      return data;
    });
  }
  // browseRowKind is what a row is to the drag and tap rules: its
  // slot family, "charge", or "cargo" for anything else.
  function browseRowKind(it) {
    var k = it.kind || "";
    if (browseRackFams[k] || k === "charge") return k;
    return "cargo";
  }
  function browseRow(it) {
    var li = document.createElement("li");
    li.className = "fbt-row";
    li.setAttribute("role", "treeitem");
    li.tabIndex = 0;
    li.setAttribute("data-fit-pick", String(it.id));
    li.setAttribute("data-kind", browseRowKind(it));
    if (it.cpu) li.setAttribute("data-cpu", fmtNum(it.cpu));
    if (it.pg) li.setAttribute("data-pg", fmtNum(it.pg));
    li._fitRow = it;
    li.appendChild(suggestIcon(it.id));
    var text = document.createElement("span");
    text.className = "sug-text";
    var name = document.createElement("span");
    name.className = "sug-name";
    name.textContent = it.name;
    text.appendChild(name);
    if (it.label) {
      var lab = document.createElement("span");
      lab.className = "sug-label";
      lab.textContent = it.label;
      text.appendChild(lab);
    }
    li.appendChild(text);
    li.appendChild(rowFacts(it));
    markBrowseRow(li);
    return li;
  }
  // markBrowseRow (re)states why a row will not fit the fit as it
  // stands; every simulate calls it again over the open rows.
  function markBrowseRow(li) {
    var it = li._fitRow;
    if (!it) return;
    li.classList.remove("sug-wontfit");
    var old = li.querySelector(".sug-why");
    if (old) old.remove();
    var why = markReason(li, it);
    var text = li.querySelector(".sug-text");
    if (why && text) text.appendChild(why);
  }
  function refreshBrowseRows() {
    if (!browseTree) return;
    var rows = browseTree.querySelectorAll(".fbt-row");
    for (var i = 0; i < rows.length; i++) markBrowseRow(rows[i]);
  }
  function browseEmpty(ul, msg) {
    var li = document.createElement("li");
    li.className = "fbt-empty";
    li.textContent = msg;
    ul.appendChild(li);
  }
  // Rows whose kind is a rack or bay family answer to Fittable
  // only; charges and cargo never do.
  function browseFittable(rows) {
    return (browseRackFams[modFamily] || modFamily === "all") ? applyFittable(rows) : rows;
  }
  function fillLevel(ul, data) {
    ul.innerHTML = "";
    var open = browseOpen[modFamily] || (browseOpen[modFamily] = {});
    (data.groups || []).forEach(function (g) {
      var li = document.createElement("li");
      li.className = "fbt-node";
      li.setAttribute("role", "treeitem");
      li.setAttribute("aria-expanded", "false");
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "fbt-toggle";
      btn.setAttribute("data-browse-group", String(g.id));
      var nm = document.createElement("span");
      nm.className = "fbt-name";
      nm.textContent = g.name;
      var n = document.createElement("span");
      n.className = "fbt-count";
      n.textContent = String(g.count);
      btn.appendChild(nm);
      btn.appendChild(n);
      li.appendChild(btn);
      var sub = document.createElement("ul");
      sub.setAttribute("role", "group");
      sub.hidden = true;
      li.appendChild(sub);
      ul.appendChild(li);
      if (open[g.id]) toggleBrowseNode(li, true);
    });
    var rows = browseFittable(data.types || []);
    rows.forEach(function (it) { ul.appendChild(browseRow(it)); });
    if (!ul.firstChild) browseEmpty(ul, "Nothing here with these filters.");
  }
  function toggleBrowseNode(li, want) {
    var btn = li.querySelector(".fbt-toggle");
    var sub = li.querySelector("ul");
    if (!btn || !sub) return;
    var id = parseInt(btn.getAttribute("data-browse-group"), 10) || 0;
    var open = browseOpen[modFamily] || (browseOpen[modFamily] = {});
    var on = want === undefined ? li.getAttribute("aria-expanded") !== "true" : want;
    li.setAttribute("aria-expanded", on ? "true" : "false");
    sub.hidden = !on;
    if (on) open[id] = true; else delete open[id];
    if (on && !sub.firstChild) {
      var fam = modFamily;
      fetchBrowse(id).then(function (data) {
        if (fam !== modFamily) return;
        fillLevel(sub, data);
        if (want === undefined) revealBranch(li);
      }).catch(function () {});
    } else if (on && want === undefined) {
      revealBranch(li);
    }
  }
  // revealBranch scrolls the tree so a branch the user just opened
  // shows its first rows.
  function revealBranch(li) {
    if (!browseTree || !li.getBoundingClientRect) return;
    if (browseTree.scrollHeight <= browseTree.clientHeight) {
      // The tree flows with the page (phones): scroll the page.
      if (li.scrollIntoView) li.scrollIntoView({ block: "nearest" });
      return;
    }
    var box = browseTree.getBoundingClientRect();
    var r = li.getBoundingClientRect();
    if (r.bottom > box.bottom) {
      browseTree.scrollTop += Math.min(r.top - box.top, r.bottom - box.bottom);
    }
  }
  function browseQuery() { return modInput ? modInput.value.trim() : ""; }
  function renderBrowse() {
    if (!browseTree) return;
    var q = browseQuery();
    var fam = modFamily;
    if (browseHint) {
      browseHint.textContent = fam === "any"
        ? "Search any item to carry in the cargo hold."
        : fam === "charge" ? "Tap to load every weapon that takes it, or drag onto a weapon."
        : "Tap to fit, or drag onto an empty slot.";
    }
    if (q.length >= 2 || fam === "any") {
      browseTree.setAttribute("role", "listbox");
      browseTree.innerHTML = "";
      if (q.length < 2) return;
      fetch(pickerURL(fam, q), { headers: { "Accept": "application/json" } }).then(function (resp) {
        return resp.ok ? resp.json() : [];
      }).then(function (rows) {
        if (fam !== modFamily || browseQuery() !== q) return;
        browseTree.innerHTML = "";
        rows = browseFittable(rows || []);
        rows.forEach(function (it) {
          if (it.kind === "ship") return;
          browseTree.appendChild(browseRow(it));
        });
        if (!browseTree.firstChild) browseEmpty(browseTree, "Nothing found. Try a different search.");
      }).catch(function () {});
      return;
    }
    browseTree.setAttribute("role", "tree");
    fetchBrowse(0).then(function (data) {
      if (fam !== modFamily || browseQuery().length >= 2) return;
      fillLevel(browseTree, data);
      browseTree.scrollTop = browseScroll;
    }).catch(function () {});
  }
  // pickRow adds what a Browse row (or a module search row) is: a
  // module or drone to its rack or bay, a charge into the weapons
  // that take it, anything else to the cargo hold. On a phone the
  // pick goes back to the racks so the change is in view.
  function pickRow(typeID, kind, name) {
    if (kind === "charge") pickCharge({ id: typeID, name: name });
    else if (kind === "cargo") addItem(typeID, 1, "cargo", true);
    else if (!checkRestricted(typeID, name, kind)) addItem(typeID, 1, kind);
    else return;
    if (phoneOn() && fitView === "browse") setFitView("racks");
  }
  if (browse) {
    if (browseTree) {
      browseTree.addEventListener("scroll", function () { browseScroll = browseTree.scrollTop; });
    }
    browse.addEventListener("click", function (ev) {
      var t = ev.target && ev.target.closest ? ev.target.closest(".fbt-toggle") : null;
      if (t) toggleBrowseNode(t.closest(".fbt-node"));
    });
    browse.addEventListener("keydown", function (ev) {
      if (ev.key !== "Enter") return;
      var row = ev.target && ev.target.closest ? ev.target.closest(".fbt-row") : null;
      if (!row) return;
      ev.preventDefault();
      var it = row._fitRow || {};
      pickRow(it.id, row.getAttribute("data-kind") || "", it.name || "");
    });
  }
  // The Fit / Browse tabs ride the re-rendered fragment: one
  // delegated listener on the editor.
  editor.addEventListener("click", function (ev) {
    var b = ev.target && ev.target.closest ? ev.target.closest("[data-fit-pane]") : null;
    if (b) setLeftPane(b.getAttribute("data-fit-pane"));
  });

  // --- pointer drag and drop --------------------------------------
  // In-game behavior, on mouse AND touch: a tap shows the tooltip or
  // adds the row; a drag moves things. Sources: ring slots, rack,
  // bay and cargo rows, Browse rows and module search rows. What a
  // drop does is dropAction's call; a drop that would replace or
  // remove a module it was not asked to is never one (my ruling,
  // 2026-11-10: on a filled slot, swap places if they can,
  // otherwise nothing happens).
  //
  // Pointer Events, not HTML5 drag-and-drop: mobile browsers never
  // fire dragstart/dragover/drop for touch. One controller serves
  // mouse and touch; on touch a long press starts a drag too.
  var DRAG_PX = 12;
  var LONG_PRESS_MS = 450;
  var dragState = null; // pending or active pointer drag
  var eatNextClick = false; // the synthetic click after a drag-source pointerup
  var rackFams = { high: 1, medium: 1, low: 1, rig: 1, subsystem: 1 };
  var bayFams = { drone: 1, fighter: 1 };
  var famWords = { high: "high slots", medium: "mid slots", low: "low slots", rig: "rig slots", subsystem: "subsystem slots", drone: "the drone bay", fighter: "the fighter bay", cargo: "the cargo hold" };
  function dragSourceAt(ev) {
    var t = ev.target;
    if (!t || !t.closest) return null;
    // A row's name is its details link on a click, and still a
    // handle to drag the row by.
    var byName = t.closest(".fit-chip > .fit-chip-main > .fit-chip-name");
    if (!byName && t.closest("[data-fit-info], .fit-tip")) return null;
    var row = t.closest(".fit-vslot.filled, [data-fit-pick]");
    if (row) return row;
    // A rack, bay or cargo row drags from anywhere but its own
    // controls (steppers, count field).
    if (!byName && t.closest("button, input, select, textarea, a")) return null;
    return t.closest(".fit-group .fit-chip[data-fit-rowtype]");
  }
  // sourceOf describes what is being dragged: where it comes from
  // ("fit" on the hull or in a bay, "cargo" carried in the hold,
  // "new" from a list) and the family it belongs to.
  function sourceOf(el) {
    var id = parseInt(el.getAttribute("data-fit-vslot") || el.getAttribute("data-fit-pick") || el.getAttribute("data-fit-rowtype") || "0", 10) || 0;
    if (!id) return null;
    if (el.hasAttribute("data-fit-vslot")) {
      return { typeID: id, origin: "fit", family: el.getAttribute("data-v-family") || "", kind: "" };
    }
    if (el.hasAttribute("data-fit-pick")) {
      var kind = el.getAttribute("data-kind") || "cargo";
      return { typeID: id, origin: "new", family: kind === "charge" ? "" : kind, kind: kind };
    }
    if (el.hasAttribute("data-fit-cargo")) {
      return { typeID: id, origin: "cargo", family: el.getAttribute("data-fit-home") || "", kind: "" };
    }
    var g = el.closest(".fit-group");
    var fam = g ? g.getAttribute("data-fit-group") || "" : "";
    return { typeID: id, origin: fam === "cargo" ? "hold" : "fit", family: fam, kind: "" };
  }
  // targetAt reads what is under the pointer: a slot (ring circle,
  // rack row, empty-slot button) of a family, a group (rack, bay,
  // cargo hold), the ring's own face, or off the fit,
  // which is everywhere else, the Browse list and the rest of the
  // workbench included: like the game, a module dragged off the
  // fit comes off.
  function targetAt(x, y) {
    var el = document.elementFromPoint ? document.elementFromPoint(x, y) : null;
    if (!el || !el.closest) return { off: true };
    var wb = workbench();
    var slot = el.closest(".fit-vslot");
    if (slot) {
      var filled = slot.classList.contains("filled");
      return {
        el: slot, slot: true, family: slot.getAttribute("data-v-family") || "", filled: filled,
        typeID: filled ? parseInt(slot.getAttribute("data-fit-vslot") || "0", 10) || 0 : 0,
        index: parseInt(slot.getAttribute("data-v-index") || "-1", 10)
      };
    }
    var group = el.closest(".fit-group[data-fit-group]");
    if (group && wb && wb.contains(group)) {
      var fam = group.getAttribute("data-fit-group") || "";
      var chip = el.closest(".fit-chip[data-fit-rowtype]");
      if (rackFams[fam] && chip) {
        return { el: chip, slot: true, family: fam, filled: true, typeID: parseInt(chip.getAttribute("data-fit-rowtype"), 10) || 0, index: -1 };
      }
      var empty = el.closest(".fit-slot-empty");
      if (rackFams[fam] && empty) return { el: empty, slot: true, family: fam, filled: false, typeID: 0, index: -1 };
      return { el: group, group: true, family: fam };
    }
    // The ring's face: a new module lands in its own rack there; a
    // short drag of a fitted one that ends on it changes nothing.
    var face = wb && wb.contains(el) ? el.closest(".fit-visual") : null;
    if (face) return { el: face, ring: true };
    return { off: true };
  }
  // dropAction decides what a drop of d on t does: { act } to do
  // it, or { why } saying why not ("" when there is nothing to
  // say). copy is Ctrl held: a fitted module dropped on an empty
  // slot of its rack is copied there.
  function dropAction(d, t, copy) {
    var id = d.typeID;
    if (t.off) {
      if (d.origin === "fit" || d.origin === "hold") return { act: function () { decItem(id); }, why: "Release to remove" };
      if (d.origin === "cargo") return { act: function () { decItem(id, true); }, why: "Release to remove" };
      return { why: "" };
    }
    if (t.none) return { why: "" };
    if (t.ring) return ringDrop(d);
    if (d.kind === "charge") {
      if (t.slot && t.filled) return { act: function () { loadChargeOn(id, t); } };
      if (t.group && t.family === "cargo") return { act: function () { addItem(id, 1, "cargo", true); } };
      return { why: "Drop it on a module that loads it" };
    }
    var fam = t.family;
    if (t.group && fam === "cargo") {
      if (d.origin === "fit") return { act: function () { moveToCargo(id); } };
      if (d.origin === "new") return { act: function () { addItem(id, 1, "cargo", true); } };
      return { why: "" };
    }
    if (d.origin === "hold") return { why: "" };
    if (!d.family || d.family !== fam) {
      if (d.family && d.family !== "cargo") return { why: "Goes in " + famWords[d.family] };
      return { why: "Goes in the cargo hold" };
    }
    if (d.origin !== "fit" && restrictionFor(id)) return { why: "Only fits " + restrictionFor(id) };
    if (bayFams[fam]) {
      if (d.origin === "new") return { act: function () { addItem(id, 1, fam); } };
      if (d.origin === "cargo") return { act: function () { moveFromCargo(id, fam); } };
      return { why: "" };
    }
    // A rack: its slots and its group.
    if (d.origin === "new") {
      if (t.slot && t.filled) return { why: "Drop on an empty slot" };
      var full = roomIn(fam);
      if (full) return { why: full };
      return { act: function () { if (!checkRestricted(id, "", fam)) addItem(id, 1, fam); } };
    }
    if (d.origin === "cargo") {
      if (t.slot && t.filled) return { act: function () { swapWithCargo(id, t.typeID, fam); } };
      var room = roomIn(fam);
      if (room) return { why: room };
      return { act: function () { moveFromCargo(id, fam); } };
    }
    // d.origin === "fit": along its own rack.
    if (t.slot && t.filled) {
      if (t.typeID === id) return { why: "" };
      return { act: function () { swapFitted(id, t.typeID); } };
    }
    if (copy) {
      var none = roomIn(fam);
      if (none) return { why: none };
      return { act: function () { addItem(id, 1, fam); } };
    }
    if (t.index >= 0) return { act: function () { pendingFlash = { type: "slot", id: id }; moveItemTo(id, fam, t.index); } };
    return { why: "" };
  }
  // ringDrop is a drop on the ring's face: a module or drone from a
  // list, or one carried in the hold, goes to its own rack or bay
  // (the first free slot); a charge loads into every weapon that
  // takes it. Fitted modules stay where they are.
  function ringDrop(d) {
    var id = d.typeID;
    if (d.origin === "fit" || d.origin === "hold") return { why: "" };
    if (d.kind === "charge") {
      if (d.origin !== "new") return { why: "" };
      return { act: function () { pickCharge({ id: id }); } };
    }
    var fam = d.family;
    if (!rackFams[fam] && !bayFams[fam]) return { why: "Goes in the cargo hold" };
    if (d.origin !== "fit" && restrictionFor(id)) return { why: "Only fits " + restrictionFor(id) };
    if (rackFams[fam]) {
      var full = roomIn(fam);
      if (full) return { why: full };
    }
    if (d.origin === "cargo") return { act: function () { moveFromCargo(id, fam); } };
    return { act: function () { if (!checkRestricted(id, "", fam)) addItem(id, 1, fam); } };
  }
  // loadChargeOn checks the charge against the weapon under it (the
  // picker's charge list for that weapon) and loads it, or flashes
  // the slot when the weapon cannot take it.
  function loadChargeOn(chargeID, t) {
    var weaponID = t.typeID;
    if (!(weaponID > 0)) return;
    fetch("/fittings/picker.json?family=charge&weapon=" + weaponID, {
      headers: { "Accept": "application/json" }
    }).then(function (resp) {
      return resp.ok ? resp.json() : [];
    }).then(function (charges) {
      var ok = (charges || []).some(function (ch) { return ch.id === chargeID; });
      // Onto a ring slot of a type showing a row per module, the
      // charge loads that one module; otherwise every module.
      var ord = t.el && t.el.getAttribute ? parseInt(t.el.getAttribute("data-v-ordinal"), 10) : NaN;
      if (ok && ord >= 0 && splitShown(String(weaponID))) { setChargeAt(String(weaponID), ord, chargeID); return; }
      if (ok) { setCharge(String(weaponID), chargeID); return; }
      var el = editor.querySelector('.fit-vslot[data-fit-vslot="' + weaponID + '"]') || t.el;
      if (!el) return;
      el.classList.add("fit-invalid");
      setTimeout(function () { el.classList.remove("fit-invalid"); }, 1000);
    }).catch(function () {});
  }
  // The ghost carries the module's icon, name and fitting cost, and
  // says why the spot under it will not take it.
  function costOf(d) {
    var el = d.el;
    var cpu = el.getAttribute("data-tip-cpu") || el.getAttribute("data-cpu") || "";
    var pg = el.getAttribute("data-tip-pg") || el.getAttribute("data-pg") || "";
    if (!cpu && !pg) {
      var v = editor.querySelector('.fit-vslot[data-fit-vslot="' + d.typeID + '"]');
      if (v) { cpu = v.getAttribute("data-tip-cpu") || ""; pg = v.getAttribute("data-tip-pg") || ""; }
    }
    var parts = [];
    if (cpu && cpu !== "0") parts.push(cpu.replace(/\s*tf$/, "") + " tf");
    if (pg && pg !== "0") parts.push(pg.replace(/\s*MW$/, "") + " MW");
    return parts.join(" · ");
  }
  function dragGhost(d) {
    var g = document.createElement("div");
    g.className = "fit-drag-ghost";
    var img = d.el.querySelector("img");
    if (img) {
      var c = document.createElement("img");
      c.src = img.src;
      c.alt = "";
      c.draggable = false;
      g.appendChild(c);
    }
    var txt = document.createElement("span");
    txt.className = "fit-ghost-text";
    var nm = document.createElement("span");
    nm.className = "fit-ghost-name";
    nm.textContent = typeNameOf(d.typeID) !== "a module" ? typeNameOf(d.typeID)
      : (d.el._fitRow && d.el._fitRow.name) || d.el.getAttribute("data-tip-name") || "";
    txt.appendChild(nm);
    var cost = costOf(d);
    if (cost) {
      var cs = document.createElement("span");
      cs.className = "fit-ghost-cost";
      cs.textContent = cost;
      txt.appendChild(cs);
    }
    var why = document.createElement("span");
    why.className = "fit-ghost-why";
    why.hidden = true;
    txt.appendChild(why);
    g.appendChild(txt);
    document.body.appendChild(g);
    d.ghostWhy = why;
    return g;
  }
  function moveGhost(d, x, y) {
    if (!d.ghost) return;
    d.ghost.style.left = (x - 22) + "px";
    d.ghost.style.top = (y - 22) + "px";
  }
  var dropCandidates = ".fit-vslot, .fit-visual, .fit-group[data-fit-group], .fit-slots .fit-chip[data-fit-rowtype], .fit-slot-empty";
  function clearDragMarks() {
    var marks = editor.querySelectorAll(".drop-ok, .drop-dim, .drop-over");
    for (var i = 0; i < marks.length; i++) {
      marks[i].classList.remove("drop-ok");
      marks[i].classList.remove("drop-dim");
      marks[i].classList.remove("drop-over");
    }
  }
  // Valid targets light, the rest dim; the target under the pointer
  // is marked on top.
  function highlightForDrag(d) {
    clearDragMarks();
    var wb = workbench();
    if (!wb) return;
    var els = wb.querySelectorAll(dropCandidates);
    for (var i = 0; i < els.length; i++) {
      var t = elementTarget(els[i]);
      var a = t ? dropAction(d, t, false) : { why: "" };
      // The ring's face holds the slots: it only ever lights up.
      if (t && t.ring && !a.act) continue;
      els[i].classList.add(a.act ? "drop-ok" : "drop-dim");
    }
  }
  // elementTarget is targetAt for one element.
  function elementTarget(el) {
    if (el.classList.contains("fit-visual")) return { el: el, ring: true };
    if (el.classList.contains("fit-vslot")) {
      var filled = el.classList.contains("filled");
      return { el: el, slot: true, family: el.getAttribute("data-v-family") || "", filled: filled,
        typeID: filled ? parseInt(el.getAttribute("data-fit-vslot") || "0", 10) || 0 : 0,
        index: parseInt(el.getAttribute("data-v-index") || "-1", 10) };
    }
    var g = el.closest(".fit-group[data-fit-group]");
    var fam = g ? g.getAttribute("data-fit-group") || "" : "";
    if (el.classList.contains("fit-chip") && rackFams[fam]) {
      return { el: el, slot: true, family: fam, filled: true, typeID: parseInt(el.getAttribute("data-fit-rowtype"), 10) || 0, index: -1 };
    }
    if (el.classList.contains("fit-slot-empty") && rackFams[fam]) {
      return { el: el, slot: true, family: fam, filled: false, typeID: 0, index: -1 };
    }
    if (el.classList.contains("fit-group")) return { el: el, group: true, family: fam };
    return null;
  }
  function dragClass(on) {
    var root = document.documentElement;
    if (root) root.classList.toggle("fit-dragging", on);
  }
  function startPointerDrag(ev, d) {
    if (d.lp) { clearTimeout(d.lp); d.lp = null; }
    var src = sourceOf(d.el);
    if (!src) { dragState = null; return; }
    for (var k in src) d[k] = src[k];
    d.active = true;
    closeTip();
    endPreview();
    // A mouse press that turned into a drag must not leave text
    // selected behind it.
    if (window.getSelection) { try { window.getSelection().removeAllRanges(); } catch (e) { /* not critical */ } }
    dragClass(true);
    try { d.el.setPointerCapture(ev.pointerId); } catch (e) { /* not critical */ }
    d.ghost = dragGhost(d);
    moveGhost(d, ev.clientX, ev.clientY);
    d.el.classList.add("drag-src");
    // A drag out of the module search: its list stops covering the
    // workbench (it stays in the page, it holds the pointer).
    d.list = d.el.closest(".suggest");
    if (d.list) d.list.classList.add("fit-list-dragging");
    highlightForDrag(d);
    movePointerDrag(ev, d);
  }
  function movePointerDrag(ev, d) {
    moveGhost(d, ev.clientX, ev.clientY);
    var t = targetAt(ev.clientX, ev.clientY);
    var a = dropAction(d, t, !!(ev.ctrlKey || ev.metaKey));
    if (d.overEl && d.overEl !== t.el) d.overEl.classList.remove("drop-over");
    d.overEl = t.el || null;
    if (d.overEl && a.act) d.overEl.classList.add("drop-over");
    if (d.ghostWhy) {
      d.ghostWhy.textContent = a.why || "";
      d.ghostWhy.hidden = !a.why;
    }
    if (d.ghost) d.ghost.classList.toggle("fit-ghost-off", !a.act);
  }
  function endPointerDrag(ev, d) {
    if (d.ghost && d.ghost.parentNode) d.ghost.parentNode.removeChild(d.ghost);
    d.el.classList.remove("drag-src");
    clearDragMarks();
    if (d.list) { d.list.classList.remove("fit-list-dragging"); modSearch.close(); }
    dragClass(false);
    eatNextClick = true;
    var t = targetAt(ev.clientX, ev.clientY);
    var a = dropAction(d, t, !!(ev.ctrlKey || ev.metaKey));
    if (a.act) {
      a.act();
      if (d.origin === "new" && phoneOn() && fitView === "browse") setFitView("racks");
    }
  }
  // A tap (press without a drag): a ring slot toggles its tooltip,
  // its ammo badge jumps to the charge picker, a Browse row adds
  // its item. Rack rows and search rows keep their own click.
  function tapDragSource(ev, d) {
    var ammo = ev.target && ev.target.closest ? ev.target.closest("[data-fit-ammo]") : null;
    if (ammo) { jumpToCharge(ammo.getAttribute("data-fit-ammo"), parseInt(ammo.getAttribute("data-fit-ammo-index"), 10)); return; }
    if (d.el.hasAttribute("data-fit-vslot")) {
      if (ev.pointerType !== "touch" && d.el.hasAttribute("data-tip-states")) {
        closeTip();
        queueStateClick(d.el, ev);
        return;
      }
      toggleTip(d.el);
      return;
    }
    var it = d.el._fitRow || {};
    pickRow(parseInt(d.el.getAttribute("data-fit-pick"), 10) || 0, d.el.getAttribute("data-kind") || "", it.name || "");
  }
  // Rack rows and module search rows answer taps with their own
  // click handlers; the controller only takes their drags.
  function ownsTap(el) {
    return !el.hasAttribute("data-fit-vslot") && !el.classList.contains("fbt-row");
  }
  editor.addEventListener("pointerdown", function (ev) {
    if (ev.button !== undefined && ev.button > 0) return;
    if (dragState) return;
    var t = dragSourceAt(ev);
    if (!t) return;
    var d = {
      el: t, typeID: 0, x0: ev.clientX, y0: ev.clientY, pid: ev.pointerId,
      active: false, ghost: null, lp: null,
      // Rows live in scrolling lists (and the page): a mostly
      // vertical swipe on one is a scroll, not a drag.
      mayScroll: !t.hasAttribute("data-fit-vslot")
    };
    if (ev.pointerType === "touch") {
      d.lp = setTimeout(function () {
        d.lp = null;
        if (dragState === d && !d.active) {
          d.mayScroll = false;
          startPointerDrag({ pointerId: d.pid, clientX: d.x0, clientY: d.y0 }, d);
        }
      }, LONG_PRESS_MS);
    }
    dragState = d;
  });
  editor.addEventListener("pointermove", function (ev) {
    var d = dragState;
    if (!d || ev.pointerId !== d.pid) return;
    var dx = ev.clientX - d.x0, dy = ev.clientY - d.y0;
    if (!d.active) {
      if (Math.sqrt(dx * dx + dy * dy) < DRAG_PX) return;
      if (d.mayScroll && Math.abs(dy) > Math.abs(dx) * 1.4) {
        if (d.lp) clearTimeout(d.lp);
        dragState = null;
        return;
      }
      startPointerDrag(ev, d);
      return;
    }
    movePointerDrag(ev, d);
  });
  // Once a touch drag runs, the page must not scroll under it.
  editor.addEventListener("touchmove", function (ev) {
    if (dragState && dragState.active) ev.preventDefault();
  }, { passive: false });
  function releasePointerDrag(ev) {
    var d = dragState;
    if (!d || ev.pointerId !== d.pid) return;
    dragState = null;
    if (d.lp) clearTimeout(d.lp);
    if (d.active) { endPointerDrag(ev, d); return; }
    if (ownsTap(d.el)) return;
    // Eat the synthetic click after pointerup so a tap never
    // double-fires (show-then-dismiss). A fresh press clears it,
    // so a cancelled click can't eat a later real one.
    eatNextClick = true;
    tapDragSource(ev, d);
  }
  editor.addEventListener("pointerup", releasePointerDrag);
  editor.addEventListener("pointercancel", function (ev) {
    var d = dragState;
    if (!d || ev.pointerId !== d.pid) return;
    dragState = null;
    if (d.lp) clearTimeout(d.lp);
    if (d.active) {
      if (d.ghost && d.ghost.parentNode) d.ghost.parentNode.removeChild(d.ghost);
      d.el.classList.remove("drag-src");
      if (d.list) d.list.classList.remove("fit-list-dragging");
      dragClass(false);
      clearDragMarks();
    }
  });
  // A real drag ends in a click; eat exactly that one so taps and
  // drags never double-fire. Clicks inside the tooltip itself
  // (its close button) always pass through. Any fresh press
  // clears the flag first.
  editor.addEventListener("pointerdown", function () { eatNextClick = false; }, true);
  editor.addEventListener("click", function (ev) {
    if (eatNextClick) {
      eatNextClick = false;
      if (ev.target && ev.target.closest && ev.target.closest(".fit-tip")) return;
      ev.stopPropagation();
      ev.preventDefault();
    }
  }, true);
  // A long press must never pop the context menu mid-drag. A
  // right-click on a fitted module opens its tooltip instead, with
  // the state buttons, the way the in-game module menu does.
  editor.addEventListener("contextmenu", function (ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot, [data-fit-pick], .fit-chip[data-fit-rowtype]") : null;
    if (!t) return;
    ev.preventDefault();
    if (ev.target.closest("input, select, textarea")) return;
    var mod = t.closest(".fit-vslot.filled, .fit-chip[data-tip-name]");
    if (mod && !phoneOn()) { cancelStateClick(); showTip(mod); }
  });
  // A double-click on a fitted module opens its details, like Show
  // Info in game; the single click it starts with is held back
  // (queueStateClick) so it never also switches the module.
  editor.addEventListener("dblclick", function (ev) {
    var t = ev.target;
    if (!t || !t.closest) return;
    if (t.closest(".fit-tip, button:not(.fit-vslot), input, select, textarea, a, [data-fit-info]")) return;
    var mod = t.closest(".fit-vslot.filled, .fit-chip[data-fit-rowtype]");
    if (!mod || phoneOn()) return;
    cancelStateClick();
    closeTip();
    var id = mod.getAttribute("data-fit-vslot") || mod.getAttribute("data-fit-rowtype");
    if (id && window.FitSheet) window.FitSheet.open(id, mod);
  });
  // Delete or Backspace on a focused fitted slot or rack row
  // removes one, like its minus button.
  editor.addEventListener("keydown", function (ev) {
    if (ev.key !== "Delete" && ev.key !== "Backspace") return;
    var t = ev.target;
    if (!t || !t.closest || t.closest("input, select, textarea")) return;
    var el = t.closest(".fit-vslot.filled, .fit-chip[data-fit-rowtype]");
    if (!el) return;
    ev.preventDefault();
    var id = parseInt(el.getAttribute("data-fit-vslot") || el.getAttribute("data-fit-rowtype") || "0", 10) || 0;
    if (id) decItem(id, inCargo(el));
  });

  // moveItemTo relocates one instance of typeID within its slot
  // family to doc-order position idx (counting only that
  // family's items). Dropping on an occupied slot swaps the two
  // modules; on an empty slot it moves there (or to the end of
  // the family when the slot is beyond the last item).
  function moveItemTo(typeID, fam, idx) {
    var famIdx = [];
    for (var i = 0; i < state.items.length; i++) {
      if (state.items[i].cargo) continue;
      if ((familyOfType(state.items[i].typeId) || "") === fam) famIdx.push(i);
    }
    var fromPos = -1;
    for (var p = 0; p < famIdx.length; p++) {
      if (state.items[famIdx[p]].typeId === typeID) { fromPos = p; break; }
    }
    if (fromPos < 0) return;
    // Clamp the target into the family's item range; an empty
    // slot beyond the last item means "end of family".
    if (idx > famIdx.length) idx = famIdx.length;
    if (idx === fromPos || (idx === famIdx.length && fromPos === famIdx.length - 1)) return;
    var items = state.items.slice();
    var moving = items.splice(famIdx[fromPos], 1)[0];
    // Recompute family positions after removal, then insert at
    // the target position (which may now be the end).
    var after = [];
    for (var j = 0; j < items.length; j++) {
      if (items[j].cargo) continue;
      if ((familyOfType(items[j].typeId) || "") === fam) after.push(j);
    }
    var at = idx < after.length ? after[idx] : items.length;
    items.splice(at, 0, moving);
    setItems(items);
  }
  // familyOfType resolves a type's slot family client-side from
  // the rendered groups (the workbench carries data-fit-group).
  function familyOfType(typeID) {
    var wb = workbench();
    if (!wb) return "";
    // The hull's own line says it; a type only carried in cargo
    // answers with the rack it fits (data-fit-home).
    var rows = wb.querySelectorAll('[data-fit-rowtype="' + typeID + '"]');
    var home = "";
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].hasAttribute("data-fit-cargo")) {
        home = home || rows[i].getAttribute("data-fit-home") || "";
        continue;
      }
      var g = rows[i].closest(".fit-group");
      return g ? (g.getAttribute("data-fit-group") || "") : "";
    }
    return home;
  }
  // --- slot tooltips ----------------------------------------------
  // Hover (desktop) or tap (mobile) on a fitted icon shows the
  // quick-look tooltip: name, slot, meta, CPU/PG. FitTipUI.place
  // puts it where it covers no other module. Tap never navigates
  // or deletes. Dismissible via the close
  // button, tapping elsewhere, or Escape.
  // One tooltip at a time. tipAnchor is the slot it belongs to;
  // a re-render replaces every slot, so reanchorTip() moves the
  // open tooltip onto the new element with the same data-tip-key
  // (or closes it), nothing may keep pointing at the old DOM.
  var tipEl = null;
  var tipAnchor = null;
  var tipHoverSlot = null;
  var tipCloseTimer = null;
  function cancelTipClose() {
    if (tipCloseTimer) clearTimeout(tipCloseTimer);
    tipCloseTimer = null;
  }
  function closeTip() {
    cancelTipClose();
    if (tipEl && tipEl.parentNode) tipEl.parentNode.removeChild(tipEl);
    tipEl = null;
    tipAnchor = null;
  }
  function showTip(slot) {
    closeTip();
    var wb = workbench();
    if (!wb) return;
    tipEl = FitTipUI.build(slot, { onState: setModuleState, onClose: closeTip });
    if (!tipEl) return;
    tipAnchor = slot;
    tipEl.style.visibility = "hidden";
    wb.appendChild(tipEl);
    FitTipUI.place(tipEl, slot, wb);
    tipEl.style.visibility = "";
  }
  function reanchorTip() {
    if (!tipEl) { tipHoverSlot = null; return; }
    var key = tipAnchor ? tipAnchor.getAttribute("data-tip-key") : "";
    var toggled = tipEl.getAttribute("data-for");
    var hovered = !!tipAnchor && tipHoverSlot === tipAnchor;
    var slot = key ? editor.querySelector('.fit-vslot[data-tip-key="' + key + '"]') : null;
    closeTip();
    tipHoverSlot = null;
    if (!slot) return;
    showTip(slot);
    if (!tipEl) return;
    // A tapped tooltip stays tapped, so a later tap on the slot
    // still closes it; a hovered one still closes on mouse leave.
    if (toggled) tipEl.setAttribute("data-for", tipToggleKey(slot));
    if (hovered) tipHoverSlot = slot;
  }
  function tipToggleKey(slot) {
    return slot.getAttribute("data-v-index") + ":" + (slot.getAttribute("data-v-family") || "");
  }
  // Jumps to a weapon's ammunition search (from its slot badge),
  // which opens on everything the weapon takes.
  function jumpToCharge(weaponID, index) {
    var input = editor.querySelector('[data-fit-ammo-for="' + weaponID + '"]');
    if (index >= 0 && splitShown(String(weaponID))) {
      input = editor.querySelector('[data-fit-ammo-one="' + weaponID + '"][data-fit-ammo-index="' + index + '"]') || input;
    }
    if (!input) return;
    closeTip();
    if (input.scrollIntoView) input.scrollIntoView({ block: "center" });
    try { input.focus({ preventScroll: true }); } catch (e) { try { input.focus(); } catch (e2) {} }
    var row = input.closest ? input.closest(".fit-imp") : null;
    if (!row) return;
    row.classList.add("flash");
    window.setTimeout(function () { row.classList.remove("flash"); }, 750);
  }
  // The tooltip rules, the same on the ring and the rack rows:
  // it shows the module under the pointer; leaving the module (or
  // the tooltip) closes it after a short delay; a click pins it,
  // so it stays after the pointer leaves, until a second click on
  // the module, a click elsewhere, its close button or Escape.
  // Hovering another module shows that one instead.
  function tipAnchorAt(node) {
    return node && node.closest ? node.closest(".fit-vslot[data-tip-name], .fit-chip[data-tip-name]") : null;
  }
  function tipPinned() { return !!(tipEl && tipEl.getAttribute("data-for")); }
  editor.addEventListener("mouseover", function (ev) {
    if (dragState && dragState.active) return; // no tooltips mid-drag
    var slot = tipAnchorAt(ev.target);
    if (!slot) return;
    // Moving between a module's own parts (icon, ammo badge,
    // name) is not leaving it.
    if (slot === tipHoverSlot) { cancelTipClose(); return; }
    tipHoverSlot = slot;
    if (tipEl && tipAnchor === slot) { cancelTipClose(); return; }
    showTip(slot);
  });
  // The close timer belongs to the tooltip it was armed for:
  // showTip cancels it, and it never closes a later tooltip
  // (leaving A arms it; arriving on B must not let it kill B's).
  // The delay covers the trip from a ring slot to its tooltip in
  // the ring's hole, where the state buttons are.
  var TIP_CLOSE_MS = 400;
  function scheduleTipClose() {
    cancelTipClose();
    if (tipPinned()) return;
    var forTip = tipEl;
    tipCloseTimer = setTimeout(function() {
      tipCloseTimer = null;
      if (!tipEl || tipEl !== forTip || tipPinned()) return;
      // The pointer came back to the tooltip or its module.
      if (tipEl.matches(":hover") || (tipAnchor && tipAnchor.matches(":hover"))) return;
      tipHoverSlot = null;
      closeTip();
    }, TIP_CLOSE_MS);
  }
  editor.addEventListener("mouseout", function (ev) {
    var slot = tipAnchorAt(ev.target);
    if (!slot || slot !== tipHoverSlot) return;
    var to = ev.relatedTarget;
    if (to && slot.contains(to)) return;
    tipHoverSlot = null;
    scheduleTipClose();
  });
  // Keep tooltip open when hovering over it
  document.addEventListener("mouseover", function(ev) {
    if (tipEl && ev.target && ev.target.closest && ev.target.closest(".fit-tip")) {
      if (tipCloseTimer) {
        clearTimeout(tipCloseTimer);
        tipCloseTimer = null;
      }
    }
  });
  document.addEventListener("mouseout", function(ev) {
    if (tipEl && ev.target && ev.target.closest && ev.target.closest(".fit-tip")) {
      var toEl = ev.relatedTarget;
      if (!toEl || !toEl.closest || !toEl.closest(".fit-tip")) {
        scheduleTipClose();
      }
    }
  });
  // Taps on slots are handled by the pointer controller (tap shows
  // the tooltip); a tap anywhere else dismisses it. Never deletes
  // or navigates. Keyboard: Enter/Space on a focused slot toggles
  // its tooltip the same way a tap does.
  // The key a tapped tooltip remembers its anchor by: a ring
  // slot by its position, a list chip (which has no slot
  // position) by its group and type.
  function tipKeyFor(el) {
    if (el.getAttribute("data-v-index") !== null) return tipToggleKey(el);
    var g = el.closest ? el.closest(".fit-group") : null;
    return "chip:" + (g ? g.getAttribute("data-fit-group") || "" : "") + ":" +
      (el.getAttribute("data-fit-rowtype") || "");
  }
  // toggleTip is a click on a module: it pins the tooltip (the
  // hover one stays where it is), or closes a pinned one.
  function toggleTip(slot) {
    var key = tipKeyFor(slot);
    if (tipEl && tipEl.getAttribute("data-for") === key) {
      closeTip();
      return;
    }
    if (!tipEl || tipAnchor !== slot) showTip(slot);
    if (tipEl) {
      cancelTipClose();
      tipEl.setAttribute("data-for", key);
    }
  }
  // Commits a state change for one fitted line's instances:
  // one history entry, one re-simulation through the coalesced
  // path. The tooltip's state buttons (setModuleState) and the
  // phone racks' tap-to-cycle both end here.
  function commitModuleStates(typeID, ordinals, newState) {
    var i = findItem(typeID);
    if (i < 0) return;
    normItems();
    var it = state.items[i];
    var todo = [];
    for (var k = 0; k < ordinals.length; k++) {
      var o = ordinals[k];
      if (o >= 0 && o < it.states.length && it.states[o] !== newState) todo.push(o);
    }
    if (!todo.length) return;
    pushHistory();
    for (var j = 0; j < todo.length; j++) it.states[todo[j]] = newState;
    markDirty();
    // The re-render re-anchors whichever tooltip is open by then
    // (reanchorTip), so a pointer that moved on is never left
    // with this module's tooltip stuck on screen.
    simulate();
  }
  // Sets one module instance's state (tipKey is "typeID:ordinal"
  // from the slot). Undo history covers it like any other edit.
  function setModuleState(tipKey, newState) {
    var parts = (tipKey || "").split(":");
    var typeID = parseInt(parts[0], 10) || 0;
    var ordinal = parseInt(parts[1], 10) || 0;
    if (!typeID || ordinal < 0) return;
    commitModuleStates(typeID, [ordinal], newState);
  }
  // Phone Slot Racks: tapping a module's card cycles the whole
  // line to the next state the module allows (the card shows one
  // state for the line), through the same commit the desktop
  // tooltip's state buttons use.
  // A mouse click on a fitted module waits one double-click span
  // before it switches the module (clickState), so a double-click
  // reads as one gesture: details, with no state change.
  var STATE_CLICK_MS = 260;
  var stateClickTimer = 0;
  function cancelStateClick() {
    if (stateClickTimer) { clearTimeout(stateClickTimer); stateClickTimer = 0; }
  }
  function queueStateClick(el, ev) {
    cancelStateClick();
    if (ev.detail > 1) return; // the second click of a double-click
    var shift = !!ev.shiftKey, ctrl = !!(ev.ctrlKey || ev.metaKey);
    stateClickTimer = setTimeout(function () {
      stateClickTimer = 0;
      applyStateClick(el, shift, ctrl);
    }, STATE_CLICK_MS);
  }
  function applyStateClick(el, shift, ctrl) {
    if (!el.isConnected) return;
    if (el.hasAttribute("data-fit-vslot")) {
      var next = FitPhoneViews.clickState(el.getAttribute("data-tip-states"), el.getAttribute("data-tip-state"), shift, ctrl);
      if (next) setModuleState(el.getAttribute("data-tip-key"), next);
      return;
    }
    var typeID = parseInt(el.getAttribute("data-fit-rowtype"), 10) || 0;
    var i = typeID ? findItem(typeID) : -1;
    if (i < 0) return;
    var lineNext = FitPhoneViews.clickState(el.getAttribute("data-fit-states"), el.getAttribute("data-fit-state"), shift, ctrl);
    if (!lineNext) return;
    normItems();
    var ordinals = [];
    for (var o = 0; o < state.items[i].states.length; o++) ordinals.push(o);
    commitModuleStates(typeID, ordinals, lineNext);
  }
  function cycleChipState(chip) {
    var typeID = parseInt(chip.getAttribute("data-fit-rowtype"), 10) || 0;
    if (!typeID) return;
    var next = FitPhoneViews.nextState(chip.getAttribute("data-fit-states"),
      chip.getAttribute("data-fit-state"));
    if (!next) return;
    var i = findItem(typeID);
    if (i < 0) return;
    normItems();
    var ordinals = [];
    for (var o = 0; o < state.items[i].states.length; o++) ordinals.push(o);
    commitModuleStates(typeID, ordinals, next);
  }

  // --- phone views ------------------------------------------------
  // At phone width (the shared 45rem band) the workbench is three
  // views under the tab bar the fragment renders: Slot Racks (the
  // slot and bay lists), Resists & Cap (the stats column) and the
  // Radial, which is where opening a fit lands. The current view
  // lives here, not in the fragment, and is re-applied after every
  // simulate swap like the other view syncs. Off the band nothing
  // is hidden and the tab bar is display:none, so the desktop
  // workbench is untouched.
  //
  // The module search keeps its home slot in the page, outside
  // the fragment: a simulate swap replaces the workbench whole,
  // so nothing the page owns may live inside it. On the phone the
  // tab bar's search button collapses it into a popup panel (the
  // .fit-collapsible rules in fit.css): the open state lives
  // here, the panel node never moves, and applyFitView re-syncs
  // the freshly rendered button after every swap.
  var phoneMQ = window.matchMedia ? window.matchMedia("(max-width: 45rem)") : null;
  var fitView = "radial";
  var searchOpen = false;
  function phoneOn() { return !!(phoneMQ && phoneMQ.matches); }
  function searchButton() {
    var wb = workbench();
    return wb ? wb.querySelector("[data-fit-search-btn]") : null;
  }
  // The open panel rides the top layer (a manual popover) where
  // the browser has one: the page is a size container, so a fixed
  // box inside it would pin to the page, not the screen. Without
  // popover support it stays in place and scrolls into view.
  var searchLayer = typeof HTMLElement !== "undefined" && HTMLElement.prototype.hasOwnProperty("popover");
  function showSearchLayer(ms, open) {
    if (!searchLayer) return;
    if (open) {
      if (!ms.hasAttribute("popover")) ms.setAttribute("popover", "manual");
      if (!ms.matches(":popover-open")) ms.showPopover();
    } else if (ms.hasAttribute("popover")) {
      if (ms.matches(":popover-open")) ms.hidePopover();
      ms.removeAttribute("popover");
    }
  }
  function syncSearchChrome() {
    var ms = document.getElementById("fit-modsearch");
    if (!ms) return;
    var btn = searchButton();
    if (phoneOn() && btn && fitView !== "browse") {
      ms.classList.add("fit-collapsible");
      ms.classList.toggle("open", searchOpen);
      showSearchLayer(ms, searchOpen);
      btn.setAttribute("aria-expanded", searchOpen ? "true" : "false");
      btn.classList.toggle("on", searchOpen);
    } else {
      showSearchLayer(ms, false);
      // Above the band, no tab bar to toggle it (no ship picked
      // yet), or the phone's Browse view, which it filters: the
      // search is the plain in-flow panel it is on desktop.
      ms.classList.remove("fit-collapsible");
      ms.classList.remove("open");
      if (btn) {
        btn.setAttribute("aria-expanded", "false");
        btn.classList.remove("on");
      }
    }
  }
  function setSearchOpen(open, focus) {
    searchOpen = !!open;
    syncSearchChrome();
    if (searchOpen) {
      // Without the top layer the panel pins to the page (see
      // fit.css): when it opens from the tab bar pinned deep in
      // the page, bring it on screen before focus goes in.
      var ms = document.getElementById("fit-modsearch");
      if (!searchLayer && ms && ms.scrollIntoView) ms.scrollIntoView({ block: "start" });
      if (focus !== false && modInput) {
        try { modInput.focus({ preventScroll: true }); } catch (e) { modInput.focus(); }
      }
    }
  }
  function applyFitView() {
    var wb = workbench();
    if (!wb) return;
    var on = phoneOn();
    var show = FitPhoneViews.sections(fitView);
    function vis(el, want) { if (el) el.classList.toggle("fv-off", on && !want); }
    vis(wb.querySelector(".fit-center"), show.center);
    vis(wb.querySelector(".fit-slots"), show.slots);
    vis(wb.querySelector(".fit-bays"), show.bays);
    vis(wb.querySelector(".fit-stats"), show.stats);
    mountBrowse();
    var tabs = wb.querySelectorAll("[data-fit-view]");
    for (var i = 0; i < tabs.length; i++) {
      var active = on && tabs[i].getAttribute("data-fit-view") === fitView;
      tabs[i].classList.toggle("on", !!active);
      tabs[i].setAttribute("aria-selected", active ? "true" : "false");
    }
    syncSearchChrome();
  }
  function setFitView(view) {
    if (view !== "racks" && view !== "stats" && view !== "radial" && view !== "browse") return;
    var was = browseShown();
    fitView = view;
    applyFitView();
    if (was !== browseShown()) searchHandover();
  }
  if (phoneMQ && phoneMQ.addEventListener) {
    phoneMQ.addEventListener("change", applyFitView);
  }
  // The search panel's own close button (page markup, never
  // re-rendered): close and hand focus back to the tab bar's
  // search button.
  var msPanel = document.getElementById("fit-modsearch");
  var msClose = msPanel ? msPanel.querySelector("[data-fit-search-close]") : null;
  if (msClose) {
    msClose.addEventListener("click", function () {
      setSearchOpen(false, false);
      var btn = searchButton();
      if (btn) btn.focus();
    });
  }
  // Escape closes the search panel; the suggestion list takes
  // the first Escape (its own handler closes it), so while the
  // list is open this leaves the key alone. Capture phase: this
  // reads the list state before the input's handler runs.
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Escape" || !searchOpen) return;
    if (modList && !modList.hidden) return;
    setSearchOpen(false, false);
    var btn = searchButton();
    if (btn) btn.focus();
  }, true);

  // --- fit head fold --------------------------------------------
  // The fit head's secondary controls fold behind its Fit details
  // toggle: at phone width every secondary control folds; on
  // desktop the strip controls (fit name, Save to EVE) stay on
  // the bar and the rest fold. fit.css hides them only while the
  // editor carries fit-armed, which is added here, without JS
  // the toggle never renders and nothing is ever hidden.
  var headEl = editor.querySelector(".fit-head");
  var headToggle = editor.querySelector("[data-fit-head-toggle]");
  if (headEl && headToggle) {
    editor.classList.add("fit-armed");
    headToggle.addEventListener("click", function () {
      var open = headEl.classList.toggle("fit-open");
      headToggle.setAttribute("aria-expanded", open ? "true" : "false");
    });
  }
  // --- paste to import ---------------------------------------------
  // EFT text pasted anywhere on the page outside a text field opens
  // as the fit, through the Import form's own route. Replacing a
  // fit with unsaved changes asks first.
  var eftHead = /^\s*\[[^,\]\n]+,[^\]\n]*\]/;
  document.addEventListener("paste", function (ev) {
    if (!document.body.contains(editor)) return;
    var t = ev.target;
    if (t && t.closest && t.closest("input, textarea, select, [contenteditable]")) return;
    var text = ev.clipboardData ? ev.clipboardData.getData("text/plain") : "";
    if (!text || !eftHead.test(text)) return;
    ev.preventDefault();
    var importArea = document.getElementById("fit-eft-in");
    var form = importArea && importArea.form;
    if (!form) return;
    askLeave("replace", function () {
      leaving = true;
      importArea.value = text;
      if (form.requestSubmit) form.requestSubmit(); else form.submit();
    });
  });
  // The Import form itself opens another fit: it asks the same way.
  var importForm = document.getElementById("fit-eft-in") && document.getElementById("fit-eft-in").form;
  if (importForm) {
    importForm.addEventListener("submit", function (ev) {
      if (!holdUnsaved()) return;
      ev.preventDefault();
      askLeave("replace", function () { importForm.submit(); });
    });
  }


  syncShipGate();
  editor.addEventListener("keydown", function (ev) {
    if (ev.key !== "Enter" && ev.key !== " ") return;
    var slot = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot.filled") : null;
    if (!slot) return;
    ev.preventDefault();
    toggleTip(slot);
  });
  // A click on a tooltip anchor is the anchor's own business
  // (a tap toggles it), never a dismissal; a click anywhere else on
  // the page closes the tooltip.
  document.addEventListener("click", function (ev) {
    if (!tipEl) return;
    var t = ev.target;
    if (t && t.closest && t.closest(".fit-tip")) return;
    if (t && t.closest && t.closest(".fit-vslot[data-tip-name], .fit-chip[data-tip-name]")) return;
    closeTip();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") closeTip();
  });

  // --- full screen -----------------------------------------------
  // The whole editor (fit bar, module search, workbench) takes the
  // screen: through the Fullscreen API where the browser has it for
  // an element, and where it does not (iPhone Safari) as a manual
  // popover in the top layer. Either way the editor wears fit-full;
  // modals, the details drawer and the keys card are top-layer
  // popovers of their own, so they still open over it. The button
  // lives in the re-rendered workbench, so its pressed state is
  // synced after every render.
  var fullViaAPI = false;
  function fullscreenElement() { return document.fullscreenElement || document.webkitFullscreenElement || null; }
  function isFull() { return editor.classList.contains("fit-full"); }
  function syncFullButton() {
    var on = isFull();
    editor.querySelectorAll("[data-fit-full]").forEach(function (b) {
      b.setAttribute("aria-pressed", on ? "true" : "false");
      b.setAttribute("aria-label", on ? "Leave full screen" : "Full screen");
      b.title = on ? "Leave full screen (Shift F)" : "Full screen (Shift F)";
    });
  }
  function fullTopLayer(on) {
    if (!editor.showPopover) return;
    if (on) {
      if (!isFull()) return;
      editor.setAttribute("popover", "manual");
      try { editor.showPopover(); } catch (e) { /* already showing */ }
    } else if (editor.hasAttribute("popover")) {
      try { editor.hidePopover(); } catch (e) { /* already hidden */ }
      editor.removeAttribute("popover");
    }
  }
  function toggleFull(force) {
    var on = force === undefined ? !isFull() : force;
    editor.classList.toggle("fit-full", on);
    document.documentElement.classList.toggle("fit-full-open", on);
    syncFullButton();
    closeTip();
    if (on) {
      var req = editor.requestFullscreen || editor.webkitRequestFullscreen;
      if (!req) { fullTopLayer(true); return; }
      try {
        var pr = req.call(editor);
        if (pr && pr.catch) pr.catch(function () { fullTopLayer(true); });
      } catch (e) { fullTopLayer(true); }
    } else {
      fullTopLayer(false);
      if (fullscreenElement() === editor) (document.exitFullscreen || document.webkitExitFullscreen).call(document);
    }
  }
  function onFullChange() {
    if (fullscreenElement() === editor) { fullViaAPI = true; return; }
    if (fullViaAPI) { fullViaAPI = false; if (isFull()) toggleFull(false); }
  }
  document.addEventListener("fullscreenchange", onFullChange);
  document.addEventListener("webkitfullscreenchange", onFullChange);
  editor.addEventListener("click", function (ev) {
    var b = ev.target && ev.target.closest ? ev.target.closest("[data-fit-full]") : null;
    if (b) toggleFull();
  });
  // Shift F toggles it; Escape leaves the popover fallback (the
  // browser's own full screen handles Escape itself). Typing in a
  // field never toggles.
  document.addEventListener("keydown", function (ev) {
    if (!document.body.contains(editor)) return;
    var t = ev.target;
    var typing = t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName || ""));
    if (ev.key === "F" && ev.shiftKey && !ev.ctrlKey && !ev.metaKey && !ev.altKey && !typing) {
      if (!workbench()) return;
      ev.preventDefault();
      toggleFull();
      return;
    }
    if (ev.key === "Escape" && isFull() && !fullscreenElement() && !ev.defaultPrevented) {
      try { if (document.querySelector(":popover-open:not(#fit-editor)")) return; } catch (e) { /* no popovers here */ }
      toggleFull(false);
    }
  });
  // A page swap that takes the editor away leaves full screen.
  document.addEventListener("evesynapse:content-ready", function () {
    if (!document.body.contains(editor) && document.documentElement.classList.contains("fit-full-open")) {
      document.documentElement.classList.remove("fit-full-open");
      if (fullscreenElement() === editor) (document.exitFullscreen || document.webkitExitFullscreen).call(document);
    }
  });

  // --- subsystem UI gating ----------------------------------------
  // The subsystem ring/group/search-family only appear when the
  // hull supports subsystems (data-fit-subsystems on .fit-wb).
  function syncSubsystemUI() {
    var wb = workbench();
    var ok = wb && wb.getAttribute("data-fit-subsystems") === "1";
    var chip = document.querySelector('#fit-famchips button[data-fam="subsystem"]');
    if (chip) chip.hidden = !ok;
    if (!ok && modFamily === "subsystem") setFamChip("all");
  }

  // --- save / export --------------------------------------------
  // Ownership chain (defense in depth):
  //   1. The "Your fits" search forks any non-owned fit via POST
  //      /fittings/fork/ before the editor ever sees it (yfLoad),
  //      so the editor only ever holds the signed-in user's own
  //      fits, public fits are read-only here and never edited
  //      in place.
  //   2. Every save carries the fit's local id (data-fit-local-id,
  //      mirrored in the hidden #fit-local-id), and the server
  //      looks that id up scoped to the current user's userID, so
  //      a save can never overwrite another user's fit.
  //   3. If the row is gone anyway, the server answers 404 and the
  //      handler below surfaces "That saved fit couldn't be
  //      found." instead of failing silently.
  var saveEveBtn = document.getElementById("fit-save-eve");
  function fitNotice(msg, isErr, relink) {
    var old = editor.querySelector(".fit-notice");
    if (old) old.remove();
    var p = document.createElement("p");
    p.className = "fit-notice" + (isErr ? " err" : "");
    p.textContent = msg;
    if (relink) {
      p.appendChild(document.createTextNode(" "));
      var a = document.createElement("a");
      a.href = "/auth/link";
      a.textContent = "Sign in again";
      p.appendChild(a);
    }
    var head = editor.querySelector(".fit-head");
    if (head && head.parentNode) head.parentNode.insertBefore(p, head.nextSibling);
    else editor.insertBefore(p, editor.firstChild);
    p.scrollIntoView({ block: "nearest" });
  }
  if (saveEveBtn) {
    saveEveBtn.addEventListener("click", function () {
      if (!state.shipTypeId) return;
      var charID = parseInt(saveEveBtn.getAttribute("data-fit-eve-char") || "0", 10) || 0;
      if (!charID) return;
      state.name = nameInput ? nameInput.value : state.name;
      saveEveBtn.disabled = true;
      fitNotice("Saving to EVE…", false, false);
      fetch("/fittings/save-to-eve/", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Accept": "application/json" },
        body: JSON.stringify({
          characterId: charID,
          name: state.name,
          shipTypeId: state.shipTypeId,
          items: state.items,
          charges: state.charges
        })
      }).then(function (resp) {
        return resp.ok ? resp.json() : null;
      }).then(function (row) {
        saveEveBtn.disabled = false;
        if (row && row.ok) {
          clearDirty();
          fitNotice("Saved \u201c" + row.name + "\u201d to EVE. Loading your in-game fitting…", false, false);
          window.setTimeout(function () {
            window.location.href = "/fittings/?character=" + row.characterId + "&esi=" + row.fittingId + "#fit-editor";
          }, 1200);
        } else if (row) {
          fitNotice(row.error || "EVE refused the fitting.", true, !!row.relink);
        } else {
          fitNotice("Could not reach the server.", true, false);
        }
      }).catch(function () {
        saveEveBtn.disabled = false;
        fitNotice("Could not reach the server.", true, false);
      });
    });
  }
  if (saveBtn) {
    saveBtn.addEventListener("click", function () {
      if (!state.shipTypeId) return;
      state.name = nameInput ? nameInput.value : state.name;
      var pubChk = document.getElementById("fit-public");
      var payload = {
        id: parseInt(saveBtn.getAttribute("data-fit-local-id") || "0", 10) || 0,
        name: state.name,
        description: descInput ? descInput.value : "",
        tags: tagsInput ? tagsInput.value : "",
        isPublic: !!(pubChk && pubChk.checked),
        promote: true,
        fit: {
          name: state.name,
          shipTypeId: state.shipTypeId,
          items: state.items,
          charges: state.charges,
          implants: state.implants || {},
          boosts: state.boosts || []
        }
      };
      fetch("/fittings/save/", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Accept": "application/json" },
        body: JSON.stringify(payload)
      }).then(function (resp) {
        if (resp.status === 404) {
          fitNotice("That saved fit couldn't be found.", true, false);
          return null;
        }
        return resp.ok ? resp.json() : null;
      }).then(function (row) {
        if (row && row.id) {
          clearDirty();
          window.location.href = "/fittings/?local=" + row.id + "#fit-editor";
        }
      }).catch(function () {});
    });
  }
  var shareAnonBtn = document.getElementById("fit-share-anon");
  if (shareAnonBtn) {
    shareAnonBtn.addEventListener("click", function () {
      if (!state.shipTypeId) return;
      state.name = nameInput ? nameInput.value : state.name;
      var linkEl = document.getElementById("fit-share-link");
      fetch("/fittings/save-anonymous/", {
        method: "POST",
        headers: { "Content-Type": "application/json", "Accept": "application/json" },
        body: JSON.stringify({
          name: state.name,
          description: descInput ? descInput.value : "",
          tags: tagsInput ? tagsInput.value : "",
          fit: {
            name: state.name,
            shipTypeId: state.shipTypeId,
            items: state.items,
            charges: state.charges,
            implants: state.implants || {},
            boosts: state.boosts || []
          }
        })
      }).then(function (resp) {
        return resp.ok ? resp.json() : null;
      }).then(function (row) {
        if (row && row.shareUrl) clearDirty(); // the shared copy is this visitor's save
        if (row && row.shareUrl && linkEl) {
          var a = document.createElement("a");
          a.href = row.shareUrl;
          a.textContent = row.shareUrl;
          linkEl.textContent = "Share link: ";
          linkEl.appendChild(a);
        } else if (linkEl) {
          linkEl.textContent = "That fit could not be shared.";
        }
      }).catch(function () {
        if (linkEl) linkEl.textContent = "That fit could not be shared.";
      });
    });
  }
  // --- EFT out and in --------------------------------------------
  // Copy EFT: the text comes from the server (/fittings/export/), so
  // the clipboard gets it as a pending item, inside this click, the
  // way Safari requires; browsers without ClipboardItem write it once
  // it arrives. Where the browser refuses both, the copy popup shows
  // the text selected, to copy by hand. Import EFT opens the import
  // popup, whose form the page-wide paste also submits.
  function exportText() {
    state.name = nameInput ? nameInput.value : state.name;
    return fetch("/fittings/export/", {
      method: "POST",
      headers: { "Content-Type": "application/json", "Accept": "text/plain" },
      body: JSON.stringify({
        name: state.name,
        shipTypeId: state.shipTypeId,
        items: state.items,
        charges: state.charges,
        pilot: pilotID()
      })
    }).then(function (resp) {
      if (!resp.ok) throw new Error("export " + resp.status);
      return resp.text();
    });
  }
  function exportFlash(label) {
    var prior = exportBtn.getAttribute("data-fit-label") || exportBtn.textContent;
    exportBtn.setAttribute("data-fit-label", prior);
    exportBtn.textContent = label;
    window.setTimeout(function () { exportBtn.textContent = prior; }, 1500);
  }
  function exportByHand(text) {
    var modal = document.getElementById("fit-copy-modal");
    if (!modal || !exportOut || !window.EveSynapseModal) return;
    exportOut.value = text;
    window.EveSynapseModal.open(modal, { focus: false });
    exportOut.focus();
    exportOut.select();
  }
  if (exportBtn) {
    exportBtn.addEventListener("click", function () {
      if (!state.shipTypeId) return;
      var pending = exportText();
      var clip = navigator.clipboard;
      var copied;
      if (clip && clip.write && window.ClipboardItem) {
        copied = clip.write([new window.ClipboardItem({
          "text/plain": pending.then(function (text) { return new Blob([text], { type: "text/plain" }); })
        })]);
      } else if (clip && clip.writeText) {
        copied = pending.then(function (text) { return clip.writeText(text); });
      } else {
        copied = Promise.reject(new Error("no clipboard"));
      }
      copied.then(function () { exportFlash("Copied"); }, function () {
        pending.then(exportByHand, function () { exportFlash("Could not copy"); });
      });
    });
  }
  var importModal = document.getElementById("fit-import-modal");
  document.addEventListener("click", function (ev) {
    var opener = ev.target && ev.target.closest ? ev.target.closest("[data-fit-import-open]") : null;
    if (!opener || !importModal || !window.EveSynapseModal || !document.body.contains(editor)) return;
    window.EveSynapseModal.open(importModal);
  });
  // Last, so the phone view state above is initialized: land on
  // the current view (the Radial, on a fresh page).
  applyFitView();
})();

// --- ESI fittings list: client-side search + sort ----------------
// (lists are small; each fit is a card)
(function () {
  var q = document.getElementById("fit-esi-q");
  var sortSel = document.getElementById("fit-esi-sort");
  var list = document.getElementById("fit-esi-list");
  if (!q || !sortSel || !list) return;
  var inc = document.getElementById("fit-esi-include");
  var exc = document.getElementById("fit-esi-exclude");
  function apply() {
    var term = q.value.trim().toLowerCase();
    var incTerm = inc ? inc.value.trim().toLowerCase() : "";
    var excTerm = exc ? exc.value.trim().toLowerCase() : "";
    var secs = Array.prototype.slice.call(list.querySelectorAll(":scope > [data-fit-name]"));
    var shown = 0;
    secs.forEach(function (s) {
      var name = (s.getAttribute("data-fit-name") || "").toLowerCase();
      var ship = (s.getAttribute("data-fit-ship") || "").toLowerCase();
      var items = (s.getAttribute("data-fit-items") || "").toLowerCase();
      var hit = !term || name.indexOf(term) >= 0 || ship.indexOf(term) >= 0;
      if (hit && incTerm) hit = items.indexOf(incTerm) >= 0;
      if (hit && excTerm) hit = items.indexOf(excTerm) < 0;
      s.style.display = hit ? "" : "none";
      if (hit) shown++;
    });
    var none = document.getElementById("fit-esi-none");
    if (none) none.hidden = shown > 0;
    var mode = sortSel.value;
    var desc = mode.slice(-4) === "desc";
    var byShip = mode.indexOf("ship") === 0;
    secs.sort(function (a, b) {
      var x = (byShip ? a.getAttribute("data-fit-ship") : a.getAttribute("data-fit-name")) || "";
      var y = (byShip ? b.getAttribute("data-fit-ship") : b.getAttribute("data-fit-name")) || "";
      var c = x.toLowerCase().localeCompare(y.toLowerCase());
      return desc ? -c : c;
    });
    secs.forEach(function (s) { list.appendChild(s); });
  }
  q.addEventListener("input", apply);
  sortSel.addEventListener("change", apply);
  if (inc) inc.addEventListener("input", apply);
  if (exc) exc.addEventListener("input", apply);
})();

// --- My Loadouts: Modules and cargo preview -----------------
// Each saved-fit card renders its grouped item lists in a hidden
// panel; the trigger button opens it as a floating preview over
// the page instead of expanding the card in place. Handlers are
// delegated from the document so content swaps can't detach
// them. One preview at a time: a click pins it open, a second
// click (or an outside click, or Escape) closes it, and on fine
// pointers hovering a card opens its preview unpinned after a
// short delay.
(function () {
  var seq = 0;
  var openWrap = null;
  var openPinned = false;
  var openTimer = null;
  var closeTimer = null;
  var finePointer = !!(window.matchMedia && window.matchMedia("(hover: hover) and (pointer: fine)").matches);

  function parts(wrap) {
    return {
      btn: wrap.querySelector(".loadout-preview-btn"),
      panel: wrap.querySelector(".loadout-preview-panel")
    };
  }
  function clearTimers() {
    if (openTimer) { clearTimeout(openTimer); openTimer = null; }
    if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
  }
  function place(panel) {
    panel.classList.remove("flip-up");
    panel.classList.remove("align-right");
    panel.style.left = "";
    var r = panel.getBoundingClientRect();
    if (r.bottom > window.innerHeight - 8) panel.classList.add("flip-up");
    if (r.right > window.innerWidth - 8) panel.classList.add("align-right");
    // Right-aligning anchors the panel to the wrapper's right
    // edge, so a panel wider than the space from the viewport's
    // left edge to that anchor hangs off the left side. Measure
    // again after the flip and shift the panel back inside with
    // an inline left: its current offset from the wrapper plus
    // the deficit to the 8px viewport margin. The max-width cap
    // (100vw - 2rem) leaves room for the shift.
    r = panel.getBoundingClientRect();
    if (r.left < 8) {
      var wrap = panel.closest ? panel.closest(".loadout-preview") : null;
      if (wrap) {
        var wr = wrap.getBoundingClientRect();
        panel.style.left = ((r.left - wr.left) + (8 - r.left)) + "px";
      }
    }
  }
  function open(wrap, pinned) {
    if (openWrap && openWrap !== wrap) close();
    var p = parts(wrap);
    if (!p.btn || !p.panel) return;
    if (!p.panel.id) {
      seq++;
      p.panel.id = "loadout-preview-panel-" + seq;
      p.btn.setAttribute("aria-controls", p.panel.id);
    }
    p.panel.hidden = false;
    p.btn.setAttribute("aria-expanded", "true");
    place(p.panel);
    openWrap = wrap;
    openPinned = pinned;
  }
  function close() {
    clearTimers();
    if (!openWrap) return;
    var p = parts(openWrap);
    if (p.panel) {
      p.panel.hidden = true;
      p.panel.style.left = "";
    }
    if (p.btn) p.btn.setAttribute("aria-expanded", "false");
    openWrap = null;
    openPinned = false;
  }
  function wrapOf(el) {
    return el && el.closest ? el.closest(".loadout-preview") : null;
  }
  if (window.addEventListener) window.addEventListener("resize", function () {
    if (!openWrap) return;
    var p = parts(openWrap);
    if (p.panel) place(p.panel);
  });
  document.addEventListener("click", function (ev) {
    var btn = ev.target && ev.target.closest ? ev.target.closest(".loadout-preview-btn") : null;
    var wrap = wrapOf(ev.target);
    if (btn && wrap) {
      clearTimers();
      if (openWrap === wrap) {
        if (openPinned) close();
        else openPinned = true; // a hover-opened preview pins on click
      } else {
        open(wrap, true);
      }
      return;
    }
    if (openWrap && !wrap) close();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key !== "Escape" || !openWrap) return;
    var btn = parts(openWrap).btn;
    close();
    if (btn && btn.focus) btn.focus();
  });
  if (finePointer) {
    document.addEventListener("pointerover", function (ev) {
      var wrap = wrapOf(ev.target);
      if (!wrap || wrap === openWrap) return;
      if (ev.relatedTarget && wrap.contains(ev.relatedTarget)) return;
      clearTimers();
      openTimer = setTimeout(function () { openTimer = null; open(wrap, false); }, 200);
    });
    document.addEventListener("pointerout", function (ev) {
      var wrap = wrapOf(ev.target);
      if (!wrap) return;
      if (ev.relatedTarget && wrap.contains(ev.relatedTarget)) return;
      if (openTimer) { clearTimeout(openTimer); openTimer = null; }
      if (openWrap === wrap && !openPinned) {
        closeTimer = setTimeout(function () { closeTimer = null; close(); }, 250);
      }
    });
  }
})();

// --- read-only workbench tooltips ---------------------------
// A public fit's page renders the same workbench without the
// editor, and the editor block above bails when there is no
// #fit-editor, so read-only surfaces get their own wiring
// here. It only ever touches .fit-ro elements, shows the
// shared tooltip (FitTipUI) with no state handler, and is
// delegated from the document so content swaps can't
// detach it.
(function () {
  var tipEl = null;
  var hoverEl = null;
  var closeTimer = null;
  var pinned = false;
  // Drone gauges on every workbench already on the page (the
  // editor re-applies its own after each re-render).
  var gaugeWbs = document.querySelectorAll(".fit-wb");
  for (var gi = 0; gi < gaugeWbs.length; gi++) FitGauges.apply(gaugeWbs[gi]);
  function closeTip() {
    if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
    if (tipEl && tipEl.parentNode) tipEl.parentNode.removeChild(tipEl);
    tipEl = null;
    hoverEl = null;
    pinned = false;
  }
  function showTip(el) {
    closeTip();
    var wb = el.closest ? el.closest(".fit-wb") : null;
    if (!wb) return;
    tipEl = FitTipUI.build(el, { onClose: closeTip });
    if (!tipEl) return;
    hoverEl = el;
    tipEl.style.visibility = "hidden";
    wb.appendChild(tipEl);
    FitTipUI.place(tipEl, el, wb);
    tipEl.style.visibility = "";
  }
  // The editor's rules: hover shows, leaving closes after a short
  // delay, a click pins until a second click, a click elsewhere,
  // the close button or Escape.
  function scheduleClose() {
    if (closeTimer) clearTimeout(closeTimer);
    if (pinned) return;
    closeTimer = setTimeout(function () {
      closeTimer = null;
      if (!tipEl || pinned) return;
      if (tipEl.matches(":hover") || (hoverEl && hoverEl.matches(":hover"))) return;
      closeTip();
    }, 400);
  }
  function roTarget(ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest(".fit-ro [data-tip-name]") : null;
    if (t && t.closest && t.closest("#fit-editor")) return null;
    return t;
  }
  document.addEventListener("mouseover", function (ev) {
    var el = roTarget(ev);
    if (el) {
      if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
      if (el !== hoverEl) showTip(el);
      return;
    }
    if (tipEl && ev.target && ev.target.closest && ev.target.closest(".fit-tip")) {
      if (closeTimer) { clearTimeout(closeTimer); closeTimer = null; }
    }
  });
  document.addEventListener("mouseout", function (ev) {
    var el = roTarget(ev);
    if (el && el === hoverEl) {
      var into = ev.relatedTarget;
      if (!(into && el.contains(into))) scheduleClose();
      return;
    }
    if (tipEl && ev.target && ev.target.closest && ev.target.closest(".fit-tip")) {
      var to = ev.relatedTarget;
      if (!to || !to.closest || !to.closest(".fit-tip")) scheduleClose();
    }
  });
  document.addEventListener("click", function (ev) {
    // The cargo list's fold line (the editor's own copy is wired
    // with its state above).
    var fold = ev.target && ev.target.closest ? ev.target.closest(".fit-ro [data-fit-fold]") : null;
    if (fold && !fold.closest("#fit-editor")) {
      var group = fold.closest(".fit-group");
      var open = !group.classList.contains("open");
      group.classList.toggle("open", open);
      fold.setAttribute("aria-expanded", open ? "true" : "false");
      var label = fold.querySelector("span");
      if (label) {
        if (!label.getAttribute("data-closed")) label.setAttribute("data-closed", label.textContent);
        label.textContent = open ? "show fewer" : label.getAttribute("data-closed");
      }
      return;
    }
    var el = roTarget(ev);
    if (el && ev.target.closest("[data-fit-info]")) { closeTip(); return; } // the details drawer opens instead
    if (el) {
      if (hoverEl === el && tipEl && pinned) { closeTip(); return; }
      if (hoverEl !== el || !tipEl) showTip(el);
      if (tipEl) pinned = true;
      return;
    }
    if (tipEl && !(ev.target && ev.target.closest && ev.target.closest(".fit-tip"))) closeTip();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") closeTip();
  });
})();
