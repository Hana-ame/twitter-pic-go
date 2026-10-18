# AGENTS.md — 仓库结构与代码修改导航指南

本文档面向 AI Agent 与人类开发者，提供 **「修改什么功能应该去哪个目录、哪个文件找」** 的精确索引，以及修改时必须遵守的跨端契约与测试栅栏。

---

## 1. 架构总览（一句话先导）

本项目是一个 **单地址（Single-Origin）、单进程、单 SQLite 数据库连接池** 的 Go + Web 服务：
- **统一入口**：`server/main.go`（或 `cmd/server/main.go`）在同一个端口（默认 `:8080`）上调用 `api.SetupRouter(db)`，同时承载后端 REST API 与 Gallery SSR 图站。
- **两大业务核心包**：
  1. **`api/` (`package api`)**：提供 V1（`/api/twitter/*`）与 V2（`/api/v2/*`、`/api/flutter/*`、`/api/twitter/v2/*`）REST API，服务 Flutter 客户端、React 客户端及 Gallery 前端异步请求。
  2. **`gallery/` (`package gallery`)**：提供 SSR 网页图站（`/`、`/u/{account}`）、Web 轻接口（`/api/tag`、`/api/tags`、`/api/reactions`）与前端静态资源（`/static/*`）。支持 **磁盘实时热更新（免编译 Go）** 与 **`//go:embed` 单二进制内嵌兜底** 双模式。
- **四大底层共享包**：`tags/`（标签与一 IP 一票账本唯一真源）、`ipban/`（IP 封禁 Trie 与真实客户端 IP 判定唯一真源）、`limit/`（按 IP 限流器）、`twimg/`（内置媒体反代）。

---

## 2. 按任务速查：修改什么内容去哪里找？

### 2.1 Gallery 前端界面、样式与交互（最高频修改区）

> **热更新提示**：Gallery 已内置磁盘修改时间（`mtime`）自动监控（`gallery/assets.go`）。在仓库根目录运行服务（或设置 `GALLERY_FRONTEND_DIR=./gallery`）时，直接修改下方 `.html` / `.css` / `.js` 文件并保存，**无需重新 `go build` 或重启进程**，刷新浏览器即可看到最新效果；线上执行 `./build.sh --frontend`（或 `make deploy-fe`）即可零停机秒级热更前端。

| 你想要修改的内容 | 目标文件路径 | 关键符号 / 选择器 / 说明 |
| :--- | :--- | :--- |
| **全站配色、字体、圆角、暗色背景、毛玻璃导航栏、底部、通用弹窗框体** | `gallery/static/css/base.css` | `:root` CSS 变量（`--bg`, `--acc`, `--card` 等）、`header`、`.lgbtn`、`.modal-*` |
| **首页 Hero 大标题、公告横幅、搜索框、工具按钮（Gay模式/屏蔽/随机）、标签云胶囊、账号网格卡片样式** | `gallery/static/css/home.css` | `.hero`, `.notice`, `.tools`, `.search`, `.tbtn`, `.tagbar`, `.tag`, `.agrid`, `.acct` |
| **账号详情页头图 Banner、头像昵称栏、账号标签投票按钮、图片/视频切换栏、瀑布流网格卡片样式** | `gallery/static/css/account.css` | `.ubanner`, `.uhead`, `.atags`, `.atag`, `.atag-vote`, `.modes`, `.grid`, `.card` |
| **全屏灯箱查看器（滑动/缩放舞台）、右侧点赞栏、自定义视频播放器 HUD（进度条/音量/倍速）、手机端响应式适配** | `gallery/static/css/lightbox.css` | `.lb`, `.lb-track`, `.lb-stage`, `.lb-rail`, `.lb-vhud`, `.lb-vprog`, `@media (max-width:640px)` |
| **公共 HTML 头部 `<head>`、顶部导航栏链接（如加群链接、旧版入口）、页脚文案** | `gallery/templates/partials.html` | `{{define "head"}}`, `{{define "header"}}`, `{{define "footer"}}` |
| **首页 HTML 结构（公告、搜索工具栏、排除标签弹窗、Gay 模式配置弹窗）** | `gallery/templates/home.html` | `#a-search`, `#a-gay-btn`, `#a-exclude-btn`, `#a-tags`, `#a-grid`, `#a-exclude-modal`, `#a-gay-modal` |
| **账号页 HTML 结构（资料头、模式切换、瀑布流容器、全屏灯箱 DOM、视频控制器 DOM）** | `gallery/templates/account.html` | `#g-atags`, `#g-grid`, `#lb`, `#lb-vhud` |
| **首页前端逻辑（搜索过滤、增量加载、流式解压 `.json.gz` 提取头像/首图、Gay 模式词表、屏蔽标签弹窗）** | `gallery/static/home.js` | `fetchMeta`, `extractMeta`, `paint`, `applyFilter`, `toggleGayMode`, `toggleExcludeTag` |
| **账号页前端逻辑（瀑布流分页、标签加减票交互、全屏灯箱手势缩放/下滑关闭、视频 HUD 进度/倍速/全屏、快捷键）** | `gallery/static/app.js` | `renderGrid`, `renderATags`, `postATag`, `openLightbox`, `attachGestures`, `bindVideoHud` |
| **前端模板与静态资源的加载方式（`embed.FS` vs 磁盘热重载、CSS 模块拼接顺序）** | `gallery/assets.go` | `cssModuleOrder`, `assetManager`, `loadDiskBundle`, `loadEmbeddedBundle` |

---

### 2.2 Gallery 后端 SSR 渲染与图站专属接口

| 你想要修改的内容 | 目标文件路径 | 关键函数 / 结构体 |
| :--- | :--- | :--- |
| **Gallery 路由表（`/`, `/u/{account}`, `/api/tag`, `/api/reactions`, `/static/*` 等）与配置初始化** | `gallery/server.go` | `NewHandler`, `galleryMux`, `config` |
| **首页与账号页的 SSR 数据组装、首屏预览条数（默认 120）、账号排序规则** | `gallery/pages.go` | `handleHome`, `buildHomeHTML`, `handleAccount`, `pageData`, `homeData`, `accountPage` |
| **`.json.gz` 解析、唯一资源 CDN (`pbs.moonchan.xyz`) 与 `cover` 封面重写、点开视频 `ouo.io` 跳转链接构造** | `gallery/media.go` | `buildAll`, `buildOuoURL`, `resolveOuoPrefix`, `mediaURL`, `defaultMediaBase`, `defaultOuoPrefix` |
| **图站标签接口（`GET /api/tags`, `POST /api/tag`, `GET /api/tag/{tag}`, `GET /api/tags/cloud`）** | `gallery/tags_handler.go` | `handlePostAccountTag`, `handleGetAccountTags`, `handleTagUsers`, `handleTagCloud` |
| **单张媒体图片/视频的赞踩计数（`GET /api/reactions`, `POST /api/react`, `reactions.json`）** | `gallery/reactions.go` | `reactionStore`, `handleGetReactions`, `handlePostReact` |
| **首页整页 SSR 内存缓存（Stale-While-Revalidate 单飞构建与失效）** | `gallery/home_cache.go` | `homeCache`, `newHomeCache`, `html()`, `invalidate()` |
| **被封禁账号（`users.status != 'SUCCESS'`）在图站全路径 404 隐身过滤** | `gallery/visibility.go` | `vis`, `hidden()`, `filter()`, `cloud()`, `invalidate()` |

---

### 2.3 后端 REST API（V1 & V2 / Flutter 接口）

| 你想要修改的内容 | 目标文件路径 | 关键函数 / 结构体 |
| :--- | :--- | :--- |
| **单端口统一路由挂载（将 `/api/twitter`、`/api/v2`、`/api/flutter` 与 `gallery` 挂在同一引擎）** | `api/router.go` | `SetupRouter(db)`, `AddToGroup(g)` |
| **【旧版 API (Legacy V1)】专供 `twitter-pic-react` `main` 分支：提交抓取、获取 `.json.gz`、V1 标签与 Emoji（严禁破坏向后兼容）** | `api/handlers_v1.go` | `CreateMetaData`, `GetMetaData`, `GetTags`, `GetLists`, `GetEmojis`, `VoteUpEmojiHandler` |
| **【新版 API (New V2)】供 `twitter-pic-react` 新分支 / Flutter：富卡片列表、带 `cover`+`ouo_url` 的 `/timeline` 与 `/media`、内嵌富卡片的 `/ranking`、CF 强缓存** | `api/handlers_v2.go` | `RegisterV2Routes`, `HandleGetUserTimelineV2`, `HandleGetUserMediaV2`, `HandleGetRankingV2`, `v2CoverURL`, `v2BuildOuoURL` |
| **V2 接口 JSON 请求与响应结构体定义** | `api/models_v2.go` | `FlutterUser`, `UserMediaResponse`, `TagUsersResponse`, `FeedResponse`, `UserProfileResponse` |
| **`users` 表建表、用户状态变更（`SUCCESS` / `BANNED`）、V1 用户列表与搜索 SQL** | `api/db_users.go` | `CreateTable`, `commitUser`, `getUserList`, `getUserListAfter`, `getUserTags` |
| **V1 标签表初始化桥接、历史 `user_tags` 迁移、V1 按标签查用户** | `api/db_tags.go` | `CreateTableV2`, `migrateAccountTags`, `addTag`, `getUserListByTag` |
| **Emoji 表情互动投票与日/周/月排行榜结算（`emoji_ranks.json` / `.emoji_ranks.json.gz`）** | `api/db_emoji.go` | `CreateTableV3`, `VoteUpEmoji`, `RefreshAllRankings`, `GetEmojiRank` |
| **Go 后端向 Python 抓取服务发送 TCP 抓取指令（含 `CALLER_TOKEN` 鉴权）** | `api/crawler_client.go` | `curlMetaData` |
| **根包向后兼容导出层（保持 `import "github.com/Hana-ame/twitter-pic-go"` 可用）** | `compat.go` | 类型别名与转发函数 |

---

### 2.4 共享核心组件（标签真源、IP 封禁、限流、反代、爬虫）

| 你想要修改的内容 | 目标文件路径 | 关键函数 / 结构体 |
| :--- | :--- | :--- |
| **标签读写 SQL、按标签反查用户分页（`UsersForTagPaged`）、标签云实时聚合（`Cloud`）** | `tags/tags.go` | `EnsureSchema`, `Store.Weights`, `Store.ForUsers`, `Store.UsersForTagPaged`, `Store.Cloud` |
| **「一 IP 一票」计票事务（`CastVotes`）、目标值语义（`+1`/`-1`/`0` 撤票）、历史底数快照、账本恒等式校验** | `tags/votes.go` | `Store.CastVotes`, `BackfillVoteBase`, `Store.InvariantViolations` |
| **标签云进程内缓存与跨层数据变更通知钩子（API 与 Gallery 同进程缓存联动）** | `tags/cloud_cache.go` | `OnDataChange`, `InvalidateCloud`, `Store.RefreshCloud` |
| **IP 封禁（`bans.txt` Radix Trie 热重载）、封禁链判定（`Chain`）与真实访客 IP 提取（`Principal`）** | `ipban/ipban.go` | `Shared()`, `Manager.Decide`, `Principal`, `Chain`, `TrustedHops` |
| **按 IP 每小时请求限流（支持 IPv4/IPv6）与全局抓取限流** | `limit/limit.go`, `limit/middleware.go`, `limit/globalRateLimiter.go` | `FastLimiter`, `RateLimitMiddleware`, `GlobalRateLimitMiddleware` |
| **配套 `pbs.moonchan.xyz` 反向代理（`TwimgProxyV2`：图片反代 `pbs.twimg.com` + `.mp4` 302 分流卸载）** | `twimg/twimg.go`, `twimg/video_stream.go`, `twimg/video_gate.go` | `Run(addr)`, `TwimgProxyV2(addr)`, `StreamProxy` |
| **Python 爬虫监听守护进程与 Twitter 媒体抓取脚本** | `crawler/caller.py`, `crawler/deamon.py`, `crawler/get2.py`, `crawler/get_meta_data.py` | `start_tcp_listener`, `call`, `extract_username` |
| **部署脚本、构建脚本与运维文档** | `Makefile`, `build.sh`, `docs/API_V2.md`, `docs/FRONTEND_GUIDE.md`, `docs/BAN_MANUAL.md` | `./build.sh`, `./build.sh --frontend` |

---

## 3. 修改代码时必须遵守的核心契约与护栏（Guardrails）

1. **前端 `home.js` 的两条硬性测试钩子（切勿误删）**：
   - `tags/gay_tags_contract_test.go` 会直接读取 `gallery/static/home.js`，断言其中存在精确字面量：
     ```js
     const DEFAULT_GAY_TAGS = ["男性", "男娘", "人妖", "露屌", "阳痿", "男同"];
     ```
     若需修改默认 Gay 模式词表，必须同步更新 `tags/gay_tags_contract_test.go` 及 Flutter 客户端。
   - `gallery/static/home.js` 中的 `/*<scan>*/ ... /*</scan>*/` 注释块是流式 JSON 解析器的提取边界，修改 `home.js` 时请保留这对标记注释。
2. **「一 IP 一票」目标值契约**：
   - 标签投票接口（`POST /api/tag`、`POST /api/twitter/tag`、`POST /api/v2/users/:username/tags`）中的 `d` 均表示 **该访客 IP 的目标票值（`1` 赞同 / `-1` 反对 / `0` 撤票）**，绝非增量累加。
   - 任何写标签路径都必须走 `tags.Store.CastVotes`，禁止直接 `UPDATE account_tags SET weight = weight + 1`（会破坏 `account_tags.weight = tag_weight_base.base + SUM(tag_votes.value)` 恒等式，导致 `TestAccountTagsRoundTrip` 与 `InvariantViolations()` 报错）。
3. **两条 IP 口径严禁混用**：
   - **封禁检查**必须用 `ipban.Manager.Decide(r)`（检查 `ipban.Chain(r)` 全链路，任一命中即 403）。
   - **限流分桶、投票账本 IP、`request_logs.ip`** 必须统一用 `ipban.Principal(r)`。
4. **被封禁账号（`users.status != 'SUCCESS'`）全站隐身**：
   - 只有 `status = 'SUCCESS'`（或仅在磁盘存在 `.json.gz` 但尚未写入 `users` 表的未注册账号）在 Gallery 可见；凡是在 `users` 表中显式非 `SUCCESS`（如 `BANNED`）的账号，在 Gallery 所有页面与接口一律返回 `404` 或从列表中剔除。
5. **每次修改后的自检命令**：
   ```bash
   make check-js   # 检查 gallery/static/*.js 语法
   make test       # 运行全量 Go 单元测试与跨层契约测试 (go test ./...)
   ```
