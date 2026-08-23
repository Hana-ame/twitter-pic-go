package gallery

import (
	"database/sql"
	"embed"
	"encoding/json"
	"html/template"
	"io"
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

var pageTemplate = template.Must(template.ParseFS(templateFS, "templates/index.html"))

// Server holds the gallery index and access history.
type Server struct {
	mu          sync.RWMutex
	gallery     *Gallery
	history     *HistoryStore
	db          *sql.DB
	accountTags map[string][]string
	imageProxy  string
	videoProxy  string
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

	g := NewGallery(jsonDir, accountTags)
	if err := g.Scan(); err != nil {
		log.Fatalf("gallery: initial scan failed: %v", err)
	}
	log.Printf("gallery: indexed %d media entries from json.gz under %s", len(g.media), jsonDir)

	db := openDB(dbPath)

	s := &Server{
		gallery:     g,
		history:     NewHistoryStore(db, 2000),
		db:          db,
		accountTags: accountTags,
		imageProxy:  envOr("GALLERY_IMAGE_PROXY", ""),
		videoProxy:  envOr("GALLERY_VIDEO_PROXY", ""),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /", s.handleBrowse)
	mux.HandleFunc("GET /tags", s.handleTagsPage)
	mux.HandleFunc("GET /history", s.handleHistoryPage)
	mux.HandleFunc("GET /recommendations", s.handleRecommendationsPage)
	mux.HandleFunc("GET /clusters", s.handleClustersPage)
	mux.HandleFunc("POST /rescan", s.handleRescan)
	mux.HandleFunc("GET /proxy", s.handleProxy)

	log.Printf("gallery: server listening on %s (json dir: %s)", addr, jsonDir)
	if err := http.ListenAndServe(addr, logRequests(mux)); err != nil {
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

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		if strings.HasPrefix(r.URL.Path, "/proxy") || r.Method == http.MethodPost {
			log.Printf("gallery: %s %s %s", r.Method, r.URL.Path, time.Since(start))
		}
	})
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

func buildBrowseURL(dir string, page int, include, exclude []string, typeFilter, sortKey string, recursive bool) string {
	q := url.Values{}
	if dir != "" {
		q.Set("dir", dir)
	}
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
	if recursive {
		q.Set("recursive", "true")
	}
	if page > 1 {
		q.Set("page", strconv.Itoa(page))
	}
	if len(q) == 0 {
		return "/"
	}
	return "/?" + q.Encode()
}

func buildTagURL(dir string, include, exclude []string, typeFilter, sortKey string, recursive bool) string {
	return buildBrowseURL(dir, 1, include, exclude, typeFilter, sortKey, recursive)
}

// buildFolderURL enters a directory in normal (non-recursive) browse mode
// while preserving the current tag/type/sort filters.
func buildFolderURL(dir string, include, exclude []string, typeFilter, sortKey string) string {
	q := url.Values{}
	if dir != "" {
		q.Set("dir", dir)
	}
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
	q.Set("recursive", "false")
	return "/?" + q.Encode()
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
		outLinks = append(outLinks, buildBrowseURL(acc, 1, include, exclude, typeFilter, sortKey, recursive))
	}
	return outParts, outLinks
}

// ---------- template data ----------

type templateDir struct {
	Name           string
	Path           string
	Count          int
	RecursiveCount int
	Link           string
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

	BreadParts      []string
	BreadLinks      []string
	History         []HistoryEntry
	Recommendations []Media
	Clusters        []Cluster

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
}

func renderPage(w http.ResponseWriter, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTemplate.Execute(w, data); err != nil {
		log.Printf("gallery: render template failed: %v", err)
	}
}

// ---------- handlers ----------

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	q := r.URL.Query()

	dir := normalizeDir(q.Get("dir"))
	page := intParam(r, "page", 1, 1, 0)
	pageSize := intParam(r, "page_size", 10, 1, 100)
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
		PrevURL:       buildBrowseURL(dir, page-1, include, exclude, typeFilter, sortKey, recursive),
		NextURL:       buildBrowseURL(dir, page+1, include, exclude, typeFilter, sortKey, recursive),
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

	renderPage(w, data)
}

func (s *Server) handleTagsPage(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	q := r.URL.Query()
	dir := normalizeDir(q.Get("dir"))
	filter := strings.ToLower(strings.TrimSpace(q.Get("q")))

	data := pageData{
		Mode:      "tags",
		Title:     "标签",
		Dir:       dir,
		TagFilter: filter,
	}
	data.Dirs = s.buildTemplateDirs(g, "", nil, nil, "", defaultSortKey)
	data.Tags = s.buildTemplateTags(g, dir, nil, nil, "", defaultSortKey, false)
	renderPage(w, data)
}

func (s *Server) handleHistoryPage(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	limit := intParam(r, "limit", 100, 1, 500)
	entries := s.history.Recent(limit)
	out := make([]HistoryEntry, 0, len(entries))
	for i := len(entries) - 1; i >= 0; i-- {
		out = append(out, entries[i])
	}
	renderPage(w, pageData{
		Mode:    "history",
		Title:   "访问历史",
		History: out,
		Dirs:    s.buildTemplateDirs(g, "", nil, nil, "", defaultSortKey),
		Tags:    s.buildTemplateTags(g, "", nil, nil, "", defaultSortKey, false),
	})
}

func (s *Server) handleRecommendationsPage(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	limit := intParam(r, "limit", 30, 1, 100)
	renderPage(w, pageData{
		Mode:            "recommendations",
		Title:           "推荐",
		Recommendations: g.Recommend(s.history, limit),
		Dirs:            s.buildTemplateDirs(g, "", nil, nil, "", defaultSortKey),
		Tags:            s.buildTemplateTags(g, "", nil, nil, "", defaultSortKey, false),
	})
}

func (s *Server) handleClustersPage(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	k := intParam(r, "k", 8, 1, 30)
	itemLimit := intParam(r, "item_limit", 12, 1, 100)
	renderPage(w, pageData{
		Mode:     "clusters",
		Title:    "聚类",
		Clusters: g.Clusters(k, itemLimit),
		Dirs:     s.buildTemplateDirs(g, "", nil, nil, "", defaultSortKey),
		Tags:     s.buildTemplateTags(g, "", nil, nil, "", defaultSortKey, false),
	})
}

func (s *Server) handleRescan(w http.ResponseWriter, r *http.Request) {
	root := s.current().Root()
	next := NewGallery(root, s.accountTags)
	if err := next.Scan(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.swap(next)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// ---------- template data builders ----------

func (s *Server) buildTemplateDirs(g *Gallery, dir string, include, exclude []string, typeFilter, sortKey string) []templateDir {
	names := g.dirs[dir]
	out := make([]templateDir, 0, len(names))
	for _, name := range names {
		p := joinDir(dir, name)
		out = append(out, templateDir{
			Name:           name,
			Path:           p,
			Count:          g.direct[p],
			RecursiveCount: g.recurs[p],
			Link:           buildFolderURL(p, include, exclude, typeFilter, sortKey),
		})
	}
	return out
}

func (s *Server) buildTemplateTags(g *Gallery, dir string, include, exclude []string, typeFilter, sortKey string, recursive bool) []templateTag {
	counts := map[string]int{}
	if dir == "" {
		// Use the precomputed global tag counts for the sidebar.
		for t, c := range g.tags {
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

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	g := s.current()
	raw := r.URL.Query().Get("url")
	if raw == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}

	var m *Media
	if id := r.URL.Query().Get("id"); id != "" {
		m = g.byID[id]
		if m == nil || m.OriginalURL != raw {
			http.NotFound(w, r)
			return
		}
	} else {
		m = g.byURL[raw]
	}
	if m == nil {
		http.NotFound(w, r)
		return
	}

	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	host := strings.ToLower(u.Hostname())
	if host != "pbs.twimg.com" && host != "video.twimg.com" {
		http.Error(w, "forbidden host", http.StatusForbidden)
		return
	}

	upstream := raw
	switch host {
	case "pbs.twimg.com":
		if s.imageProxy != "" {
			upstream = replaceProxyHost(raw, s.imageProxy)
		}
	case "video.twimg.com":
		if s.videoProxy != "" {
			upstream = replaceProxyHost(raw, s.videoProxy)
		}
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream, nil)
	if err != nil {
		http.Error(w, "bad url", http.StatusBadRequest)
		return
	}
	if rng := r.Header.Get("Range"); rng != "" {
		req.Header.Set("Range", rng)
	}
	if inm := r.Header.Get("If-None-Match"); inm != "" {
		req.Header.Set("If-None-Match", inm)
	}
	if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		req.Header.Set("If-Modified-Since", ims)
	}
	ua := r.Header.Get("User-Agent")
	if ua == "" {
		ua = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/120 Safari/537.36"
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Referer", "https://x.com/")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Record access history. Only real GET views with a successful upstream
	// response count.
	if r.Method == http.MethodGet && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		s.history.Record(HistoryEntry{
			MediaID:   m.ID,
			Path:      m.Path,
			URL:       m.URL,
			Tags:      m.AllTags(),
			ViewedAt:  time.Now(),
			IP:        clientIP(r),
			UserAgent: r.UserAgent(),
		})
	}

	copyHeader(w, resp, "Content-Type")
	copyHeader(w, resp, "Content-Length")
	copyHeader(w, resp, "Content-Range")
	copyHeader(w, resp, "Accept-Ranges")
	copyHeader(w, resp, "Last-Modified")
	copyHeader(w, resp, "ETag")
	copyHeader(w, resp, "Cache-Control")
	copyHeader(w, resp, "Content-Encoding")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(resp.StatusCode)

	if r.Method == http.MethodGet {
		_, _ = io.Copy(w, resp.Body)
	}
}

func copyHeader(w http.ResponseWriter, resp *http.Response, name string) {
	if v := resp.Header.Get(name); v != "" {
		w.Header().Set(name, v)
	}
}

func replaceProxyHost(raw, proxyBase string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	p, err := url.Parse(proxyBase)
	if err != nil || p.Scheme == "" || p.Host == "" {
		return raw
	}
	u.Scheme = p.Scheme
	u.Host = p.Host
	return u.String()
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		if len(parts) > 0 {
			return strings.TrimSpace(parts[0])
		}
	}
	host := r.RemoteAddr
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	return host
}
