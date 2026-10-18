package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// CORS 提供跨域资源共享（CORS）Gin 中间件，允许前端、移动端及跨域调试工具访问 API。
func CORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")
		if origin == "" {
			origin = "*"
		}
		c.Header("Access-Control-Allow-Origin", origin)
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, HEAD, OPTIONS")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Content-Length, Accept, Accept-Encoding, Authorization, X-Requested-With, If-Modified-Since, Cache-Control")
		c.Header("Access-Control-Expose-Headers", "Content-Length, Content-Encoding, Last-Modified, Cache-Control")
		c.Header("Access-Control-Allow-Credentials", "true")

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
