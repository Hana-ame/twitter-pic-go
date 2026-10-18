package ginkit

import (
	"github.com/gin-gonic/gin"
)

// AbortWithError 在 err != nil 时中断 Gin 请求并返回统一格式的 JSON 错误响应 {"error": err.Error()}。
func AbortWithError(c *gin.Context, code int, err error) bool {
	if err == nil {
		return false
	}
	c.AbortWithStatusJSON(code, gin.H{
		"error": err.Error(),
	})
	return true
}
