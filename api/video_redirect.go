// ============================================================================
// 【新版 API 专属模块】视频链接重定向与 ouo.io 广告变现处理器 (video_redirect.go)
//
// 职责与完整链路说明：
//   1. 前端（twitter-pic-react 新分支 / Gallery）将视频卡片替换为：
//      - 封面图 (cover)：直接将 pbs.twimg.com 替换 host 为 pbs.moonchan.xyz
//        例如：https://pbs.moonchan.xyz/ext_tw_video_thumb/1916894180884062208/pu/img/X472TmC88Oy-kvGn?format=jpg&name=orig
//      - 点击链接：指向本后端专属视频端点
//        路径式：/api/twitter/v2/video/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/xxx.mp4?tag=12
//        参数式：/api/twitter/v2/video?url=https://video.twimg.com/ext_tw_video/.../xxx.mp4?tag=12
//   2. 本端点接收到视频请求后：
//      - 第一步（直接替换 host 生成代理链接）：
//        将原视频地址的 host 直接替换为 twimg.moonchan.xyz，完整保留 path 与 query：
//        https://twimg.moonchan.xyz/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/xxx.mp4?tag=12
//      - 第二步（通过 ouo.io/qs 或 ouo.io/api 重定向至广告中转页收钱）：
//        - 默认通过 ouo.io Quick Link (`http://ouo.io/qs/6z4kqAz7?s=<encoded_proxy_url>`)
//          或调用 `http://ouo.io/api/6z4kqAz7?s=<encoded_proxy_url>` 获取短链后（带内存缓存），
//          向浏览器返回 HTTP 302 Found 重定向到 ouo.io 广告中转页面；
//        - 用户在 ouo.io 完成广告跳转后，自动进入 https://twimg.moonchan.xyz/... 播放视频。
// ============================================================================

package api

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/Hana-ame/twitter-pic-go/limit"
	"github.com/gin-gonic/gin"
)

const (
	// DefaultImageCDNHost 唯一图片与封面资源 CDN Host（直接替换 pbs.twimg.com）
	DefaultImageCDNHost = "pbs.moonchan.xyz"

	// DefaultVideoProxyHost 视频 Cloudflare Worker 代理 Host（直接替换 video.twimg.com）
	DefaultVideoProxyHost = "twimg.moonchan.xyz"

	// DefaultOuoKey 默认 ouo.io 广告变现 Key
	DefaultOuoKey = "6z4kqAz7"

	// DefaultVideoEndpointPrefix 前端点击视频时指向的 Go 后端专属端点前缀
	DefaultVideoEndpointPrefix = "/api/twitter/v2/video"
)

var (
	ouoShortCacheMu sync.RWMutex
	ouoShortCache   = make(map[string]string)
	ouoShortKeys    []string
	ouoHTTPClient   = &http.Client{Timeout: 2 * time.Second}
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

func resolveOuoKey() string {
	if k := strings.TrimSpace(os.Getenv("GALLERY_OUO_KEY")); k != "" {
		return k
	}
	if k := strings.TrimSpace(os.Getenv("OUO_KEY")); k != "" {
		return k
	}
	return DefaultOuoKey
}

// BuildOuoRedirectTarget 根据代理后的视频 URL（https://twimg.moonchan.xyz/...）
// 构造跳转到 ouo.io 广告中转页的最终重定向地址：
//   - 当 OUO_MODE=api 时：调用 http://ouo.io/api/6z4kqAz7?s=<encoded_url> 获取短链接（如 https://ouo.io/xxxx）并缓存，失败自动回退 qs；
//   - 默认（或 OUO_MODE=qs 时）：使用 http://ouo.io/qs/6z4kqAz7?s=<encoded_url> 直接 302 到 ouo.io 广告页，随后自动转到 twimg.moonchan.xyz/...
func BuildOuoRedirectTarget(proxyVideoURL string) string {
	proxyVideoURL = strings.TrimSpace(proxyVideoURL)
	if proxyVideoURL == "" {
		return ""
	}

	if customPrefix := strings.TrimSpace(os.Getenv("GALLERY_OUO_PREFIX")); customPrefix != "" {
		return customPrefix + url.QueryEscape(proxyVideoURL)
	}
	if customPrefix := strings.TrimSpace(os.Getenv("OUO_PREFIX")); customPrefix != "" {
		return customPrefix + url.QueryEscape(proxyVideoURL)
	}

	key := resolveOuoKey()
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("OUO_MODE")))

	// 若启用了 api 模式，先查内存缓存，未命中则请求 http://ouo.io/api/<key>?s=<encoded>
	if mode == "api" {
		ouoShortCacheMu.RLock()
		cached, ok := ouoShortCache[proxyVideoURL]
		ouoShortCacheMu.RUnlock()
		if ok && cached != "" {
			return cached
		}

		apiURL := "http://ouo.io/api/" + url.PathEscape(key) + "?s=" + url.QueryEscape(proxyVideoURL)
		resp, err := ouoHTTPClient.Get(apiURL)
		if err == nil {
			body, rErr := io.ReadAll(io.LimitReader(resp.Body, 512))
			resp.Body.Close()
			if rErr == nil && resp.StatusCode == http.StatusOK {
				shortURL := strings.TrimSpace(string(body))
				if strings.HasPrefix(shortURL, "http://ouo.io/") || strings.HasPrefix(shortURL, "https://ouo.io/") {
					saveOuoShortCache(proxyVideoURL, shortURL)
					return shortURL
				}
			}
		}
	}

	// Quick Link 广告中转页（浏览器访问该地址会直接进入 ouo.io 广告倒计时页面，完成后跳转到 proxyVideoURL）
	return "http://ouo.io/qs/" + url.PathEscape(key) + "?s=" + url.QueryEscape(proxyVideoURL)
}

func saveOuoShortCache(proxyVideoURL, shortURL string) {
	ouoShortCacheMu.Lock()
	defer ouoShortCacheMu.Unlock()
	if _, exists := ouoShortCache[proxyVideoURL]; !exists {
		if len(ouoShortKeys) >= 500 {
			oldest := ouoShortKeys[0]
			ouoShortKeys = ouoShortKeys[1:]
			delete(ouoShortCache, oldest)
		}
		ouoShortKeys = append(ouoShortKeys, proxyVideoURL)
	}
	ouoShortCache[proxyVideoURL] = shortURL
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
//   - 每 IP 每日前 N 个请求（N=limit.VideoDirectQuota()，默认 10，env VIDEO_DIRECT_QUOTA 可调）：
//     直接 302 到真实链接 https://twimg.moonchan.xyz/...，不经 ouo.io。
//   - 超出每日免费额度后：302 到 ouo.io 广告中转页（BuildOuoRedirectTarget），与既有变现路径一致。
//
// 「每日」由进程每日重启清空内存实现（见 limit.DailyRestartQuota 注释），
// 不记录日期。取不到真实客户端 IP 时 Claim 返回 false，保守走 ouo.io。
func HandleVideoRedirectV2(c *gin.Context) {
	pathParam := c.Param("filepath")
	proxyVideoURL, ok := ExtractAndBuildProxyVideoURL(c.Request, pathParam)
	if !ok || proxyVideoURL == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "missing video path or url parameter",
		})
		return
	}

	// 严禁缓存 302 跳转响应（无论直跳还是经 ouo.io），确保每次点击都重新判定额度/触发计费。
	c.Header("Cache-Control", "no-cache, no-store, private, must-revalidate")

	// 每 IP 每日前 N 个请求直跳真实链接（twimg.moonchan.xyz），不经过 ouo.io。
	// ipban.Principal 取真实客户端 IP（CF-Connecting-IP → XFF 右数第 N → RemoteAddr），
	// 与封禁/限流同一口径；解析失败时 Claim 返回 false → 保守走 ouo.io。
	if limit.VideoDirectQuota().Claim(ipban.Principal(c.Request)) {
		c.Redirect(http.StatusFound, proxyVideoURL)
		return
	}

	ouoRedirectURL := BuildOuoRedirectTarget(proxyVideoURL)
	c.Redirect(http.StatusFound, ouoRedirectURL)
}
