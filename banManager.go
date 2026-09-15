package twitter

import (
	"net/http"

	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/gin-gonic/gin"
)

// StrictIPBanMiddleware 是封禁判定在 gin 侧的薄包装：判定规则、IP 口径、响应体
// 全部来自 ipban 包，gallery 侧用同一个 Manager 的 ipban.Middleware。
// 这里**不再有任何一份自己的循环或解析**——原先这个文件里同时长着 trie 实现、
// 文件解析、两条被注释掉的中间件草稿，两层要一致只能靠人肉对齐。
//
// 用法见 twitter_handlers.go 的 AddToGroup（走 ipban.Shared() 单例）。
func StrictIPBanMiddleware(m *ipban.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		if ip, banned := m.Decide(c.Request); banned {
			c.AbortWithStatusJSON(http.StatusForbidden, ipban.Denied{
				Error: "Access Denied", Reason: "Banned IP detected in chain", IP: ip,
			})
			return
		}
		c.Next()
	}
}
