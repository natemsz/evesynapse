// Settings page: the tab switch and the Interface tab's theme
// picker, colour-blind mode included. Loaded on that page only.
//
// The theme is an account setting: every pick is posted to the
// account (POST /settings/theme, ui_theme.go), which renders it
// onto <html> on every signed-in load, on every device. The
// localStorage copy stays as this browser's pre-paint cache and
// the signed-out behaviour, and boot.js applies it before first
// paint. Picking a scheme leaves the saved backdrop alone, so
// returning to the default brings back the backdrop strength
// the default was left at.
//
// The swap shell adopts this script once and never runs it
// again, so init wires the document as it stands each time it
// runs: once now (a full load, or the swap that adopted the
// script) and again on every content-ready, when the shell has
// swapped fresh Settings markup in, history restores included.
// Wired elements are marked, so a repeat init never binds a
// second handler to the same node.
(function () {
  'use strict';

  var root = document.documentElement;

  function wire(el) {
    if (!el || el.__esSettingsWired) return false;
    el.__esSettingsWired = true;
    return true;
  }

  function init() {
    // --- tabs -----------------------------------------------------
    var tabLinks = document.querySelectorAll('.section-tabs a[data-tab]');
    var panels = document.querySelectorAll('[data-tabpanel]');
    if (tabLinks.length && panels.length) {
      var showTab = function (id) {
        var i, found = false;
        for (i = 0; i < panels.length; i++) {
          var on = panels[i].getAttribute('data-tabpanel') === id;
          panels[i].hidden = !on;
          if (on) found = true;
        }
        if (!found) id = 'notifications';
        for (i = 0; i < tabLinks.length; i++) {
          var active = tabLinks[i].getAttribute('data-tab') === id;
          tabLinks[i].classList.toggle('active', active);
          if (active) tabLinks[i].setAttribute('aria-current', 'page');
          else tabLinks[i].removeAttribute('aria-current');
        }
        if (window.history && window.history.replaceState) {
          // The shell's state shape rides along: a bare state
          // here would cost the entry its scroll record.
          window.history.replaceState(
            { esSwap: 1, scroll: window.scrollY }, '', '#' + id);
        }
      };
      for (var t = 0; t < tabLinks.length; t++) {
        if (!wire(tabLinks[t])) continue;
        tabLinks[t].addEventListener('click', function (ev) {
          ev.preventDefault();
          showTab(this.getAttribute('data-tab'));
        });
      }
      var hash = window.location.hash;
      showTab(hash === '#interface' ? 'interface' : hash === '#security' ? 'security' : hash === '#api' ? 'api' : 'notifications');
    }

    // --- theme picker ---------------------------------------------
    var rows = document.querySelectorAll('[data-scheme-row]');
    if (!rows.length) return;

    function store(key, value) {
      try {
        if (value === null) window.localStorage.removeItem(key);
        else window.localStorage.setItem(key, value);
      } catch (e) { /* private mode: the click still works this visit */ }
    }

    // saveAccount posts the pick to the account, which is what
    // carries the theme across devices; the localStorage writes
    // in the apply functions are this browser's cache of it. The
    // wire shape is the whole state: the scheme, its tier, and
    // the backdrop strength the default was left at.
    function saveAccount() {
      // Only a signed-in account that can save carries the meta tag
      // (base.html); signed-out and demo visitors keep the theme in
      // this browser alone.
      if (!window.fetch || !document.querySelector('meta[name="es-theme-account"]')) return;
      var st = currentState();
      var body = new URLSearchParams();
      body.set('scheme', st.scheme);
      body.set('tier', st.scheme ? st.tier : '');
      var bd = null;
      try { bd = window.localStorage.getItem('evesynapse-backdrop'); } catch (e) {}
      body.set('vision', root.getAttribute('data-vision') || '');
      body.set('backdrop', st.scheme
        ? (bd === 'muted' || bd === 'glow' || bd === 'full' ? bd : '')
        : (st.tier === 'full' ? 'full' : st.tier === 'minimalistic' ? 'glow' : 'muted'));
      fetch('/settings/theme', {
        method: 'POST',
        headers: { 'X-Requested-With': 'XMLHttpRequest' },
        body: body,
        credentials: 'same-origin',
        keepalive: true
      }).then(function (resp) {
        if (resp.ok) store('evesynapse-theme-src', 'account');
      }, function () {});
    }

    function currentState() {
      var scheme = root.getAttribute('data-scheme') || '';
      if (scheme) {
        var tier = root.getAttribute('data-tier');
        return { scheme: scheme, tier: tier === 'minimalistic' ? 'minimalistic' : tier === 'full' ? 'full' : 'muted' };
      }
      var bd = root.getAttribute('data-backdrop');
      return { scheme: '', tier: bd === 'full' ? 'full' : bd === 'glow' ? 'minimalistic' : 'muted' };
    }

    function markCurrent() {
      var st = currentState();
      for (var i = 0; i < rows.length; i++) {
        var row = rows[i];
        var name = row.getAttribute('data-scheme-row') || '';
        row.classList.toggle('current', name === st.scheme);
        var buttons = row.querySelectorAll('.tier-seg button');
        for (var j = 0; j < buttons.length; j++) {
          var btn = buttons[j], on = false;
          if (name === '' && st.scheme === '') {
            var bd = btn.getAttribute('data-bd');
            on = (bd === 'full' && st.tier === 'full') ||
              ((bd === 'muted' || bd === '') && st.tier === 'muted') ||
              (bd === 'glow' && st.tier === 'minimalistic');
          } else if (name !== '' && name === st.scheme) {
            on = btn.getAttribute('data-tier') === st.tier;
          }
          btn.classList.toggle('on', on);
        }
      }
    }

    function applyDefault(bd) {
      root.removeAttribute('data-scheme');
      root.removeAttribute('data-tier');
      if (bd === 'full' || bd === 'glow') root.setAttribute('data-backdrop', bd);
      else root.setAttribute('data-backdrop', 'muted');
      store('evesynapse-scheme', null);
      store('evesynapse-tier', null);
      store('evesynapse-backdrop', bd || 'muted');
      saveAccount();
      markCurrent();
    }

    function applyScheme(scheme, tier) {
      root.setAttribute('data-scheme', scheme);
      root.setAttribute('data-tier', tier);
      root.removeAttribute('data-backdrop');
      store('evesynapse-scheme', scheme);
      store('evesynapse-tier', tier);
      saveAccount();
      markCurrent();
    }

    // The colour-blind mode sits beside the scheme: it retunes the
    // status colours of whichever scheme is on (bg.css), so a pick
    // here leaves the scheme, its tier and the backdrop alone.
    var visionButtons = document.querySelectorAll('[data-vision-row] [data-vision]');
    function markVision() {
      var v = root.getAttribute('data-vision') || '';
      for (var k = 0; k < visionButtons.length; k++) {
        visionButtons[k].classList.toggle('on', visionButtons[k].getAttribute('data-vision') === v);
      }
    }
    function applyVision(v) {
      if (v === 'redgreen' || v === 'blueyellow') root.setAttribute('data-vision', v);
      else { v = ''; root.removeAttribute('data-vision'); }
      store('evesynapse-vision', v || null);
      saveAccount();
      markVision();
    }
    for (var k = 0; k < visionButtons.length; k++) {
      (function (btn) {
        if (!wire(btn)) return;
        btn.addEventListener('click', function () { applyVision(btn.getAttribute('data-vision') || ''); });
      })(visionButtons[k]);
    }
    markVision();

    for (var i = 0; i < rows.length; i++) {
      (function (row) {
        var name = row.getAttribute('data-scheme-row') || '';
        var buttons = row.querySelectorAll('.tier-seg button');
        for (var j = 0; j < buttons.length; j++) {
          (function (btn) {
            if (!wire(btn)) return;
            btn.addEventListener('click', function () {
              if (name === '') applyDefault(btn.getAttribute('data-bd') || '');
              else if (btn.getAttribute('data-tier')) applyScheme(name, btn.getAttribute('data-tier'));
            });
          })(buttons[j]);
        }
      })(rows[i]);
    }
    markCurrent();
  }

  init();
  document.addEventListener('evesynapse:content-ready', init);
})();
