# twitter-pic-go

Twitter 媒体抓取与图库浏览。

## 组件

| 组件 | 路径 | 说明 |
|------|------|------|
| TCP 服务器 | `caller.py` | 接收 Go 端发来的 username，调用 `get2.py` 抓取元数据 |
| 队列处理 | `deamon.py` | 从 pending 文件读取 URL，翻译成抓取命令，排队执行 |
| Go API | `twitter_handlers.go` | REST API：创建/查询元数据、标签管理、Emoji 投票 |
| 图库 | `gallery/` | 直接服务 HTML 的图站后端，读取 json.gz 渲染媒体列表 |
| 标签存储 | `tags/tags.go` | **账号标签的唯一实现**：`account_tags` 读写 + `request_logs` 流水，Go API 与 gallery 共用同一份语义 |
| IP 封禁 | `ipban/ipban.go` | **封禁的唯一实现**：bans.txt 编译 trie + 链上任一命中 + 热重载 + 统一 IP 口径，Go API 与 gallery 共用同一个单例 |
| twimg 反代 | `twimg/main.go` | 反向代理 pbs.twimg.com 图片 |

## 标签（账号级）的存储

**只有一个库、只有一张表**：`twitter.db` 的 `account_tags(username, tag, weight)`。

- 两层入口共用 `tags` 包：Go API 的 `GET /api/twitter/tags/:username` /
  `POST /api/twitter/:username` / `GET /api/twitter/?by=tag&search=<tag>`，与 gallery 的
  `GET /api/tags?keys=` / `POST /api/tag`（别名 `/api/account-tags`、`/api/account-tag`）。
- 写语义：`weight` 累加，恰好归零删行，负权重保留；每次 POST 的 delta 归一到 ±1；
  **每次写请求记一行 `request_logs`**（username / tags / ip / ua，两层都记）。
- 写失败**报 500**，不再吞掉错误回 `200 {"message":"ok"}`；gallery 侧对应 400/500/503。
  抓取排队（`curlMetaData`）失败不算写失败，只记日志——否则 caller.py 不在时会把
  一次成功的标签写入判成失败。
- 读语义：直接按 `account_tags` 现场聚合，不再读旧的 `user_tags` JSON 大字段，
  也不再有独立的 `tags.db` 快照或 `account_votes.json` 投票文件。
  两层的过滤口径**逐字一致**：`weight != 0` 都返回（含负权重）、`weight = 0` 都不返回。
  根 API 侧由 `userSelectQuery` 的 `AND a.weight != 0` 保证，与 `tags.Store.Weights` 对齐，
  并有测试钉住（`sql_tags_readparity_test.go`）——否则库上一旦出现零权行（外部 sqlite3
  运维直写、历史脏数据），同一账号两层会给出不同标签集。
- `by=tag` 的反查只取**正权重**、按权重降序、精确匹配（非 LIKE）、`status='SUCCESS'`、
  `LIMIT 15`；空结果返回 `[]` 而不是 `null`。
- gallery 的库路径由 `GALLERY_DB` 指定，默认 `./twitter.db`——与 Go API 同一个文件。
- 旧的 `user_tags` 表只作为历史遗留存在，服务端启动时一次性回填进
  `account_tags`（`migrateAccountTags`），此后不再作为数据源被读取。

## IP 封禁

**只有一个实现、一个实例**：`ipban/` 包（只依赖 `net/http` + `net/netip` + `go-iptrie`，
不吃 gin）。根 API 的 `StrictIPBanMiddleware`（`banManager.go`）与 gallery 的
`handlePostAccountTag` 各自薄薄包一层，调同一个 `ipban.Shared()`。

- **两条 IP 口径，别混用**：
  - `ipban.Chain(r)` / `Manager.Decide`：**封禁判定**。RemoteAddr host + XFF **全部**条目，
    **任一命中即封**。只看 XFF 首项会被 `X-Forwarded-For: <好人IP>, <被封IP>` 绕过。
  - `ipban.Principal(r)`：**「这个请求是谁」**，用于限流分桶与 `request_logs.ip`。
    退化顺序 **`CF-Connecting-IP` → XFF 从右往左第 `TRUSTED_PROXY_HOPS` 个 → RemoteAddr**。
    原先根 API 记整个 XFF 头串、gallery 记首项，两层流水根本对不上，现统一。
- **`CF-Connecting-IP` 优先**（`CF_CONNECTING_IP` 可关，默认开）：Cloudflare 总是**覆写**
  这个头，所以经 CF 进来的请求伪造不了它，比数 XFF 跳数可靠。
  ⚠️ 它可信的**前提**是「源站只允许 CF 回源」。只要存在绕过 CF 直连源站的通路
  （源站 IP 泄露、别的域名/端口直回源、IPv6 没纳入限制），这个头和 XFF 一样可被任意伪造，
  那时唯一可靠做法是**防火墙只放行 CF 网段**。bwh 的 ufw 是否已经这么做：**未验证**，
  所以这里是「写明依赖」，不是「已经安全」。
- **`TRUSTED_PROXY_HOPS` 默认 2**（CF + nginx 两跳）。判据——**两种配错都会错一格**：

  | nginx 行为 | 到达源站的 XFF | 真实客户端位置 | 该配 |
  |---|---|---|---|
  | `proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for`（**追加**） | `<自报…>, <真实客户端>, <CF 边缘>` | 右数第 **2** | `2` |
  | 原样**透传**（没有那一行） | `<自报…>, <真实客户端>` | 右数第 **1** | `1` |

  本站实测为**追加**：线上 `request_logs.ip` 的形态是 `183.34.64.0, 104.22.109.48`
  （左为真实客户端、右为 CF 边缘段），故默认 2。
- **配错要能看见，不静默假绿**：启动时 `ipban.LogEffectiveConfig()` 打印实际生效口径与
  封禁表加载条数；链长撑不起配置跳数时归属会退化到「客户端可自报的最左项」，该退化按次
  计入 `xff-clamped`，并在每次热重载时由 `warnPrincipalAnomaly` 打日志（含来源分布）。
- **同一份实例**：`Shared()` 用 `sync.Once` 给出进程级单例，热重载协程（`BAN_RELOAD_MINUTES`
  默认 10 分钟）也只挂这一个。两份内存副本各自 reload 会出现「API 侧已封、gallery 侧没封」。
- 清单路径 `BANS_FILE`（默认 `bans.txt`）；文件缺失=空表放行（封禁清单丢了不该变成全站 403）；
  非法行跳过并告警，不因一行脏数据丢掉整张表。
- 403 响应体统一为 `ipban.Denied{error, reason, ip}`，两层逐字相同。

⚠️ 两个已知权衡：① `TRUSTED_PROXY_HOPS` 配错时 25/IP/h 配额仍可被伪造 XFF 换桶绕过
（封禁不受影响，它看整条链）；② 「链上任一命中」允许攻击者把别人的 IP 塞进 XFF
来陷害其被封——这是既有严格策略的固有权衡，收紧就得只查右数 N 项，代价是
真实 IP 不在链上时漏封。

## 被封账号的可见性

`users.status` 是唯一真源，判定口径**只有一条**：`status = 'SUCCESS'` 才算可见。

- 根 API：`GET /:fn` 对非 SUCCESS 回 200 + `banned.json` 占位（该文件缺失时退化 404）、
  账号列表与 `by=tag` 都带 `u.status = 'SUCCESS'`。
- 图站（gallery）：原先**只看磁盘上有没有 `<name>.json.gz`**，完全不看 status，
  实测置 BANNED 后根侧 `by=tag` 回 `[]` 而图站反查仍列出该账号。现已在全部读路径收口，
  统一 **404**（不确认「存在但被封」），细节与 URL 影响面见 `gallery/README.md`。
- 封禁生效时限：图站的 `users.status` 视图按 60s TTL 惰性刷新，不等重启进程。
- 降级方向是 **fail-open**（读不到 `users` 就不隐身）：把整站藏起来等于自我 DoS，
  而"谁被封"这件事本身没坏，坏的只是可见性——与 `bans.txt` 缺失=空表放行同向。

## 媒体来源

媒体文件（图片/视频）通过以下方式获取：

### 1. twimg 反代（内置）

`twimg/main.go` 是一个反向代理服务器，监听 `TWIMG_ADDR` 环境变量指定的地址。
它代理 `pbs.twimg.com` 的图片请求，同时支持 `video.twimg.com` 的 iframe 播放器。

gallery 通过 `GALLERY_MEDIA_BASE` 或 `TWIMG_ADDR` 环境变量来配置媒体 URL 前缀。

### 2. peerfs-proxy（WebRTC 方式）

`peerfs-proxy` 是一个独立的 ECH 代理节点，通过 WebRTC DataChannel 提供 twimg 媒体文件。
它运行在 `https://peerfs.moonchan.xyz/`，节点 ID 为 `twimg-proxy`。

**作用：** 当 twimg 反代无法直接访问（如网络限制）时，peerfs-proxy 通过 ECH 域前置加密连接到 `video-cf.twimg.com`，绕过封锁获取图片/视频，然后通过 WebRTC 传给浏览器。

**如何使用：**

```bash
# 浏览器打开 https://peerfs.moonchan.xyz/
# 连接信令 → 点击 twimg-proxy 节点 → 发送 read 请求读取文件

# 请求格式：
# {"type":"read","path":"twimg/media/xxx.jpg","offset":0,"size":-1,"reqId":"r1"}
# 响应：meta → 二进制数据块 → done
```

详细协议见 `peerfs-chat/README.md`。

## 数据流

```
Twitter API / X
    │
    ▼
get2.py (Python)  →  生成 <username>.json.gz
    │
    ├──→ gallery (图站浏览)
    │
    └──→ twimg 反代 / peerfs-proxy (获取实际图片/视频)
```