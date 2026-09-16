package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
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

	err = twitter.RefreshAllRankings()
	if err != nil {
		log.Println(err)
	}

	r := setupRouter(twitter.DB)

	// 用显式 http.Server 代替 r.Run：统一运行在 8080 端口（优先 LISTEN_ADDR，其次 GALLERY_ADDR，默认 :8080）
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = os.Getenv("GALLERY_ADDR")
	}
	if addr == "" {
		addr = ":8080"
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20,
		IdleTimeout:       60 * time.Second,
	}
	log.Printf("server: serving on %s (api on /api/twitter, gallery SSR occupies the rest)", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func setupRouter(db *sql.DB) *gin.Engine {
	r := gin.Default()
	r.Use(middleware.CORS())

	// API 专属路径给原来的 twitter API
	api := r.Group("/api/twitter")
	twitter.AddToGroup(api)

	ipban.LogEffectiveConfig()

	// 初始化 gallery SSR 处理引擎，与 API 共享同一 DB 连接池
	galleryHandler := gallery.NewHandler(db)

	// API 相关 path 以外的全部路由，全量交由 Gallery 的 SSR 占据
	r.NoRoute(func(c *gin.Context) {
		c.Status(http.StatusOK)
		galleryHandler.ServeHTTP(c.Writer, c.Request)
	})

	return r
}
