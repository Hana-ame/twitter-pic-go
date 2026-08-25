# twitter-pic-go

Twitter 媒体抓取与图库浏览。

## 组件

| 组件 | 路径 | 说明 |
|------|------|------|
| TCP 服务器 | `caller.py` | 接收 Go 端发来的 username，调用 `get2.py` 抓取元数据 |
| 队列处理 | `deamon.py` | 从 pending 文件读取 URL，翻译成抓取命令，排队执行 |
| Go API | `twitter_handlers.go` | REST API：创建/查询元数据、标签管理、Emoji 投票 |
| 图库 | `gallery/` | 直接服务 HTML 的图站后端，读取 json.gz 渲染媒体列表 |
| twimg 反代 | `twimg/main.go` | 反向代理 pbs.twimg.com 图片 |

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