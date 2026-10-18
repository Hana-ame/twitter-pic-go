package gallery

import (
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/Hana-ame/twitter-pic-go/limit"
	"github.com/Hana-ame/twitter-pic-go/tags"
	_ "modernc.org/sqlite"
)

const defaultPageSize = 12
const cloudTopN = 36
const tagRateMax = 25

type config struct {
	addr          string
	jsonDir       string
	mediaBase     string
	videoBase     string
	ouoPrefix     string
	legacyBase    string
	pageSize      int
	reactionsFile string
	db            *sql.DB
	tags          *tags.Store
	cloud         []tags.Count
	writable      bool
	tagLimit      *limit.FastLimiter
	bans          *ipban.Manager
	vis           *vis
	home          *homeCache
}

// NewHandler 构建并返回图站的 http.Handler（供与 API 挂载在同一个 HTTP 路由与端口上使用）。
// 可选传入已初始化的 *sql.DB（与 API 共用同一数据库连接池）；若未传入，则读取 GALLERY_DB（默认 ./twitter.db）开启。
func NewHandler(dbs ...*sql.DB) http.Handler {
	cfg := config{
		jsonDir:       envOr("GALLERY_JSON_DIR", "."),
		mediaBase:     strings.TrimRight(envOr("GALLERY_MEDIA_BASE", defaultMediaBase), "/"),
		videoBase:     strings.TrimRight(envOr("GALLERY_VIDEO_BASE", defaultVideoBase), "/"),
		ouoPrefix:     resolveOuoPrefix(),
		legacyBase:    legacyBase(),
		pageSize:      envIntOr("GALLERY_PAGE_SIZE", defaultPageSize),
		reactionsFile: envOr("GALLERY_REACTIONS_FILE", "./reactions.json"),
	}
	if cfg.videoBase == "" {
		cfg.videoBase = defaultVideoBase
	}
	if cfg.pageSize <= 0 {
		cfg.pageSize = defaultPageSize
	}

	reactions := newReactionStore(cfg.reactionsFile)

	var db *sql.DB
	if len(dbs) > 0 && dbs[0] != nil {
		db = dbs[0]
	}

	if db != nil {
		cfg.db = db
		cfg.tags = tags.New(db)
		cfg.writable = true
		if err := tags.EnsureSchema(db); err != nil {
			log.Printf("gallery: account_tags 建表失败（标签降为只读）: %v", err)
			cfg.writable = false
		} else {
			if _, err := tags.BackfillVoteBase(db); err != nil {
				log.Printf("gallery: 历史底数快照失败（计票仍可用，写侧会逐行补）: %v", err)
			}
		}
	} else if store, writable, err := openTagStore(envOr("GALLERY_DB", "./twitter.db")); err == nil {
		cfg.db = store.DB()
		cfg.tags = store
		cfg.writable = writable
		if writable {
			if _, err := tags.BackfillVoteBase(store.DB()); err != nil {
				log.Printf("gallery: 历史底数快照失败（计票仍可用，写侧会逐行补）: %v", err)
			}
		}
	} else {
		log.Printf("gallery: 打开标签库失败: %v", err)
	}

	cfg.vis = newVis(cfg.tags)
	cfg.tagLimit = limit.NewFastLimiter(envIntOr("GALLERY_TAG_RATE_MAX", tagRateMax))
	cfg.bans = ipban.Shared()

	cfg.home = newHomeCache(
		time.Duration(envIntOr("GALLERY_HOME_CACHE_TTL", int(homeCacheTTL/time.Second)))*time.Second,
		func() ([]byte, error) { return buildHomeHTML(cfg) },
	)

	// 注册跨层数据变更钩子：当 API 侧（如 /api/twitter/tag、/api/v2/tags/vote 或封禁接口）
	// 修改标签或用户状态时，自动使 Gallery 的可见性视图和首页 SSR 缓存失效。
	targetDB := cfg.db
	visRef := cfg.vis
	homeRef := cfg.home
	tags.OnDataChange(func(changedDB *sql.DB) {
		if changedDB == nil || changedDB == targetDB {
			if visRef != nil {
				visRef.invalidate()
			}
			if homeRef != nil {
				homeRef.invalidate()
			}
		}
	})

	if cfg.home != nil {
		go func() {
			time.Sleep(2 * time.Second)
			cfg.home.warm()
		}()
	}

	mux := galleryMux(cfg, reactions)
	return mux
}

// Run 单独启动 Gallery HTTP 服务（供独立调试模式 cmd/gallery 使用；
// 生产环境由 server/main.go 统一在单地址挂载 API 与 Gallery）。
func Run(addr string) {
	if addr == "" {
		addr = envOr("LISTEN_ADDR", envOr("GALLERY_ADDR", ":8080"))
	}
	handler := logRequests(NewHandler())

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("gallery: serving standalone on %s (json dir: %s)", addr, envOr("GALLERY_JSON_DIR", "."))
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("gallery: %v", err)
	}
}

// galleryMux 注册图站全部路由。
func galleryMux(cfg config, reactions *reactionStore) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { handleHome(w, r, cfg) })
	mux.HandleFunc("GET /index.html", func(w http.ResponseWriter, r *http.Request) { handleHome(w, r, cfg) })
	mux.HandleFunc("GET /u/{account}", func(w http.ResponseWriter, r *http.Request) { handleAccount(w, r, cfg) })
	mux.HandleFunc("GET /video/{filepath...}", func(w http.ResponseWriter, r *http.Request) { handleGalleryVideoRedirect(w, r, cfg) })
	mux.HandleFunc("GET /api/video/{filepath...}", func(w http.ResponseWriter, r *http.Request) { handleGalleryVideoRedirect(w, r, cfg) })
	mux.HandleFunc("GET /api/reactions", func(w http.ResponseWriter, r *http.Request) { handleGetReactions(w, r, reactions) })
	mux.HandleFunc("POST /api/react", func(w http.ResponseWriter, r *http.Request) { handlePostReact(w, r, reactions) })
	mux.HandleFunc("GET /api/tag-cloud", func(w http.ResponseWriter, r *http.Request) { handleTagCloud(w, r, cfg) })
	mux.HandleFunc("GET /api/tags/cloud", func(w http.ResponseWriter, r *http.Request) { handleTagCloud(w, r, cfg) })
	mux.HandleFunc("GET /api/tag/{tag}", func(w http.ResponseWriter, r *http.Request) { handleTagUsers(w, r, cfg) })
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) { handleGetAccountTags(w, r, cfg) })
	mux.HandleFunc("GET /api/account-tags", func(w http.ResponseWriter, r *http.Request) { handleGetAccountTags(w, r, cfg) })
	mux.HandleFunc("POST /api/tag", func(w http.ResponseWriter, r *http.Request) { handlePostAccountTag(w, r, cfg) })
	mux.HandleFunc("POST /api/account-tag", func(w http.ResponseWriter, r *http.Request) { handlePostAccountTag(w, r, cfg) })

	mux.Handle("GET /static/", defaultAssets.staticHandler())
	return mux
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envIntOr(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n
		}
	}
	return def
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
