/* twitter-pic gallery 前端：网格分页 + TikTok 式竖滑全屏 + 赞/踩/喜欢 */
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
    else { n = document.createElement('img'); n.loading = 'lazy'; n.alt = ''; }
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

  /* ---------- TikTok 式竖滑全屏 ---------- */
  var lb = document.getElementById('lb');
  var track = document.getElementById('lb-track');
  var lbInfo = document.getElementById('lb-info');
  var likeBtn = document.getElementById('lb-like');
  var dislikeBtn = document.getElementById('lb-dislike');
  var favBtn = document.getElementById('lb-fav');
  var likeCount = document.getElementById('lb-likes');
  var dislikeCount = document.getElementById('lb-dislikes');

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
    if (lbInfo) lbInfo.textContent = '@' + slug + ' · ' + (cur + 1) + '/' + slides.length + (m.id ? ' · ' + m.id : '');
    loadCounts(key);
  }

  function buildSlide(m) {
    var s = document.createElement('div'); s.className = 'lb-slide';
    var n = mediaNode(m, true);
    n.classList.add('lb-media');
    s.appendChild(n);
    return s;
  }
  function openLightbox(idx) {
    slides = list();                       // 全局（受 type 过滤影响）而不是当前页
    if (!slides.length) return;
    cur = Math.max(0, Math.min(idx || 0, slides.length - 1));
    var frag = document.createDocumentFragment();
    for (var i = 0; i < slides.length; i++) frag.appendChild(buildSlide(slides[i]));
    track.replaceChildren(frag);
    lb.hidden = false;
    document.documentElement.style.overflow = 'hidden';
    requestAnimationFrame(function () { track.scrollTop = cur * track.clientHeight; track.focus(); paintRail(); });
  }
  function closeLightbox() {
    lb.hidden = true;
    document.documentElement.style.overflow = '';
    track.replaceChildren();
    slides = []; cur = 0;
  }

  var scrollTimer = null;
  track.addEventListener('scroll', function () {
    if (scrollTimer) return;
    scrollTimer = setTimeout(function () {
      scrollTimer = null;
      var h = track.clientHeight || 1;
      var i = Math.round(track.scrollTop / h);
      if (i !== cur && slides[i]) { cur = i; setURL(typeFilter(), slides[cur].id); paintRail(); }
    }, 120);
  }, { passive: true });

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
    var on = !isFav(key); setFav(key, on); favBtn.classList.toggle('on', on);
  });
  var closeBtn = document.getElementById('lb-close');
  if (closeBtn) closeBtn.addEventListener('click', closeLightbox);
  document.addEventListener('keydown', function (e) {
    if (lb.hidden) return;
    if (e.key === 'Escape') closeLightbox();
    else if (e.key === 'ArrowDown' || e.key === 'j') { e.preventDefault(); track.scrollBy({ top: track.clientHeight, behavior: 'smooth' }); }
    else if (e.key === 'ArrowUp' || e.key === 'k') { e.preventDefault(); track.scrollBy({ top: -track.clientHeight, behavior: 'smooth' }); }
  });

  renderGrid();
})();
