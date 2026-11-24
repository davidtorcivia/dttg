(function () {
  'use strict';

  // ---- service worker (PWA installability + Android share target) ----
  if ('serviceWorker' in navigator) {
    window.addEventListener('load', function () {
      navigator.serviceWorker.register('/sw.js').catch(function () {});
    });
  }

  // ---- helpers ----
  var typing = function (el) { return el && el.matches && el.matches('input, textarea, select, [contenteditable]'); };
  var pad2 = function (n) { return (n < 10 ? '0' : '') + n; };
  var parseGallery = function (el) {
    if (!el) return null;
    var raw = el.getAttribute('data-gallery');
    if (!raw) return null;
    try {
      var list = JSON.parse(raw);
      return Array.isArray(list) && list.length > 1 ? list : null;
    } catch (e) { return null; }
  };
  var preloadURL = (function () {
    var seen = {};
    return function (url) {
      if (!url || seen[url]) return;
      seen[url] = 1;
      var i = new Image();
      i.src = url;
    };
  })();

  // ---- row-major masonry ----
  var mq = window.matchMedia ? {
    small: window.matchMedia('(max-width: 600px)'),
    med:   window.matchMedia('(max-width: 1000px)'),
    wide:  window.matchMedia('(min-width: 1600px)')
  } : null;
  function gridColumnCount(maxCols) {
    if (mq) {
      if (mq.small.matches) return 1;
      if (mq.med.matches) return 2;
      if (!mq.wide.matches) return Math.min(3, maxCols);
      return maxCols;
    }
    var w = window.innerWidth;
    if (w <= 600) return 1;
    if (w <= 1000) return 2;
    if (w < 1600) return Math.min(3, maxCols);
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
    if (!force && grid.__n === n) return;
    if (!grid.__order) grid.__order = Array.prototype.slice.call(grid.querySelectorAll('.card'));
    var cards = grid.__order;
    grid.textContent = '';
    grid.classList.add('grid--cols');
    var cols = [], heights = [];
    for (var i = 0; i < n; i++) {
      var c = document.createElement('div');
      c.className = 'grid-col';
      grid.appendChild(c);
      cols.push(c);
      heights.push(0);
    }
    cards.forEach(function (card) {
      var m = shortestCol(heights);
      cols[m].appendChild(card);
      heights[m] = cols[m].offsetHeight;
    });
    grid.__cols = cols;
    grid.__heights = heights;
    grid.__n = n;
  }
  function masonryAppend(grid, card) {
    var cols = grid.__cols, heights = grid.__heights;
    if (grid.__order) grid.__order.push(card);
    if (!cols || !cols.length) { grid.appendChild(card); return; }
    var m = shortestCol(heights);
    cols[m].appendChild(card);
    heights[m] = cols[m].offsetHeight;
  }

  // Blur-up: show tiny placeholder behind a thumbnail while it loads, then clear
  // on load (or error so a broken image doesn't keep stale state).
  function blurUp(img) {
    var c = img && img.parentElement;
    if (!c) return;
    var clear = function () { c.style.backgroundImage = 'none'; c.style.backgroundColor = 'transparent'; };
    if (img.complete && img.naturalWidth) { clear(); return; }
    var ph = img.getAttribute('data-ph');
    if (ph) {
      c.style.backgroundImage = 'url("' + ph + '")';
      c.style.backgroundSize = 'cover';
      c.style.backgroundPosition = 'center';
    }
    img.addEventListener('load', clear, { once: true });
    img.addEventListener('error', clear, { once: true });
  }

  var eagerIO = ('IntersectionObserver' in window) ? new IntersectionObserver(function (entries) {
    entries.forEach(function (e) {
      if (!e.isIntersecting) return;
      e.target.loading = 'eager';
      eagerIO.unobserve(e.target);
    });
  }, { rootMargin: '0px 0px 200% 0px' }) : null;
  function observeEager(img) {
    if (eagerIO && img && img.getAttribute('loading') === 'lazy' && !(img.complete && img.naturalWidth)) {
      eagerIO.observe(img);
    }
  }

  // Multi-image card quick-peek: cycle through a post's series right on the board
  // via the hover micro-arrows without navigating away.
  function initCardPeek(card) {
    if (!card || card.__peekInit) return;
    var slides = parseGallery(card);
    if (!slides) return;
    card.__peekInit = true;
    var img = card.querySelector('.card-img-container > img');
    var countEl = card.querySelector('.card-series-count');
    var dots = card.querySelectorAll('.card-peek-dots i');
    var idx = 0;
    if (!img) return;

    var show = function (next) {
      idx = (next + slides.length) % slides.length;
      var s = slides[idx];
      img.classList.add('is-swapping');
      img.removeAttribute('srcset');
      img.removeAttribute('sizes');
      var done = function () { img.classList.remove('is-swapping'); };
      img.addEventListener('load', done, { once: true });
      img.addEventListener('error', done, { once: true });
      img.src = s.thumb || s.full;
      if (img.complete && img.naturalWidth) done();
      if (countEl) countEl.textContent = (idx + 1) + '/' + slides.length;
      dots.forEach(function (d, i) { d.classList.toggle('is-active', i === idx); });
      preloadURL(slides[(idx + 1) % slides.length].thumb || slides[(idx + 1) % slides.length].full);
    };

    card.querySelectorAll('.card-peek-btn').forEach(function (btn) {
      btn.addEventListener('click', function (e) {
        e.preventDefault();
        e.stopPropagation();
        var delta = parseInt(btn.getAttribute('data-peek') || '1', 10);
        show(idx + delta);
      });
    });
  }

  // Hydrate a card rendered after load (infinite scroll, live search).
  function hydrateCard(card) {
    var img = card.querySelector('.card-img-container img');
    if (img) { blurUp(img); observeEager(img); }
    var vt = card.querySelector('[data-vt]');
    if (vt) { try { vt.style.viewTransitionName = vt.getAttribute('data-vt'); } catch (e) {} }
    initCardPeek(card);
    card.classList.add('visible');
  }

  // Focus trap & modal page lock helpers
  function focusableEls(el) {
    return [].slice.call(el.querySelectorAll(
      'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
    )).filter(function (n) { return n.offsetWidth > 0 || n.offsetHeight > 0 || n === document.activeElement; });
  }
  function trapFocus(container) {
    var handler = function (e) {
      if (e.key !== 'Tab') return;
      var f = focusableEls(container);
      if (!f.length) return;
      var first = f[0], last = f[f.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    };
    container.addEventListener('keydown', handler);
    return function () { container.removeEventListener('keydown', handler); };
  }

  var pageLockPrev = '';
  var pageLockDepth = 0;
  var pageChrome = function () {
    return document.querySelectorAll('body > :not(.search-overlay):not(.shortcuts):not(.lightbox):not(.cursor):not(.loupe):not(.smudge)');
  };
  function lockPage() {
    if (pageLockDepth === 0) {
      pageLockPrev = document.body.style.overflow || '';
      document.body.style.overflow = 'hidden';
      pageChrome().forEach(function (el) {
        if ('inert' in el) el.inert = true;
      });
    }
    pageLockDepth++;
  }
  function unlockPage() {
    if (pageLockDepth === 0) return;
    pageLockDepth--;
    if (pageLockDepth === 0) {
      document.body.style.overflow = pageLockPrev;
      pageChrome().forEach(function (el) {
        if ('inert' in el) el.inert = false;
      });
    }
  }
  function anyModalOpen() {
    return !!(
      document.querySelector('.search-overlay.open') ||
      document.querySelector('.shortcuts.open') ||
      document.querySelector('.lightbox.open')
    );
  }

  (function () {
    var grid = document.getElementById('grid');
    if (!grid) return;
    var relayout = function () { layoutMasonry(grid); };
    layoutMasonry(grid, true);
    if (mq) {
      [mq.small, mq.med, mq.wide].forEach(function (m) {
        if (m.addEventListener) m.addEventListener('change', relayout);
        else if (m.addListener) m.addListener(relayout);
      });
    }
    window.addEventListener('load', function () { layoutMasonry(grid, true); });
    window.addEventListener('pageshow', function (e) { if (e.persisted) layoutMasonry(grid, true); });
    var rt;
    window.addEventListener('resize', function () {
      clearTimeout(rt);
      rt = setTimeout(relayout, 150);
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
      cursor.classList.toggle('cursor-hidden', !!e.target.closest('input, textarea, select, [contenteditable]'));
    });
  }

  // ---- loupe: magnifier lens over zoomable images (fine pointers) ----
  var hideLoupe = function () {};
  if (window.matchMedia('(pointer: fine)').matches) {
    var SIZE = 190, ZOOM = 2.2, loupe = null, blendCursor = null;
    hideLoupe = function () {
      if (loupe) loupe.classList.remove('on');
      if (blendCursor) blendCursor.style.opacity = '';
    };
    document.addEventListener('mousemove', function (e) {
      var img = e.target.closest ? e.target.closest('img.zoomable, #lightbox-img') : null;
      if (!img || !img.complete || !img.naturalWidth || img.classList.contains('lb-zoomed')) {
        hideLoupe();
        return;
      }
      if (!loupe) {
        loupe = document.createElement('div');
        loupe.className = 'loupe';
        loupe.setAttribute('aria-hidden', 'true');
        document.body.appendChild(loupe);
        blendCursor = document.querySelector('.cursor');
      }
      var r = img.getBoundingClientRect();
      var src = img.getAttribute('data-full') || img.currentSrc || img.src;
      if (loupe.__src !== src) { loupe.style.backgroundImage = 'url("' + src + '")'; loupe.__src = src; }
      var x = e.clientX - r.left, y = e.clientY - r.top;
      loupe.style.backgroundSize = (r.width * ZOOM) + 'px ' + (r.height * ZOOM) + 'px';
      loupe.style.backgroundPosition = (SIZE / 2 - x * ZOOM) + 'px ' + (SIZE / 2 - y * ZOOM) + 'px';
      loupe.style.left = e.clientX + 'px';
      loupe.style.top = e.clientY + 'px';
      loupe.classList.add('on');
      if (blendCursor) blendCursor.style.opacity = '0';
    });
  }

  // ---- search overlay + live results + keyboard arrow nav ----
  (function () {
    var toggle = document.getElementById('search-toggle');
    var overlay = document.getElementById('search-overlay');
    var input = document.getElementById('search-input');
    var close = document.getElementById('search-close');
    var results = document.getElementById('search-results');
    var status = document.getElementById('search-status');
    if (!overlay) return;

    var lastGood = '', activeIdx = -1;
    var isOpen = function () { return overlay.classList.contains('open'); };
    var setStatus = function (msg) { if (status) status.textContent = msg || ''; };
    var lastFocus = null, untrap = null;
    var setActive = function (next) {
      if (!results) return;
      var items = results.querySelectorAll('.search-result');
      if (!items.length) { activeIdx = -1; return; }
      if (activeIdx >= 0 && items[activeIdx]) items[activeIdx].classList.remove('is-active');
      activeIdx = (next + items.length) % items.length;
      items[activeIdx].classList.add('is-active');
      items[activeIdx].scrollIntoView({ block: 'nearest' });
    };
    var open = function (e) {
      if (e) e.preventDefault();
      if (isOpen()) return;
      lastFocus = document.activeElement;
      overlay.classList.add('open');
      if (toggle) toggle.setAttribute('aria-expanded', 'true');
      lockPage();
      untrap = trapFocus(overlay);
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
    var hide = function () {
      if (!isOpen()) return;
      overlay.classList.remove('open');
      if (toggle) toggle.setAttribute('aria-expanded', 'false');
      if (untrap) { untrap(); untrap = null; }
      unlockPage();
      if (lastFocus && lastFocus.focus) lastFocus.focus();
    };

    if (toggle) toggle.addEventListener('click', open);
    if (close) close.addEventListener('click', hide);
    overlay.addEventListener('click', function (e) { if (e.target === overlay) hide(); });
    document.addEventListener('keydown', function (e) {
      if (e.key === 'Escape' && isOpen()) { hide(); return; }
      if (e.key === '/' && !anyModalOpen() && !typing(e.target) && !e.metaKey && !e.ctrlKey && !e.altKey) {
        open(e);
      }
    });
    if (input) {
      input.addEventListener('keydown', function (e) {
        if (!results) return;
        var items = results.querySelectorAll('.search-result');
        if (!items.length) return;
        if (e.key === 'ArrowDown') { e.preventDefault(); setActive(activeIdx + 1); }
        else if (e.key === 'ArrowUp') { e.preventDefault(); setActive(activeIdx <= 0 ? items.length - 1 : activeIdx - 1); }
        else if (e.key === 'Enter' && activeIdx >= 0 && items[activeIdx]) {
          e.preventDefault();
          window.location.href = items[activeIdx].getAttribute('href');
        }
      });
    }

    var esc = function (s) {
      return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
        return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
      });
    };
    var render = function (items) {
      if (!results) return;
      activeIdx = -1;
      if (!items.length) {
        results.innerHTML = '<div class="search-empty">No matches</div>';
        lastGood = results.innerHTML;
        setStatus('No matches');
        return;
      }
      results.innerHTML = items.map(function (it) {
        var media = it.thumb
          ? '<img src="' + esc(it.thumb) + '" alt="' + esc(it.title || 'untitled') + '">'
          : '<span class="sr-tile">' + esc((it.kind || '').slice(0, 3).toUpperCase()) + '</span>';
        var meta = [it.category, it.date].filter(Boolean).map(esc).join(' · ');
        return '<a class="search-result" href="' + esc(it.url) + '">' + media +
          '<span class="sr-main"><span class="sr-title">' + esc(it.title || '(untitled)') +
          '</span><span class="sr-meta">' + meta + '</span></span></a>';
      }).join('');
      lastGood = results.innerHTML;
      setStatus(items.length + ' result' + (items.length === 1 ? '' : 's') + ' · ↑↓ to select');
    };
    var showError = function () {
      if (!results) return;
      if (lastGood) results.innerHTML = lastGood;
      else results.innerHTML = '<div class="search-empty">Couldn’t load results — try again</div>';
      setStatus('Couldn’t load results — try again');
    };
    var timer;
    if (input && results) {
      input.addEventListener('input', function () {
        clearTimeout(timer);
        var q = input.value.trim();
        if (!q) { results.innerHTML = ''; lastGood = ''; activeIdx = -1; setStatus(''); return; }
        timer = setTimeout(function () {
          fetch('/api/search?q=' + encodeURIComponent(q))
            .then(function (r) {
              if (!r.ok) throw new Error('search ' + r.status);
              return r.json();
            })
            .then(function (d) { render(d.results || []); })
            .catch(function () { showError(); });
        }, 180);
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
    var lastGoodHTML = grid.innerHTML;
    var lastGoodMeta = meta ? meta.textContent : '';
    var revive = function () { grid.querySelectorAll('.card').forEach(hydrateCard); };
    var skeleton = function () {
      grid.className = 'grid';
      grid.textContent = '';
      var sheet = document.createElement('div');
      sheet.className = 'sheet';
      for (var i = 0; i < 8; i++) sheet.appendChild(document.createElement('span'));
      grid.appendChild(sheet);
    };
    var timer;
    var run = function () {
      var q = input.value.trim();
      if (!q) {
        grid.className = 'grid';
        grid.textContent = '';
        grid.__order = null;
        lastGoodHTML = '';
        lastGoodMeta = '';
        if (meta) meta.textContent = '';
        return;
      }
      skeleton();
      fetch('/search/cards?q=' + encodeURIComponent(q))
        .then(function (r) {
          if (!r.ok) throw new Error('search cards ' + r.status);
          return r.text();
        })
        .then(function (html) {
          grid.innerHTML = html;
          grid.__order = null;
          revive();
          layoutMasonry(grid, true);
          var n = grid.querySelectorAll('.card').length;
          var msg = n + ' result' + (n === 1 ? '' : 's') + ' for “' + q + '”';
          if (meta) meta.textContent = msg;
          lastGoodHTML = grid.innerHTML;
          lastGoodMeta = msg;
        }).catch(function () {
          if (lastGoodHTML) {
            grid.innerHTML = lastGoodHTML;
            grid.__order = null;
            revive();
            layoutMasonry(grid, true);
            if (meta) meta.textContent = lastGoodMeta || 'Couldn’t load results — try again';
          } else {
            grid.className = 'grid';
            grid.innerHTML = '<p class="empty">Couldn’t load results — try again</p>';
            if (meta) meta.textContent = 'Couldn’t load results — try again';
          }
        });
    };
    input.addEventListener('input', function () { clearTimeout(timer); timer = setTimeout(run, 200); });
  })();

  // ---- detail video: custom control bar ----
  (function () {
    var SVG = {
      play:  '<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"><path d="M4 3l9 5-9 5z" fill="currentColor"/></svg>',
      pause: '<svg viewBox="0 0 16 16" width="14" height="14" aria-hidden="true"><rect x="4" y="3" width="3" height="10" fill="currentColor"/><rect x="9" y="3" width="3" height="10" fill="currentColor"/></svg>',
      sound: '<svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true"><path d="M2 6h3l4-3v10L5 10H2z" fill="currentColor"/><path d="M11 5.4a3 3 0 0 1 0 5.2" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>',
      muted: '<svg viewBox="0 0 16 16" width="16" height="16" aria-hidden="true"><path d="M2 6h3l4-3v10L5 10H2z" fill="currentColor"/><path d="M11 6l3.2 4M14.2 6L11 10" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"/></svg>',
      full:  '<svg viewBox="0 0 16 16" width="15" height="15" aria-hidden="true"><path d="M2 6V2h4M14 6V2h-4M2 10v4h4M14 10v4h-4" fill="none" stroke="currentColor" stroke-width="1.4"/></svg>',
      exit:  '<svg viewBox="0 0 16 16" width="15" height="15" aria-hidden="true"><path d="M6 2v4H2M10 2v4h4M6 14v-4H2M10 14v-4h4" fill="none" stroke="currentColor" stroke-width="1.4"/></svg>'
    };
    var fmt = function (t) {
      if (!isFinite(t) || t < 0) t = 0;
      var m = Math.floor(t / 60), s = Math.floor(t % 60);
      return m + ':' + (s < 10 ? '0' : '') + s;
    };
    document.querySelectorAll('video.detail-video').forEach(function (v) {
      var stage = v.parentElement;
      if (!stage) return;
      v.tabIndex = 0;
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
        else if (v.webkitEnterFullscreen) { v.webkitEnterFullscreen(); }
      };

      playBtn.addEventListener('click', togglePlay);
      muteBtn.addEventListener('click', function () { v.muted = !v.muted; });
      fullBtn.addEventListener('click', toggleFull);
      v.addEventListener('play', syncPlay);
      v.addEventListener('pause', syncPlay);
      v.addEventListener('volumechange', syncMute);
      document.addEventListener('fullscreenchange', syncFull);
      var lastLabel = '';
      var sync = function () {
        var d = v.duration || 0, frac = d ? v.currentTime / d : 0;
        fill.style.transform = 'scaleX(' + frac + ')';
        var label = fmt(v.currentTime) + (d ? ' / ' + fmt(d) : '');
        if (label !== lastLabel) {
          lastLabel = label;
          timeEl.textContent = label;
          track.setAttribute('aria-valuenow', Math.round(frac * 100));
        }
      };
      var raf = null;
      var loop = function () { sync(); raf = requestAnimationFrame(loop); };
      var startRAF = function () { if (raf === null) raf = requestAnimationFrame(loop); };
      var stopRAF = function () { if (raf !== null) { cancelAnimationFrame(raf); raf = null; } sync(); };
      v.addEventListener('play', startRAF);
      v.addEventListener('playing', startRAF);
      v.addEventListener('pause', stopRAF);
      v.addEventListener('ended', stopRAF);
      v.addEventListener('timeupdate', sync);
      v.addEventListener('seeked', sync);
      if (!v.paused) startRAF();
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

  // ---- detail multi-image gallery + viewport fit + lightbox (shared state) ----
  var detailGallery = null;
  (function () {
    var stage = document.getElementById('detail-gallery');
    var media = document.querySelector('.image-wrapper img.zoomable, .image-wrapper video.detail-video');
    var slides = parseGallery(stage);
    var wrap = media && media.closest('.image-wrapper');
    var expandBtn = document.getElementById('expand-full');
    var vtitle = wrap && wrap.querySelector('.media-title-v');
    var expanded = false;

    // Fit media within viewport so it + controls + filmstrip sit comfortably on screen.
    var fit = function () {
      if (!media) return;
      if (expanded) { media.style.maxHeight = ''; return; }
      media.style.maxHeight = '';
      var below = 0;
      var col = media.closest('.media-col');
      var anchor = col ? media.closest('.media-stage') : media;
      for (var s = anchor && anchor.nextElementSibling; s; s = s.nextElementSibling) {
        if (s.hidden) continue;
        var cs = getComputedStyle(s);
        if (cs.display === 'none') continue;
        below += s.offsetHeight + parseFloat(cs.marginTop || 0) + parseFloat(cs.marginBottom || 0);
      }
      var docTop = media.getBoundingClientRect().top + window.scrollY;
      var avail = window.innerHeight - docTop - below - 20;
      if (avail > 180) media.style.maxHeight = avail + 'px';
    };

    var syncBtn = function () {
      if (!expandBtn || !media) return;
      if (expanded) { expandBtn.hidden = false; return; }
      var fullW = parseInt(media.getAttribute('width'), 10) || media.naturalWidth;
      var fullH = parseInt(media.getAttribute('height'), 10) || media.naturalHeight;
      var cw = media.clientWidth, ch = media.clientHeight;
      var bigger = cw > 0 && (fullW > cw * 1.1 || fullH > ch * 1.1);
      expandBtn.hidden = !bigger;
    };

    var syncTitle = function () {
      if (!vtitle || !media) return;
      var vertical = getComputedStyle(vtitle).writingMode.indexOf('vertical') === 0;
      vtitle.style.height = vertical ? media.clientHeight + 'px' : '';
    };

    var refreshFit = function () {
      if (!media) return;
      fit();
      syncBtn();
      fit();
      syncTitle();
    };

    if (media) {
      if (media.tagName === 'IMG') {
        media.addEventListener('load', refreshFit);
      }
      if (media.tagName === 'VIDEO') media.addEventListener('loadedmetadata', refreshFit);
      refreshFit();
      requestAnimationFrame(refreshFit);
      window.addEventListener('load', refreshFit);
      window.addEventListener('pageshow', function (e) { if (e.persisted) refreshFit(); });
      var rt;
      window.addEventListener('resize', function () { clearTimeout(rt); rt = setTimeout(refreshFit, 150); });
    }

    if (expandBtn && media) {
      expandBtn.addEventListener('click', function () {
        expanded = !expanded;
        if (expanded) {
          media.removeAttribute('srcset');
          media.removeAttribute('sizes');
        }
        media.classList.toggle('expanded', expanded);
        if (wrap) wrap.classList.toggle('is-expanded', expanded);
        expandBtn.textContent = expanded ? '[ fit to screen ]' : '[ expand full size ]';
        if (expanded) {
          media.style.maxHeight = '';
          syncTitle();
          window.scrollTo({ top: 0 });
        } else {
          refreshFit();
        }
      });
    }

    // Multi-image gallery controller
    var curIdx = 0;
    var listeners = [];
    if (stage && slides && media && media.tagName === 'IMG') {
      var curEl = stage.querySelector('.gallery-cur');
      var prevBtn = document.getElementById('gallery-prev');
      var nextBtn = document.getElementById('gallery-next');
      var altEl = document.getElementById('gallery-alt');
      var strip = document.getElementById('gallery-strip');
      var thumbs = strip ? Array.prototype.slice.call(strip.querySelectorAll('.gallery-thumb')) : [];
      var baseAlt = media.getAttribute('alt') || '';

      var prefetchAdjacent = function (i) {
        preloadURL(slides[(i + 1) % slides.length].full);
        preloadURL(slides[(i - 1 + slides.length) % slides.length].full);
      };

      var go = function (next) {
        curIdx = ((next % slides.length) + slides.length) % slides.length;
        var s = slides[curIdx];
        hideLoupe();
        media.classList.add('is-swapping');
        media.removeAttribute('srcset');
        media.removeAttribute('sizes');
        var done = function () {
          media.classList.remove('is-swapping');
          refreshFit();
        };
        media.addEventListener('load', done, { once: true });
        media.addEventListener('error', done, { once: true });
        media.setAttribute('data-full', s.full);
        if (s.w && s.h) {
          media.setAttribute('width', String(s.w));
          media.setAttribute('height', String(s.h));
        } else {
          media.removeAttribute('width');
          media.removeAttribute('height');
        }
        media.alt = s.alt || baseAlt;
        media.src = s.full;
        if (media.complete && media.naturalWidth) done();

        if (curEl) curEl.textContent = pad2(curIdx + 1);
        if (altEl) {
          altEl.textContent = s.alt || '';
          altEl.hidden = !s.alt;
        }
        thumbs.forEach(function (tb, i) {
          var active = i === curIdx;
          tb.classList.toggle('is-active', active);
          tb.setAttribute('aria-selected', active ? 'true' : 'false');
          if (active && tb.scrollIntoView) {
            tb.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'center' });
          }
        });
        prefetchAdjacent(curIdx);
        listeners.forEach(function (fn) { fn(curIdx, s); });
      };

      if (prevBtn) prevBtn.addEventListener('click', function (e) { e.stopPropagation(); go(curIdx - 1); });
      if (nextBtn) nextBtn.addEventListener('click', function (e) { e.stopPropagation(); go(curIdx + 1); });
      thumbs.forEach(function (tb) {
        tb.addEventListener('click', function () {
          go(parseInt(tb.getAttribute('data-slide') || '0', 10));
        });
      });

      // Touch/pointer horizontal swipe on the stage
      var sx = 0, sy = 0, swiping = false;
      stage.addEventListener('touchstart', function (e) {
        if (e.touches.length !== 1) return;
        sx = e.touches[0].clientX;
        sy = e.touches[0].clientY;
        swiping = true;
      }, { passive: true });
      stage.addEventListener('touchend', function (e) {
        if (!swiping || !e.changedTouches.length) return;
        swiping = false;
        var dx = e.changedTouches[0].clientX - sx;
        var dy = e.changedTouches[0].clientY - sy;
        if (Math.abs(dx) > 42 && Math.abs(dx) > Math.abs(dy) * 1.2) {
          go(curIdx + (dx < 0 ? 1 : -1));
        }
      }, { passive: true });

      prefetchAdjacent(0);
      detailGallery = {
        slides: slides,
        index: function () { return curIdx; },
        go: go,
        onChange: function (fn) { listeners.push(fn); }
      };
    }

    // ---- lightbox (with zoom + pan + multi-image cycling) ----
    var box = document.getElementById('lightbox');
    var img = document.getElementById('lightbox-img');
    if (!box || !img) return;

    var MAX = 6, scale = 1, tx = 0, ty = 0;
    var pointers = {}, pinchDist = 0, pinchScale = 1, dragId = null, dragX = 0, dragY = 0, dragTx = 0, dragTy = 0, moved = false;
    var openLink = document.getElementById('lightbox-open');
    var closeBtn = document.getElementById('lightbox-close');
    var lbPrev = document.getElementById('lightbox-prev');
    var lbNext = document.getElementById('lightbox-next');
    var lbCounter = document.getElementById('lightbox-counter');
    var lbCaption = document.getElementById('lightbox-caption');
    var lbLast = null, lbUntrap = null;

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

    var syncLightboxSlide = function (i, s) {
      if (!box.classList.contains('open')) return;
      hideLoupe();
      reset(false);
      img.src = s.full;
      img.alt = s.alt || '';
      if (openLink) openLink.href = s.full;
      if (lbCounter && detailGallery) {
        lbCounter.textContent = pad2(i + 1) + ' / ' + pad2(detailGallery.slides.length);
        lbCounter.hidden = false;
      }
      if (lbCaption) {
        lbCaption.textContent = s.alt || '';
        lbCaption.hidden = !s.alt;
      }
    };
    if (detailGallery) detailGallery.onChange(syncLightboxSlide);

    var open = function (z) {
      hideLoupe();
      lbLast = document.activeElement;
      var full = z.getAttribute('data-full') || z.src;
      img.src = full;
      img.alt = z.getAttribute('alt') || '';
      if (openLink) {
        openLink.href = full;
        openLink.setAttribute('target', '_blank');
        openLink.setAttribute('rel', 'noopener');
      }
      var hasMulti = !!(detailGallery && detailGallery.slides.length > 1);
      if (lbPrev) lbPrev.hidden = !hasMulti;
      if (lbNext) lbNext.hidden = !hasMulti;
      if (lbCounter) {
        if (hasMulti) {
          lbCounter.textContent = pad2(detailGallery.index() + 1) + ' / ' + pad2(detailGallery.slides.length);
          lbCounter.hidden = false;
        } else {
          lbCounter.hidden = true;
        }
      }
      if (lbCaption) {
        var curSlide = hasMulti ? detailGallery.slides[detailGallery.index()] : null;
        var cap = curSlide && curSlide.alt ? curSlide.alt : '';
        lbCaption.textContent = cap;
        lbCaption.hidden = !cap;
      }
      scale = 1; tx = 0; ty = 0;
      img.classList.remove('lb-zoomed');
      img.style.transition = 'none';
      img.style.transform = 'translateY(20px) scale(.94)';
      box.classList.add('open');
      lockPage();
      box.focus();
      requestAnimationFrame(function () {
        img.style.transition = 'transform .5s var(--ease-out)';
        img.style.transform = 'translate(0px,0px) scale(1)';
      });
      lbUntrap = trapFocus(box);
    };
    var close = function () {
      if (!box.classList.contains('open')) return;
      hideLoupe();
      box.classList.remove('open');
      reset(false);
      if (lbUntrap) { lbUntrap(); lbUntrap = null; }
      unlockPage();
      if (lbLast && lbLast.focus) lbLast.focus();
    };

    document.querySelectorAll('.zoomable').forEach(function (z) {
      z.addEventListener('click', function () { open(z); });
    });
    box.addEventListener('click', function (e) { if (e.target === box) close(); });
    if (closeBtn) closeBtn.addEventListener('click', function (e) { e.preventDefault(); e.stopPropagation(); close(); });
    if (openLink) openLink.addEventListener('click', function (e) { e.stopPropagation(); });
    if (lbPrev) lbPrev.addEventListener('click', function (e) { e.stopPropagation(); if (detailGallery) detailGallery.go(detailGallery.index() - 1); });
    if (lbNext) lbNext.addEventListener('click', function (e) { e.stopPropagation(); if (detailGallery) detailGallery.go(detailGallery.index() + 1); });

    document.addEventListener('keydown', function (e) {
      if (!box.classList.contains('open')) return;
      if (e.key === 'Escape') close();
      else if (e.key === 'ArrowLeft' && detailGallery) { e.preventDefault(); detailGallery.go(detailGallery.index() - 1); }
      else if (e.key === 'ArrowRight' && detailGallery) { e.preventDefault(); detailGallery.go(detailGallery.index() + 1); }
      else if (detailGallery && e.key >= '1' && e.key <= '9') {
        var n = parseInt(e.key, 10) - 1;
        if (n < detailGallery.slides.length) { e.preventDefault(); detailGallery.go(n); }
      }
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
      e.stopPropagation();
      if (moved) return;
      if (scale > 1.01) reset(true); else zoomTo(2.5, e.clientX, e.clientY, true);
    });

    // Pointer-based pan (zoomed), swipe to cycle (unzoomed), and pinch zoom
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
      if (e.pointerId === dragId && scale <= 1.01 && detailGallery) {
        var dx = e.clientX - dragX, dy = e.clientY - dragY;
        if (Math.abs(dx) > 50 && Math.abs(dx) > Math.abs(dy) * 1.3) {
          moved = true;
          detailGallery.go(detailGallery.index() + (dx < 0 ? 1 : -1));
        }
      }
      delete pointers[e.pointerId];
      if (Object.keys(pointers).length < 2) pinchDist = 0;
      if (e.pointerId === dragId) dragId = null;
    };
    img.addEventListener('pointerup', endPointer);
    img.addEventListener('pointercancel', endPointer);
  })();

  // ---- detail keyboard nav (images in multi-image series + prev/next item) ----
  (function () {
    var prev = document.getElementById('nav-prev');
    var next = document.getElementById('nav-next');
    if (!prev && !next && !detailGallery) return;
    document.addEventListener('keydown', function (e) {
      if (typing(e.target) || e.metaKey || e.ctrlKey || e.altKey || anyModalOpen()) return;
      if (detailGallery) {
        if (e.key === 'ArrowLeft') {
          e.preventDefault();
          if (e.shiftKey && prev) { window.location.href = prev.href; return; }
          detailGallery.go(detailGallery.index() - 1);
          return;
        }
        if (e.key === 'ArrowRight') {
          e.preventDefault();
          if (e.shiftKey && next) { window.location.href = next.href; return; }
          detailGallery.go(detailGallery.index() + 1);
          return;
        }
        if (e.key >= '1' && e.key <= '9') {
          var n = parseInt(e.key, 10) - 1;
          if (n < detailGallery.slides.length) {
            e.preventDefault();
            detailGallery.go(n);
            return;
          }
        }
      } else {
        if (e.key === 'ArrowLeft' && prev) window.location.href = prev.href;
        if (e.key === 'ArrowRight' && next) window.location.href = next.href;
      }
    });
  })();

  // ---- bookmarklet builder ----
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

  // ---- confirm destructive forms (data-confirm; inline handlers are CSP-blocked) ----
  document.addEventListener('submit', function (e) {
    var msg = e.target.getAttribute && e.target.getAttribute('data-confirm');
    if (msg && !window.confirm(msg)) e.preventDefault();
  });

  // ---- themed file inputs + multi-file dropzone, live previews & pre-upload ordering ----
  (function () {
    var applyFileList = function (inp, list) {
      if (typeof DataTransfer === 'undefined') return;
      var dt = new DataTransfer();
      list.forEach(function (f) { dt.items.add(f); });
      inp.files = dt.files;
      renderFileLabel(inp);
    };
    var renderFileLabel = function (inp) {
      var host = inp.closest('.file-field, .dropzone');
      var label = host && host.querySelector('[data-file-name]');
      var files = inp.files ? Array.prototype.slice.call(inp.files) : [];
      if (label) {
        if (!files.length) label.textContent = host.hasAttribute('data-dropzone') ? 'Drop images here, paste, or browse' : 'No file chosen';
        else if (files.length === 1) label.textContent = files[0].name;
        else label.textContent = files.length + ' files selected (reorder below)';
      }
      var previews = inp.closest('.form-field') && inp.closest('.form-field').querySelector('[data-upload-previews]');
      if (!previews) return;
      previews.textContent = '';
      if (!files.length) { previews.hidden = true; return; }
      previews.hidden = false;
      files.forEach(function (f, i) {
        var tile = document.createElement('div');
        tile.className = 'upload-preview-item';
        var thumb = document.createElement('div');
        thumb.className = 'upload-preview-thumb';
        if (f.type && f.type.indexOf('image/') === 0 && window.URL && URL.createObjectURL) {
          var im = document.createElement('img');
          var url = URL.createObjectURL(f);
          im.src = url;
          im.alt = f.name;
          im.addEventListener('load', function () { URL.revokeObjectURL(url); }, { once: true });
          thumb.appendChild(im);
        } else {
          var box = document.createElement('div');
          box.className = 'upload-preview-file';
          box.textContent = f.name;
          thumb.appendChild(box);
        }
        var idx = document.createElement('span');
        idx.className = 'upload-preview-idx';
        idx.textContent = pad2(i + 1) + (i === 0 && files.length > 1 ? ' · COVER' : '');
        thumb.appendChild(idx);
        tile.appendChild(thumb);

        if (typeof DataTransfer !== 'undefined') {
          var bar = document.createElement('div');
          bar.className = 'upload-preview-bar';
          var mkBtn = function (txt, title, fn, cls) {
            var b = document.createElement('button');
            b.type = 'button';
            b.textContent = txt;
            b.title = title;
            if (cls) b.className = cls;
            b.addEventListener('click', fn);
            return b;
          };
          if (i > 0) {
            bar.appendChild(mkBtn('←', 'Move left', function () {
              var next = files.slice();
              var tmp = next[i - 1]; next[i - 1] = next[i]; next[i] = tmp;
              applyFileList(inp, next);
            }));
          }
          if (i < files.length - 1) {
            bar.appendChild(mkBtn('→', 'Move right', function () {
              var next = files.slice();
              var tmp = next[i + 1]; next[i + 1] = next[i]; next[i] = tmp;
              applyFileList(inp, next);
            }));
          }
          bar.appendChild(mkBtn('×', 'Remove', function () {
            var next = files.slice();
            next.splice(i, 1);
            applyFileList(inp, next);
          }, 'is-danger'));
          tile.appendChild(bar);
        }
        previews.appendChild(tile);
      });
    };

    document.querySelectorAll('input[type=file]').forEach(function (inp) {
      inp.addEventListener('change', function () { renderFileLabel(inp); });
    });

    // Drag-and-drop & clipboard paste onto dropzone on /admin/new
    var dz = document.querySelector('[data-dropzone]');
    var dzInput = dz && dz.querySelector('input[type=file]');
    if (dz && dzInput && typeof DataTransfer !== 'undefined') {
      var addFiles = function (list) {
        if (!list || !list.length) return;
        var existing = dzInput.files ? Array.prototype.slice.call(dzInput.files) : [];
        var merged = existing.concat(Array.prototype.slice.call(list));
        applyFileList(dzInput, merged);
      };
      ['dragenter', 'dragover'].forEach(function (ev) {
        dz.addEventListener(ev, function (e) { e.preventDefault(); dz.classList.add('is-dragover'); });
      });
      ['dragleave', 'drop'].forEach(function (ev) {
        dz.addEventListener(ev, function (e) { e.preventDefault(); dz.classList.remove('is-dragover'); });
      });
      dz.addEventListener('drop', function (e) {
        if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length) {
          addFiles(e.dataTransfer.files);
        }
      });
      document.addEventListener('paste', function (e) {
        if (typing(e.target) && e.target.tagName !== 'BODY') return;
        var items = e.clipboardData && e.clipboardData.files;
        if (items && items.length) addFiles(items);
      });
    }
  })();

  // ---- blur-up placeholders + multi-image board card quick-peek ----
  document.querySelectorAll('.card-img-container img').forEach(function (img) { blurUp(img); observeEager(img); });
  document.querySelectorAll('.card--multi').forEach(initCardPeek);

  // ---- staggered card entrance (chronological order so top row sweeps left-to-right) ----
  (function () {
    var grid = document.getElementById('grid');
    var list = (grid && grid.__order) ? grid.__order : Array.prototype.slice.call(document.querySelectorAll('.card'));
    if (!list.length) return;
    var observer = new IntersectionObserver(function (entries) {
      entries.forEach(function (entry) {
        if (entry.isIntersecting) {
          entry.target.classList.add('visible');
          observer.unobserve(entry.target);
        }
      });
    }, { threshold: 0.01, rootMargin: '0px 0px -10px 0px' });
    list.forEach(function (card, i) {
      card.style.transitionDelay = (i % 4) * 0.07 + 's';
      observer.observe(card);
    });
  })();

  // ---- system clock (true NYC time) + NYC weather ----
  var clock = document.getElementById('sys-clock');
  if (clock) {
    var TEMP_TTL = 15 * 60 * 1000;
    var temp = '--';
    var timeFmt = null;
    try {
      timeFmt = new Intl.DateTimeFormat('en-US', {
        timeZone: 'America/New_York',
        hourCycle: 'h23',
        hour: '2-digit',
        minute: '2-digit'
      });
    } catch (e) {}
    var update = function () {
      var now = new Date();
      var t = timeFmt ? timeFmt.format(now) : now.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit' });
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
        .catch(function () {});
    };
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
    setInterval(fetchWeather, TEMP_TTL);
  }

  // ---- view transition names ----
  document.querySelectorAll('[data-vt]').forEach(function (el) {
    try { el.style.viewTransitionName = el.getAttribute('data-vt'); } catch (e) {}
  });

  // ---- dark mode ----
  (function () {
    var root = document.documentElement;
    var btn = document.getElementById('theme-toggle');
    var apply = function (theme, persist) {
      root.setAttribute('data-theme', theme);
      root.style.colorScheme = theme;
      if (btn) btn.setAttribute('aria-pressed', theme === 'dark' ? 'true' : 'false');
      try { document.cookie = 'dnttg-theme=' + theme + '; path=/; max-age=31536000; samesite=lax'; } catch (e) {}
      if (persist) { try { localStorage.setItem('dnttg-theme', theme); } catch (e) {} }
      var tc = document.getElementById('theme-color');
      if (tc) tc.setAttribute('content', theme === 'light' ? '#f5f3ec' : '#0b0907');
    };
    if (btn) btn.setAttribute('aria-pressed', root.getAttribute('data-theme') === 'dark' ? 'true' : 'false');
    window.__toggleTheme = function () {
      apply(root.getAttribute('data-theme') === 'dark' ? 'light' : 'dark', true);
    };
    if (btn) btn.addEventListener('click', window.__toggleTheme);
    try {
      matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function (e) {
        var saved = localStorage.getItem('dnttg-theme');
        if (saved !== 'light' && saved !== 'dark') apply(e.matches ? 'dark' : 'light', false);
      });
    } catch (e) {}
  })();

  // ---- shortcuts sheet + global keys (?, g, d, esc) ----
  (function () {
    var sheet = document.getElementById('shortcuts');
    var last = null, untrap = null;
    var openSheet = function () {
      if (!sheet || sheet.classList.contains('open')) return;
      last = document.activeElement;
      sheet.classList.add('open');
      lockPage();
      sheet.focus();
      untrap = trapFocus(sheet);
    };
    var closeSheet = function () {
      if (!sheet || !sheet.classList.contains('open')) return;
      sheet.classList.remove('open');
      if (untrap) { untrap(); untrap = null; }
      unlockPage();
      if (last && last.focus) last.focus();
    };
    var closeBtn = document.getElementById('shortcuts-close');
    if (closeBtn) closeBtn.addEventListener('click', function (e) { e.preventDefault(); closeSheet(); });
    document.addEventListener('keydown', function (e) {
      if (typing(e.target) || e.metaKey || e.ctrlKey || e.altKey) return;
      if (e.key === '?') { if (sheet && sheet.classList.contains('open')) closeSheet(); else if (!anyModalOpen()) openSheet(); }
      else if (e.key === 'Escape') { closeSheet(); }
      else if (anyModalOpen()) return;
      else if (e.key === 'g' || e.key === 'G') { window.location.href = '/'; }
      else if (e.key === 'r' || e.key === 'R') { window.location.href = '/random'; }
      else if (e.key === 'd' || e.key === 'D') { if (window.__toggleTheme) window.__toggleTheme(); }
    });
    if (sheet) sheet.addEventListener('click', function (e) { if (e.target === sheet) closeSheet(); });
  })();

  // ---- board keyboard nav (j/k + enter in chronological order) ----
  (function () {
    var grid = document.getElementById('grid');
    if (!grid) return;
    var idx = -1;
    var cards = function () {
      return grid.__order ? grid.__order : Array.prototype.slice.call(grid.querySelectorAll('.card'));
    };
    var focus = function (n) {
      var list = cards();
      if (!list.length) return;
      if (idx >= 0 && list[idx]) list[idx].classList.remove('kbd-focus');
      idx = Math.max(0, Math.min(n, list.length - 1));
      list[idx].classList.add('kbd-focus');
      list[idx].scrollIntoView({ block: 'center', behavior: 'smooth' });
    };
    document.addEventListener('keydown', function (e) {
      if (typing(e.target) || e.metaKey || e.ctrlKey || e.altKey || anyModalOpen()) return;
      if (e.key === 'j' || e.key === 'J') { e.preventDefault(); focus(idx + 1); }
      else if (e.key === 'k' || e.key === 'K') { e.preventDefault(); focus(idx - 1); }
      else if (e.key === 'Enter' && idx >= 0) { var l = cards()[idx]; if (l) l.click(); }
    });
  })();

  // ---- infinite scroll (keyset cursor) ----
  (function () {
    var sentinel = document.getElementById('board-more');
    var grid = document.getElementById('grid');
    if (!sentinel || !grid) return;
    var page = parseInt(sentinel.dataset.page || '48', 10);
    var cursorCreated = sentinel.dataset.cursorCreated || '';
    var cursorId = sentinel.dataset.cursorId || '';
    var cat = sentinel.dataset.cat || '', tag = sentinel.dataset.tag || '', color = sentinel.dataset.color || '';
    var done = sentinel.dataset.done === '1' || !cursorCreated || !cursorId;
    var loading = false;
    var retryEl = null;
    var markEnd = function () {
      if (done && grid.querySelector('.card') && !document.querySelector('.board-end')) {
        var end = document.createElement('div');
        end.className = 'board-end';
        end.textContent = 'end of the archive';
        sentinel.parentNode.insertBefore(end, sentinel.nextSibling);
      }
    };
    var clearRetry = function () {
      if (retryEl && retryEl.parentNode) retryEl.parentNode.removeChild(retryEl);
      retryEl = null;
    };
    var showRetry = function () {
      if (retryEl) return;
      retryEl = document.createElement('button');
      retryEl.type = 'button';
      retryEl.className = 'board-retry';
      retryEl.textContent = 'Couldn’t load more — retry';
      retryEl.addEventListener('click', function () { clearRetry(); load(); });
      sentinel.parentNode.insertBefore(retryEl, sentinel.nextSibling);
    };
    var load = function () {
      if (loading || done) { markEnd(); return; }
      if (!cursorCreated || !cursorId) { done = true; markEnd(); return; }
      loading = true;
      var q = '/board/more?cursor=' + encodeURIComponent(cursorCreated + ':' + cursorId) +
        (cat ? '&cat=' + encodeURIComponent(cat) : '') +
        (tag ? '&tag=' + encodeURIComponent(tag) : '') +
        (color ? '&color=' + encodeURIComponent(color) : '');
      fetch(q).then(function (r) {
        if (!r.ok) throw new Error('board more ' + r.status);
        return r.text();
      }).then(function (html) {
        var tmp = document.createElement('div');
        tmp.innerHTML = html;
        var added = tmp.querySelectorAll('.card');
        var lastCard = null;
        added.forEach(function (card) {
          hydrateCard(card);
          masonryAppend(grid, card);
          lastCard = card;
        });
        if (lastCard) {
          cursorId = lastCard.getAttribute('data-id') || cursorId;
          cursorCreated = lastCard.getAttribute('data-created') || cursorCreated;
        }
        if (added.length < page) { done = true; markEnd(); }
        clearRetry();
        loading = false;
      }).catch(function () {
        loading = false;
        showRetry();
      });
    };
    new IntersectionObserver(function (en) { en.forEach(function (x) { if (x.isIntersecting) load(); }); }, { rootMargin: '1500px' }).observe(sentinel);
    if (done) markEnd();
  })();

  // ---- intent prefetch ----
  (function () {
    var seen = {};
    var prefetch = function (url) {
      if (!url || url.charAt(0) !== '/' || seen[url]) return;
      seen[url] = 1;
      var l = document.createElement('link');
      l.rel = 'prefetch';
      l.as = 'document';
      l.href = url;
      document.head.appendChild(l);
    };
    var onIntent = function (e) {
      var a = e.target.closest && e.target.closest('a.card, a.dn-link, a.related-item, a.search-result, a[href^="/item/"]');
      if (a) prefetch(a.getAttribute('href'));
    };
    document.addEventListener('mouseover', onIntent, { passive: true });
    document.addEventListener('focusin', onIntent);
    document.addEventListener('touchstart', onIntent, { passive: true });
  })();

  // ---- colophon reveal ----
  (function () {
    var col = document.getElementById('colophon');
    if (!col) return;
    var io = new IntersectionObserver(function (en) {
      en.forEach(function (x) { if (x.isIntersecting) { col.classList.add('revealed'); io.disconnect(); } });
    }, { threshold: 0.2 });
    io.observe(col);
  })();

  // ---- related items reveal ----
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

  // ---- admin dashboard bulk actions ----
  (function () {
    var form = document.getElementById('bulk-form');
    var all = document.getElementById('bulk-all');
    var countEl = document.getElementById('bulk-count');
    var action = document.getElementById('bulk-action');
    var catInp = document.getElementById('bulk-category');
    var tagsInp = document.getElementById('bulk-tags');
    if (!form || !action) return;
    var checks = function () { return Array.prototype.slice.call(document.querySelectorAll('.bulk-check')); };
    var sync = function () {
      var list = checks();
      var n = list.filter(function (c) { return c.checked; }).length;
      form.hidden = n === 0;
      if (countEl) countEl.textContent = n + ' selected';
      if (all) {
        all.checked = list.length > 0 && n === list.length;
        all.indeterminate = n > 0 && n < list.length;
      }
    };
    var syncInputs = function () {
      var v = action.value;
      if (catInp) catInp.hidden = v !== 'category';
      if (tagsInp) tagsInp.hidden = v !== 'tags';
    };
    if (all) {
      all.addEventListener('change', function () {
        checks().forEach(function (c) { c.checked = all.checked; });
        sync();
      });
    }
    document.addEventListener('change', function (e) {
      if (e.target && e.target.classList && e.target.classList.contains('bulk-check')) sync();
    });
    action.addEventListener('change', syncInputs);
    form.addEventListener('submit', function (e) {
      var n = checks().filter(function (c) { return c.checked; }).length;
      if (!n) { e.preventDefault(); return; }
      if (action.value === 'delete' && !window.confirm('Permanently delete ' + n + ' item(s) and their media?')) {
        e.preventDefault();
      }
    });
    sync();
    syncInputs();
  })();

  // ---- admin edit gallery drag-and-drop reordering ----
  (function () {
    var grid = document.getElementById('edit-gallery-grid');
    if (!grid) return;
    var url = grid.getAttribute('data-reorder-url');
    var csrf = grid.getAttribute('data-csrf');
    if (!url || !csrf) return;
    var dragged = null;

    var refreshBadges = function () {
      var cards = Array.prototype.slice.call(grid.querySelectorAll('.edit-gallery-card'));
      cards.forEach(function (c, i) {
        c.classList.toggle('is-cover', i === 0);
        var pos = c.querySelector('.edit-gallery-pos');
        if (pos) pos.textContent = pad2(i + 1) + (i === 0 ? ' · COVER' : '');
      });
    };

    var persistOrder = function () {
      var ids = Array.prototype.slice.call(grid.querySelectorAll('.edit-gallery-card[data-img-id]'))
        .map(function (c) { return c.getAttribute('data-img-id'); });
      if (!ids.length) return;
      var body = new URLSearchParams();
      body.set('csrf_token', csrf);
      body.set('order', ids.join(','));
      fetch(url, {
        method: 'POST',
        headers: { 'Content-Type': 'application/x-www-form-urlencoded', 'Accept': 'application/json' },
        body: body.toString()
      }).then(function (r) {
        if (!r.ok) window.location.reload();
      }).catch(function () { window.location.reload(); });
    };

    grid.addEventListener('dragstart', function (e) {
      var card = e.target.closest && e.target.closest('.edit-gallery-card[data-img-id]');
      if (!card || typing(e.target)) { e.preventDefault(); return; }
      dragged = card;
      card.classList.add('is-dragging');
      if (e.dataTransfer) {
        e.dataTransfer.effectAllowed = 'move';
        try { e.dataTransfer.setData('text/plain', card.getAttribute('data-img-id') || ''); } catch (err) {}
      }
    });
    grid.addEventListener('dragover', function (e) {
      if (!dragged) return;
      var target = e.target.closest && e.target.closest('.edit-gallery-card[data-img-id]');
      if (!target || target === dragged) return;
      e.preventDefault();
      if (e.dataTransfer) e.dataTransfer.dropEffect = 'move';
      grid.querySelectorAll('.is-drag-over').forEach(function (el) { el.classList.remove('is-drag-over'); });
      target.classList.add('is-drag-over');
    });
    grid.addEventListener('dragleave', function (e) {
      var target = e.target.closest && e.target.closest('.edit-gallery-card');
      if (target) target.classList.remove('is-drag-over');
    });
    grid.addEventListener('drop', function (e) {
      if (!dragged) return;
      e.preventDefault();
      var target = e.target.closest && e.target.closest('.edit-gallery-card[data-img-id]');
      grid.querySelectorAll('.is-drag-over').forEach(function (el) { el.classList.remove('is-drag-over'); });
      if (!target || target === dragged) return;
      var cards = Array.prototype.slice.call(grid.children);
      if (cards.indexOf(dragged) < cards.indexOf(target)) {
        grid.insertBefore(dragged, target.nextSibling);
      } else {
        grid.insertBefore(dragged, target);
      }
      refreshBadges();
      persistOrder();
    });
    grid.addEventListener('dragend', function () {
      if (dragged) dragged.classList.remove('is-dragging');
      grid.querySelectorAll('.is-drag-over').forEach(function (el) { el.classList.remove('is-drag-over'); });
      dragged = null;
    });
  })();

  // ===== easter eggs =====

  try {
    console.log('%cDO NOT TOUCH THE GLASS', 'font:600 22px sans-serif;color:#111;background:#fff;padding:6px 10px;');
    console.log('%cyou found the back of the glass. — D.T.', 'color:#888;font-style:italic');
  } catch (e) {}

  // touch the glass — a smudge + ripple where you press the main image or lightbox
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
      if (t && t.matches && t.matches('img.zoomable, #lightbox-img')) smudge(e.clientX, e.clientY);
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
    var mark = document.querySelector('.header-title');
    if (!mark) return;
    var n = 0, t;
    mark.addEventListener('click', function () {
      n++; clearTimeout(t); t = setTimeout(function () { n = 0; }, 500);
      if (n >= 3) { n = 0; mark.classList.remove('datamosh'); void mark.offsetWidth; mark.classList.add('datamosh'); }
    });
  })();

})();
