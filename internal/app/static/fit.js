// Fitting simulator editor (v0.3.21): the browser keeps the fit
// document (ship, items, charge choices) and posts it to
// /fittings/simulate/ on every change; the server answers with
// the re-rendered workbench fragment, which swaps in place. The
// canonical document rides back on the fragment root, so the
// editor state always matches what the server calculated.
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
  var picker = document.getElementById("fit-picker");
  var pickerHint = document.getElementById("fit-picker-hint");
  var pickerList = document.getElementById("fit-picker-list");
  var pickerFamily = document.getElementById("fit-picker-family");
  var saveBtn = document.getElementById("fit-save");
  var exportBtn = document.getElementById("fit-export-btn");
  var exportOut = document.getElementById("fit-eft-out");

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
  // After a tooltip state change re-simulates, the re-rendered
  // workbench replaces the tooltip's slot: this key re-opens the
  // tooltip on the new element so it stays open on the new state.
  var pendingTipKey = null;
  function simulate() {
    // Coalesce bursts: one request in flight at a time; a change
    // made mid-flight re-sends once with the latest state instead
    // of piling up stale POSTs.
    if (simInFlight) { simQueued = true; return; }
    simInFlight = true;
    state.name = nameInput ? nameInput.value : (state.name || "");
    var seq = ++simSeq;
    var payload = {
      name: state.name,
      shipTypeId: state.shipTypeId,
      items: state.items,
      charges: state.charges,
      pilot: pilotID(),
      clone: cloneID()
    };
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
      current.replaceWith(fresh);
      var next = readState();
      if (next) {
        next.items = next.items || [];
        next.charges = next.charges || {};
        state = next;
        normItems();
      }
      if (pendingTipKey) {
        var key = pendingTipKey;
        pendingTipKey = null;
        var slot = editor.querySelector('.fit-vslot[data-tip-key="' + key + '"]');
        if (slot) {
          showTip(slot);
          // Match toggleTip's key so a later tap on the same
          // slot closes the tooltip instead of re-showing it.
          if (tipEl) tipEl.setAttribute("data-for",
            slot.getAttribute("data-v-index") + ":" + (slot.getAttribute("data-v-family") || ""));
        }
      }
      syncFilterUI();
      syncSubsystemUI();
      syncUndoButtons();
      runPendingFlash();
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
      el = editor.querySelector('select[data-fit-charge="' + f.id + '"]');
      if (!el) el = editor.querySelector('.fit-vslot[data-fit-vslot="' + f.id + '"] .fit-vammo');
    }
    if (!el) return;
    el.classList.add("flash");
    if (el.scrollIntoView) el.scrollIntoView({ block: "nearest" });
    window.setTimeout(function () { el.classList.remove("flash"); }, 750);
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

  function findItem(typeID) {
    for (var i = 0; i < state.items.length; i++) {
      if (state.items[i].typeId === typeID) return i;
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
      charges: JSON.parse(JSON.stringify(state.charges || {}))
    };
  }
  function applySnap(s) {
    state.shipTypeId = s.shipTypeId;
    state.items = JSON.parse(JSON.stringify(s.items || []));
    state.charges = JSON.parse(JSON.stringify(s.charges || {}));
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

  // --- ship-restricted modules -------------------------------------
  // The workbench carries data-fit-restricted: {typeID: "Dreadnoughts"}.
  function restrictionFor(typeID) {
    var wb = workbench();
    if (!wb) return "";
    try {
      var m = JSON.parse(wb.getAttribute("data-fit-restricted") || "{}");
      return m[String(typeID)] || "";
    } catch (e) { return ""; }
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
  function checkRestricted(typeID, name) {
    var req = restrictionFor(typeID);
    if (req) {
      fitNotice((name || "That module") + " can only be fitted to " + req + ".", true, false);
      return true;
    }
    return false;
  }

  // Slot families with a fixed maximum: adding past it is a
  // plain-language refusal, never a silent no-op.
  var fitFamLabels = { high: "high", medium: "mid", low: "low", rig: "rig" };
  function addItem(typeID, qty) {
    var fam = familyOfType(typeID) || "";
    if (fitFamLabels[fam]) {
      var max = 0;
      var g = editor.querySelector('.fit-group[data-fit-group="' + fam + '"]');
      if (g) max = parseInt(g.getAttribute("data-fit-max") || "0", 10) || 0;
      var count = 0;
      for (var i = 0; i < state.items.length; i++) {
        if ((familyOfType(state.items[i].typeId) || "") === fam) count += state.items[i].qty || 1;
      }
      if (max > 0 && count >= max) {
        fitNotice("No free " + fitFamLabels[fam] + " slots — remove something first.", true, false);
        return;
      }
    }
    pushHistory();
    var i = findItem(typeID);
    if (i >= 0) {
      state.items[i].qty += qty;
      for (var k = 0; k < qty; k++) state.items[i].states.push("");
    } else {
      state.items.push({ typeId: typeID, qty: qty, states: [] });
    }
    normItems();
    markDirty();
    pendingFlash = fam === "drone" || fam === "cargo"
      ? { type: "group", family: fam }
      : { type: "slot", id: typeID };
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
  function decItem(typeID) {
    pushHistory();
    var i = findItem(typeID);
    if (i < 0) return;
    var fam = familyOfType(typeID) || "";
    state.items[i].qty -= 1;
    state.items[i].states.pop();
    if (state.items[i].qty <= 0) state.items.splice(i, 1);
    markDirty();
    // The removed module is gone; flash its group so the removal
    // reads as an action, not a glitch.
    pendingFlash = fam ? { type: "group", family: fam } : null;
    simulate();
  }
  function setItemQty(typeID, qty) {
    pushHistory();
    var i = findItem(typeID);
    if (qty <= 0) {
      if (i >= 0) state.items.splice(i, 1);
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
      if (t.closest("[data-fit-undo]")) { undo(); return; }
      if (t.closest("[data-fit-redo]")) { redo(); return; }
      var dec = t.getAttribute("data-fit-dec");
      if (dec) { decItem(parseInt(dec, 10) || 0); return; }
      var inc = t.getAttribute("data-fit-inc");
      if (inc) { addItem(parseInt(inc, 10) || 0, 1); return; }
      var add = t.getAttribute("data-fit-add");
      if (add) {
        if (add === "drone") { focusModuleSearch("drone"); return; }
        openPicker(add);
        return;
      }
    }
  });
  editor.addEventListener("change", function (ev) {
    var t = ev.target;
    if (!t || !t.getAttribute) return;
    var chargeFor = t.getAttribute("data-fit-charge");
    if (chargeFor) {
      var weapon = parseInt(chargeFor, 10) || 0;
      var charge = parseInt(t.value, 10) || 0;
      if (charge > 0) state.charges[weapon] = charge;
      else delete state.charges[weapon];
      pendingFlash = { type: "charge", id: weapon };
      simulate();
      return;
    }
    var qtyFor = t.getAttribute("data-fit-qty");
    if (qtyFor) {
      setItemQty(parseInt(qtyFor, 10) || 0, parseInt(t.value, 10) || 0);
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
        fit: { name: nm, shipTypeId: state.shipTypeId, items: state.items, charges: state.charges }
      })
    }).catch(function () {});
  });
  syncFilterUI();
  syncSubsystemUI();

  // --- split searches: ships at the top, modules above the workbench --
  // Ship search: ships only. Module search: modules/drones/rigs/subs
  // only, with the kind-labeled suggestions and the filter row.
  var modFamily = "all";
  var kindOrder = ["high", "medium", "low", "rig", "subsystem", "drone", "charge"];
  var kindLabels = {
    high: "High slots", medium: "Mid slots", low: "Low slots",
    rig: "Rigs", subsystem: "Subsystems", drone: "Drones", charge: "Charges"
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
      "&q=" + encodeURIComponent(q) + filterQuery();
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
    ok["ship"] = true;
    return ok;
  }
  function applyFittable(rows) {
    var ok = fittableKinds();
    var restricted = restrictedSet();
    if (!ok && !restricted) return rows;
    return (rows || []).filter(function (it) {
      // Ship-restricted modules never list under "Fittable only"
      // for an incompatible hull.
      if (restricted && restricted[it.id]) return false;
      return !ok || !!ok[it.kind || ""];
    });
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
    }
    function icon(id) {
      var img = document.createElement("img");
      img.className = "sug-icon";
      img.alt = "";
      img.loading = "lazy";
      img.draggable = false;
      img.src = "https://images.evetech.net/types/" + id + "/icon?size=32";
      return img;
    }
    function query(force) {
      if (!input || !list) return;
      var q = input.value.trim();
      if (q.length < 2 && !force) { close(); return; }
      fetch(pickerURL(cfg.family(), q), {
        cache: "no-store", headers: { "Accept": "application/json" }
      }).then(function (resp) {
        return resp.ok ? resp.json() : [];
      }).then(function (data) {
        var filtered = applyFittable(data || []);
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
          li.appendChild(icon(it.id));
          var name = document.createElement("span");
          name.textContent = it.name;
          li.appendChild(name);
          if (it.label) {
            var lab = document.createElement("span");
            lab.className = "sug-label";
            lab.textContent = it.label;
            li.appendChild(lab);
          }
          rows.push({ it: it, li: li });
          li.addEventListener("mousedown", function (ev) {
            ev.preventDefault();
            cfg.pick(it);
          });
          list.appendChild(li);
        });
      });
      if (!any) {
        var li = document.createElement("li");
        li.className = "sug-empty";
        li.textContent = "Nothing found — try a different search.";
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
            cfg.pick(rows[active].it);
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

  // Charges can't be fitted as items: set them on a fitted weapon's
  // ammunition select when one takes them, else fall back to the
  // picker so the user can browse.
  function pickCharge(it) {
    var sels = editor.querySelectorAll("select[data-fit-charge]");
    for (var i = 0; i < sels.length; i++) {
      var opts = sels[i].options;
      for (var j = 0; j < opts.length; j++) {
        if (String(opts[j].value) === String(it.id)) {
          sels[i].value = opts[j].value;
          var ev;
          try {
            ev = new Event("change", { bubbles: true });
          } catch (e) {
            ev = document.createEvent("Event");
            ev.initEvent("change", true, true);
          }
          sels[i].dispatchEvent(ev);
          if (modInput) modInput.value = "";
          return;
        }
      }
    }
    if (modInput) modInput.value = it.name;
    openPicker("any");
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
    }
  });
  var modSearch = makeSuggester(modInput, modList, {
    family: function () { return modFamily; },
    dropShips: true,
    pick: function (it) {
      var kind = it.kind || "";
      if (kind === "charge") { pickCharge(it); return; }
      if (kind === "high" || kind === "medium" || kind === "low" ||
          kind === "rig" || kind === "subsystem" || kind === "drone") {
        if (checkRestricted(it.id, it.name)) return;
        addItem(it.id, 1);
        return;
      }
      // Anything else (e.g. cargo items): jump the picker to it.
      if (modInput) modInput.value = it.name;
      openPicker("any");
    }
  });

  if (famChips) {
    famChips.addEventListener("click", function (ev) {
      var b = ev.target && ev.target.closest ? ev.target.closest("button[data-fam]") : null;
      if (!b) return;
      setFamChip(b.getAttribute("data-fam"));
      // The user took the wheel: a chip set by the drone-add flow
      // no longer restores.
      modSearchReturnFam = null;
    });
  }
  function setFamChip(fam) {
    modFamily = fam;
    if (famChips) {
      var btns = famChips.querySelectorAll("button[data-fam]");
      for (var i = 0; i < btns.length; i++) {
        btns[i].classList.toggle("on", btns[i].getAttribute("data-fam") === fam);
      }
    }
    modSearch.query();
  }
  // Drones flow: one search to rule them all. The "+ Add drones"
  // control focuses the module search with the Drones chip
  // pre-selected; drones are picked from the normal results. When
  // the user clears the search or picks another chip, the previous
  // filter state restores automatically.
  var modSearchReturnFam = null;
  function focusModuleSearch(fam) {
    if (modSearchReturnFam === null) modSearchReturnFam = modFamily;
    if (modInput) modInput.value = "";
    setFamChip(fam);
    modSearch.query(true); // empty query lists the whole family
    if (modInput) {
      if (modInput.scrollIntoView) modInput.scrollIntoView({ block: "center" });
      try { modInput.focus({ preventScroll: true }); } catch (e) { modInput.focus(); }
    }
  }
  if (modInput) {
    modInput.addEventListener("input", function () {
      if (modInput.value.trim() === "" && modSearchReturnFam !== null) {
        var back = modSearchReturnFam;
        modSearchReturnFam = null;
        setFamChip(back);
      }
    });
  }
  function filtersChanged() {
    modSearch.query();
    if (picker && !picker.hidden) queryPicker();
  }
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

  // --- module picker -------------------------------------------
  function openPicker(family) {
    if (!picker) return;
    picker.hidden = false;
    if (pickerFamily && family) pickerFamily.value = family;
    queryPicker();
    picker.scrollIntoView({ block: "nearest" });
  }
  function closePicker() { if (picker) picker.hidden = true; }
  function queryPicker() {
    if (!pickerList) return;
    var family = pickerFamily ? pickerFamily.value : "any";
    var q = modInput ? modInput.value.trim() : "";
    fetch(pickerURL(family, q), {
      cache: "no-store", headers: { "Accept": "application/json" }
    }).then(function (resp) {
      return resp.ok ? resp.json() : [];
    }).then(function (rows) {
      pickerList.innerHTML = "";
      (applyFittable(rows) || []).forEach(function (it) {
        var li = document.createElement("li");
        li.setAttribute("data-fit-pick", String(it.id));
        if (it.kind) li.setAttribute("data-kind", it.kind);
        // Tap adds (handled by the pointer controller); press-and-
        // drag moves it onto a slot. No mousedown+preventDefault
        // here — that kills the drag gesture before it starts.
        li.appendChild(suggestIcon(it.id));
        var name = document.createElement("span");
        name.textContent = it.name;
        li.appendChild(name);
        if (it.label) {
          var lab = document.createElement("span");
          lab.className = "sug-label";
          lab.textContent = it.label;
          li.appendChild(lab);
        }
        pickerList.appendChild(li);
      });
      if (!rows || rows.length === 0) {
        var li = document.createElement("li");
        li.textContent = "Nothing found — try a different search.";
        pickerList.appendChild(li);
      }
      if (pickerHint) {
        if (q.length >= 2) {
          pickerHint.textContent = "matching \u201c" + q + "\u201d";
          pickerHint.hidden = false;
        } else {
          pickerHint.hidden = true;
        }
      }
    }).catch(function () {});
  }
  if (pickerFamily) pickerFamily.addEventListener("change", queryPicker);
  var pickerClose = document.getElementById("fit-picker-close");
  if (pickerClose) pickerClose.addEventListener("click", closePicker);

  // --- pointer drag and drop --------------------------------------
  // In-game behavior, on mouse AND touch: tap shows the tooltip
  // (never deletes); drag moves between slots. Dropping on an
  // empty highlighted slot moves the module there; on an occupied
  // highlighted slot the two swap. Invalid drops cancel;
  // drag-off-to-delete still removes (the chip minus buttons stay
  // the explicit touch path).
  //
  // Why Pointer Events and not HTML5 drag-and-drop: mobile
  // browsers never fire dragstart/dragover/drop for touch, so the
  // old HTML5 wiring was dead on phones; and the picker's
  // mousedown+preventDefault killed the native drag on desktop
  // too. One pointer controller serves both. Slots carry
  // data-v-family / data-v-index (doc order); the reorder applies
  // against state.items directly.
  var DRAG_PX = 12;
  var dragState = null; // pending or active pointer drag
  var eatNextClick = false; // the synthetic click after a drag-source pointerup
  function dragSourceAt(ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot.filled,[data-fit-pick]") : null;
    return t;
  }
  function slotAtPoint(x, y) {
    var el = document.elementFromPoint ? document.elementFromPoint(x, y) : null;
    var t = el && el.closest ? el.closest(".fit-vslot") : null;
    return t;
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
    } else {
      g.textContent = d.el.textContent;
    }
    document.body.appendChild(g);
    return g;
  }
  function moveGhost(d, x, y) {
    if (!d.ghost) return;
    d.ghost.style.left = (x - 22) + "px";
    d.ghost.style.top = (y - 22) + "px";
  }
  function clearDragMarks() {
    var marks = editor.querySelectorAll(".fit-vslot.drop-ok,.fit-vslot.drop-dim");
    for (var i = 0; i < marks.length; i++) {
      marks[i].classList.remove("drop-ok");
      marks[i].classList.remove("drop-dim");
    }
  }
  function highlightForDrag(d) {
    clearDragMarks();
    if (restrictionFor(d.typeID)) return; // lights up nothing
    var slots = editor.querySelectorAll(".fit-vslot");
    for (var i = 0; i < slots.length; i++) {
      var s = slots[i];
      if (d.kind === "charge") {
        // Charge drag: highlight filled weapon slots (Issue 2)
        if (s.classList.contains("filled") && s.getAttribute("data-fit-vslot")) {
          s.classList.add("drop-ok");
        } else {
          s.classList.add("drop-dim");
        }
      } else {
        var fam = s.getAttribute("data-v-family") || "";
        if (d.family && fam === d.family) s.classList.add("drop-ok");
        else s.classList.add("drop-dim");
      }
    }
  }
  function startPointerDrag(ev, d) {
    if (restrictionFor(d.typeID)) { dragState = null; return; }
    d.active = true;
    closeTip();
    try { d.el.setPointerCapture(ev.pointerId); } catch (e) { /* not critical */ }
    d.ghost = dragGhost(d);
    moveGhost(d, ev.clientX, ev.clientY);
    d.el.classList.add("drag-src");
    highlightForDrag(d);
    movePointerDrag(ev, d);
  }
  function movePointerDrag(ev, d) {
    moveGhost(d, ev.clientX, ev.clientY);
    var slot = slotAtPoint(ev.clientX, ev.clientY);
    d.dropIndex = -1;
    if (slot) {
      var fam = slot.getAttribute("data-v-family") || "";
      if (d.kind === "charge") {
        // Charge drag: highlight filled weapon slots (Issue 2)
        if (slot.classList.contains("filled") && slot.getAttribute("data-fit-vslot")) {
          d.dropIndex = parseInt(slot.getAttribute("data-v-index") || "-1", 10);
          if (!(d.dropIndex >= 0)) d.dropIndex = -1;
        }
      } else if (d.family && fam === d.family && !restrictionFor(d.typeID)) {
        d.dropIndex = parseInt(slot.getAttribute("data-v-index") || "-1", 10);
        if (!(d.dropIndex >= 0)) d.dropIndex = -1;
      }
    }
  }
  function endPointerDrag(ev, d) {
    if (d.ghost && d.ghost.parentNode) d.ghost.parentNode.removeChild(d.ghost);
    d.el.classList.remove("drag-src");
    clearDragMarks();
    // The synthetic click lands right after pointerup; eat exactly it.
    eatNextClick = true;
    var typeID = d.typeID, fam = d.family, idx = d.dropIndex, fromFit = d.fromFit;
    if (idx >= 0) {
      if (fromFit) {
        pendingFlash = { type: "slot", id: typeID };
        moveItemTo(typeID, fam, idx);
      } else if (d.kind === "charge") {
        // Charge dropped on a weapon slot: validate then set as ammo (Issue 2)
        var slotEl = editor.querySelector('.fit-vslot[data-v-index="' + idx + '"]');
        if (slotEl) {
          var weaponID = parseInt(slotEl.getAttribute("data-fit-vslot") || "0", 10);
          if (weaponID > 0) {
            // Validate charge compatibility via the picker API
            fetch("/fittings/picker.json?family=charge&weapon=" + weaponID, {
              headers: { "Accept": "application/json" }
            }).then(function(resp) {
              return resp.ok ? resp.json() : [];
            }).then(function(charges) {
              var valid = charges.some(function(ch) { return ch.id === typeID; });
              if (valid) {
                state.charges[weaponID] = typeID;
                pendingFlash = { type: "charge", id: weaponID };
                simulate();
              } else {
                // Invalid charge for this weapon - flash the slot red
                slotEl.classList.add("fit-invalid");
                setTimeout(function() { slotEl.classList.remove("fit-invalid"); }, 1000);
              }
            }).catch(function() {
              // On error, allow it (backend will handle)
              state.charges[weaponID] = typeID;
              pendingFlash = { type: "charge", id: weaponID };
              simulate();
            });
          }
        }
      } else if (!checkRestricted(typeID, "")) {
        addItem(typeID, 1); // arms its own flash
      }
    } else if (fromFit) {
      // Dropped off the display: same as the minus button.
      decItem(typeID);
    }
    // A picker drag to nowhere simply cancels.
  }
  // Tap (press without drag): slot toggles the tooltip, the ammo
  // badge jumps to the charge picker, a picker row adds its item.
  function tapDragSource(ev, d) {
    var ammo = ev.target && ev.target.closest ? ev.target.closest("[data-fit-ammo]") : null;
    if (ammo) { jumpToCharge(ammo.getAttribute("data-fit-ammo")); return; }
    if (d.fromFit) {
      toggleTip(d.el);
      return;
    }
    if (!checkRestricted(d.typeID, "")) addItem(d.typeID, 1);
  }
  editor.addEventListener("pointerdown", function (ev) {
    if (ev.button !== undefined && ev.button > 0) return;
    if (dragState) return;
    var t = dragSourceAt(ev);
    if (!t) return;
    var vslot = t.getAttribute("data-fit-vslot");
    var pick = t.getAttribute("data-fit-pick");
    var typeID = parseInt(vslot || pick || "0", 10) || 0;
    if (!typeID) return;
    dragState = {
      el: t, typeID: typeID, fromFit: !!vslot,
      family: vslot ? (t.getAttribute("data-v-family") || "") : pickerDropFamily(),
      kind: t.getAttribute("data-kind") || "",
      x0: ev.clientX, y0: ev.clientY, pid: ev.pointerId,
      active: false, dropIndex: -1, ghost: null,
      mayScroll: !vslot // picker rows live in a scrollable list
    };
  });
  // The bottom picker's family select is the drop family for picker
  // drags ("any" has no slot family, so those taps only).
  function pickerDropFamily() {
    var v = pickerFamily ? pickerFamily.value : "";
    return v === "any" ? "" : v;
  }
  editor.addEventListener("pointermove", function (ev) {
    var d = dragState;
    if (!d || ev.pointerId !== d.pid) return;
    var dx = ev.clientX - d.x0, dy = ev.clientY - d.y0;
    if (!d.active) {
      if (Math.sqrt(dx * dx + dy * dy) < DRAG_PX) return;
      // A mostly-vertical press on a picker row is a list scroll,
      // not a drag — hand it back to the browser.
      if (d.mayScroll && Math.abs(dy) > Math.abs(dx) * 1.4) { dragState = null; return; }
      startPointerDrag(ev, d);
      return;
    }
    movePointerDrag(ev, d);
  });
  function releasePointerDrag(ev) {
    var d = dragState;
    if (!d || ev.pointerId !== d.pid) return;
    dragState = null;
    // Eat the synthetic click after pointerup so a tap never
    // double-fires (show-then-dismiss) and a drag never leaks a
    // click into the workbench handlers. A fresh press clears it,
    // so a cancelled click can't eat a later real one.
    eatNextClick = true;
    if (d.active) endPointerDrag(ev, d);
    else tapDragSource(ev, d);
  }
  editor.addEventListener("pointerup", releasePointerDrag);
  editor.addEventListener("pointercancel", function (ev) {
    var d = dragState;
    if (!d || ev.pointerId !== d.pid) return;
    dragState = null;
    if (d.active) {
      if (d.ghost && d.ghost.parentNode) d.ghost.parentNode.removeChild(d.ghost);
      d.el.classList.remove("drag-src");
      clearDragMarks();
    }
  });
  // A real drag ends in a click; eat exactly that one so taps and
  // drags never double-fire. Clicks inside the tooltip itself
  // (its close button) always pass through. Any fresh press
  // clears the flag first, so a click that never arrives can't
  // eat a later genuine one.
  editor.addEventListener("pointerdown", function () { eatNextClick = false; }, true);
  editor.addEventListener("click", function (ev) {
    if (eatNextClick) {
      eatNextClick = false;
      if (ev.target && ev.target.closest && ev.target.closest(".fit-tip")) return;
      ev.stopPropagation();
      ev.preventDefault();
    }
  }, true);

  // A long-press must never pop the context menu mid-drag.
  editor.addEventListener("contextmenu", function (ev) {
    var t = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot,[data-fit-pick]") : null;
    if (t) ev.preventDefault();
  });

  // moveItemTo relocates one instance of typeID within its slot
  // family to doc-order position idx (counting only that
  // family's items). Dropping on an occupied slot swaps the two
  // modules; on an empty slot it moves there (or to the end of
  // the family when the slot is beyond the last item).
  function moveItemTo(typeID, fam, idx) {
    var famIdx = [];
    for (var i = 0; i < state.items.length; i++) {
      if ((familyOfType(state.items[i].typeId) || "") === fam) famIdx.push(i);
    }
    var fromPos = -1;
    for (var p = 0; p < famIdx.length; p++) {
      if (state.items[famIdx[p]].typeId === typeID) { fromPos = p; break; }
    }
    if (fromPos < 0) { addItem(typeID, 1); return; }
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
    var rows = wb.querySelectorAll("[data-fit-rowtype]");
    for (var i = 0; i < rows.length; i++) {
      if (rows[i].getAttribute("data-fit-rowtype") === String(typeID)) {
        var g = rows[i].closest(".fit-group");
        return g ? (g.getAttribute("data-fit-group") || "") : "";
      }
    }
    return "";
  }
  // --- slot tooltips ----------------------------------------------
  // Hover (desktop) or tap (mobile) on a fitted icon shows the
  // quick-look tooltip: name, slot, meta, CPU/PG. Positioned
  // outward from the ship center so it never covers the render.
  // Tap never navigates or deletes. Dismissible via the close
  // button, tapping elsewhere, or Escape.
  var tipEl = null;
  function closeTip() {
    if (tipEl && tipEl.parentNode) tipEl.parentNode.removeChild(tipEl);
    tipEl = null;
  }
  function showTip(slot) {
    closeTip();
    var name = slot.getAttribute("data-tip-name") || "";
    var sub = slot.getAttribute("data-tip-sub") || "";
    var meta = slot.getAttribute("data-tip-meta") || "";
    var cpu = slot.getAttribute("data-tip-cpu") || "";
    var pg = slot.getAttribute("data-tip-pg") || "";
    var stat = slot.getAttribute("data-tip-stat") || "";
    var charge = slot.getAttribute("data-tip-charge") || "";
    var wb = workbench();
    if (!wb || !name) return;
    tipEl = document.createElement("div");
    tipEl.className = "fit-tip";
    tipEl.setAttribute("role", "tooltip");
    var h = document.createElement("div");
    h.className = "fit-tip-name";
    h.textContent = name;
    tipEl.appendChild(h);
    if (sub) {
      var s = document.createElement("div");
      s.className = "fit-tip-sub";
      s.textContent = sub;
      tipEl.appendChild(s);
    }
    var lines = [];
    if (meta) lines.push(meta);
    if (cpu) lines.push(cpu);
    if (pg) lines.push(pg);
    if (stat) lines.push(stat);
    if (lines.length) {
      var d = document.createElement("div");
      d.className = "fit-tip-stats";
      d.textContent = lines.join(" · ");
      tipEl.appendChild(d);
    }
    if (charge) {
      var c = document.createElement("div");
      c.className = "fit-tip-stats";
      c.textContent = charge;
      tipEl.appendChild(c);
    }
    // Module states: the server lists only the states this
    // module can take; the current one is highlighted. Tapping a
    // state applies it and re-simulates; the tooltip re-opens on
    // the new state once the workbench re-renders.
    var tipStates = slot.getAttribute("data-tip-states") || "";
    var tipState = slot.getAttribute("data-tip-state") || "";
    var tipKey = slot.getAttribute("data-tip-key") || "";
    if (tipStates && tipKey) {
      var labels = { offline: "Offline", online: "Online", active: "Active", overheated: "Overheat" };
      var row = document.createElement("div");
      row.className = "fit-tip-states";
      row.setAttribute("role", "group");
      row.setAttribute("aria-label", "Module state");
      tipStates.split(",").forEach(function (s) {
        if (!labels[s]) return;
        var b = document.createElement("button");
        b.type = "button";
        b.className = "fit-tip-state" + (s === tipState ? " on" : "");
        b.textContent = labels[s];
        b.setAttribute("aria-pressed", s === tipState ? "true" : "false");
        (function (st, btn) {
          btn.addEventListener("click", function (ev) {
            ev.stopPropagation();
            // Optimistic highlight; the re-render confirms it.
            var sibs = row.querySelectorAll(".fit-tip-state");
            for (var k = 0; k < sibs.length; k++) {
              sibs[k].classList.remove("on");
              sibs[k].setAttribute("aria-pressed", "false");
            }
            btn.classList.add("on");
            btn.setAttribute("aria-pressed", "true");
            setModuleState(tipKey, st);
          });
        })(s, b);
        row.appendChild(b);
      });
      tipEl.appendChild(row);
    }
    var x = document.createElement("button");
    x.className = "fit-tip-x";
    x.setAttribute("aria-label", "Close");
    x.textContent = "×";
    x.addEventListener("click", function (ev) { ev.stopPropagation(); closeTip(); });
    tipEl.appendChild(x);
    // Anchor to the slot icon: below it when there is room,
    // otherwise above; clamped inside the workbench so it can
    // never cover the page header. The workbench is the offset
    // parent (position: relative).
    tipEl.style.visibility = "hidden";
    wb.appendChild(tipEl);
    var wr = wb.getBoundingClientRect();
    var sr = slot.getBoundingClientRect();
    var tw = tipEl.offsetWidth || 190, th = tipEl.offsetHeight || 110;
    var cx = sr.left - wr.left + sr.width / 2;
    var below = sr.bottom - wr.top + 10;
    var above = sr.top - wr.top - th - 10;
    var ty = (below + th <= wr.height) ? below : Math.max(8, above);
    var tx = cx - tw / 2;
    if (tx < 8) tx = 8;
    if (tx + tw > wr.width - 8) tx = wr.width - tw - 8;
    if (tx < 8) tx = 8;
    tipEl.style.left = Math.round(tx) + "px";
    tipEl.style.top = Math.round(ty) + "px";
    tipEl.style.visibility = "";
  }
  // Jumps to a weapon's ammunition select (from its slot badge).
  function jumpToCharge(weaponID) {
    var sel = editor.querySelector('select[data-fit-charge="' + weaponID + '"]');
    if (!sel) return;
    closeTip();
    if (sel.scrollIntoView) sel.scrollIntoView({ block: "center" });
    try { sel.focus({ preventScroll: true }); } catch (e) { try { sel.focus(); } catch (e2) {} }
    sel.classList.add("flash");
    window.setTimeout(function () { sel.classList.remove("flash"); }, 750);
  }
  var tipHoverSlot = null;
  editor.addEventListener("mouseover", function (ev) {
    if (dragState && dragState.active) return; // no tooltips mid-drag
    var slot = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot[data-tip-name]") : null;
    if (slot && slot !== tipHoverSlot) {
      tipHoverSlot = slot;
      showTip(slot);
    }
  });
  var tipCloseTimer = null;
  function scheduleTipClose() {
    if (tipCloseTimer) clearTimeout(tipCloseTimer);
    tipCloseTimer = setTimeout(function() {
      tipCloseTimer = null;
      // Only close if mouse is not over the tooltip
      if (tipEl && !tipEl.matches(':hover')) {
        tipHoverSlot = null;
        closeTip();
      }
    }, 150);
  }
  editor.addEventListener("mouseout", function (ev) {
    var slot = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot[data-tip-name]") : null;
    if (slot && slot === tipHoverSlot) {
      scheduleTipClose();
    }
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
  function toggleTip(slot) {
    var key = slot.getAttribute("data-v-index") + ":" + (slot.getAttribute("data-v-family") || "");
    if (tipEl && tipEl.getAttribute("data-for") === key) {
      closeTip();
    } else {
      showTip(slot);
      if (tipEl) tipEl.setAttribute("data-for", key);
    }
  }
  // Sets one module instance's state (tipKey is "typeID:ordinal"
  // from the slot) and re-simulates through the coalesced path.
  // Undo history covers it like any other edit.
  function setModuleState(tipKey, newState) {
    var parts = (tipKey || "").split(":");
    var typeID = parseInt(parts[0], 10) || 0;
    var ordinal = parseInt(parts[1], 10) || 0;
    if (!typeID || ordinal < 0) return;
    var i = findItem(typeID);
    if (i < 0) return;
    normItems();
    var it = state.items[i];
    if (ordinal >= it.states.length || it.states[ordinal] === newState) return;
    pushHistory();
    it.states[ordinal] = newState;
    markDirty();
    pendingTipKey = tipKey;
    simulate();
  }
  editor.addEventListener("keydown", function (ev) {
    if (ev.key !== "Enter" && ev.key !== " ") return;
    var slot = ev.target && ev.target.closest ? ev.target.closest(".fit-vslot.filled") : null;
    if (!slot) return;
    ev.preventDefault();
    toggleTip(slot);
  });
  editor.addEventListener("click", function (ev) {
    if (tipEl && !(ev.target && ev.target.closest && ev.target.closest(".fit-tip"))) closeTip();
  });
  document.addEventListener("keydown", function (ev) {
    if (ev.key === "Escape") closeTip();
  });

  // --- subsystem UI gating ----------------------------------------
  // The subsystem ring/group/search-family only appear when the
  // hull supports subsystems (data-fit-subsystems on .fit-wb).
  function syncSubsystemUI() {
    var wb = workbench();
    var ok = wb && wb.getAttribute("data-fit-subsystems") === "1";
    var chip = document.querySelector('#fit-famchips button[data-fam="subsystem"]');
    if (chip) chip.hidden = !ok;
    var opt = document.querySelector('#fit-picker-family option[value="subsystem"]');
    if (opt) opt.hidden = !ok;
  }

  // --- save / export --------------------------------------------
  // Ownership chain (defense in depth):
  //   1. The "Your fits" search forks any non-owned fit via POST
  //      /fittings/fork/ before the editor ever sees it (yfLoad),
  //      so the editor only ever holds the signed-in user's own
  //      fits — public fits are read-only here and never edited
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
      a.href = "/auth/eve";
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
          fitNotice("Saved \u201c" + row.name + "\u201d to EVE — loading your in-game fitting…", false, false);
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
          charges: state.charges
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
  if (exportBtn) {
    exportBtn.addEventListener("click", function () {
      state.name = nameInput ? nameInput.value : state.name;
      fetch("/fittings/export/", {
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
        return resp.ok ? resp.text() : "";
      }).then(function (text) {
        if (exportOut) {
          exportOut.value = text;
          exportOut.focus();
          exportOut.select();
        }
      }).catch(function () {});
    });
  }
})();

// --- ESI fittings list: client-side search + sort ----------------
// (lists are small; the foldable sections now start collapsed)
(function () {
  var q = document.getElementById("fit-esi-q");
  var sortSel = document.getElementById("fit-esi-sort");
  var list = document.getElementById("fit-esi-list");
  if (!q || !sortSel || !list) return;
  function apply() {
    var term = q.value.trim().toLowerCase();
    var secs = Array.prototype.slice.call(list.querySelectorAll(":scope > section.foldable"));
    secs.forEach(function (s) {
      var name = (s.getAttribute("data-fit-name") || "").toLowerCase();
      var ship = (s.getAttribute("data-fit-ship") || "").toLowerCase();
      var hit = !term || name.indexOf(term) >= 0 || ship.indexOf(term) >= 0;
      s.style.display = hit ? "" : "none";
    });
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
})();
