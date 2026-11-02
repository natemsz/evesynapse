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
