// 主页脚本：账号搜索 / 增量加载 / 随机逛逛。
// 全量账号名以 JSON 嵌在 #a-data 中，SSR 已渲染前 120 个作为预览。
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

  // 与服务端 hueOf 相同的算法（账号名均为 ASCII）
  const hueOf = (s) => { let h = 0; for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) % 360; return h; };

  const cardHTML = (n) =>
    '<a class="acct" href="/u/' + encodeURIComponent(n) + '">' +
    '<span class="av" style="--h:' + hueOf(n) + '">' + (n[0] || "?").toUpperCase() + "</span>" +
    '<span class="at">@' + n + "</span></a>";

  let view = names;   // 当前过滤结果
  let rendered = 0;   // view 中已渲染数量

  function renderChunk() {
    const slice = view.slice(rendered, rendered + STEP);
    if (slice.length) {
      grid.insertAdjacentHTML("beforeend", slice.map(cardHTML).join(""));
      rendered += slice.length;
    }
    update();
  }

  function reset(list) {
    view = list;
    rendered = 0;
    grid.innerHTML = "";
    renderChunk();
  }

  function update() {
    if (shownEl) shownEl.textContent = "显示 " + rendered + " / " + view.length + " 个账号";
    if (moreBtn) moreBtn.hidden = rendered >= view.length;
    if (emptyEl) emptyEl.hidden = view.length > 0;
  }

  // SSR 预览由 JS 接管重绘，保证计数/按钮状态统一
  reset(names);

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
