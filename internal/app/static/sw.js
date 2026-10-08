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
  }
  event.waitUntil(self.registration.showNotification(data.title || 'EveSynapse', options));
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
