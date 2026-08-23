package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Hana-ame/twitter-pic-go"
	"github.com/Hana-ame/twitter-pic-go/Tools/ginkit/middleware"
	"github.com/Hana-ame/twitter-pic-go/Tools/sqlite"
	"github.com/Hana-ame/twitter-pic-go/gallery"
	"github.com/Hana-ame/twitter-pic-go/twimg"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	_ "github.com/joho/godotenv/autoload"
)

func main() {
	godotenv.Load(".env")

	go twimg.Run(os.Getenv("TWIMG_ADDR"))

	// gallery 作为独立包运行在同一二进制内，单独监听 GALLERY_ADDR（默认 :8090）
	go gallery.Run(os.Getenv("GALLERY_ADDR"))

	var err error
	twitter.DB, err = sqlite.NewSQLiteDB("./twitter.db?parseTime=true&_loc=UTC")
	if err != nil {
		fmt.Println(err)
		return
	}
	err = twitter.CreateTableV3()
	if err != nil {
		log.Println(err)
	}
	err = twitter.RefreshAllRankings()
	if err != nil {
		log.Println(err)
	}

	r := gin.Default()
	r.Use(middleware.CORS())

	api := r.Group("/api/twitter")

	twitter.AddToGroup(api)

	r.NoRoute(func(c *gin.Context) {
		staticRoot := os.Getenv("STATIC_ROOT")
		if staticRoot == "" {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}

		// 获取请求路径，并清理和校验
		path := c.Request.URL.Path
		// 移除前导斜杠，得到相对路径
		relPath := strings.TrimPrefix(path, "/")
		// 安全拼接完整路径
		fullPath := filepath.Join(staticRoot, relPath)
		// 清理路径（去除多余斜杠、.. 等）
		fullPath = filepath.Clean(fullPath)

		// 防止路径遍历攻击：确保最终路径仍然在 staticRoot 之下
		if !strings.HasPrefix(fullPath, staticRoot) {
			c.AbortWithStatus(http.StatusForbidden)
			return
		}

		// 获取文件信息
		info, err := os.Stat(fullPath)
		if err != nil {
			// 如果文件不存在，返回 index.html（前端路由）
			if os.IsNotExist(err) {
				c.File(filepath.Join(staticRoot, "index.html"))
				return
			}
			// 其他错误（如权限）返回 500
			c.AbortWithError(http.StatusInternalServerError, err)
			return
		}

		// 如果是目录，也返回 index.html（可根据需求调整）
		if info.IsDir() {
			c.File(filepath.Join(staticRoot, "index.html"))
			return
		}

		// 正常提供文件
		c.File(fullPath)
	})

	r.Run(os.Getenv("LISTEN_ADDR"))
}
