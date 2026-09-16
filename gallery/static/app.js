/* twitter-pic gallery 前端：网格分页 + 手机式全屏查看器（横向翻页 / 双指与双击缩放 / 下滑关闭 / chrome 自动隐藏）+ 赞/踩/喜欢 */
(function () {
  'use strict';

  var dataEl = document.getElementById('g-data');
  var grid = document.getElementById('g-grid');
  if (!dataEl || !grid) return;

  var data;
  try { data = JSON.parse(dataEl.textContent); } catch (e) { return; }

  var aTags = {}; // 账号标签展示计数 {tag: count}（user_tags 权重 + 投票合并，服务端算好）
  var aTagsEl = document.getElementById('g-atags');
  var aTagsData = document.getElementById('g-atags-data');
  try { if (aTagsData) aTags = JSON.parse(aTagsData.textContent) || {}; } catch (e) {}

  var VIDEO_BASE = 'https://twimg.l.moonchan.xyz:8443';
  function videoURL(raw) {
    if (!raw) return '';
    try {
      var u = new URL(raw);
      return VIDEO_BASE + u.pathname + (u.search || '');
    } catch (e) {
      return raw;
    }
  }

  // 图片（含头像）改写：pbs.twimg.com → MEDIA_BASE（服务端 data-base 下发，默认 pbs.moonchan.xyz）。
  // 与 Go 侧 mediaURL 同规则：只改 pbs.twimg.com 的 host，路径与 query 原样保留。
  var MEDIA_BASE = (grid.getAttribute('data-base') || '').replace(/\/+$/, '');
  function mediaURL(raw) {
    if (!raw || !MEDIA_BASE) return raw;
    try {
      var u = new URL(raw);
      if (u.host === 'pbs.twimg.com') return MEDIA_BASE + u.pathname + (u.search || '');
    } catch (e) {}
    return raw;
  }

  var slug = grid.getAttribute('data-slug') || '';
  var PER = parseInt(grid.getAttribute('data-per') || '12', 10) || 12;
  var all = (data.timeline || []).filter(function (m) { return m && m.url; }).map(function (m) {
    var isVid = (m.type === 'video' || m.type === 'animated_gif');
    return {
      url: isVid ? videoURL(m.url) : mediaURL(m.url),
      id: (m.tweet_id || 0),
      video: isVid
    };
  });

  /* ---------- URL 状态：type 过滤 + tweet-id 游标 ---------- */
  function qs() { return new URLSearchParams(location.search); }
  function typeFilter() {
    var t = qs().get('type');
    if (t === 'all') return 'all';
    if (t === 'video') return 'video';
    return 'photo'; // 默认图片 only
  }
  function cursorID() { var n = parseInt(qs().get('cursor') || '', 10); return (isFinite(n) && n > 0) ? n : 0; }
  function list() {
    var t = typeFilter();
    if (t === 'all') return all;
    return all.filter(function (m) { return t === 'video' ? m.video : !m.video; });
  }
  function clampStart(arr, s) { if (!arr.length) return 0; var last = ((arr.length - 1) / PER | 0) * PER; return Math.min(Math.max(0, s), last); }
  function href(t, c) {
    var u = '/u/' + encodeURIComponent(slug), p = [];
    if (t) p.push('type=' + encodeURIComponent(t));
    if (c > 0) p.push('cursor=' + c);
    return p.length ? u + '?' + p.join('&') : u;
  }
  function setURL(t, c) { history.replaceState(null, '', href(t, c)); }

  /* ---- 动态瀑布流系统：基于真实高度动态平衡各列，实时监听图片/视频加载尺寸 ---- */
  var aspectCache = {};
  try {
    var savedAspect = sessionStorage.getItem('tw_aspect_cache');
    if (savedAspect) aspectCache = JSON.parse(savedAspect) || {};
  } catch (e) {}

  function setAspect(url, ratio) {
    if (!url || !ratio || aspectCache[url] === ratio) return;
    aspectCache[url] = ratio;
    try {
      sessionStorage.setItem('tw_aspect_cache', JSON.stringify(aspectCache));
    } catch (e) {}
  }

  function guessAspectFromURL(url) {
    if (!url) return '';
    var match = url.match(/\/(\d{2,4})x(\d{2,4})\//);
    if (match && match[1] && match[2]) {
      return match[1] + ' / ' + match[2];
    }
    // twitter 图片没有尺寸段，给个默认占位比，避免初始 0 高度导致瀑布流全挤一列→load 后重排闪烁
    return '1 / 1';
  }

  function nCols() {
    var w = grid.clientWidth || window.innerWidth || 900;
    // 移动端手机屏 (w < 560px) 保证 2 列瀑布流，超小屏兜底 1 列，平板 3 列，桌面 4-6 列
    var minCol = w < 560 ? 155 : (w < 860 ? 220 : 280);
    return Math.max(1, Math.min(8, Math.floor(w / minCol)));
  }

  function thumbURL(url) {
    if (!url) return '';
    return url.replace(/([?&]name=)[^&]+/, '$1small');
  }

  function mediaNode(m, controls, isThumb) {
    var n;
    var url = (!m.video && isThumb) ? thumbURL(m.url) : m.url;
    var cachedRatio = aspectCache[m.url] || (m.video ? guessAspectFromURL(m.url) : guessAspectFromURL(m.url));
    if (cachedRatio) aspectCache[m.url] = cachedRatio;
    if (m.video) {
      n = document.createElement('video');
      n.controls = !!controls;
      n.playsInline = true;
      n.preload = controls ? 'none' : 'metadata';
      if (cachedRatio) n.style.aspectRatio = cachedRatio;
      n.addEventListener('loadedmetadata', function () {
        if (n.videoWidth && n.videoHeight) {
          setAspect(m.url, n.videoWidth + ' / ' + n.videoHeight);
          n.style.aspectRatio = n.videoWidth + ' / ' + n.videoHeight;
          scheduleWaterfall();
        }
      });
      n.addEventListener('error', function () { scheduleWaterfall(); });
    } else {
      n = document.createElement('img');
      n.loading = 'lazy';
      n.alt = '';
      n.draggable = false;
      if (cachedRatio) n.style.aspectRatio = cachedRatio;
      n.addEventListener('load', function () {
        if (n.naturalWidth && n.naturalHeight) {
          setAspect(m.url, n.naturalWidth + ' / ' + n.naturalHeight);
          n.style.aspectRatio = n.naturalWidth + ' / ' + n.naturalHeight;
          scheduleWaterfall();
        }
      });
      n.addEventListener('error', function () { scheduleWaterfall(); });
      if (n.complete && n.naturalWidth && n.naturalHeight) {
        setAspect(m.url, n.naturalWidth + ' / ' + n.naturalHeight);
        n.style.aspectRatio = n.naturalWidth + ' / ' + n.naturalHeight;
      }
    }
    n.src = url;
    return n;
  }

  /* ---------- 网格分页 ---------- */
  var info = document.getElementById('g-info');
  var countEl = document.getElementById('g-count');
  var prevA = document.getElementById('g-prev');
  var nextA = document.getElementById('g-next');
  var modes = Array.prototype.slice.call(document.querySelectorAll('[data-mode]'));

  var cardRO = (typeof ResizeObserver !== 'undefined') ? new ResizeObserver(function () {
    scheduleWaterfall();
  }) : null;

  function gridCard(m, gIdx) {
    var d = document.createElement('div');
    d.className = 'card' + (m.video ? ' has-video' : '');
    d.dataset.gidx = gIdx;
    var n = mediaNode(m, false, true);
    n.addEventListener('click', function () { openLightbox(gIdx); });
    d.appendChild(n);
    if (cardRO) cardRO.observe(d);
    return d;
  }

  var wfRaf = 0;
  function scheduleWaterfall() {
    if (wfRaf) cancelAnimationFrame(wfRaf);
    wfRaf = requestAnimationFrame(function () {
      wfRaf = 0;
      balanceWaterfall();
    });
  }

  function balanceWaterfall() {
    var cols = Array.prototype.slice.call(grid.querySelectorAll('.gcol'));
    if (cols.length < 2) return;
    var cards = Array.prototype.slice.call(grid.querySelectorAll('.card'));
    if (!cards.length) return;

    // 按原始序号排序，保持瀑布流由上至下的流向次序
    cards.sort(function (a, b) { return (+a.dataset.gidx) - (+b.dataset.gidx); });

    var n = cols.length;
    var cardHeights = cards.map(function (c) {
      return c.getBoundingClientRect().height || c.offsetHeight || 220;
    });

    var colHeights = new Array(n).fill(0);
    var assignments = [];
    for (var c = 0; c < n; c++) assignments.push([]);

    for (var i = 0; i < cards.length; i++) {
      var minCol = 0;
      for (var c = 1; c < n; c++) {
        if (colHeights[c] < colHeights[minCol]) minCol = c;
      }
      assignments[minCol].push(cards[i]);
      var gap = (window.innerWidth < 640 ? 8 : 16);
      colHeights[minCol] += cardHeights[i] + gap;
    }

    var anyChange = false;
    for (var c = 0; c < n; c++) {
      var currentChildren = Array.prototype.slice.call(cols[c].children);
      var targetCards = assignments[c];
      if (currentChildren.length !== targetCards.length) {
        anyChange = true;
        break;
      }
      for (var k = 0; k < targetCards.length; k++) {
        if (currentChildren[k] !== targetCards[k]) {
          anyChange = true;
          break;
        }
      }
      if (anyChange) break;
    }
    if (!anyChange) return;

    for (var c = 0; c < n; c++) {
      if (cols[c].replaceChildren) {
        cols[c].replaceChildren.apply(cols[c], assignments[c]);
      } else {
        while (cols[c].firstChild) cols[c].removeChild(cols[c].firstChild);
        for (var k = 0; k < assignments[c].length; k++) {
          cols[c].appendChild(assignments[c][k]);
        }
      }
    }
  }

  function renderGrid() {
    if (cardRO) cardRO.disconnect();
    var arr = list();
    var start = clampStart(arr, cursorID());
    var end = Math.min(start + PER, arr.length);
    var n = nCols();
    var cols = [];
    for (var i = 0; i < n; i++) {
      var c = document.createElement('div');
      c.className = 'gcol';
      cols.push(c);
    }
    var cards = [];
    for (var i = start; i < end; i++) {
      cards.push(gridCard(arr[i], i));
    }
    var frag = document.createDocumentFragment();
    for (var i = 0; i < n; i++) frag.appendChild(cols[i]);
    grid.replaceChildren(frag);

    for (var i = 0; i < cards.length; i++) {
      cols[i % n].appendChild(cards[i]);
    }

    scheduleWaterfall();

    if (countEl) countEl.textContent = arr.length + ' media';
    if (info) info.textContent = arr.length ? (start + 1) + '–' + end + ' / ' + arr.length : '0';
    if (prevA) { prevA.href = href(typeFilter(), start > 0 ? Math.max(0, start - PER) : 0); prevA.hidden = !(start > 0); }
    if (nextA) { nextA.href = href(typeFilter(), end < arr.length ? end : 0); nextA.hidden = !(end < arr.length); }
    modes.forEach(function (a) { a.classList.toggle('active', a.getAttribute('data-mode') === typeFilter()); });
  }

  var gridResTimer = 0;
  window.addEventListener('resize', function () {
    clearTimeout(gridResTimer);
    gridResTimer = setTimeout(function () {
      if (lb.hidden) {
        if (nCols() !== grid.querySelectorAll('.gcol').length) {
          renderGrid();
        } else {
          scheduleWaterfall();
        }
      }
    }, 100);
  });
  function go(t, c) { setURL(t, c); renderGrid(); }

  modes.forEach(function (a) {
    a.addEventListener('click', function (e) { e.preventDefault(); go(a.getAttribute('data-mode') || '', 0); });
  });
  if (prevA) prevA.addEventListener('click', function (e) {
    e.preventDefault(); var arr = list(), start = clampStart(arr, cursorID());
    go(typeFilter(), start > 0 ? Math.max(0, start - PER) : 0);
  });
  if (nextA) nextA.addEventListener('click', function (e) {
    e.preventDefault(); var arr = list(), start = clampStart(arr, cursorID()), end = Math.min(start + PER, arr.length);
    go(typeFilter(), end < arr.length ? end : 0);
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
  function myATagVote(tag) { return parseInt(lg('gallery:atagvote:' + slug + ':' + tag, '0'), 10) || 0; }
  function setMyATagVote(tag, v) { ls('gallery:atagvote:' + slug + ':' + tag, String(v || 0)); }
  function voteUIShown() { return lg('gallery:atagvote-ui', '1') === '1'; }
  function setVoteUIShown(on) { ls('gallery:atagvote-ui', on ? '1' : '0'); }
  // 乐观更新：静态示例（无后端，POST 404）时也能看到计数变化；真机随后被服务端返回覆盖。
  function applyATagLocal(tag, delta) {
    var n = (aTags[tag] || 0) + delta;
    if (n > 0) { aTags[tag] = n; } else { delete aTags[tag]; }
  }
  function escT(s) { return String(s).replace(/[&<>"]/g, function (c) { return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' }[c]; }); }
  // 与 twitter API 同口径：单次写请求对单标签最多贡献 ±1（服务端同幅限幅）。
  // 反向切换（本地票 +1 到 -1，净差 ±2）拆成两笔同向 ±1；同向增量可交换，到达顺序无关。
  // 发的是**本访客对该标签的目标值**（-1 | 0 | +1，0=撤票），不是差值。
  // 服务端按 (账号,标签,IP) 记票并对同值幂等，所以：
  //   - 不需要再"拆两笔同向 ±1"去绕开每请求 ±1 限幅——那是旧累加语义下的补丁，
  //     目标值语义下反向改票（+1 → -1）本就该是一笔请求（净变化 ±2）；
  //   - 重复点击/重发都是幂等的，服务端不依赖这里的 localStorage。
  // 本地的乐观更新只管即时反馈，最终权重一律以响应里的 tags 为权威（整份覆盖）。
  function postATag(tag, target) {
    fetch('/api/twitter/tag', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ user: slug, tag: tag, d: target })
    }).then(function (r) { return r.json(); })
      .then(function (j) { if (j && j.tags) { aTags = j.tags; renderATags(); } })
      .catch(function () {});
  }
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
  function renderATags() {
    if (!aTagsEl) return;
    var show = voteUIShown();
    // account_tags 保留负权重（与 twitter API 的 GET 同口径），但站点只显示正分，
    // 与首页卡片/标签筛选、以及 applyATagLocal 的隐藏规则一致。
    var names = Object.keys(aTags).filter(function (t) { return aTags[t] > 0; });
    names.sort(function (a, b) { return aTags[b] - aTags[a] || (a < b ? -1 : 1); });
    var html = '';
    for (var i = 0; i < names.length; i++) {
      var t = names[i], v = myATagVote(t);
      var cls = 'atag' + (v === 1 ? ' up' : v === -1 ? ' down' : '');
      html += '<span class="' + cls + '"><span>#' + escT(t) + '</span> <b>' + aTags[t] + '</b>';
      if (show) {
        html += '<button class="atag-vote up' + (v === 1 ? ' on' : '') + '" data-t="' + escT(t) + '" data-d="1" title="' + (v === 1 ? '取消投票' : '投一票') + '">+</button>' +
          '<button class="atag-vote down' + (v === -1 ? ' on' : '') + '" data-t="' + escT(t) + '" data-d="-1" title="' + (v === -1 ? '取消投票' : '减一票') + '">\u2212</button>';
      }
      html += '</span>';
    }
    html += '<button class="atag atag-toggle' + (show ? ' on' : '') + '" id="g-atagtoggle" type="button" title="显示/隐藏投票按钮">\u00B1 ' + (show ? '收起投票' : '投票') + '</button>';
    html += '<button class="atag atag-add" id="g-atagadd" type="button" title="添加标签">\uFF0B 标签</button>';
    aTagsEl.innerHTML = html;
    var tg = document.getElementById('g-atagtoggle');
    if (tg) tg.addEventListener('click', function () { setVoteUIShown(!voteUIShown()); renderATags(); });
    var add = document.getElementById('g-atagadd');
    if (add) add.addEventListener('click', function () {
      var inp = document.getElementById('g-ataginput');
      if (!inp) return;
      inp.hidden = !inp.hidden;
      if (!inp.hidden) inp.focus();
    });
    var votes = aTagsEl.querySelectorAll('.atag-vote');
    for (var k = 0; k < votes.length; k++) {
      (function (btn) {
        btn.addEventListener('click', function () {
          var t = btn.getAttribute('data-t');
          var dir = parseInt(btn.getAttribute('data-d'), 10) || 1;
          var cur = myATagVote(t);
          // 该方向已投 -> 目标值 0（撤票）；否则目标值就是该方向本身（含反向改票）。
          var next = (dir === 1) ? (cur === 1 ? 0 : 1) : (cur === -1 ? 0 : -1);
          if (next === cur) return;
          setMyATagVote(t, next);
          applyATagLocal(t, next - cur); // 只为即时反馈，真实值看响应
          renderATags();
          postATag(t, next);
        });
      })(votes[k]);
    }
  }
  function initAccountTags() {
    var inp = document.getElementById('g-ataginput');
    if (!inp) return;
    inp.addEventListener('keydown', function (e) {
      if (e.key !== 'Enter') return;
      var t = inp.value.trim().replace(/^#/, '');
      if (!t) return;
      inp.value = '';
      var cur = myATagVote(t);
      if (cur === 1) return; // 已经是 +1：目标值没变，服务端会幂等，但省一趟请求
      setMyATagVote(t, 1);
      applyATagLocal(t, 1 - cur);
      renderATags();
      postATag(t, 1); // 目标值：从 -1 直接改成 +1 也是一笔（旧累加语义要拆两笔）
    });
    inp.addEventListener('blur', function () { inp.hidden = true; });
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
    if (s._v) {
      if (now - lastTapOf(s) < 280) {
        clearTimeout(s._tapTimer);
        lastTap[s._i] = 0;
        var r = s.getBoundingClientRect();
        var clickX = e.clientX - r.left;
        if (s._v.duration) {
          if (clickX < r.width * 0.38) {
            s._v.currentTime = Math.max(0, s._v.currentTime - 5);
            syncHudState(s._v);
            showUI();
            return;
          } else if (clickX > r.width * 0.62) {
            s._v.currentTime = Math.min(s._v.duration, s._v.currentTime + 5);
            syncHudState(s._v);
            showUI();
            return;
          }
        }
      }
      lastTap[s._i] = now;
      s._tapTimer = setTimeout(function () {
        if (lb.classList.contains('hide-ui')) {
          showUI();
        } else {
          toggleVideo(s, s._v);
        }
      }, 240);
      return;
    }
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

  /* ---------- 视频播放器 HUD 控制器 ---------- */
  var vHud = document.getElementById('lb-vhud');
  var vProg = document.getElementById('lb-vprog');
  var vBuf = document.getElementById('lb-vbuf');
  var vBar = document.getElementById('lb-vbar');
  var vThumb = document.getElementById('lb-vthumb');
  var vTip = document.getElementById('lb-vtip');
  var vPlay = document.getElementById('lb-vplay');
  var vTime = document.getElementById('lb-vtime');
  var vMute = document.getElementById('lb-vmute');
  var vSlider = document.getElementById('lb-vslider');
  var vSpeed = document.getElementById('lb-vspeed');
  var vFS = document.getElementById('lb-vfs');

  var activeVideo = null;
  var isScrubbing = false;

  function formatTime(sec) {
    if (!isFinite(sec) || sec < 0) return '00:00';
    var m = Math.floor(sec / 60);
    var s = Math.floor(sec % 60);
    return (m < 10 ? '0' : '') + m + ':' + (s < 10 ? '0' : '') + s;
  }

  function syncHudState(v) {
    if (!vHud || !v) return;
    var isPaused = v.paused || v.ended;
    vHud.classList.toggle('paused', isPaused);
    var isMuted = v.muted || v.volume === 0;
    vHud.classList.toggle('muted', isMuted);
    if (vSlider && !isScrubbing) vSlider.value = isMuted ? 0 : v.volume;

    var curT = v.currentTime || 0;
    var dur = v.duration || 0;
    if (vTime) vTime.textContent = formatTime(curT) + ' / ' + formatTime(dur);

    if (!isScrubbing && dur > 0) {
      var pct = Math.min(100, Math.max(0, (curT / dur) * 100));
      if (vBar) vBar.style.width = pct + '%';
      if (vThumb) vThumb.style.left = pct + '%';
    }

    if (vBuf && dur > 0 && v.buffered && v.buffered.length > 0) {
      try {
        var bufEnd = v.buffered.end(v.buffered.length - 1);
        var bPct = Math.min(100, Math.max(0, (bufEnd / dur) * 100));
        vBuf.style.width = bPct + '%';
      } catch (e) {}
    }

    if (vSpeed) vSpeed.textContent = (v.playbackRate || 1) + 'x';

    var isFS = !!(document.fullscreenElement || document.webkitFullscreenElement);
    vHud.classList.toggle('is-fs', isFS);
  }

  function onVTimeUpdate() { if (activeVideo) syncHudState(activeVideo); }
  function onVProgress() { if (activeVideo) syncHudState(activeVideo); }
  function onVPlay() { if (activeVideo) syncHudState(activeVideo); }
  function onVPause() { if (activeVideo) syncHudState(activeVideo); }
  function onVVol() { if (activeVideo) syncHudState(activeVideo); }
  function onVMeta() { if (activeVideo) syncHudState(activeVideo); }

  function bindVideoHud(v) {
    if (!vHud) return;
    if (activeVideo && activeVideo !== v) unbindVideoHud();
    activeVideo = v;
    lb.classList.add('has-vhud');
    vHud.hidden = false;
    syncHudState(v);

    v.addEventListener('timeupdate', onVTimeUpdate);
    v.addEventListener('progress', onVProgress);
    v.addEventListener('play', onVPlay);
    v.addEventListener('pause', onVPause);
    v.addEventListener('volumechange', onVVol);
    v.addEventListener('loadedmetadata', onVMeta);
  }

  function unbindVideoHud() {
    if (!vHud) return;
    if (activeVideo) {
      activeVideo.removeEventListener('timeupdate', onVTimeUpdate);
      activeVideo.removeEventListener('progress', onVProgress);
      activeVideo.removeEventListener('play', onVPlay);
      activeVideo.removeEventListener('pause', onVPause);
      activeVideo.removeEventListener('volumechange', onVVol);
      activeVideo.removeEventListener('loadedmetadata', onVMeta);
    }
    activeVideo = null;
    vHud.hidden = true;
    lb.classList.remove('has-vhud');
  }

  if (vHud) {
    vHud.addEventListener('pointerdown', function (e) { e.stopPropagation(); showUI(); });
    vHud.addEventListener('click', function (e) { e.stopPropagation(); showUI(); });
    vHud.addEventListener('mousemove', function () { showUI(); });
  }

  if (vPlay) {
    vPlay.addEventListener('click', function (e) {
      e.stopPropagation();
      if (!activeVideo) return;
      if (activeVideo.paused || activeVideo.ended) {
        activeVideo.play();
      } else {
        activeVideo.pause();
      }
      showUI();
    });
  }

  if (vMute) {
    vMute.addEventListener('click', function (e) {
      e.stopPropagation();
      if (!activeVideo) return;
      activeVideo.muted = !activeVideo.muted;
      if (!activeVideo.muted && activeVideo.volume === 0) {
        activeVideo.volume = 1;
      }
      syncHudState(activeVideo);
      showUI();
    });
  }

  if (vSlider) {
    vSlider.addEventListener('input', function (e) {
      e.stopPropagation();
      if (!activeVideo) return;
      var val = parseFloat(vSlider.value);
      activeVideo.volume = val;
      activeVideo.muted = (val === 0);
      syncHudState(activeVideo);
      showUI();
    });
  }

  var speedList = [1, 1.25, 1.5, 2, 0.5];
  if (vSpeed) {
    vSpeed.addEventListener('click', function (e) {
      e.stopPropagation();
      if (!activeVideo) return;
      var curSpd = activeVideo.playbackRate || 1;
      var idx = speedList.indexOf(curSpd);
      var nextSpd = speedList[(idx + 1) % speedList.length];
      activeVideo.playbackRate = nextSpd;
      vSpeed.textContent = nextSpd + 'x';
      showUI();
    });
  }

  if (vFS) {
    vFS.addEventListener('click', function (e) {
      e.stopPropagation();
      if (!activeVideo) return;
      if (document.fullscreenElement || document.webkitFullscreenElement) {
        if (document.exitFullscreen) document.exitFullscreen();
        else if (document.webkitExitFullscreen) document.webkitExitFullscreen();
      } else {
        var el = activeVideo;
        if (el.requestFullscreen) el.requestFullscreen();
        else if (el.webkitRequestFullscreen) el.webkitRequestFullscreen();
        else if (el.webkitEnterFullscreen) el.webkitEnterFullscreen();
      }
      showUI();
    });
  }

  if (vProg) {
    function seekByProgEvent(e) {
      if (!activeVideo || !activeVideo.duration) return;
      var rect = vProg.getBoundingClientRect();
      var pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
      activeVideo.currentTime = pct * activeVideo.duration;
      if (vBar) vBar.style.width = (pct * 100) + '%';
      if (vThumb) vThumb.style.left = (pct * 100) + '%';
      if (vTime) vTime.textContent = formatTime(activeVideo.currentTime) + ' / ' + formatTime(activeVideo.duration);
    }

    vProg.addEventListener('pointerdown', function (e) {
      e.stopPropagation();
      isScrubbing = true;
      vProg.classList.add('scrubbing');
      try { vProg.setPointerCapture(e.pointerId); } catch (err) {}
      seekByProgEvent(e);
      showUI();
    });
    vProg.addEventListener('pointermove', function (e) {
      if (activeVideo && activeVideo.duration) {
        var rect = vProg.getBoundingClientRect();
        var pct = Math.max(0, Math.min(1, (e.clientX - rect.left) / rect.width));
        if (vTip) {
          vTip.textContent = formatTime(pct * activeVideo.duration);
          vTip.style.left = (pct * 100) + '%';
        }
      }
      if (isScrubbing) {
        e.stopPropagation();
        seekByProgEvent(e);
        showUI();
      }
    });
    function endScrub(e) {
      if (isScrubbing) {
        isScrubbing = false;
        vProg.classList.remove('scrubbing');
        try { vProg.releasePointerCapture(e.pointerId); } catch (err) {}
        if (activeVideo) syncHudState(activeVideo);
        showUI();
      }
    }
    vProg.addEventListener('pointerup', endScrub);
    vProg.addEventListener('pointercancel', endScrub);
  }

  function playVideo(s, v) {
    var p = v.play();
    if (p && p.catch) p.catch(function () { v.muted = true; var q = v.play(); if (q && q.catch) q.catch(function () {}); });
    s.classList.remove('paused');
    if (activeVideo === v) syncHudState(v);
  }
  function toggleVideo(s, v) {
    if (v.paused || v.ended) playVideo(s, v);
    else { v.pause(); s.classList.add('paused'); }
    if (activeVideo === v) syncHudState(v);
  }
  function activate(i) {
    var nodes = track.children;
    for (var j = 0; j < nodes.length; j++) {
      if (j !== i) { var v = nodes[j]._v; if (v) v.pause(); resetZoom(nodes[j]); }
    }
    var c = nodes[i];
    if (c && c._v) {
      bindVideoHud(c._v);
      playVideo(c, c._v);
    } else {
      unbindVideoHud();
    }
  }
  function pauseAll() {
    unbindVideoHud();
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
      if (i !== cur && slides[i]) { cur = i; setURL(typeFilter(), cur); paintRail(); activate(cur); }
    }, 90);
  }, { passive: true });

  function navTo(delta) {
    var w = track.clientWidth || 1;
    var t = Math.round(track.scrollLeft / w) + delta;
    if (t < 0 || t >= slides.length) return;
    if (track.scrollTo) { track.scrollTo({ left: t * w, behavior: 'smooth' }); } else { track.scrollLeft = t * w; }
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
    if (e.key === 'Escape') {
      closeLightbox();
    } else if (e.key === ' ' && activeVideo) {
      e.preventDefault();
      if (activeVideo.paused || activeVideo.ended) activeVideo.play();
      else activeVideo.pause();
      showUI();
    } else if ((e.key === 'm' || e.key === 'M') && activeVideo) {
      e.preventDefault();
      activeVideo.muted = !activeVideo.muted;
      syncHudState(activeVideo);
      showUI();
    } else if (e.key === 'ArrowRight' || e.key === 'j') {
      e.preventDefault(); navTo(1);
    } else if (e.key === 'ArrowLeft' || e.key === 'k') {
      e.preventDefault(); navTo(-1);
    }
  });

  initAccountTags();
  renderATags();
  renderGrid();
})();
