// Overview directory filter: typing in the directory search
// hides directory entries whose group name does not match, the
// same client-side filter the Assets matrix uses. Directory
// entries are links, so a filter that hides one never loses it:
// clearing the box brings every group back.
(function () {
  "use strict";
  var input = document.querySelector("[data-ov-filter]");
  if (!input) {
    return;
  }
  var items = document.querySelectorAll("[data-ov-dirlist] .diritem");
  input.addEventListener("input", function () {
    var q = input.value.trim().toLowerCase();
    items.forEach(function (el) {
      var name = (el.getAttribute("data-name") || "").toLowerCase();
      el.style.display = !q || name.indexOf(q) !== -1 ? "" : "none";
    });
  });
})();

// Category jumps: following a directory entry swaps the sheet in
// place and keeps the window where it was, so a category picked
// from the bottom of the directory lands mid-sheet. A directory
// click marks the jump; once the fresh sheet is in place, the
// window returns to the top of the sheet pane.
(function () {
  "use strict";
  var JUMP_KEY = "ov-cat-jump";
  document.addEventListener("click", function (ev) {
    var link = ev.target && ev.target.closest
      ? ev.target.closest("[data-ov-dirlist] .diritem")
      : null;
    if (link) {
      try { sessionStorage.setItem(JUMP_KEY, "1"); } catch (e) {}
    }
  });
  function jumpToSheetTop() {
    var marked = false;
    try {
      marked = sessionStorage.getItem(JUMP_KEY) === "1";
      if (marked) sessionStorage.removeItem(JUMP_KEY);
    } catch (e) {}
    if (!marked) return;
    var main = document.querySelector(".ov-main");
    if (main) main.scrollIntoView({ block: "start", behavior: "auto" });
  }
  document.addEventListener("evesynapse:content-ready", jumpToSheetTop);
  jumpToSheetTop();
})();
