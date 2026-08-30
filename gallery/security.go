package gallery

// gallery 的写接口鉴权与限流。
//
// 主 API（Gin 侧）已经有 limit.RateLimitMiddleware / banManager，
// 但 gallery 是独立的 net/http 服务，此前完全没有限流，
// POST /rescan 也能匿名触发全量索引重建。这里补上这两块。

import (
	"crypto/subtle"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// defaultRate 每秒请求数；defaultBurst 突发额度。
// 图库是浏览型站点，正常翻页/开图不会触及；能挡住脚本刷 /rescan、/react。
const (
	defaultRate  = 20.0
	defaultBurst = 60
	// maxTrackedIPs 是 IP 表上限，防止被扫描时内存无限增长。
	maxTrackedIPs = 20000
)

// securityPolicy 持有 gallery 的限流器与写接口密钥。
type securityPolicy struct {
	lim *rateLimiter
	key string
}

// loadSecurityPolicy 读取配置。密钥优先 GALLERY_ADMIN_KEY，回退到主 API 的
// DELETE_KEY，两者都为空时写接口保持匿名可用（并打印告警）。
func loadSecurityPolicy() *securityPolicy {
	key := strings.TrimSpace(os.Getenv("GALLERY_ADMIN_KEY"))
	if key == "" {
		key = strings.TrimSpace(os.Getenv("DELETE_KEY"))
	}

	rps := defaultRate
	if v := strings.TrimSpace(os.Getenv("GALLERY_RATE_LIMIT")); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			rps = f
		}
	}

	if key == "" {
		log.Printf("gallery: 警告：未设置 GALLERY_ADMIN_KEY / DELETE_KEY，" +
			"POST /rescan 与 /api/link 仍是匿名可写")
	}

	return &securityPolicy{
		lim: newRateLimiter(rps, defaultBurst),
		key: key,
	}
}

// rateLimit 包装限流中间件。/api/health 是探活用，不计入限流。
func (p *securityPolicy) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			next.ServeHTTP(w, r)
			return
		}
		if !p.lim.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireKey 保护写接口。密钥可通过 ?key= 或 X-Admin-Key 头传递。
// 未配置密钥时直接放行（保持旧的匿名行为，避免部署时被打断）。
func (p *securityPolicy) requireKey(w http.ResponseWriter, r *http.Request) bool {
	if p.key == "" {
		return true
	}
	got := strings.TrimSpace(r.Header.Get("X-Admin-Key"))
	if got == "" {
		got = strings.TrimSpace(r.URL.Query().Get("key"))
	}
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(p.key)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	return true
}

// keyHandler 把 requireKey 套在某个 handler 外面，便于注册路由时一行搞定。
func (p *securityPolicy) keyHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !p.requireKey(w, r) {
			return
		}
		h(w, r)
	}
}

// rateLimiter 是按 IP 的令牌桶限流器。
// 只用每 IP 限流，不加全局闸：图库是公开站点，
// 全局额度会把正常并发浏览一起压掉。
type rateLimiter struct {
	mu     sync.Mutex
	rps    rate.Limit
	burst  int
	perIP  map[string]*rateLimiterEntry
	maxIPs int
}

type rateLimiterEntry struct {
	lim  *rate.Limiter
	last time.Time
}

func newRateLimiter(rps float64, burst int) *rateLimiter {
	return &rateLimiter{
		rps:    rate.Limit(rps),
		burst:  burst,
		perIP:  make(map[string]*rateLimiterEntry),
		maxIPs: maxTrackedIPs,
	}
}

// allow 判断给定 IP 的请求是否放行。
func (l *rateLimiter) allow(ip string) bool {
	if ip == "" {
		ip = "?"
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	e, ok := l.perIP[ip]
	if !ok {
		// 表满时挑一个最久未活跃的踢掉，避免 map 无限增长。
		if len(l.perIP) >= l.maxIPs {
			l.evictOldest()
		}
		e = &rateLimiterEntry{lim: rate.NewLimiter(l.rps, l.burst)}
		l.perIP[ip] = e
	}
	e.last = now
	return e.lim.Allow()
}

func (l *rateLimiter) evictOldest() {
	var oldest string
	var oldestTime time.Time
	for ip, e := range l.perIP {
		if oldest == "" || e.last.Before(oldestTime) {
			oldest = ip
			oldestTime = e.last
		}
	}
	delete(l.perIP, oldest)
}

// clientIP 取客户端 IP。尊重 X-Forwarded-For 的第一段（gallery 前面有反代），
// 解析失败时回退到 RemoteAddr 的主机部分。
func clientIP(r *http.Request) string {
	if xf := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xf != "" {
		if first := strings.TrimSpace(strings.Split(xf, ",")[0]); first != "" {
			return first
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
