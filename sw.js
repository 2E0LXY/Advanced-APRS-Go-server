/* APRS Net Service Worker — handles Web Push notifications */
/* Installed by the main page; receives push events when the page is closed */

self.addEventListener('push', function(event) {
  if (!event.data) return;
  var payload;
  try { payload = event.data.json(); } catch(e) { return; }

  var title   = payload.title  || 'APRS Net';
  var options = {
    body:  payload.body || '',
    tag:   payload.tag  || 'aprs-net',
    icon:  '/favicon-32.png',
    badge: '/favicon-32.png',
    data:  payload.data || {},
    vibrate: [200, 100, 200],
    requireInteraction: false,
    silent: false,
  };

  // Colour the notification icon by type
  switch (payload.type) {
    case 'geofence_enter': options.body = '📍 ' + options.body; break;
    case 'geofence_exit':  options.body = '📤 ' + options.body; break;
    case 'message':        options.body = '💬 ' + options.body; break;
    case 'sota_pota':      options.body = '🏔 '  + options.body; break;
    case 'hab_start':      options.body = '🎈 ' + options.body; break;
    case 'hab_landed':     options.body = '🎈✓ ' + options.body; break;
  }

  event.waitUntil(self.registration.showNotification(title, options));
});

self.addEventListener('notificationclick', function(event) {
  event.notification.close();
  event.waitUntil(
    clients.matchAll({ type: 'window', includeUncontrolled: true })
      .then(function(list) {
        for (var c of list) {
          if (c.url.includes(self.location.origin) && 'focus' in c) {
            return c.focus();
          }
        }
        if (clients.openWindow) {
          return clients.openWindow('/');
        }
      })
  );
});

self.addEventListener('install',  function() { self.skipWaiting(); });
self.addEventListener('activate', function(event) {
  event.waitUntil(self.clients.claim());
});
