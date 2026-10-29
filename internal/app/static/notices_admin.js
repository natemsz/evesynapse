// The Admin page's Notices tab (notices.go): the detail post's live
// preview. The preview is never rendered here: the draft goes to
// /admin/notices/preview, which runs the same Markdown renderer and
// sanitizer the save does, and only its answer is shown. Wired on
// load and on every content-ready; the form-identity guard keeps a
// repeat call from binding twice.
(function () {
  "use strict";

  var DELAY = 350;

  function wire(form) {
    if (form.dataset.gnEditorWired === "1") return;
    form.dataset.gnEditorWired = "1";
    var body = form.querySelector("[data-gn-body]");
    var pane = form.querySelector("[data-gn-preview]");
    var severity = form.querySelector("[data-gn-severity]");
    var urgent = form.querySelector("[data-gn-urgent]");
    if (!body || !pane) return;

    var timer = 0;
    var seq = 0;
    var empty = pane.innerHTML;

    function show(html) {
      // The answer is the server's sanitized output; parsing it in
      // an inert template keeps anything else from running while it
      // is moved in.
      var tpl = document.createElement("template");
      tpl.innerHTML = html;
      pane.replaceChildren(tpl.content);
    }
    function note(text) {
      var p = document.createElement("p");
      p.className = "gn-muted";
      p.textContent = text;
      pane.replaceChildren(p);
    }
    function refresh() {
      var text = body.value;
      if (!text.trim()) { show(empty); return; }
      var mine = ++seq;
      var data = new URLSearchParams();
      data.set("body", text);
      fetch("/admin/notices/preview", { method: "POST", body: data, credentials: "same-origin" })
        .then(function (r) {
          if (!r.ok) throw new Error(String(r.status));
          return r.json();
        })
        .then(function (res) {
          if (mine !== seq) return;
          if (res.too_long) { note("The post is over the length limit."); return; }
          show(res.html || "");
        })
        .catch(function () {
          if (mine === seq) note("The preview could not be loaded.");
        });
    }
    body.addEventListener("input", function () {
      clearTimeout(timer);
      timer = setTimeout(refresh, DELAY);
    });

    // App notifications are for warnings and critical notices.
    function gate() {
      if (!urgent || !severity) return;
      var ok = severity.value === "warning" || severity.value === "critical";
      urgent.disabled = !ok;
      if (!ok) urgent.checked = false;
    }
    if (severity) severity.addEventListener("change", gate);
    gate();
    refresh();
  }

  function boot() {
    var forms = document.querySelectorAll("form[data-gn-editor]");
    for (var i = 0; i < forms.length; i++) wire(forms[i]);
  }
  if (document.readyState === "loading") {
    document.addEventListener("DOMContentLoaded", boot);
  } else {
    boot();
  }
  document.addEventListener("evesynapse:content-ready", boot);
})();
