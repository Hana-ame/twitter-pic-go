package limit

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// RateLimitMiddleware 包装 FastLimiter 为 Gin 中间件
func RateLimitMiddleware(l *FastLimiter) gin.HandlerFunc {
	return func(c *gin.Context) {
		// 1. 获取客户端 IP (Gin 会自动处理 X-Forwarded-For 等头部)
		ip := c.ClientIP()

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
