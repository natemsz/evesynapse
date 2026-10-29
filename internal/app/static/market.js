// Market browser: the category tree lives in the page and never
// re-renders. Opening a category fetches just its children from
// /market/browse-children and unfolds them in place with a short
// height animation; picking a category, paging, sorting and
// filtering swap only the item list from /market/browse-fragment.
// The address bar follows picks, so a view is shareable; Back is
// left to the page swap (swap.js), which restores the full page
// for the URL. Listeners sit on the document and bind once, so
// they serve the list however often it is swapped in.
(function () {
  "use strict";
  if (window.esMarketBrowse) return;
  window.esMarketBrowse = true;

  var pending = null;

  function markChosen(groupID) {
    var root = document.querySelector("[data-browse-root]");
    if (!root) return;
    var prev = root.querySelectorAll(".mb-node.chosen");
    for (var i = 0; i < prev.length; i++) {
      prev[i].classList.remove("chosen");
      var a = prev[i].querySelector("a[data-browse-link]");
      if (a) a.removeAttribute("aria-current");
    }
    if (!groupID) return;
    var li = root.querySelector('.mb-node[data-group="' + groupID + '"]');
    if (li) {
      li.classList.add("chosen");
      var link = li.querySelector("a[data-browse-link]");
      if (link) link.setAttribute("aria-current", "true");
    }
  }

  function loadItems(href, push) {
    var url = new URL(href, location.href);
    var box = document.querySelector("[data-browse-items]");
    if (!box || url.pathname !== "/market/" || url.searchParams.has("type")) {
      location.assign(url.href);
      return;
    }
    if (pending) pending.abort();
    pending = new AbortController();
    box.setAttribute("aria-busy", "true");
    fetch("/market/browse-fragment" + url.search, { signal: pending.signal, credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) throw new Error("status " + r.status);
        return r.text();
      })
      .then(function (html) {
        var active = document.activeElement;
        var field = active && box.contains(active) && active.name ? active.name : "";
        box.innerHTML = html;
        if (field) {
          var again = box.querySelector('form[data-browse-form] [name="' + field + '"]');
          if (again) again.focus({ preventScroll: true });
        }
        markChosen(url.searchParams.get("group"));
        syncChrome(url);
        if (push === "replace") history.replaceState({ esSwap: 1, scroll: window.scrollY }, "", url.pathname + url.search);
        else if (push) history.pushState({ esSwap: 1, scroll: window.scrollY }, "", url.pathname + url.search);
      })
      .catch(function (err) {
        if (err && err.name === "AbortError") return;
        location.assign(url.href);
      })
      .then(function () { box.removeAttribute("aria-busy"); });
  }

  // syncChrome mirrors the list's state into the static row
  // above the browser (the filter form's hidden fields and
  // volume box, the search box), which never re-renders.
  function setHidden(form, name, value) {
    var el = form.querySelector('input[type="hidden"][name="' + name + '"]');
    if (!value) {
      if (el) el.remove();
      return;
    }
    if (!el) {
      el = document.createElement("input");
      el.type = "hidden";
      el.name = name;
      form.appendChild(el);
    }
    el.value = value;
  }

  function syncChrome(url) {
    var form = document.querySelector("form[data-browse-form]");
    if (form) {
      setHidden(form, "group", url.searchParams.get("group"));
      setHidden(form, "region", url.searchParams.get("region"));
      setHidden(form, "sort", url.searchParams.get("sort"));
      setHidden(form, "bq", url.searchParams.get("bq"));
      var mv = form.querySelector('[name="minvol"]');
      if (mv && document.activeElement !== mv) mv.value = url.searchParams.get("minvol") || "";
    }
    var q = document.querySelector("#market-q");
    if (q && document.activeElement !== q) q.value = url.searchParams.get("bq") || "";
    var reg = document.querySelector("#market-region");
    var region = url.searchParams.get("region");
    if (reg && region) reg.value = region;
  }

  // slide unfolds or folds one tree level: a 130ms height
  // animation, nothing else on the page moves.
  function slide(ul, opening, li) {
    var btn = li.querySelector(":scope > .mb-row [data-browse-toggle]");
    if (btn) btn.setAttribute("aria-expanded", opening ? "true" : "false");
    if (opening) li.classList.add("open");
    if (!ul.animate) {
      ul.hidden = !opening;
      if (!opening) li.classList.remove("open");
      return;
    }
    if (opening) ul.hidden = false;
    var from = opening ? "0px" : ul.scrollHeight + "px";
    var to = opening ? ul.scrollHeight + "px" : "0px";
    var anim = ul.animate([{ height: from }, { height: to }], { duration: 130, easing: "ease-out" });
    anim.finished.then(function () {
      if (!opening) {
        ul.hidden = true;
        li.classList.remove("open");
      }
    }, function () {});
  }

  function toggleNode(btn) {
    var li = btn.closest(".mb-node");
    if (!li) return;
    var ul = li.querySelector(":scope > ul.mb-children");
    if (li.classList.contains("open")) {
      if (ul) slide(ul, false, li);
      else {
        li.classList.remove("open");
        btn.setAttribute("aria-expanded", "false");
      }
      return;
    }
    if (ul) {
      slide(ul, true, li);
      return;
    }
    var link = li.querySelector("a[data-browse-link]");
    var region = "";
    if (link) region = new URL(link.href).searchParams.get("region") || "";
    btn.disabled = true;
    fetch("/market/browse-children?group=" + encodeURIComponent(li.getAttribute("data-group")) + "&region=" + encodeURIComponent(region), { credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) throw new Error("status " + r.status);
        return r.text();
      })
      .then(function (html) {
        var holder = document.createElement("div");
        holder.innerHTML = html;
        var fresh = holder.querySelector("ul.mb-children");
        if (!fresh) return;
        fresh.hidden = true;
        li.appendChild(fresh);
        slide(fresh, true, li);
      })
      .catch(function () { /* the level simply stays closed */ })
      .then(function () { btn.disabled = false; });
  }

  document.addEventListener("click", function (e) {
    var btn = e.target.closest("[data-browse-toggle]");
    if (btn) {
      e.preventDefault();
      toggleNode(btn);
      return;
    }
    var a = e.target.closest("a[data-browse-link]");
    if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    e.stopPropagation();
    var li = a.closest(".mb-node");
    if (li && !li.classList.contains("open")) {
      var tgl = li.querySelector(":scope > .mb-row [data-browse-toggle]");
      if (tgl) toggleNode(tgl);
    }
    loadItems(a.href, true);
  }, true);

  document.addEventListener("submit", function (e) {
    var sf = e.target.closest("#market-search");
    if (sf) {
      // On the browser page the search filters the list in
      // place, keeping the category, sort and volume state;
      // anywhere else the form submits as it always has.
      if (!document.querySelector("[data-browse-items]")) return;
      e.preventDefault();
      e.stopPropagation();
      var params = new URLSearchParams(location.search);
      var input = sf.querySelector('[name="q"]');
      var q = input ? input.value.trim() : "";
      if (q) params.set("bq", q);
      else params.delete("bq");
      params.delete("q");
      params.delete("page");
      loadItems("/market/?" + params.toString(), true);
      return;
    }
    var form = e.target.closest("form[data-browse-form]");
    if (!form) return;
    e.preventDefault();
    e.stopPropagation();
    var params = new URLSearchParams(new FormData(form));
    params.forEach(function (v, k) { if (v === "") params.delete(k); });
    var mode = form.dataset.live === "1" ? "replace" : true;
    delete form.dataset.live;
    loadItems("/market/?" + params.toString(), mode);
  }, true);

  // The volume field applies after a short pause in typing.
  var typing = null;
  document.addEventListener("input", function (e) {
    var form = e.target.closest("form[data-browse-form]");
    if (!form || e.target.name !== "minvol") return;
    clearTimeout(typing);
    typing = setTimeout(function () { form.dataset.live = "1"; form.requestSubmit(); }, 350);
  });
})();
