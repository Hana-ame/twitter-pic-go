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
  try { names = JSON.parse(dataEl.textContent) || []; } catch (e) { return; }

  const search = document.getElementById("a-search");
  const shownEl = document.getElementById("a-shown");
  const moreBtn = document.getElementById("a-more");
  const randBtn = document.getElementById("a-rand");
  const emptyEl = document.getElementById("a-empty");
  const STEP = 120;
  const CONC = 6;        // 并发预览拉取数
  const MAX_SEEK = 8e6;  // 单文件解压后最多扫 8MB

  const BASE = (grid.dataset.base || "").replace(/\/+$/, "");
  const cache = new Map();
  try {
    const pre = JSON.parse(document.getElementById("a-meta").textContent || "{}");
    for (const k in pre) cache.set(k, pre[k]);
  } catch (e) { /* 无预烘焙数据 */ }

  // 与 Go hueOf 同算法（账号名为 ASCII）
  const hueOf = (s) => { let h = 0; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 360; return h; };
  const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
  const mediaURL = (raw) => {
    if (!raw) return "";
    if (!BASE) return raw;
    try { const u = new URL(raw); if (u.host === "pbs.twimg.com") return BASE + u.pathname + (u.search || ""); } catch (e) {}
    return raw;
  };

  const cardHTML = (n) => {
    const ini = esc((n[0] || "?").toUpperCase());
    return '<a class="acct" href="/u/' + encodeURIComponent(n) + '" data-n="' + esc(n) + '">' +
      '<span class="bnr-ph" style="--h:' + hueOf(n) + '">' + ini + "</span>" +
      '<span class="arow"><span class="av" style="--h:' + hueOf(n) + '">' + ini + "</span>" +
      '<span class="col"><span class="at">@' + esc(n) + "</span><span class=\"nk\"></span></span></span></a>";
  };

  // ---- 预览绘制 ----
  function paint(el, m) {
    el.dataset.painted = "1";
    const name = el.dataset.n;
    const b = mediaURL(m.b || "");
    if (b) {
      const ph = el.querySelector(".bnr-ph");
      if (ph) {
        const media = m.v
          ? '<video class="bnr" muted loop playsinline preload="metadata" src="' + esc(b) + '"></video>'
          : '<img class="bnr" loading="lazy" decoding="async" alt="" src="' + esc(b) + '">';
        ph.outerHTML = media;
      }
    }
    const a = mediaURL(m.a || "");
    if (a) {
      const av = el.querySelector(".av");
      if (av && !av.querySelector("img")) av.insertAdjacentHTML("afterbegin", '<img alt="" src="' + esc(a) + '">');
    }
    const nick = el.querySelector(".nk");
    if (nick && m.i && m.i !== name) nick.textContent = m.i;
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
        if (document.body.contains(el)) paint(el, m);
      }).catch(() => {
        cache.set(name, { n: name });
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
        for (; scanned < 60; scanned++) {
          while (pos < text.length && /[\s,]/.test(text[pos])) pos++;
          if (pos >= text.length) { tl = null; break; }       // 数据不够
          if (text[pos] === "]") { tl = { url: "", type: "" }; break; } // 无有效媒体
          if (text[pos] !== "{") { tl = null; break; }
          const o = jsonSlice(text, pos);
          if (!o) {
            if (final) { const mm = /"url"\s*:\s*"([^"]*)"/.exec(text.slice(pos, pos + 4000)); tl = mm ? { url: mm[1], type: (/"type"\s*:\s*"([^"]*)"/.exec(text.slice(pos, pos + 4000)) || [])[1] || "" } : { url: "", type: "" }; }
            else tl = null;
            break;
          }
          const mu = /"url"\s*:\s*"([^"]*)"/.exec(o);
          if (mu && mu[1]) { tl = { url: mu[1], type: (/"type"\s*:\s*"([^"]*)"/.exec(o) || [])[1] || "" }; break; }
          pos += o.length;
        }
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
    const url = "/raw/" + encodeURIComponent(name);
    const res = await fetch(url);
    if (!res.ok) throw new Error("HTTP " + res.status);
    if (!res.body || !window.DecompressionStream) {
      // 兜底：整包解压失败就全量 parse（旧浏览器）
      const j = await res.json().catch(() => null);
      if (!j) throw new Error("no stream");
      return extractMeta(JSON.stringify(j), true) || { n: name };
    }
    const rd = res.body.pipeThrough(new DecompressionStream("gzip")).getReader();
    const td = new TextDecoder();
    let text = "", done = false;
    while (!done) {
      const r = await rd.read();
      if (r.done) break;
      text += td.decode(r.value, { stream: true });
      const m = extractMeta(text, false);
      if (m) { rd.cancel().catch(() => {}); return m; }
      if (text.length > MAX_SEEK) break;
    }
    return extractMeta(text, true) || { n: name };
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

  reset(names); // 接管 SSR 预览重绘

  let timer = 0;
  search && search.addEventListener("input", () => {
    clearTimeout(timer);
    timer = setTimeout(() => {
      const q = search.value.trim().toLowerCase();
      if (!q) return reset(names);
      const pre = [], sub = [];
      for (const n of names) {
        const l = n.toLowerCase();
        if (l.startsWith(q)) pre.push(n);
        else if (l.includes(q)) sub.push(n);
        if (pre.length >= 2000) break;
      }
      reset(pre.concat(sub));
    }, 120);
  });

  moreBtn && moreBtn.addEventListener("click", renderChunk);

  randBtn && randBtn.addEventListener("click", () => {
    if (!names.length) return;
    location.href = "/u/" + encodeURIComponent(names[Math.floor(Math.random() * names.length)]);
  });

  // 斜杠键聚焦搜索框
  document.addEventListener("keydown", (e) => {
    if (e.key === "/" && document.activeElement !== search) {
      e.preventDefault();
      search && search.focus();
    }
  });
})();
