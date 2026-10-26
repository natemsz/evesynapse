// Settings page: the tab switch and the Interface tab's theme
// picker. Loaded on that page only.
//
// The theme choice is this browser's alone: it rides data
// attributes on <html> (data-scheme and data-tier for a scheme,
// data-backdrop for the default), is saved to localStorage, and
// boot.js reapplies it before first paint. Nothing is stored
// server-side. Picking a scheme leaves the saved backdrop alone,
// so returning to the default brings back the backdrop strength
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
      showTab(window.location.hash === '#interface' ? 'interface' : 'notifications');
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

    function currentState() {
      var scheme = root.getAttribute('data-scheme') || '';
      if (scheme) {
        return { scheme: scheme, tier: root.getAttribute('data-tier') === 'minimalistic' ? 'minimalistic' : 'muted' };
      }
      var bd = root.getAttribute('data-backdrop');
      return { scheme: '', tier: bd === 'muted' ? 'muted' : bd === 'glow' ? 'minimalistic' : 'full' };
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
            on = (bd === '' && st.tier === 'full') || (bd === 'muted' && st.tier === 'muted') ||
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
      if (bd) root.setAttribute('data-backdrop', bd);
      else root.removeAttribute('data-backdrop');
      store('evesynapse-scheme', null);
      store('evesynapse-tier', null);
      store('evesynapse-backdrop', bd || 'art');
      markCurrent();
    }

    function applyScheme(scheme, tier) {
      root.setAttribute('data-scheme', scheme);
      root.setAttribute('data-tier', tier);
      root.removeAttribute('data-backdrop');
      store('evesynapse-scheme', scheme);
      store('evesynapse-tier', tier);
      markCurrent();
    }

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
