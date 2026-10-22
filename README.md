# twitter-pic-go

Twitter 媒体元数据抓取、V1/V2 轻量动态切片 API 与 SSR 沉浸式图库（单进程、单地址统一服务架构）。

---

## 1. 快速导航

- **[`AGENTS.md`](AGENTS.md)**：**代码修改速查地图**（改前端样式、改灯箱交互、改 API 路由、改标签计票、改爬虫分别去哪个文件找，以及跨端契约护栏）。
- **[`docs/FRONTEND_GUIDE.md`](docs/FRONTEND_GUIDE.md)**：**Gallery 前端频繁迭代指南**（模块化 CSS/JS 拆分说明、免编译磁盘实时热更新、线上秒级仅热更前端工作流）。
- **[`docs/API_V2.md`](docs/API_V2.md)**：**V2 动态切片 API 与 Cloudflare 边缘缓存规范**（面向 Flutter 客户端与现代 Web）。
- **[`gallery/README.md`](gallery/README.md)**：**Gallery SSR 图站包架构与路由说明**。
- **[`api/README.md`](api/README.md)**：**API 后端包结构说明**。
- **[`docs/BAN_MANUAL.md`](docs/BAN_MANUAL.md)** & **[`docs/BAN_SUMMARY.md`](docs/BAN_SUMMARY.md)**：**IP 与违规账号封禁运维手册**。

---

## 2. 仓库架构（Formal Repository Layout）

本项目将后端 **REST API (`api/`)** 与 **Web 图站 (`gallery/`)** 作为两个对等的一级业务包，通过统一入口 `server/main.go`（或 `cmd/server/main.go`）在**同一个监听地址（默认 `:8080`）和同一个 SQLite 数据库连接池（`twitter.db`）**中协同运行：

```text
twitter-pic-go/
├── AGENTS.md                  # 修改指南：改什么内容去哪里找 & 核心契约护栏
├── README.md                  # 项目总览、架构、运行与部署说明（本文档）
├── Makefile                   # 常用开发、测试、构建与前端热部署命令
├── build.sh                   # 生产构建与 SCP 部署脚本（支持 --frontend 仅热更前端）
├── compat.go                  # 根包向 api 子包的向后兼容导出层
├── bans.txt                   # IP 封禁清单（单 IP / CIDR，10 分钟自动热重载）
│
├── server/                    # 统一服务主入口（兼容 CI 与现有部署脚本 `go build ./server`）
│   ├── main.go                # 初始化共享 SQLite 连接池，在单一端口同时挂载 API 与 Gallery
│   └── main_test.go           # 单端口路由共存与跨层数据同步集成测试
│
├── cmd/                       # 标准 CLI 入口
│   ├── server/main.go         # 标准统一服务入口（同 server/main.go）
│   └── gallery/main.go        # 仅启动 Gallery 的独立调试入口
│
├── api/                       # 【核心业务包 1】后端 REST API (package api)
│   ├── router.go              # SetupRouter（统一挂载 API + Gallery）与 AddToGroup
│   ├── handlers_v1.go         # V1 接口（/api/twitter/*：元数据、标签、Emoji、封禁管理）
│   ├── handlers_v2.go         # V2 轻量切片接口（/api/v2/*, /api/flutter/*, /api/twitter/v2/*）
│   ├── models_v2.go           # V2 数据模型（FlutterUser, UserMediaResponse, FeedResponse 等）
│   ├── db_users.go            # users 表管理、状态更新与用户查询
│   ├── db_tags.go             # 标签表迁移桥接与按标签查用户
│   ├── db_emoji.go            # Emoji 投票与日/周/月排行榜压缩文件结算
│   ├── crawler_client.go      # 向 Python caller.py 发送抓取指令的 TCP 客户端
│   ├── middleware_ban.go      # Gin 侧 IP 封禁中间件
│   └── *_test.go              # API 单元与契约回归测试
│
├── gallery/                   # 【核心业务包 2】Web 图站 SSR 与模块化前端 (package gallery)
│   ├── server.go              # 图站 Handler、路由表与跨层缓存失效订阅
│   ├── assets.go              # 双模前端加载器（//go:embed 内嵌兜底 + 磁盘 mtime 免编译实时热更新）
│   ├── pages.go               # 首页 (/) 与账号页 (/u/{account}) SSR 渲染控制器
│   ├── media.go               # .json.gz 解析、媒体代理域名重写、游标分页
│   ├── tags_handler.go        # 图站标签接口（/api/tag, /api/tags, /api/tag/{tag}, /api/tags/cloud）
│   ├── reactions.go           # 媒体单图赞/踩持久化（/api/reactions, /api/react）
│   ├── home_cache.go          # 首页整页 SSR Stale-While-Revalidate 缓存
│   ├── visibility.go          # 被封账号全路径 404 隐身视图
│   ├── templates/             # HTML 模板（partials.html, home.html, account.html）
│   ├── static/                # 前端静态资源
│   │   ├── css/               # 模块化样式表（base.css, home.css, account.css, lightbox.css）
│   │   ├── home.js            # 首页交互脚本（搜索、筛选、Gay模式、流式解压预览）
│   │   └── app.js             # 账号页交互脚本（瀑布流、标签投票、手势灯箱、视频 HUD）
│   └── *_test.go              # 图站路由、可见性、投票契约与缓存测试
│
├── tags/                      # 【共享底层包】账号标签与「一 IP 一票」账本唯一真源
├── ipban/                     # 【共享底层包】IP 封禁 Radix Trie 与真实访客 IP (Principal) 提取
├── limit/                     # 【共享底层包】按 IP 每小时限流器与全局限流中间件
├── twimg/                     # 【共享底层包】内置 pbs.twimg.com 媒体反向代理
├── Tools/                     # 【内置工具包】sqlite 连接封装与 ginkit 错误/CORS 中间件（无外部 submodule 依赖）
├── crawler/                   # 【Python 抓取套件】caller.py, deamon.py, get2.py, get_meta_data.py
└── docs/                      # 【文档中心】API_V2.md, FRONTEND_GUIDE.md, BAN_MANUAL.md, BAN_SUMMARY.md
```

---

## 3. 单地址运行与路由全景（API + Gallery 同源共存）

启动服务后，**`api` 与 `gallery` 运行在同一个监听地址（默认 `:8080`）**，共享同一个 `twitter.db` 连接池与进程内缓存总线（`tags.OnDataChange`）：

```text
HTTP Server (LISTEN_ADDR, 默认 :8080)
 │
 ├── /api/twitter/*          ──► api 包 (V1 REST API: 元数据 .json.gz、标签、Emoji、管理员封禁)
 ├── /api/twitter/v2/*       ──► api 包 (V2 动态切片 API: Feed、媒体分页、标签用户、304 缓存)
 ├── /api/v2/*               ──► api 包 (V2 根路径别名)
 ├── /api/flutter/*          ──► api 包 (Flutter 专属路径别名)
 │
 └── 其余全部路由             ──► gallery 包 (SSR 图站与 Web 交互)
      ├── GET  / , /index.html          (SSR 首页，带 60s Stale-While-Revalidate 整页缓存)
      ├── GET  /u/{account}             (SSR 账号瀑布流详情页，支持 ?type=photo|video|all&cursor=)
      ├── GET  /static/*                (前端静态资源：/static/css/*.css, /static/home.js, /static/app.js)
      ├── GET  /api/tags , /api/account-tags          (批量查询账号标签权重)
      ├── POST /api/tag  , /api/account-tag           (图站单标签一 IP 一票投票)
      ├── GET  /api/tag/{tag}                         (按标签分页反查账号)
      ├── GET  /api/tags/cloud , /api/tag-cloud       (热门标签云统计)
      ├── GET  /api/reactions  , POST /api/react      (单张媒体赞/踩互动)
      └── GET  /healthz                               (健康检查)
```

---

## 4. 快速启动、前端热更新与部署工作流

### 4.1 本地开发（开启 Gallery 前端免编译实时热更新）

```bash
# 启动统一服务（默认监听 :8080）
make dev
# 或直接运行：
LISTEN_ADDR=:8080 GALLERY_DEV=1 GALLERY_FRONTEND_DIR=./gallery go run ./server
```

- **前端实时修改免重启**：直接修改 `gallery/templates/*.html`、`gallery/static/css/*.css` 或 `gallery/static/*.js`，保存后**直接刷新浏览器即可看到最新效果**，无需重新编译 Go 二进制或重启进程（详见 [`docs/FRONTEND_GUIDE.md`](docs/FRONTEND_GUIDE.md)）。

### 4.2 运行测试与语法自检

```bash
# 仅检查前端 JS 语法
make check-js

# 运行 JS 语法检查 + 全量 Go 单元测试与跨层契约测试
make test
```

### 4.3 编译与生产部署

```bash
# 1. 仅在本地编译统一二进制（生成 ./bin/twitter-server 与 ./server/server）
make build

# 2. 仅热更新线上 Gallery 前端（免编译 Go、零停机，仅 SCP 同步 templates 与 static）
make deploy-fe
# 等价于：./build.sh --frontend

# 3. 完整编译并部署后端二进制、前端资源与 Python 抓取脚本
make deploy
# 等价于：./build.sh
```

---

## 5. 核心机制说明

### 5.1 新旧 API 双轨兼容架构（支持 `twitter-pic-react` `main` 旧分支与新分支）

本项目在 `/api/twitter` 下完整保留**旧版 V1 API（供 `twitter-pic-react` `main` 分支使用，不作任何破坏性修改）**，同时在 `/api/twitter/v2`（及 `/api/v2`、`/api/flutter`）提供**新版 V2 API（供 `twitter-pic-react` 新分支、`gallery` 与移动端使用）**：

- **旧版 API（`api/handlers_v1.go` — 供 `twitter-pic-react` `main` 使用）**：
  - `GET /api/twitter/?list=users&after=`、`GET /api/twitter/?by=username|nick&search=`
  - `GET /api/twitter/:username.json.gz?t=`、`POST /api/twitter/:username`（`?do_not_tag=true` / `?do_not_renew=true`）
  - `GET /api/twitter/tags/:username`、`POST /api/twitter/tags/:username`、`POST /api/twitter/tag`
  - `GET /api/twitter/emojis`、`GET /api/twitter/emojis.json.gz`、`POST /api/twitter/emojis`
- **新版 API（`api/handlers_v2.go` — 供 `twitter-pic-react` 新分支使用）**：
  - **视频封面 + 直连代理链接（2026-10-23 起广告跳转已关闭）**：所有视频条目返回 `cover`（`https://pbs.moonchan.xyz/ext_tw_video_thumb/...?format=jpg&name=orig`）与 `ouo_url`（历史契约字段名，值为本后端视频端点 `/api/twitter/v2/video/<path>?<query>`，点击后 302 直连 `https://twimg.moonchan.xyz/...`）。
  - **零 N+1 请求**：`GET /api/twitter/v2/users`、`/search`、`/users/batch`、`/ranking` 直接返回含头像、昵称、标签、媒体统计的富卡片（`FlutterUser`）。
  - **单次时间线与分页切片**：`GET /api/twitter/v2/users/:username/timeline?t=<last_modify>` 与 `/media` 支持 `?t=` 版本化 Cloudflare 边缘强缓存（`s-maxage=86400`）与 `304 Not Modified` 本地协商缓存。
  - 详细新旧接口对照表见 **[`api/README.md`](api/README.md)** 与 **[`docs/API_V2.md`](docs/API_V2.md)**。

### 5.1b Cloudflare 边缘缓存与本地缓存设计
详细接口契约见 **[`docs/API_V2.md`](docs/API_V2.md)**：
- **边缘缓存由 Cloudflare 统一负责**：V2 所有只读接口均下发标准 `Cache-Control: public, max-age=..., s-maxage=...` 及 `Last-Modified`，支持 HTTP `304 Not Modified` 条件协商。
- **按需动态切片**：
  - `GET /api/v2/users/:username/media?limit=24&type=photo` 将首屏瀑布流由数兆 `.json.gz` 压缩至 **~3KB**；
  - `GET /api/v2/tags/:tag/users` 强制校验 `users.status = 'SUCCESS'` 并单次返回头像/昵称/标签，彻底消灭 404 幽灵账号与 N+1 请求风暴；
  - `GET /api/v2/feed` 提供冷启动聚合，服务端内置 ≤30 用户的极小有界淘汰缓存（<2MB），保护 1 vCPU / 528MB 小内存源站。

### 5.2 标签存储与「一 IP 一票」计票契约
**唯一真源**：`twitter.db` 中的三张核心表（由 `tags/` 包统一管理）：
```text
tag_votes(username, tag, ip, value, updated_at)   PK(username,tag,ip) WITHOUT ROWID  ← 票账本（真源）
tag_weight_base(username, tag, base)              PK(username,tag)    WITHOUT ROWID  ← 历史底数（快照后不可变）
account_tags(username, tag, weight)               PK(username,tag)    WITHOUT ROWID  ← 物化滚动值
```
- **恒等式**：`account_tags.weight = IFNULL(base, 0) + IFNULL(Σ tag_votes.value, 0)`（随时可通过 `tags.Store.InvariantViolations()` 校验）。
- **请求与目标值语义**：
  | 入口 | 方法/路径 | 请求体 | `d` / value 含义 |
  | :--- | :--- | :--- | :--- |
  | 图站前端 | `POST /api/tag`（或 `/api/twitter/tag`） | `{"user":"<账号>","tag":"<标签>","d":1}` | **该访客 IP 的目标值**：`1` 投 / `-1` 减 / `0` 撤票（`d` 必填） |
  | V2 客户端 | `POST /api/v2/users/:username/tags` | `{"tag":"...", "d":1}` 或 `{"tags":{"女性":1,"自拍":0}}` | 单标签目标值或全量期望态字典（`0` = 撤掉该 IP 的票） |
  | V1 App | `POST /api/twitter/:username?do_not_renew=true` | `{"<标签>":1,"<另一个>":0}` | 按标签逐项给目标值；未出现在 map 中的 key 保持原样不动 |
- **天然幂等**：同一 IP 重复提交相同目标值，权重只算一票；从 `+1` 改投 `-1` 单笔请求净变化为 `-2`；撤票（`0`）直接删除 `tag_votes` 中的票行并重算滚动值，恰好归零时自动清理 `account_tags` 零权行。每次写请求照记一条 `request_logs` 审计流水。

### 5.3 IP 封禁与客户端真实身份口径（`ipban/`）
`api` 与 `gallery` 共享同一个进程级单例 `ipban.Shared()`（读取 `BANS_FILE`，默认 `bans.txt`，每 `BAN_RELOAD_MINUTES=10` 分钟热重载）：
- **封禁判定（`ipban.Chain(r)` / `Manager.Decide(r)`）**：检查 `RemoteAddr` + `X-Forwarded-For` 全部条目 + `CF-Connecting-IP`，**链上任一 IP 命中即返回 403**（统一响应体 `{"error":"Access Denied","reason":"Banned IP detected in chain","ip":"..."}`），防止伪造 XFF 首项绕过封禁。
- **真实访客身份（`ipban.Principal(r)`）**：用于限流分桶、`tag_votes.ip` 票桶与 `request_logs.ip`。默认优先读取 `CF-Connecting-IP`（可通过 `CF_CONNECTING_IP=0` 关闭），退化时从 `X-Forwarded-For` 右往左取第 `TRUSTED_PROXY_HOPS`（默认 `2`，对应 Cloudflare + Nginx 追加两跳）个地址，最后兜底 `RemoteAddr`。

### 5.4 被封账号的全站隐身（`users.status`）
- `twitter.db` 的 `users.status` 是唯一真源，仅 `status = 'SUCCESS'` 视为正常公开账号。
- 当管理员通过 `DELETE /api/twitter/:username?delete=<DELETE_KEY>` 或 SQLite 将账号置为 `BANNED` 时：
  - API V1 `GET /api/twitter/:fn` 返回 `banned.json`；
  - API V2 与 Gallery 的全部读路径（首页列表、`#a-data`、标签云计数、`/u/{account}`、`/api/tag/{tag}`、`/api/tags`）及投票写路径（`POST /api/tag`）均将该账号隐身或返回 **404**。

---

## 6. 环境变量总表

| 变量名 | 默认值 | 作用范围 | 说明 |
| :--- | :--- | :--- | :--- |
| `LISTEN_ADDR` | `:8080` | 全局服务 | 统一 HTTP 服务监听地址（未设置时回退读取 `GALLERY_ADDR`，再默认 `:8080`） |
| `DB_PATH` / `GALLERY_DB` | `./twitter.db` | 全局服务 | SQLite 数据库文件路径（API 与 Gallery 共用） |
| `GALLERY_FRONTEND_DIR` | 自动探测 `./gallery` | Gallery | 前端 `templates/` 与 `static/` 磁盘路径，存在时自动开启文件修改热重载 |
| `GALLERY_DEV` | `1`（自动） | Gallery | 设为 `0` 时强制禁用磁盘热重载，仅使用编译内嵌的 `//go:embed` 资源 |
| `GALLERY_JSON_DIR` | `.` | API & Gallery | 存放 `<username>.json.gz` 时间线数据的目录 |
| `GALLERY_MEDIA_BASE` | `https://pbs.moonchan.xyz` | Gallery | 图片（含头像/封面）代理基址 |
| `GALLERY_VIDEO_BASE` | `https://twimg.moonchan.xyz` | Gallery | 视频直连落地基址（2026-10-23 起广告跳转已关闭：点开视频由服务端 302 直连该 Worker 地址） |
| ~~`GALLERY_OUO_PREFIX` / `OUO_KEY`~~ | （已废弃） | — | 2026-10-23 起广告跳转已关闭，`ouo.io` 相关环境变量（`OUO_KEY` / `OUO_MODE` / `OUO_PREFIX` / `GALLERY_OUO_*` / `TWIMG_OUO_PREFIX`）全部不再被读取，可安全删除 |
| `TWIMG_ADDR` / `TWIMG_V2` | `""`（如 `127.26.8.10:8080`） | 配套反代 (`twimg/`) | 启动配套的 `TwimgProxyV2`（`pbs.moonchan.xyz`：图片走本体反代，`.mp4` 强制 302 卸载） |
| `GALLERY_LEGACY_BASE` | `https://x.4545810.xyz` | Gallery | 旧版界面跳转基址（设为空串隐藏按钮） |
| `GALLERY_PAGE_SIZE` | `12` | Gallery | 账号页每页媒体卡片数量 |
| `GALLERY_HOME_CACHE_TTL` | `60` | Gallery | 首页 SSR 整页缓存 TTL（秒） |
| `GALLERY_REACTIONS_FILE` | `./reactions.json` | Gallery | 单张媒体赞/踩持久化 JSON 文件路径 |
| `GALLERY_TAG_RATE_MAX` | `25` | Gallery | 每 IP 每小时标签投票次数上限 |
| `BANS_FILE` | `./bans.txt` | 全局封禁 | IP / CIDR 封禁列表文件路径 |
| `BAN_RELOAD_MINUTES` | `10` | 全局封禁 | `bans.txt` 自动热重载间隔（分钟） |
| `CF_CONNECTING_IP` | `1` | 全局 IP 口径 | 是否优先信任 `CF-Connecting-IP` 请求头（设 `0` 关闭） |
| `TRUSTED_PROXY_HOPS` | `2` | 全局 IP 口径 | XFF 从右往左取真实客户端的跳数（CF + Nginx 追加 = `2`） |
| `DELETE_KEY` | （必填才生效） | API 管理 | `DELETE`/`PUT /api/twitter/:username?delete=` 封禁/解封密钥 |
| `CALLER_ADDR` | `127.25.9.19:8080` | API 抓取 | Python `crawler/caller.py` 的 TCP 监听地址 |
| `CALLER_TOKEN` | `""` | API & 爬虫 | Go 与 `caller.py` 之间的可选共享鉴权令牌 |
| `TWIMG_ADDR` | `""`（空则不启动） | 内置反代 | 内置 `pbs.twimg.com` 反向代理监听地址 |
