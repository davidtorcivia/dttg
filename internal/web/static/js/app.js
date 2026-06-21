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
  function layoutMasonry(grid, force) {
    var n = gridColumnCount(gridMaxCols(grid));
    if (!force && grid.__n === n) return; // column count unchanged; nothing to do
    var cards = Array.prototype.slice.call(grid.querySelectorAll('.card'));
    var frag = document.createDocumentFragment();
    var cols = [];
    for (var i = 0; i < n; i++) {
      var c = document.createElement('div');
      c.className = 'grid-col';
      cols.push(c);
      frag.appendChild(c);
    }
    cards.forEach(function (card, i) { cols[i % n].appendChild(card); });
    grid.textContent = '';
    grid.appendChild(frag);
    grid.__cols = cols;
    grid.__n = n;
  }
  // Append one card, continuing the round-robin from the current card count.
  function masonryAppend(grid, card) {
    var cols = grid.__cols;
    if (!cols || !cols.length) { grid.appendChild(card); return; }
    cols[grid.querySelectorAll('.card').length % cols.length].appendChild(card);
  }
  (function () {
    var grid = document.getElementById('grid');
    if (!grid) return;
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
      // focus() is a no-op while the overlay is still visibility:hidden mid-
      // transition, so defer one frame until it's actually visible.
      if (input) { requestAnimationFrame(function () { input.focus(); input.select(); }); }
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
        var img = card.querySelector('img[data-ph]');
        if (img) { var c = img.parentElement; c.style.backgroundImage = 'url("' + img.getAttribute('data-ph') + '")'; c.style.backgroundSize = 'cover'; c.style.backgroundPosition = 'center'; }
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
          revive();
          layoutMasonry(grid, true);
          var n = grid.querySelectorAll('.card').length;
          if (meta) meta.textContent = n + ' result' + (n === 1 ? '' : 's') + ' for “' + q + '”';
        }).catch(function () {});
    };
    input.addEventListener('input', function () { clearTimeout(timer); timer = setTimeout(run, 200); });
  })();

  // ---- detail video: native controls only while hovered ----
  // (the video autoplays muted on a loop; controls would otherwise sit on top of
  // it the whole time). Toggle the attribute since CSS can't hide native controls
  // cross-browser. Pointer-only — autoplay can auto-focus the video, so keying
  // off focus would make the controls appear on load.
  document.querySelectorAll('video.detail-video').forEach(function (v) {
    v.addEventListener('pointerenter', function () { v.setAttribute('controls', ''); });
    v.addEventListener('pointerleave', function () { v.removeAttribute('controls'); });
  });

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
  document.querySelectorAll('img[data-ph]').forEach(function (img) {
    var c = img.parentElement;
    if (!c) return;
    c.style.backgroundImage = 'url("' + img.getAttribute('data-ph') + '")';
    c.style.backgroundSize = 'cover';
    c.style.backgroundPosition = 'center';
  });

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

  // ---- lightbox ----
  (function () {
    var box = document.getElementById('lightbox');
    var img = document.getElementById('lightbox-img');
    if (!box || !img) return;
    document.querySelectorAll('.zoomable').forEach(function (z) {
      z.addEventListener('click', function () { img.src = z.getAttribute('data-full') || z.src; box.classList.add('open'); });
    });
    box.addEventListener('click', function (e) { if (e.target === box) box.classList.remove('open'); });
    document.addEventListener('keydown', function (e) { if (e.key === 'Escape') box.classList.remove('open'); });

    // "Expand full size" — shown only when the detail image is displayed smaller
    // than its source (i.e. there's more detail to see); opens the lightbox.
    var dimg = document.querySelector('.image-wrapper img.zoomable');
    var btn = document.getElementById('expand-full');
    if (dimg && btn) {
      var sync = function () {
        var bigger = dimg.naturalWidth > dimg.clientWidth + 4 || dimg.naturalHeight > dimg.clientHeight + 4;
        btn.hidden = !(dimg.clientWidth && bigger);
      };
      if (dimg.complete && dimg.naturalWidth) sync(); else dimg.addEventListener('load', sync);
      window.addEventListener('resize', sync);
      btn.addEventListener('click', function () {
        img.src = dimg.getAttribute('data-full') || dimg.src;
        box.classList.add('open');
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
          var img = card.querySelector('img[data-ph]');
          if (img) { var c = img.parentElement; c.style.backgroundImage = 'url("' + img.getAttribute('data-ph') + '")'; c.style.backgroundSize = 'cover'; c.style.backgroundPosition = 'center'; }
          var vt = card.querySelector('[data-vt]'); if (vt) { try { vt.style.viewTransitionName = vt.getAttribute('data-vt'); } catch (e) {} }
          card.classList.add('visible');
          masonryAppend(grid, card);
        });
        offset += added.length;
        if (added.length < page) done = true;
        loading = false;
      }).catch(function () { loading = false; });
    };
    new IntersectionObserver(function (en) { en.forEach(function (x) { if (x.isIntersecting) load(); }); }, { rootMargin: '700px' }).observe(sentinel);
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
