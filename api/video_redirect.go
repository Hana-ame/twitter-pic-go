// ============================================================================
// 【新版 API 专属模块】视频链接重定向处理器 (video_redirect.go)
//
// 职责与完整链路说明：
//   1. 前端（twitter-pic-react 新分支 / Gallery）将视频卡片替换为：
//      - 封面图 (cover)：直接将 pbs.twimg.com 替换 host 为 pbs.moonchan.xyz
//        例如：https://pbs.moonchan.xyz/ext_tw_video_thumb/1916894180884062208/pu/img/X472TmC88Oy-kvGn?format=jpg&name=orig
//      - 点击链接：指向本后端专属视频端点
//        路径式：/api/twitter/v2/video/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/xxx.mp4?tag=12
//        参数式：/api/twitter/v2/video?url=https://video.twimg.com/ext_tw_video/.../xxx.mp4?tag=12
//   2. 本端点接收到视频请求后：
//      将原视频地址的 host 直接替换为 twimg.moonchan.xyz（完整保留 path 与 query），
//      直接 302 到代理链接（2026-10-23 起广告跳转已关闭，不再经 ouo.io 广告中转页）：
//      https://twimg.moonchan.xyz/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/xxx.mp4?tag=12
// ============================================================================

package api

import (
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

const (
	// DefaultImageCDNHost 唯一图片与封面资源 CDN Host（直接替换 pbs.twimg.com）
	DefaultImageCDNHost = "pbs.moonchan.xyz"

	// DefaultVideoProxyHost 视频 Cloudflare Worker 代理 Host（直接替换 video.twimg.com）
	DefaultVideoProxyHost = "twimg.moonchan.xyz"

	// DefaultVideoEndpointPrefix 前端点击视频时指向的 Go 后端专属端点前缀
	DefaultVideoEndpointPrefix = "/api/twitter/v2/video"
)

// ReplaceHostDirectly 将任意 URL 的 host 直接替换为 targetHost（默认 https 协议），
// 完整保留原始 Path 与 RawQuery。
// 例如：
//   ReplaceHostDirectly("https://video.twimg.com/ext_tw_video/123/pu/vid/720x1280/a.mp4?tag=12", "twimg.moonchan.xyz")
//   => "https://twimg.moonchan.xyz/ext_tw_video/123/pu/vid/720x1280/a.mp4?tag=12"
func ReplaceHostDirectly(rawURL, targetHost string) string {
	rawURL = strings.TrimSpace(rawURL)
	targetHost = strings.TrimSpace(targetHost)
	targetHost = strings.TrimPrefix(targetHost, "https://")
	targetHost = strings.TrimPrefix(targetHost, "http://")
	targetHost = strings.TrimRight(targetHost, "/")
	if rawURL == "" || targetHost == "" {
		return rawURL
	}

	if strings.HasPrefix(rawURL, "/") {
		return "https://" + targetHost + rawURL
	}

	u, err := url.Parse(rawURL)
	if err != nil || u.Path == "" {
		return rawURL
	}
	u.Scheme = "https"
	u.Host = targetHost
	return u.String()
}

// BuildVideoEndpointURL 将原始视频链接（如 https://video.twimg.com/ext_tw_video/.../xxx.mp4?tag=12）
// 转换为指向本 Go 服务专属视频端点的链接：
//   /api/twitter/v2/video/ext_tw_video/.../xxx.mp4?tag=12
func BuildVideoEndpointURL(rawVideoURL string) string {
	rawVideoURL = strings.TrimSpace(rawVideoURL)
	if rawVideoURL == "" {
		return ""
	}
	if strings.HasPrefix(rawVideoURL, DefaultVideoEndpointPrefix+"/") {
		return rawVideoURL
	}
	u, err := url.Parse(rawVideoURL)
	if err != nil || u.Path == "" {
		return DefaultVideoEndpointPrefix + "?url=" + url.QueryEscape(rawVideoURL)
	}
	p := u.Path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	out := DefaultVideoEndpointPrefix + p
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

func resolveVideoProxyHost() string {
	if v := strings.TrimSpace(os.Getenv("GALLERY_VIDEO_BASE")); v != "" {
		v = strings.TrimPrefix(v, "https://")
		v = strings.TrimPrefix(v, "http://")
		v = strings.TrimRight(v, "/")
		if v != "" {
			return v
		}
	}
	return DefaultVideoProxyHost
}

// ExtractAndBuildProxyVideoURL 从 HTTP 请求（支持 /*filepath 路径式与 ?url= 参数式）中提取视频路径与参数，
// 并通过直接替换 host 构造 https://twimg.moonchan.xyz/<path>?<query> 代理链接。
func ExtractAndBuildProxyVideoURL(req *http.Request, pathParam string) (string, bool) {
	proxyHost := resolveVideoProxyHost()

	// 1. 优先检查查询参数 ?url= / ?u= / ?s=（当路径为空或根路径时）
	trimmedPath := strings.TrimSpace(pathParam)
	if trimmedPath == "" || trimmedPath == "/" {
		q := req.URL.Query()
		rawParam := strings.TrimSpace(q.Get("url"))
		if rawParam == "" {
			rawParam = strings.TrimSpace(q.Get("u"))
		}
		if rawParam == "" {
			rawParam = strings.TrimSpace(q.Get("s"))
		}
		if rawParam == "" {
			return "", false
		}
		u, err := url.Parse(rawParam)
		if err != nil || u.Path == "" || u.Path == "/" {
			return "", false
		}
		return ReplaceHostDirectly(rawParam, proxyHost), true
	}

	// 2. 路径式：/api/twitter/v2/video/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/xxx.mp4?tag=12
	if !strings.HasPrefix(trimmedPath, "/") {
		trimmedPath = "/" + trimmedPath
	}
	target := "https://" + proxyHost + trimmedPath
	if req.URL.RawQuery != "" {
		target += "?" + req.URL.RawQuery
	}
	return target, true
}

// HandleVideoRedirectV2 Gin 处理器：
// 接收视频链接 -> 直接替换 host 为 twimg.moonchan.xyz（保留完整 path+query） -> 302 直连代理地址。
// （2026-10-23 起广告跳转已关闭：不再构造 ouo.io 广告中转链接，一律直接 302 到 https://twimg.moonchan.xyz/...）
func HandleVideoRedirectV2(c *gin.Context) {
	pathParam := c.Param("filepath")
	proxyVideoURL, ok := ExtractAndBuildProxyVideoURL(c.Request, pathParam)
	if !ok || proxyVideoURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "missing video path or url parameter",
		})
		return
	}

	// 禁缓存，避免浏览器/CDN 粘住重定向地址。
	c.Header("Cache-Control", "no-cache, no-store, private, must-revalidate")
	c.Redirect(http.StatusFound, proxyVideoURL)
}
