package main

import (
	"os"

	"github.com/Hana-ame/twitter-pic-go"
	"github.com/Hana-ame/twitter-pic-go/Tools/ginkit/middleware"
	"github.com/Hana-ame/twitter-pic-go/twimg"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	_ "github.com/joho/godotenv/autoload"
)

func main() {
	godotenv.Load(".env")

	go twimg.Run(os.Getenv("TWIMG_ADDR"))

	r := gin.Default()
	r.Use(middleware.CORS())

	api := r.Group("/api/twitter")

	twitter.AddToGroup(api)

	r.NoRoute(func(c *gin.Context) {
		staticRoot := os.Getenv("STATIC_ROOT")
		// 获取请求的文件路径
		path := c.Request.URL.Path

		// 检查本地是否存在该文件
		_, err := os.Stat(staticRoot + path)
		if err != nil {
			// 如果文件不存在，直接返回 index.html (前端路由常用)
			c.File(staticRoot + "/index.html")
			return
		}
		// 如果文件存在，则提供该文件
		c.File(staticRoot + path)
	})

	r.Run(os.Getenv("LISTEN_ADDR"))
}
