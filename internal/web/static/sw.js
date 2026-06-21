// Service worker: PWA installability (Android share target) + a designed offline
// page. Navigations are network-first; when the network is unavailable we serve
// the cached "the glass is dark" page. No other page/asset caching — assets are
// fingerprinted and the server stays the source of truth.
var CACHE = 'dnttg-v1';
var OFFLINE = '/offline.html';

self.addEventListener('install', function (event) {
  self.skipWaiting();
  event.waitUntil(caches.open(CACHE).then(function (c) { return c.add(OFFLINE); }));
});

self.addEventListener('activate', function (event) {
  event.waitUntil(Promise.all([
    self.clients.claim(),
    caches.keys().then(function (keys) {
      return Promise.all(keys.filter(function (k) { return k !== CACHE; }).map(function (k) { return caches.delete(k); }));
    }),
  ]));
});

self.addEventListener('fetch', function (event) {
  var req = event.request;
  if (req.method !== 'GET') return;
  if (req.mode === 'navigate') {
    event.respondWith(fetch(req).catch(function () { return caches.match(OFFLINE); }));
  }
});
