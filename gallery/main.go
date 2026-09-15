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
	LegacyURL string
}

// acctItem 主页账号卡片（首字母头像色相与服务端/前端同算法）。
type acctItem struct {
	Name    string
	Initial string
	Hue     int
	Tags    []string
}

// acctEntry 是嵌入 #a-data 给前端的条目：账号名 + 标签（标签由后端从 tags.db 读出）。
type acctEntry struct {
	Name string   `json:"n"`
	Tags []string `json:"t,omitempty"`
}

type tagCount struct {
	Tag   string
	Count int
}

type homeData struct {
	Total     int
	Preview   []acctItem
	Cloud     []tagCount // 标签云（SSR 前 36 个；前端拿全量数据重绘）
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
	tags          *tagStore
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
	cfg.tags = openTagStore(envOr("GALLERY_TAGS_DB", "./tags.db"))

	reactions := newReactionStore(cfg.reactionsFile)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { handleHome(w, r, cfg) })
	mux.HandleFunc("GET /u/{account}", func(w http.ResponseWriter, r *http.Request) { handleAccount(w, r, cfg) })
	// /raw/{account} 原样吐 json.gz（不解析）；首页卡片的头像/昵称/首图由前端流式自取。
	mux.HandleFunc("GET /raw/{account}", func(w http.ResponseWriter, r *http.Request) {
		acc := r.PathValue("account")
		if !safeName(acc) {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(cfg.jsonDir, acc+".json.gz"))
	})
	mux.HandleFunc("GET /api/reactions", func(w http.ResponseWriter, r *http.Request) { handleGetReactions(w, r, reactions) })
	mux.HandleFunc("POST /api/react", func(w http.ResponseWriter, r *http.Request) { handlePostReact(w, r, reactions) })

	if sub, err := fs.Sub(staticFS, "static"); err == nil {
		mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	}

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

func handleHome(w http.ResponseWriter, r *http.Request, cfg config) {
	names, err := listAccounts(cfg.jsonDir)
	if err != nil {
		http.Error(w, "read json dir: "+err.Error(), http.StatusInternalServerError)
		return
	}
	const previewN = 120
	// 账号级 tags 按 username 现查（PK 前缀索引，分块 IN）；标签云用启动时聚合一次的缓存。
	tagged := cfg.tags.tagsFor(names)
	entries := make([]acctEntry, 0, len(names))
	untagged := 0
	for _, n := range names {
		t := tagged[n]
		entries = append(entries, acctEntry{Name: n, Tags: t})
		if len(t) == 0 {
			untagged++
		}
	}
	var top []tagCount
	if cfg.tags != nil {
		top = cfg.tags.cloud // 全局标签云：启动时聚合一次的缓存；db 不可用时为 nil（前端自行从 a-data 重算）
	}
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
		Title:      "首页",
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
	if !safeName(slug) {
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

	start := clampStart(list, indexOfTweet(list, parseCursorID(r.URL.Query().Get("cursor"))), cfg.pageSize)
	end := start + cfg.pageSize
	if end > len(list) {
		end = len(list)
	}

	var prevCursor, nextCursor int64
	if start > 0 {
		prevCursor = cursorOf(list, clampStart(list, start-cfg.pageSize, cfg.pageSize))
	}
	if end < len(list) {
		nextCursor = list[end].TweetID
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		raw = []byte("{}")
	}

	acc := &accountPage{
		Slug:      slug,
		Name:      firstNonEmpty(strings.TrimSpace(doc.AccountInfo.Name), slug),
		Nick:      strings.TrimSpace(doc.AccountInfo.Nick),
		Avatar:    mediaURL(cfg.mediaBase, doc.AccountInfo.ProfileImage),
		Media:     list[start:end],
		Total:     len(list),
		Filter:    filter,
		Cursor:    cursorOf(list, start),
		PageSize:  cfg.pageSize,
		HasPrev:   start > 0,
		HasNext:   end < len(list),
		HrefAll:   buildHref(slug, "", 0),
		HrefPhoto: buildHref(slug, "photo", 0),
		HrefVideo: buildHref(slug, "video", 0),
		PrevHref:  buildHref(slug, filter, prevCursor),
		NextHref:  buildHref(slug, filter, nextCursor),
		RawJSON:   template.JS(raw),
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

func indexOfTweet(items []mediaItem, id int64) int {
	if id == 0 {
		return 0
	}
	for i := range items {
		if items[i].TweetID == id {
			return i
		}
	}
	return 0
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

func cursorOf(items []mediaItem, i int) int64 {
	if i < 0 || i >= len(items) {
		return 0
	}
	return items[i].TweetID
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

// tagStore 是 tags.db（account_tags 表，见 scripts/build_tags_db.py）的只读访问器。
// 设计：不把全表常驻内存——账号级标签按 username 现查（WITHOUT ROWID 主键
// (username,tag) 前缀命中，分块 IN 减少往返）；唯一的缓存是全局标签云，
// 启动时 GROUP BY 聚合一次，之后不再算。文件缺失/损坏只降级为「无标签」。
type tagStore struct {
	db    *sql.DB
	cloud []tagCount // 全局 top36，仅启动时算一次
}

func openTagStore(path string) *tagStore {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		log.Printf("gallery: tags db %s not used: %v", path, err)
		return nil
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Clean(path)+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		log.Printf("gallery: open tags db: %v", err)
		return nil
	}
	st := &tagStore{db: db}
	rows, err := db.Query("SELECT tag, COUNT(DISTINCT username) FROM account_tags GROUP BY tag ORDER BY 2 DESC, tag LIMIT 36")
	if err != nil {
		log.Printf("gallery: tags cloud query failed (degraded to no tags): %v", err)
		db.Close()
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var tag string
		var cnt int
		if err := rows.Scan(&tag, &cnt); err != nil {
			continue
		}
		st.cloud = append(st.cloud, tagCount{Tag: tag, Count: cnt})
	}
	if err := rows.Err(); err != nil {
		log.Printf("gallery: tags cloud scan: %v", err)
	}
	log.Printf("gallery: tags db ready (cloud cached: %d tags)", len(st.cloud))
	return st
}

// tagsFor 按 username 分块查询标签，权重降序。db 不可用返回空 map（调用方按无标签处理）。
func (st *tagStore) tagsFor(names []string) map[string][]string {
	out := map[string][]string{}
	if st == nil || len(names) == 0 {
		return out
	}
	const chunk = 400 // 远低于 SQLite 变量数上限
	for i := 0; i < len(names); i += chunk {
		batch := names[i:min(i+chunk, len(names))]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for k, n := range batch {
			args[k] = n
		}
		rows, err := st.db.Query(
			"SELECT username, tag FROM account_tags WHERE username IN ("+ph+") ORDER BY username, weight DESC, tag",
			args...)
		if err != nil {
			log.Printf("gallery: tagsFor chunk@%d failed: %v", i, err)
			return out
		}
		for rows.Next() {
			var u, t string
			if err := rows.Scan(&u, &t); err != nil {
				continue
			}
			out[u] = append(out[u], t)
		}
		rows.Close()
	}
	return out
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
