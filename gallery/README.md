# Gallery Server

一个直接服务 HTML 的 Go 图站后端。读取 `<account>.json.gz` 中的 twitter 媒体外链
（`pbs.twimg.com` / `video.twimg.com`），在服务端渲染首页摘要、目录浏览、分页、
标签搜索/排除、账号收藏与赞/踩。

gallery 作为独立包编译进单一二进制（见 `../server/main.go`），单独监听 `GALLERY_ADDR`。

## 运行

```bash
cd /home/lumin/twitter-pic-go

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

打开 `http://localhost:8090/` 直接浏览。首次访问会先经过 18+ 年龄验证页。

## 页面路由

- `GET /` — 首页：最新 / 最热 / 收藏 三块账号摘要（各 4 行，背景图为该账号最新一张图）
- `GET /latest` — 完整「最新」账号列表（按 db 的 last_modify 排序，翻页式）
- `GET /hot` — 「最热」账号列表（按 👍 总票数排序）
- `GET /favorites` — 我的收藏（收藏存浏览器 localStorage，前端过滤渲染）
- `GET /{username}` 或 `GET /?dir=...` — 某账号的媒体网格（10 条/页）
  - `tags` / `exclude_tags`：包含 / 排除标签（逗号分隔，跨账号搜索时默认递归）
  - `type`：`image` / `video` / `photo` / `animated_gif`
  - `sort`：`name` / `time` / `size` / `random`
  - `page`：页码
- `GET /tags` — 标签页：标签云 → 选 tag 后「按账号」/「按图片」两种视图
- `POST /rescan` — 重新扫描 json.gz 目录（保留远程源配置与 db 账号索引）
- `POST /react` — 单张媒体的 👍/👎（`media_id` + `emoji` + `voter` 识别码）
- `GET|POST /age-gate` — 年龄验证
- `GET /sitemap.xml` — 站点地图
- `POST /api/link` / `DELETE /api/link` — 管理账号外部链接（pixiv / fanbox 等，JSON body）

## 页面功能

- 右上角「识别码」徽标：浏览器本地身份（赞/踩 的 voter），支持「引继」导入其他识别码继承身份
- 账号卡片：头像、昵称（db users.nick）、@用户名、媒体数、最近更新日期、外部平台图标链接、☆ 收藏
- 视频/动图：通过 srcdoc iframe + no-referrer 绕过 video.twimg.com 防盗链

## 数据来源

- 扫描 `GALLERY_JSON_DIR` 下所有 `*.json.gz`
- 每个 json.gz 的 `account_info.name` 作为目录名（账号），`profile_image` 作为头像
- 只使用 `timeline[]` 的 `url`、`date`、`type`、`tweet_id` 四个字段
- `type` 保留原始值：`photo` / `video` / `animated_gif`
- 标签来自 `user_tags` 表中权重 > 0 的标签（账号级）；账号名本身也作为目录标签
- db `users` 表提供昵称与最近更新时间：还没有本地媒体的账号也会出现在账号列表中
