package limit

import (
	"net/http"

	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/gin-gonic/gin"
)

// RateLimitMiddleware 包装 FastLimiter 为 Gin 中间件。
//
// 分桶键用 ipban.Principal 而不是 gin 的 c.ClientIP()：gin 侧从未调用
// SetTrustedProxies（默认信任所有代理），ClientIP 直接吃客户端自带的
// X-Forwarded-For 首项——伪造一个头就能无限换桶，25/IP/h 等于没配。
// Principal 按可信跳数从右往左取，与 gallery 侧、与 request_logs 记的 IP 同一口径。
func RateLimitMiddleware(l *FastLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 取「这个请求是谁」（统一口径，见 ipban.Principal）
		ip := ipban.Principal(c.Request)

		// 2. 检查是否允许访问
		if !l.Allow(ip) {
			// 3. 如果不允许，返回 429 状态码并中断后续逻辑
			c.JSON(http.StatusTooManyRequests, gin.H{
				"code":    429,
				"message": "请求过于频繁，请一小时后再试",
			})
			c.Abort() // 必须调用 Abort，否则会执行后续的 Handler
			return
		}

		// 4. 允许访问，继续执行下一个中间件或 Handler
		c.Next()
	}
}
