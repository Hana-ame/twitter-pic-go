# gallery

twitter-pic-go 内的 SSR 图库包（`package gallery`），由 `server/main.go` 以
`go gallery.Run(os.Getenv("GALLERY_ADDR"))` 启动，**编进同一个二进制**。

## 设计

- 无全局 media 索引：首页只 `os.ReadDir` 列账号名；`/u/{account}` 才解析那一个账号。
- 账号页把 json 原文嵌进 HTML，前端做网格分页、手机式全屏查看器（横向翻页 / 捏合与双击缩放 / 下滑关闭 / chrome 自动隐藏）、赞/踩/喜欢。
- `?type=photo|video` 过滤；`?cursor=<tweet_id>` 游标（非偏移）。
- 赞/踩计数：`GET /api/reactions`、`POST /api/react`，落 `GALLERY_REACTIONS_FILE`。
- 卡片预览（头像/昵称/首图 banner）：前端流式 `GET /raw/{account}`（原样吐 json.gz，不解析），DecompressionStream 解压到够用作废，不占后端内存。`#a-meta` 可在离线导出时预烘焙当缓存。
- 依赖：标准库 + 纯 Go 的 `modernc.org/sqlite` 驱动 + 仓库内的 `tags`（标签唯一真源）
  与 `limit`（写配额）两个包。模板/JS 用 `//go:embed` 编进二进制。
  （`limit` 带 gin 中间件，所以 gallery 不再是不依赖 gin 的纯标准库包；
  反正它与 API 编在同一个二进制里，换来的是两层共用同一个限流实现。）

## 标签（唯一真源：`twitter.db` 的 `account_tags`）

**只有一个库**。gallery 不再读独立的 `tags.db` 快照，也不再写 `account_votes.json`
投票文件；标签的读写全部走 `tags` 包（`tags/tags.go`），与根包的 twitter REST API
打同一张 `account_tags` 表、调同一套语义——两层是同一份实现，不存在漂移。

- **写**：`POST /api/tag`（别名 `POST /api/account-tag`），body `{user|key, tag, d}`。
  `d` 是**该 IP 的目标值**（`1`/`-1`/`0`=撤票，越界 400 不静默夹），服务端按
  `(账号,标签,IP)` 记票并把 `weight` **重算**为「历史底数 + Σ票」（不是累加），
  **恰好归零删行、负权重保留**；同时**照记一条 `request_logs` 流水**
  （username / tags / ip / ua，`ip` 取 `ipban.Principal`——与票桶、限流分桶同一个值）。
  完整契约（含响应体、错误码阶梯、恒等式与校验 SQL）在根 `README.md` 的
  「一 IP 一票」一节，两处改一处要同步。
- **读**：`GET /api/tags?keys=a,b`（别名 `GET /api/account-tags`）**直接用新数据源**，
  返回 `{user: {tag: weight}}`；账号页把同一份结果嵌进 `#g-atags-data`。
  接口按实返回（含负权重，与根 API 的 `GET /tags/:username` 同口径），
  **只显示正分**是前端的渲染规则（与首页卡片、`applyATagLocal` 一致）。
- **首页标签与排序**：`ForUsers`（只取正权重，分块 IN 走 PK 前缀）+
  `LastModifyFor`（时间源就是 `users.last_modify`，不再有 `accounts` 表）；
  全局标签云启动时聚合一次（top 36）作唯一缓存。
- **反查**：`GET /api/tag/{tag}?limit=`（权重降序、只算正权重、边扫边滤只留磁盘
  存在的账号，默认 2000 上限 50000）。
- **降级**：库文件缺失/打不开 → 无标签继续跑；建表失败 → 降为只读，POST 返回 503
  而不是静默丢投票。
- 并发：API 侧连接与 gallery 连接共同读写同一 sqlite 文件，靠 WAL + `busy_timeout(5000)` 兜底
  （两侧 DSN 都带 `busy_timeout`，否则并发写会直接抛 `SQLITE_BUSY`，
  「两层同一真源」就退化成「谁抢到谁写」）。
- **配额**：`POST` 走与根 API 同一个 `limit.FastLimiter`，默认每 IP 每小时 25 次
  （`GALLERY_TAG_RATE_MAX` 可调，对齐根 API 的 `NewFastLimiter(25)`）；超返 429，
  且**被拒的请求不写表也不写流水**。这一步是必须的——gallery 现在写的是权威表，
  没配额等于给整条标签链开了一个不限速的写入侧门。
- **IP 封禁**：`POST` 的第一道守卫是 `ipban.Manager.Decide`（与根 API 同一个进程级单例
  `ipban.Shared()`、同一份 `bans.txt`、同一个热重载协程）。被封 IP 一律 **403**，
  响应体 `{error, reason, ip}` 与根 API 逐字相同；且它在 `Add` 之前，
  所以被拒的请求**既不写 `account_tags` 也不写 `request_logs`**。
  判定取 `Chain(r)` = RemoteAddr host + XFF **全部**条目，**任一命中即封**——
  只看 XFF 首项的旧写法会被 `X-Forwarded-For: <好人IP>, <被封IP>` 直接绕过。
- 历史：`scripts/build_tags_db.py`（把快照拍平成独立 tags.db）随该管线一并**停用**，
  代码已无任何引用，仅留在仓库里备查。

## env

| env | default | 说明 |
|---|---|---|
| `GALLERY_ADDR` | `:8090` | 监听地址 |
| `GALLERY_JSON_DIR` | `./` | json.gz 目录 |
| `GALLERY_MEDIA_BASE` | `https://pbs.twimg.com` | 图片（含头像/封面/媒体）初始基址；前端按 `pbs.twimg.com` → `twimg.l.moonchan.xyz:8443` → `pbs.moonchan.xyz` 顺序 fallback（含 drop 超时检测） |
| `GALLERY_VIDEO_BASE` | `https://video.twimg.com` | 视频初始基址；前端按 `video.twimg.com` → `twimg.l.moonchan.xyz:8443` 顺序 fallback（含 drop 超时检测） |
| `GALLERY_LEGACY_BASE` | `https://x.4545810.xyz` | 旧版站点基址；账号页「切换到旧版」跳 `{base}/{user}`，置空隐藏入口 |
| `GALLERY_PAGE_SIZE` | `12` | 每页媒体数 |
| `GALLERY_REACTIONS_FILE` | `./reactions.json` | 赞踩计数文件 |
| `GALLERY_DB` | `./twitter.db` | 标签唯一真源（`account_tags` + `request_logs`），**与 twitter API 同一个库**；文件缺失自动降级为无标签 |
| `GALLERY_TAG_RATE_MAX` | `25` | 标签写入的每 IP 每小时配额（与根 API 同量） |
| `BANS_FILE` | `./bans.txt` | **进程级共用**（`ipban.Shared()`）：IP 封禁清单，单 IP + CIDR，`#` 注释；非法行跳过不丢整表；缺失=空表放行 |
| `BAN_RELOAD_MINUTES` | `10` | **进程级共用**：bans.txt 热重载周期，全进程只有这一个协程 |
| `CF_CONNECTING_IP` | `1` | **进程级共用**：`Principal` 是否优先读 `CF-Connecting-IP`（CF 覆写、经 CF 不可伪造）。设 `0/off` 可关 |
| `TRUSTED_PROXY_HOPS` | `2` | **进程级共用**：退化时 `Principal` 从 XFF 右往左数第 N 个（N=真实代理层数：CF+nginx 追加=2、透传=1）。限流分桶与 `request_logs.ip` 用它 |

⚠️ IP 口径的两条前提（代码自证不了，见根 `README.md` 的「IP 封禁」，含判据表）：
① 优先信 `CF-Connecting-IP` 的前提是**源站只允许 CF 回源**，否则它同样可伪造，唯一可靠
做法是防火墙只放行 CF 网段（**bwh 是否已这么做：未验证**）；② 退化数跳数时
`TRUSTED_PROXY_HOPS` 必须等于真实层数（追加=2 / 透传=1，本站实测是追加）。
配错的表现是限流可被换桶绕过，封禁不受影响（看整条链）。启动时会打印生效口径，
归属退化按次计数并在热重载时告警——不静默假绿。

已废弃（代码不再读取）：`GALLERY_TAGS_DB`、`GALLERY_ACCOUNT_VOTES_FILE`、`GALLERY_MEDIA_TAGS_FILE`。

## 被封账号隐身

真源是 `twitter.db` 的 `users.status`，判定只有一条：**`status = 'SUCCESS'` 才可见**，
与根 API 逐字同向。图站原先只看「磁盘上有没有 `<name>.json.gz`」，完全不看 status。

实现在 `visibility.go` 的 `vis`（60s TTL 缓存视图，标签云在同一次刷新里由 SQL 排除，
不做"事后扣减"——扣减用快照会漏掉窗口内被封账号新增的标签，这个缺陷是实测出来的）。

**用 404 而不是 403**：403 会确认「这个账号存在但被封」，且图站没有授权语义，
403 会被理解成"换个身份就能看"；而图站找不到账号本来就是 `http.NotFound`。
因此被封账号在图站上与"从不存在"完全不可区分。批量接口 `/api/tags?keys=` 用
**省略 key** 表达同一件事（逐个 404 会打断整批）。

**不保留直链看快照的口子**：`/raw/{account}` 是最大的泄漏面（整个时间线原样吐出），
一并 404。

| URL | 被封账号 | 正常账号 |
|---|---|---|
| `GET /u/{name}` | **404**（原 200） | 200 |
| `GET /raw/{name}` | **404**（原 200，直吐快照） | 200 |
| `GET /`（列表 + `#a-data` + 标签云计数） | 不出现；只剩它 in 用的标签整条消失 | 正常 |
| `GET /api/tag/{tag}` | 不出现在 `users` | 正常 |
| `GET /api/tags?keys=`（含别名） | 该 key 从返回里省略 | 正常 |
| `POST /api/tag` 投给被封账号 | **404**，不落库不落流水 | 正常 |

降级方向是 **fail-open**：`users` 表读不到时不隐身（把整站藏起来等于自我 DoS），
并在日志里吼一声。磁盘有 `json.gz` 但 `users` 表里没行的账号**按可见处理**——
那是"没注册过"不是"被封"，把它们一起隐藏会在数据形状不符合预期时清空首页。

## 路由

`GET /` · `GET /u/{account}?type=&cursor=` · `GET /raw/{account}` · `GET /api/tag/{tag}?limit=` · `GET /api/tags?keys=` · `POST /api/tag` · `GET /api/account-tags`（别名）· `POST /api/account-tag`（别名）· `GET /api/reactions` · `POST /api/react` · `GET /static/*` · `GET /healthz`

### `POST /api/tag` 的守卫阶梯

顺序固定，与根 API 的中间件链同构（被封的请求不该消耗自己的配额，
也不该靠状态码差异探出「标签功能开没开」）：

| 顺序 | 条件 | 码 | body |
|---|---|---|---|
| 1 | `ipban.Decide` 链上任一命中 | **403** | `{"error":"Access Denied","reason":"Banned IP detected in chain","ip":"<命中IP>"}` |
| 2 | 标签库缺失/不可写 | 503 | `tags disabled` |
| 3 | 超 `GALLERY_TAG_RATE_MAX` | 429 | `{"code":429,"message":"请求过于频繁，请一小时后再试"}` |
| 4 | 空 user/tag、`d` 缺失或 ∉ `{-1,0,1}`、user 含路径穿越 | 400 | `user, tag and d required` / `d must be -1, 0 or 1` |
| 5 | 投票目标是被封账号 | **404** | `404 page not found`（不给被封账号补标签）|
| 6 | 写库失败 | 500 | `write failed` |
| 7 | 成功 | 200 | `{"user":...,"tags":{tag:weight}}` |

1–5 都在 `tags.Add` 之前返回，因此**不会**留下 `account_tags` 行或 `request_logs` 流水。
