package gallery

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/Hana-ame/twitter-pic-go/tags"
)

func openTagStore(path string) (*tags.Store, bool, error) {
	if path == "" {
		return nil, false, fmt.Errorf("GALLERY_DB 未配置")
	}
	if _, err := os.Stat(path); err != nil {
		return nil, false, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.Clean(path)+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_txlock=immediate")
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

func handleTagCloud(w http.ResponseWriter, r *http.Request, cfg config) {
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	var cloud []tags.Count
	if cfg.tags != nil {
		cloud = cfg.tags.Cloud(limit, true)
	}
	if cloud == nil {
		cloud = []tags.Count{}
	}
	writeJSON(w, cloud)
}

func handleTagUsers(w http.ResponseWriter, r *http.Request, cfg config) {
	tag := strings.TrimSpace(r.PathValue("tag"))
	if tag == "" || len(tag) > 64 {
		http.Error(w, "bad tag", http.StatusBadRequest)
		return
	}
	limit := 50
	page := 1
	offset := 0

	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if v := r.URL.Query().Get("page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			page = n
			if limit > 0 {
				offset = (page - 1) * limit
			}
		}
	}
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			offset = n
		}
	}

	if cfg.tags == nil {
		writeJSON(w, map[string]any{
			"tag":   tag,
			"count": 0,
			"total": 0,
			"page":  page,
			"limit": limit,
			"users": []string{},
		})
		return
	}

	users, total, err := cfg.tags.UsersForTagPaged(tag, limit, offset, true)
	if err != nil {
		http.Error(w, "query tags: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if cfg.vis != nil {
		users = cfg.vis.filter(users)
	}
	if users == nil {
		users = []string{}
	}

	writeJSON(w, map[string]any{
		"tag":   tag,
		"count": len(users),
		"total": total,
		"page":  page,
		"limit": limit,
		"users": users,
	})
}

func handleGetAccountTags(w http.ResponseWriter, r *http.Request, cfg config) {
	out := map[string]map[string]int{}
	for _, u := range strings.Split(r.URL.Query().Get("keys"), ",") {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		if cfg.vis.hidden(u) {
			continue
		}
		out[u] = cfg.tags.Weights(u)
	}
	writeJSON(w, out)
}

func handlePostAccountTag(w http.ResponseWriter, r *http.Request, cfg config) {
	if ip, banned := cfg.bans.Decide(r); banned {
		ipban.WriteDenied(w, ip)
		return
	}
	if cfg.tags == nil || !cfg.writable {
		http.Error(w, "tags disabled", http.StatusServiceUnavailable)
		return
	}
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
		Key  string `json:"key"`
		Tag  string `json:"tag"`
		D    *int   `json:"d"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	user := firstNonEmpty(strings.TrimSpace(req.User), strings.TrimSpace(req.Key))
	tag := sanitizeTag(req.Tag)
	if user == "" || tag == "" || !safeName(user) || req.D == nil {
		http.Error(w, "user, tag and d required", http.StatusBadRequest)
		return
	}
	if *req.D < -1 || *req.D > 1 {
		http.Error(w, "d must be -1, 0 or 1", http.StatusBadRequest)
		return
	}
	d := *req.D
	if cfg.vis.hidden(user) {
		http.NotFound(w, r)
		return
	}
	err := cfg.tags.CastVotes(user, ip, map[string]int{tag: d}, r.UserAgent())
	if err != nil {
		log.Printf("gallery: POST tag %s %q=%d ip=%q: %v", user, tag, d, ip, err)
		http.Error(w, "write failed", http.StatusInternalServerError)
		return
	}
	cfg.home.invalidate()
	writeJSON(w, map[string]any{"user": user, "tags": cfg.tags.Weights(user)})
}
