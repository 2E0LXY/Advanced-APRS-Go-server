// ===========================================================================
//  APRS Net Mobile - app logic
//  Touch-first SPA served at /mobile. Shares the live WebSocket + REST API
//  with the desktop dashboard. Designed to be the target of the Android app:
//  if the native bridge (window.aprsClient) is present it is used for
//  settings, GPS and notifications; otherwise everything works standalone.
// ===========================================================================

var mStations = {};
var mMessages = [];
var mMap, mMarkers = {}, mMyMarker = null;
var mWs = null, mWsRetry = 0, mWsAuthed = false;
var mCfg = { callsign:'', passcode:'', memberUser:'', memberPass:'' };
var mMsgCounter = 1;
var mNative = (typeof window !== 'undefined' && window.aprsClient) ? window.aprsClient : null;

// -- settings: native bridge if present, else localStorage --------------------
function loadCfg() {
  return new Promise(function (resolve) {
    if (mNative && mNative.getSettings) {
      mNative.getSettings().then(function (s) {
        mCfg.callsign   = (s && s.callsign)   || '';
        mCfg.passcode   = (s && s.passcode)   || '';
        mCfg.memberUser = (s && s.memberUser) || '';
        mCfg.memberPass = (s && s.memberPass) || '';
        resolve();
      }).catch(function () { resolve(); });
    } else {
      try {
        var raw = localStorage.getItem('aprs-mobile-cfg');
        if (raw) mCfg = Object.assign(mCfg, JSON.parse(raw));
      } catch (e) {}
      resolve();
    }
  });
}

function saveCfg() {
  if (mNative && mNative.saveSettings) {
    return mNative.saveSettings(mCfg);
  }
  try { localStorage.setItem('aprs-mobile-cfg', JSON.stringify(mCfg)); } catch (e) {}
  return Promise.resolve();
}

// -- map setup ----------------------------------------------------------------
mMap = L.map('m-map', { zoomControl:true, attributionControl:false }).setView([53.7,-1.5], 8);
L.tileLayer('https://{s}.tile.openstreetmap.org/{z}/{x}/{y}.png', { maxZoom:18 }).addTo(mMap);

// -- tab switching ------------------------------------------------------------
function switchView(view) {
  document.querySelectorAll('.m-view').forEach(function (v) { v.classList.remove('active'); });
  document.querySelectorAll('.m-tab').forEach(function (t) { t.classList.remove('active'); });
  document.getElementById('view-' + view).classList.add('active');
  var tab = document.querySelector('.m-tab[data-view="' + view + '"]');
  if (tab) tab.classList.add('active');
  if (view === 'map') setTimeout(function () { mMap.invalidateSize(); }, 100);
  if (view === 'status') loadMobileStatus();
  if (view === 'stations') renderStationList();
  if (view === 'messages') renderMobileMessages();
  if (view === 'send') updateSendCount();
  if (view === 'setup') fillSetupForm();
}

// -- WebSocket ----------------------------------------------------------------
function mobileConnect() {
  var proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  mWs = new WebSocket(proto + '//' + location.host + '/ws');
  mWs.onopen = function () {
    mWsRetry = 0;
    document.getElementById('m-dot').classList.add('live');
    // Authenticate if we have credentials (needed to send messages)
    if (mCfg.callsign && mCfg.passcode) {
      mWs.send(JSON.stringify({ type:'auth', callsign:mCfg.callsign,
        passcode:mCfg.passcode, software:'APRSNetMobile 1.1' }));
    }
  };
  mWs.onclose = function () {
    document.getElementById('m-dot').classList.remove('live');
    mWsAuthed = false;
    mWsRetry++;
    var delay = Math.min(30000, 1000 * Math.pow(1.5, Math.min(mWsRetry, 10)));
    setTimeout(mobileConnect, delay);
  };
  mWs.onerror = function () {};
  mWs.onmessage = function (ev) {
    try { handleMobilePacket(JSON.parse(ev.data)); } catch (e) {}
  };
}

function handleMobilePacket(d) {
  if (d.type === 'auth' || d.type === 'authok' || d.type === 'logresp') {
    mWsAuthed = true;
    return;
  }
  if (d.type === 'rx' && d.data) {
    var p = d.data;
    if (p.call && typeof p.lat === 'number' && typeof p.lon === 'number') {
      mStations[p.call] = { call:p.call, lat:p.lat, lon:p.lon,
        ts:p.ts || (Date.now()/1000), raw:p.raw || '', sym:p.sym || '' };
      updateMobileMarker(p.call);
    }
    var sc = document.getElementById('m-stn-count');
    if (sc) sc.innerText = Object.keys(mStations).length + ' stn';
  }
  if (d.type === 'rx' && d.packet) {
    parseMessagePacket(d.packet);
  }
}

// -- message parsing (incoming) ----------------------------------------------
// APRS message format:  FROM>path::TO_______:text{NN
function parseMessagePacket(packet) {
  var mm = packet.match(/^([A-Z0-9\-]+)>[^:]*::([A-Z0-9\- ]{9}):(.*)$/);
  if (!mm) return;
  var from = mm[1];
  var to   = mm[2].trim();
  var body = mm[3];

  // ACK packets
  var ackm = body.match(/^ack([0-9A-Za-z]+)/);
  if (ackm) {
    mMessages.unshift({ from:from, to:to, text:'(ack ' + ackm[1] + ')',
      ts:Date.now(), mine:false, ack:true });
    trimAndRender();
    return;
  }

  // strip trailing {NN message id
  var id = null;
  var idm = body.match(/\{([0-9A-Za-z]+)\}?\s*$/);
  if (idm) { id = idm[1]; body = body.replace(/\{[0-9A-Za-z]+\}?\s*$/, ''); }

  var addressedToMe = mCfg.callsign && to.toUpperCase() === mCfg.callsign.toUpperCase();

  mMessages.unshift({ from:from, to:to, text:body, ts:Date.now(),
    mine:false, addressed:addressedToMe });
  trimAndRender();

  // ACK any message addressed to us
  if (addressedToMe && id && mWsAuthed && mWs && mWs.readyState === 1) {
    var ackPkt = mCfg.callsign + '>APRS,TCPIP*::' +
      from.padEnd(9, ' ') + ':ack' + id;
    mWs.send(JSON.stringify({ type:'tx', packet:ackPkt }));
  }

  // Notify when a message is addressed to us
  if (addressedToMe) {
    notify('APRS message from ' + from, body);
  }
}

function trimAndRender() {
  if (mMessages.length > 80) mMessages = mMessages.slice(0, 80);
  if (document.getElementById('view-messages').classList.contains('active')) {
    renderMobileMessages();
  }
}

// -- notifications: native bridge if present, else web Notification ----------
function notify(title, body) {
  if (mNative && mNative.showNotification) {
    mNative.showNotification(title, body);
    return;
  }
  try {
    if ('Notification' in window && Notification.permission === 'granted') {
      new Notification(title, { body:body });
    }
  } catch (e) {}
}

// -- send a message -----------------------------------------------------------
function sendAprsMessage() {
  var toEl = document.getElementById('m-send-to');
  var txEl = document.getElementById('m-send-text');
  var status = document.getElementById('m-send-status');
  var to = (toEl.value || '').trim().toUpperCase();
  var text = (txEl.value || '').trim();

  function fail(msg) { status.className = 'm-status-line err'; status.innerText = msg; }
  function ok(msg)   { status.className = 'm-status-line ok';  status.innerText = msg; }

  if (!mCfg.callsign || !mCfg.passcode) {
    fail('Set your callsign and passcode in Setup first.');
    switchView('setup');
    return;
  }
  if (!to)   { fail('Enter a destination callsign.'); return; }
  if (!text) { fail('Enter a message.'); return; }
  if (!mWs || mWs.readyState !== 1) { fail('Not connected - try again in a moment.'); return; }
  if (!mWsAuthed) {
    // try to authenticate now
    mWs.send(JSON.stringify({ type:'auth', callsign:mCfg.callsign,
      passcode:mCfg.passcode, software:'APRSNetMobile 1.1' }));
    fail('Authenticating - tap Send again in a second.');
    return;
  }

  var id = String(mMsgCounter++ % 100).padStart(2, '0');
  var packet = mCfg.callsign + '>APRS,TCPIP*::' + to.padEnd(9, ' ') + ':' + text + '{' + id;
  mWs.send(JSON.stringify({ type:'tx', packet:packet }));

  mMessages.unshift({ from:mCfg.callsign, to:to, text:text, ts:Date.now(), mine:true });
  trimAndRender();

  txEl.value = '';
  updateSendCount();
  ok('Message sent to ' + to);
  setTimeout(function () { status.innerText = ''; }, 4000);
}

function updateSendCount() {
  var txEl = document.getElementById('m-send-text');
  var c = document.getElementById('m-send-count');
  if (txEl && c) c.innerText = (txEl.value || '').length + ' / 67';
}

// -- station markers + list ---------------------------------------------------
function updateMobileMarker(call) {
  var s = mStations[call];
  if (!s) return;
  if (mMarkers[call]) {
    mMarkers[call].setLatLng([s.lat, s.lon]);
  } else {
    mMarkers[call] = L.circleMarker([s.lat, s.lon], {
      radius:6, color:'#60a5fa', fillColor:'#3b82f6', fillOpacity:0.8, weight:2
    }).addTo(mMap);
    mMarkers[call].bindPopup('<b>' + call + '</b>');
  }
}

function renderStationList() {
  var list = document.getElementById('m-station-list');
  var calls = Object.keys(mStations).sort();
  if (!calls.length) { list.innerHTML = '<div class="m-loading">No stations heard yet</div>'; return; }
  var filter = (document.getElementById('m-search-box').value || '').toUpperCase();
  var now = Date.now() / 1000;
  var html = '';
  calls.forEach(function (call) {
    if (filter && call.indexOf(filter) === -1) return;
    var s = mStations[call];
    var age = Math.round((now - s.ts) / 60);
    var ageStr = age < 1 ? 'just now' : age + ' min ago';
    html += '<div class="m-list-item" onclick="focusStation(\'' + call + '\')">'
      + '<div style="flex:1"><div class="m-call">' + call + '</div>'
      + '<div class="m-meta">' + ageStr + ' &middot; ' + s.lat.toFixed(3) + ', ' + s.lon.toFixed(3)
      + '</div></div><div style="color:#475569">&rsaquo;</div></div>';
  });
  list.innerHTML = html || '<div class="m-loading">No matches</div>';
}

function filterStations() { renderStationList(); }

function focusStation(call) {
  var s = mStations[call];
  if (!s) return;
  switchView('map');
  setTimeout(function () {
    mMap.setView([s.lat, s.lon], 12);
    if (mMarkers[call]) mMarkers[call].openPopup();
  }, 200);
}

// -- messages render ----------------------------------------------------------
function renderMobileMessages() {
  var log = document.getElementById('m-msg-log');
  if (!mMessages.length) { log.innerHTML = '<div class="m-loading">No messages yet</div>'; return; }
  log.innerHTML = mMessages.map(function (m) {
    var t = new Date(m.ts).toLocaleTimeString();
    var cls = m.mine ? 'm-msg mine' : 'm-msg';
    var dir = m.mine ? (esc(m.from) + ' &rarr; ' + esc(m.to))
                     : (esc(m.from) + ' &rarr; ' + esc(m.to));
    return '<div class="' + cls + '"><span class="time">' + t + '</span>'
      + '<span class="from">' + dir + '</span>'
      + '<span class="txt">' + esc(m.text) + '</span></div>';
  }).join('');
}

// -- my location (GPS) --------------------------------------------------------
function locateMe() {
  var btn = document.getElementById('m-locate-btn');
  if (btn) btn.innerHTML = '&#8230;';

  function plot(lat, lon, label) {
    if (mMyMarker) {
      mMyMarker.setLatLng([lat, lon]);
    } else {
      mMyMarker = L.marker([lat, lon]).addTo(mMap);
    }
    mMyMarker.bindPopup('<b>' + (mCfg.callsign || 'My location') + '</b><br>' +
      lat.toFixed(5) + ', ' + lon.toFixed(5)).openPopup();
    mMap.setView([lat, lon], 13);
    if (btn) btn.innerHTML = '&#9678;';
  }

  // Prefer the native bridge (Android location services)
  if (mNative && mNative.getPosition) {
    mNative.getPosition().then(function (pos) {
      if (pos && typeof pos.lat === 'number') { plot(pos.lat, pos.lon); }
      else { browserGeo(plot, btn); }
    }).catch(function () { browserGeo(plot, btn); });
    return;
  }
  browserGeo(plot, btn);
}

function browserGeo(plot, btn) {
  if (!navigator.geolocation) {
    if (btn) btn.innerHTML = '&#9678;';
    alert('Location is not available on this device.');
    return;
  }
  navigator.geolocation.getCurrentPosition(
    function (p) { plot(p.coords.latitude, p.coords.longitude); },
    function () {
      if (btn) btn.innerHTML = '&#9678;';
      alert('Could not get your location. Check location permission.');
    },
    { enableHighAccuracy:true, timeout:10000, maximumAge:60000 }
  );
}

// -- status -------------------------------------------------------------------
function loadMobileStatus() {
  fetch('/api/status').then(function (r) { return r.json(); }).then(function (d) {
    document.getElementById('m-st-stations').innerText = Object.keys(mStations).length;
    document.getElementById('m-st-rx').innerText = (d.pkts_rx || 0).toLocaleString();
    document.getElementById('m-st-up').innerText = d.upstream_connected ? 'Connected' : 'Down';
    document.getElementById('m-st-uptime').innerText = d.uptime || '-';
    var mins = parseUptimeMin(d.uptime);
    document.getElementById('m-activity').innerText = Math.round((d.pkts_rx || 0) / Math.max(1, mins));
  }).catch(function () {});
}

function parseUptimeMin(up) {
  if (!up) return 1;
  var min = 0;
  var h = up.match(/(\d+)h/); if (h) min += parseInt(h[1]) * 60;
  var m = up.match(/(\d+)m/); if (m) min += parseInt(m[1]);
  return Math.max(1, min);
}

// -- setup form ---------------------------------------------------------------
function fillSetupForm() {
  document.getElementById('m-cfg-call').value  = mCfg.callsign || '';
  document.getElementById('m-cfg-pass').value  = mCfg.passcode || '';
  document.getElementById('m-cfg-muser').value = mCfg.memberUser || '';
  document.getElementById('m-cfg-mpass').value = mCfg.memberPass || '';
}

function saveSetup() {
  mCfg.callsign   = (document.getElementById('m-cfg-call').value || '').trim().toUpperCase();
  mCfg.passcode   = (document.getElementById('m-cfg-pass').value || '').trim();
  mCfg.memberUser = (document.getElementById('m-cfg-muser').value || '').trim();
  mCfg.memberPass = (document.getElementById('m-cfg-mpass').value || '');
  var status = document.getElementById('m-cfg-status');
  saveCfg().then(function () {
    status.className = 'm-status-line ok';
    status.innerText = 'Saved';
    updateCallBadge();
    // reconnect so the WebSocket authenticates with the new credentials
    if (mWs) { try { mWs.close(); } catch (e) {} }
    setTimeout(function () { status.innerText = ''; }, 3000);
  });
}

function updateCallBadge() {
  var b = document.getElementById('m-call-badge');
  if (b) b.innerText = mCfg.callsign || '';
}

function esc(s) {
  return String(s == null ? '' : s)
    .replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;');
}

// -- boot ---------------------------------------------------------------------
document.getElementById('m-send-text').addEventListener('input', updateSendCount);

loadCfg().then(function () {
  updateCallBadge();
  fillSetupForm();
  mobileConnect();
  // Ask for web notification permission when not running in the native app
  if (!mNative && 'Notification' in window && Notification.permission === 'default') {
    try { Notification.requestPermission(); } catch (e) {}
  }
});

setInterval(function () {
  if (document.getElementById('view-status').classList.contains('active')) loadMobileStatus();
}, 30000);