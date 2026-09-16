// 主页脚本：账号搜索 / 增量加载 / 随机逛逛 + 卡片预览。
// 预览（头像、昵称、banner=该账号第一张图）不进后端：前端逐个 fetch /raw/<name>
// （原样吐 json.gz），用 DecompressionStream 流式解压，扫到 account_info 和
// timeline 里第一条有 url 的媒体就中断读取，不全量 JSON.parse。
// 若页面内嵌 #a-meta（如离线导出的示例），直接当缓存用，不发请求。
(() => {
  const grid = document.getElementById("a-grid");
  const dataEl = document.getElementById("a-data");
  if (!grid || !dataEl) return;

  let names = [];
  const tagsMap = new Map(); // username -> tags（后端从 tags.db 合并进 #a-data）
  try {
    const raw = JSON.parse(dataEl.textContent) || [];
    for (const e of raw) {
      // 只接受字符串账号名；畸形条目（n 是对象等）直接丢弃。
      // 否则 cardHTML 里 String(obj) 会渲染成 "@[object Object]"，
      // n[0] 取不到 → "?"，hueOf(obj) 循环不执行 → 0 → 红色占位块。
      const n = typeof e === "string" ? e : (e && typeof e === "object" ? e.n : null);
      if (typeof n !== "string" || !n) continue;
      names.push(n);
      if (e && Array.isArray(e.t)) {
        const ts = e.t.filter((x) => typeof x === "string" && x);
        if (ts.length) tagsMap.set(n, ts);
      }
    }
  } catch (e) { return; }

  const search = document.getElementById("a-search");
  const shownEl = document.getElementById("a-shown");
  const moreBtn = document.getElementById("a-more");
  const randBtn = document.getElementById("a-rand");
  const emptyEl = document.getElementById("a-empty");
  const gayBtn = document.getElementById("a-gay-btn");
  const gayCfgBtn = document.getElementById("a-gay-cfg");
  const gayModal = document.getElementById("a-gay-modal");
  const gayModalClose = document.getElementById("a-gay-modal-close");
  const gayModalDone = document.getElementById("a-gay-modal-done");
  const gayModalReset = document.getElementById("a-gay-modal-reset");
  const gayModalTags = document.getElementById("a-gay-modal-tags");
  const gayModalStat = document.getElementById("a-gay-modal-stat");
  const gayInput = document.getElementById("a-gay-input");
  const gayAddBtn = document.getElementById("a-gay-add-btn");
  const excludeBtn = document.getElementById("a-exclude-btn");
  const excludeModal = document.getElementById("a-exclude-modal");
  const modalCloud = document.getElementById("a-modal-cloud");
  const modalClose = document.getElementById("a-modal-close");
  const modalDone = document.getElementById("a-modal-done");
  const modalClear = document.getElementById("a-modal-clear");
  const modalStat = document.getElementById("a-modal-stat");
  const STEP = 120;
  const CONC = 6;        // 并发预览拉取数
  const MAX_SEEK = 8e6;  // 单文件解压后最多扫 8MB

  const BASE = (grid.dataset.base || "").replace(/\/+$/, "");
  const cache = new Map();
  try {
    const pre = JSON.parse(document.getElementById("a-meta").textContent || "{}");
    for (const k in pre) cache.set(k, pre[k]);
  } catch (e) { /* 无预烘焙数据 */ }

  // 客户端持久缓存：首次访问流式读取，二次访问直接从本地 0ms 呈现
  const LS_KEY = "tp_meta_v1";
  let lsCache = {};
  try {
    lsCache = JSON.parse(localStorage.getItem(LS_KEY) || "{}");
    for (const k in lsCache) {
      if (!cache.has(k) && lsCache[k] && typeof lsCache[k] === "object") {
        cache.set(k, lsCache[k]);
      }
    }
  } catch (e) {}

  let saveTimer = 0;
  function persistCache(k, v) {
    if (!k || !v) return;
    lsCache[k] = v;
    clearTimeout(saveTimer);
    saveTimer = setTimeout(() => {
      try {
        const keys = Object.keys(lsCache);
        if (keys.length > 1000) {
          const trimmed = {};
          keys.slice(-800).forEach((x) => { trimmed[x] = lsCache[x]; });
          lsCache = trimmed;
        }
        localStorage.setItem(LS_KEY, JSON.stringify(lsCache));
      } catch (e) {}
    }, 400);
  }

  // 与 Go hueOf 同算法（账号名为 ASCII）
  const hueOf = (s) => { let h = 0; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 360; return h; };
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const thumbURL = (raw) => {
    if (!raw) return "";
    // Twitter 缩略图优化：卡片展示仅需 small 规格 (680px)，相比 orig 原图节省 90%+ 带宽
    return raw.replace(/([?&]name=)[^&]+/, "$1small");
  };
  const VIDEO_BASE = "https://twimg.l.moonchan.xyz:8443";
  const videoURL = (raw) => {
    if (!raw) return "";
    try { const u = new URL(raw); return VIDEO_BASE + u.pathname + (u.search || ""); } catch (e) {}
    return raw;
  };
  const mediaURL = (raw) => {
    if (!raw) return "";
    if (!BASE) return raw;
    try { const u = new URL(raw); if (u.host === "pbs.twimg.com") return BASE + u.pathname + (u.search || ""); } catch (e) {}
    return raw;
  };

  // 卡片标签**全部显示**：不截断、不折 +N。
  // 顺序与条数都不在前端二次加工：后端 ForUsers 的 SQL 已经是
  // `WHERE weight > 0 ... ORDER BY username, weight DESC, tag`（tags/tags.go），
  // 前端按数组原样渲染即为「只含正分、权重降序、同分按标签名」。
  // 注意 SSR 模板 templates/home.html 里是同一份逻辑的另一半，两处必须一起改。
  const tagPills = (n) => {
    let ts = tagsMap.get(n) || [];
    if (!ts.length) return "";
    if (excludedTags.size > 0) ts = ts.filter((t) => !excludedTags.has(t));
    if (!gayMode) ts = ts.filter((t) => !GAY_TAGS.has(t));
    if (!ts.length) return "";
    const h = ts.map((t) => '<span class="tg' + (activeTags.has(t) ? " active" : "") + '" data-t="' + esc(t) + '">#' + esc(t) + "</span>").join("");
    return '<span class="tgs">' + h + "</span>";
  };
  const cardHTML = (n) => {
    const ini = esc((n[0] || "?").toUpperCase());
    return '<a class="acct" href="/u/' + encodeURIComponent(n) + '?type=photo" data-n="' + esc(n) + '">' +
      '<span class="bnr-ph" style="--h:' + hueOf(n) + '">' + ini + "</span>" +
      '<span class="arow"><span class="av" style="--h:' + hueOf(n) + '">' + ini + "</span>" +
      '<span class="col"><span class="at">@' + esc(n) + "</span><span class=\"nk\"></span></span></span>" + tagPills(n) + "</a>";
  };

  // ---- 预览绘制 ----
  function paint(el, m) {
    el.dataset.painted = "1";
    if (!m || typeof m !== "object") return;
    const name = el.dataset.n;
    const rawB = typeof m.b === "string" ? m.b : "";
    const b = m.v ? videoURL(rawB) : mediaURL(thumbURL(rawB));
    if (b) {
      const ph = el.querySelector(".bnr-ph");
      if (ph) {
        if (m.v) {
          const videoSrc = esc(b) + (b.indexOf("#") >= 0 ? "" : "#t=0.001");
          const media = '<video class="bnr" muted loop playsinline autoplay preload="metadata" src="' + videoSrc + '"></video>';
          ph.outerHTML = media;
        } else {
          ph.outerHTML = '<img class="bnr" loading="lazy" decoding="async" alt="" src="' + esc(b) + '">';
        }
      }
    }
    const a = mediaURL(typeof m.a === "string" ? m.a : "");
    if (a) {
      const av = el.querySelector(".av");
      if (av && !av.querySelector("img")) av.insertAdjacentHTML("afterbegin", '<img alt="" src="' + esc(a) + '">');
    }
    const nick = el.querySelector(".nk");
    if (nick && typeof m.i === "string" && m.i && m.i !== name) nick.textContent = m.i;
  }

  function fallback(el) {
    el.dataset.painted = "1"; // 拉不到就保留字母占位
  }

  let inflight = 0;
  const queue = [];

  function pump() {
    while (inflight < CONC && queue.length) {
      const el = queue.shift();
      const name = el.dataset.n;
      if (cache.has(name)) { paint(el, cache.get(name)); continue; }
      inflight++;
      fetchMeta(name).then((m) => {
        cache.set(name, m);
        persistCache(name, m);
        if (document.body.contains(el)) paint(el, m);
      }).catch(() => {
        const fb = { n: name };
        cache.set(name, fb);
        persistCache(name, fb);
        if (document.body.contains(el)) fallback(el);
      }).finally(() => { inflight--; pump(); });
    }
  }

  function schedule() {
    const todo = grid.querySelectorAll(".acct:not([data-painted])");
    for (const el of todo) queue.push(el);
    pump();
  }

  // ---- 流式提取：解压前缀，拿到 account_info + 首条带 url 的 timeline 就停 ----
  /*<scan>*/ // 测试钩子：home_test.mjs 会抽取本标记内的纯函数在 Node 下运行
  function jsonSlice(s, i) { // 从 s[i]（'{'）起取配平对象；不完整返回 null
    if (s[i] !== "{" && s[i] !== "[") return null;
    const close = s[i] === "{" ? "}" : "]";
    let d = 0, inStr = false, bs = false;
    for (let j = i; j < s.length; j++) {
      const c = s[j];
      if (inStr) { if (bs) bs = false; else if (c === "\\") bs = true; else if (c === '"') inStr = false; continue; }
      if (c === '"') inStr = true;
      else if (c === s[i]) d++;
      else if (c === close && --d === 0) return s.slice(i, j + 1);
    }
    return null;
  }

  function extractMeta(text, final) {
    let info = null;
    const ai = text.indexOf('"account_info"');
    if (ai >= 0) {
      const ob = text.indexOf("{", ai);
      if (ob >= 0) {
        const o = jsonSlice(text, ob);
        if (o) { try { info = JSON.parse(o); } catch (e) { info = null; } }
        if (!o && !final) return null; // 等更多
        if (!info) info = rxInfo(text.slice(ob, ob + 4000));
      }
    } else if (!final && text.length < 65536) return null;
    info = info || {};
    let tl = null; // {url,type}
    const ti = text.indexOf('"timeline"');
    if (ti < 0) { if (!final) return null; }
    else {
      const lb = text.indexOf("[", ti);
      if (lb < 0) { if (!final) return null; }
      else if (final && /^\s*\]/.test(text.slice(lb + 1))) tl = { url: "", type: "" };
      else {
        let pos = lb + 1, scanned = 0;
        let firstMedia = null;
        for (; scanned < 60; scanned++) {
          while (pos < text.length && /[\s,]/.test(text[pos])) pos++;
          if (pos >= text.length) { break; }       // 数据不够
          if (text[pos] === "]") {                 // 列表结束
            if (firstMedia) tl = firstMedia;
            else tl = { url: "", type: "" };
            break;
          }
          if (text[pos] !== "{") { break; }
          const o = jsonSlice(text, pos);
          if (!o) {
            if (final) {
              const mm = /"url"\s*:\s*"([^"]*)"/.exec(text.slice(pos, pos + 4000));
              if (mm) {
                const mt = (/"type"\s*:\s*"([^"]*)"/.exec(text.slice(pos, pos + 4000)) || [])[1] || "";
                if (!firstMedia) firstMedia = { url: mm[1], type: mt };
                if (!/video|gif/i.test(mt)) { tl = { url: mm[1], type: mt }; break; }
              }
            }
            break;
          }
          const mu = /"url"\s*:\s*"([^"]*)"/.exec(o);
          if (mu && mu[1]) {
            const mt = (/"type"\s*:\s*"([^"]*)"/.exec(o) || [])[1] || "";
            const isVid = /video|gif/i.test(mt);
            if (!firstMedia) firstMedia = { url: mu[1], type: mt };
            if (!isVid) { // 优先取第一张图片作为 banner
              tl = { url: mu[1], type: mt };
              break;
            }
          }
          pos += o.length;
        }
        if (!tl && firstMedia && (final || scanned >= 20)) tl = firstMedia;
        if (final && !tl) tl = { url: "", type: "" };
      }
    }
    if (!tl && !final) return null;
    return {
      n: info.name || "", i: info.nick || "", a: info.profile_image || "",
      b: tl ? tl.url : "", v: tl ? /video|gif/i.test(tl.type) : false,
    };
  }

  function rxInfo(s) {
    const g = (k) => (new RegExp('"' + k + '"\\s*:\\s*"((?:[^"\\\\]|\\\\.)*)"').exec(s) || [])[1] || "";
    return { name: g("name"), nick: g("nick"), profile_image: g("profile_image") };
  }
  async function fetchMeta(name) {
    const url = "/api/twitter/" + encodeURIComponent(name) + ".json.gz";
    const res = await fetch(url);
    if (!res.ok) throw new Error("HTTP " + res.status);
    if (!res.body) {
      const j = await res.json().catch(() => null);
      if (!j) throw new Error("no stream");
      return extractMeta(JSON.stringify(j), true) || { n: name };
    }
    try {
      const td = new TextDecoder();
      const rd = res.body.getReader();
      let text = "", done = false;
      const first = await rd.read();
      if (first.done) return { n: name };
      const firstChunk = first.value;
      if (firstChunk && firstChunk.length >= 2 && firstChunk[0] === 0x1f && firstChunk[1] === 0x8b && window.DecompressionStream) {
        const stream = new ReadableStream({
          start(controller) {
            controller.enqueue(firstChunk);
            function pump() {
              return rd.read().then(({done, value}) => {
                if (done) { controller.close(); return; }
                controller.enqueue(value);
                return pump();
              });
            }
            return pump();
          }
        });
        const dRd = stream.pipeThrough(new DecompressionStream("gzip")).getReader();
        while (!done) {
          const r = await dRd.read();
          if (r.done) break;
          text += td.decode(r.value, { stream: true });
          const m = extractMeta(text, false);
          if (m) { dRd.cancel().catch(() => {}); return m; }
          if (text.length > MAX_SEEK) break;
        }
        return extractMeta(text, true) || { n: name };
      }
      text += td.decode(firstChunk, { stream: true });
      const m = extractMeta(text, false);
      if (m) { rd.cancel().catch(() => {}); return m; }
      while (!done) {
        const r = await rd.read();
        if (r.done) break;
        text += td.decode(r.value, { stream: true });
        const m = extractMeta(text, false);
        if (m) { rd.cancel().catch(() => {}); return m; }
        if (text.length > MAX_SEEK) break;
      }
      return extractMeta(text, true) || { n: name };
    } catch (e) {
      const j = await res.json().catch(() => null);
      if (j) return extractMeta(JSON.stringify(j), true) || { n: name };
      throw e;
    }
  }
  /*</scan>*/

  // ---- 列表 / 搜索 / 分页 ----
  let view = names, rendered = 0;

  function renderChunk() {
    const slice = view.slice(rendered, rendered + STEP);
    if (slice.length) {
      grid.insertAdjacentHTML("beforeend", slice.map(cardHTML).join(""));
      rendered += slice.length;
      schedule();
    }
    update();
  }

  function reset(list) {
    view = list; rendered = 0;
    grid.innerHTML = "";
    renderChunk();
  }

  function update() {
    if (shownEl) shownEl.textContent = "显示 " + rendered + " / " + view.length + " 个账号";
    if (moreBtn) moreBtn.hidden = rendered >= view.length;
    if (emptyEl) emptyEl.hidden = view.length > 0;
  }

  // ---- 标签分类与排除 / Gay 模式（tag 数据由后端 tags.db 合并进 #a-data）----
  const tagbar = document.getElementById("a-tags");
  const UNTAGGED = "__untagged__";
  const activeTags = new Set();
  const excludedTags = new Set();
  const EXCLUDE_LS_KEY = "tp_excluded_tags_v1";

  const DEFAULT_GAY_TAGS = ["男同", "男性", "露屌"];
  const GAY_TAGS = new Set(DEFAULT_GAY_TAGS);
  const GAY_TAGS_LS_KEY = "tp_gay_tags_v1";
  const GAY_LS_KEY = "tp_gay_mode_v1";
  let gayMode = false;
  try {
    gayMode = localStorage.getItem(GAY_LS_KEY) === "1" || localStorage.getItem("gay-mode") === "true";
  } catch (e) {}

  try {
    const rawGay = JSON.parse(localStorage.getItem(GAY_TAGS_LS_KEY) || localStorage.getItem("gay-tags") || "null");
    if (Array.isArray(rawGay) && rawGay.length > 0) {
      GAY_TAGS.clear();
      for (const t of rawGay) {
        if (typeof t === "string" && t.trim()) GAY_TAGS.add(t.trim().replace(/^#+/, ""));
      }
    }
  } catch (e) {}

  function persistGayTags() {
    try {
      const arr = Array.from(GAY_TAGS);
      localStorage.setItem(GAY_TAGS_LS_KEY, JSON.stringify(arr));
      localStorage.setItem("gay-tags", JSON.stringify(arr));
    } catch (e) {}
  }

  function renderGayModalTags() {
    if (!gayModalTags) return;
    if (gayModalStat) gayModalStat.textContent = "当前配置 " + GAY_TAGS.size + " 个专属标签";
    if (GAY_TAGS.size === 0) {
      gayModalTags.innerHTML = '<p class="muted">暂无专属标签</p>';
      return;
    }
    gayModalTags.innerHTML = Array.from(GAY_TAGS).map((t) => {
      return '<button class="tag gay-tag" type="button" data-del-gay="' + esc(t) + '" title="点击移除">✕ #' + esc(t) + '</button>';
    }).join("");
  }

  function openGayModal() {
    if (!gayModal) return;
    renderGayModalTags();
    gayModal.hidden = false;
    gayInput && gayInput.focus();
  }

  function closeGayModal() {
    if (!gayModal) return;
    gayModal.hidden = true;
  }

  function addGayTag(raw) {
    const t = (raw || "").trim().replace(/^#+/, "");
    if (!t || GAY_TAGS.has(t)) return;
    GAY_TAGS.add(t);
    persistGayTags();
    renderGayModalTags();
    renderTagBar();
    renderExcludeModalCloud();
    applyFilter();
    syncURL();
  }

  function removeGayTag(raw) {
    const t = (raw || "").trim().replace(/^#+/, "");
    if (!t || !GAY_TAGS.has(t)) return;
    GAY_TAGS.delete(t);
    persistGayTags();
    renderGayModalTags();
    renderTagBar();
    renderExcludeModalCloud();
    applyFilter();
    syncURL();
  }

  function resetGayTags() {
    GAY_TAGS.clear();
    for (const t of DEFAULT_GAY_TAGS) GAY_TAGS.add(t);
    persistGayTags();
    renderGayModalTags();
    renderTagBar();
    renderExcludeModalCloud();
    applyFilter();
    syncURL();
  }

  try {
    const savedEx = JSON.parse(localStorage.getItem(EXCLUDE_LS_KEY) || "[]");
    if (Array.isArray(savedEx)) {
      for (const t of savedEx) {
        if (typeof t === "string" && t.trim()) excludedTags.add(t.trim());
      }
    }
  } catch (e) {}

  try {
    const p = new URLSearchParams(location.search);
    for (const raw of p.getAll("tag")) {
      for (const t of raw.split(",")) {
        const clean = t.trim();
        if (clean && !excludedTags.has(clean) && (gayMode || !GAY_TAGS.has(clean))) activeTags.add(clean);
      }
    }
  } catch (e) {}

  function persistExcludedTags() {
    try {
      localStorage.setItem(EXCLUDE_LS_KEY, JSON.stringify(Array.from(excludedTags)));
    } catch (e) {}
  }

  function updateGayBtnUI() {
    if (!gayBtn) return;
    if (gayMode) {
      gayBtn.classList.add("active");
      gayBtn.textContent = "🌈 Gay模式 (开)";
    } else {
      gayBtn.classList.remove("active");
      gayBtn.textContent = "🌈 Gay模式";
    }
  }

  function toggleGayMode() {
    gayMode = !gayMode;
    try {
      localStorage.setItem(GAY_LS_KEY, gayMode ? "1" : "0");
      localStorage.setItem("gay-mode", gayMode ? "true" : "false");
    } catch (e) {}
    activeTags.clear();
    updateGayBtnUI();
    renderTagBar();
    renderExcludeModalCloud();
    applyFilter();
    syncURL();
  }

  function updateExcludeBtnUI() {
    if (!excludeBtn) return;
    if (excludedTags.size > 0) {
      excludeBtn.textContent = "🚫 不看这些tag (" + excludedTags.size + ")";
      excludeBtn.classList.add("active");
    } else {
      excludeBtn.textContent = "🚫 不看这些tag";
      excludeBtn.classList.remove("active");
    }
  }

  function isUserExcluded(n) {
    if (excludedTags.size === 0) return false;
    const ts = tagsMap.get(n);
    if (!ts || !ts.length) return false;
    for (const t of ts) {
      if (excludedTags.has(t)) return true;
    }
    return false;
  }

  function userHasGayTag(n) {
    const ts = tagsMap.get(n);
    if (!ts || !ts.length) return false;
    for (let i = 0; i < ts.length; i++) {
      if (GAY_TAGS.has(ts[i])) return true;
    }
    return false;
  }

  function isUserGayModeMatched(n) {
    const hasGay = userHasGayTag(n);
    return gayMode ? hasGay : !hasGay;
  }

  let tagsExpanded = false;
  const hasTag = (n, t) => (t === UNTAGGED ? !tagsMap.has(n) : (tagsMap.get(n) || []).indexOf(t) >= 0);

  // 全量标签词频统计（供排除弹窗使用，根据当前 gayMode 匹配账号词频）
  function allTagsCloud() {
    const cnt = new Map();
    for (const n of names) {
      if (!isUserGayModeMatched(n)) continue;
      const ts = tagsMap.get(n);
      if (!ts || !ts.length) continue;
      for (const t of ts) {
        if (!gayMode && GAY_TAGS.has(t)) continue;
        cnt.set(t, (cnt.get(t) || 0) + 1);
      }
    }
    return [...cnt.entries()].sort((x, y) => y[1] - x[1] || (x[0] < y[0] ? -1 : 1));
  }

  // 首页标签云统计（排除已排除标签及其对应的账号标签贡献；根据当前 gayMode 精确统计）
  function tagCloud() {
    const cnt = new Map();
    for (const n of names) {
      if (!isUserGayModeMatched(n)) continue;
      if (isUserExcluded(n)) continue;
      const ts = tagsMap.get(n);
      if (!ts || !ts.length) {
        if (!gayMode) {
          cnt.set(UNTAGGED, (cnt.get(UNTAGGED) || 0) + 1);
        }
        continue;
      }
      for (const t of ts) {
        if (excludedTags.has(t)) continue;
        if (!gayMode && GAY_TAGS.has(t)) continue;
        cnt.set(t, (cnt.get(t) || 0) + 1);
      }
    }
    return [...cnt.entries()].sort((x, y) => y[1] - x[1] || (x[0] === UNTAGGED ? 1 : y[0] === UNTAGGED ? -1 : (x[0] < y[0] ? -1 : 1)));
  }

  function renderTagBar() {
    if (!tagbar) return;
    const cloud = tagCloud();
    if (!cloud.length) { tagbar.hidden = true; return; }
    const shown = tagsExpanded || cloud.length <= 37 ? cloud : cloud.slice(0, 36);
    let html = "";
    if (activeTags.size > 0) {
      html += '<button class="tag tag-clear" type="button" data-clear="1" style="color:var(--acc-3);border-color:rgba(247,118,142,.35)">✕ 清空筛选 (' + activeTags.size + ')</button>';
    }
    html += shown.map((x) => '<button class="tag' + (activeTags.has(x[0]) ? " active" : "") + '" type="button" data-t="' +
      esc(x[0]) + '">' + (x[0] === UNTAGGED ? "未分类" : "#" + esc(x[0])) + " <span>" + x[1] + "</span></button>").join("");
    if (cloud.length > 36) html += '<button class="tag" type="button" data-expand="1">' + (tagsExpanded ? "收起 ↑" : "更多标签 ↓ " + cloud.length) + "</button>";
    tagbar.innerHTML = html;
    tagbar.hidden = false;
  }

  function renderExcludeModalCloud() {
    if (!modalCloud) return;
    const allTags = allTagsCloud();
    if (modalStat) modalStat.textContent = "已排除 " + excludedTags.size + " 个标签（共 " + allTags.length + " 个）";
    if (!allTags.length) {
      modalCloud.innerHTML = '<p class="muted">暂无可排除标签</p>';
      return;
    }
    modalCloud.innerHTML = allTags.map((x) => {
      const isEx = excludedTags.has(x[0]);
      return '<button class="tag' + (isEx ? " excluded" : "") + '" type="button" data-ex-tag="' +
        esc(x[0]) + '">' + (isEx ? "✕ #" : "#") + esc(x[0]) + " <span>" + x[1] + "</span></button>";
    }).join("");
  }

  function openExcludeModal() {
    if (!excludeModal) return;
    renderExcludeModalCloud();
    excludeModal.hidden = false;
  }

  function closeExcludeModal() {
    if (!excludeModal) return;
    excludeModal.hidden = true;
  }

  function syncURL() {
    try {
      const url = new URL(location.href);
      if (activeTags.size > 0) url.searchParams.set("tag", Array.from(activeTags).join(","));
      else url.searchParams.delete("tag");
      history.replaceState(null, "", url);
    } catch (err) { /* file:// 下忽略 */ }
  }

  function toggleTag(t) {
    if (!t || excludedTags.has(t)) return;
    if (t === UNTAGGED) {
      if (activeTags.has(UNTAGGED)) activeTags.delete(UNTAGGED);
      else {
        activeTags.clear();
        activeTags.add(UNTAGGED);
      }
    } else {
      activeTags.delete(UNTAGGED);
      if (activeTags.has(t)) activeTags.delete(t);
      else activeTags.add(t);
    }
    renderTagBar();
    applyFilter();
    syncURL();
  }

  function toggleExcludeTag(t) {
    if (!t) return;
    if (excludedTags.has(t)) {
      excludedTags.delete(t);
    } else {
      excludedTags.add(t);
      if (activeTags.has(t)) activeTags.delete(t);
    }
    persistExcludedTags();
    updateExcludeBtnUI();
    renderExcludeModalCloud();
    renderTagBar();
    applyFilter();
    syncURL();
  }

  function clearAllExcluded() {
    if (excludedTags.size === 0) return;
    excludedTags.clear();
    persistExcludedTags();
    updateExcludeBtnUI();
    renderExcludeModalCloud();
    renderTagBar();
    applyFilter();
    syncURL();
  }

  function applyFilter() {
    let list = names.filter(isUserGayModeMatched);
    if (excludedTags.size > 0) {
      list = list.filter((n) => !isUserExcluded(n));
    }
    const q = ((search && search.value.trim().toLowerCase()) || "");
    if (q) {
      const pre = [], sub = [];
      for (const n of list) {
        const l = n.toLowerCase();
        if (l.startsWith(q)) pre.push(n);
        else if (l.includes(q)) sub.push(n);
        if (pre.length >= 2000) break;
      }
      list = pre.concat(sub);
    }
    if (activeTags.size > 0) {
      list = list.filter((n) => {
        for (const t of activeTags) {
          if (!hasTag(n, t)) return false;
        }
        return true;
      });
    }
    reset(list);
  }

  updateGayBtnUI();
  updateExcludeBtnUI();
  renderTagBar();
  applyFilter(); // 接管 SSR 预览

  let timer = 0;
  search && search.addEventListener("input", () => {
    clearTimeout(timer);
    timer = setTimeout(applyFilter, 120);
  });

  gayBtn && gayBtn.addEventListener("click", toggleGayMode);
  gayCfgBtn && gayCfgBtn.addEventListener("click", openGayModal);
  gayModalClose && gayModalClose.addEventListener("click", closeGayModal);
  gayModalDone && gayModalDone.addEventListener("click", closeGayModal);
  gayModalReset && gayModalReset.addEventListener("click", resetGayTags);
  gayModal && gayModal.addEventListener("click", (e) => {
    if (e.target === gayModal) closeGayModal();
  });
  gayModalTags && gayModalTags.addEventListener("click", (e) => {
    const btn = e.target.closest("button");
    if (!btn) return;
    const t = btn.dataset.delGay || "";
    if (t) removeGayTag(t);
  });
  gayAddBtn && gayAddBtn.addEventListener("click", () => {
    if (!gayInput) return;
    addGayTag(gayInput.value);
    gayInput.value = "";
  });
  gayInput && gayInput.addEventListener("keydown", (e) => {
    if (e.key === "Enter") {
      e.preventDefault();
      addGayTag(gayInput.value);
      gayInput.value = "";
    }
  });
  excludeBtn && excludeBtn.addEventListener("click", openExcludeModal);
  modalClose && modalClose.addEventListener("click", closeExcludeModal);
  modalDone && modalDone.addEventListener("click", closeExcludeModal);
  modalClear && modalClear.addEventListener("click", clearAllExcluded);
  excludeModal && excludeModal.addEventListener("click", (e) => {
    if (e.target === excludeModal) closeExcludeModal();
  });

  modalCloud && modalCloud.addEventListener("click", (e) => {
    const btn = e.target.closest("button");
    if (!btn) return;
    const t = btn.dataset.exTag || "";
    if (t) toggleExcludeTag(t);
  });

  tagbar && tagbar.addEventListener("click", (e) => {
    const btn = e.target.closest("button");
    if (!btn) return;
    if (btn.dataset.clear !== undefined) {
      activeTags.clear();
      renderTagBar();
      applyFilter();
      syncURL();
      return;
    }
    if (btn.dataset.expand !== undefined) { tagsExpanded = !tagsExpanded; return renderTagBar(); }
    const t = btn.dataset.t || "";
    toggleTag(t);
  });

  grid && grid.addEventListener("click", (e) => {
    const tg = e.target.closest(".tg");
    if (!tg) return;
    e.preventDefault();
    e.stopPropagation();
    const t = tg.dataset.t || tg.textContent.replace(/^#/, "").trim();
    if (t) toggleTag(t);
  });

  grid && grid.addEventListener("mouseenter", (e) => {
    const card = e.target.closest(".acct");
    if (!card) return;
    const vid = card.querySelector("video.bnr");
    if (vid && vid.paused) vid.play().catch(() => {});
  }, true);
  moreBtn && moreBtn.addEventListener("click", renderChunk);

  randBtn && randBtn.addEventListener("click", () => {
    let candidates = names.filter(isUserGayModeMatched);
    if (excludedTags.size > 0) {
      candidates = candidates.filter((n) => !isUserExcluded(n));
    }
    if (!candidates.length) return;
    location.href = "/u/" + encodeURIComponent(candidates[Math.floor(Math.random() * candidates.length)]) + "?type=photo";
  });

  // 快捷键支持：ESC 关闭弹窗，斜杠键聚焦搜索框
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") {
      if (excludeModal && !excludeModal.hidden) {
        e.preventDefault();
        closeExcludeModal();
        return;
      }
      if (gayModal && !gayModal.hidden) {
        e.preventDefault();
        closeGayModal();
        return;
      }
    }
    if (e.key === "/" && document.activeElement !== search && (!excludeModal || excludeModal.hidden) && (!gayModal || gayModal.hidden)) {
      e.preventDefault();
      search && search.focus();
    }
  });
})();
