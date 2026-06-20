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
})();
