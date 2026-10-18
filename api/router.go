// Package api 实现 Twitter Pic 的后端 REST API（含 V1 传统接口与 V2 轻量动态切片接口），
// 并提供 SetupRouter 将 API 与 Gallery SSR 挂载在同一个 HTTP 地址与同一个 SQLite 连接池上运行。
package api

import (
	"database/sql"
	"net/http"
	"strings"

	"github.com/Hana-ame/twitter-pic-go/Tools/ginkit/middleware"
	"github.com/Hana-ame/twitter-pic-go/gallery"
	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/gin-gonic/gin"
)

func init() {
	// 当 Gallery 侧或 V1 侧写入标签或封禁用户时，同步清空 V2 首页 Feed 缓存。
	// （通过 tags.OnDataChange 订阅，保持单端口下 API 与 Gallery 缓存实时一致）
	registerFeedInvalidationHook()
}

// SetupRouter 构建统一的 Gin 路由引擎：在同一个监听地址（端口）与同一个 SQLite 数据库连接池上
// 同时服务后端 REST API（/api/twitter/*、/api/v2/*、/api/flutter/*）与 Gallery SSR 前端图站。
func SetupRouter(db *sql.DB) *gin.Engine {
	if db != nil {
		DB = db
	}

	r := gin.Default()
	r.Use(middleware.CORS())

	// 1. V1 API 路由组（/api/twitter/*，内含 /api/twitter/v2/*）
	apiGroup := r.Group("/api/twitter")
	AddToGroup(apiGroup)

	// 2. 面向 Flutter 与现代客户端的根路径 V2 别名：支持 /api/v2/... 与 /api/flutter/...
	RegisterV2Routes(r.Group("/api/v2"))
	RegisterV2Routes(r.Group("/api/flutter"))

	ipban.LogEffectiveConfig()

	// 3. 初始化 Gallery SSR 处理器，与 API 共享同一个 *sql.DB 连接池与同一份内存缓存钩子
	galleryHandler := gallery.NewHandler(DB)

	// 4. 除 /api/twitter、/api/v2、/api/flutter 专属前缀外的全部路由交由 Gallery SSR 处理器接管
	//    （包括 /、/u/:account、/static/*、/healthz、/api/reactions、/api/react、/api/tag、/api/tags 等）
	r.NoRoute(func(c *gin.Context) {
		p := c.Request.URL.Path
		if strings.HasPrefix(p, "/api/twitter/") || strings.HasPrefix(p, "/api/v2/") || strings.HasPrefix(p, "/api/flutter/") {
			c.JSON(http.StatusNotFound, gin.H{"error": "api endpoint not found"})
			return
		}
		c.Status(http.StatusOK)
		galleryHandler.ServeHTTP(c.Writer, c.Request)
	})

	return r
}
