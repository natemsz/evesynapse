// The landing page's one script: the feature explorer's tab
// switcher. The default panel is rendered active in the HTML,
// so the section reads fine with JavaScript off; this only
// moves the active marker between tabs and panels on click.
(function () {
  var tabs = document.querySelectorAll(".ld-xtab");
  var panels = document.querySelectorAll(".ld-xpanel");
  if (!tabs.length || !panels.length) return;
  tabs.forEach(function (tab) {
    tab.addEventListener("click", function () {
      tabs.forEach(function (t) {
        var on = t === tab;
        t.classList.toggle("active", on);
        t.setAttribute("aria-selected", on ? "true" : "false");
      });
      panels.forEach(function (p) {
        p.classList.toggle("active", p.getAttribute("data-xpanel") === tab.getAttribute("data-xtab"));
      });
    });
  });
})();
