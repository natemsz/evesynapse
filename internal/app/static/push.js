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

  // within gives up on a step that never answers, so a button cannot
  // hang: the wait ends with message as the error.
  function within(promise, ms, message) {
    return new Promise(function (resolve, reject) {
      var timer = setTimeout(function () { reject(new Error(message)); }, ms);
      promise.then(function (value) { clearTimeout(timer); resolve(value); },
        function (err) { clearTimeout(timer); reject(err); });
    });
  }

  // askPermission raises the browser's prompt, unless the answer is
  // already known. Older browsers take a callback instead of returning
  // a promise; both are handled.
  function askPermission() {
    if (Notification.permission !== 'default') {
      return Promise.resolve(Notification.permission);
    }
    return new Promise(function (resolve, reject) {
      var answered;
      try {
        answered = Notification.requestPermission(resolve);
      } catch (err) {
        reject(err);
        return;
      }
      if (answered && typeof answered.then === 'function') { answered.then(resolve, reject); }
    });
  }

  // The registration is used once its worker is active: subscribing
  // against one that is still installing does not complete.
  navigator.serviceWorker.register('/sw.js').then(function () {
    return within(navigator.serviceWorker.ready, 20000, 'the notification worker did not start');
  }).then(function (reg) {
    registration = reg;
    return refresh();
  }).catch(function (err) {
    say('Could not set up notifications in this browser: ' + err.message);
  });

  enableBtn.addEventListener('click', function () {
    if (!registration) { return; }
    enableBtn.disabled = true;
    say('Waiting for you to allow notifications…');
    within(askPermission(), 60000,
      'no permission prompt was answered. If none appeared, allow notifications for EveSynapse in the device’s settings (on Android: Settings, Apps, EveSynapse or your browser, Notifications), then come back here.'
    ).then(function (permission) {
      if (permission !== 'granted') { return refresh(); }
      say('Subscribing this browser…');
      return within(registration.pushManager.subscribe({
        userVisibleOnly: true,
        applicationServerKey: keyBytes(key)
      }), 30000, 'the browser’s push service did not answer. Check the connection and try again.').then(function (sub) {
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

  // The worker reports a test it received (sw.js), which tells apart
  // the two ways a test can fail to appear: the message never reached
  // this browser, or it did and the notification was not shown.
  var testReport = null;
  navigator.serviceWorker.addEventListener('message', function (event) {
    if (event.data && event.data.pushTest && testReport) { testReport(event.data); }
  });

  testBtn.addEventListener('click', function () {
    testBtn.disabled = true;
    var arrived = new Promise(function (resolve) { testReport = resolve; });
    registration.pushManager.getSubscription().then(function (sub) {
      return post('/notifications/push/test', sub ? { endpoint: sub.endpoint } : {});
    }).then(function () {
      var sent = 'Test sent. ';
      say(sent + 'Waiting for it to reach this browser…');
      return within(arrived, 30000, 'timeout').then(function (report) {
        if (report.pushTest === 'shown') {
          say(sent + 'It reached this browser, and the browser accepted the notification. If you did not see it, the system is hiding it: check its notification settings for this browser, and Do Not Disturb.');
        } else {
          say(sent + 'It reached this browser, but the browser refused to show it: ' + report.reason);
        }
      }, function () {
        say(sent + 'It has not reached this browser after 30 seconds. The push service took it, so the browser is not collecting its messages: restart the browser, and check it is allowed to run in the background.');
      });
    }).catch(function (err) {
      say('The test could not be sent: ' + err.message);
    }).then(function () { testReport = null; testBtn.disabled = false; });
  });
})();
