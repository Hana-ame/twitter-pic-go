# Gallery Server

一个直接服务 HTML 的 Go 图站后端。读取 `<account>.json.gz` 中的 twitter 媒体外链
（`pbs.twimg.com` / `video.twimg.com`），在服务端渲染首页摘要、目录浏览、分页、
标签搜索/排除、账号收藏与赞/踩。

gallery 作为独立包编译进单一二进制（见 `../server/main.go`），单独监听 `GALLERY_ADDR`。

## 运行

```bash
cd /mnt/d/Workplace/twitter-pic/go

GALLERY_JSON_DIR=./ \
GALLERY_TAG_DB=./twitter.db \
GALLERY_DB=./gallery.db \
GALLERY_ADDR=:8090 \
go run ./server
```

（`go run ./server` 会同时启动 twimg 反代与 API；gallery 在 `GALLERY_ADDR` 上独立监听。）

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `GALLERY_ADDR` | `:8090` | HTTP 监听地址 |
| `GALLERY_JSON_DIR` | `./` | 存放 `<account>.json.gz` 的目录 |
| `GALLERY_TAG_DB` | `./twitter.db` | 读取 `user_tags` 表作为账号标签；读取 `users` 表作为账号索引（昵称/最近更新） |
| `GALLERY_DB` | `./gallery.db` | 赞/踩、账号外链 SQLite 数据库 |
| `GALLERY_MEDIA_BASE` / `TWIMG_ADDR` | 空 | pbs.twimg.com 图片反代前缀；为空则直链原始 URL |
| `GALLERY_REMOTE_JSON_BASE` | 空 | 可选，远程 JSON 源（临时调试）。设置后：本地没有的账号按需从远端拉取元数据，仅存内存、绝不落盘 |
| `GALLERY_RATE_LIMIT` | `20` | 限流：每 IP 每秒允许请求数（令牌桶速率） |
| `GALLERY_RATE_BURST` | `60` | 限流：每 IP 突发额度（令牌桶初始/最大令牌数）；调大容忍更高瞬时并发，调小更严格 |
| `GALLERY_ADMIN_KEY` | 空 | 写接口（`POST /rescan`、`/api/link`）鉴权密钥；为空则匿名可写（会打印告警） |

打开 `http://localhost:8090/` 直接浏览。首次访问会先经过 18+ 年龄验证页。

## 页面路由

- `GET /` — 首页：最新 / 最热 / 收藏 三块账号摘要（各 4 行，背景图为该账号最新一张图）；`?q=` 按用户名/昵称子串搜索（复用「最新」排序口径，与侧栏搜索框一致）
- `GET /latest` — 完整「最新」账号列表（按账号最新媒体时间降序；无媒体、只有 db 索引的账号用 `users.last_modify` 兜底，与首页「最新」板块同口径；翻页式）
- `GET /hot` — 「最热」账号列表（按 👍 总票数排序）
- `GET /favorites` — 我的收藏（收藏存浏览器 localStorage，前端过滤渲染）
- `GET /{username}` 或 `GET /?dir=...` — 某账号的媒体网格（10 条/页）
  - `tags` / `exclude_tags`：包含 / 排除标签（逗号分隔，跨账号搜索时默认递归）
  - `type`：`image` / `video` / `photo` / `animated_gif`
  - `sort`：`name` / `time` / `size` / `random`
  - `page`：页码
- `GET /tags` — 标签页：标签云 → 选 tag 后「按账号」/「按图片」两种视图
- `GET /clusters` — 聚类页：按标签向量的 k-means 自动分组，跨账号找相似内容
  - `k`：簇数量（默认 6，1-30；页面有 6/12/18 三个预设按钮）
  - `item_limit`：每簇预览的媒体数（默认 12）
- `POST /rescan` — 重新扫描 json.gz 目录（保留远程源配置与 db 账号索引）
- `POST /react` — 单张媒体的 👍/👎（`media_id` + `emoji` + `voter` 识别码）
- `GET|POST /age-gate` — 年龄验证
- `GET /sitemap.xml` — 站点地图
- `POST /api/link` / `DELETE /api/link` — 管理账号外部链接（pixiv / fanbox 等，JSON body）

## 页面功能

- 侧边栏「搜索账号」：按用户名 / 昵称子串过滤，提交到 `/?q=...`（等价 `/latest?q=...`），全局可用
- 右上角「识别码」徽标：浏览器本地身份（赞/踩 的 voter），支持「引继」导入其他识别码继承身份
- 账号卡片：头像、昵称（db users.nick）、@用户名、媒体数、最近更新日期、外部平台图标链接、☆ 收藏
- Lightbox 灯箱：点击卡片页内预览大图/视频，← → 切换、Esc 关闭，`#lbN` 锚点直达第 N 张
- 网格与灯箱一律用 origin URL（不再改写 `name=` 档位）；视频 iframe 滚动到视口附近才创建
- 分页带页码窗口（基于当前 URL 渐进增强，无 JS 时退回上一页/下一页）
- 视频/动图：通过 srcdoc iframe + no-referrer 绕过 video.twimg.com 防盗链
- 侧栏「显示方式」：`网格 / 下拉` 两档切换（选择记浏览器 localStorage）。网格模式下账号列表显示为 `.account-card` 网格；下拉模式下替换为单个 `<select id="accountsDropdown">`，选项来自服务端渲染或前端 `ACCOUNTS` 数据兜底，选项跳转账号页面。**首页也支持下拉模式**：切到下拉时隐藏「最新/最热/收藏」摘要板块，只显示账号 `<select>`，避免视觉重叠。
- 账号卡片「⚙ / +🔗」编辑外部链接：弹窗式 modal（替代原生 `prompt()`），逐平台填写 URL，保存即 `POST /api/link`、清空即 `DELETE`，Esc 关闭；未知平台链接也会作为可编辑行保留，不会误删。

## 聚类（/clusters）

图库没有 embedding 服务，聚类走纯离线的「标签向量 k-means」，实现见 `recommend.go`：

1. 每张媒体用 `Media.ClusterTags()`（账号 tag + per-image tag）编码成二值向量，L2 归一化；
   刻意排除 `DirTags` —— 那是账号名，对每条媒体都相同，算进去会让聚类退化成「按账号分组」，
   和账号列表页完全重复。
2. k-means++ 挑初始中心（避免轮转抽样产生重复中心、导致簇塌缩），最多迭代 50 次。
3. 簇标签取簇内出现次数最多的标签；完全没有标签的媒体落到 `未分类` 簇。
4. 随机性全部走单个 `math/rand.Rand`（种子由媒体内容推导），同一批数据每次产出同样的聚类。

边界行为：所有媒体标签完全相同时数据没有区分度，结果退化为 1 个簇（不是实现问题）。
逻辑与页面渲染的单测见 `recommend_test.go`。

## JSON API（只读）

### GET /api/health

返回服务器状态。

```json
{"status":"ok","accounts":42,"media":12345}
```

### GET /api/accounts

账号列表，支持按用户名/昵称搜索。

```
?q=xxx          # 搜索用户名或昵称
&page=1         # 页码
&page_size=20   # 每页数量
```

### GET /api/media

媒体列表，支持目录/类型/排序/分页。

```
?dir=account          # 目录名，空=全站
&type=photo           # photo / video / animated_gif
&sort=time            # name / time / size / random
&page=1               # 页码
&page_size=20         # 每页数量
```

### GET /api/media/view

返回单个媒体条目的完整详情，含前后导航链接。

**参数：** 通过 URL 查询字符串或路径传递

| 参数 | 说明 | 示例 |
|------|------|------|
| `path` | 媒体虚拟路径 | `path=account/photo.jpg` |
| `dir` + `name` | 分拆传目录和文件名 | `dir=account&name=photo.jpg` |

**返回示例：**

```json
{
  "media": {
    "id": "abc123",
    "path": "account/photo.jpg",
    "dir": "account",
    "name": "photo.jpg",
    "type": "photo",
    "ext": "jpg",
    "size": 123456,
    "url": "http://.../proxy/...",
    "original_url": "https://pbs.twimg.com/media/abc123.jpg",
    "tweet_id": 123456789,
    "tags": ["tag1", "tag2"],
    "like_count": 5,
    "dislike_count": 1,
    "mod_time": "2026-08-25T00:00:00Z"
  },
  "index": 42,
  "total": 1000,
  "prev": "account/previous.jpg",
  "next": "account/next.jpg",
  "dir": "account",
  "dirIndex": 3,
  "dirTotal": 50,
  "dirPrev": "account/prev.jpg",
  "dirNext": "account/next.jpg"
}
```

**字段说明：**

| 字段 | 说明 |
|------|------|
| `media` | Media 完整对象，包含 URL、标签、赞踩、推文 ID 等 |
| `index` / `total` | 全局媒体列表中的位置索引和总数 |
| `prev` / `next` | 全局前后媒体路径（用于全部浏览时的翻页） |
| `dir` | 当前媒体所属目录 |
| `dirIndex` / `dirTotal` | 当前目录内的位置索引和总数 |
| `dirPrev` / `dirNext` | 当前目录内的前后媒体路径（用于目录浏览时的翻页） |

**错误：** 媒体不存在时返回 `{"error":"not found"}`，缺少参数时返回 `{"error":"path or dir+name required"}`。

**使用示例（curl）：**

```bash
# 通过 path 参数
curl http://localhost:8090/api/media/view?path=account/photo.jpg

# 通过 dir + name 参数
curl http://localhost:8090/api/media/view?dir=account&name=photo.jpg

# 通过 JavaScript 调用
fetch('/api/media/view?path=account/photo.jpg')
  .then(r => r.json())
  .then(data => console.log(data.media.url))
```

## 远程 JSON 源（临时调试）

设置 `GALLERY_REMOTE_JSON_BASE`（如 `https://x.example.com/api/twitter`）后开启**懒加载代理**：

- 访问本地没有的账号（`GET /{username}`）时，按需向 `{base}/{username}.json.gz?t=1` 拉取该账号元数据；
- 拉回的文档**仅存内存、绝不落盘**，随后走与本地文件相同的扫描逻辑建索引（`scanNext` 合并 `remoteDocs`）；
- 成功后内存缓存，失败进入 **负缓存**（`negativeTTL`，默认 10 分钟）避免对同一账号反复重试；
- 非法用户名（含 `..`、路径分隔符）直接拒绝，不发请求；
- 不设置该环境变量时整段逻辑完全不触发，本地目录扫描零改动。

> 联调需要真实的远端 server（返回 `gzDocument`：含 `account_info` 与 `timeline[]`，媒体 URL 须为 `pbs.twimg.com` / `video.twimg.com`）。`remote_test.go` 用 `httptest` 桩验证了 fetch → 索引合并 → 负缓存 整条路径。

## 数据来源

- 扫描 `GALLERY_JSON_DIR` 下所有 `*.json.gz`
- 每个 json.gz 的 `account_info.name` 作为目录名（账号），`profile_image` 作为头像
- 只使用 `timeline[]` 的 `url`、`date`、`type`、`tweet_id` 四个字段
- `type` 保留原始值：`photo` / `video` / `animated_gif`
- 标签来自 `user_tags` 表中权重 > 0 的标签（账号级）；账号名本身也作为目录标签
- db `users` 表提供昵称与最近更新时间：还没有本地媒体的账号也会出现在账号列表中
