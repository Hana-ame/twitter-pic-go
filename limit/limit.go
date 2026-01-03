package limit

import (
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// IPRateLimiter 定义一个结构体用来存储每个IP的限流器
type IPRateLimiter struct {
	ips map[string]*rate.Limiter
	mu  *sync.RWMutex
	r   rate.Limit // 每秒产生的令牌数 (TPS)
	b   int        // 桶的大小 (突发并发数)
}

// NewIPRateLimiter 创建一个新的限流器管理器
func NewIPRateLimiter(r rate.Limit, b int) *IPRateLimiter {
	i := &IPRateLimiter{
		ips: make(map[string]*rate.Limiter),
		mu:  &sync.RWMutex{},
		r:   r,
		b:   b,
	}

	// 启动一个协程定期清理过期的IP（防止内存泄漏）
	// 这里为了演示简单，简单地每分钟清空一次，生产环境建议记录最后访问时间来清理
	go func() {
		for {
			time.Sleep(12 * time.Minute)
			i.mu.Lock()
			i.ips = make(map[string]*rate.Limiter)
			i.mu.Unlock()
		}
	}()

	return i
}

// GetLimiter 根据IP获取对应的限流器
func (i *IPRateLimiter) GetLimiter(ip string) *rate.Limiter {
	i.mu.Lock()
	defer i.mu.Unlock()

	limiter, exists := i.ips[ip]
	if !exists {
		limiter = rate.NewLimiter(i.r, i.b)
		i.ips[ip] = limiter
	}

	return limiter
}

// RateLimitMiddleware Gin中间件核心逻辑
func RateLimitMiddleware(limit *IPRateLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := c.ClientIP()
		limiter := limit.GetLimiter(ip)

		// Allow() 方法会消耗一个令牌，如果成功返回 true
		if !limiter.Allow() {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
				"code":    429,
				"message": "请求太频繁，请稍后再试",
			})
			return
		}

		c.Next()
	}
}
