// Minimal service worker — enough to make the site installable as a PWA (so it
// can register as an Android share target). Network passthrough, no caching.
self.addEventListener('install', function () {
  self.skipWaiting();
});

self.addEventListener('activate', function (event) {
  event.waitUntil(self.clients.claim());
});

self.addEventListener('fetch', function () {
  // passthrough; presence of this listener is what matters for installability
});
