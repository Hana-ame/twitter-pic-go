# Twitter Pic Go — V2 API 设计与客户端接口规范

> **适用端**：Flutter 移动端 / 桌面端、现代 Web 前端以及所有需要低延迟、低带宽的客户端。  
> **服务基址（Base URL）**：
> - 直连基址（Flutter 推荐）：`https://x.moonchan.xyz/api/twitter/v2`
> - 根前缀别名：`https://x.moonchan.xyz/api/v2` 或 `https://x.moonchan.xyz/api/flutter`

---

## 1. 架构定位：Cloudflare 边缘缓存 与 V2 动态轻量切片

本项目采用 **Cloudflare 边缘 CDN + Go 轻量切片源站** 的架构：

```
┌────────────────────────────────────────────────────────────────────────┐
│               架构核心：Cloudflare 边缘托管 + V2 轻量切片                │
└────────────────────────────────────────────────────────────────────────┘
                                     │
                 ┌───────────────────┴───────────────────┐
                 ▼                                       ▼
     【边缘缓存层】Cloudflare CDN                 【源站切片层】Go V2 REST API
     ├── 承接全球访问、边缘强缓存与 304 协商     ├── 面向 Flutter / Web 的现代 JSON 切片
     ├── 命中 Cache-Control s-maxage 毫秒级直出  ├── 分页切片（~3KB）、冷启动 Feed、实时投票
     ├── 完全吸收高并发流量，保护源站小内存      ├── 100% 杜绝 404 幽灵账号，消除 N+1 风暴
     └── 客户端（Flutter）无需处理任何 json.gz   └── 内部仅设 ≤30 个用户的有界淘汰缓存（<2MB）
```

### 1.1 客户端完全不需要处理 `json.gz`
* **历史背景**：服务端抓取落盘的 `<username>.json.gz` 本质是后端全量冷备文件。过去旧客户端因缺乏合适接口而被迫直接下载整个压缩包（单文件 5~20MB），造成巨额流量浪费与手机端解包卡死。
* **现代架构（Cloudflare 负责缓存）**：
  * **所有缓存已由 Cloudflare CDN 在边缘接管**：V2 所有只读接口均下发规范的 HTTP `Cache-Control: public, max-age=..., s-maxage=...` 及 `Last-Modified`。
  * **客户端零缓存包袱**：Flutter / 移动端**绝不需要**下载、解压或自行维护 `.json.gz`。客户端直接请求标准 V2 JSON 接口，绝大部分请求直接由 Cloudflare 边缘节点以毫秒级返回，彻底告别流量雪崩。

### 1.2 V2 API 的核心使命（尽可能减轻负担）
* **彻底根除 N+1 请求**：单次请求直接返回包含 `username`、`nick`、`avatar`、`total_urls`、`tags`、`status` 的完整卡片对象，彻底告别“拉取用户名后再发 25 次请求补齐信息”。
* **100% 杜绝 404 幽灵账号**：SQL 联表强制保证 `u.status = 'SUCCESS'` 且存在于数据库，绝不向客户端吐出注定 404 的死链账号。
* **瀑布流按需分页（按需切片）**：媒体流支持 `limit` / `cursor` / `type`（photo/video），首屏由 10MB 压减至 **~3KB**，节约 99% 流量与移动端内存。
* **小内存保护（528MB 机器安全）**：服务端内部针对 timeline 采用容量上限为 30 的有界淘汰缓存（Bounded Cache），内存占用严格低于 2MB；针对首页 Feed 采用 30 秒内存自愈缓存，高并发下 SQLite 零压力。

---

## 2. 接口详细定义

### 2.1 首页 Feed 聚合（冷启动秒开）
一次请求获取热门标签、推荐用户与系统活跃统计，冷启动仅需单次往返。服务端 30s 内存缓存。

* **方法与路径**：`GET /api/twitter/v2/feed`（别名 `/api/v2/feed`、`/api/flutter/feed`）
* **响应格式**：
```json
{
  "top_tags": [
    { "tag": "女性", "count": 3520 },
    { "tag": "自拍", "count": 2100 }
  ],
  "recent_users": [
    {
      "username": "alice",
      "nick": "爱丽丝",
      "avatar": "https://pbs.twimg.com/avatar_alice.jpg",
      "total_urls": 42,
      "tags": { "女性": 5, "自拍": 3 },
      "last_modify": "2026-10-06T10:00:00Z",
      "status": "SUCCESS",
      "json_gz_url": "/api/twitter/alice.json.gz?t=2026-10-06T10%3A00%3A00Z"
    }
  ],
  "total_users": 5280,
  "updated_at": "2026-10-06T10:30:00Z"
}
```

---

### 2.2 标签反查用户列表（分页 + 完整卡片）
专为 Flutter 标签页设计，彻底解决旧接口只有裸字符串与 404 幽灵账号的严重性能退化。

* **方法与路径**：`GET /api/twitter/v2/tags/:tag/users`（别名 `/tag/:tag/users`）
* **参数**：
  * `limit` (int): 每页条数（默认 25，最大 100）
  * `page` (int): 页码（从 1 开始）
  * `offset` (int): 偏移量（若传则覆盖 `(page-1)*limit`）
  * `sort` (string): `updated`（默认，最新打标最前）或 `weight`（按该标签权重降序）
* **契约保证**：
  1. 仅返回真实存在且 `status = 'SUCCESS'` 的账号（0 幽灵账号、0 死链）；
  2. 仅返回当前标签投票 `> 0` 的账号（严格过滤负权票与撤票）；
  3. 单次返回完整用户元数据，列表直接秒开。
* **响应格式**：
```json
{
  "tag": "女性",
  "total": 3520,
  "count": 25,
  "page": 1,
  "limit": 25,
  "offset": 0,
  "has_more": true,
  "users": [
    {
      "username": "alice",
      "nick": "爱丽丝",
      "avatar": "https://pbs.twimg.com/avatar_alice.jpg",
      "total_urls": 42,
      "tags": { "女性": 5, "自拍": 3 },
      "last_modify": "2026-10-06T10:00:00Z",
      "status": "SUCCESS",
      "json_gz_url": "/api/twitter/alice.json.gz?t=2026-10-06T10%3A00%3A00Z"
    }
  ]
}
```

---

### 2.3 用户媒体瀑布流分页（按需轻量切片）
替代一次性下载几十兆完整 `json.gz`，按需分批拉取图片或视频。支持 HTTP 304 条件缓存。

* **方法与路径**：`GET /api/twitter/v2/users/:username/media`
* **参数**：
  * `limit` (int): 每页条数（默认 24，最大 100）
  * `cursor` / `offset` (int): 偏移量（首次传 0，后续传入上一次返回的 `next_cursor`）
  * `type` (string): 筛选媒体类型，可选 `all`（全部，默认）、`photo`（图片）、`video`（视频及动图）
* **响应头**：`Last-Modified`（支持带 `If-Modified-Since` 请求返回 `304 Not Modified`）
* **响应格式**：
```json
{
  "username": "alice",
  "total": 350,
  "count": 24,
  "page": 1,
  "limit": 24,
  "offset": 0,
  "next_cursor": 24,
  "has_more": true,
  "filter": "photo",
  "json_gz_url": "/api/twitter/alice.json.gz?t=2026-10-06T10%3A00%3A00Z",
  "media": [
    {
      "tweet_id": 182736452819,
      "url": "https://pbs.twimg.com/media/xxx.jpg",
      "type": "photo",
      "date": "2026-10-06T10:00:00Z"
    }
  ]
}
```

---

### 2.4 用户轻量资料概览与统计
用于用户主页头部资料卡与计数展示，避免为了统计照片/视频总数而下载整个 timeline。支持 HTTP 304 条件缓存。

* **方法与路径**：`GET /api/twitter/v2/users/:username/profile`
* **响应头**：`Last-Modified`（支持带 `If-Modified-Since` 请求返回 `304 Not Modified`）
* **响应格式**：
```json
{
  "username": "alice",
  "nick": "爱丽丝",
  "avatar": "https://pbs.twimg.com/avatar_alice.jpg",
  "total_urls": 350,
  "photo_count": 320,
  "video_count": 30,
  "tags": { "女性": 5, "自拍": 3 },
  "last_modify": "2026-10-06T10:00:00Z",
  "status": "SUCCESS",
  "json_gz_url": "/api/twitter/alice.json.gz?t=2026-10-06T10%3A00%3A00Z"
}
```

---

### 2.5 全局用户列表
* **方法与路径**：`GET /api/twitter/v2/users`（别名 `/users/list`）
* **参数**：
  * `after` (string): 游标分页（传入上一页末尾用户的 username，推荐）
  * `limit` (int): 每页条数（默认 25，最大 100）
  * `page` / `offset` (int): 页码分页（可选）
* **响应格式**：
```json
{
  "count": 25,
  "limit": 25,
  "has_more": true,
  "next_cursor": "last_username",
  "users": [ ... ]
}
```

---

### 2.6 批量获取用户卡片
客户端本地已有多个用户名时，单次换取这批用户的头像、昵称、标签与媒体数。

* **方法与路径**：`GET /api/twitter/v2/users/batch?keys=u1,u2...` 或 `POST /api/twitter/v2/users/batch`
* **POST 请求体**：`{"users": ["u1", "u2"]}` 或直接 `["u1", "u2"]`
* **响应格式**：
```json
{
  "count": 2,
  "users": [ ... ],
  "by_name": {
    "u1": { "username": "u1", "nick": "...", "avatar": "...", "tags": { ... } },
    "u2": { "username": "u2", "nick": "...", "avatar": "...", "tags": { ... } }
  }
}
```

---

### 2.7 标签投票（一 IP 一票契约）
统一移动端与 Web 端的投票模型，消除冗余 URL flag。

* **方法与路径**：`POST /api/twitter/v2/users/:username/tags` 或 `POST /api/twitter/v2/tags/vote`
* **请求体**：
  - 单标签增减：`{"tag": "自拍", "d": 1}`（`d` 取 `1` 投 / `-1` 减 / `0` 撤票）
  - 全量期望态：`{"tags": {"女性": 1, "自拍": 0}}`
  - 若调用通用 `/tags/vote`，可在 body 中增加 `"user": "alice"` 字段。
* **响应格式**：
```json
{
  "username": "alice",
  "tags": { "女性": 5, "自拍": 0 },
  "message": "votes applied successfully"
}
```

---

### 2.8 多维度增强搜索
* **方法与路径**：`GET /api/twitter/v2/search?q=xxx&by=tag|nick|username&limit=25&page=1`
* **参数**：
  * `q` (string): 搜索关键词
  * `by` (string): 维度，`tag`（精确标签，按权重降序）、`nick`（昵称模糊匹配）、`username`（用户名模糊匹配，默认）
  * `limit`, `page`, `offset`: 分页参数
* **响应格式**：
```json
{
  "query": "猫",
  "by": "nick",
  "total": 5,
  "count": 5,
  "has_more": false,
  "users": [ ... ]
}
```

---

### 2.9 标签云统计
* **方法与路径**：`GET /api/twitter/v2/tags/cloud`（别名 `/tag-cloud`）
* **参数**：`limit` (int, 默认 50)
* **响应格式**：
```json
{
  "total": 50,
  "tags": [
    { "tag": "女性", "count": 3520 },
    { "tag": "自拍", "count": 2100 }
  ]
}
```

---

## 3. 缓存设计与负载分工（Cloudflare 边缘加速）

通过 Cloudflare 边缘 CDN 接管缓存，移动端与源站实现了双向极端减负：

### 3.1 边缘强缓存：Cloudflare 全球就近加速
- **分工**：源站配置了精准的 `Cache-Control: public, max-age=..., s-maxage=...` 响应头：
  - 媒体瀑布流 `/media` 与 标签云 `/tags/cloud`：Cloudflare 边缘缓存 10 分钟（`s-maxage=600`）
  - 用户资料 `/profile` 与 卡片详情 `/users/:u`：Cloudflare 边缘缓存 5 分钟（`s-maxage=300`）
  - 标签用户列表 `/tags/:tag/users` 与 全局列表 `/users`：Cloudflare 边缘缓存 2 分钟（`s-maxage=120`）
  - 首页 Feed `/feed`：Cloudflare 边缘缓存 1 分钟（`s-maxage=60`）
  - 投票写操作 `/tags`：严格 `no-cache, no-store`，杜绝边缘误缓存
- **效果**：无论全球多少客户端高频并发拉取瀑布流，**99% 以上请求均在 Cloudflare 边缘节点直接命中**，响应延迟压缩至 10ms 以内，完全不消耗源站 528MB 机器的带宽与 CPU。

### 3.2 客户端零包袱：无需关心 `json.gz`
- 客户端（Flutter / Web）完全不需要为了“缓存”去下载几十兆的 `.json.gz` 并手动解压。
- 移动端直接把 V2 当作标准 REST API 发起轻量请求（单页仅 ~3KB），缓存交给 Cloudflare 处理即可。

### 3.3 HTTP 304 条件请求支持
- 媒体切片、个人资料卡与用户卡片均支持标准 `Last-Modified`。
- Cloudflare 边缘或客户端重复刷新时，带上 `If-Modified-Since` 请求头，数据未变更时源站直接返回 **`304 Not Modified`**（0 字节负载），零反序列化开销。


