/*!
 * APRS symbol engine for aprsnet.uk
 *
 * Renders the official aprs.fi high-resolution APRS symbol set
 * (https://github.com/hessu/aprs-symbols, by Heikki Hannikainen OH7LZB) with
 * the descriptions from https://github.com/hessu/aprs-symbol-index (CC BY-SA 4.0).
 * See symbols/ATTRIBUTION.md.
 *
 * Symbol identification (APRS 1.0.1 ch.20 + compressed-position rules):
 *   table char '/'            -> primary table
 *   table char '\'            -> alternate table, no overlay
 *   table char 'A'-'Z','0'-'9'-> alternate table, with that overlay character
 *   table char 'a'-'j'        -> alternate table, overlay '0'-'9' (compressed-position encoding)
 *
 * Sprite sheets are 16 columns x 6 rows. The cell for symbol code `c` is
 *   idx = c.charCodeAt(0) - 33;  col = idx % 16;  row = idx / 16 | 0
 * Sheet 0 = primary, 1 = alternate, 2 = overlay characters (same cell layout,
 * drawn on top of the alternate-table symbol).
 *
 * Public API (window.AprsSymbols):
 *   parse(sym)            -> {table:0|1, code, overlay, idx} | null
 *   normalize(sym)        -> canonical 2-char code ('/>' or 'D#'), '' if invalid
 *   html(sym, opts)       -> HTML string for an inline symbol (opts: size, title, style, cls)
 *   draw(ctx, sym, x, y, size) -> Promise; draws onto a canvas 2D context
 *   dataURL(sym, size)    -> Promise<string> PNG data URL (notifications, favicons)
 *   describe(sym)         -> human description from the official index
 *   defaultFor(type)      -> fallback official symbol for a station type
 *   picker(el, opts)      -> mounts the full symbol picker widget
 *   ready                 -> Promise resolved when the description index has loaded
 */
(function (global) {
  'use strict';

  // Asset base: the directory this script was loaded from, so the engine also works when it is
  // pulled cross-origin (e.g. by the LoRa iGate's local dashboard). Falls back to /symbols/.
  var BASE = (function () {
    var cs = document.currentScript;
    return (cs && cs.src) ? cs.src.replace(/[^\/]*(\?.*)?$/, '') : '/symbols/';
  })();
  var COLS = 16, ROWS = 6;
  var DESC = {};
  var FALLBACK = '//';             // official "dot" - used when a symbol is missing/invalid

  function parse(sym) {
    if (typeof sym !== 'string' || sym.length < 2) return null;
    var t = sym.charAt(0), c = sym.charAt(1), cc = c.charCodeAt(0), table, overlay = null;
    if (cc < 33 || cc > 126) return null;
    if (t === '/') table = 0;
    else if (t === '\\') table = 1;
    else if ((t >= 'A' && t <= 'Z') || (t >= '0' && t <= '9')) { table = 1; overlay = t; }
    else if (t >= 'a' && t <= 'j') { table = 1; overlay = String.fromCharCode(48 + t.charCodeAt(0) - 97); }
    else return null;
    return { table: table, code: c, overlay: overlay, idx: cc - 33 };
  }

  function normalize(sym) {
    var p = parse(sym);
    if (!p) return '';
    return (p.table === 0 ? '/' : (p.overlay || '\\')) + p.code;
  }

  function dpr() { return Math.max(1, global.devicePixelRatio || 1); }

  // Pick the sheet resolution: 64px cells cover up to ~80 device px, 128px above that.
  function sheetPx(cssSize) { return (cssSize * dpr()) <= 80 ? 64 : 128; }
  function sheetUrl(px, table) { return BASE + 'aprs-symbols-' + px + '-' + table + '.png'; }

  function cssBackground(p, size, px) {
    var x = (p.idx % COLS) * size, y = ((p.idx / COLS) | 0) * size;
    var pos = '-' + x + 'px -' + y + 'px', bs = (COLS * size) + 'px ' + (ROWS * size) + 'px';
    var layers = [];
    if (p.overlay) {
      var ox = (p.overlay.charCodeAt(0) - 33);
      layers.push('url(' + sheetUrl(px, 2) + ') -' + ((ox % COLS) * size) + 'px -' + (((ox / COLS) | 0) * size) + 'px/' + bs + ' no-repeat');
    }
    layers.push('url(' + sheetUrl(px, p.table) + ') ' + pos + '/' + bs + ' no-repeat');
    return layers.join(',');
  }

  function describe(sym) {
    var p = parse(sym);
    if (!p) return 'Unknown symbol';
    var d = DESC[(p.table === 0 ? '/' : '\\') + p.code];
    if (!d) d = 'Reserved / unassigned';
    if (p.overlay) d += ' (overlay ' + p.overlay + ')';
    return d;
  }

  function html(sym, opts) {
    opts = opts || {};
    var size = Math.round(opts.size || 24);
    var p = parse(sym), valid = !!p;
    if (!p) p = parse(FALLBACK);
    var label = valid ? describe(sym) : 'Unknown symbol';
    var title = opts.title !== undefined ? opts.title : label + (valid ? ' (' + normalize(sym) + ')' : '');
    return '<span class="aprs-sym' + (opts.cls ? ' ' + opts.cls : '') + '" role="img" aria-label="' + esc(label) + '" title="' + esc(title) + '"'
      + ' style="display:inline-block;flex:none;vertical-align:middle;width:' + size + 'px;height:' + size + 'px;'
      + 'background:' + cssBackground(p, size, sheetPx(size)) + ';' + (opts.style || '') + '"></span>';
  }

  function esc(s) { return String(s).replace(/[&<>"']/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]; }); }

  // ── Canvas helpers ─────────────────────────────────────────────────────────
  var imgCache = {};
  function loadSheet(px, table) {
    var key = px + '-' + table;
    if (!imgCache[key]) imgCache[key] = new Promise(function (res, rej) {
      var im = new Image(); im.crossOrigin = 'anonymous'; im.onload = function () { res(im); }; im.onerror = rej; im.src = sheetUrl(px, table);
    });
    return imgCache[key];
  }

  function draw(ctx, sym, x, y, size) {
    var p = parse(sym) || parse(FALLBACK), px = sheetPx(size);
    var jobs = [loadSheet(px, p.table)];
    if (p.overlay) jobs.push(loadSheet(px, 2));
    return Promise.all(jobs).then(function (ims) {
      var s = ims[0].width / COLS;
      ctx.drawImage(ims[0], (p.idx % COLS) * s, ((p.idx / COLS) | 0) * s, s, s, x, y, size, size);
      if (ims[1]) {
        var o = p.overlay.charCodeAt(0) - 33;
        ctx.drawImage(ims[1], (o % COLS) * s, ((o / COLS) | 0) * s, s, s, x, y, size, size);
      }
    });
  }

  function dataURL(sym, size) {
    size = size || 64;
    var cv = document.createElement('canvas'); cv.width = cv.height = size;
    return draw(cv.getContext('2d'), sym, 0, 0, size).then(function () { return cv.toDataURL('image/png'); });
  }

  // Fallback official symbol for stations that never sent one.
  function defaultFor(type) {
    switch (type) {
      case 'ship':   return '/s';
      case 'ogn':    return "/'";
      case 'cwop':   return '/_';
      case 'lora':   return '\\&';
      case 'pistar': return '\\&';
      default:       return FALLBACK;
    }
  }

  // ── Picker widget ──────────────────────────────────────────────────────────
  // opts: {value:'/>', size:28, onChange:function(sym){}, compact:bool}
  function picker(el, opts) {
    opts = opts || {};
    var size = opts.size || 28;
    var cur = parse(opts.value || '//') || parse(FALLBACK);
    var state = { table: cur.table, overlay: cur.overlay, code: cur.code };
    el.innerHTML = '';
    el.classList.add('aprs-sym-picker');

    var bar = document.createElement('div'); bar.style.cssText = 'display:flex;gap:6px;align-items:center;flex-wrap:wrap;margin-bottom:6px';
    var tabs = [['Primary /', 0], ['Alternate \\', 1]].map(function (t) {
      var b = document.createElement('button'); b.type = 'button'; b.textContent = t[0];
      b.style.cssText = 'font-size:12px;padding:3px 10px;border-radius:4px;border:0;cursor:pointer;color:#fff';
      b.onclick = function () { state.table = t[1]; if (!t[1]) state.overlay = null; render(); };
      bar.appendChild(b); return b;
    });
    var ovWrap = document.createElement('label'); ovWrap.style.cssText = 'font-size:12px;color:#9ca3af;display:flex;gap:4px;align-items:center';
    ovWrap.appendChild(document.createTextNode('Overlay'));
    var ovSel = document.createElement('select'); ovSel.style.cssText = 'background:#1f2937;color:#e5e7eb;border:1px solid #374151;border-radius:4px;padding:2px 4px;font-size:12px';
    ovSel.innerHTML = '<option value="">none</option>' + '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ'.split('').map(function (c) { return '<option>' + c + '</option>'; }).join('');
    ovSel.onchange = function () { state.overlay = ovSel.value || null; render(); };
    ovWrap.appendChild(ovSel); bar.appendChild(ovWrap);

    var grid = document.createElement('div');
    grid.style.cssText = 'display:grid;grid-template-columns:repeat(16,' + (size + 4) + 'px);gap:2px;overflow:auto;max-height:' + (opts.compact ? 200 : 280) + 'px;padding:2px';
    var info = document.createElement('div'); info.style.cssText = 'display:flex;gap:10px;align-items:center;margin-top:8px;font-size:12px;color:#e5e7eb';
    el.appendChild(bar); el.appendChild(grid); el.appendChild(info);

    function value() { return (state.table === 0 ? '/' : (state.overlay || '\\')) + state.code; }

    function render() {
      tabs.forEach(function (b, i) { b.style.background = (i === state.table) ? '#0f766e' : '#374151'; });
      ovWrap.style.display = state.table === 1 ? 'flex' : 'none';
      ovSel.value = state.overlay || '';
      var base = (state.table === 0 ? '/' : '\\');
      var out = '';
      for (var i = 33; i <= 126; i++) {
        var c = String.fromCharCode(i), sel = (c === state.code);
        var t = (state.table === 1 && state.overlay) ? state.overlay : base;
        out += '<div data-c="' + esc(c) + '" style="cursor:pointer;border-radius:4px;padding:1px;' + (sel ? 'background:#0f766e' : '') + '">'
          + html(t + c, { size: size }) + '</div>';
      }
      grid.innerHTML = out;
      Array.prototype.forEach.call(grid.children, function (d) {
        d.onclick = function () { state.code = d.getAttribute('data-c'); render(); if (opts.onChange) opts.onChange(value()); };
      });
      info.innerHTML = html(value(), { size: 40 }) + '<div><div style="font-weight:700">' + esc(describe(value()))
        + '</div><div style="font-family:monospace;color:#9ca3af">Code: ' + esc(value()) + '</div></div>';
    }
    render();
    ready.then(render);   // refresh descriptions once the index has loaded
    return { value: value, set: function (s) { var p = parse(s); if (p) { state = { table: p.table, overlay: p.overlay, code: p.code }; render(); } } };
  }

  var ready = fetch(BASE + 'symbols.json', { cache: 'force-cache' }).then(function (r) { return r.json(); })
    .then(function (j) { DESC = j; }).catch(function () { /* descriptions are optional */ });

  global.AprsSymbols = {
    parse: parse, normalize: normalize, html: html, draw: draw, dataURL: dataURL,
    describe: describe, defaultFor: defaultFor, picker: picker, ready: ready, FALLBACK: FALLBACK
  };
})(window);
