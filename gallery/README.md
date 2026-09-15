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
  `d` 归一到 ±1（与根 API 的 POST 归一化一致），事务内 `weight += d` 按行 upsert，
  **恰好归零删行、负权重保留**；同时**照记一条 `request_logs` 流水**
  （username / tags / ip / ua，ip 取 `X-Forwarded-For` 首个地址）。
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
- **已知缺口（待决）**：根 API 的写路径还有 `StrictIPBanMiddleware`（`bans.txt` IP 黑名单），
  gallery 的 `POST /api/tag` **没有**这道检查。`BanManager` 核心不依赖 gin，但它在根包里，
  gallery 复用它就要连带引入整个 `twitter` 包（含 gin 与全局 `DB`）——属于架构耦合决策，
  未擅自做。当前实际风险被上面的 per-IP 配额压到与根 API 同量级。
- 历史：`scripts/build_tags_db.py`（把快照拍平成独立 tags.db）随该管线一并**停用**，
  代码已无任何引用，仅留在仓库里备查。

## env

| env | default | 说明 |
|---|---|---|
| `GALLERY_ADDR` | `:8090` | 监听地址 |
| `GALLERY_JSON_DIR` | `./` | json.gz 目录 |
| `GALLERY_MEDIA_BASE` | 空 | pbs.twimg.com 改写前缀 |
| `GALLERY_LEGACY_BASE` | `https://x.4545810.xyz` | 旧版站点基址；账号页「切换到旧版」跳 `{base}/{user}`，置空隐藏入口 |
| `GALLERY_PAGE_SIZE` | `12` | 每页媒体数 |
| `GALLERY_REACTIONS_FILE` | `./reactions.json` | 赞踩计数文件 |
| `GALLERY_DB` | `./twitter.db` | 标签唯一真源（`account_tags` + `request_logs`），**与 twitter API 同一个库**；文件缺失自动降级为无标签 |
| `GALLERY_TAG_RATE_MAX` | `25` | 标签写入的每 IP 每小时配额（与根 API 同量） |

已废弃（代码不再读取）：`GALLERY_TAGS_DB`、`GALLERY_ACCOUNT_VOTES_FILE`、`GALLERY_MEDIA_TAGS_FILE`。

## 路由

`GET /` · `GET /u/{account}?type=&cursor=` · `GET /raw/{account}` · `GET /api/tag/{tag}?limit=` · `GET /api/tags?keys=` · `POST /api/tag` · `GET /api/account-tags`（别名）· `POST /api/account-tag`（别名）· `GET /api/reactions` · `POST /api/react` · `GET /static/*` · `GET /healthz`
