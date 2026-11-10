// Killboard live feed: opens an EventSource to /intel/kills/stream
// (carrying the page's scope, if any) and slots each kill that
// arrives after load into the list by kill time, in the same markup
// the server renders. The list never grows past the page size: rows
// past 40 drop off the bottom. A kill that lands unpriced shows
// "pricing…" until its price event fills the value in. Only the
// first page is live; an older page (?before=) stays as it is.
//
// New kills stay highlighted until they are seen: a row fades back
// to normal a few seconds after it is on screen in a visible tab.
// The newest kill seen is remembered per scope in localStorage, so
// coming back to the page highlights the kills that arrived while
// you were away. The "Live" pill says whether the stream is open.
// The server-rendered first paint is untouched; without script the
// page is the list it always was.
(function () {
  "use strict";
  if (window.esKillboardLive) return;
  window.esKillboardLive = true;

  var PAGE_SIZE = 40; // killPageSize in killboard.go
  var HOLD_MS = 6000; // how long a seen row keeps its highlight
  var FADE_MS = 1500; // the fade back to normal (killboard.css)
  var SEEN_PREFIX = "kb-seen:";
  var source = null;
  var streamURL = "";
  var watcher = null;

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function img(src, cls, size) {
    return '<img class="' + cls + '" src="' + src + '" alt="" width="' + size + '" height="' + size + '" loading="lazy">';
  }

  // orgLinks renders the corporation and alliance links of one
  // party, the same shape the server renders under a name.
  function orgLinks(corpID, corpName, allianceID, allianceName) {
    var out = "";
    if (corpID) {
      out += '<a class="kb-org" href="/intel/kills/?corporation=' + corpID + '">' +
        img("https://images.evetech.net/corporations/" + corpID + "/logo?size=32", "", 14) +
        '<span class="kb-org-n">' + esc(corpName) + "</span></a>";
    }
    if (allianceID) {
      out += '<a class="kb-org" href="/intel/kills/?alliance=' + allianceID + '">' +
        img("https://images.evetech.net/alliances/" + allianceID + "/logo?size=32", "", 14) +
        '<span class="kb-org-n">' + esc(allianceName) + "</span></a>";
    }
    return out ? '<span class="kb-party-orgs">' + out + "</span>" : "";
  }

  function victimParty(ev) {
    var info = '<div class="kb-party-info"><span class="kb-role">Victim</span>';
    if (ev.structure) {
      var face = ev.corp ? img("https://images.evetech.net/corporations/" + ev.corp + "/logo?size=64", "kb-face", 34) : "";
      info += '<span class="kb-party-name">' +
        (ev.corp ? '<a href="/intel/kills/?corporation=' + ev.corp + '">' + esc(ev.corpName) + "</a>" : "") +
        "</span></div>";
      return face + info;
    }
    info += '<span class="kb-party-name"><a href="/intel/kills/?character=' + ev.victim + '">' + esc(ev.victimName) + "</a></span>" +
      orgLinks(ev.corp, ev.corpName, ev.alliance, ev.allianceName) + "</div>";
    return img("https://images.evetech.net/characters/" + ev.victim + "/portrait?size=64", "kb-face", 34) + info;
  }

  function attackerParty(ev) {
    if (ev.final) {
      return img("https://images.evetech.net/characters/" + ev.final + "/portrait?size=64", "kb-face", 34) +
        '<div class="kb-party-info"><span class="kb-role">Attacker</span>' +
        '<span class="kb-party-name"><a href="/intel/kills/?character=' + ev.final + '">' + esc(ev.finalName) + "</a></span>" +
        orgLinks(ev.finalCorp, ev.finalCorpName, ev.finalAlliance, ev.finalAllianceName) + "</div>";
    }
    if (ev.finalCorp) {
      return img("https://images.evetech.net/corporations/" + ev.finalCorp + "/logo?size=64", "kb-face", 34) +
        '<div class="kb-party-info"><span class="kb-role">Attacker</span>' +
        '<span class="kb-party-name"><a href="/intel/kills/?corporation=' + ev.finalCorp + '">' + esc(ev.finalCorpName) + "</a></span>" +
        (ev.finalAlliance ? '<span class="kb-party-orgs"><a class="kb-org" href="/intel/kills/?alliance=' + ev.finalAlliance + '">' +
          img("https://images.evetech.net/alliances/" + ev.finalAlliance + "/logo?size=32", "", 14) +
          '<span class="kb-org-n">' + esc(ev.finalAllianceName) + "</span></a></span>" : "") +
        "</div>";
    }
    return "";
  }

  function rowHTML(ev) {
    var tags = "";
    if (ev.solo) tags += ' <span class="kb-tag">solo</span>';
    if (ev.awox) tags += ' <span class="kb-tag warn">awox</span>';
    if (ev.npc) tags += ' <span class="kb-tag dim">NPC</span>';
    return '<li class="kb-kill' + (ev.big ? " big" : "") + '" data-kill-id="' + ev.id + '">' +
      img("https://images.evetech.net/types/" + ev.shipType + "/icon?size=64", "kb-kill-ship", 44) +
      '<div class="kb-kill-main">' +
      '<a class="kb-kill-link" href="/intel/kill/' + ev.id + '/">' + esc(ev.ship) + "</a>" +
      '<div class="kb-party">' + victimParty(ev) + "</div>" +
      "</div>" +
      '<div class="kb-party kb-atk">' + attackerParty(ev) + "</div>" +
      '<div class="kb-kill-where"><span class="sec sec-' + ev.secTier + '">' + esc(ev.sec) + '</span> ' +
      '<a href="/intel/kills/?system=' + ev.system + '">' + esc(ev.systemName) + '</a> ' +
      '<a class="kb-kill-region" href="/intel/kills/?region=' + ev.region + '">' + esc(ev.regionName) + "</a></div>" +
      '<div class="kb-kill-value">' + (ev.value ? esc(ev.value) : '<span class="muted">pricing…</span>') + "</div>" +
      '<div class="kb-kill-meta"><time datetime="' + esc(ev.time) + '" title="' + esc(ev.when) + ' EVE">' + esc(ev.ago) + "</time>" +
      '<span class="kb-att"> · ' + ev.attackers + " " + (ev.attackers === 1 ? "attacker" : "attackers") + "</span>" + tags + "</div>" +
      "</li>";
  }

  // --- Seen tracking ---------------------------------------------

  function seenKey() {
    return SEEN_PREFIX + streamURLFor();
  }

  function readSeen() {
    try {
      var v = localStorage.getItem(seenKey());
      return v === null ? null : +v || 0;
    } catch (err) {
      return null;
    }
  }

  function noteSeen(id) {
    try {
      var prev = readSeen();
      if (prev === null || id > prev) localStorage.setItem(seenKey(), String(id));
    } catch (err) {
      /* storage off: highlights still fade, nothing is remembered */
    }
  }

  function rowID(row) {
    return +row.getAttribute("data-kill-id") || 0;
  }

  // fade returns a seen row to normal and records it as seen.
  function fade(row) {
    if (watcher) watcher.unobserve(row);
    row.classList.remove("is-live");
    row.classList.add("is-fading");
    setTimeout(function () { row.classList.remove("is-fading"); }, FADE_MS);
    noteSeen(rowID(row));
  }

  // arm starts or stops a live row's hold timer: it runs only while
  // the row is on screen in a visible tab.
  function arm(row) {
    var viewing = row._kbOnScreen && document.visibilityState === "visible";
    if (viewing && !row._kbTimer) {
      row._kbTimer = setTimeout(function () { row._kbTimer = 0; fade(row); }, HOLD_MS);
    } else if (!viewing && row._kbTimer) {
      clearTimeout(row._kbTimer);
      row._kbTimer = 0;
    }
  }

  function markLive(row) {
    row.classList.add("is-live");
    if (!watcher) {
      if (!("IntersectionObserver" in window)) {
        row._kbOnScreen = true;
        arm(row);
        return;
      }
      watcher = new IntersectionObserver(function (entries) {
        entries.forEach(function (e) {
          e.target._kbOnScreen = e.isIntersecting;
          arm(e.target);
        });
      }, { threshold: 0.5 });
    }
    watcher.observe(row);
  }

  function rearmAll() {
    document.querySelectorAll(".kb-kill.is-live").forEach(arm);
  }

  // markUnseen highlights the rows that arrived since this scope's
  // list was last seen. A first visit highlights nothing and starts
  // the record from the newest row on the page.
  function markUnseen(list) {
    if (list.getAttribute("data-kb-seen") === "1") return;
    list.setAttribute("data-kb-seen", "1");
    var rows = list.querySelectorAll("li.kb-kill");
    var prev = readSeen();
    var newest = 0;
    rows.forEach(function (row) {
      var id = rowID(row);
      if (id > newest) newest = id;
      if (prev !== null && id > prev) markLive(row);
    });
    if (prev === null && newest) noteSeen(newest);
  }

  // --- Rows --------------------------------------------------------

  function killTime(row) {
    var t = row.querySelector("time[datetime]");
    return t ? Date.parse(t.getAttribute("datetime")) || 0 : 0;
  }

  // addKill slots a streamed kill into the list by kill time, newest
  // first, the order the server renders. A kill older than every
  // row on a full page is not news here and is skipped.
  function addKill(ev) {
    var list = document.querySelector(".kb-list");
    if (!list || !ev || !ev.id) return;
    if (list.querySelector('[data-kill-id="' + ev.id + '"]')) return;
    var at = Date.parse(ev.time) || 0;
    var rows = list.querySelectorAll("li.kb-kill");
    var before = null;
    for (var i = 0; i < rows.length; i++) {
      var t = killTime(rows[i]);
      if (at > t || (at === t && ev.id > rowID(rows[i]))) {
        before = rows[i];
        break;
      }
    }
    if (!before && rows.length >= PAGE_SIZE) return;
    var empty = list.querySelector("li.prose");
    if (empty) empty.remove();
    var head = list.querySelector("li.kb-head");
    if (!head) {
      head = document.createElement("li");
      head.className = "kb-head";
      head.setAttribute("aria-hidden", "true");
      head.innerHTML = '<span class="kb-h-detail">Details</span><span class="kb-h-ship">Ship</span><span class="kb-h-main">Victim</span><span class="kb-h-atk">Final blow</span><span class="kb-h-where">System</span><span class="kb-h-value">Est. value</span>';
      list.prepend(head);
    }
    var holder = document.createElement("div");
    holder.innerHTML = rowHTML(ev);
    var row = holder.firstChild;
    if (before) before.before(row);
    else list.append(row);
    markLive(row);
    rows = list.querySelectorAll("li.kb-kill");
    for (var j = PAGE_SIZE; j < rows.length; j++) {
      if (watcher) watcher.unobserve(rows[j]);
      clearTimeout(rows[j]._kbTimer);
      rows[j].remove();
    }
  }

  function setKillPrice(ev) {
    var row = document.querySelector('[data-kill-id="' + ev.id + '"]');
    if (!row) return;
    var cell = row.querySelector(".kb-kill-value");
    if (!cell) return;
    if (ev.value) {
      cell.textContent = ev.value;
      if (ev.big) row.classList.add("big");
    } else {
      cell.innerHTML = '<span class="muted">pricing…</span>';
    }
  }

  function streamURLFor() {
    var params = new URLSearchParams(location.search);
    var out = new URLSearchParams();
    ["system", "region", "ship", "character", "corporation", "alliance"].forEach(function (k) {
      var v = params.get(k);
      if (v) out.set(k, v);
    });
    ["big", "solo"].forEach(function (k) {
      if (params.get(k) === "1") out.set(k, "1");
    });
    var q = out.toString();
    return "/intel/kills/stream" + (q ? "?" + q : "");
  }

  // setPill shows the stream's state: "Live" while it is open,
  // "Reconnecting" while the browser retries, "Offline" once it
  // has given up (the server turned the stream away).
  function setPill(state) {
    var pill = document.querySelector("[data-live-pill]");
    if (!pill) return;
    pill.hidden = false;
    pill.setAttribute("data-state", state);
    pill.textContent = state === "live" ? "Live" : state === "retry" ? "Reconnecting" : "Offline";
  }

  function closeStream() {
    if (source) {
      source.close();
      source = null;
      streamURL = "";
    }
    if (watcher) {
      watcher.disconnect();
      watcher = null;
    }
  }

  function openStream() {
    var list = document.querySelector(".kb-list");
    if (!list || /[?&]before=/.test(location.search)) {
      closeStream();
      return;
    }
    markUnseen(list);
    var url = streamURLFor();
    if (source && streamURL === url) return;
    if (source) {
      source.close();
      source = null;
    }
    if (!("EventSource" in window)) return;
    streamURL = url;
    source = new EventSource(url);
    source.addEventListener("open", function () { setPill("live"); });
    source.addEventListener("error", function () {
      if (!source) return;
      setPill(source.readyState === EventSource.CLOSED ? "down" : "retry");
    });
    source.addEventListener("kill", function (e) {
      try {
        addKill(JSON.parse(e.data));
      } catch (err) {
        /* a bad frame is skipped */
      }
    });
    source.addEventListener("kill-price", function (e) {
      try {
        setKillPrice(JSON.parse(e.data));
      } catch (err) {
        /* a bad frame is skipped */
      }
    });
  }

  document.addEventListener("evesynapse:content-ready", openStream);
  document.addEventListener("evesynapse:content-will-swap", closeStream);
  document.addEventListener("visibilitychange", rearmAll);
  openStream();
})();
