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
  }
  var savedTheme = window.localStorage.getItem("evesynapse-theme");
  if (savedTheme === "light" || savedTheme === "dark") {
    document.documentElement.setAttribute("data-theme", savedTheme);
  }
} catch (e) {}

// The wordmark font (VT323) loads asynchronously: a
// render-blocking stylesheet link for one heading font is not
// worth delaying first paint, and font-display=swap swaps it in
// when it lands. The preconnects in base.html cover the fetch.
try {
  var vt323 = document.createElement("link");
  vt323.rel = "stylesheet";
  vt323.href = "https://fonts.googleapis.com/css2?family=VT323&display=swap";
  document.head.appendChild(vt323);
} catch (e) {}
