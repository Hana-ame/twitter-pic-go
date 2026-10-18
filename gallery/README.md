# `gallery` Package — SSR 图站与模块化前端 (`package gallery`)

`gallery` 是 `twitter-pic-go` 的 Web 图库包（`package gallery`），由统一入口 `server/main.go`（通过 `api.SetupRouter(db)`）**与后端 REST API 挂载在同一个 HTTP 地址（默认 `:8080`）和同一个 `twitter.db` 连接池上运行**；同时也支持通过 `cmd/gallery` 独立启动调试。

---

## 1. 核心架构亮点

### 1.1 与 API 同源单端口运行（Single-Origin）
- **同一端口监听**：不再需要分别维护 `:8080`（API）和 `:8090`（Gallery）两个端口。`/api/twitter/*`、`/api/v2/*`、`/api/flutter/*` 由 Gin API 路由处理，其余路径（`/`、`/u/{account}`、`/static/*`、`/api/tag`、`/api/tags`、`/api/reactions`、`/healthz`）由 `gallery.NewHandler(db)` 原生处理。
- **同一数据库连接池与实时缓存同步**：API 与 Gallery 共享同一个 `*sql.DB` 连接池、同一个 `ipban.Shared()` 封禁实例、同一个 `tags.Store`。通过 `tags.OnDataChange` 跨层钩子，任一端（Web 图站、React、Flutter 或管理员封禁接口）修改标签或用户状态，Gallery 的首页 SSR 缓存（`homeCache`）与可见性视图（`vis`）以及 API V2 的 `feedCache` 均**立即同步失效刷新**。
- **前端零跨域直接拉取 `.json.gz`**：首页卡片预览（`static/home.js`）直接请求同源的 `/api/twitter/{account}.json.gz`，通过浏览器 `DecompressionStream` 流式解压前缀提取头像、昵称与首图即可中断读取，无需穿透完整文件。

### 1.2 面向高频前端迭代的「双模资源加载器」（`assets.go`）
为了支持后续频繁更新前端 UI、样式与交互，`gallery` 将原本混在单文件里的代码拆分为清晰的模块化结构，并提供**免编译实时热更新**：
- **模块化 CSS（`static/css/`）**：
  - `base.css`：全局 CSS 变量（色板/圆角/阴影）、Reset、毛玻璃顶栏、页脚、通用弹窗框体
  - `home.css`：首页 Hero、公告横幅、搜索与筛选工具栏、标签云、账号卡片网格
  - `account.css`：账号页 Banner/头像、标签投票胶囊、CSS 多列自适应瀑布流、模式切换与分页
  - `lightbox.css`：手机式全屏手势灯箱（横向翻页/双指缩放/下滑关闭）、右侧点赞栏、自定义视频播放器 HUD、移动端响应式适配
- **自动磁盘热重载（Live Disk Reload）**：
  - 当工作目录下存在 `gallery/templates` 与 `gallery/static`（或通过 `GALLERY_FRONTEND_DIR` 指定）时，`assets.go` 会在每次请求时对比磁盘上 `.html`、`.css`、`.js` 的修改时间戳（`mtime`）。
  - **一旦你在本地或服务器上修改了任何 HTML / CSS / JS 文件，无需重新 `go build` 或重启服务**，服务端会自动重新加载模板与样式并使 `homeCache` 失效，浏览器刷新立即可见！
- **`//go:embed` 单二进制兜底**：
  - 所有模板与静态资源同时通过 `//go:embed` 编译进二进制。在无源码目录的纯二进制部署环境或 `go test` 测试环境下，自动无缝使用内嵌资源。

---

## 2. 包内文件结构

| 文件 / 目录 | 职责说明 |
| :--- | :--- |
| `server.go` | `NewHandler(dbs...)`、`Run(addr)`、`galleryMux` 路由表注册、跨层缓存失效钩子绑定 |
| `assets.go` | 前端模板与静态资源的双模加载器（`//go:embed` + 磁盘 `mtime` 自动热重载）、CSS 模块合并 |
| `pages.go` | 首页（`/`）与账号详情页（`/u/{account}`）SSR 控制器与视图模型 |
| `media.go` | `<account>.json.gz` 解析、图片/视频代理域名重写（`pbs.moonchan.xyz`）、游标分页计算 |
| `tags_handler.go` | 图站标签接口：`GET /api/tags`、`POST /api/tag`、`GET /api/tag/{tag}`、`GET /api/tags/cloud` |
| `reactions.go` | 媒体单图赞/踩计数持久化（`reactions.json`）与 `GET /api/reactions`、`POST /api/react` |
| `home_cache.go` | 首页整页 SSR 缓存（Stale-While-Revalidate + 冷启动 Single-Flight 防击穿） |
| `visibility.go` | 被封禁账号（`users.status != 'SUCCESS'`）全路径 404 隐身视图 |
| `templates/` | HTML 模板（`partials.html`, `home.html`, `account.html`） |
| `static/css/` | 模块化样式表（`base.css`, `home.css`, `account.css`, `lightbox.css`） |
| `static/home.js` | 首页前端脚本（搜索、标签筛选、Gay 模式、流式解压 `.json.gz` 卡片预览） |
| `static/app.js` | 账号页前端脚本（瀑布流分页、标签加减票、手势灯箱、自定义视频 HUD） |

---

## 3. 路由一览

| 方法与路径 | 说明 |
| :--- | :--- |
| `GET /` · `GET /index.html` | 图站 SSR 首页（带 `homeCache` Stale-While-Revalidate 缓存） |
| `GET /u/{account}?type=&cursor=` | 账号媒体瀑布流 SSR 详情页（`type=photo\|video\|all`） |
| `GET /static/*` | 静态资源访问（如 `/static/app.js`, `/static/home.js`, `/static/css/base.css` 等） |
| `GET /api/tags?keys=u1,u2` | 批量读取账号标签权重（别名 `GET /api/account-tags`） |
| `POST /api/tag` | 账号标签「一 IP 一票」目标值投票，请求体 `{"user":"...","tag":"...","d":1\|-1\|0}`（别名 `POST /api/account-tag`） |
| `GET /api/tag/{tag}?limit=&page=&offset=` | 按标签分页反查可见账号列表（按权重降序） |
| `GET /api/tags/cloud?limit=` | 获取全局热门标签云计数（别名 `GET /api/tag-cloud`） |
| `GET /api/reactions?keys=` · `POST /api/react` | 单张媒体赞/踩计数读取与提交 |
| `GET /healthz` | 健康检查（返回 `200 ok`） |
