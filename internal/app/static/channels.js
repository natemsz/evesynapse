/* channels.js -- an intel channel (channel.html) and the
 * cross-channel feed (feed.html).
 *
 * The feed is live: every 10 seconds while the page is in view it
 * asks feed.json for the reports after the newest it shows, and puts
 * them on top, built the same way the server builds a row. Ages
 * ("3 min ago") tick on their own. The grant form shows the name box
 * or the group list for the kind chosen. Runs at load and on every
 * page swap that brings a channel back.
 */
(function () {
  "use strict";

  function ago(unix) {
    var s = Math.max(0, Date.now() / 1000 - unix);
    if (s < 90) return "just now";
    var m = Math.round(s / 60);
    if (m < 60) return m + " min ago";
    var h = Math.round(m / 60);
    if (h < 48) return h + " h ago";
    return Math.round(h / 24) + " days ago";
  }

  function row(doc, r) {
    var li = doc.createElement("li");
    li.className = "ch-rep" + (r.clear ? " clear" : "");
    li.setAttribute("data-id", String(r.id));
    li.setAttribute("data-at", String(r.at));
    var sys = doc.createElement("span");
    sys.className = "ch-rep-sys";
    var sec = doc.createElement("span");
    sec.className = "sec sec-" + r.tier;
    sec.textContent = r.sec;
    var a = doc.createElement("a");
    a.href = "/map/?system=" + r.sys;
    a.textContent = r.sysName;
    sys.appendChild(sec);
    sys.appendChild(doc.createTextNode(" "));
    sys.appendChild(a);
    var what = doc.createElement("span");
    what.className = "ch-rep-what";
    function badge(cls, text) {
      var b = doc.createElement("span");
      b.className = "badge " + cls;
      b.textContent = text;
      what.appendChild(b);
      what.appendChild(doc.createTextNode(" "));
    }
    // What the report is, as the server worked it out, then the count.
    if (r.kindLabel) badge(r.tone || "loss", r.kindLabel);
    else if (r.clear) badge("kill", "Clear");
    if (r.n && !r.clear) badge("loss", String(r.n));
    if (r.text) what.appendChild(doc.createTextNode(r.text));
    if (r.scan) {
      var sa = doc.createElement("a");
      sa.href = "/intel/scan/" + encodeURIComponent(r.scan) + "/";
      sa.textContent = "scan";
      what.appendChild(doc.createTextNode(" "));
      what.appendChild(sa);
    }
    // The pilots (each opening their page), ships and gate read from it.
    if ((r.pilots && r.pilots.length) || (r.ships && r.ships.length) || r.gate) {
      var tags = doc.createElement("span");
      tags.className = "ch-rep-tags";
      (r.pilots || []).forEach(function (p) {
        var pa = doc.createElement("a");
        pa.className = "ch-tag" + (p.friendly ? " friendly" : "");
        pa.href = "/pilot/?character=" + p.id;
        if (p.friendly) pa.title = "Friendly";
        pa.textContent = p.name;
        tags.appendChild(pa);
      });
      (r.ships || []).forEach(function (s) {
        var label = s.name + (s.n > 1 ? " ×" + s.n : "");
        var sp;
        if (s.id) {
          sp = doc.createElement("a");
          sp.href = "/items/type/" + s.id + "/";
        } else {
          sp = doc.createElement("span");
        }
        sp.className = "ch-tag ship";
        sp.textContent = label;
        tags.appendChild(sp);
      });
      if (r.gate) {
        var g = doc.createElement("a");
        g.className = "ch-tag";
        g.href = "/map/?system=" + r.gateId;
        g.textContent = r.gate + " gate";
        tags.appendChild(g);
      }
      what.appendChild(tags);
    }
    // The standing split as a proportional bar, its counts in
    // the legend under it.
    if (r.split && r.split.length) {
      var strip = doc.createElement("span");
      strip.className = "ch-strip";
      strip.setAttribute("role", "img");
      strip.setAttribute("aria-label", "Standing split: " + r.split.map(function (s) { return s.name + " " + s.n; }).join(", "));
      var legend = doc.createElement("span");
      legend.className = "ch-strip-legend";
      r.split.forEach(function (s) {
        var seg = doc.createElement("span");
        seg.className = "ch-seg " + s.class;
        seg.setAttribute("data-n", String(s.n));
        seg.style.setProperty("--n", String(s.n));
        seg.title = s.name + " · " + s.n;
        strip.appendChild(seg);
        var item = doc.createElement("span");
        item.className = "ch-strip-item";
        var dot = doc.createElement("span");
        dot.className = "ch-dot " + s.class;
        item.appendChild(dot);
        item.appendChild(doc.createTextNode(s.name + " "));
        var strong = doc.createElement("strong");
        strong.textContent = String(s.n);
        item.appendChild(strong);
        var pct = doc.createElement("span");
        pct.className = "muted";
        pct.textContent = " " + s.pct + "%";
        item.appendChild(pct);
        legend.appendChild(item);
      });
      what.appendChild(strip);
      what.appendChild(legend);
    }
    // The report's doors: its system's kills and page.
    var doors = doc.createElement("span");
    doors.className = "ch-rep-doors";
    var kills = doc.createElement("a");
    kills.href = "/intel/kills/?system=" + r.sys;
    kills.textContent = "System kills";
    doors.appendChild(kills);
    doors.appendChild(doc.createTextNode(" · "));
    var sysPage = doc.createElement("a");
    sysPage.href = "/system/?system=" + r.sys;
    sysPage.textContent = "System page";
    doors.appendChild(sysPage);
    what.appendChild(doors);
    var meta = doc.createElement("span");
    meta.className = "ch-rep-meta";
    var t = doc.createElement("time");
    t.dateTime = new Date(r.at * 1000).toISOString();
    t.textContent = ago(r.at);
    meta.appendChild(doc.createTextNode(r.by));
    if (r.chn) {
      // The cross-channel feed names the channel each report is in.
      var ca = doc.createElement("a");
      ca.href = "/intel/channels/" + r.ch + "/";
      ca.textContent = r.chn;
      meta.appendChild(doc.createTextNode(" · "));
      meta.appendChild(ca);
    }
    meta.appendChild(doc.createTextNode(" · "));
    meta.appendChild(t);
    if (r.mine) {
      var f = doc.createElement("form");
      f.method = "post";
      f.action = "/intel/channels/" + r.ch + "/reports/" + r.id + "/delete";
      f.className = "inline-form";
      var x = doc.createElement("button");
      x.type = "submit";
      x.className = "ch-x";
      x.setAttribute("aria-label", "Remove this report");
      x.textContent = "×";
      f.appendChild(x);
      meta.appendChild(f);
    }
    li.appendChild(sys);
    li.appendChild(what);
    li.appendChild(meta);
    return li;
  }

  function init(root) {
    if (!root.querySelector) return;
    // Server-rendered bars: their widths ride on data-n, as the
    // scan strips' do.
    root.querySelectorAll(".ch-seg[data-n]").forEach(function (b) {
      b.style.setProperty("--n", String(Math.max(0, +b.getAttribute("data-n") || 0)));
    });
    var feed = root.querySelector("[data-ch-feed]");
    if (feed && !feed.getAttribute("data-bound")) {
      feed.setAttribute("data-bound", "1");
      bindFeed(root, feed);
    }
    var kind = root.querySelector("[data-ch-kind]");
    if (kind && !kind.getAttribute("data-bound")) {
      kind.setAttribute("data-bound", "1");
      var sync = function () {
        var g = kind.value === "group";
        var name = root.querySelector("[data-ch-name]"), group = root.querySelector("[data-ch-group]");
        if (name) { name.hidden = g; name.querySelector("input").disabled = g; }
        if (group) { group.hidden = !g; group.querySelector("select").disabled = !g; }
      };
      kind.addEventListener("change", sync);
      sync();
    }
  }

  function bindFeed(root, feed) {
    var doc = root.ownerDocument || root;
    var url = feed.getAttribute("data-ch-feed");
    var after = +feed.getAttribute("data-ch-after") || 0;
    var live = doc.querySelector("[data-ch-live]");
    // Only the first page is live; an older page stays as it is.
    if (/[?&]before=/.test(location.search)) return;
    if (live) live.hidden = false;
    feed.querySelectorAll("li[data-id] time").forEach(function (t) {
      t.parentNode.parentNode.setAttribute("data-at", String(Date.parse(t.dateTime) / 1000));
    });
    var polling = false;
    function poll() {
      if (polling || document.visibilityState !== "visible" || !feed.isConnected) return;
      polling = true;
      fetch(url + "?after=" + after, { credentials: "same-origin" }).then(function (r) {
        return r.ok ? r.json() : { reports: [] };
      }).then(function (d) {
        var list = (d && d.reports) || [];
        if (!list.length) return;
        var empty = feed.querySelector("[data-ch-empty]");
        if (empty) empty.remove();
        // Newest first from the server: insert oldest first on top.
        for (var i = list.length - 1; i >= 0; i--) {
          if (feed.querySelector('li[data-id="' + list[i].id + '"]')) {
            after = Math.max(after, list[i].id);
            continue // already on the page (an overlapping poll)
          }
          var li = row(doc, list[i]);
          li.classList.add("new");
          feed.insertBefore(li, feed.firstChild);
          after = Math.max(after, list[i].id);
        }
        // The live page holds one page of reports: what
        // prepends push past it leave from the bottom, so
        // the feed (and the tick below) does not grow with
        // the session.
        var rows = feed.querySelectorAll("li[data-id]");
        for (var j = 60; j < rows.length; j++) rows[j].remove();
      }).catch(function () { /* the next poll tries again */ })
        .then(function () { polling = false; });
    }
    function tick() {
      if (document.visibilityState !== "visible" || !feed.isConnected) return;
      feed.querySelectorAll("li[data-at]").forEach(function (li) {
        var t = li.querySelector("time");
        if (t) t.textContent = ago(+li.getAttribute("data-at"));
      });
    }
    var timer = setInterval(function () { poll(); tick(); }, 10000);
    // Back from another tab: catch up at once.
    function onVisible() { if (document.visibilityState === "visible") { poll(); tick(); } }
    doc.addEventListener("visibilitychange", onVisible);
    doc.addEventListener("evesynapse:content-will-swap", function stop() {
      clearInterval(timer);
      doc.removeEventListener("visibilitychange", onVisible);
      doc.removeEventListener("evesynapse:content-will-swap", stop);
    });
  }

  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", function () { init(document); });
  } else {
    init(document);
  }
  if (window.EveSynapseContent && window.EveSynapseContent.onReady) {
    window.EveSynapseContent.onReady(init);
  }
})();
