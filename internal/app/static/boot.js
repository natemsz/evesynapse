// EveSynapse head bootstrap: a plain <script> in <head>, not
// deferred, so it runs before first paint and a saved nav state or
// theme applies without flashing the default first. Keep it
// dependency-free and tiny: every byte here delays first paint.
try {
  document.documentElement.classList.add("js");
  var savedNav = window.localStorage.getItem("evesynapse-nav");
  if (savedNav === "rail" || savedNav === "hidden" || savedNav === "expanded") {
    document.documentElement.setAttribute("data-nav", savedNav);
    // The server renders the saved layout from a cookie (ui_navstate.go),
    // so the page arrives right even when this script runs late.
    // Readers whose choice predates the cookie get it written here.
    var navCookie = document.cookie.match(/(?:^|; )evesynapse-nav=([^;]*)/);
    if (!navCookie || navCookie[1] !== savedNav) {
      document.cookie = "evesynapse-nav=" + savedNav + "; path=/; max-age=31536000; samesite=lax" +
        (window.location.protocol === "https:" ? "; secure" : "");
    }
  } else if (!document.documentElement.hasAttribute("data-nav")) {
    // No saved preference anywhere and the server rendered no
    // layout: the default is the icon rail, applied here so it
    // still paints right on shell-less responses.
    document.documentElement.setAttribute("data-nav", "rail");
  }
  var savedTheme = window.localStorage.getItem("evesynapse-theme");
  if (savedTheme === "light" || savedTheme === "dark") {
    document.documentElement.setAttribute("data-theme", savedTheme);
  }
  var savedBackdrop = window.localStorage.getItem("evesynapse-backdrop");
  if (savedBackdrop === "muted" || savedBackdrop === "glow") {
    document.documentElement.setAttribute("data-backdrop", savedBackdrop);
  }
} catch (e) {}
