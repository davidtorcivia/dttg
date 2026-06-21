(function () {
  'use strict';

  // ---- service worker (PWA installability + Android share target) ----
  if ('serviceWorker' in navigator) {
    window.addEventListener('load', function () {
      navigator.serviceWorker.register('/sw.js').catch(function () {});
    });
  }

  // ---- row-major masonry ----
  // The board reads chronologically left-to-right across the top row, then wraps
  // down (newest three in the top row, etc.). Cards are distributed round-robin
  // into N flex columns; each column still packs to its own content height, so it
  // stays a masonry without the column-major fill order of CSS `column-count`.
  // Phones get a single column and small/narrow windows two; desktop honours the
  // configured 3 or 4 (the masonry order stays chronological via grid.__order, so
  // collapsing to fewer columns doesn't scramble it).
  function gridColumnCount(maxCols) {
    var w = window.innerWidth;
    if (w <= 600) return 1;
    if (w <= 1000) return 2;
    return maxCols;
  }
  function gridMaxCols(grid) {
    var v = parseInt(grid.style.getPropertyValue('--cols'), 10);
    return (v === 3 || v === 4) ? v : 3;
  }
  function shortestCol(heights) {
    var m = 0;
    for (var j = 1; j < heights.length; j++) if (heights[j] < heights[m]) m = j;
    return m;
  }
  function layoutMasonry(grid, force) {
    var n = gridColumnCount(gridMaxCols(grid));
    if (!force && grid.__n === n) return; // column count unchanged; nothing to do
    // Redistribute from the captured chronological order, NOT the current DOM order:
    // after a multi-column layout the DOM is grouped by column, so collapsing to a
    // single column (e.g. rotating to portrait) would otherwise scramble the order.
    // Capture on first layout, while the DOM is still in server (chronological) order.
    if (!grid.__order) grid.__order = Array.prototype.slice.call(grid.querySelectorAll('.card'));
    var cards = grid.__order;
    grid.textContent = '';
    var cols = [], heights = [];
    for (var i = 0; i < n; i++) {
      var c = document.createElement('div');
      c.className = 'grid-col';
      grid.appendChild(c);
      cols.push(c);
      heights.push(0);
    }
    // Walk cards newest-first and drop each into the currently shortest column.
    // Image heights are reserved via width/height attributes, so the measured
    // column height is accurate even before the images finish loading.
    cards.forEach(function (card) {
      var m = shortestCol(heights);
      cols[m].appendChild(card);
      heights[m] = cols[m].offsetHeight;
    });
    grid.__cols = cols;
    grid.__heights = heights;
    grid.__n = n;
  }
  // Append one card into the currently shortest column (infinite scroll).
  function masonryAppend(grid, card) {
    var cols = grid.__cols, heights = grid.__heights;
    if (grid.__order) grid.__order.push(card); // keep the chronological order in sync
    if (!cols || !cols.length) { grid.appendChild(card); return; }
    var m = shortestCol(heights);
    cols[m].appendChild(card);
    heights[m] = cols[m].offsetHeight;
  }
  // Blur-up: show the tiny placeholder behind a thumbnail while it loads, then
  // drop both the placeholder and the dominant-color backdrop once it has loaded.
  // A card shorter than its meta column would otherwise show that fill in the gap
  // below the image — leave the page background showing through instead.
  function blurUp(img) {
    var c = img && img.parentElement;
    if (!c) return;
    var clear = function () { c.style.backgroundImage = 'none'; c.style.backgroundColor = 'transparent'; };
    if (img.complete && img.naturalWidth) { clear(); return; } // already loaded
    var ph = img.getAttribute('data-ph');
    if (ph) {
      c.style.backgroundImage = 'url("' + ph + '")';
      c.style.backgroundSize = 'cover';
      c.style.backgroundPosition = 'center';
    }
    img.addEventListener('load', clear);
  }
  // Eager-ahead loading: the browser's native loading=lazy threshold is tight
  // enough that thumbnails visibly pop in while scrolling. This observer forces a
  // lazy thumb to start fetching ~2 viewports before it enters view, so it's
  // already decoded by the time the card scrolls in.
  var eagerIO = ('IntersectionObserver' in window) ? new IntersectionObserver(function (entries) {
    entries.forEach(function (e) {
      if (!e.isIntersecting) return;
      e.target.loading = 'eager'; // kicks the fetch in modern browsers
      eagerIO.unobserve(e.target);
    });
  }, { rootMargin: '0px 0px 200% 0px' }) : null;
  function observeEager(img) {
    if (eagerIO && img && img.getAttribute('loading') === 'lazy' && !(img.complete && img.naturalWidth)) {
      eagerIO.observe(img);
    }
  }
  (function () {
    var grid = document.getElementById('grid');
    if (!grid) return;
    grid.classList.add('grid--cols'); // upgrade from the block fallback to flex columns
    layoutMasonry(grid, true);
    var rt;
    window.addEventListener('resize', function () {
      clearTimeout(rt);
      rt = setTimeout(function () { layoutMasonry(grid); }, 150);
    });
  })();

  // ---- custom blend-mode cursor (fine pointers only) ----
  if (window.matchMedia('(pointer: fine)').matches) {
    var cursor = document.createElement('div');
    cursor.className = 'cursor';
    document.body.appendChild(cursor);
    document.addEventListener('mousemove', function (e) {
      cursor.style.left = e.clientX + 'px';
      cursor.style.top = e.clientY + 'px';
    });
    document.body.addEventListener('mouseover', function (e) {
      var hot = e.target.closest('a, button, img, .card');
      cursor.classList.toggle('hovering', !!hot);
    });
  }

  // ---- search overlay + live results ----
  (function () {
    var toggle = document.getElementById('search-toggle');
    var overlay = document.getElementById('search-overlay');
    var input = document.getElementById('search-input');
    var close = document.getElementById('search-close');
    var results = document.getElementById('search-results');
    if (!overlay) return;

    var isOpen = function () { return overlay.classList.contains('open'); };
    var open = function (e) {
      if (e) e.preventDefault();
      overlay.classList.add('open');
      // focus() is a no-op while the overlay is still visibility:hidden (it stays
      // hidden until the .open style is applied + composited), and we can't know
      // exactly which frame that lands on — so focus and retry each frame until it
      // actually sticks, so you can start typing the moment the bar opens.
      if (input) {
        var tries = 0;
        var focusNow = function () {
          input.focus();
          if (document.activeElement === input) { input.select(); return; }
          if (tries++ < 15) requestAnimationFrame(focusNow);
        };
        focusNow();
      }
    };
    var hide = function () { overlay.classList.remove('open'); };

    if (toggle) toggle.addEventListener('click', open);
    if (close) close.addEventListener('click', hide);
    overlay.addEventListener('click', function (e) { if (e.target === overlay) hide(); });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape' && isOpen()) hide();
      if (e.key === '/' && document.activeElement === document.body) open(e);
    });

    var esc = function (s) {
      return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
        return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
      });
    };
    var render = function (items) {
      if (!results) return;
      if (!items.length) { results.innerHTML = '<div class="search-empty">No matches</div>'; return; }
      results.innerHTML = items.map(function (it) {
        var media = it.thumb
          ? '<img src="' + esc(it.thumb) + '" alt="">'
          : '<span class="sr-tile">' + esc((it.kind || '').slice(0, 3).toUpperCase()) + '</span>';
        var meta = [it.category, it.date].filter(Boolean).map(esc).join(' · ');
        return '<a class="search-result" href="' + esc(it.url) + '">' + media +
          '<span class="sr-main"><span class="sr-title">' + esc(it.title || '(untitled)') +
          '</span><span class="sr-meta">' + meta + '</span></span></a>';
      }).join('');
    };
    var timer;
    if (input && results) {
      input.addEventListener('input', function () {
        clearTimeout(timer);
        var q = input.value.trim();
        if (!q) { results.innerHTML = ''; return; }
        timer = setTimeout(function () {
          fetch('/api/search?q=' + encodeURIComponent(q))
            .then(function (r) { return r.json(); })
            .then(function (d) { render(d.results || []); })
            .catch(function () {});
        }, 200);
      });
    }
  })();

  // ---- live results on the /search page ----
  (function () {
    var form = document.querySelector('.search-page-form');
    var input = form && form.querySelector('input[name="q"]');
    var grid = document.getElementById('grid');
    var meta = document.querySelector('.search-meta');
    if (!form || !input || !grid) return;
    var revive = function () {
      grid.querySelectorAll('.card').forEach(function (card) {
        var img = card.querySelector('.card-img-container img');
        if (img) { blurUp(img); observeEager(img); }
        var vt = card.querySelector('[data-vt]'); if (vt) { try { vt.style.viewTransitionName = vt.getAttribute('data-vt'); } catch (e) {} }
        card.classList.add('visible');
      });
    };
    var timer;
    var run = function () {
      var q = input.value.trim();
      if (!q) { grid.innerHTML = ''; if (meta) meta.textContent = ''; return; }
      fetch('/search/cards?q=' + encodeURIComponent(q))
        .then(function (r) { return r.text(); })
        .then(function (html) {
          grid.innerHTML = html;
          grid.__order = null; // new result set — recapture chronological order
          revive();
          layoutMasonry(grid, true);
          var n = grid.querySelectorAll('.card').length;
          if (meta) meta.textContent = n + ' result' + (n === 1 ? '' : 's') + ' for “' + q + '”';
        }).catch(function () {});
    };
    input.addEventListener('input', function () { clearTimeout(timer); timer = setTimeout(run, 200); });
  })();

  // ---- detail video: custom, on-brand control bar (fades in on hover/focus) ----
  // The video autoplays muted on a loop; native controls can't be styled or fade-
  // timed cross-browser, so we draw our own thin monochrome bar — play/pause, a
  // keyboard-operable scrub slider, time, mute, and fullscreen — over the media.
  // Shortcuts (when the video/controls have focus): Space/k play, j/l/←/→ seek,
  // m mute, f fullscreen. stopPropagation keeps them off the detail prev/next nav.
  (function () {
    var SVG = {
      play:  '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><path d="M4 3l9 5-9 5z" fill="currentColor"/></svg>',
      pause: '<svg viewBox="0 0 16 16" width="11" height="11" aria-hidden="true"><rect x="4" y="3" width="3" height="10" fill="currentColor"/><rect x="9" y="3" width="3" height="10" fill="currentColor"/></svg>',
      sound: '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true"><path d="M2 6h3l4-3v10L5 10H2z" fill="currentColor"/><path d="M11 5.4a3 3 0 0 1 0 5.2" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>',
      muted: '<svg viewBox="0 0 16 16" width="13" height="13" aria-hidden="true"><path d="M2 6h3l4-3v10L5 10H2z" fill="currentColor"/><path d="M11 6l3.2 4M14.2 6L11 10" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>',
      full:  '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true"><path d="M2 6V2h4M14 6V2h-4M2 10v4h4M14 10v4h-4" fill="none" stroke="currentColor" stroke-width="1.4"/></svg>',
      exit:  '<svg viewBox="0 0 16 16" width="12" height="12" aria-hidden="true"><path d="M6 2v4H2M10 2v4h4M6 14v-4H2M10 14v-4h4" fill="none" stroke="currentColor" stroke-width="1.4"/></svg>'
    };
    var fmt = function (t) {
      if (!isFinite(t) || t < 0) t = 0;
      var m = Math.floor(t / 60), s = Math.floor(t % 60);
      return m + ':' + (s < 10 ? '0' : '') + s;
    };
    document.querySelectorAll('video.detail-video').forEach(function (v) {
      var stage = v.parentElement;
      if (!stage) return;
      v.tabIndex = 0; // reachable by keyboard; focus also reveals the bar (:focus-within)
      var bar = document.createElement('div');
      bar.className = 'vbar';
      bar.innerHTML =
        '<button type="button" class="vbar-btn vbar-play" aria-label="Play / pause"></button>' +
        '<div class="vbar-track" role="slider" tabindex="0" aria-label="Seek" aria-valuemin="0" aria-valuemax="100" aria-valuenow="0"><div class="vbar-fill"></div></div>' +
        '<span class="vbar-time">0:00</span>' +
        '<button type="button" class="vbar-btn vbar-mute" aria-label="Mute / unmute"></button>' +
        '<button type="button" class="vbar-btn vbar-full" aria-label="Fullscreen"></button>';
      stage.appendChild(bar);
      var playBtn = bar.querySelector('.vbar-play');
      var muteBtn = bar.querySelector('.vbar-mute');
      var fullBtn = bar.querySelector('.vbar-full');
      var track = bar.querySelector('.vbar-track');
      var fill = bar.querySelector('.vbar-fill');
      var timeEl = bar.querySelector('.vbar-time');

      var syncPlay = function () { playBtn.innerHTML = v.paused ? SVG.play : SVG.pause; };
      var syncMute = function () { muteBtn.innerHTML = v.muted ? SVG.muted : SVG.sound; };
      var syncFull = function () { fullBtn.innerHTML = document.fullscreenElement ? SVG.exit : SVG.full; };
      var togglePlay = function () { if (v.paused) { v.play(); } else { v.pause(); } };
      var seek = function (d) { if (v.duration) v.currentTime = Math.min(v.duration, Math.max(0, v.currentTime + d)); };
      var toggleFull = function () {
        if (document.fullscreenElement) { if (document.exitFullscreen) document.exitFullscreen(); }
        else if (stage.requestFullscreen) { stage.requestFullscreen(); }
        else if (v.webkitEnterFullscreen) { v.webkitEnterFullscreen(); } // iOS Safari (video only)
      };

      playBtn.addEventListener('click', togglePlay);
      muteBtn.addEventListener('click', function () { v.muted = !v.muted; });
      fullBtn.addEventListener('click', toggleFull);
      v.addEventListener('play', syncPlay);
      v.addEventListener('pause', syncPlay);
      v.addEventListener('volumechange', syncMute);
      document.addEventListener('fullscreenchange', syncFull);
      v.addEventListener('timeupdate', function () {
        var d = v.duration || 0, pct = d ? (v.currentTime / d * 100) : 0;
        fill.style.width = pct + '%';
        track.setAttribute('aria-valuenow', Math.round(pct));
        timeEl.textContent = fmt(v.currentTime) + (d ? ' / ' + fmt(d) : '');
      });
      track.addEventListener('click', function (e) {
        var r = track.getBoundingClientRect();
        if (v.duration) v.currentTime = Math.min(1, Math.max(0, (e.clientX - r.left) / r.width)) * v.duration;
      });
      track.addEventListener('keydown', function (e) {
        var handled = true;
        switch (e.key) {
          case 'ArrowLeft': case 'ArrowDown': seek(-5); break;
          case 'ArrowRight': case 'ArrowUp': seek(5); break;
          case 'Home': if (v.duration) v.currentTime = 0; break;
          case 'End': if (v.duration) v.currentTime = v.duration; break;
          default: handled = false;
        }
        if (handled) { e.preventDefault(); e.stopPropagation(); }
      });
      v.addEventListener('keydown', function (e) {
        var handled = true;
        switch (e.key) {
          case ' ': case 'k': togglePlay(); break;
          case 'ArrowLeft': case 'j': seek(-5); break;
          case 'ArrowRight': case 'l': seek(5); break;
          case 'm': v.muted = !v.muted; break;
          case 'f': toggleFull(); break;
          default: handled = false;
        }
        if (handled) { e.preventDefault(); e.stopPropagation(); }
      });
      syncPlay();
      syncMute();
      syncFull();
    });
  })();

  // ---- detail prev/next keyboard nav (arrow keys) ----
  (function () {
    var prev = document.getElementById('nav-prev');
    var next = document.getElementById('nav-next');
    if (!prev && !next) return;
    document.addEventListener('keydown', function (e) {
      var t = e.target;
      if (t && t.matches && t.matches('input, textarea, select')) return;
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === 'ArrowLeft' && prev) window.location.href = prev.href;
      if (e.key === 'ArrowRight' && next) window.location.href = next.href;
    });
  })();

  // ---- bookmarklet builder (set href client-side to avoid URL re-encoding) ----
  (function () {
    var bm = document.getElementById('bookmarklet');
    if (!bm) return;
    var base = (bm.getAttribute('data-base') || '').replace(/\/+$/, '');
    bm.setAttribute('href',
      "javascript:(function(){var u=encodeURIComponent(location.href)," +
      "t=encodeURIComponent(document.title)," +
      "s=encodeURIComponent(String(window.getSelection()||''));" +
      "window.open('" + base + "/admin/new?url='+u+'&title='+t+'&note='+s," +
      "'dnttg','width=520,height=760');})();");
    bm.addEventListener('click', function (e) {
      e.preventDefault();
      bm.textContent = '↑ Drag me to your bookmarks bar';
      setTimeout(function () { bm.textContent = '+ Save to the glass'; }, 1800);
    });
  })();

  // ---- themed file input filename ----
  document.querySelectorAll('input[type=file]').forEach(function (inp) {
    inp.addEventListener('change', function () {
      var label = inp.parentElement && inp.parentElement.querySelector('[data-file-name]');
      if (label) label.textContent = (inp.files && inp.files.length) ? inp.files[0].name : 'No file chosen';
    });
  });

  // ---- blur-up placeholders (tiny base64 image scaled to cover) ----
  document.querySelectorAll('.card-img-container img').forEach(function (img) { blurUp(img); observeEager(img); });

  // ---- staggered card entrance ----
  var cards = document.querySelectorAll('.card');
  if (cards.length) {
    var observer = new IntersectionObserver(function (entries) {
      entries.forEach(function (entry) {
        if (entry.isIntersecting) {
          entry.target.classList.add('visible');
          observer.unobserve(entry.target);
        }
      });
    }, { threshold: 0.1, rootMargin: '0px 0px -50px 0px' });
    cards.forEach(function (card, i) {
      card.style.transitionDelay = (i % 3) * 0.1 + 's';
      observer.observe(card);
    });
  }

  // ---- system clock + NYC weather ----
  var clock = document.getElementById('sys-clock');
  if (clock) {
    var TEMP_TTL = 15 * 60 * 1000; // refresh the temperature at most this often
    var temp = '--';
    var update = function () {
      var now = new Date();
      var t = now.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit' });
      clock.textContent = t + ' // NYC ' + temp + '°F';
    };
    var setTemp = function (v) {
      temp = v;
      try { localStorage.setItem('dnttg-temp', JSON.stringify({ t: v, at: Date.now() })); } catch (e) {}
      update();
    };
    var fetchWeather = function () {
      fetch('/api/weather')
        .then(function (r) { return r.json(); })
        .then(function (d) { if (typeof d.temp === 'number') setTemp(d.temp); })
        .catch(function () { /* keep whatever we already have */ });
    };
    // Hydrate instantly from the last fetch so moving around the site doesn't
    // re-request the temperature on every click; only hit the network when the
    // cached value is missing or older than the TTL.
    var stale = true;
    try {
      var cachedTemp = JSON.parse(localStorage.getItem('dnttg-temp') || 'null');
      if (cachedTemp && typeof cachedTemp.t === 'number') {
        temp = cachedTemp.t;
        stale = Date.now() - (cachedTemp.at || 0) >= TEMP_TTL;
      }
    } catch (e) {}
    update();
    setInterval(update, 1000);
    if (stale) fetchWeather();
    setInterval(fetchWeather, TEMP_TTL); // keep it current while the tab stays open
  }

  var typing = function (el) { return el && el.matches && el.matches('input, textarea, select'); };

  // ---- view transition names (shared-element morph board <-> detail) ----
  document.querySelectorAll('[data-vt]').forEach(function (el) {
    try { el.style.viewTransitionName = el.getAttribute('data-vt'); } catch (e) {}
  });

  // ---- dark mode ----
  // A click is authoritative: it sets an explicit data-theme AND persists it, so it
  // always wins. The OS preference is only the default until you've chosen, and a
  // live OS change is applied only while no choice is stored ("auto").
  (function () {
    var root = document.documentElement;
    var apply = function (theme, persist) {
      root.setAttribute('data-theme', theme);
      root.style.colorScheme = theme;
      // Always mirror the *resolved* theme to a cookie so the server can render
      // <html data-theme> + <meta name=color-scheme> from byte 0 — even in auto/system
      // mode. Without that, a slow (tunnel) navigation paints one light frame before
      // JS sets the theme, which is the Firefox white flash.
      try { document.cookie = 'dnttg-theme=' + theme + '; path=/; max-age=31536000; samesite=lax'; } catch (e) {}
      if (persist) { try { localStorage.setItem('dnttg-theme', theme); } catch (e) {} }
    };
    window.__toggleTheme = function () {
      apply(root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark', true);
    };
    var btn = document.getElementById('theme-toggle');
    if (btn) btn.addEventListener('click', window.__toggleTheme);
    try {
      matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function (e) {
        var saved = localStorage.getItem('dnttg-theme');
        if (saved !== 'light' && saved !== 'dark') apply(e.matches ? 'dark' : 'light', false);
      });
    } catch (e) {}
  })();

  // ---- lightbox (with zoom + pan) ----
  // Wheel / pinch to zoom toward the pointer, drag to pan when zoomed, click the
  // image to toggle zoom, click the backdrop or Esc to close. transform-origin is
  // the default (center), so scaling grows from the middle and translate recenters.
  (function () {
    var box = document.getElementById('lightbox');
    var img = document.getElementById('lightbox-img');
    if (!box || !img) return;
    var MAX = 6, scale = 1, tx = 0, ty = 0;
    var pointers = {}, pinchDist = 0, pinchScale = 1, dragId = null, dragX = 0, dragY = 0, dragTx = 0, dragTy = 0, moved = false;

    var clampPan = function () {
      var bx = Math.max(0, (img.clientWidth * scale - window.innerWidth) / 2 + 40);
      var by = Math.max(0, (img.clientHeight * scale - window.innerHeight) / 2 + 40);
      tx = Math.min(bx, Math.max(-bx, tx));
      ty = Math.min(by, Math.max(-by, ty));
    };
    var apply = function (animate) {
      img.style.transition = animate ? 'transform .3s cubic-bezier(.2,.8,.2,1)' : 'none';
      img.style.transform = 'translate(' + tx + 'px,' + ty + 'px) scale(' + scale + ')';
      img.classList.toggle('lb-zoomed', scale > 1.01);
    };
    var reset = function (animate) { scale = 1; tx = 0; ty = 0; apply(animate); };
    // zoom toward a screen point (cx,cy), keeping that point stationary
    var zoomTo = function (next, cx, cy, animate) {
      next = Math.min(MAX, Math.max(1, next));
      var r = img.getBoundingClientRect();
      var dx = cx - (r.left + r.width / 2), dy = cy - (r.top + r.height / 2);
      var ratio = next / scale;
      tx -= dx * (ratio - 1);
      ty -= dy * (ratio - 1);
      scale = next;
      if (scale <= 1.01) { scale = 1; tx = 0; ty = 0; }
      clampPan();
      apply(animate);
    };
    var open = function (z) { img.src = z.getAttribute('data-full') || z.src; reset(false); box.classList.add('open'); };
    var close = function () { box.classList.remove('open'); reset(false); };

    document.querySelectorAll('.zoomable').forEach(function (z) {
      z.addEventListener('click', function () { open(z); });
    });
    box.addEventListener('click', function (e) { if (e.target === box) close(); });
    document.addEventListener('keydown', function (e) {
      if (!box.classList.contains('open')) return;
      if (e.key === 'Escape') close();
      else if (e.key === '+' || e.key === '=') zoomTo(scale * 1.4, window.innerWidth / 2, window.innerHeight / 2, true);
      else if (e.key === '-' || e.key === '_') zoomTo(scale / 1.4, window.innerWidth / 2, window.innerHeight / 2, true);
      else if (e.key === '0') reset(true);
    });
    box.addEventListener('wheel', function (e) {
      if (!box.classList.contains('open')) return;
      e.preventDefault();
      zoomTo(scale * (e.deltaY < 0 ? 1.18 : 1 / 1.18), e.clientX, e.clientY, false);
    }, { passive: false });

    img.addEventListener('click', function (e) {
      e.stopPropagation();              // don't let the backdrop handler close it
      if (moved) return;                // this was a drag, not a click
      if (scale > 1.01) reset(true); else zoomTo(2.5, e.clientX, e.clientY, true);
    });

    // pointer-based pan (1 finger / mouse) + pinch (2 fingers)
    img.addEventListener('pointerdown', function (e) {
      pointers[e.pointerId] = { x: e.clientX, y: e.clientY };
      var ids = Object.keys(pointers);
      if (ids.length === 1) {
        dragId = e.pointerId; dragX = e.clientX; dragY = e.clientY; dragTx = tx; dragTy = ty; moved = false;
        try { img.setPointerCapture(e.pointerId); } catch (err) {}
      } else if (ids.length === 2) {
        var a = pointers[ids[0]], b = pointers[ids[1]];
        pinchDist = Math.hypot(a.x - b.x, a.y - b.y); pinchScale = scale; dragId = null;
      }
    });
    img.addEventListener('pointermove', function (e) {
      if (!pointers[e.pointerId]) return;
      pointers[e.pointerId] = { x: e.clientX, y: e.clientY };
      var ids = Object.keys(pointers);
      if (ids.length >= 2) {
        var a = pointers[ids[0]], b = pointers[ids[1]];
        var dist = Math.hypot(a.x - b.x, a.y - b.y);
        if (pinchDist > 0) zoomTo(pinchScale * (dist / pinchDist), (a.x + b.x) / 2, (a.y + b.y) / 2, false);
        moved = true;
      } else if (e.pointerId === dragId && scale > 1.01) {
        tx = dragTx + (e.clientX - dragX); ty = dragTy + (e.clientY - dragY);
        if (Math.abs(e.clientX - dragX) + Math.abs(e.clientY - dragY) > 4) moved = true;
        clampPan(); apply(false);
      }
    });
    var endPointer = function (e) {
      delete pointers[e.pointerId];
      if (Object.keys(pointers).length < 2) pinchDist = 0;
      if (e.pointerId === dragId) dragId = null;
    };
    img.addEventListener('pointerup', endPointer);
    img.addEventListener('pointercancel', endPointer);
  })();

  // ---- detail media: fit to viewport + "expand full size" ----
  (function () {
    var media = document.querySelector('.image-wrapper img.zoomable, .image-wrapper video.detail-video');
    if (!media) return;
    var wrap = media.closest('.image-wrapper');
    var btn = document.getElementById('expand-full');
    var vtitle = wrap && wrap.querySelector('.media-title-v');
    var expanded = false;

    // Cap the media's height so it + the caption/expand link fit on screen without
    // scrolling. Measured against the live layout, so it's exact regardless of the
    // header/chrome height.
    var fit = function () {
      if (expanded) { media.style.maxHeight = ''; return; }
      media.style.maxHeight = '';                       // reset before measuring
      var below = 0;
      for (var s = media.nextElementSibling; s; s = s.nextElementSibling) {
        var cs = getComputedStyle(s);
        if (cs.display === 'none') continue;
        below += s.offsetHeight + parseFloat(cs.marginTop || 0) + parseFloat(cs.marginBottom || 0);
      }
      var docTop = media.getBoundingClientRect().top + window.scrollY; // fixed chrome height above the media
      var avail = window.innerHeight - docTop - below - 16;
      if (avail > 160) media.style.maxHeight = avail + 'px';
    };

    // Show "expand full size" only when the source is larger than what's shown.
    var syncBtn = function () {
      if (!btn) return;
      if (expanded) { btn.hidden = false; return; }
      var bigger = media.naturalWidth > media.clientWidth + 4 || media.naturalHeight > media.clientHeight + 4;
      btn.hidden = !(media.clientWidth && bigger);
    };

    // Match the vertical title strip's height to the media so its border runs the
    // full side of the image (and a long title clips rather than stretching it).
    var syncTitle = function () {
      if (!vtitle) return;
      var vertical = getComputedStyle(vtitle).writingMode.indexOf('vertical') === 0;
      vtitle.style.height = vertical ? media.clientHeight + 'px' : '';
    };

    var refresh = function () { fit(); syncBtn(); fit(); syncTitle(); }; // 2nd pass accounts for the button's height

    if (media.tagName === 'IMG' && !(media.complete && media.naturalWidth)) {
      media.addEventListener('load', refresh);
    }
    if (media.tagName === 'VIDEO') media.addEventListener('loadedmetadata', refresh);
    refresh();
    window.addEventListener('resize', refresh);

    if (btn) {
      btn.addEventListener('click', function () {
        expanded = !expanded;
        media.classList.toggle('expanded', expanded);   // full natural size on the page
        wrap.classList.toggle('is-expanded', expanded);
        btn.textContent = expanded ? '[ fit to screen ]' : '[ expand full size ]';
        if (expanded) { media.style.maxHeight = ''; window.scrollTo({ top: 0 }); } else refresh();
      });
    }
  })();

  // ---- shortcuts sheet + global keys (?, g, d, esc) ----
  (function () {
    var sheet = document.getElementById('shortcuts');
    document.addEventListener('keydown', function (e) {
      if (typing(e.target) || e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === '?') { if (sheet) sheet.classList.toggle('open'); }
      else if (e.key === 'Escape') { if (sheet) sheet.classList.remove('open'); }
      else if (e.key === 'g' || e.key === 'G') { window.location.href = '/'; }
      else if (e.key === 'd' || e.key === 'D') { if (window.__toggleTheme) window.__toggleTheme(); }
    });
    if (sheet) sheet.addEventListener('click', function (e) { if (e.target === sheet) sheet.classList.remove('open'); });
  })();

  // ---- board keyboard nav (j/k/arrows + enter) ----
  (function () {
    var grid = document.getElementById('grid');
    if (!grid) return;
    var idx = -1;
    var cards = function () { return Array.prototype.slice.call(grid.querySelectorAll('.card')); };
    var focus = function (n) {
      var list = cards();
      if (!list.length) return;
      if (idx >= 0 && list[idx]) list[idx].classList.remove('kbd-focus');
      idx = Math.max(0, Math.min(n, list.length - 1));
      list[idx].classList.add('kbd-focus');
      list[idx].scrollIntoView({ block: 'center', behavior: 'smooth' });
    };
    document.addEventListener('keydown', function (e) {
      if (typing(e.target) || e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === 'j' || e.key === 'J' || e.key === 'ArrowDown') { e.preventDefault(); focus(idx + 1); }
      else if (e.key === 'k' || e.key === 'K' || e.key === 'ArrowUp') { e.preventDefault(); focus(idx - 1); }
      else if (e.key === 'Enter' && idx >= 0) { var l = cards()[idx]; if (l) l.click(); }
    });
  })();

  // ---- infinite scroll ----
  (function () {
    var sentinel = document.getElementById('board-more');
    var grid = document.getElementById('grid');
    if (!sentinel || !grid) return;
    var page = parseInt(sentinel.dataset.page || '48', 10);
    var offset = parseInt(sentinel.dataset.offset || '0', 10);
    var cat = sentinel.dataset.cat || '', tag = sentinel.dataset.tag || '';
    var done = offset < page, loading = false;
    var load = function () {
      if (loading || done) return;
      loading = true;
      var q = '/board/more?offset=' + offset + (cat ? '&cat=' + encodeURIComponent(cat) : '') + (tag ? '&tag=' + encodeURIComponent(tag) : '');
      fetch(q).then(function (r) { return r.text(); }).then(function (html) {
        var tmp = document.createElement('div');
        tmp.innerHTML = html;
        var added = tmp.querySelectorAll('.card');
        added.forEach(function (card) {
          var img = card.querySelector('.card-img-container img');
          if (img) { blurUp(img); observeEager(img); }
          var vt = card.querySelector('[data-vt]'); if (vt) { try { vt.style.viewTransitionName = vt.getAttribute('data-vt'); } catch (e) {} }
          card.classList.add('visible');
          masonryAppend(grid, card);
        });
        offset += added.length;
        if (added.length < page) done = true;
        loading = false;
      }).catch(function () { loading = false; });
    };
    // Load the next batch well ahead (~1.5k px) so cards exist before the eager
    // image observer reaches for them — keeps thumbnails from popping in on scroll.
    new IntersectionObserver(function (en) { en.forEach(function (x) { if (x.isIntersecting) load(); }); }, { rootMargin: '1500px' }).observe(sentinel);
  })();

  // ---- colophon reveal (past the end) ----
  (function () {
    var col = document.getElementById('colophon');
    if (!col) return;
    var io = new IntersectionObserver(function (en) {
      en.forEach(function (x) { if (x.isIntersecting) { col.classList.add('revealed'); io.disconnect(); } });
    }, { threshold: 0.2 });
    io.observe(col);
  })();

  // ---- related items reveal (staggered, on scroll into view) ----
  (function () {
    var rel = document.querySelector('.related');
    if (!rel) return;
    var io = new IntersectionObserver(function (en) {
      en.forEach(function (x) { if (x.isIntersecting) { rel.classList.add('revealed'); io.disconnect(); } });
    }, { threshold: 0.15 });
    io.observe(rel);
  })();

  // ---- translate text blocks in place ----
  (function () {
    var blocks = document.querySelectorAll('.quote, .note-content, .detail-text');
    if (!blocks.length) return;
    var target = (navigator.language || 'en').slice(0, 2);
    blocks.forEach(function (block) {
      var btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'translate-btn';
      btn.textContent = 'Translate';
      block.insertAdjacentElement('afterend', btn);
      var orig = null, trans = null, showing = 'orig';
      btn.addEventListener('click', function () {
        if (trans != null) {
          if (showing === 'orig') { block.textContent = trans; showing = 'trans'; btn.textContent = 'Show original'; }
          else { block.textContent = orig; showing = 'orig'; btn.textContent = 'Show translation'; }
          return;
        }
        orig = block.textContent;
        btn.disabled = true; btn.textContent = 'Translating…';
        fetch('/api/translate', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ text: orig, target: target })
        }).then(function (r) { return r.ok ? r.json() : Promise.reject(); })
          .then(function (d) {
            trans = d.text || orig;
            block.textContent = trans; showing = 'trans';
            btn.disabled = false; btn.textContent = 'Show original';
          })
          .catch(function () {
            btn.disabled = false; btn.textContent = 'Translate failed';
            setTimeout(function () { btn.textContent = 'Translate'; }, 2200);
          });
      });
    });
  })();

  // ===== easter eggs =====

  // console colophon
  try {
    console.log('%cDO NOT TOUCH THE GLASS', 'font:600 22px sans-serif;color:#111;background:#fff;padding:6px 10px;');
    console.log('%cyou found the back of the glass. — D.T.', 'color:#888;font-style:italic');
  } catch (e) {}

  // touch the glass — a smudge + ripple where you press an open image
  (function () {
    var smudge = function (x, y) {
      var el = document.createElement('div');
      el.className = 'smudge';
      el.style.left = x + 'px';
      el.style.top = y + 'px';
      el.innerHTML = '<span class="ring"></span><span class="word">Do not touch the glass</span>';
      document.body.appendChild(el);
      setTimeout(function () { el.remove(); }, 1700);
    };
    document.addEventListener('pointerdown', function (e) {
      var t = e.target;
      if (t && t.tagName === 'IMG' && t.closest('.image-wrapper, .lightbox')) smudge(e.clientX, e.clientY);
    });
  })();

  // lights out — Konami code flips dark mode with a flash
  (function () {
    var seq = [38, 38, 40, 40, 37, 39, 37, 39, 66, 65], pos = 0;
    document.addEventListener('keydown', function (e) {
      pos = (e.keyCode === seq[pos]) ? pos + 1 : (e.keyCode === seq[0] ? 1 : 0);
      if (pos === seq.length) {
        pos = 0;
        document.body.classList.add('flash-invert');
        setTimeout(function () { document.body.classList.remove('flash-invert'); }, 550);
        if (window.__toggleTheme) window.__toggleTheme();
      }
    });
  })();

  // datamosh the wordmark on a triple-click
  (function () {
    var mark = document.querySelector('.header-left + div');
    if (!mark) return;
    var n = 0, t;
    mark.style.cursor = 'pointer';
    mark.addEventListener('click', function () {
      n++; clearTimeout(t); t = setTimeout(function () { n = 0; }, 500);
      if (n >= 3) { n = 0; mark.classList.remove('datamosh'); void mark.offsetWidth; mark.classList.add('datamosh'); }
    });
  })();

})();
