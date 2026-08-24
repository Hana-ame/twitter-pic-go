package gallery

import (
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed templates/index.html
var templateFS embed.FS

var pageTemplate = template.Must(
	template.New("index.html").Funcs(template.FuncMap{
		"platformIcon":    platformIcon,
		"platformName":    platformName,
		"resolvePlatform": resolvePlatform,
	}).ParseFS(templateFS, "templates/index.html"),
)

// Server holds the gallery index.
type Server struct {
	mu          sync.RWMutex
	gallery     *Gallery
	db          *sql.DB
	tagDB       *sql.DB // 读取账号级 tag / emoji 投票（GALLERY_TAG_DB，默认 twitter.db）
	accountTags map[string][]string
	links       map[string][]AccountLink // account → 外部链接
}

func (s *Server) current() *Gallery {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.gallery
}

func (s *Server) swap(next *Gallery) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gallery = next
}

// Run starts the gallery HTTP server on the given address. If addr is empty,
// it falls back to the GALLERY_ADDR env var, then ":8090".
// Intended to be launched as a goroutine from the main binary so the whole
// service ships as a single binary while gallery stays its own package.
func Run(addr string) {
	if addr == "" {
		addr = envOr("GALLERY_ADDR", ":8090")
	}
	jsonDir := envOr("GALLERY_JSON_DIR", "./")
	tagDBPath := envOr("GALLERY_TAG_DB", "./twitter.db")
	dbPath := envOr("GALLERY_DB", "./gallery.db")

	// Tags are account-level tags stored by the existing pipeline in SQLite.
	tagDB := openDB(tagDBPath)
	accountTags := loadAccountTags(tagDB)

	// —— 远程 JSON 源（临时调试，见 remote.go）：不设置环境变量则完全不走这段 ——
	g := NewGallery(jsonDir, accountTags)
	// db 账号索引（users 表的昵称/最近更新）：必须在首次 Scan() 之前注入，
	// 这样还没有媒体数据的账号也能出现在最新/最热/收藏等列表里。
	g.SetDBAccounts(loadAccountMeta(tagDB))
	if remoteBase := os.Getenv("GALLERY_REMOTE_JSON_BASE"); remoteBase != "" {
		log.Printf("gallery: REMOTE JSON SOURCE (lazy proxy) enabled: %s", remoteBase)
		g.SetRemoteSource(remoteBase)
	}
	if err := g.Scan(); err != nil {
		log.Fatalf("gallery: initial scan failed: %v", err)
	}
	log.Printf("gallery: indexed %d media entries from json.gz under %s", len(g.media), jsonDir)

	db := openDB(dbPath)

	// 扁平 per-image 标签体系（与账号 tag 完全分离）
	initMediaTagSchema(db)
	// 账号外部链接（pixiv / ci-en / fanbox 等）
	initAccountLinksSchema(db)
	// 把已持久化的 per-image 标签（含赞/倒赞）回填进内存索引
	g.AttachReactions(db)

	s := &Server{
		gallery:     g,
		db:          db,
		tagDB:       tagDB,
		accountTags: accountTags,
		links:       loadAccountLinks(db),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleBrowse)
	mux.HandleFunc("GET /latest", s.handleLatest)
	mux.HandleFunc("GET /hot", s.handleHot)
	mux.HandleFunc("GET /favorites", s.handleFavorites)
	mux.HandleFunc("GET /tags", s.handleTagsPage)
	mux.HandleFunc("GET /clusters", s.handleClustersPage)
	mux.HandleFunc("GET /{username}", s.handleBrowse)
	mux.HandleFunc("GET /{username}/{rest...}", s.handleBrowse)
	mux.HandleFunc("POST /rescan", s.handleRescan)
	mux.HandleFunc("POST /react", s.handleReact)
	mux.HandleFunc("GET /age-gate", s.handleAgeGatePage)
	mux.HandleFunc("POST /age-gate", s.handleAgeGateConfirm)
	mux.HandleFunc("GET /sitemap.xml", s.handleSitemap)
	mux.HandleFunc("POST /api/link", s.handleSetLink)
	mux.HandleFunc("DELETE /api/link", s.handleDeleteLink)

	log.Printf("gallery: server listening on %s (json dir: %s)", addr, jsonDir)
	if err := http.ListenAndServe(addr, logRequests(ageGate(mux))); err != nil {
		log.Fatalf("gallery: server error: %v", err)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func openDB(path string) *sql.DB {
	if path == "" {
		return nil
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		log.Printf("gallery: open db %s failed: %v", path, err)
		return nil
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		log.Printf("gallery: ping db %s failed: %v", path, err)
		return nil
	}
	return db
}

// loadAccountTags reads the existing user_tags table (username -> tag weights)
// and keeps positive tags for searching.
func loadAccountTags(db *sql.DB) map[string][]string {
	out := map[string][]string{}
	if db == nil {
		return out
	}
	rows, err := db.Query(`SELECT username, tags FROM user_tags`)
	if err != nil {
		log.Printf("gallery: query user_tags failed: %v", err)
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var username, tagsRaw string
		if err := rows.Scan(&username, &tagsRaw); err != nil {
			continue
		}
		var weights map[string]int
		if err := json.Unmarshal([]byte(tagsRaw), &weights); err != nil {
			continue
		}
		var tags []string
		for t, w := range weights {
			if w > 0 {
				tags = append(tags, t)
			}
		}
		out[username] = uniqueSorted(tags)
	}
	return out
}

// loadAccountMeta reads the users table (username / nick / last_modify) from
// the pipeline db, so accounts without local media can still be listed
// (最新/最热/收藏) with nickname and last-update date.
func loadAccountMeta(db *sql.DB) map[string]AccountMeta {
	out := map[string]AccountMeta{}
	if db == nil {
		return out
	}
	rows, err := db.Query(`SELECT username, COALESCE(nick,''), COALESCE(last_modify,'') FROM users`)
	if err != nil {
		log.Printf("gallery: query users failed: %v", err)
		return out
	}
	defer rows.Close()

	for rows.Next() {
		var username, nick, lastMod string
		if err := rows.Scan(&username, &nick, &lastMod); err != nil {
			continue
		}
		if username == "" {
			continue
		}
		out[username] = AccountMeta{Nick: nick, LastModify: parseFlexibleTime(lastMod)}
	}
	return out
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/proxy") || r.Method == http.MethodPost {
			log.Printf("gallery: %s %s %s", r.Method, r.URL.Path, time.Since(start))
		}
	})
}

var ageGateHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<meta name="rating" content="mature">
<meta name="robots" content="noindex">
<title>年龄验证 · Gallery</title>
<style>
*{margin:0;padding:0;box-sizing:border-box}
body{background:#111;color:#e6e9ef;font-family:system-ui,-apple-system,sans-serif;
  display:flex;align-items:center;justify-content:center;min-height:100vh}
.card{background:#191d24;border:1px solid #2d3542;border-radius:16px;
  padding:48px 40px;max-width:420px;text-align:center}
h1{font-size:22px;margin-bottom:12px}
p{color:#8b95a7;font-size:14px;line-height:1.6;margin-bottom:28px}
.actions{display:flex;gap:12px;justify-content:center;align-items:center;flex-wrap:wrap}
.btn{display:inline-block;padding:12px 24px;border-radius:10px;font-size:15px;
  font-weight:600;cursor:pointer;text-decoration:none;border:1px solid transparent;margin:0}
.btn-yes{background:#4f8cff;color:#fff}
.btn-yes:hover{background:#3a7aee}
.btn-no{background:transparent;color:#8b95a7;border:1px solid #2d3542}
.btn-no:hover{color:#e6e9ef;border-color:#4f8cff}
</style>
</head>
<body>
<div class="card">
  <h1>&#9888; 年龄验证</h1>
  <p>本站包含成人内容。请确认您已年满 <strong>18 岁</strong>。</p>
  <div class="actions">
    <form method="POST" action="/age-gate" style="margin:0">
      <button type="submit" class="btn btn-yes">我已满 18 岁，进入</button>
    </form>
    <a href="https://www.google.com" class="btn btn-no">未满 18 岁，离开</a>
  </div>
</div>
</body>
</html>`

func ageGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/age-gate" || r.URL.Path == "/sitemap.xml" || r.Method == http.MethodPost ||
			strings.HasPrefix(r.URL.Path, "/favicon") || strings.HasPrefix(r.URL.Path, "/api/") {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie("age_verified"); err == nil && c.Value == "1" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, ageGateHTML)
	})
}

func (s *Server) handleAgeGatePage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, ageGateHTML)
}

func (s *Server) handleAgeGateConfirm(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     "age_verified",
		Value:    "1",
		Path:     "/",
		MaxAge:   86400 * 365,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleSitemap(w http.ResponseWriter, r *http.Request) {
	host := "http://" + r.Host
	if r.TLS != nil {
		host = "https://" + r.Host
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?>
<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">
  <url><loc>`+host+`/</loc><changefreq>daily</changefreq><priority>1.0</priority></url>
  <url><loc>`+host+`/hot</loc><changefreq>daily</changefreq><priority>0.8</priority></url>
  <url><loc>`+host+`/tags</loc><changefreq>weekly</changefreq><priority>0.6</priority></url>
`)
	s.mu.RLock()
	seen := map[string]bool{}
	for _, m := range s.gallery.media {
		if m.Dir == "" || seen[m.Dir] {
			continue
		}
		seen[m.Dir] = true
		fmt.Fprint(w, `  <url><loc>`+host+`/`+m.Dir+`</loc><changefreq>weekly</changefreq><priority>0.7</priority></url>`+"\n")
	}
	s.mu.RUnlock()
	fmt.Fprint(w, `</urlset>`)
}

func (s *Server) handleSetLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account  string `json:"account"`
		Platform string `json:"platform"`
		URL      string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Account == "" || req.URL == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	platform := resolvePlatform(req.Platform)
	if err := upsertAccountLink(s.db, req.Account, platform, req.URL); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.links = loadAccountLinks(s.db)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleDeleteLink(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account  string `json:"account"`
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Account == "" || req.Platform == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := deleteAccountLink(s.db, req.Account, req.Platform); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.links = loadAccountLinks(s.db)
	w.WriteHeader(http.StatusOK)
}

// ---------- small helpers ----------

func parseCSV(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

func intParam(r *http.Request, key string, def, min, max int) int {
	v, err := strconv.Atoi(r.URL.Query().Get(key))
	if err != nil {
		return def
	}
	if v < min {
		v = min
	}
	if max > 0 && v > max {
		v = max
	}
	return v
}

func matchesTypeFilter(m *Media, filter string) bool {
	switch filter {
	case "image", "photo":
		return m.Type == "photo"
	case "video":
		return m.IsVideo()
	case "animated_gif", "gif":
		return m.Type == "animated_gif"
	default:
		return true
	}
}

func (s *Server) mediaUnder(g *Gallery, dir string) []*Media {
	if dir == "" {
		return g.media
	}
	prefix := dir + "/"
	out := make([]*Media, 0)
	for _, m := range g.media {
		if m.Dir == dir || strings.HasPrefix(m.Dir, prefix) {
			out = append(out, m)
		}
	}
	return out
}

func sortMedia(list []*Media, sortKey string) {
	switch sortKey {
	case "time":
		sort.Slice(list, func(i, j int) bool {
			if !list[i].ModTime.Equal(list[j].ModTime) {
				return list[i].ModTime.After(list[j].ModTime)
			}
			return list[i].Name < list[j].Name
		})
	case "size":
		sort.Slice(list, func(i, j int) bool {
			if list[i].Size != list[j].Size {
				return list[i].Size > list[j].Size
			}
			return list[i].Name < list[j].Name
		})
	default:
		sort.Slice(list, func(i, j int) bool {
			return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name)
		})
	}
}

// ---------- browse URL building ----------

const defaultSortKey = "time"

const defaultPageSize = 10

func addTag(list []string, tag string) []string {
	for _, t := range list {
		if t == tag {
			return list
		}
	}
	out := make([]string, 0, len(list)+1)
	out = append(out, list...)
	out = append(out, tag)
	return out
}

// browseURLParams 一条浏览 URL 的全部可变参数。
type browseURLParams struct {
	dir        string
	page       int
	pageSize   int // <=0 表示不写入（用服务端默认值）
	include    []string
	exclude    []string
	typeFilter string
	sortKey    string
	recursive  bool
}

func buildBrowseURLParams(p browseURLParams) string {
	basePath := "/"
	if p.dir != "" {
		basePath = "/" + p.dir
	}
	q := url.Values{}
	if len(p.include) > 0 {
		q.Set("tags", strings.Join(p.include, ","))
	}
	if len(p.exclude) > 0 {
		q.Set("exclude_tags", strings.Join(p.exclude, ","))
	}
	if p.typeFilter != "" {
		q.Set("type", p.typeFilter)
	}
	if p.sortKey != "" && p.sortKey != defaultSortKey {
		q.Set("sort", p.sortKey)
	}
	if p.recursive {
		q.Set("recursive", "true")
	}
	if p.pageSize > 0 && p.pageSize != defaultPageSize {
		q.Set("page_size", strconv.Itoa(p.pageSize))
	}
	if p.page > 1 {
		q.Set("page", strconv.Itoa(p.page))
	}
	if len(q) == 0 {
		return basePath
	}
	return basePath + "?" + q.Encode()
}

func buildBrowseURL(dir string, page int, include, exclude []string, typeFilter, sortKey string, recursive bool) string {
	return buildBrowseURLParams(browseURLParams{
		dir: dir, page: page,
		include: include, exclude: exclude,
		typeFilter: typeFilter, sortKey: sortKey, recursive: recursive,
	})
}

func buildTagURL(dir string, include, exclude []string, typeFilter, sortKey string, recursive bool) string {
	return buildBrowseURL(dir, 1, include, exclude, typeFilter, sortKey, recursive)
}

// buildFolderURL enters a directory in normal (non-recursive) browse mode
// while preserving the current tag/type/sort filters.
func buildFolderURL(dir string, include, exclude []string, typeFilter, sortKey string) string {
	basePath := "/"
	if dir != "" {
		basePath = "/" + dir
	}
	q := url.Values{}
	if len(include) > 0 {
		q.Set("tags", strings.Join(include, ","))
	}
	if len(exclude) > 0 {
		q.Set("exclude_tags", strings.Join(exclude, ","))
	}
	if typeFilter != "" {
		q.Set("type", typeFilter)
	}
	if sortKey != "" && sortKey != defaultSortKey {
		q.Set("sort", sortKey)
	}
	if len(q) == 0 {
		return basePath
	}
	return basePath + "?" + q.Encode()
}

func buildBreadcrumbs(dir string, include, exclude []string, typeFilter, sortKey string, recursive bool) ([]string, []string) {
	if dir == "" {
		return nil, nil
	}
	parts := strings.Split(dir, "/")
	outParts := make([]string, 0, len(parts))
	outLinks := make([]string, 0, len(parts))
	acc := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if acc == "" {
			acc = p
		} else {
			acc = acc + "/" + p
		}
		outParts = append(outParts, p)
		outLinks = append(outLinks, "/"+acc)
	}
	return outParts, outLinks
}

// ---------- template data ----------

type templateDir struct {
	Name           string
	Path           string
	Count          int
	RecursiveCount int
	Votes          int // 最热视图：账号 emoji 总票数
	Link           string
	Cover          string        // 封面图（优先 photo）
	Avatar         string        // 头像（account_info.profile_image）
	BG             string        // 背景：该账号最新一张图
	Nick           string        // 昵称（db users 表，可能为空）
	Date           string        // 最近更新日期（db users.last_modify，可能为空）
	Links          []AccountLink // 外部平台链接
}

type templateTag struct {
	Name        string
	Count       int
	IncludeLink string
	ExcludeLink string
}

type pageData struct {
	Mode        string
	Title       string
	Dir         string
	Parent      string
	Dirs        []templateDir
	Items       []Media
	Page        int
	PageSize    int
	Total       int
	TotalPages  int
	Tags        []templateTag
	IncludeTags []string
	ExcludeTags []string
	IncTagCSV   string
	ExcTagCSV   string
	TypeFilter  string
	SortKey     string
	Recursive   bool

	BreadParts []string
	BreadLinks []string
	Clusters   []Cluster
	Home       []HomeSection

	PrevURL       string
	NextURL       string
	UpURL         string
	SortNameURL   string
	SortTimeURL   string
	SortSizeURL   string
	SortRandomURL string
	TypeAllURL    string
	TypeImageURL  string
	TypeVideoURL  string
	RecursiveURL  string

	TagFilter string

	// 左导航 / 新后端逻辑
	ActiveTab    string         `json:"-"` // latest/hot/favorites/tags
	Votes        map[string]int `json:"-"` // 账号 -> emoji 总票数（最热用）
	AccountsJSON template.JS    `json:"-"` // 全部账号（收藏页 client 过滤用）
	MediaJSON    template.JS    `json:"-"` // 当前页媒体清单（灯箱导航用）
	TagMode      string         `json:"-"` // tags 页：accounts | images
	TagActive    string         `json:"-"` // 当前选中的 tag

	// OG / SEO（空则用默认值）
	OGTitle       string `json:"-"`
	OGDescription string `json:"-"`
	OGImage       string `json:"-"`
}

// lightboxItem 灯箱导航所需的最小媒体信息（序列化进页面）。
type lightboxItem struct {
	URL     string `json:"url"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Dir     string `json:"dir"`
	TweetID int64  `json:"tweet_id"`
}

func renderPage(w http.ResponseWriter, data pageData) {
	// 有媒体网格的页面内嵌当前页媒体清单，供前端灯箱翻页。
	if len(data.Items) > 0 {
		lb := make([]lightboxItem, 0, len(data.Items))
		for _, m := range data.Items {
			lb = append(lb, lightboxItem{URL: m.URL, Type: m.Type, Name: m.Name, Dir: m.Dir, TweetID: m.TweetID})
		}
		if js, err := json.Marshal(lb); err == nil {
			data.MediaJSON = template.JS(js)
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, data); err != nil {
		log.Printf("gallery: render template failed: %v", err)
	}
}

// ---------- handlers ----------

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	q := r.URL.Query()

	// 优先读 path（/:username/），fallback 到 ?dir= 兼容旧链接。
	dir := ""
	if u := r.PathValue("username"); u != "" {
		dir = u
		if rest := r.PathValue("rest"); rest != "" {
			dir = dir + "/" + rest
		}
		dir = normalizeDir(dir)
	} else {
		dir = normalizeDir(q.Get("dir"))
	}
	page := intParam(r, "page", 1, 1, 0)
	pageSize := intParam(r, "page_size", defaultPageSize, 1, 100)
	include := parseCSV(q.Get("tags"))
	exclude := parseCSV(q.Get("exclude_tags"))
	typeFilter := strings.ToLower(strings.TrimSpace(q.Get("type")))
	sortKey := strings.ToLower(strings.TrimSpace(q.Get("sort")))
	if sortKey == "" {
		sortKey = defaultSortKey
	}

	recStr := strings.ToLower(strings.TrimSpace(q.Get("recursive")))
	var recursive bool
	switch recStr {
	case "true":
		recursive = true
	case "false":
		recursive = false
	default:
		// Root shows all media like a normal image site; directories are
		// browsed non-recursively unless a tag filter is active.
		recursive = dir == "" || len(include) > 0 || len(exclude) > 0
	}

	tagMode := strings.ToLower(strings.TrimSpace(q.Get("tag_mode")))
	if tagMode == "" {
		tagMode = "all"
	}

	var candidates []*Media
	if !recursive && len(include) == 0 && len(exclude) == 0 {
		candidates = g.byDir[dir]
	} else {
		candidates = s.mediaUnder(g, dir)
	}

	// 远程源懒加载（临时调试，仅存内存）：本地没有该账号的数据时，
	// 按需从远端 API 拉取这一个账号的 json 到内存，重建索引后继续。
	if len(candidates) == 0 && dir != "" && g.RemoteBase() != "" {
		if _, err := g.FetchRemoteDoc(dir); err == nil {
			log.Printf("gallery: remote lazy-fetched %s (memory only)", dir)
			g.Scan() // 重建内存索引（含头像/标签）
			if !recursive && len(include) == 0 && len(exclude) == 0 {
				candidates = g.byDir[dir]
			} else {
				candidates = s.mediaUnder(g, dir)
			}
		} else {
			log.Printf("gallery: remote lazy-fetch %s skipped: %v", dir, err)
		}
	}

	list := make([]*Media, 0, len(candidates))
	for _, m := range candidates {
		if typeFilter != "" && !matchesTypeFilter(m, typeFilter) {
			continue
		}
		if len(include) > 0 {
			matched := 0
			for _, t := range include {
				if m.HasTag(t) {
					matched++
				}
			}
			if tagMode == "any" {
				if matched == 0 {
					continue
				}
			} else {
				if matched != len(include) {
					continue
				}
			}
		}
		if len(exclude) > 0 {
			excluded := false
			for _, t := range exclude {
				if m.HasTag(t) {
					excluded = true
					break
				}
			}
			if excluded {
				continue
			}
		}
		list = append(list, m)
	}

	switch sortKey {
	case "random":
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		rng.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
	default:
		sortMedia(list, sortKey)
	}

	total := len(list)
	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	if totalPages == 0 {
		page = 1
	} else if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	if start > total {
		start = total
	}

	items := make([]Media, 0, end-start)
	for _, m := range list[start:end] {
		items = append(items, *m)
	}

	parent := ""
	if dir != "" {
		parent = normalizeDir(path.Dir(dir))
	}
	var upURL string
	if dir != "" {
		upURL = buildBrowseURL(parent, 1, include, exclude, typeFilter, sortKey, recursive)
	}

	breadParts, breadLinks := buildBreadcrumbs(dir, include, exclude, typeFilter, sortKey, recursive)

	mode := "gallery"
	if dir == "" && len(include) == 0 && len(exclude) == 0 && typeFilter == "" {
		mode = "index"
	}

	if mode == "index" {
		// 首页：最新 / 最热 / 收藏 三块摘要，各 4 行；完整列表走 /latest、/hot、/favorites。
		renderPage(w, s.buildHome(g))
		return
	}

	data := pageData{
		Mode:          mode,
		Title:         "浏览",
		Dir:           dir,
		Parent:        parent,
		BreadParts:    breadParts,
		BreadLinks:    breadLinks,
		Items:         items,
		Page:          page,
		PageSize:      pageSize,
		Total:         total,
		TotalPages:    totalPages,
		IncludeTags:   include,
		ExcludeTags:   exclude,
		IncTagCSV:     strings.Join(include, ","),
		ExcTagCSV:     strings.Join(exclude, ","),
		TypeFilter:    typeFilter,
		SortKey:       sortKey,
		Recursive:     recursive,
		PrevURL:       buildBrowseURLParams(browseURLParams{dir: dir, page: page - 1, pageSize: pageSize, include: include, exclude: exclude, typeFilter: typeFilter, sortKey: sortKey, recursive: recursive}),
		NextURL:       buildBrowseURLParams(browseURLParams{dir: dir, page: page + 1, pageSize: pageSize, include: include, exclude: exclude, typeFilter: typeFilter, sortKey: sortKey, recursive: recursive}),
		UpURL:         upURL,
		SortNameURL:   buildBrowseURL(dir, 1, include, exclude, typeFilter, "name", recursive),
		SortTimeURL:   buildBrowseURL(dir, 1, include, exclude, typeFilter, "time", recursive),
		SortSizeURL:   buildBrowseURL(dir, 1, include, exclude, typeFilter, "size", recursive),
		SortRandomURL: buildBrowseURL(dir, 1, include, exclude, typeFilter, "random", recursive),
		TypeAllURL:    buildBrowseURL(dir, 1, include, exclude, "", sortKey, recursive),
		TypeImageURL:  buildBrowseURL(dir, 1, include, exclude, "image", sortKey, recursive),
		TypeVideoURL:  buildBrowseURL(dir, 1, include, exclude, "video", sortKey, recursive),
		RecursiveURL:  buildBrowseURL(dir, 1, include, exclude, typeFilter, sortKey, !recursive),
	}

	data.Dirs = s.buildTemplateDirs(g, dir, include, exclude, typeFilter, sortKey)
	data.Tags = s.buildTemplateTags(g, dir, include, exclude, typeFilter, sortKey, recursive)

	// OG / SEO：账号页用昵称 + 封面图
	if meta, ok := g.dbAccounts[dir]; ok && meta.Nick != "" {
		data.OGTitle = meta.Nick + " (@" + dir + ") · Gallery"
	} else if dir != "" {
		data.OGTitle = "@" + dir + " · Gallery"
	}
	if rec := g.recurs[dir]; rec > 0 {
		data.OGDescription = fmt.Sprintf("@%s 的 %d 张媒体（图片/视频）在线浏览。", dir, rec)
	}
	if cover := dirCover(g, dir); cover != "" {
		data.OGImage = cover
	}

	renderPage(w, data)
}

func (s *Server) handleClustersPage(w http.ResponseWriter, r *http.Request) {
	// 聚类页暂未完成：入口已从侧边栏移除，直接访问也回首页。
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	// 在现有实例上重扫：Replace 会保留 remoteBase / remoteDocs / dbAccounts，
	// 新建 Gallery 则会把远程源配置与 db 账号索引全部丢掉。
	g := s.current()
	if err := g.Scan(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 重新扫描后同样要把 per-image 标签回填
	g.AttachReactions(s.db)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleReact 处理单张媒体的赞/倒赞 emoji 反应（扁平 per-image 标签）。
// POST /react?media_id=<id>&emoji=👍|👎&voter=<token>
// voter 由前端生成并持久化（localStorage），用于聚合计数与单人切换。
func (s *Server) handleReact(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	g := s.current()
	mediaID := r.FormValue("media_id")
	emoji := r.FormValue("emoji")
	voter := r.FormValue("voter")
	if mediaID == "" || emoji == "" {
		http.Error(w, "missing media_id or emoji", http.StatusBadRequest)
		return
	}
	// 从内存索引取出 tweet_id，写入点赞记录以便溯源。
	var tweetID int64
	if m, ok := g.byID[mediaID]; ok {
		tweetID = m.TweetID
	}
	likes, dislikes, err := React(s.db, mediaID, emoji, voter, tweetID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 同步内存中的计数，使 /hot 与卡片上的赞/踩数即时生效（无需等 /rescan）。
	if m, ok := g.byID[mediaID]; ok {
		m.LikeCount = likes
		m.DislikeCount = dislikes
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"likes": likes, "dislikes": dislikes})
}

// accountList 返回顶层账号卡片（最新/最热/收藏/tag-账号 共用的右侧账号网格）。
func accountList(g *Gallery) []templateDir {
	names := g.dirs[""]
	out := make([]templateDir, 0, len(names))
	bgm := latestPhotoBG(g)
	for _, name := range names {
		meta := g.dbAccounts[name]
		out = append(out, templateDir{
			Name:           name,
			Path:           name,
			Count:          g.direct[name],
			RecursiveCount: g.recurs[name],
			Link:           buildFolderURL(name, nil, nil, "", ""),
			Cover:          dirCover(g, name),
			Avatar:         g.Avatar(name),
			BG:             bgm[name],
			Nick:           meta.Nick,
			Date:           formatDate(meta.LastModify),
		})
	}
	return out
}

// pageURL 返回把当前请求的 page 参数改成 page 后的 URL（用于账号翻页）。
func pageURL(r *http.Request, page int) string {
	q := r.URL.Query()
	if page <= 1 {
		q.Del("page")
	} else {
		q.Set("page", strconv.Itoa(page))
	}
	u := *r.URL
	u.RawQuery = q.Encode()
	return u.String()
}

// paginateDirs 对账号网格做翻页式分页，并把结果写回 pageData。
func (s *Server) paginateDirs(dirs []templateDir, r *http.Request, data *pageData) {
	const pageSize = 24
	total := len(dirs)
	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	page := intParam(r, "page", 1, 1, 100000)
	if page < 1 {
		page = 1
	}
	if totalPages > 0 && page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	if start > total {
		start = total
	}
	data.Dirs = dirs[start:end]
	data.Page = page
	data.Total = total
	data.TotalPages = totalPages
	if page > 1 {
		data.PrevURL = pageURL(r, page-1)
	}
	if page < totalPages {
		data.NextURL = pageURL(r, page+1)
	}
}

// dirCover 返回某账号目录的封面：优先选 photo，避免拿到视频破图；找不到则退回第一张。
func dirCover(g *Gallery, dir string) string {
	list := g.byDir[dir]
	if len(list) == 0 {
		return ""
	}
	for _, m := range list {
		if m.Type == "photo" {
			return m.URL
		}
	}
	return list[0].URL
}

// latestPhotoBG 返回每个账号最新一张 photo 的 URL，用作行/卡片背景。
func latestPhotoBG(g *Gallery) map[string]string {
	out := map[string]string{}
	latestT := map[string]time.Time{}
	for _, m := range g.media {
		if m.Type != "photo" {
			continue
		}
		if m.ModTime.After(latestT[m.Dir]) {
			latestT[m.Dir] = m.ModTime
			out[m.Dir] = m.URL
		}
	}
	return out
}

// HomeRow 首页摘要的一行（一个账号）。
type HomeRow struct {
	Name   string `json:"name"`
	Link   string `json:"link"`
	Cover  string `json:"cover"`
	Avatar string `json:"avatar"`
	BG     string `json:"bg"`
	Sub    string `json:"sub"`
}

// formatDate 把 db 的 last_modify 格式化为简短日期串（无则空串）。
func formatDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format("2006-01-02")
}

// HomeSection 首页的一个板块（最新 / 最热 / 收藏）。
type HomeSection struct {
	Title   string    `json:"title"`
	MoreURL string    `json:"more_url,omitempty"`
	Fav     bool      `json:"fav,omitempty"` // 收藏板块由前端按 localStorage 渲染
	Rows    []HomeRow `json:"rows"`
}

// buildHome 组装首页数据：最新 / 最热 各取前 4 个账号，收藏交给前端按 localStorage 渲染。
func (s *Server) buildHome(g *Gallery) pageData {
	const homeRows = 4
	votes := map[string]int{}
	latest := map[string]time.Time{}
	for _, m := range g.media {
		votes[m.Dir] += m.LikeCount
		if m.ModTime.After(latest[m.Dir]) {
			latest[m.Dir] = m.ModTime
		}
	}
	// db 提供的账号索引也参与「最新」排序：没有媒体的账号用 db 的 last_modify。
	for name, meta := range g.dbAccounts {
		if _, ok := latest[name]; !ok && !meta.LastModify.IsZero() {
			latest[name] = meta.LastModify
		}
	}
	names := append([]string(nil), g.dirs[""]...)

	hot := append([]string(nil), names...)
	sort.Slice(hot, func(i, j int) bool {
		if votes[hot[i]] != votes[hot[j]] {
			return votes[hot[i]] > votes[hot[j]]
		}
		return hot[i] < hot[j]
	})

	lat := append([]string(nil), names...)
	sort.Slice(lat, func(i, j int) bool {
		a, b := latest[lat[i]], latest[lat[j]]
		if !a.Equal(b) {
			return a.After(b)
		}
		return lat[i] < lat[j]
	})
	if len(hot) > homeRows {
		hot = hot[:homeRows]
	}
	if len(lat) > homeRows {
		lat = lat[:homeRows]
	}

	bgm := latestPhotoBG(g)
	mkRows := func(list []string, withVotes bool) []HomeRow {
		rows := make([]HomeRow, 0, len(list))
		for _, n := range list {
			meta := g.dbAccounts[n]
			name := n
			if meta.Nick != "" {
				name = meta.Nick
			}
			var sub string
			if rec := g.recurs[n]; rec > 0 {
				sub = fmt.Sprintf("@%s · 🖼 %d", n, rec)
				if withVotes {
					sub = fmt.Sprintf("@%s · 🔥 %d · 🖼 %d", n, votes[n], rec)
				}
			} else if d := formatDate(meta.LastModify); d != "" {
				sub = fmt.Sprintf("@%s · %s", n, d)
			} else {
				sub = "@" + n
			}
			rows = append(rows, HomeRow{Name: name, Link: "/" + n, Cover: dirCover(g, n), Avatar: g.Avatar(n), BG: bgm[n], Sub: sub})
		}
		return rows
	}

	js, _ := json.Marshal(accountList(g))
	return pageData{
		Mode:  "home",
		Title: "首页",
		Tags:  s.buildTemplateTags(g, "", nil, nil, "", defaultSortKey, false),
		Home: []HomeSection{
			{Title: "最新", MoreURL: "/latest", Rows: mkRows(lat, false)},
			{Title: "最热", MoreURL: "/hot", Rows: mkRows(hot, true)},
			{Title: "收藏", Fav: true},
		},
		AccountsJSON: template.JS(js),
	}
}

// handleLatest 完整的「最新」账号列表（原首页行为，翻页式）。
// 支持 q= 按用户名 / 昵称子串搜索（大小写不敏感）。
func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	accounts := s.buildTemplateDirs(g, "", nil, nil, "", defaultSortKey)
	if query := strings.TrimSpace(r.URL.Query().Get("q")); query != "" {
		accounts = filterAccounts(accounts, query)
	}
	var d pageData
	d.Mode = "index"
	d.Title = "最新"
	d.ActiveTab = "latest"
	d.TagFilter = strings.TrimSpace(r.URL.Query().Get("q"))
	d.Tags = s.buildTemplateTags(g, "", nil, nil, "", defaultSortKey, false)
	s.paginateDirs(accounts, r, &d)
	renderPage(w, d)
}

// filterAccounts 按用户名或昵称的子串（大小写不敏感）过滤账号卡片。
func filterAccounts(dirs []templateDir, query string) []templateDir {
	q := strings.ToLower(query)
	out := make([]templateDir, 0, len(dirs))
	for _, d := range dirs {
		if strings.Contains(strings.ToLower(d.Name), q) ||
			strings.Contains(strings.ToLower(d.Nick), q) {
			out = append(out, d)
		}
	}
	return out
}

// handleHot 按 reaction（点赞）总票数给账号排序（最热）。
// 票数来自 gallery.db 的 media_tags（见 mediatags.go），聚合到每个账号(Dir)。
// 注意：账号 tag（user_tags）与 per-image reaction 完全分离，这里只统计 reaction。
func (s *Server) handleHot(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	accounts := accountList(g)
	votes := map[string]int{}
	for _, m := range g.media {
		votes[m.Dir] += m.LikeCount
	}
	for i := range accounts {
		accounts[i].Votes = votes[accounts[i].Name]
	}
	sort.Slice(accounts, func(i, j int) bool {
		vi, vj := accounts[i].Votes, accounts[j].Votes
		if vi != vj {
			return vi > vj
		}
		return accounts[i].Name < accounts[j].Name
	})
	var data pageData
	s.paginateDirs(accounts, r, &data)
	data.Mode = "accounts"
	data.Title = "最热"
	data.Votes = votes
	data.ActiveTab = "hot"
	renderPage(w, data)
}

// handleFavorites 我的收藏：收藏列表存浏览器 localStorage，本页只把全部账号下发，
// 由前端按收藏名过滤渲染（与 twitter-pic-react 的本地偏好一致）。
func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	accounts := accountList(g)
	js, _ := json.Marshal(accounts)
	renderPage(w, pageData{
		Mode:         "favorites",
		Title:        "我的收藏",
		Dirs:         accounts,
		AccountsJSON: template.JS(js),
		ActiveTab:    "favorites",
	})
}

// handleTagsPage tags 页：无 tag 时显示标签云；选 tag 后按 mode 切换
// 「按账号」(列出带该 tag 的账号) 或 「按图片」(列出这些账号的媒体)。
func (s *Server) handleTagsPage(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	q := r.URL.Query()
	mode := strings.ToLower(strings.TrimSpace(q.Get("mode")))
	if mode != "images" {
		mode = "accounts"
	}
	tag := strings.TrimSpace(q.Get("tag"))
	filter := strings.ToLower(strings.TrimSpace(q.Get("q")))

	data := pageData{
		Mode:      "tags",
		Title:     "标签",
		TagMode:   mode,
		TagFilter: filter,
		ActiveTab: "tags",
	}

	if tag == "" {
		data.Dirs = s.buildTemplateDirs(g, "", nil, nil, "", defaultSortKey)
		data.Tags = s.buildTemplateTags(g, "", nil, nil, "", defaultSortKey, false)
		renderPage(w, data)
		return
	}

	// 收集带该 tag 的账号（账号级 tag，大小写不敏感）
	matched := []string{}
	for _, name := range g.dirs[""] {
		tags := g.accountTags[name]
		if tags == nil {
			tags = g.accountTags[strings.ToLower(name)]
		}
		for _, t := range tags {
			if strings.EqualFold(t, tag) {
				matched = append(matched, name)
				break
			}
		}
	}

	if mode == "accounts" {
		out := make([]templateDir, 0, len(matched))
		bgm := latestPhotoBG(g)
		for _, name := range matched {
			meta := g.dbAccounts[name]
			out = append(out, templateDir{
				Name:           name,
				Path:           name,
				Count:          g.direct[name],
				RecursiveCount: g.recurs[name],
				Link:           buildFolderURL(name, nil, nil, "", ""),
				Cover:          dirCover(g, name),
				Avatar:         g.Avatar(name),
				BG:             bgm[name],
				Nick:           meta.Nick,
				Date:           formatDate(meta.LastModify),
			})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		s.paginateDirs(out, r, &data)
		data.TagActive = tag
		data.Title = "标签 · " + tag
		data.Mode = "accounts"
		data.ActiveTab = "tags"
		renderPage(w, data)
		return
	}

	// 按图片：列出这些账号的全部媒体（分页），支持 type / sort 筛选
	set := map[string]bool{}
	for _, n := range matched {
		set[n] = true
	}
	typeFilter := strings.ToLower(strings.TrimSpace(q.Get("type")))
	sortKey := strings.ToLower(strings.TrimSpace(q.Get("sort")))
	if sortKey == "" {
		sortKey = defaultSortKey
	}
	var list []*Media
	for _, m := range g.media {
		if !set[m.Dir] {
			continue
		}
		if typeFilter != "" && !matchesTypeFilter(m, typeFilter) {
			continue
		}
		list = append(list, m)
	}
	switch sortKey {
	case "random":
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		rng.Shuffle(len(list), func(i, j int) { list[i], list[j] = list[j], list[i] })
	default:
		sortMedia(list, sortKey)
	}
	pageSize := intParam(r, "page_size", defaultPageSize, 1, 100)
	page := intParam(r, "page", 1, 1, 0)
	total := len(list)
	totalPages := 0
	if total > 0 {
		totalPages = (total + pageSize - 1) / pageSize
	}
	if totalPages == 0 {
		page = 1
	} else if page > totalPages {
		page = totalPages
	}
	start := (page - 1) * pageSize
	if start < 0 {
		start = 0
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	items := make([]Media, 0, end-start)
	for _, m := range list[start:end] {
		items = append(items, *m)
	}
	data.Items = items
	data.TagActive = tag
	data.Title = "标签 · " + tag
	data.Page = page
	data.Total = total
	data.TotalPages = totalPages
	data.Mode = "media"
	// 翻页/筛选链接：保留 tag / mode / q / type / sort / page_size
	//（此前翻页链接缺失，「按图片」第二页之后无法继续翻页）
	tagsImagesURL := func(p int, tf, sk string) string {
		qv := url.Values{}
		qv.Set("tag", tag)
		qv.Set("mode", "images")
		if filter != "" {
			qv.Set("q", filter)
		}
		if tf != "" {
			qv.Set("type", tf)
		}
		if sk != "" && sk != defaultSortKey {
			qv.Set("sort", sk)
		}
		if pageSize != defaultPageSize {
			qv.Set("page_size", strconv.Itoa(pageSize))
		}
		if p > 1 {
			qv.Set("page", strconv.Itoa(p))
		}
		return "/tags?" + qv.Encode()
	}
	if page > 1 {
		data.PrevURL = tagsImagesURL(page-1, typeFilter, sortKey)
	}
	if page < totalPages {
		data.NextURL = tagsImagesURL(page+1, typeFilter, sortKey)
	}
	data.TypeAllURL = tagsImagesURL(1, "", sortKey)
	data.TypeImageURL = tagsImagesURL(1, "image", sortKey)
	data.TypeVideoURL = tagsImagesURL(1, "video", sortKey)
	data.SortNameURL = tagsImagesURL(1, typeFilter, "name")
	data.SortTimeURL = tagsImagesURL(1, typeFilter, "time")
	data.SortSizeURL = tagsImagesURL(1, typeFilter, "size")
	data.SortRandomURL = tagsImagesURL(1, typeFilter, "random")
	data.TypeFilter = typeFilter
	data.SortKey = sortKey
	renderPage(w, data)
}

// ---------- template data builders ----------

func (s *Server) buildTemplateDirs(g *Gallery, dir string, include, exclude []string, typeFilter, sortKey string) []templateDir {
	names := g.dirs[dir]
	out := make([]templateDir, 0, len(names))
	bgm := latestPhotoBG(g)
	for _, name := range names {
		p := joinDir(dir, name)
		meta := g.dbAccounts[name]
		out = append(out, templateDir{
			Name:           name,
			Path:           p,
			Count:          g.direct[p],
			RecursiveCount: g.recurs[p],
			Link:           buildFolderURL(p, include, exclude, typeFilter, sortKey),
			Cover:          dirCover(g, p),
			Avatar:         g.Avatar(p),
			BG:             bgm[p],
			Nick:           meta.Nick,
			Date:           formatDate(meta.LastModify),
			Links:          s.links[name],
		})
	}
	return out
}

func (s *Server) buildTemplateTags(g *Gallery, dir string, include, exclude []string, typeFilter, sortKey string, recursive bool) []templateTag {
	counts := map[string]int{}
	if dir == "" {
		// 左导航/标签云：统计“多少个账号拥有该 tag”，而非 media 数。
		for t, c := range g.accountTagCounts {
			counts[t] = c
		}
	} else {
		for _, m := range s.mediaUnder(g, dir) {
			seen := map[string]bool{}
			for _, t := range m.AllTags() {
				if !seen[t] {
					seen[t] = true
					counts[t]++
				}
			}
		}
	}

	list := make([]templateTag, 0, len(counts))
	for t, c := range counts {
		list = append(list, templateTag{Name: t, Count: c})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Count != list[j].Count {
			return list[i].Count > list[j].Count
		}
		return list[i].Name < list[j].Name
	})

	for i := range list {
		t := list[i].Name
		list[i].IncludeLink = buildTagURL(dir, addTag(include, t), exclude, typeFilter, sortKey, recursive)
		list[i].ExcludeLink = buildTagURL(dir, include, addTag(exclude, t), typeFilter, sortKey, recursive)
	}
	return list
}

// ---------- media proxy ----------
