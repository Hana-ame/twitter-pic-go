// Package twimg 提供配套的 Twitter 媒体反向代理服务（迁移自 api-pack/internal/proxy）。
//
// 核心架构（TwimgProxyV2，部署于 pbs.moonchan.xyz）：
//   - 图片与视频封面（.jpg / .png / format=jpg 等）：由本服务通过 StreamProxy 直接反代
//     https://pbs.twimg.com（自带 TCP 背压与客户端断开级联取消），配合 Cloudflare 边缘缓存提供高命中率图片加速。
//   - 视频文件（.mp4）：绝不经过本机 VPS 转发视频流字节（防止透过 CF 的视频请求占满 VPS 出站带宽），
//     一律返回 302 Found（禁缓存）重定向至外部 Cloudflare Worker（https://twimg.moonchan.xyz，
//     2026-10-23 起广告跳转已关闭，不再支持 TWIMG_OUO_PREFIX 包装广告链接）。
package twimg

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

// Run 根据环境变量启动配套的 TwimgProxyV2 服务（供 server/main.go 异步调用）。
// 若 addr 为空，则依次回退读取 TWIMG_V2 与 TWIMG 环境变量；均为空则不启动。
func Run(addr string) {
	if strings.TrimSpace(addr) == "" {
		addr = strings.TrimSpace(os.Getenv("TWIMG_V2"))
	}
	if strings.TrimSpace(addr) == "" {
		addr = strings.TrimSpace(os.Getenv("TWIMG"))
	}
	if addr == "" {
		return
	}
	log.Printf("[twimg-v2] starting companion proxy on %s (images -> pbs.twimg.com, .mp4 -> 302 redirect)", addr)
	if err := TwimgProxyV2(addr); err != nil {
		log.Printf("[twimg-v2] exited: %v", err)
	}
}

// corsMiddleware 为反代响应附加跨域与 Cross-Origin-Resource-Policy 头（对齐 api-pack CORSMiddleware）。
func corsMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			origin = "*"
		}
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH, HEAD")
		if reqHeaders := c.GetHeader("Access-Control-Request-Headers"); reqHeaders != "" {
			c.Header("Access-Control-Allow-Headers", reqHeaders)
		} else {
			c.Header("Access-Control-Allow-Headers", "Accept, Authorization, Range, Cache-Control, X-Requested-With")
		}
		c.Header("Access-Control-Allow-Credentials", "true")
		c.Header("Access-Control-Expose-Headers", "*")
		c.Header("Cross-Origin-Resource-Policy", "cross-origin")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

// StreamProxy 创建一个原生的流式反向代理：
//  1. 实现 TCP 背压：当 Client 接收慢时，Write 阻塞导致 Read 阻塞，迫使上游 Fetch 自动降速。
//  2. 实现级联取消：客户端断开时自动掐断发往目标服务器的 TCP 请求。
//  3. 使用 Go 1.20+ 推荐的 Rewrite 替代被弃用的 Director，提升安全性。
func StreamProxy(targetURL string, headerProcesser func(http.Header) http.Header) gin.HandlerFunc {
	target, err := url.Parse(targetURL)
	if err != nil {
		panic(err)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = target.Host
			if headerProcesser != nil {
				pr.Out.Header = headerProcesser(pr.Out.Header)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				log.Printf("[twimg-proxy] client disconnected: %v", err)
				return
			}
			log.Printf("[twimg-proxy] proxy error: %v", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}

	return func(c *gin.Context) {
		select {
		case <-c.Request.Context().Done():
			c.AbortWithStatus(499)
			return
		default:
		}
		proxy.ServeHTTP(c.Writer, c.Request)
	}
}

// TwimgProxy 旧版 twimg 反代（保留自 api-pack 兼容调用）。
func TwimgProxy(addr string) error {
	if addr == "" {
		return fmt.Errorf("addr is empty")
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(corsMiddleware())

	headerProcesser := func(h http.Header) http.Header {
		h.Set("Referer", "https://x.com")
		return h
	}

	twimgTarget, _ := url.Parse("https://pbs.twimg.com")
	twimgProxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(twimgTarget)
			pr.Out.Host = twimgTarget.Host
			if headerProcesser != nil {
				pr.Out.Header = headerProcesser(pr.Out.Header)
			}
		},
		Transport: &http.Transport{
			MaxIdleConns:          100,
			MaxConnsPerHost:       200,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 20 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				log.Printf("[twimg-proxy] client disconnected: %v", err)
				return
			}
			log.Printf("[twimg-proxy] proxy error: %v", err)
			http.Error(w, "Bad Gateway", http.StatusBadGateway)
		},
	}

	videoProxy := StreamVideoProxy("https://video.twimg.com", headerProcesser)

	handler := func(c *gin.Context) {
		path := c.Request.URL.Path
		country := c.GetHeader("Cf-Ipcountry")
		var host string
		var isVideo bool

		if strings.HasPrefix(path, "/tweet_video/") || strings.HasPrefix(path, "/ext_tw_video/") || strings.HasPrefix(path, "/amplify_video/") {
			host = "video.twimg.com"
			isVideo = true
		} else {
			host = "pbs.twimg.com"
		}

		if country != "" && country != "CN" {
			c.Header("Cache-Control", "no-cache, no-store, private")
			c.Redirect(http.StatusFound, "https://"+host+c.Request.URL.String())
			return
		}

		if isVideo {
			videoProxy(c)
		} else {
			twimgProxy.ServeHTTP(c.Writer, c.Request)
		}
	}

	r.GET("/*any", handler)
	r.HEAD("/*any", handler)

	return r.Run(addr)
}

// ---- TwimgProxyV2：图片本体反代 + 视频 302 外部卸载（彻底解决 VPS 视频流量占满问题） ----

const (
	redirectSourceLimit   = 140_000
	defaultDivertQPS      = 10
	defaultMaxDivertRatio = 0.8
	divertBlockStartHour  = 20
	defaultConfigPath     = "twimg_v2.json"
	defaultVideoIPQuotaGB = 10
)

type configSource struct {
	Domain  string `json:"domain"`
	Limit   int64  `json:"limit"`
	Enabled bool   `json:"enabled"`
}

type twimgV2Config struct {
	QPSThreshold   *int           `json:"qps_threshold"`
	MaxDivertRatio *float64       `json:"max_divert_ratio"`
	VideoIPQuotaGB *float64       `json:"video_ip_quota_gb"`
	Sources        []configSource `json:"sources"`
}

type redirectSource struct {
	domain  string
	limit   atomic.Int64
	enabled atomic.Bool
	count   atomic.Int64
}

func (s *redirectSource) disabled() bool {
	return !s.enabled.Load() || s.count.Load() >= s.limit.Load()
}

func (s *redirectSource) hit() {
	n := s.count.Add(1)
	if n == s.limit.Load() {
		log.Printf("[twimg-v2] source %s 跳转次数达到 %d，已禁用", s.domain, n)
	}
}

func divertWindowAllows(now time.Time) bool {
	return now.UTC().Hour() < divertBlockStartHour
}

type divertManager struct {
	sources        []*redirectSource
	qpsThreshold   int
	divertPermille int
	tick           atomic.Int64
	videoGate      *videoGate
}

func newDivertManager(path string) *divertManager {
	m := &divertManager{
		qpsThreshold:   defaultDivertQPS,
		divertPermille: int(defaultMaxDivertRatio * 1000),
	}

	var cfg twimgV2Config
	data, err := os.ReadFile(path)
	switch {
	case err != nil:
		log.Printf("[twimg-v2] 读取分流配置 %s 失败: %v，使用内置默认节点", path, err)
		cfg.Sources = []configSource{
			{Domain: "twimg.810114.xyz", Limit: redirectSourceLimit, Enabled: true},
			{Domain: "114514.beer", Limit: redirectSourceLimit, Enabled: true},
		}
	case json.Unmarshal(data, &cfg) != nil:
		log.Printf("[twimg-v2] 解析分流配置 %s 失败: %v，使用内置默认节点", path, err)
		cfg.Sources = []configSource{
			{Domain: "twimg.810114.xyz", Limit: redirectSourceLimit, Enabled: true},
			{Domain: "114514.beer", Limit: redirectSourceLimit, Enabled: true},
		}
	}
	if cfg.QPSThreshold != nil {
		m.qpsThreshold = *cfg.QPSThreshold
	}
	if cfg.MaxDivertRatio != nil {
		ratio := *cfg.MaxDivertRatio
		if ratio < 0 {
			ratio = 0
		}
		if ratio > 1 {
			ratio = 1
		}
		m.divertPermille = int(ratio * 1000)
	}
	quotaGB := float64(defaultVideoIPQuotaGB)
	if cfg.VideoIPQuotaGB != nil && *cfg.VideoIPQuotaGB >= 0 {
		quotaGB = *cfg.VideoIPQuotaGB
	}
	if quotaGB > 0 {
		m.videoGate = &videoGate{
			perIPQuota: int64(quotaGB * 1024 * 1024 * 1024),
			perIP:      make(map[string]int64),
			pool:       make(map[string]struct{}),
			poolSlots:  make(chan struct{}, 1),
		}
	}
	for _, cs := range cfg.Sources {
		if cs.Domain == "" {
			continue
		}
		limit := cs.Limit
		if limit <= 0 {
			limit = redirectSourceLimit
		}
		s := &redirectSource{domain: cs.Domain}
		s.limit.Store(limit)
		s.enabled.Store(cs.Enabled)
		m.sources = append(m.sources, s)
	}
	return m
}

func (m *divertManager) resetCounters() {
	for _, s := range m.sources {
		s.count.Store(0)
	}
	log.Printf("[twimg-v2] 分流计数已全部重置")
}

func (m *divertManager) resetLoop() {
	for {
		now := time.Now().UTC()
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		time.Sleep(time.Until(next))
		m.resetCounters()
	}
}

func (m *divertManager) divertPermilleFor(currentQPS int) int {
	if currentQPS <= m.qpsThreshold {
		return 0
	}
	p := (currentQPS - m.qpsThreshold) * 1000 / currentQPS
	if p > m.divertPermille {
		p = m.divertPermille
	}
	return p
}

func (m *divertManager) shouldDivert(c *gin.Context, currentQPS int) bool {
	if !divertWindowAllows(time.Now()) {
		return false
	}

	p := m.divertPermilleFor(currentQPS)
	if p <= 0 {
		return false
	}

	n := m.tick.Add(1)
	if (n*int64(p))%1000 >= int64(p) {
		return false
	}

	var chosen *redirectSource
	for _, s := range m.sources {
		if !s.disabled() {
			chosen = s
			break
		}
	}
	if chosen == nil {
		return false
	}

	chosen.hit()
	c.Header("Cache-Control", "no-cache, no-store, private")
	c.Redirect(http.StatusFound, "https://"+chosen.domain+c.Request.URL.String())
	return true
}

type qpsTracker struct {
	mu     sync.Mutex
	window time.Duration
	times  []time.Time
}

func newQPSTracker(window time.Duration) *qpsTracker {
	return &qpsTracker{window: window}
}

func (t *qpsTracker) record() int {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := now.Add(-t.window)
	keep := t.times[:0]
	for _, ts := range t.times {
		if ts.After(cutoff) {
			keep = append(keep, ts)
		}
	}
	t.times = append(keep, now)
	return len(t.times)
}

// TwimgProxyV2 twimg 反代（部署于 pbs.moonchan.xyz）：
//   - 仅扩展名为 .mp4 的请求（视频）302 重定向到外部 Worker（https://twimg.moonchan.xyz，
//     保留 path+query，禁缓存；2026-10-23 起广告跳转已关闭，不再支持 TWIMG_OUO_PREFIX 包装）
//   - 其余请求（.jpg / 视频 cover 封面图 / 头像 / 无扩展名 / query 形如 format=jpg 的图片）
//     一律由本体直接反代 pbs.twimg.com，不做任何 302，也不看 Cf-Ipcountry
//   - StreamProxy 基于 request context，客户端断开会级联取消上游请求，避免流量空跑
func TwimgProxyV2(addr string) error {
	if addr == "" {
		addr = "127.26.8.10:8080"
	}
	r := gin.New()
	r.Use(gin.Recovery())
	r.Use(corsMiddleware())

	headerProcesser := func(h http.Header) http.Header {
		h.Set("Referer", "https://x.com")
		return h
	}
	imageProxy := StreamProxy("https://pbs.twimg.com", headerProcesser)

	videoRedirectBase := strings.TrimRight(strings.TrimSpace(os.Getenv("TWIMG_VIDEO_REDIRECT")), "/")
	if videoRedirectBase == "" {
		videoRedirectBase = "https://twimg.moonchan.xyz"
	}

	v2Handler := func(c *gin.Context) {
		if strings.HasSuffix(strings.ToLower(c.Request.URL.Path), ".mp4") {
			c.Header("Cache-Control", "no-cache, no-store, private")
			target := videoRedirectBase + c.Request.URL.String()
			c.Redirect(http.StatusFound, target)
			return
		}
		imageProxy(c)
	}
	r.GET("/*any", v2Handler)
	r.HEAD("/*any", v2Handler)

	lc := net.ListenConfig{KeepAlive: 30 * time.Second}
	ln, err := lc.Listen(context.Background(), "tcp", addr)
	if err != nil {
		return err
	}
	return (&http.Server{
		Handler:      r,
		IdleTimeout:  120 * time.Second,
		WriteTimeout: 30 * time.Minute,
	}).Serve(ln)
}
