// Sov fights on the sovereignty tracker (sov_fights.go). A contest's
// Fight cell opens that contest's kills in a row under it, fetched
// from /intel/sov/fight/{id}/; without script the cell is a plain
// link to the fight's kills on the killboard. While the page shows a
// live contest, the killboard's stream (/intel/kills/stream?fights=1)
// sends each kill filed into a fight as it lands: the contest's count
// ticks, and an open panel takes the kill on top. The stream closes
// when a swap takes the page away.
(function () {
  if (typeof window === "undefined" || !window.esLive || window.esSovFights) return;
  window.esSovFights = true;
  var MAX = 20;
  var source = null;

  function esc(s) {
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
    });
  }

  function panelOf(id) {
    return document.getElementById("sov-fight-" + String(+id));
  }

  function rowHTML(k, side) {
    var who = k.structure ? k.corpName : k.victimName;
    if (k.allianceName) who += " · " + k.allianceName;
    var badge = side === 1 ? '<span class="badge loss">Defender lost</span>' : side === 2 ? '<span class="badge kill">Defender scored</span>' : "";
    return '<img src="https://images.evetech.net/types/' + (+k.shipType) + '/icon?size=64" alt="" width="32" height="32" loading="lazy">' +
      '<span class="sov-fk-main"><a href="/intel/kill/' + (+k.id) + '/">' + esc(k.ship) + '</a><span class="sov-dim">' + esc(who) + "</span></span>" +
      '<span class="sov-fk-where"><a href="/intel/kills/?system=' + (+k.system) + '">' + esc(k.systemName) + '</a><span class="sov-dim">' + (+k.attackers) + (k.attackers === 1 ? " attacker" : " attackers") + "</span></span>" +
      '<span class="sov-fk-side">' + badge + "</span>" +
      '<span class="sov-fk-v"><b data-kill-value>' + (k.value ? esc(k.value) : "pricing…") + '</b><time class="sov-dim" datetime="' + esc(k.time) + '">just now</time></span>';
  }

  // The panel row: made on the first open, then shown and hidden.
  function toggle(link) {
    var id = link.getAttribute("data-fight-open");
    var row = link.closest("tr");
    if (!id || !row) return;
    var panel = panelOf(id);
    var open = link.getAttribute("aria-expanded") !== "true";
    document.querySelectorAll('[data-fight-open="' + id + '"]').forEach(function (l) {
      l.setAttribute("aria-expanded", open ? "true" : "false");
    });
    if (panel) {
      panel.hidden = !open;
      return;
    }
    panel = document.createElement("tr");
    panel.id = "sov-fight-" + id;
    panel.className = "sov-fp-row";
    var cell = document.createElement("td");
    cell.colSpan = row.cells.length;
    cell.innerHTML = '<p class="sov-fp-sum sov-dim">Loading the kills…</p>';
    panel.appendChild(cell);
    row.parentNode.insertBefore(panel, row.nextSibling);
    fetch("/intel/sov/fight/" + encodeURIComponent(id) + "/", { credentials: "same-origin" })
      .then(function (r) { if (!r.ok) throw new Error(String(r.status)); return r.text(); })
      .then(function (html) { cell.innerHTML = html; })
      .catch(function () {
        cell.innerHTML = '<p class="sov-fp-sum">The kills could not be loaded. <a href="' + esc(link.getAttribute("href")) + '">See them on the killboard</a>.</p>';
      });
  }

  // Capture phase: the page swapper's click handler, registered
  // first, would otherwise navigate before this one runs; it skips a
  // click this one has handled.
  document.addEventListener("click", function (ev) {
    var t = ev.target && ev.target.closest ? ev.target : null;
    if (!t) return;
    var link = t.closest("[data-fight-open]");
    if (link && !(ev.metaKey || ev.ctrlKey || ev.shiftKey || ev.button > 0)) {
      ev.preventDefault();
      toggle(link);
      return;
    }
    // A sort reorders the rows: open panels close rather than
    // stay behind under the wrong contest.
    var th = t.closest(".sov-t thead th");
    if (th) {
      th.closest("table").querySelectorAll(".sov-fp-row").forEach(function (p) { p.remove(); });
      th.closest("table").querySelectorAll('[data-fight-open][aria-expanded="true"]').forEach(function (l) {
        l.setAttribute("aria-expanded", "false");
      });
    }
  }, true);

  function bump(id) {
    document.querySelectorAll('[data-fight-open="' + String(+id) + '"]').forEach(function (l) {
      var n = l.querySelector("[data-fight-kills]");
      if (!n) return;
      var count = (+n.textContent || 0) + 1;
      n.textContent = String(count);
      var word = l.querySelector("[data-fight-word]");
      if (word) word.textContent = count === 1 ? "kill" : "kills";
      window.esLive.flash(n);
    });
  }

  function onKill(ev) {
    if (!ev || !ev.kill) return;
    (ev.fights || []).forEach(function (f) {
      bump(f.campaign);
      var panel = panelOf(f.campaign);
      var list = panel && panel.querySelector("[data-sov-fk-list]");
      if (!list || list.querySelector('[data-kill-id="' + String(+ev.kill.id) + '"]')) return;
      var li = document.createElement("li");
      li.className = "sov-fk" + (ev.kill.big ? " big" : "");
      li.setAttribute("data-kill-id", String(+ev.kill.id));
      li.innerHTML = rowHTML(ev.kill, f.side);
      list.insertBefore(li, list.firstChild);
      while (list.children.length > MAX) list.removeChild(list.lastElementChild);
      window.esLive.mark(li);
    });
  }

  function onPrice(ev) {
    if (!ev) return;
    document.querySelectorAll('[data-sov-fk-list] [data-kill-id="' + String(+ev.id) + '"]').forEach(function (li) {
      var v = li.querySelector("[data-kill-value]");
      if (v && ev.value) v.textContent = ev.value;
      li.classList.toggle("big", !!ev.big);
    });
  }

  function close() {
    if (source) {
      source.close();
      source = null;
    }
  }

  function open() {
    var host = document.querySelector("[data-sov-fights]");
    if (!host || !host.hasAttribute("data-live")) {
      close();
      return;
    }
    if (source) return;
    source = window.esLive.stream("/intel/kills/stream?fights=1", host.querySelector("[data-live-pill]"), {
      "fight-kill": onKill,
      "kill-price": onPrice
    });
  }

  document.addEventListener("evesynapse:content-ready", open);
  document.addEventListener("evesynapse:content-will-swap", close);
  open();
})();
