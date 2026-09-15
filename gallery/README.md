# gallery

twitter-pic-go 内的 SSR 图库包（`package gallery`），由 `server/main.go` 以
`go gallery.Run(os.Getenv("GALLERY_ADDR"))` 启动，**编进同一个二进制**。

## 设计

- 无全局 media 索引：首页只 `os.ReadDir` 列账号名；`/u/{account}` 才解析那一个账号。
- 账号页把 json 原文嵌进 HTML，前端做网格分页、TikTok 式竖滑全屏、赞/踩/喜欢。
- `?type=photo|video` 过滤；`?cursor=<tweet_id>` 游标（非偏移）。
- 赞/踩计数：`GET /api/reactions`、`POST /api/react`，落 `GALLERY_REACTIONS_FILE`。
- 纯标准库，模板/JS 用 `//go:embed` 编进二进制。

## env

| env | default | 说明 |
|---|---|---|
| `GALLERY_ADDR` | `:8090` | 监听地址 |
| `GALLERY_JSON_DIR` | `./` | json.gz 目录 |
| `GALLERY_MEDIA_BASE` | 空 | pbs.twimg.com 改写前缀 |
| `GALLERY_PAGE_SIZE` | `12` | 每页媒体数 |
| `GALLERY_REACTIONS_FILE` | `./reactions.json` | 赞踩计数文件 |

## 路由

`GET /` · `GET /u/{account}?type=&cursor=` · `GET /api/reactions` · `POST /api/react` · `GET /static/*` · `GET /healthz`
