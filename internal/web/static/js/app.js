(function () {
  'use strict';

  // ---- service worker (PWA installability + Android share target) ----
  if ('serviceWorker' in navigator) {
    window.addEventListener('load', function () {
      navigator.serviceWorker.register('/sw.js').catch(function () {});
    });
  }

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
      if (input) { input.focus(); input.select(); }
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
    var temp = '--';
    var update = function () {
      var now = new Date();
      var t = now.toLocaleTimeString('en-US', { hour12: false, hour: '2-digit', minute: '2-digit' });
      clock.textContent = t + ' // NYC ' + temp + '°F';
    };
    var fetchWeather = function () {
      fetch('/api/weather')
        .then(function (r) { return r.json(); })
        .then(function (d) { if (typeof d.temp === 'number') { temp = d.temp; } update(); })
        .catch(function () { /* leave placeholder */ });
    };
    update();
    setInterval(update, 1000);
    fetchWeather();
    setInterval(fetchWeather, 15 * 60 * 1000);
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
          grid.appendChild(card);
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

  // ken burns idle screensaver (single timer, reset on activity)
  (function () {
    if (!document.getElementById('grid')) return;
    var timer, hint;
    var sleep = function () {
      document.body.classList.add('screensaver');
      hint = document.createElement('div');
      hint.className = 'screensaver-hint';
      hint.textContent = "you're still looking.";
      document.body.appendChild(hint);
    };
    var wake = function () {
      if (document.body.classList.contains('screensaver')) {
        document.body.classList.remove('screensaver');
        if (hint) { hint.remove(); hint = null; }
      }
      clearTimeout(timer);
      timer = setTimeout(sleep, 90000);
    };
    ['mousemove', 'keydown', 'scroll', 'touchstart', 'pointerdown'].forEach(function (ev) {
      window.addEventListener(ev, wake, { passive: true });
    });
    wake();
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

  // seance — park the cursor dead-center to read the glass
  (function () {
    var box = null, timer = null;
    var open = function () {
      box = document.createElement('div');
      box.className = 'seance';
      box.innerHTML = '<div class="seance-inner"><div class="big">…</div><div class="sub">reading the glass</div></div>';
      document.body.appendChild(box);
      requestAnimationFrame(function () { box.classList.add('open'); });
      fetch('/api/stats').then(function (r) { return r.json(); }).then(function (d) {
        if (!box) return;
        var big = box.querySelector('.big'), sub = box.querySelector('.sub');
        var target = d.count || 0, t0 = null;
        var tick = function (ts) {
          if (!box) return;
          if (t0 == null) t0 = ts;
          var p = Math.min((ts - t0) / 1100, 1);
          big.textContent = Math.round((1 - Math.pow(1 - p, 3)) * target) + ' pieces';
          if (p < 1) requestAnimationFrame(tick);
        };
        requestAnimationFrame(tick);
        sub.textContent = 'archived since ' + d.oldest + ' — you may look, but do not touch';
      }).catch(function () {});
      var close = function () {
        if (!box) return;
        var b = box; box = null; b.classList.remove('open'); setTimeout(function () { b.remove(); }, 600);
        document.removeEventListener('pointerdown', close); document.removeEventListener('keydown', close);
      };
      setTimeout(function () { document.addEventListener('pointerdown', close); document.addEventListener('keydown', close); }, 60);
    };
    document.addEventListener('mousemove', function (e) {
      if (box) return;
      var near = Math.abs(e.clientX - window.innerWidth / 2) < 110 && Math.abs(e.clientY - window.innerHeight / 2) < 110;
      clearTimeout(timer);
      if (near) timer = setTimeout(open, 1600);
    });
  })();
})();
