# `api` Package — 后端 REST API 与统一路由 (`package api`)

`api/` 包承载 `twitter-pic-go` 的全部 JSON REST API，清晰划分为 **旧版 API（Legacy V1）** 与 **新版 API（New V2）** 两套并存的协议层，并通过 `SetupRouter(db)` 将 `api` 与 `gallery` 统一挂载在同一个 HTTP 端口与同一个 SQLite 数据库连接池上运行。

---

## 1. 新旧 API 对照总表（重要）

### 1.1 【旧版 API (Legacy V1)】— 供 `twitter-pic-react` (`main` 分支) 使用，完整保留不修改

> **代码位置**：`api/handlers_v1.go`（挂载于 `/api/twitter`）  
> **兼容承诺**：所有路径、查询参数、响应结构与状态码 100% 保持原样，确保 `twitter-pic-react` 的 `main` 分支无需任何修改即可正常使用。

| 方法 | 完整路径 | 参数说明 | 用途与返回格式（旧版 `main` 分支） |
| :--- | :--- | :--- | :--- |
| `GET` | `/api/twitter/` | `?list=users&after=<username>` | 获取 25 条用户列表（仅含 `username`, `last_modify`, `tags`, `status`，不含头像/昵称） |
| `GET` | `/api/twitter/` | `?by=username\|nick\|tag&search=<q>` | 搜索用户（返回 `[]User` 简表） |
| `GET` | `/api/twitter/:username.json.gz` | `?t=<last_modify>` | 获取该用户的原始 `.json.gz` 全量压缩包（带 `Content-Encoding: gzip` 与 CF 边缘缓存头） |
| `GET` | `/api/twitter/:username` | 无 | `302 Found` 重定向至 `/api/twitter/:username.json.gz?t=<last_modify>` |
| `POST` | `/api/twitter/:username` | `?do_not_tag=true` / `?do_not_renew=true` | 新增用户并打标、仅触发后台抓取更新、或仅修改标签 |
| `GET` | `/api/twitter/tags/:username` | 无 | 获取单用户的 `User` 结构（含 `tags` 权重字典） |
| `POST` | `/api/twitter/tags/:username` | Body: `{tag, d}` 或 `map[string]int` | 为单用户提交标签投票（一 IP 一票） |
| `POST` | `/api/twitter/tag` | Body: `{"user","tag","d"}` | 单标签投票（别名 `/api/twitter/account-tag`） |
| `GET` | `/api/twitter/emojis` | `?username=<username>` | 有 `username` 时返回该用户 Emoji 计数字典；无 `username` 时返回全站排行榜 JSON |
| `GET` | `/api/twitter/emojis.json.gz` | 无 | 获取 Gzip 压缩的全站 Emoji 排行榜 |
| `POST` | `/api/twitter/emojis` | `?username=<u>&emoji=<e>` | 为指定用户的 Emoji 投票 `+1` |
| `DELETE` | `/api/twitter/:username` | `?delete=<DELETE_KEY>` | 管理员封禁账号（置 `status='BANNED'`） |
| `PUT` | `/api/twitter/:username` | `?delete=<DELETE_KEY>` | 管理员解封账号（置 `status='SUCCESS'`） |

---

### 1.2 【新版 API (New V2)】— 供 `twitter-pic-react`（新分支）、`twitter-pic-flutter` 与 `gallery` 使用

> **代码位置**：`api/handlers_v2.go` & `api/models_v2.go`（同时挂载于 `/api/twitter/v2`、`/api/v2`、`/api/flutter`）  
> **核心升级**：
> 1. **视频封面 (`cover`) + `ouo.io` 广告变现链接 (`ouo_url`)**：所有视频条目自动将 `gallery_dl` 采集的 `cover` 重写为 `https://pbs.moonchan.xyz/...` 封面图，并生成指向 Go 专属视频重定向端点的 `ouo_url = "/api/twitter/v2/video/<path>?<query>"`（点击后由 Go 端将 host 直接替换为 `twimg.moonchan.xyz` 并通过 `ouo.io/qs` 或 `ouo.io/api` 302 重定向到 `ouo.io` 广告中转页，随后进入 `https://twimg.moonchan.xyz/<path>?<query>`）。前端仅渲染封面图，点击后跳转 `ouo.io` 获取广告收益并限流，彻底杜绝 `.mp4` 预加载打满 VPS 带宽。
> 2. **彻底消除 N+1 请求风暴**：用户列表、搜索、批量查询、排行榜一次性返回包含 `username`、`nick`、`avatar`、`banner`、`total_urls`、`tags`、`last_modify` 的完整富卡片（`FlutterUser`），侧边栏与收藏夹无需再逐个下载 `.json.gz`。
> 3. **Cloudflare 边缘强缓存 + 本地缓存协同**：支持 `?t=<last_modify>` 版本化 URL、`Cache-Control: public, max-age=..., s-maxage=..., stale-while-revalidate=...` 以及 `Last-Modified` / `304 Not Modified`。

| 方法 | 完整路径（以 `/api/twitter/v2` 为例） | 参数 / 请求体 | 用途与返回格式（新版 `twitter-pic-react` 分支） |
| :--- | :--- | :--- | :--- |
| `GET` | `/api/twitter/v2/users` | `?after=<cursor>&limit=25` | 全局用户富卡片列表（含头像、昵称、标签、媒体数、`next_cursor`） |
| `GET` | `/api/twitter/v2/search` | `?q=<kw>&by=username\|nick\|tag&limit=25` | 多维度搜索，直接返回 `FlutterUser` 富卡片数组 |
| `GET` | `/api/twitter/v2/tags/:tag/users` | `?limit=25&page=1&sort=updated\|weight` | 按标签分页反查富卡片列表（100% 剔除 BANNED 与幽灵死链账号） |
| `GET`/`POST` | `/api/twitter/v2/users/batch` | `?keys=u1,u2` 或 `{"users":["u1","u2"]}` | 批量水合多用户富卡片（供收藏夹单次请求加载） |
| `GET` | `/api/twitter/v2/users/:username/timeline` | `?t=<last_modify>` | **新版 React 核心接口**：单次返回 `account_info`、`tags`、`emojis` 及带 `cover` + `ouo_url` 的去重 `timeline` |
| `GET` | `/api/twitter/v2/users/:username/media` | `?type=all\|photo\|video&limit=24&cursor=0&t=` | 用户媒体瀑布流按需分页切片（每条视频均含 `cover` 与 `ouo_url`） |
| `GET` | `/api/twitter/v2/users/:username/profile` | `?t=<last_modify>` | 单用户轻量资料卡、照片/视频统计、标签与 Emoji 计数 |
| `GET` | `/api/twitter/v2/users/:username` | 无 | 单用户 `FlutterUser` 富卡片详情 |
| `POST` | `/api/twitter/v2/users/:username` | Body: `{"tags":{"女性":1}}` | 新增账号并写入初始标签，自动排入爬虫抓取队列 |
| `POST` | `/api/twitter/v2/users/:username/refresh` | 无 | 触发已注册账号的后台爬虫更新队列（替代旧版 `?do_not_tag=true`） |
| `POST` | `/api/twitter/v2/users/:username/tags` | Body: `{"tag":"...","d":1}` 或 `{"tags":{...}}` | 标签「一 IP 一票」投票（别名 `POST /api/twitter/v2/tags/vote`） |
| `GET` | `/api/twitter/v2/users/:username/emojis` | 无 | 获取单用户 Emoji 计数 |
| `POST` | `/api/twitter/v2/users/:username/emojis` | `?emoji=😍` 或 Body `{"emoji":"😍"}` | 为单用户 Emoji 投票 `+1`（别名 `POST /api/twitter/v2/emojis/vote`） |
| `GET` | `/api/twitter/v2/ranking` | 无 | Emoji 日/周/月排行榜 + 内嵌所有上榜用户的 `users` 富卡片字典（1 次请求渲染整页榜单） |
| `GET` | `/api/twitter/v2/feed` | 无 | 首页冷启动聚合（热门标签 + 最新用户 + 总账号数，30s 内存缓存） |
| `GET` | `/api/twitter/v2/tags/cloud` | `?limit=50` | 全局热门标签云统计 |
| `GET` | `/api/twitter/v2/video/*filepath` | 路径 `/ext_tw_video/...` 或 `?url=` | **视频广告中转专属端点**：直接将视频 host 替换为 `twimg.moonchan.xyz`（保留完整 path+query），并通过 `ouo.io/qs` 或 `ouo.io/api` 302 重定向到 `ouo.io` 广告页，完成后跳往 `https://twimg.moonchan.xyz/...` |

---

## 2. 文件分工

| 文件 | 职责说明 |
| :--- | :--- |
| `router.go` | `SetupRouter(db)`（统一单端口路由引擎，同时挂载旧版 V1 API、新版 V2 API 与 Gallery SSR） |
| `handlers_v1.go` | **【旧版 API】** V1 路由处理器（`/api/twitter/*`），专供 `twitter-pic-react` `main` 分支使用，保持不变 |
| `handlers_v2.go` | **【新版 API】** V2 路由处理器（`/api/twitter/v2/*`、`/api/v2/*`、`/api/flutter/*`），供 `twitter-pic-react` 新分支与现代客户端使用 |
| `video_redirect.go` | **【新版 API】** 视频链接专属重定向处理器（`HandleVideoRedirectV2`、`ReplaceHostDirectly`、`BuildOuoRedirectTarget`） |
| `models_v2.go` | **【新版 API】** V2 请求与响应模型（含 `V2MediaItem.Cover`、`V2MediaItem.OuoURL`、`UserTimelineResponse`、`RankingV2Response` 等） |
| `db_users.go` | `users` 表建表、用户状态更新（`commitUser` 封禁/解封联动清理标签云缓存）、V1 用户列表与搜索 SQL |
| `db_tags.go` | `CreateTableV2`（初始化标签表并迁移历史 `user_tags` 数据）、`Store()` 访问器、`getUserListByTag` |
| `db_emoji.go` | `CreateTableV3`、`VoteUpEmoji` 以及日/周/月 Emoji 排行榜定时结算 |
| `crawler_client.go` | `curlMetaData`：通过 TCP 向 `crawler/caller.py` 发送抓取任务 |
| `middleware_ban.go` | `StrictIPBanMiddleware`：基于 `ipban.Shared()` 的 Gin 封禁中间件 |
