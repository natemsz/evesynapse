// Market browser: category clicks, paging and the filter form swap
// the browser panel from /market/browse-fragment instead of loading
// a new page, and the address bar follows so Back and shared links
// land on the same view (swap.js restores those entries on Back). Listeners sit on the document and bind
// once, so they serve the panel however often it, or the page
// around it, is swapped in.
(function () {
  "use strict";
  if (window.esMarketBrowse) return;
  window.esMarketBrowse = true;

  var pending = null;

  function load(href, push) {
    var url = new URL(href, location.href);
    var panel = document.querySelector("[data-browse-root]");
    if (!panel || url.pathname !== "/market/" || url.searchParams.has("type")) {
      location.assign(url.href);
      return;
    }
    if (pending) pending.abort();
    pending = new AbortController();
    panel.setAttribute("aria-busy", "true");
    fetch("/market/browse-fragment" + url.search, { signal: pending.signal, credentials: "same-origin" })
      .then(function (r) {
        if (!r.ok) throw new Error("status " + r.status);
        return r.text();
      })
      .then(function (html) {
        var holder = document.createElement("div");
        holder.innerHTML = html;
        var fresh = holder.querySelector("[data-browse-root]");
        if (!fresh) throw new Error("no panel");
        // A filter field being typed in keeps its focus and caret
        // across the swap.
        var active = document.activeElement;
        var field = active && panel.contains(active) && active.name ? active.name : "";
        var caret = field && typeof active.selectionStart === "number" ? active.selectionStart : null;
        panel.replaceWith(fresh);
        if (field) {
          var again = fresh.querySelector('form[data-browse-form] [name="' + field + '"]');
          if (again) {
            again.focus({ preventScroll: true });
            if (caret !== null && typeof again.setSelectionRange === "function") {
              try { again.setSelectionRange(caret, caret); } catch (err) { /* number fields have no caret */ }
            }
          }
        }
        if (push === "replace") history.replaceState({ esSwap: 1, scroll: window.scrollY }, "", url.pathname + url.search);
        else if (push) history.pushState({ esSwap: 1, scroll: window.scrollY }, "", url.pathname + url.search);
        var chosen = fresh.querySelector(".mb-node.chosen a");
        if (chosen && push && !field) chosen.focus({ preventScroll: true });
      })
      .catch(function (err) {
        if (err && err.name === "AbortError") return;
        location.assign(url.href);
      });
  }

  document.addEventListener("click", function (e) {
    var a = e.target.closest("a[data-browse-link]");
    if (!a || e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    e.stopPropagation();
    load(a.href, true);
  }, true);

  document.addEventListener("submit", function (e) {
    var form = e.target.closest("form[data-browse-form]");
    if (!form) return;
    e.preventDefault();
    e.stopPropagation();
    var params = new URLSearchParams(new FormData(form));
    params.forEach(function (v, k) { if (v === "") params.delete(k); });
    var mode = form.dataset.live === "1" ? "replace" : true;
    delete form.dataset.live;
    load("/market/?" + params.toString(), mode);
  }, true);

  // The sort and volume fields apply as soon as they change; the
  // name filter waits for a short pause in typing.
  var typing = null;
  document.addEventListener("input", function (e) {
    var form = e.target.closest("form[data-browse-form]");
    if (!form) return;
    clearTimeout(typing);
    var wait = e.target.name === "bq" ? 350 : 0;
    typing = setTimeout(function () { form.dataset.live = "1"; form.requestSubmit(); }, wait);
  });
})();
