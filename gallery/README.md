# gallery

twitter-pic-go 内的 SSR 图库包（`package gallery`），由 `server/main.go` 以
`go gallery.Run(os.Getenv("GALLERY_ADDR"))` 启动，**编进同一个二进制**。

## 设计

- 无全局 media 索引：首页只 `os.ReadDir` 列账号名；`/u/{account}` 才解析那一个账号。
- 账号页把 json 原文嵌进 HTML，前端做网格分页、TikTok 式竖滑全屏、赞/踩/喜欢。
- `?type=photo|video` 过滤；`?cursor=<tweet_id>` 游标（非偏移）。
- 赞/踩计数：`GET /api/reactions`、`POST /api/react`，落 `GALLERY_REACTIONS_FILE`。- 主页 tag 分类（后端处理）：`scripts/build_tags_db.py` 把 bwh `twitter.db` 的 `user_tags`（JSON 权重对象）拍平成独立 `tags.db` 的 `account_tags` 表；gallery 启动时一次性读入内存，标签云与每账号标签随 `#a-data`（`[{n,t}]`）嵌进首页，前端只做筛选展示。更新方式：bwh 上`VACUUM INTO` 快照 → scp 拉回 → 跑脚本 → 推 tags.db。- 卡片预览（头像/昵称/首图 banner）：前端流式 `GET /raw/{account}`（原样吐 json.gz，不解析），DecompressionStream 解压到够用作废，不占后端内存。`#a-meta` 可在离线导出时预烘焙当缓存。
- 纯标准库，模板/JS 用 `//go:embed` 编进二进制。

## env

| env | default | 说明 |
|---|---|---|
| `GALLERY_ADDR` | `:8090` | 监听地址 |
| `GALLERY_JSON_DIR` | `./` | json.gz 目录 |
| `GALLERY_MEDIA_BASE` | 空 | pbs.twimg.com 改写前缀 |
| `GALLERY_LEGACY_BASE` | `https://x.4545810.xyz` | 旧版站点基址；账号页「切换到旧版」跳 `{base}/{user}`，置空隐藏入口 |
| `GALLERY_PAGE_SIZE` | `12` | 每页媒体数 |
| `GALLERY_REACTIONS_FILE` | `./reactions.json` | 赞踩计数文件 |
| `GALLERY_TAGS_DB` | `./tags.db` | 主页标签分类数据库（`account_tags(username, tag, weight)`）；文件缺失自动降级为无标签 |

## 路由

`GET /` · `GET /u/{account}?type=&cursor=` · `GET /api/reactions` · `POST /api/react` · `GET /static/*` · `GET /healthz`
