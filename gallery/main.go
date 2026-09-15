// Command gallery is a standalone, server-rendered gallery for twitter-pic.
//
// 账号页把 json 原文嵌进 HTML，前端（static/app.js）做网格分页、TikTok 式竖滑全屏
// 与赞/踩/喜欢；赞踩计数用本地 JSON 文件持久化。没有全局 media 索引。
package gallery

import (
	"compress/gzip"
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
)

//go:embed templates/*.html
var tmplFS embed.FS

//go:embed static
var staticFS embed.FS

var templates = template.Must(template.New("").ParseFS(tmplFS, "templates/*.html"))

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
}

// acctItem 主页账号卡片（首字母头像色相与服务端/前端同算法）。
type acctItem struct {
	Name    string
	Initial string
	Hue     int
}

type homeData struct {
	Total     int
	Preview   []acctItem
	NamesJSON template.JS
	MediaBase string // 前端据此重写 pbs.twimg.com 图片域名（同 mediaURL 规则）
}

type pageData struct {
	Title   string
	Home    *homeData
	Account *accountPage
	AppJS   template.JS
	HomeJS  template.JS
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
	pageSize      int
	reactionsFile string
}

func Run(addr string) {
	if addr == "" {
		addr = envOr("GALLERY_ADDR", ":8090")
	}
	cfg := config{
		addr:          addr,
		jsonDir:       envOr("GALLERY_JSON_DIR", "."),
		mediaBase:     strings.TrimRight(envOr("GALLERY_MEDIA_BASE", ""), "/"),
		pageSize:      envIntOr("GALLERY_PAGE_SIZE", defaultPageSize),
		reactionsFile: envOr("GALLERY_REACTIONS_FILE", "./reactions.json"),
	}
	if cfg.pageSize <= 0 {
		cfg.pageSize = defaultPageSize
	}

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
	preview := make([]acctItem, 0, previewN)
	for _, n := range names[:min(len(names), previewN)] {
		initial := "?"
		if n != "" {
			initial = strings.ToUpper(n[:1])
		}
		preview = append(preview, acctItem{Name: n, Initial: initial, Hue: hueOf(n)})
	}
	namesJSON, err := json.Marshal(names)
	if err != nil {
		http.Error(w, "marshal names: "+err.Error(), http.StatusInternalServerError)
		return
	}
	render(w, "home.html", pageData{
		Title:  "首页",
		Home:   &homeData{Total: len(names), Preview: preview, NamesJSON: template.JS(namesJSON), MediaBase: cfg.mediaBase},
		HomeJS: homeJS,
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
	}
	render(w, "account.html", pageData{Title: acc.Name, Account: acc, AppJS: appJS})
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
