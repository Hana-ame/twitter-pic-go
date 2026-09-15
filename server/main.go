package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Hana-ame/twitter-pic-go"
	"github.com/Hana-ame/twitter-pic-go/Tools/ginkit/middleware"
	"github.com/Hana-ame/twitter-pic-go/Tools/sqlite"
	"github.com/Hana-ame/twitter-pic-go/gallery"
	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/Hana-ame/twitter-pic-go/twimg"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	_ "github.com/joho/godotenv/autoload"
)

func main() {
	godotenv.Load(".env")

	// DB 与表结构先就位：gallery 与 twitter API 共用同一个 twitter.db
	//（account_tags + request_logs 是标签唯一真源），监听器不能跑在建表之前。
	//
	// busy_timeout 必须有：gallery 现在也写这个库（账号标签投票），
	// 本进程不再是唯一写入者。没有 busy_timeout 时两侧并发写会直接抛
	// SQLITE_BUSY，把「两层同一真源」变成「谁抢到谁写」。
	var err error
	// _txlock=immediate：见 tags/votes.go 的 CastVotes——写事务必须在第一条语句前
	// 拿到写锁，否则两个 IP 并发改同一标签行会丢票。gallery 侧同一个参数。
	twitter.DB, err = sqlite.NewSQLiteDB("./twitter.db?parseTime=true&_loc=UTC&_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		fmt.Println(err)
		return
	}
	err = twitter.CreateTableV3()
	if err != nil {
		log.Println(err)
	}

	go twimg.Run(os.Getenv("TWIMG_ADDR"))

	// gallery 作为独立包运行在同一二进制内，单独监听 GALLERY_ADDR（默认 :8090）
	go gallery.Run(os.Getenv("GALLERY_ADDR"))

	err = twitter.RefreshAllRankings()
	if err != nil {
		log.Println(err)
	}

	r := gin.Default()
	r.Use(middleware.CORS())

	api := r.Group("/api/twitter")

	twitter.AddToGroup(api)

	// 打印实际生效的 IP 口径（CF 头优先与否 / TRUSTED_PROXY_HOPS / 封禁表加载条数）。
	// AddToGroup 里已经初始化过 ipban.Shared()，这里的 Count 才是真值。
	// 目的是「配错了要能看见」：跳数配错只会让限流和归属静默失效，不留日志就是假绿。
	ipban.LogEffectiveConfig()

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

		// 防止路径遍历攻击：用 filepath.Rel 判断 fullPath 是否真的在 staticRoot 之下。
		// HasPrefix 不行：staticRoot=/var/www 时 /../wwwfoo 清成 /var/wwwfoo，
		// HasPrefix 误判为 true，兄弟目录文件可读。
		rel, err := filepath.Rel(staticRoot, fullPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
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

	// 用显式 http.Server 代替 r.Run：gin 的 r.Run 无法配置超时。
	// addr 为空时 gin 的 r.Run 会绑 ":80"（gin 的 net.ListenAddr 把空串归一化
	// 成 ":80"），而 http.Server{Addr:""} 会绑随机端口 —— 语义不同，这里显式对齐。
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":80"
	}
	// 同 gallery / twimg：只设握手期与 keep-alive 的超时，**不设**
	// ReadTimeout / WriteTimeout。这个服务同时出 /api/twitter 和静态文件
	//（含媒体下载），长连接是正常业务，WriteTimeout 会把媒体流掐断。
	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20,
		IdleTimeout:       60 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}
