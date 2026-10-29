// Market baskets: the list page builds and edits baskets in
// place. The New basket button gives its spot to a builder that
// asks for the basket's name first; then a suggest search picks
// the items, which collect in a list with icons and typical
// prices, and Save posts the named basket into the table.
// Each basket row's pencil opens the same editor under the row,
// seeded with the basket's items, each removable, with the
// search on hand to add more. Both editors keep the classic
// type-the-names form behind a small text link.
(function () {
  "use strict";
  if (window.esBaskets) return;
  window.esBaskets = true;

  function iconURL(id) {
    return "https://images.evetech.net/types/" + id + "/icon?size=32";
  }

  function showErr(el, msg) {
    if (!el) return;
    el.textContent = msg || "";
    el.hidden = !msg;
  }

  // wireEditor binds one editor root: search input (.mbk-search),
  // suggestion list (.mbk-suggest), picked list (.mbk-items) and
  // error line (.mbk-err). region() reports the region prices
  // are read in. Returns the picked items and a reset.
  function wireEditor(root, region) {
    var search = root.querySelector(".mbk-search");
    var suggest = root.querySelector(".mbk-suggest");
    var list = root.querySelector(".mbk-items");
    var err = root.querySelector(".mbk-err");
    var items = [];
    var debounce = null;
    var seq = 0;

    function render() {
      list.innerHTML = "";
      if (!items.length) {
        var hint = document.createElement("li");
        hint.className = "mbk-empty";
        hint.innerHTML = "<small>No items yet. Search above to add.</small>";
        list.appendChild(hint);
        return;
      }
      items.forEach(function (it) {
        var li = document.createElement("li");
        li.className = "mbk-item";
        var img = document.createElement("img");
        img.src = iconURL(it.id);
        img.alt = "";
        img.width = 32;
        img.height = 32;
        var name = document.createElement("span");
        name.className = "mbk-item-name";
        name.textContent = it.name + (it.qty > 1 ? " ×" + it.qty.toLocaleString("en-US") : "");
        var price = document.createElement("span");
        price.className = "mbk-item-price";
        price.textContent = it.price ? it.price + " ISK" : "—";
        var x = document.createElement("button");
        x.type = "button";
        x.className = "mbk-remove";
        x.setAttribute("aria-label", "Remove " + it.name);
        x.textContent = "×";
        x.addEventListener("click", function () {
          items = items.filter(function (o) { return o.id !== it.id; });
          render();
        });
        li.appendChild(img);
        li.appendChild(name);
        li.appendChild(price);
        li.appendChild(x);
        list.appendChild(li);
      });
    }

    function fetchPrices() {
      if (!items.length) return;
      var ids = items.map(function (it) { return it.id; }).join(",");
      fetch("/market/baskets/prices?region=" + encodeURIComponent(region()) + "&types=" + ids, { credentials: "same-origin" })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (data) {
          if (!data || !data.prices) return;
          var by = {};
          data.prices.forEach(function (p) { by[p.id] = p.price; });
          items.forEach(function (it) { if (by[it.id] !== undefined) it.price = by[it.id]; });
          render();
        })
        .catch(function () { /* prices stay blank; the basket still saves */ });
    }

    function addItem(id, name) {
      for (var i = 0; i < items.length; i++) {
        if (items[i].id === id) {
          items[i].qty += 1;
          render();
          return;
        }
      }
      if (items.length >= 200) {
        showErr(err, "A basket holds at most 200 items.");
        return;
      }
      items.push({ id: id, name: name, qty: 1, price: "" });
      showErr(err, "");
      render();
      fetchPrices();
    }

    // addGroup bulk-adds a market group's whole subtree (the
    // suggest feed's group rows). Types already in the basket
    // are skipped, never incremented. A group whose new items
    // outnumber the room left adds nothing: a partial bulk add
    // would be an arbitrary slice of the group.
    function addGroup(id, name) {
      fetch("/market/baskets/group-types?id=" + encodeURIComponent(id), { credentials: "same-origin" })
        .then(function (r) { return r.ok ? r.json() : null; })
        .then(function (data) {
          if (!data || !data.items) {
            showErr(err, "That group could not be loaded.");
            return;
          }
          var have = {};
          items.forEach(function (it) { have[it.id] = true; });
          var fresh = [];
          var skipped = 0;
          data.items.forEach(function (row) {
            if (have[row.id]) skipped++;
            else fresh.push(row);
          });
          // Past the fetched rows the tail is all new items.
          var totalNew = fresh.length + Math.max(0, (data.count || 0) - data.items.length);
          var room = 200 - items.length;
          if (totalNew > room) {
            showErr(err, name + " adds " + totalNew.toLocaleString("en-US") + " new items, but only " + room + " fit. A basket holds at most 200 items.");
            return;
          }
          fresh.forEach(function (row) {
            items.push({ id: row.id, name: row.name, qty: 1, price: "" });
          });
          showErr(err, skipped ? "Added " + fresh.length + " items from " + name + "; " + skipped + " already in the basket." : "");
          render();
          fetchPrices();
        })
        .catch(function () { showErr(err, "That group could not be loaded."); });
    }

    function hideSuggest() {
      suggest.hidden = true;
      suggest.innerHTML = "";
      search.setAttribute("aria-expanded", "false");
    }

    search.addEventListener("input", function () {
      clearTimeout(debounce);
      var q = search.value.trim();
      if (q.length < 2) {
        hideSuggest();
        return;
      }
      debounce = setTimeout(function () {
        var mine = ++seq;
        fetch("/market/baskets/suggest?q=" + encodeURIComponent(q), { credentials: "same-origin" })
          .then(function (r) { return r.ok ? r.json() : []; })
          .then(function (rows) {
            if (mine !== seq) return;
            suggest.innerHTML = "";
            (rows || []).forEach(function (row) {
              var li = document.createElement("li");
              var btn = document.createElement("button");
              btn.type = "button";
              btn.className = "mbk-suggest-pick";
              btn.textContent = row.name;
              if (row.label) {
                var small = document.createElement("small");
                small.textContent = " " + row.label;
                btn.appendChild(small);
              }
              btn.addEventListener("click", function () {
                if (row.kind === "group") addGroup(row.id, row.name);
                else addItem(row.id, row.name);
                search.value = "";
                hideSuggest();
                search.focus();
              });
              li.appendChild(btn);
              suggest.appendChild(li);
            });
            suggest.hidden = !suggest.children.length;
            search.setAttribute("aria-expanded", suggest.hidden ? "false" : "true");
          })
          .catch(function () { hideSuggest(); });
      }, 200);
    });
    search.addEventListener("keydown", function (e) {
      if (e.key === "Escape") hideSuggest();
    });
    document.addEventListener("click", function (e) {
      if (!root.contains(e.target)) hideSuggest();
    });

    return {
      items: function () {
        return items.map(function (it) { return { id: it.id, qty: it.qty }; });
      },
      seed: function (seedItems) {
        items = seedItems.slice();
        render();
      },
      reset: function () {
        items = [];
        search.value = "";
        hideSuggest();
        showErr(err, "");
        render();
      },
      refreshPrices: fetchPrices,
      error: function (msg) { showErr(err, msg); },
      focus: function () { search.focus(); }
    };
  }

  function postJSON(url, body) {
    return fetch(url, {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    }).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (data) {
        if (!r.ok) throw new Error(data.error || "That didn't go through. Try again.");
        return data;
      });
    });
  }

  function pencilCell(id) {
    var td = document.createElement("td");
    td.className = "mt-num";
    td.innerHTML = '<button type="button" class="mbk-edit-btn" data-mbk-edit="' + id + '" aria-label="Edit basket" title="Edit basket"><svg class="glyph" width="16" height="16" viewBox="0 0 24 24" aria-hidden="true"><g fill="none" stroke="url(#nav-glyph-gradient)" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M17 3a2.8 2.8 0 1 1 4 4L8 20l-5 1 1-5Z"/><path d="M15 5l4 4"/></g></svg></button>';
    return td;
  }

  // --- wiring -------------------------------------------------
  // A swapped-in page never re-runs this script, so the wiring
  // runs once here and again whenever the shell announces new
  // content. The wired-element guards keep a second pass over
  // the same DOM from binding anything twice.
  var wiredBuilder = null;
  var wiredTbody = null;
  function init() {
    // --- the create builder -----------------------------------
    var newBtn = document.getElementById("mbk-new");
    var builder = document.getElementById("mbk-builder");
    if (newBtn && builder && builder !== wiredBuilder) {
      wiredBuilder = builder;
          var regionSel = document.getElementById("mbk-region");
          var editor = wireEditor(builder, function () { return regionSel.value; });
          var pickRow = builder.querySelector(".mbk-pickrow");
          var textlinkRow = builder.querySelector(".mbk-textlink-row");
          var itemsList = document.getElementById("mbk-items");
          var actions = document.getElementById("mbk-actions");
          var naming = document.getElementById("mbk-naming");
          var nameInput = document.getElementById("mbk-name");
          var nameErr = document.getElementById("mbk-name-err");
          var textform = document.getElementById("mbk-textform");

          function setNaming(on) {
            pickRow.hidden = on;
            textlinkRow.hidden = on;
            itemsList.hidden = on;
            actions.hidden = on;
            textform.hidden = true;
            naming.hidden = !on;
            if (on) nameInput.focus();
          }
          function closeBuilder() {
            builder.hidden = true;
            newBtn.hidden = false;
            editor.reset();
            setNaming(true);
            showErr(nameErr, "");
            nameInput.value = "";
          }
          newBtn.addEventListener("click", function () {
            builder.hidden = false;
            newBtn.hidden = true;
            setNaming(true);
          });
          document.getElementById("mbk-name-cancel").addEventListener("click", closeBuilder);
          document.getElementById("mbk-back").addEventListener("click", function () { setNaming(true); });
          document.getElementById("mbk-to-items").addEventListener("click", function () {
            if (!nameInput.value.trim()) {
              showErr(nameErr, "Give the basket a name.");
              nameInput.focus();
              return;
            }
            showErr(nameErr, "");
            setNaming(false);
            editor.focus();
          });
          document.getElementById("mbk-textlink").addEventListener("click", function (e) {
            e.preventDefault();
            textform.hidden = !textform.hidden;
          });
          regionSel.addEventListener("change", function () { editor.refreshPrices(); });
          document.getElementById("mbk-save").addEventListener("click", function () {
            var name = nameInput.value.trim();
            if (!name) {
              setNaming(true);
              showErr(nameErr, "Give the basket a name.");
              return;
            }
            if (!editor.items().length) {
              editor.error("Add at least one item first.");
              return;
            }
            editor.error("");
            postJSON("/market/baskets/api", { name: name, region: parseInt(regionSel.value, 10), items: editor.items() })
              .then(function (data) {
                var tbody = document.getElementById("mbk-tbody");
                var tr = document.createElement("tr");
                tr.setAttribute("data-mbk-row", data.id);
                var nameTd = document.createElement("td");
                var a = document.createElement("a");
                a.href = "/market/baskets/" + data.id + "/";
                a.textContent = data.name;
                nameTd.appendChild(a);
                var regionTd = document.createElement("td");
                regionTd.textContent = data.region_name;
                var countTd = document.createElement("td");
                countTd.className = "mt-num";
                countTd.setAttribute("data-mbk-count", "");
                countTd.textContent = data.items;
                var worthTd = document.createElement("td");
                worthTd.className = "mt-num";
                worthTd.textContent = "—";
                var yoursTd = document.createElement("td");
                yoursTd.innerHTML = "<small>Yours</small>";
                tr.appendChild(nameTd);
                tr.appendChild(regionTd);
                tr.appendChild(countTd);
                tr.appendChild(worthTd);
                tr.appendChild(yoursTd);
                tr.appendChild(pencilCell(data.id));
                tbody.appendChild(tr);
                closeBuilder();
              })
              .catch(function (err) { editor.error(err.message); });
          });
    }

    // --- the per-row editor -----------------------------------
    var tbody = document.getElementById("mbk-tbody");
    var template = document.getElementById("mbk-editor-template");
    if (tbody && template && tbody !== wiredTbody) {
      wiredTbody = tbody;
          tbody.addEventListener("click", function (e) {
            var btn = e.target.closest("[data-mbk-edit]");
            if (!btn) return;
            var id = parseInt(btn.getAttribute("data-mbk-edit"), 10);
            if (!id) return;
            var row = tbody.querySelector('tr[data-mbk-row="' + id + '"]');
            if (!row) return;
            var open = tbody.querySelector('tr[data-mbk-editor-for="' + id + '"]');
            if (open) {
              open.remove();
              return;
            }
            fetch("/market/baskets/" + id + "/items.json", { credentials: "same-origin" })
              .then(function (r) { return r.ok ? r.json() : null; })
              .then(function (data) {
                if (!data) return;
                var er = document.createElement("tr");
                er.className = "mbk-editor-row";
                er.setAttribute("data-mbk-editor-for", id);
                var td = document.createElement("td");
                td.colSpan = 6;
                td.appendChild(template.content.cloneNode(true));
                er.appendChild(td);
                row.after(er);
                var root = er.querySelector(".mbk-editor");
                var ed = wireEditor(root, function () { return data.region; });
                ed.seed(data.items.map(function (it) {
                  return { id: it.id, name: it.name, qty: it.qty, price: it.price || "" };
                }));
                root.querySelector(".mbk-cancel").addEventListener("click", function () { er.remove(); });
                root.querySelector(".mbk-save").addEventListener("click", function () {
                  postJSON("/market/baskets/" + id + "/api-edit", { items: ed.items() })
                    .then(function (res) {
                      var count = row.querySelector("[data-mbk-count]");
                      if (count) count.textContent = res.items;
                      er.remove();
                    })
                    .catch(function (err) { ed.error(err.message); });
                });
                var textlink = root.querySelector(".mbk-textlink");
                var textform = root.querySelector(".mbk-textform");
                textform.action = "/market/baskets/" + id + "/edit";
                textlink.addEventListener("click", function (ev) {
                  ev.preventDefault();
                  if (textform.hidden) {
                    textform.querySelector(".mbk-tf-name").value = data.name;
                    textform.querySelector(".mbk-tf-region").value = data.region;
                    textform.querySelector(".mbk-tf-items").value = data.items.map(function (it) {
                      return it.qty > 1 ? it.name + " " + it.qty : it.name;
                    }).join("\n");
                  }
                  textform.hidden = !textform.hidden;
                });
              })
              .catch(function () { /* the editor simply does not open */ });
          });
    }
  }
  init();
  document.addEventListener("evesynapse:content-ready", init);
})();
