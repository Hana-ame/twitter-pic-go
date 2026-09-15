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