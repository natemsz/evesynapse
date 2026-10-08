# Decision: service worker and PWA scope for browser push

Status: proposed. Needed before the notifications work starts.

## Background

Background browser push (a notification while no EveSynapse tab is open)
requires a service worker; there is no way around that. In-app icons and
notifications shown while a tab is open do not.

Where the app stands today:

- `static/manifest.webmanifest` already exists (standalone, icons, scope
  `/`), so the app is installable. It has no service worker.
- The CSP is `default-src 'self'` with no `worker-src`, so a same-origin
  worker script is already allowed. No inline script is allowed, which suits
  a worker file.
- Static files are embedded and served from `/static/`. A worker served from
  there would be scoped to `/static/` only; it must be served at `/sw.js`
  (or with a `Service-Worker-Allowed: /` header) to cover the app.
- The background worker already walks every linked character each 60
  seconds and detects the changes that become notifications, so the server
  side of push has a natural home.
- Push needs HTTPS (`servedOverTLS`), VAPID keys, a push subscription table
  and a Web Push library. Roughly: one new dependency, one table, one
  endpoint to store a subscription, and a send step after each worker cycle.

## Options

1. **Minimal service worker (recommended).** A small `/sw.js` that handles
   only `push` and `notificationclick`. No fetch handler, no caching, no
   offline mode. Keeps the "server-rendered, always fresh" model and the
   `?v=` cache scheme untouched, and avoids stale-page and stale-CSRF
   problems. Installed-app status (already available via the manifest) is
   what iOS requires for push anyway.
2. **No service worker.** In-app icons plus notifications only while a tab is
   open (polling or SSE). Simplest, but a user who closed the tab gets
   nothing, which defeats most of the value for skill complete, stopped PI
   and industry alerts.
3. **Full PWA with offline caching.** Offline shell and cached pages. Large
   scope, real staleness and invalidation risk against server-rendered,
   per-character pages, and nothing on the roadmap needs it.

## Recommendation

Option 1. Build in-app notifications first (they work with or without push),
then add push as an opt-in channel using the same notification records and
per-type settings. Revisit offline caching only if a feature asks for it,
for example the wormhole mapper.
