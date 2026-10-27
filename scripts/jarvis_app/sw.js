// Jarvis Chat service worker: cache the app shell so it opens fast (and in
// airplane mode), and always go to the network for API calls (/chat, /voice,
// /models) — those are dynamic and only work on the home LAN/VPN anyway.
const CACHE = 'jarvis-app-v1';
const SHELL = [
  '/app/',
  '/app/index.html',
  '/app/style.css',
  '/app/app.js',
  '/app/manifest.webmanifest',
  '/app/icons/icon-192.png',
  '/app/icons/icon-512.png',
  '/app/icons/apple-touch-icon.png',
];

self.addEventListener('install', (e) => {
  e.waitUntil(
    caches.open(CACHE)
      .then((c) => c.addAll(SHELL))
      .then(() => self.skipWaiting())
  );
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

self.addEventListener('fetch', (e) => {
  if (e.request.method !== 'GET') return; // never intercept POST /chat, /voice
  const url = new URL(e.request.url);
  if (!url.pathname.startsWith('/app/')) return; // API + everything else: network
  // Network-first for the shell; fall back to cache when offline.
  e.respondWith(
    fetch(e.request)
      .then((res) => {
        const copy = res.clone();
        caches.open(CACHE).then((c) => c.put(e.request, copy)).catch(() => {});
        return res;
      })
      .catch(() => caches.match(e.request).then((r) => r || caches.match('/app/')))
  );
});
