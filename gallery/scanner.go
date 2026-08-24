package gallery

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Media represents a single remote image/video entry parsed from a
// <account>.json.gz timeline.
type Media struct {
	ID          string    `json:"id"`
	Path        string    `json:"path"` // virtual path: account/urlbase_0001.jpg
	Dir         string    `json:"dir"`  // virtual directory: account
	Name        string    `json:"name"`
	Type        string    `json:"type"` // photo / video / animated_gif
	Ext         string    `json:"ext"`
	Size        int64     `json:"size"` // unknown for remote media
	ModTime     time.Time `json:"mod_time"`
	Tags         []string `json:"tags"`
	DirTags      []string `json:"dir_tags,omitempty"`
	PerImageTags []string `json:"per_image_tags,omitempty"` // 扁平 per-image 标签（与账号 tag 完全分离）
	LikeCount    int      `json:"like_count"`
	DislikeCount int      `json:"dislike_count"`
	URL          string   `json:"url"`          // proxy URL on this server
	OriginalURL  string   `json:"original_url"` // pbs.twimg.com / video.twimg.com
}

// IsVideo reports whether the media is a video or animated gif.
func (m *Media) IsVideo() bool {
	return m.Type == "video" || m.Type == "animated_gif"
}

// AllTags returns account tags plus directory-derived tags (unique, sorted).
func (m *Media) AllTags() []string {
	set := map[string]struct{}{}
	for _, t := range m.Tags {
		set[t] = struct{}{}
	}
	for _, t := range m.DirTags {
		set[t] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// HasTag reports whether the media has a tag (account or directory-derived).
func (m *Media) HasTag(tag string) bool {
	for _, t := range m.Tags {
		if t == tag {
			return true
		}
	}
	for _, t := range m.DirTags {
		if t == tag {
			return true
		}
	}
	return false
}

// DirInfo describes one subdirectory in a directory listing.
type DirInfo struct {
	Name           string `json:"name"`
	Path           string `json:"path"`
	Count          int    `json:"count"`           // media directly inside this directory
	RecursiveCount int    `json:"recursive_count"` // media in the whole subtree
}

// Gallery is an in-memory index over all json.gz timelines found in root.
type Gallery struct {
	root        string
	accountTags map[string][]string // username -> positive tags from SQLite

	media  []*Media
	byID   map[string]*Media
	byURL  map[string]*Media
	byDir  map[string][]*Media // direct children only
	dirs   map[string][]string // parent dir -> immediate subdir names
	direct map[string]int      // dir -> count of media directly inside
	recurs map[string]int      // dir -> count of media in subtree
	tags   map[string]int      // tag -> media count
	// accountTagCounts: tag -> 账号数（左导航/标签云统计“哪些账号拥有该 tag”用，与 media 数分离）
	accountTagCounts map[string]int
	// mediaBase: pbs.twimg.com 媒体的服务前缀（endpoint B / twimg），path 形式；
	// 为空则图片直链原始 URL。
	mediaBase string
}

var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".gif": true,
	".webp": true, ".bmp": true, ".svg": true, ".avif": true, ".ico": true,
}

var videoExts = map[string]bool{
	".mp4": true, ".webm": true, ".mov": true, ".m4v": true,
	".mkv": true, ".avi": true, ".ts": true, ".mts": true,
}

func mediaID(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:8])
}

// normalizeDir converts a user supplied directory path into canonical form:
// "" for root, forward slashes, no leading/trailing slashes.
func normalizeDir(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" || dir == "." || dir == "/" {
		return ""
	}
	dir = path.Clean(filepath.ToSlash(dir))
	dir = strings.Trim(dir, "/")
	if dir == "." || dir == ".." || dir == "" {
		return ""
	}
	if strings.HasPrefix(dir, "../") || strings.Contains(dir, "/../") {
		return ""
	}
	return dir
}

func joinDir(base, name string) string {
	if base == "" {
		return name
	}
	return base + "/" + name
}

func uniqueSorted(in []string) []string {
	set := map[string]struct{}{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s != "" {
			set[s] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// NewGallery creates a gallery index for the given json.gz directory.
func NewGallery(root string, accountTags map[string][]string) *Gallery {
	if accountTags == nil {
		accountTags = map[string][]string{}
	}
	// 统计每个 tag 被多少个账号拥有（左导航/标签云用）。
	accountTagCounts := map[string]int{}
	for _, tags := range accountTags {
		seen := map[string]bool{}
		for _, t := range tags {
			if !seen[t] {
				seen[t] = true
				accountTagCounts[t]++
			}
		}
	}
	return &Gallery{
		root:             root,
		accountTags:      accountTags,
		byID:             map[string]*Media{},
		byURL:            map[string]*Media{},
		byDir:            map[string][]*Media{},
		dirs:             map[string][]string{},
		direct:           map[string]int{},
		recurs:           map[string]int{},
		tags:             map[string]int{},
		accountTagCounts: accountTagCounts,
		mediaBase:         deriveMediaBase(),
	}
}

func (g *Gallery) Root() string { return g.root }

// serveURL 根据 mediaBase 决定媒体 URL：pbs.twimg.com 走 endpoint B（path 形式），
// 其他域名直链原始 URL（video.twimg.com 等直接加载，无需反代）。
func serveURL(mediaBase, raw string) string {
	if mediaBase == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	// 只有 pbs.twimg.com 走 twimg 反代；video.twimg.com 等直链。
	if u.Host != "pbs.twimg.com" {
		return raw
	}
	return mediaBase + u.Path + func() string {
		if u.RawQuery != "" {
			return "?" + u.RawQuery
		}
		return ""
	}()
}

// deriveMediaBase 返回 pbs.twimg.com 媒体的服务前缀（endpoint B / twimg 反代）。
// 优先 GALLERY_MEDIA_BASE，否则复用 TWIMG_ADDR；都为空则返回 ""（图片直链原始 URL）。
func deriveMediaBase() string {
	base := os.Getenv("GALLERY_MEDIA_BASE")
	if base == "" {
		base = os.Getenv("TWIMG_ADDR")
	}
	if base == "" {
		return ""
	}
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	return strings.TrimRight(base, "/")
}

type gzTimelineEntry struct {
	URL  string `json:"url"`
	Date string `json:"date"`
	Type string `json:"type"`
}

type gzAccountInfo struct {
	Name string `json:"name"`
	Nick string `json:"nick"`
}

type gzDocument struct {
	AccountInfo gzAccountInfo     `json:"account_info"`
	Timeline    []gzTimelineEntry `json:"timeline"`
}

// parseJSONGz reads a .json.gz metadata file produced by the twitter pipeline.
func parseJSONGz(fp string) (gzDocument, error) {
	var doc gzDocument
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

	err = json.NewDecoder(zr).Decode(&doc)
	return doc, err
}

// inferMediaExtType returns the file extension and canonical media type for a
// timeline entry. Only twimg.com URLs are accepted.
func inferMediaExtType(te gzTimelineEntry) (ext, typ string) {
	raw := strings.TrimSpace(te.URL)
	if raw == "" {
		return "", ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	host := strings.ToLower(u.Hostname())
	if host != "pbs.twimg.com" && host != "video.twimg.com" {
		return "", ""
	}

	originalType := strings.ToLower(strings.TrimSpace(te.Type))
	if host == "video.twimg.com" || originalType == "video" || originalType == "animated_gif" {
		ext = strings.ToLower(path.Ext(u.Path))
		if !videoExts[ext] {
			ext = ".mp4"
		}
		typ = originalType
		if typ != "animated_gif" {
			typ = "video"
		}
		return ext, typ
	}

	ext = strings.ToLower(path.Ext(u.Path))
	if !imageExts[ext] {
		format := strings.ToLower(u.Query().Get("format"))
		if imageExts["."+format] {
			ext = "." + format
		} else {
			ext = ".jpg"
		}
	}
	typ = originalType
	if typ != "photo" {
		typ = "photo"
	}
	return ext, typ
}

func parseFlexibleTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	layouts := []string{
		time.RFC3339Nano,
		time.RFC3339,
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02",
	}
	for _, layout := range layouts {
		if t, err := time.ParseInLocation(layout, s, time.UTC); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Scan walks the json.gz directory and rebuilds the in-memory index.
func (g *Gallery) Scan() error {
	info, err := os.Stat(g.root)
	if err != nil {
		return fmt.Errorf("json dir %s is not accessible: %w", g.root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("json dir %s is not a directory", g.root)
	}

	next := NewGallery(g.root, g.accountTags)
	mediaByDir := map[string][]*Media{}
	subdirs := map[string]map[string]bool{}

	entries, err := os.ReadDir(g.root)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), ".json.gz") {
			continue
		}

		username := strings.TrimSuffix(name, ".json.gz")
		fp := filepath.Join(g.root, name)

		doc, err := parseJSONGz(fp)
		if err != nil || len(doc.Timeline) == 0 {
			continue
		}

		account := strings.TrimSpace(doc.AccountInfo.Name)
		if account == "" {
			account = username
		}
		dir := normalizeDir(account)
		if dir == "" {
			dir = account
		}

		tags := next.accountTags[account]
		if len(tags) == 0 {
			tags = next.accountTags[username]
		}
		tags = uniqueSorted(tags)
		dirTags := uniqueSorted([]string{dir})

		for i := range doc.Timeline {
			te := doc.Timeline[i]
			ext, typ := inferMediaExtType(te)
			if ext == "" || typ == "" {
				continue
			}

			// Derive a stable display name from the URL basename; tweet_id is not needed.
			base := fmt.Sprintf("%s_%04d", dir, i)
			if u, err := url.Parse(te.URL); err == nil {
				if b := path.Base(u.Path); b != "" && b != "." && b != "/" {
					b = strings.TrimSuffix(b, path.Ext(b))
					if b != "" {
						base = b
					}
				}
			}
			vname := fmt.Sprintf("%s_%04d%s", base, i, ext)
			vpath := dir + "/" + vname
			id := mediaID(dir + "\x00" + te.URL)

			m := &Media{
				ID:          id,
				Path:        vpath,
				Dir:         dir,
				Name:        vname,
				Type:        typ,
				Ext:         ext,
				ModTime:     parseFlexibleTime(te.Date),
				Tags:        tags,
				DirTags:     dirTags,
				URL:         serveURL(next.mediaBase, te.URL),
				OriginalURL: te.URL,
			}

			next.byID[m.ID] = m
			next.byURL[te.URL] = m
			next.media = append(next.media, m)
			mediaByDir[dir] = append(mediaByDir[dir], m)

			if subdirs[""] == nil {
				subdirs[""] = map[string]bool{}
			}
			subdirs[""][dir] = true
		}
	}

	// Sort media in every directory by name (case-insensitive).
	for dir := range mediaByDir {
		items := mediaByDir[dir]
		sort.Slice(items, func(i, j int) bool {
			return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
		})
		next.byDir[dir] = items
		next.direct[dir] = len(items)
	}

	// Compute recursive counts and tag counts.
	// Tag cloud counts only real account-level tags (m.Tags), not directory-derived
	// account names (m.DirTags) — those are reachable via the account list instead.
	for _, m := range next.media {
		next.recurs[m.Dir]++
		for _, t := range m.Tags {
			next.tags[t]++
		}
	}
	next.recurs[""] = len(next.media)

	// Convert subdir map to sorted slice.
	for parent, set := range subdirs {
		names := make([]string, 0, len(set))
		for name := range set {
			names = append(names, name)
		}
		sort.Strings(names)
		next.dirs[parent] = names
	}

	g.Replace(next)
	return nil
}

// Replace swaps the index contents under the caller's lock.
func (g *Gallery) Replace(next *Gallery) {
	*g = *next
}
