// Browser notifications on the settings page: turn them on or off
// for this browser, and send a test. Loaded on that page only.
//
// The page works without this file; it only adds the three buttons'
// behaviour. Subscribing is always the visitor's own click: the
// browser's permission prompt is never raised on page load.
(function () {
  'use strict';

  var panel = document.getElementById('push-panel');
  if (!panel) { return; }
  var statusLine = document.getElementById('push-status');
  var enableBtn = document.getElementById('push-enable');
  var disableBtn = document.getElementById('push-disable');
  var testBtn = document.getElementById('push-test');
  var key = panel.getAttribute('data-vapid-key') || '';

  function say(text) { statusLine.textContent = text; }

  function show(subscribed) {
    enableBtn.hidden = subscribed;
    disableBtn.hidden = !subscribed;
    testBtn.hidden = !subscribed;
  }

  // The key arrives as base64url; subscribe() wants its bytes.
  function keyBytes(b64) {
    var padded = b64.replace(/-/g, '+').replace(/_/g, '/');
    while (padded.length % 4) { padded += '='; }
    var raw = atob(padded);
    var out = new Uint8Array(raw.length);
    for (var i = 0; i < raw.length; i++) { out[i] = raw.charCodeAt(i); }
    return out;
  }

  function post(path, body) {
    return fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      credentials: 'same-origin',
      body: JSON.stringify(body || {})
    }).then(function (res) {
      if (!res.ok) {
        return res.text().then(function (text) { throw new Error(text.trim() || ('HTTP ' + res.status)); });
      }
      return res;
    });
  }

  if (!('serviceWorker' in navigator) || !('PushManager' in window) || !('Notification' in window)) {
    say('This browser cannot receive push notifications. On an iPhone or iPad, add EveSynapse to the Home Screen first and open it from there.');
    return;
  }

  var registration = null;

  function refresh() {
    return registration.pushManager.getSubscription().then(function (sub) {
      if (sub) {
        say('This browser receives EveSynapse notifications.');
        show(true);
      } else if (Notification.permission === 'denied') {
        say('Notifications from this site are blocked in this browser. Allow them in the browser’s site settings, then come back here.');
        enableBtn.hidden = true;
        disableBtn.hidden = true;
        testBtn.hidden = true;
      } else {
        say('This browser does not receive EveSynapse notifications yet.');
        show(false);
      }
      return sub;
    });
  }

  navigator.serviceWorker.register('/sw.js').then(function (reg) {
    registration = reg;
    return refresh();
  }).catch(function (err) {
    say('Could not set up notifications in this browser: ' + err.message);
  });

  enableBtn.addEventListener('click', function () {
    if (!registration) { return; }
    enableBtn.disabled = true;
    Notification.requestPermission().then(function (permission) {
      if (permission !== 'granted') { return refresh(); }
      return registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: keyBytes(key)
      }).then(function (sub) {
        return post('/notifications/push/subscribe', sub.toJSON()).then(refresh, function (err) {
          // The server did not take it: do not leave the browser
          // subscribed to something nothing will send to.
          return sub.unsubscribe().then(function () { throw err; });
        });
      });
    }).catch(function (err) {
      say('Could not turn notifications on: ' + err.message);
    }).then(function () { enableBtn.disabled = false; });
  });

  disableBtn.addEventListener('click', function () {
    if (!registration) { return; }
    disableBtn.disabled = true;
    registration.pushManager.getSubscription().then(function (sub) {
      if (!sub) { return; }
      var endpoint = sub.endpoint;
      return sub.unsubscribe().then(function () {
        return post('/notifications/push/unsubscribe', { endpoint: endpoint });
      });
    }).then(refresh).catch(function (err) {
      say('Could not turn notifications off: ' + err.message);
    }).then(function () { disableBtn.disabled = false; });
  });

  testBtn.addEventListener('click', function () {
    testBtn.disabled = true;
    post('/notifications/push/test').then(function () {
      say('Test sent. It should appear in a moment.');
    }).catch(function (err) {
      say('The test could not be sent: ' + err.message);
    }).then(function () { testBtn.disabled = false; });
  });
})();
