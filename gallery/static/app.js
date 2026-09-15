/* twitter-pic gallery 前端：网格分页 + 手机式全屏查看器（横向翻页 / 双指与双击缩放 / 下滑关闭 / chrome 自动隐藏）+ 赞/踩/喜欢 */
(function () {
  'use strict';

  var dataEl = document.getElementById('g-data');
  var grid = document.getElementById('g-grid');
  if (!dataEl || !grid) return;

  var data;
  try { data = JSON.parse(dataEl.textContent); } catch (e) { return; }

  var slug = grid.getAttribute('data-slug') || '';
  var PER = parseInt(grid.getAttribute('data-per') || '12', 10) || 12;
  var all = (data.timeline || []).filter(function (m) { return m && m.url; }).map(function (m) {
    return { url: m.url, id: (m.tweet_id || 0), video: (m.type === 'video' || m.type === 'animated_gif') };
  });

  /* ---------- URL 状态：type 过滤 + tweet-id 游标 ---------- */
  function qs() { return new URLSearchParams(location.search); }
  function typeFilter() { var t = qs().get('type') || ''; return (t === 'photo' || t === 'video') ? t : ''; }
  function cursorID() { var n = parseInt(qs().get('cursor') || '', 10); return (isFinite(n) && n > 0) ? n : 0; }
  function list() {
    var t = typeFilter();
    if (!t) return all;
    return all.filter(function (m) { return t === 'video' ? m.video : !m.video; });
  }
  function indexOf(arr, id) { if (!id) return 0; for (var i = 0; i < arr.length; i++) { if (arr[i].id === id) return i; } return 0; }
  function clampStart(arr, s) { if (!arr.length) return 0; var last = ((arr.length - 1) / PER | 0) * PER; return Math.min(Math.max(0, s), last); }
  function href(t, c) {
    var u = '/u/' + encodeURIComponent(slug), p = [];
    if (t) p.push('type=' + encodeURIComponent(t));
    if (c > 0) p.push('cursor=' + c);
    return p.length ? u + '?' + p.join('&') : u;
  }
  function setURL(t, c) { history.replaceState(null, '', href(t, c)); }

  function mediaNode(m, controls) {
    var n;
    if (m.video) { n = document.createElement('video'); n.controls = !!controls; n.playsInline = true; n.preload = controls ? 'none' : 'metadata'; }
    else { n = document.createElement('img'); n.loading = 'lazy'; n.alt = ''; n.draggable = false; }
    n.src = m.url;
    return n;
  }

  /* ---------- 网格分页 ---------- */
  var info = document.getElementById('g-info');
  var countEl = document.getElementById('g-count');
  var prevA = document.getElementById('g-prev');
  var nextA = document.getElementById('g-next');
  var modes = Array.prototype.slice.call(document.querySelectorAll('[data-mode]'));

  function gridCard(m, gIdx) {
    var d = document.createElement('div'); d.className = 'card';
    var n = mediaNode(m, false);
    n.addEventListener('click', function () { openLightbox(gIdx); });
    d.appendChild(n);
    return d;
  }
  function renderGrid() {
    var arr = list();
    var start = clampStart(arr, indexOf(arr, cursorID()));
    var end = Math.min(start + PER, arr.length);
    var frag = document.createDocumentFragment();
    for (var i = start; i < end; i++) frag.appendChild(gridCard(arr[i], i));
    grid.replaceChildren(frag);
    if (countEl) countEl.textContent = arr.length + ' media';
    if (info) info.textContent = arr.length ? (start + 1) + '–' + end + ' / ' + arr.length : '0';
    if (prevA) { prevA.href = href(typeFilter(), start > 0 ? arr[Math.max(0, start - PER)].id : 0); prevA.hidden = !(start > 0); }
    if (nextA) { nextA.href = href(typeFilter(), end < arr.length ? arr[end].id : 0); nextA.hidden = !(end < arr.length); }
    modes.forEach(function (a) { a.classList.toggle('active', a.getAttribute('data-mode') === typeFilter()); });
  }
  function go(t, c) { setURL(t, c); renderGrid(); }

  modes.forEach(function (a) {
    a.addEventListener('click', function (e) { e.preventDefault(); go(a.getAttribute('data-mode') || '', 0); });
  });
  if (prevA) prevA.addEventListener('click', function (e) {
    e.preventDefault(); var arr = list(), start = indexOf(arr, cursorID());
    go(typeFilter(), start > 0 ? arr[Math.max(0, start - PER)].id : 0);
  });
  if (nextA) nextA.addEventListener('click', function (e) {
    e.preventDefault(); var arr = list(), start = indexOf(arr, cursorID()), end = Math.min(start + PER, arr.length);
    go(typeFilter(), end < arr.length ? arr[end].id : 0);
  });

  /* ---------- 手机式全屏查看器 ----------
     横向 scroll-snap 翻页；图片双指捏合 / 双击缩放（以触点为焦点），放大后单指拖平移；
     下滑关闭（跟手 + 渐隐）；空白处点击关闭；3.2s 无操作自动隐藏 chrome；
     桌面：左右圆形按钮 + 滚轮 + 方向键。 */
  var lb = document.getElementById('lb');
  var track = document.getElementById('lb-track');
  var lbInfo = document.getElementById('lb-info');
  var lbInd = document.getElementById('lb-ind');
  var likeBtn = document.getElementById('lb-like');
  var dislikeBtn = document.getElementById('lb-dislike');
  var favBtn = document.getElementById('lb-fav');
  var likeCount = document.getElementById('lb-likes');
  var dislikeCount = document.getElementById('lb-dislikes');
  var prevBtn = document.getElementById('lb-prev');
  var nextBtn = document.getElementById('lb-next');

  var slides = [];
  var cur = 0;
  var counts = {};

  function lg(k, d) { try { var v = localStorage.getItem(k); return v === null ? d : v; } catch (e) { return d; } }
  function ls(k, v) { try { localStorage.setItem(k, v); } catch (e) {} }
  function myVote(key) { return parseInt(lg('gallery:vote:' + key, '0'), 10) || 0; }
  function setMyVote(key, v) { ls('gallery:vote:' + key, String(v)); }
  function isFav(key) { return lg('gallery:fav:' + key, '0') === '1'; }
  function setFav(key, on) { ls('gallery:fav:' + key, on ? '1' : '0'); }
  function keyOf(i) { return slides[i] ? slides[i].url : ''; }

  function loadCounts(key) {
    if (!key || counts[key]) { paintCounts(); return; }
    fetch('/api/reactions?keys=' + encodeURIComponent(key))
      .then(function (r) { return r.json(); })
      .then(function (j) { counts[key] = j[key] || { likes: 0, dislikes: 0 }; paintCounts(); })
      .catch(function () {});
  }
  function postReact(key, dl, dd) {
    fetch('/api/react', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ key: key, dl: dl, dd: dd })
    }).then(function (r) { return r.json(); })
      .then(function (j) { counts[key] = { likes: j.likes, dislikes: j.dislikes }; paintCounts(); })
      .catch(function () {});
  }
  function paintCounts() {
    var key = keyOf(cur); if (!key) return;
    var c = counts[key] || { likes: 0, dislikes: 0 };
    if (likeCount) likeCount.textContent = c.likes;
    if (dislikeCount) dislikeCount.textContent = c.dislikes;
  }
  function paintRail() {
    var m = slides[cur]; if (!m) return;
    var key = m.url, v = myVote(key);
    paintCounts();
    if (likeBtn) likeBtn.classList.toggle('on', v === 1);
    if (dislikeBtn) dislikeBtn.classList.toggle('on', v === -1);
    if (favBtn) favBtn.classList.toggle('on', isFav(key));
    if (lbInd) lbInd.textContent = (cur + 1) + ' / ' + slides.length;
    if (lbInfo) lbInfo.textContent = '@' + slug + (m.id ? ' · ' + m.id : '');
    if (prevBtn) prevBtn.classList.toggle('dis', cur <= 0);
    if (nextBtn) nextBtn.classList.toggle('dis', cur >= slides.length - 1);
    loadCounts(key);
  }

  /* chrome 自动隐藏 */
  var uiTimer = 0;
  function showUI() {
    lb.classList.remove('hide-ui');
    clearTimeout(uiTimer);
    uiTimer = setTimeout(function () { lb.classList.add('hide-ui'); }, 3200);
  }

  /* ---------- 缩放（PhotoSwipe 式：translate 在前，scale 在后，焦点固定） ---------- */
  function applyZ(s) {
    var z = s._z;
    s._st.style.transform = 'translate(' + z.x + 'px,' + z.y + 'px) scale(' + z.s + ')';
    s.classList.toggle('zoomed', z.s > 1.01);
  }
  function clampZ(s) {
    var z = s._z, n = s._media;
    var w = n.offsetWidth || 1, h = n.offsetHeight || 1;
    var mx = Math.max(0, (z.s - 1) * w / 2), my = Math.max(0, (z.s - 1) * h / 2);
    z.x = Math.min(mx, Math.max(-mx, z.x));
    z.y = Math.min(my, Math.max(-my, z.y));
  }
  function resetZoom(s) {
    if (!s || !s._z) return;
    s._z = { s: 1, x: 0, y: 0 };
    s._st.classList.add('anim');
    applyZ(s);
    setTimeout(function () { s._st.classList.remove('anim'); }, 260);
  }

  function buildSlide(m, i) {
    var s = document.createElement('div'); s.className = 'lb-slide';
    var st = document.createElement('div'); st.className = 'lb-stage';
    var n = mediaNode(m, false);
    n.classList.add('lb-media');
    if (m.video) {
      n.preload = 'none'; n.loop = true;
      var icon = document.createElement('div'); icon.className = 'lb-playicon'; icon.textContent = '\u25B6';
      s._v = n; s.appendChild(icon);
    } else {
      n.addEventListener('dragstart', function (e) { e.preventDefault(); });
    }
    st.appendChild(n); s.appendChild(st);
    s._st = st; s._media = n; s._z = { s: 1, x: 0, y: 0 };
    attachGestures(s);
    return s;
  }

  function attachGestures(s) {
    var pts = {}, np = 0, mode = '', moved = 0, swY = 0;
    var downX = 0, downY = 0, downT = 0, lastX = 0, lastY = 0, hit = null;
    var pinch = null;

    function focal(cx, cy) { var r = s.getBoundingClientRect(); return { x: cx - (r.left + r.width / 2), y: cy - (r.top + r.height / 2) }; }

    s.addEventListener('pointerdown', function (e) {
      if (e.pointerType === 'mouse' && e.button !== 0) return;
      pts[e.pointerId] = { x: e.clientX, y: e.clientY }; np++;
      try { s.setPointerCapture(e.pointerId); } catch (err) {}
      if (np >= 2) {
        mode = 'pinch'; swY = 0;
        var a = pts[Object.keys(pts)[0]], b = pts[Object.keys(pts)[1]];
        var d = Math.hypot(a.x - b.x, a.y - b.y) || 1;
        var z = s._z, f = focal((a.x + b.x) / 2, (a.y + b.y) / 2);
        pinch = { d0: d, s0: z.s, x0: z.x, y0: z.y, fx: (f.x - z.x) / z.s, fy: (f.y - z.y) / z.s };
        s._st.classList.remove('anim');
        return;
      }
      mode = s._z.s > 1.01 ? 'pan' : 'drag';
      hit = e.target; // pointerup 时 target 已被 setPointerCapture 改道，命中判定记在 down 上
      downX = lastX = e.clientX; downY = lastY = e.clientY; downT = Date.now(); moved = 0; swY = 0;
      showUI();
    });

    s.addEventListener('pointermove', function (e) {
      var p = pts[e.pointerId]; if (!p) return;
      var dx = e.clientX - lastX, dy = e.clientY - lastY;
      lastX = e.clientX; lastY = e.clientY;
      if (mode === 'pinch' && pinch && np >= 2) {
        p.x = e.clientX; p.y = e.clientY;
        var ks = Object.keys(pts), a = pts[ks[0]], b = pts[ks[1]];
        if (!a || !b) return;
        var z = s._z, d = Math.hypot(a.x - b.x, a.y - b.y) || 1;
        z.s = Math.min(4, Math.max(1, pinch.s0 * d / pinch.d0));
        var f = focal((a.x + b.x) / 2, (a.y + b.y) / 2);
        z.x = f.x - z.s * pinch.fx; z.y = f.y - z.s * pinch.fy;
        clampZ(s); applyZ(s);
        return;
      }
      moved += Math.abs(dx) + Math.abs(dy);
      p.x = e.clientX; p.y = e.clientY;
      if (mode === 'pan') {
        var z2 = s._z; z2.x += dx; z2.y += dy; clampZ(s); applyZ(s);
      } else if (mode === 'drag') {
        var ty = e.clientY - downY, tx = e.clientX - downX;
        if (ty > 8 && ty > Math.abs(tx) * 1.3 && s._z.s <= 1.01) {
          swY = ty;
          track.style.transform = 'translateY(' + swY + 'px)';
          track.style.opacity = String(Math.max(0.3, 1 - swY / 550));
        }
      }
    });

    function endG(e) {
      if (!(e.pointerId in pts)) return;
      delete pts[e.pointerId];
      if (mode === 'pinch') {
        np = Math.max(0, np - 1);
        if (np < 2) {
          if (s._z.s < 1.15) { s._z = { s: 1, x: 0, y: 0 }; }
          s._st.classList.add('anim'); clampZ(s); applyZ(s);
          setTimeout(function () { s._st.classList.remove('anim'); }, 260);
          mode = ''; pinch = null;
        }
        return;
      }
      np = Math.max(0, np - 1);
      var dt = Math.max(1, Date.now() - downT);
      if (swY > 0) {
        var vy = swY / dt;
        if (swY > 110 || vy > 0.55) { closeSwipe(); }
        else {
          track.style.transition = 'transform .2s ease,opacity .2s ease';
          track.style.transform = ''; track.style.opacity = '';
          setTimeout(function () { track.style.transition = ''; }, 240);
        }
        swY = 0; mode = ''; return;
      }
      if (moved < 10 && dt < 300) tap(s, e, hit);
      mode = '';
    }
    function cancelG(e) {
      if (!(e.pointerId in pts)) { if (swY > 0) { track.style.transform = ''; track.style.opacity = ''; swY = 0; } return; }
      delete pts[e.pointerId]; np = Math.max(0, np - 1);
      if (swY > 0) {
        track.style.transition = 'transform .2s ease,opacity .2s ease';
        track.style.transform = ''; track.style.opacity = '';
        setTimeout(function () { track.style.transition = ''; }, 240);
        swY = 0;
      }
      mode = '';
    }
    s.addEventListener('pointerup', endG);
    s.addEventListener('pointercancel', cancelG);
  }

  function tap(s, e, hit) {
    var now = Date.now();
    if (s._v) { toggleVideo(s, s._v); return; }
    if (now - lastTapOf(s) < 300) {
      clearTimeout(s._tapTimer); lastTap[s._i] = 0;
      var z = s._z;
      if (z.s > 1.01) { resetZoom(s); return; }
      var r = s.getBoundingClientRect();
      var fx = e.clientX - (r.left + r.width / 2), fy = e.clientY - (r.top + r.height / 2);
      z.s = 2.5; z.x = fx * (1 - z.s); z.y = fy * (1 - z.s);
      s._st.classList.add('anim'); clampZ(s); applyZ(s);
      setTimeout(function () { s._st.classList.remove('anim'); }, 260);
      return;
    }
    lastTap[s._i] = now;
    s._tapTimer = setTimeout(function () {
      if (hit === s._media) toggleUI(); else closeLightbox();
    }, 300);
  }
  var lastTap = {};
  function lastTapOf(s) { return lastTap[s._i] || 0; }
  function toggleUI() { if (lb.classList.contains('hide-ui')) showUI(); else { clearTimeout(uiTimer); lb.classList.add('hide-ui'); } }

  function playVideo(s, v) {
    var p = v.play();
    if (p && p.catch) p.catch(function () { v.muted = true; var q = v.play(); if (q && q.catch) q.catch(function () {}); });
    s.classList.remove('paused');
  }
  function toggleVideo(s, v) {
    if (v.paused || v.ended) playVideo(s, v);
    else { v.pause(); s.classList.add('paused'); }
  }
  function activate(i) {
    var nodes = track.children;
    for (var j = 0; j < nodes.length; j++) {
      if (j !== i) { var v = nodes[j]._v; if (v) v.pause(); resetZoom(nodes[j]); }
    }
    var c = nodes[i];
    if (c && c._v) playVideo(c, c._v);
  }
  function pauseAll() {
    var nodes = track.children;
    for (var j = 0; j < nodes.length; j++) { var v = nodes[j]._v; if (v) v.pause(); }
  }

  function openLightbox(idx) {
    slides = list();
    if (!slides.length) return;
    cur = Math.max(0, Math.min(idx || 0, slides.length - 1));
    var frag = document.createDocumentFragment();
    for (var i = 0; i < slides.length; i++) { var sl = buildSlide(slides[i], i); sl._i = i; frag.appendChild(sl); }
    track.replaceChildren(frag);
    lb.hidden = false;
    document.documentElement.style.overflow = 'hidden';
    requestAnimationFrame(function () {
      track.scrollLeft = cur * (track.clientWidth || 1);
      track.focus();
      paintRail();
      activate(cur);
      showUI();
    });
  }
  function closeLightbox() {
    lb.hidden = true;
    lb.classList.remove('hide-ui');
    document.documentElement.style.overflow = '';
    pauseAll();
    track.style.transform = ''; track.style.opacity = ''; track.style.transition = '';
    track.replaceChildren();
    slides = []; cur = 0;
    clearTimeout(uiTimer);
  }
  function closeSwipe() {
    track.style.transition = 'transform .22s ease,opacity .22s ease';
    track.style.transform = 'translateY(110vh)';
    track.style.opacity = '0';
    setTimeout(closeLightbox, 200);
  }

  var scrollTimer = null;
  track.addEventListener('scroll', function () {
    if (scrollTimer) return;
    scrollTimer = setTimeout(function () {
      scrollTimer = null;
      var w = track.clientWidth || 1;
      var i = Math.round(track.scrollLeft / w);
      if (i !== cur && slides[i]) { cur = i; setURL(typeFilter(), slides[cur].id); paintRail(); activate(cur); }
    }, 90);
  }, { passive: true });

  function navTo(delta) {
    var w = track.clientWidth || 1;
    var t = Math.round(track.scrollLeft / w) + delta;
    if (t < 0 || t >= slides.length) return;
    track.scrollTo({ left: t * w, behavior: 'smooth' });
  }
  if (prevBtn) prevBtn.addEventListener('click', function () { navTo(-1); });
  if (nextBtn) nextBtn.addEventListener('click', function () { navTo(1); });

  var wheelLock = 0;
  lb.addEventListener('wheel', function (e) {
    if (lb.hidden) return;
    var now = Date.now();
    if (now - wheelLock < 280 || Math.abs(e.deltaY) < 4) return;
    wheelLock = now;
    navTo(e.deltaY > 0 ? 1 : -1);
    e.preventDefault();
  }, { passive: false });

  if (likeBtn) likeBtn.addEventListener('click', function () {
    var key = keyOf(cur); if (!key) return;
    var v = myVote(key), dl = 0, dd = 0;
    if (v === 1) { dl = -1; setMyVote(key, 0); }
    else { dl = 1; if (v === -1) dd = -1; setMyVote(key, 1); }
    postReact(key, dl, dd);
  });
  if (dislikeBtn) dislikeBtn.addEventListener('click', function () {
    var key = keyOf(cur); if (!key) return;
    var v = myVote(key), dl = 0, dd = 0;
    if (v === -1) { dd = -1; setMyVote(key, 0); }
    else { dd = 1; if (v === 1) dl = -1; setMyVote(key, -1); }
    postReact(key, dl, dd);
  });
  if (favBtn) favBtn.addEventListener('click', function () {
    var key = keyOf(cur); if (!key) return;
    var f = !isFav(key); setFav(key, f); favBtn.classList.toggle('on', f);
  });
  var closeBtn = document.getElementById('lb-close');
  if (closeBtn) closeBtn.addEventListener('click', closeLightbox);
  lb.addEventListener('mousemove', function () { if (!lb.hidden) showUI(); });
  document.addEventListener('keydown', function (e) {
    if (lb.hidden) return;
    if (e.key === 'Escape') closeLightbox();
    else if (e.key === 'ArrowRight' || e.key === 'j') { e.preventDefault(); navTo(1); }
    else if (e.key === 'ArrowLeft' || e.key === 'k') { e.preventDefault(); navTo(-1); }
  });

  renderGrid();
})();
