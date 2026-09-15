// Command gallery is a standalone, server-rendered gallery for twitter-pic.
//
// 账号页把 json 原文嵌进 HTML，前端（static/app.js）做网格分页、手机式全屏查看器
// （横向翻页 / 捏合与双击缩放 / 下滑关闭）与赞/踩/喜欢；赞踩计数用本地 JSON 文件持久化。没有全局 media 索引。
package gallery

import (
	"compress/gzip"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/Hana-ame/twitter-pic-go/limit"
	"github.com/Hana-ame/twitter-pic-go/tags"
	_ "modernc.org/sqlite" // 纯 Go 驱动，CGO_ENABLED=0 可用
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static
var staticFS embed.FS

var templates = template.Must(
	template.New("").Funcs(template.FuncMap{
		"sub": func(a, b int) int { return a - b },
	}).ParseFS(tmplFS, "templates/*.html"),
)

// appJS 内联进页面，保证导出的 HTML 单文件可用（不依赖 /static/app.js）。
var appJS = template.JS(mustReadStatic("app.js"))

// homeJS 主页脚本（搜索 / 增量加载 / 随机），内联进首页。
var homeJS = template.JS(mustReadStatic("home.js"))

const defaultPageSize = 12

func mustReadStatic(name string) string {
	b, err := staticFS.ReadFile("static/" + name)
	if err != nil {
		log.Printf("gallery: read static %s: %v", name, err)
		return ""
	}
	return string(b)
}

// ---- 数据模型 ----

type accountInfo struct {
	Name         string `json:"name"`
	Nick         string `json:"nick"`
	ProfileImage string `json:"profile_image"`
}

type timelineEntry struct {
	URL     string `json:"url"`
	Date    string `json:"date"`
	TweetID int64  `json:"tweet_id"`
	Type    string `json:"type"`
}

type document struct {
	AccountInfo accountInfo     `json:"account_info"`
	Timeline    []timelineEntry `json:"timeline"`
}

// ---- 视图模型 ----

type mediaItem struct {
	URL     string
	IsVideo bool
	TweetID int64
}

type accountPage struct {
	Slug      string
	Name      string
	Nick      string
	Avatar    string
	Media     []mediaItem
	Total     int
	Filter    string
	Cursor    int64
	PageSize  int
	HasPrev   bool
	HasNext   bool
	HrefAll   string
	HrefPhoto string
	HrefVideo string
	PrevHref  string
	NextHref  string
	RawJSON   template.JS
	ATagsJSON template.JS
	LegacyURL string
}

// acctItem 主页账号卡片（首字母头像色相与服务端/前端同算法）。
type acctItem struct {
	Name    string
	Initial string
	Hue     int
	Tags    []string
}

// acctEntry 是嵌入 #a-data 给前端的条目：账号名 + 标签 + 更新时间（均从 account_tags/users 读出）。
// U 为 "YYYY-MM-DD HH:MM:SS"（UTC，定宽格式，字典序即时间序）；缺失为空串排最后。
type acctEntry struct {
	Name string   `json:"n"`
	Tags []string `json:"t,omitempty"`
	U    string   `json:"u,omitempty"`
}

type homeData struct {
	Total     int
	Preview   []acctItem
	Cloud     []tags.Count // 标签云（SSR 前 36 个；前端拿全量数据重绘）
	Untagged  int
	NamesJSON template.JS // 实际是 []acctEntry 的 JSON
	MediaBase string      // 前端据此重写 pbs.twimg.com 图片域名（同 mediaURL 规则）
}

type pageData struct {
	Title      string
	Home       *homeData
	Account    *accountPage
	AppJS      template.JS
	HomeJS     template.JS
	LegacyBase string
}

// hueOf 与 static/home.js 中的 hueOf 保持一致（账号名为 ASCII）。
func hueOf(s string) int {
	h := 0
	for i := 0; i < len(s); i++ {
		h = (h*31 + int(s[i])) % 360
	}
	return h
}

type config struct {
	addr          string
	jsonDir       string
	mediaBase     string
	legacyBase    string
	pageSize      int
	reactionsFile string
	db            *sql.DB // 单一 twitter.db：标签唯一真源（与 twitter API 同一个库同一张表）
	tags          *tags.Store
	cloud         []tags.Count       // 全局标签云：启动时聚合一次的缓存
	writable      bool               // 标签库可写（POST 落 account_tags + request_logs）
	tagLimit      *limit.FastLimiter // 标签写入的 per-IP 配额，与根 API 同一个实现
	bans          *ipban.Manager     // IP 封禁：与根 API 同一个进程级单例（同一份 bans.txt）
	vis           *vis               // 被封账号隐身：users.status 视图（与根 API 同真源）
}

func Run(addr string) {
	if addr == "" {
		addr = envOr("GALLERY_ADDR", ":8090")
	}
	cfg := config{
		addr:          addr,
		jsonDir:       envOr("GALLERY_JSON_DIR", "."),
		mediaBase:     strings.TrimRight(envOr("GALLERY_MEDIA_BASE", ""), "/"),
		legacyBase:    legacyBase(),
		pageSize:      envIntOr("GALLERY_PAGE_SIZE", defaultPageSize),
		reactionsFile: envOr("GALLERY_REACTIONS_FILE", "./reactions.json"),
	}
	if cfg.pageSize <= 0 {
		cfg.pageSize = defaultPageSize
	}

	reactions := newReactionStore(cfg.reactionsFile)

	// 标签唯一真源：与 twitter API 同一个 twitter.db 的 account_tags 表。
	// 不再有独立的 tags.db 快照，也不再有 account_votes.json 投票文件。
	if store, writable, err := openTagStore(envOr("GALLERY_DB", "./twitter.db")); err == nil {
		cfg.db = store.DB()
		cfg.tags = store
		cfg.writable = writable
		defer cfg.db.Close()
	}
	cfg.vis = newVis(cfg.tags) // cfg.tags 为 nil 时 vis 一律 fail-open（不隐身）
	cfg.tagLimit = limit.NewFastLimiter(envIntOr("GALLERY_TAG_RATE_MAX", tagRateMax))
	// 封禁必须与根 API 共用进程级单例：两份内存副本各自 reload 会出现
	// 「API 侧已封、gallery 侧还没封」的窗口。热重载协程也只在 Shared() 里挂一次。
	cfg.bans = ipban.Shared()

	mux := galleryMux(cfg, reactions)

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("gallery: serving %s (json dir: %s)", cfg.addr, cfg.jsonDir)
	if err := srv.ListenAndServe(); err != nil {
		log.Printf("gallery: %v", err)
	}
}

// galleryMux 注册图站全部路由。
//
// 单独成函数是为了**能被测试走到**：/raw/{account} 的隐身判断写在路由闭包里，
// 只直调 handler 的测试永远盖不到它。路由语义（404 还是 200）应当在真实入口上验。
func galleryMux(cfg config, reactions *reactionStore) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { handleHome(w, r, cfg) })
	mux.HandleFunc("GET /u/{account}", func(w http.ResponseWriter, r *http.Request) { handleAccount(w, r, cfg) })
	// /raw/{account} 原样吐 json.gz（不解析）；首页卡片的头像/昵称/首图由前端流式自取。
	// 这是最大的泄漏口子：被封账号的完整时间线快照。用户已定「都挡」，不保留直链。
	mux.HandleFunc("GET /raw/{account}", func(w http.ResponseWriter, r *http.Request) {
		acc := r.PathValue("account")
		if !safeName(acc) || cfg.vis.hidden(acc) {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(cfg.jsonDir, acc+".json.gz"))
	})
	mux.HandleFunc("GET /api/reactions", func(w http.ResponseWriter, r *http.Request) { handleGetReactions(w, r, reactions) })
	mux.HandleFunc("GET /api/tag/{tag}", func(w http.ResponseWriter, r *http.Request) { handleTagUsers(w, r, cfg) })
	mux.HandleFunc("POST /api/react", func(w http.ResponseWriter, r *http.Request) { handlePostReact(w, r, reactions) })
	mux.HandleFunc("GET /api/tags", func(w http.ResponseWriter, r *http.Request) { handleGetAccountTags(w, r, cfg) })
	mux.HandleFunc("POST /api/tag", func(w http.ResponseWriter, r *http.Request) { handlePostAccountTag(w, r, cfg) })
	// /api/account-tags 与 /api/account-tag 是同一套处理器的别名（账号级标签改名后的入口）。
	mux.HandleFunc("GET /api/account-tags", func(w http.ResponseWriter, r *http.Request) { handleGetAccountTags(w, r, cfg) })
	mux.HandleFunc("POST /api/account-tag", func(w http.ResponseWriter, r *http.Request) { handlePostAccountTag(w, r, cfg) })

	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	}
	return mux
}

// handleTagUsers GET /api/tag/{tag}?limit= — tag 反查账号（权重降序）。
// 只返回磁盘上真实存在 json.gz、且未被封的账号，避免给出死链；标签库缺失时返回空列表。
func handleTagUsers(w http.ResponseWriter, r *http.Request, cfg config) {
	tag := strings.TrimSpace(r.PathValue("tag"))
	if tag == "" || len(tag) > 64 {
		http.Error(w, "bad tag", http.StatusBadRequest)
		return
	}
	limit := 2000
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = min(n, 50000)
		}
	}
	names, err := listAccounts(cfg.jsonDir)
	if err != nil {
		http.Error(w, "read json dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 反查列表也挡被封账号：过滤 exist 集合即可——UsersForTag 一边扫一边拿它筛，
	// 被过滤掉的不会占 limit 名额。根 API 的 by=tag 靠 SQL 里的 status='SUCCESS'
	// 挡的是同一件事，两层结果集因此对齐。
	names = cfg.vis.filter(names)
	exist := make(map[string]struct{}, len(names))
	for _, n := range names {
		exist[n] = struct{}{}
	}
	users := cfg.tags.UsersForTag(tag, exist, limit)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"tag": tag, "count": len(users), "users": users})
}

func handleHome(w http.ResponseWriter, r *http.Request, cfg config) {
	names, err := listAccounts(cfg.jsonDir)
	if err != nil {
		http.Error(w, "read json dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// 被封账号在首页隐身：账号列表和 #a-data 里都不出现（#a-data 是前端筛选与
	// 「加载更多」的数据源，漏在这里就等于全站泄漏）。
	names = cfg.vis.filter(names)
	const previewN = 120
	// 账号级 tags 按 username 现查（PK 前缀索引，分块 IN）；标签云用启动时聚合一次的缓存。
	tagged := cfg.tags.ForUsers(names)
	lm := cfg.tags.LastModifyFor(names)
	entries := make([]acctEntry, 0, len(names))
	untagged := 0
	for _, n := range names {
		t := tagged[n]
		entries = append(entries, acctEntry{Name: n, Tags: t, U: lm[n]})
		if len(t) == 0 {
			untagged++
		}
	}
	// 首页列表与标签筛选结果统一按 update 从新到旧；同刻/缺失按名字字典序兜底。
	// 前端从 #a-data 顺序继承该排序，筛选与"加载更多"分块不再重排。
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].U != entries[j].U {
			return entries[i].U > entries[j].U
		}
		return entries[i].Name < entries[j].Name
	})
	// 全局标签云：与账号列表同一个视图，封禁排除在 SQL 侧做（不做事后扣减）。
	// 视图没建立（无库/读不到 users）时为 nil，前端自行从已过滤的 a-data 重算。
	top := cfg.vis.cloud()
	preview := make([]acctItem, 0, previewN)
	for _, e := range entries[:min(len(entries), previewN)] {
		initial := "?"
		if e.Name != "" {
			initial = strings.ToUpper(e.Name[:1])
		}
		preview = append(preview, acctItem{Name: e.Name, Initial: initial, Hue: hueOf(e.Name), Tags: e.Tags})
	}
	dataJSON, err := json.Marshal(entries)
	if err != nil {
		http.Error(w, "marshal entries: "+err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "home.html", pageData{
		Title: "首页",
		Home: &homeData{
			Total: len(names), Preview: preview, Cloud: top, Untagged: untagged,
			NamesJSON: template.JS(dataJSON), MediaBase: cfg.mediaBase,
		},
		HomeJS:     homeJS,
		LegacyBase: cfg.legacyBase,
	})
}

func handleAccount(w http.ResponseWriter, r *http.Request, cfg config) {
	slug := r.PathValue("account")
	if !safeName(slug) || cfg.vis.hidden(slug) {
		// 被封账号一律 404（不是 403）：与"这个账号在图站上不存在"完全不可区分，
		// 不确认"存在但被封"。gallery 现有约定里找不到账号就是 http.NotFound，
		// 且这里没有"授权"语义，403 会被误解成"换个身份就能看"。
		http.NotFound(w, r)
		return
	}
	doc, err := loadDocument(filepath.Join(cfg.jsonDir, slug+".json.gz"))
	if err != nil {
		http.NotFound(w, r)
		return
	}

	filter := normalizeFilter(r.URL.Query().Get("type"))
	list := filterMedia(buildAll(doc, cfg.mediaBase), filter)

	start := clampStart(list, int(parseCursorID(r.URL.Query().Get("cursor"))), cfg.pageSize)
	end := start + cfg.pageSize
	if end > len(list) {
		end = len(list)
	}

	var prevCursor, nextCursor int64
	if start > 0 {
		prevCursor = int64(clampStart(list, start-cfg.pageSize, cfg.pageSize))
	}
	if end < len(list) {
		nextCursor = int64(end)
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		raw = []byte("{}")
	}

	var atagJSON []byte
	if b, err := json.Marshal(cfg.tags.Weights(slug)); err == nil {
		atagJSON = b
	}
	if atagJSON == nil {
		atagJSON = []byte("{}")
	}

	acc := &accountPage{
		Slug:      slug,
		Name:      firstNonEmpty(strings.TrimSpace(doc.AccountInfo.Name), slug),
		Nick:      strings.TrimSpace(doc.AccountInfo.Nick),
		Avatar:    mediaURL(cfg.mediaBase, doc.AccountInfo.ProfileImage),
		Media:     list[start:end],
		Total:     len(list),
		Filter:    filter,
		Cursor:    int64(start),
		PageSize:  cfg.pageSize,
		HasPrev:   start > 0,
		HasNext:   end < len(list),
		HrefAll:   buildHref(slug, "", 0),
		HrefPhoto: buildHref(slug, "photo", 0),
		HrefVideo: buildHref(slug, "video", 0),
		PrevHref:  buildHref(slug, filter, prevCursor),
		NextHref:  buildHref(slug, filter, nextCursor),
		RawJSON:   template.JS(raw),
		ATagsJSON: template.JS(atagJSON),
		LegacyURL: legacyURL(cfg.legacyBase, slug),
	}
	render(w, "account.html", pageData{Title: acc.Name, Account: acc, AppJS: appJS, LegacyBase: cfg.legacyBase})
}

// ---- reactions ----

type reactionCounts struct {
	Likes    int `json:"likes"`
	Dislikes int `json:"dislikes"`
}

type reactionStore struct {
	mu   sync.Mutex
	path string
	m    map[string]*reactionCounts
}

func newReactionStore(path string) *reactionStore {
	s := &reactionStore{path: path, m: map[string]*reactionCounts{}}
	if path != "" {
		if b, err := os.ReadFile(path); err == nil {
			_ = json.Unmarshal(b, &s.m)
		}
	}
	return s
}

func (s *reactionStore) snapshot(keys []string) map[string]reactionCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]reactionCounts, len(keys))
	for _, k := range keys {
		if c := s.m[k]; c != nil {
			out[k] = *c
		} else {
			out[k] = reactionCounts{}
		}
	}
	return out
}

func (s *reactionStore) apply(key string, dl, dd int) reactionCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.m[key]
	if c == nil {
		c = &reactionCounts{}
		s.m[key] = c
	}
	c.Likes += dl
	if c.Likes < 0 {
		c.Likes = 0
	}
	c.Dislikes += dd
	if c.Dislikes < 0 {
		c.Dislikes = 0
	}
	s.saveLocked()
	return *c
}

func (s *reactionStore) saveLocked() {
	if s.path == "" {
		return
	}
	b, err := json.MarshalIndent(s.m, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.path)
}

func handleGetReactions(w http.ResponseWriter, r *http.Request, s *reactionStore) {
	var keys []string
	for _, k := range strings.Split(r.URL.Query().Get("keys"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	writeJSON(w, s.snapshot(keys))
}

func handlePostReact(w http.ResponseWriter, r *http.Request, s *reactionStore) {
	var req struct {
		Key string `json:"key"`
		DL  int    `json:"dl"`
		DD  int    `json:"dd"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Key) == "" {
		http.Error(w, "key required", http.StatusBadRequest)
		return
	}
	c := s.apply(req.Key, req.DL, req.DD)
	writeJSON(w, map[string]any{"key": req.Key, "likes": c.Likes, "dislikes": c.Dislikes})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// ---- helpers ----

func buildAll(doc document, mediaBase string) []mediaItem {
	out := make([]mediaItem, 0, len(doc.Timeline))
	for _, te := range doc.Timeline {
		raw := strings.TrimSpace(te.URL)
		if raw == "" {
			continue
		}
		out = append(out, mediaItem{
			URL:     mediaURL(mediaBase, raw),
			IsVideo: te.Type == "video" || te.Type == "animated_gif",
			TweetID: te.TweetID,
		})
	}
	return out
}

func filterMedia(items []mediaItem, filter string) []mediaItem {
	if filter == "" {
		return items
	}
	out := make([]mediaItem, 0, len(items))
	for _, m := range items {
		switch filter {
		case "video":
			if m.IsVideo {
				out = append(out, m)
			}
		case "photo":
			if !m.IsVideo {
				out = append(out, m)
			}
		}
	}
	return out
}

func normalizeFilter(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "photo", "image", "img", "picture":
		return "photo"
	case "video", "videos", "movie", "animated_gif", "gif":
		return "video"
	default:
		return ""
	}
}

func parseCursorID(s string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func clampStart(items []mediaItem, start, pageSize int) int {
	if len(items) == 0 {
		return 0
	}
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	last := ((len(items) - 1) / pageSize) * pageSize
	if start < 0 {
		return 0
	}
	if start > last {
		return last
	}
	return start
}

func buildHref(slug, filter string, cursor int64) string {
	q := url.Values{}
	if filter != "" {
		q.Set("type", filter)
	}
	if cursor != 0 {
		q.Set("cursor", strconv.FormatInt(cursor, 10))
	}
	u := "/u/" + url.PathEscape(slug)
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	return u
}

func listAccounts(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasPrefix(n, ".") {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(n), ".json.gz") {
			continue
		}
		names = append(names, strings.TrimSuffix(n, ".json.gz"))
	}
	sort.Strings(names)
	return names, nil
}

// cloudTopN 是全局标签云的条目上限（首页 SSR 用）。
const cloudTopN = 36

// tagRateMax 是标签写入的每 IP 每小时配额，默认值与根 API 的
// limit.NewFastLimiter(25) 对齐——两层现在写同一张表，配额必须同量。
const tagRateMax = 25

// openTagStore 打开**与 twitter API 同一个** twitter.db，返回 account_tags 的
// 读写访问器。读写语义全部在 tags 包里，两层共用一份实现。
//
// 同一个 sqlite 文件由 API 侧连接与本连接共同读写：busy_timeout + WAL 兜底并发。
// 文件缺失/打不开只降级为「无标签」（图站照常跑）；建表失败降级为只读（POST 503）。
func openTagStore(path string) (*tags.Store, bool, error) {
	if path == "" {
		return nil, false, fmt.Errorf("GALLERY_DB 未配置")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, false, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Clean(path)+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, false, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, false, err
	}
	writable := true
	if err := tags.EnsureSchema(db); err != nil {
		log.Printf("gallery: account_tags 建表失败（标签降为只读）: %v", err)
		writable = false
	}
	return tags.New(db), writable, nil
}

func loadDocument(fp string) (document, error) {
	var doc document
	f, err := os.Open(fp)
	if err != nil {
		return doc, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return doc, err
	}
	defer zr.Close()
	if err := json.NewDecoder(zr).Decode(&doc); err != nil {
		return doc, err
	}
	return doc, nil
}

func mediaURL(base, raw string) string {
	raw = strings.TrimSpace(raw)
	if base == "" || raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host != "pbs.twimg.com" {
		return raw
	}
	out := base + u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// legacyBase 读 GALLERY_LEGACY_BASE：未设置用默认；显式设为空串则隐藏旧版入口。
func legacyBase() string {
	v, ok := os.LookupEnv("GALLERY_LEGACY_BASE")
	if !ok {
		v = "https://x.4545810.xyz"
	}
	return strings.TrimRight(v, "/")
}

// legacyURL 拼旧版站点链接：{base}/{user}；base 为空则不显示旧版入口。
func legacyURL(base, slug string) string {
	if base == "" {
		return ""
	}
	return base + "/" + url.PathEscape(slug)
}

func safeName(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, `/\\`) || strings.Contains(name, "..") {
		return false
	}
	return name == filepath.Base(name)
}

func render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("gallery: render %s: %v", name, err)
	}
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

func sanitizeTag(t string) string {
	t = strings.TrimSpace(t)
	var b strings.Builder
	for _, r := range t {
		if r == '#' || r == ' ' || r < 32 {
			continue
		}
		b.WriteRune(r)
	}
	rs := []rune(b.String())
	if len(rs) > 24 {
		rs = rs[:24]
	}
	return string(rs)
}

// ---- 账号级标签：唯一真源 account_tags（与 twitter API 同表、同语义） ----

// handleGetAccountTags GET /api/tags?keys=u1,u2 · GET /api/account-tags?keys=
// 直接用新数据源：读 account_tags 的权重，不再读任何投票 JSON。
func handleGetAccountTags(w http.ResponseWriter, r *http.Request, cfg config) {
	out := map[string]map[string]int{}
	for _, u := range strings.Split(r.URL.Query().Get("keys"), ",") {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		// 被封的 key 直接从返回里省略——等价于"没有这个账号"。批量接口逐个 404
		// 会把整批请求打断，而且"少一个 key"和"不存在"本来就无法区分。
		if cfg.vis.hidden(u) {
			continue
		}
		out[u] = cfg.tags.Weights(u)
	}
	writeJSON(w, out)
}

// handlePostAccountTag POST /api/tag · POST /api/account-tag
// body {user|key, tag, d}：d 归一到 ±1（与根 API 的 POST 归一化一致），
// 累加写进 account_tags，并照记一条 request_logs 流水。
//
// 守卫顺序（与根 API 的中间件链同构）：
// 封禁 403 → 库不可写 503 → 配额 429 → 入参 400 → 目标被封 404 → 写失败 500 → 200。
// 封禁放最前：被封 IP 不该消耗自己的配额，也不该靠状态码差异探出
// 「标签功能开没开」；且它在 Add 之前，所以既不写库也不写 request_logs。
func handlePostAccountTag(w http.ResponseWriter, r *http.Request, cfg config) {
	if ip, banned := cfg.bans.Decide(r); banned {
		ipban.WriteDenied(w, ip)
		return
	}
	if cfg.tags == nil || !cfg.writable {
		http.Error(w, "tags disabled", http.StatusServiceUnavailable)
		return
	}
	// IP 口径统一走 ipban.Principal（按可信跳数取）：限流分桶、request_logs.ip、
	// 根 API 三处同一个值。原先这里是「XFF 首项」，既能被
	// `X-Forwarded-For: <好人IP>, <被封IP>` 绕过封禁，也和流水里的归属对不上。
	ip := ipban.Principal(r)
	if cfg.tagLimit != nil && !cfg.tagLimit.Allow(ip) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 429, "message": "请求过于频繁，请一小时后再试",
		})
		return
	}
	var req struct {
		User string `json:"user"`
		Key  string `json:"key"` // 旧字段名，兼容保留
		Tag  string `json:"tag"`
		D    int    `json:"d"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	user := firstNonEmpty(strings.TrimSpace(req.User), strings.TrimSpace(req.Key))
	tag := sanitizeTag(req.Tag)
	d := req.D
	if d > 1 {
		d = 1
	} else if d < -1 {
		d = -1
	}
	if user == "" || tag == "" || d == 0 || !safeName(user) {
		http.Error(w, "user and tag required", http.StatusBadRequest)
		return
	}
	// 不给被封账号投票：投了就会把它的标签重新推上首页标签云，等于把刚挡掉的
	// 存在性又写回去。404 与 /u/{name} 的口径一致（不确认存在）。
	// 放在配额之后：刷不存在账号的 IP 照样该被配额管住。
	if cfg.vis.hidden(user) {
		http.NotFound(w, r)
		return
	}
	if err := cfg.tags.Add(user, map[string]int{tag: d}, ip, r.UserAgent()); err != nil {
		log.Printf("gallery: POST tag %s %q=%d: %v", user, tag, d, err)
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"user": user, "tags": cfg.tags.Weights(user)})
}
