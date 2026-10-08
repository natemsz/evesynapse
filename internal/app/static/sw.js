// EveSynapse service worker: browser push, and nothing else.
//
// It shows a notification when a push arrives and opens the right
// page when the notification is clicked. It has no fetch handler and
// caches nothing, on purpose: pages are always served fresh by the
// server, exactly as they are without it.
//
// Served from /sw.js (not /static/) so that it can act for the whole
// site.
'use strict';

self.addEventListener('install', function () {
  self.skipWaiting();
});

self.addEventListener('activate', function (event) {
  event.waitUntil(self.clients.claim());
});

// localTarget keeps a notification inside this site: whatever a
// message names, the address opened is one of the app's own.
function localTarget(path) {
  var fallback = new URL('/notifications/', self.location.origin);
  try {
    var url = new URL(path || '/notifications/', self.location.origin);
    return url.origin === self.location.origin ? url : fallback;
  } catch (e) {
    return fallback;
  }
}

self.addEventListener('push', function (event) {
  var data = {};
  try {
    data = event.data ? event.data.json() : {};
  } catch (e) {
    data = {};
  }
  var options = {
    body: data.body || '',
    icon: '/static/icon-192.png',
    badge: '/static/badge-96.png',
    data: { url: data.url || '/notifications/' }
  };
  if (data.tag) {
    options.tag = data.tag;
    // A message that replaces one still on screen (the same tag) is
    // otherwise swapped in silently, with no banner and no sound.
    options.renotify = true;
  }
  var shown = self.registration.showNotification(data.title || 'EveSynapse', options);
  if (data.tag !== 'test') {
    event.waitUntil(shown);
    return;
  }
  // The test from the settings page: tell any open EveSynapse page
  // that the message arrived here and whether the browser took the
  // notification, so the page can say which half failed.
  event.waitUntil(shown.then(function () {
    return { pushTest: 'shown' };
  }, function (err) {
    return { pushTest: 'failed', reason: String((err && err.message) || err) };
  }).then(function (report) {
    return self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function (windows) {
      windows.forEach(function (win) { win.postMessage(report); });
    });
  }));
});

self.addEventListener('notificationclick', function (event) {
  event.notification.close();
  var target = localTarget(event.notification.data && event.notification.data.url);
  event.waitUntil(
    self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(function (windows) {
      // An EveSynapse tab that is already open is brought forward and
      // sent to the page; otherwise a new one is opened.
      for (var i = 0; i < windows.length; i++) {
        var win = windows[i];
        if ('focus' in win && 'navigate' in win) {
          return win.navigate(target.href).then(function (w) { return (w || win).focus(); });
        }
      }
      return self.clients.openWindow(target.href);
    })
  );
});
