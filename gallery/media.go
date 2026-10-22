package gallery

import (
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type accountInfo struct {
	Name          string `json:"name"`
	Nick          string `json:"nick"`
	ProfileImage  string `json:"profile_image"`
	ProfileBanner string `json:"profile_banner"`
}

type timelineEntry struct {
	URL     string `json:"url"`
	Date    string `json:"date"`
	TweetID int64  `json:"tweet_id"`
	Type    string `json:"type"`
	Cover   string `json:"cover"`
}

type document struct {
	AccountInfo accountInfo     `json:"account_info"`
	Timeline    []timelineEntry `json:"timeline"`
}

// defaultMediaBase 媒体与视频封面唯一 CDN 入口：https://pbs.moonchan.xyz（直接替换 pbs.twimg.com host）。
const defaultMediaBase = "https://pbs.moonchan.xyz"

// defaultVideoBase 视频直连代理域名：https://twimg.moonchan.xyz（直接替换 video.twimg.com host）。
const defaultVideoBase = "https://twimg.moonchan.xyz"

// defaultVideoEndpointPrefix 点开视频时指向的 Go 后端专属视频处理端点。
// 该端点接收视频 path+query 后，将 host 替换为 twimg.moonchan.xyz 并 302 直连（2026-10-23 起无广告跳转）。
const defaultVideoEndpointPrefix = "/api/twitter/v2/video"

func resolveOuoPrefix() string {
	if v, ok := os.LookupEnv("GALLERY_VIDEO_ENDPOINT"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimRight(strings.TrimSpace(v), "/")
	}
	return defaultVideoEndpointPrefix
}

// buildVideoEndpointURL 将原始视频地址（如 https://video.twimg.com/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/xxx.mp4?tag=12）
// 转换为 Go 后端视频专属端点链接：
//   /api/twitter/v2/video/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/xxx.mp4?tag=12
func buildVideoEndpointURL(endpointPrefix, rawVideoURL string) string {
	rawVideoURL = strings.TrimSpace(rawVideoURL)
	if rawVideoURL == "" {
		return ""
	}
	endpointPrefix = strings.TrimRight(strings.TrimSpace(endpointPrefix), "/")
	if endpointPrefix == "" {
		endpointPrefix = defaultVideoEndpointPrefix
	}
	if strings.HasPrefix(rawVideoURL, endpointPrefix+"/") {
		return rawVideoURL
	}
	u, err := url.Parse(rawVideoURL)
	if err != nil || u.Path == "" {
		return endpointPrefix + "?url=" + url.QueryEscape(rawVideoURL)
	}
	p := u.Path
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	out := endpointPrefix + p
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// handleGalleryVideoRedirect 供 gallery 独立运行或 /video/{path...} 路由使用的视频重定向处理器：
// 1. 直接将视频链接的 host 替换为 twimg.moonchan.xyz（完整保留 path 与 query）
// 2. 302 直连该代理地址（2026-10-23 起广告跳转已关闭，不再经 ouo.io 广告中转页）
func handleGalleryVideoRedirect(w http.ResponseWriter, r *http.Request, cfg config) {
	videoBase := strings.TrimRight(cfg.videoBase, "/")
	if videoBase == "" {
		videoBase = defaultVideoBase
	}
	if !strings.HasPrefix(videoBase, "http://") && !strings.HasPrefix(videoBase, "https://") {
		videoBase = "https://" + videoBase
	}

	rawPath := strings.TrimSpace(r.PathValue("filepath"))
	var proxyVideoURL string
	if rawPath == "" || rawPath == "/" {
		rawParam := strings.TrimSpace(r.URL.Query().Get("url"))
		if rawParam == "" {
			rawParam = strings.TrimSpace(r.URL.Query().Get("s"))
		}
		if rawParam == "" {
			http.Error(w, "missing video path or url", http.StatusBadRequest)
			return
		}
		proxyVideoURL = overrideVideoURL(videoBase, rawParam)
	} else {
		if !strings.HasPrefix(rawPath, "/") {
			rawPath = "/" + rawPath
		}
		proxyVideoURL = videoBase + rawPath
		if r.URL.RawQuery != "" {
			proxyVideoURL += "?" + r.URL.RawQuery
		}
	}

	w.Header().Set("Cache-Control", "no-cache, no-store, private, must-revalidate")
	http.Redirect(w, r, proxyVideoURL, http.StatusFound)
}

func buildAll(doc document, mediaBase, videoBase, videoEndpointPrefix string) []mediaItem {
	out := make([]mediaItem, 0, len(doc.Timeline))
	for _, te := range doc.Timeline {
		raw := strings.TrimSpace(te.URL)
		if raw == "" {
			continue
		}
		isVideo := te.Type == "video" || te.Type == "animated_gif"
		u := raw
		ouo := ""
		if isVideo {
			u = overrideVideoURL(videoBase, raw)
			ouo = buildVideoEndpointURL(videoEndpointPrefix, raw)
		} else {
			u = mediaURL(mediaBase, raw)
		}
		out = append(out, mediaItem{
			URL:     u,
			OuoURL:  ouo,
			IsVideo: isVideo,
			TweetID: te.TweetID,
			// 视频封面使用 gallery_dl 添加的 cover 字段，直接替换 host 为 pbs.moonchan.xyz
			Cover: mediaURL(mediaBase, strings.TrimSpace(te.Cover)),
		})
	}
	return out
}

func filterMedia(items []mediaItem, filter string) []mediaItem {
	if filter == "all" || filter == "" {
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

// mediaURL 直接将图片/封面 URL 的 host 替换为 mediaBase（默认 https://pbs.moonchan.xyz），完整保留 path 与 query。
func mediaURL(base, raw string) string {
	raw = strings.TrimSpace(raw)
	if base == "" || raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return raw
	}
	base = strings.TrimRight(base, "/")
	out := base + u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

// overrideVideoURL 直接将视频 URL 的 host 替换为 videoBase（默认 https://twimg.moonchan.xyz），完整保留 path 与 query。
func overrideVideoURL(videoBase, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	base := strings.TrimRight(videoBase, "/")
	if base == "" {
		base = defaultVideoBase
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return raw
	}
	out := base + u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out
}

func legacyBase() string {
	v, ok := os.LookupEnv("GALLERY_LEGACY_BASE")
	if !ok {
		v = "https://x.4545810.xyz"
	}
	return strings.TrimRight(v, "/")
}

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
	if strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return false
	}
	return name == filepath.Base(name)
}
