package main

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/Hana-ame/twitter-pic-go/Tools/sqlite"
	"github.com/Hana-ame/twitter-pic-go/api"
	"github.com/Hana-ame/twitter-pic-go/twimg"
	"github.com/gin-gonic/gin"
	"github.com/joho/godotenv"
	_ "github.com/joho/godotenv/autoload"
)

func main() {
	_ = godotenv.Load(".env")

	// 内存上限：本机 528MB / 1 vCPU，给其他服务与内核留出余量，
	// 把 Go 堆钉在 ~240MB，避免流量峰值时堆无限制膨胀吃光整机内存
	// （历史事故：内存耗尽 → 内核 page allocation failure → CPU 100%）。
	// GOMEMLIMIT 环境变量由 Go runtime 启动时自动读取；只有未设置时才补默认值。
	// （注意：不能调 debug.SetMemoryLimit(-1)，那会禁用限制并覆盖环境变量。）
	const memLimit = 240 << 20 // 240 MiB
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memLimit)
	}

	// DB 与表结构先就位：API 与 Gallery 共用同一个 twitter.db 连接池
	// （account_tags + user_tag_cnt + tag_votes + request_logs 是唯一真源）。
	// busy_timeout(5000) + WAL + _txlock=immediate 保证并发写安全与「一 IP 一票」事务串行。
	dbPath := strings.TrimSpace(os.Getenv("DB_PATH"))
	if dbPath == "" {
		dbPath = strings.TrimSpace(os.Getenv("GALLERY_DB"))
	}
	if dbPath == "" {
		dbPath = "./twitter.db"
	}

	dsn := dbPath + "?parseTime=true&_loc=UTC&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	var err error
	api.DB, err = sqlite.NewSQLiteDB(dsn)
	if err != nil {
		fmt.Println(err)
		return
	}

	if err := api.CreateTableV3(); err != nil {
		log.Println(err)
	}

	go twimg.Run(os.Getenv("TWIMG_ADDR"))

	// CreateTableV3 启动时已完成首次榜单结算；此处启动后台定时协程每小时自动刷新日/周/月榜
	go func() {
		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()
		for range ticker.C {
			if err := api.RefreshAllRankings(); err != nil {
				log.Printf("RefreshAllRankings 定时刷新失败: %v", err)
			}
		}
	}()

	r := setupRouter(api.DB)

	// 统一单端口监听（优先 LISTEN_ADDR，其次兼容 GALLERY_ADDR，默认 :8080）
	addr := strings.TrimSpace(os.Getenv("LISTEN_ADDR"))
	if addr == "" {
		addr = strings.TrimSpace(os.Getenv("GALLERY_ADDR"))
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
	log.Printf("server: unified service listening on %s (API on /api/twitter & /api/v2, Gallery SSR & static on /)", addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server: %v", err)
	}
}

func setupRouter(db *sql.DB) *gin.Engine {
	return api.SetupRouter(db)
}
