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
  // A saved colour scheme (Settings, Interface tab) replaces the
  // backdrop choice: the scheme carries its own strength, so the
  // backdrop attribute comes off and the scheme's tier applies.
  var savedScheme = window.localStorage.getItem("evesynapse-scheme");
  if (savedScheme === "jove" || savedScheme === "caldari" || savedScheme === "gallente" ||
      savedScheme === "amarr" || savedScheme === "triglavian" || savedScheme === "edencom") {
    document.documentElement.setAttribute("data-scheme", savedScheme);
    document.documentElement.removeAttribute("data-backdrop");
    var savedTier = window.localStorage.getItem("evesynapse-tier");
    document.documentElement.setAttribute("data-tier",
      savedTier === "minimalistic" ? "minimalistic" : "muted");
  }
} catch (e) {}
