// EveSynapse head bootstrap: a plain <script> in <head>, not
// deferred, so it runs before first paint and a saved nav state or
// theme applies without flashing the default first. (It used to be
// an inline block; the content security policy no longer allows
// those.) Keep it dependency-free and tiny: every byte here delays
// first paint.
try {
  document.documentElement.classList.add("js");
  var savedNav = window.localStorage.getItem("evesynapse-nav");
  if (savedNav === "rail" || savedNav === "hidden" || savedNav === "expanded") {
    document.documentElement.setAttribute("data-nav", savedNav);
    // The server renders the saved layout from a cookie (navstate.go),
    // so the page arrives right even when this script runs late.
    // Readers whose choice predates the cookie get it written here.
    var navCookie = document.cookie.match(/(?:^|; )evesynapse-nav=([^;]*)/);
    if (!navCookie || navCookie[1] !== savedNav) {
      document.cookie = "evesynapse-nav=" + savedNav + "; path=/; max-age=31536000; samesite=lax" +
        (window.location.protocol === "https:" ? "; secure" : "");
    }
  }
  var savedTheme = window.localStorage.getItem("evesynapse-theme");
  if (savedTheme === "light" || savedTheme === "dark") {
    document.documentElement.setAttribute("data-theme", savedTheme);
  }
} catch (e) {}
