# Gallery Server

一个直接服务 HTML 的 Go 图站后端。读取 `<account>.json.gz` 中的 twitter 媒体外链
（`pbs.twimg.com` / `video.twimg.com`），在服务端渲染目录浏览、分页、标签搜索/排除、
访问历史、推荐与聚类页面。

## 运行

```bash
cd /home/lumin/twitter-pic-go

GALLERY_JSON_DIR=./ \
GALLERY_TAG_DB=./twitter.db \
GALLERY_DB=./gallery.db \
GALLERY_ADDR=:8090 \
go run ./gallery
```

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `GALLERY_ADDR` | `:8090` | HTTP 监听地址 |
| `GALLERY_JSON_DIR` | `./` | 存放 `<account>.json.gz` 的目录 |
| `GALLERY_TAG_DB` | `./twitter.db` | 读取 `user_tags` 表作为账号标签 |
| `GALLERY_DB` | `./gallery.db` | 访问历史 SQLite 数据库 |
| `GALLERY_IMAGE_PROXY` | 空 | 可选，图片代理基地址（如 `https://pbs.moonchan.xyz`） |
| `GALLERY_VIDEO_PROXY` | 空 | 可选，视频代理基地址（如 `https://twimg.moonchan.xyz`） |

打开 `http://localhost:8090/` 直接浏览。

## 页面路由

- `GET /` — 首页 index 模式：目录分类 + 最新媒体（按时间倒序，10 条/页）
- `GET /?dir=...` 或 `/?tags=...` — gallery 模式：面包屑 + 筛选栏 + 媒体网格（10 条/页）
  - `dir`：目录（账号），如 `/?dir=alice`
  - `tags`：包含标签，逗号分隔，如 `/?tags=cat,cute`
  - `exclude_tags`：排除标签，逗号分隔
  - `type`：`image` / `video` / `photo` / `animated_gif`
  - `sort`：`name` / `time` / `size` / `random`
  - `recursive=true|false`：带标签搜索时默认递归
  - `page`：页码
- `GET /tags` — 全部标签，`＋` 加入包含，`－` 加入排除
- `GET /recommendations` — 基于访问历史的推荐
- `GET /clusters` — k-means 自动聚类
- `GET /history` — 访问历史
- `POST /rescan` — 重新扫描 json.gz 目录
- `GET /proxy?id=<media_id>&url=<urlencoded>` — 代理原始外链并记录访问历史
  （只允许已索引的 `pbs.twimg.com` / `video.twimg.com` URL）

## 数据来源

- 扫描 `GALLERY_JSON_DIR` 下所有 `*.json.gz`
- 每个 json.gz 的 `account_info.name` 作为目录名（账号）
- 只使用 `timeline[]` 的 `url`、`date`、`type` 三个字段
- `type` 保留原始值：`photo` / `video` / `animated_gif`
- 标签来自 `user_tags` 表中权重 > 0 的标签，账号名也作为目录标签

## 推荐与聚类

- 每次通过 `/proxy` 成功加载媒体都会写入内存环形缓冲并异步写入 SQLite
  `access_history` 表
- 推荐：对最近 100 条访问记录的标签做 24 小时半衰期时间衰减，得到用户偏好，
  再对所有媒体计算亲和度，排除最近 20 条已看内容
- 聚类：基于媒体标签向量做 k-means（k-means++ 初始化，余弦相似度）
