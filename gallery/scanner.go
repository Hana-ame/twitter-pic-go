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
	"sync"
	"sync/atomic"
	"time"
)

// mediaCounts 持有单个媒体条目的赞/倒赞计数。
//
// 它通过*指针*挂在 Media 上，而不是内嵌 int 字段。原因：代码里存在
// `items = append(items, *m)` 这类整结构体值拷贝，以及 pageData.Items []Media
// 交给 html/template 渲染 —— 这些路径都会非原子地读到内嵌的计数字段，race 检测器
// 会命中。指针本身的拷贝是安全的，真正的读写全部走 atomic。
//
// 对调用方暴露 LikeCount()/DislikeCount() 方法（无参），因此模板里的
// {{$m.LikeCount}} 写法不变。
type mediaCounts struct {
	likes    int32
	dislikes int32
}

// LikeCount 返回当前赞数（原子读）。
func (m Media) LikeCount() int32 {
	if m.Counts == nil {
		return 0
	}
	return atomic.LoadInt32(&m.Counts.likes)
}

// DislikeCount 返回当前倒赞数（原子读）。
func (m Media) DislikeCount() int32 {
	if m.Counts == nil {
		return 0
	}
	return atomic.LoadInt32(&m.Counts.dislikes)
}

// SetCounts 原子写入赞/倒赞计数。Media 在 ingestAccountDoc 中已初始化 Counts，
// 这里的 nil 判断只为防御零值 Media（零值 Media 不会被多个 goroutine 共享写入）。
func (m *Media) SetCounts(likes, dislikes int32) {
	if m.Counts == nil {
		m.Counts = &mediaCounts{}
	}
	atomic.StoreInt32(&m.Counts.likes, likes)
	atomic.StoreInt32(&m.Counts.dislikes, dislikes)
}

// Media represents a single remote image/video entry parsed from a
// <account>.json.gz timeline.
type Media struct {
	ID           string    `json:"id"`
	Path         string    `json:"path"` // virtual path: account/urlbase_0001.jpg
	Dir          string    `json:"dir"`  // virtual directory: account
	Name         string    `json:"name"`
	Type         string    `json:"type"` // photo / video / animated_gif
	Ext          string    `json:"ext"`
	Size         int64     `json:"size"` // unknown for remote media
	ModTime      time.Time `json:"mod_time"`
	Tags         []string  `json:"tags"`
	DirTags      []string  `json:"dir_tags,omitempty"`
	PerImageTags []string  `json:"per_image_tags,omitempty"` // 扁平 per-image 标签（与账号 tag 完全分离）
	// Counts 不直接进 JSON（由 MarshalJSON 原子输出 like_count/dislike_count）。
	Counts      *mediaCounts `json:"-"`
	URL         string       `json:"url"`             // proxy URL on this server
	Thumb       string       `json:"thumb,omitempty"` // 网格缩略图（twimg name=small 变体；视频为空）
	OriginalURL string       `json:"original_url"`    // pbs.twimg.com / video.twimg.com
	TweetID     int64        `json:"tweet_id"`        // 原推 ID，溯源用
}

// IsVideo reports whether the media is a video or animated gif.
func (m *Media) IsVideo() bool {
	return m.Type == "video" || m.Type == "animated_gif"
}

// mediaJSON 是 Media 的序列化视图。
//
// LikeCount/DislikeCount 会被 POST /react 在请求路径上并发写（见 handleReact），
// 同时被其他 goroutine 经 json.Marshal 读。直接暴露 int32 字段会让 race 检测器
// 命中 reflect 的非原子读，因此这里用独立的快照结构 + atomic load 输出。
type mediaJSON struct {
	ID           string    `json:"id"`
	Path         string    `json:"path"` // virtual path: account/urlbase_0001.jpg
	Dir          string    `json:"dir"`  // virtual directory: account
	Name         string    `json:"name"`
	Type         string    `json:"type"` // photo / video / animated_gif
	Ext          string    `json:"ext"`
	Size         int64     `json:"size"` // unknown for remote media
	ModTime      time.Time `json:"mod_time"`
	Tags         []string  `json:"tags"`
	DirTags      []string  `json:"dir_tags,omitempty"`
	PerImageTags []string  `json:"per_image_tags,omitempty"`
	LikeCount    int32     `json:"like_count"`
	DislikeCount int32     `json:"dislike_count"`
	URL          string    `json:"url"`
	Thumb        string    `json:"thumb,omitempty"`
	OriginalURL  string    `json:"original_url"`
	TweetID      int64     `json:"tweet_id"`
}

// MarshalJSON 序列化时用原子的 LikeCount()/DislikeCount() 快照计数，
// 避免与 handleReact 的写入竞争。mediaJSON 是独立类型且无方法，不会递归。
//
// 用*值*接收者：代码里存在 []Media 值切片（pageData.Items、handleAPIMedia 的
// items）以及零值 Media 直接 Marshal 的路径，指针接收者会让这些场景退回按字段
// 序列化并把 like_count/dislike_count 整个丢掉。
func (m Media) MarshalJSON() ([]byte, error) {
	return json.Marshal(mediaJSON{
		ID:           m.ID,
		Path:         m.Path,
		Dir:          m.Dir,
		Name:         m.Name,
		Type:         m.Type,
		Ext:          m.Ext,
		Size:         m.Size,
		ModTime:      m.ModTime,
		Tags:         m.Tags,
		DirTags:      m.DirTags,
		PerImageTags: m.PerImageTags,
		LikeCount:    m.LikeCount(),
		DislikeCount: m.DislikeCount(),
		URL:          m.URL,
		Thumb:        m.Thumb,
		OriginalURL:  m.OriginalURL,
		TweetID:      m.TweetID,
	})
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

// ClusterTags 返回聚类（/clusters）使用的标签集合：账号级 tag 与 per-image tag，
// 刻意排除 DirTags。
//
// DirTags 就是账号名本身，对图库里的每条媒体都相同 —— 把它算进向量会让 k-means
// 退化成「按账号分组」，和账号列表页完全重复，聚类页就没意义了。
// 聚类页的目的是「跨账号找相似内容」，所以只看账号 tag 与 per-image tag。
func (m *Media) ClusterTags() []string {
	set := map[string]struct{}{}
	for _, t := range m.Tags {
		if t != "" {
			set[t] = struct{}{}
		}
	}
	for _, t := range m.PerImageTags {
		if t != "" {
			set[t] = struct{}{}
		}
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

// Gallery is an in-memory index over all json.gz timelines found in root.
type Gallery struct {
	root        string
	accountTags map[string][]string // username -> positive tags from SQLite

	media  []*Media
	byID   map[string]*Media
	byURL  map[string]*Media
	byPath map[string]*Media // virtual path -> media（/api/media/view 的 O(1) 查找）
	// byDirIndex: dir -> path -> 该 dir 排序后的下标（同目录 prev/next 导航用）
	byDirIndex map[string]map[string]int
	// bySeq: path -> 该 media 在 g.media 中的下标（全站 prev/next 导航用）
	bySeq  map[string]int
	byDir  map[string][]*Media // direct children only
	dirs   map[string][]string // parent dir -> immediate subdir names
	direct map[string]int      // dir -> count of media directly inside
	recurs map[string]int      // dir -> count of media in subtree
	// accountTagCounts: tag -> 账号数（左导航/标签云统计“哪些账号拥有该 tag”用，与 media 数分离）
	accountTagCounts map[string]int
	// avatars: 账号头像（account_info.profile_image，已按需走 twimg 反代）
	avatars map[string]string
	// bg: 每个账号最新一张 photo 的 URL（Scan 时预计算，用作行/卡片背景）
	bg map[string]string
	// latestPhoto: Scan 过程中记录每个账号最新 photo 时间的临时表
	latestPhoto map[string]time.Time
	// 远程 JSON 源地址（临时调试用，见 remote.go）；空串表示未启用
	remoteBase string
	// 远程拉取的文档仅存内存，绝不落盘（临时调试，见 remote.go）
	muRemote   sync.Mutex
	remoteDocs map[string][]byte
	// negativeCache：远程拉取失败记录（由 muRemote 保护），避免同一账号反复重试
	negativeCache map[string]time.Time
	// dbAccounts：来自本地 db（users 表）的账号索引，
	// 用于在未拉取媒体前就能展示完整账号列表（最新/最热/收藏等）。
	dbAccounts map[string]AccountMeta
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

// shortHash 返回 URL 的短哈希（8 字符 hex）。
func shortHash(s string) string {
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
		byPath:           map[string]*Media{},
		byDirIndex:       map[string]map[string]int{},
		bySeq:            map[string]int{},
		byDir:            map[string][]*Media{},
		dirs:             map[string][]string{},
		direct:           map[string]int{},
		recurs:           map[string]int{},
		avatars:          map[string]string{},
		bg:               map[string]string{},
		latestPhoto:      map[string]time.Time{},
		remoteDocs:       map[string][]byte{},
		negativeCache:    map[string]time.Time{},
		accountTagCounts: accountTagCounts,
		mediaBase:        deriveMediaBase(),
	}
}

func (g *Gallery) Root() string { return g.root }

// AccountMeta 来自 db users 表的账号元信息（无媒体时仍可展示）。
type AccountMeta struct {
	Nick       string
	LastModify time.Time
}

// SetDBAccounts 注入 db 账号索引，必须在首次 Scan() 之前调用。
func (g *Gallery) SetDBAccounts(m map[string]AccountMeta) { g.dbAccounts = m }

// Avatar 返回某账号的头像 URL（无则空串）。
func (g *Gallery) Avatar(dir string) string { return g.avatars[dir] }

// BG 返回某账号最新一张 photo 的 URL（Scan 时预计算；无则空串）。
func (g *Gallery) BG(dir string) string { return g.bg[dir] }

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

// thumbVariant 返回适合网格缩略图的 URL：加/改 name=small 查询参数
// （twimg 对 pbs 图片会返回小尺寸变体），解析失败则原样返回。
// raw 可以是原始 URL 或已走反代的 path 形式（查询参数照常透传）。
func thumbVariant(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	q.Set("name", "small")
	u.RawQuery = q.Encode()
	return u.String()
}

type gzTimelineEntry struct {
	URL     string `json:"url"`
	Date    string `json:"date"`
	TweetID int64  `json:"tweet_id"`
	Type    string `json:"type"`
}

type gzAccountInfo struct {
	Name           string `json:"name"`
	Nick           string `json:"nick"`
	ProfileImage   string `json:"profile_image"`
	FollowersCount int    `json:"followers_count"`
	StatusesCount  int    `json:"statuses_count"`
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

// ingestAccountDoc 把单个账号文档并入索引（next）及聚合 map，本地文件与远程内存文档共用。
func ingestAccountDoc(next *Gallery, username string, doc *gzDocument, mediaByDir map[string][]*Media, subdirs map[string]map[string]bool) {
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
	if av := strings.TrimSpace(doc.AccountInfo.ProfileImage); av != "" {
		next.avatars[dir] = serveURL(next.mediaBase, av)
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
		id := shortHash(dir + "\x00" + te.URL)

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
			TweetID:     te.TweetID,
			Counts:      &mediaCounts{},
		}
		if typ == "photo" {
			m.Thumb = thumbVariant(m.URL)
		}

		next.byID[m.ID] = m
		next.byURL[te.URL] = m
		next.byPath[m.Path] = m
		next.bySeq[m.Path] = len(next.media)
		next.media = append(next.media, m)
		mediaByDir[dir] = append(mediaByDir[dir], m)

		if subdirs[""] == nil {
			subdirs[""] = map[string]bool{}
		}
		subdirs[""][dir] = true
	}
}

// Scan walks the json.gz directory and rebuilds the in-memory index.
// scanNext builds a fresh index snapshot from g's configuration/data and returns it
// without mutating g. The caller decides whether to Replace in place or atomically swap.
func (g *Gallery) scanNext() (*Gallery, error) {
	info, err := os.Stat(g.root)
	if err != nil {
		return nil, fmt.Errorf("json dir %s is not accessible: %w", g.root, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("json dir %s is not a directory", g.root)
	}

	next := NewGallery(g.root, g.accountTags)
	next.remoteBase = g.remoteBase // 保留远程源配置（Replace 会整体覆盖）
	next.dbAccounts = g.dbAccounts // 保留 db 账号索引
	// 拷贝一份远程内存文档快照，避免扫描中与并发 fetch 互相干扰。
	g.muRemote.Lock()
	for username, raw := range g.remoteDocs {
		next.remoteDocs[username] = raw
	}
	g.muRemote.Unlock()
	mediaByDir := map[string][]*Media{}
	subdirs := map[string]map[string]bool{}
	seen := map[string]bool{}

	entries, err := os.ReadDir(g.root)
	if err != nil {
		return nil, err
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
		seen[username] = true
		ingestAccountDoc(next, username, &doc, mediaByDir, subdirs)
	}

	// 远程文档（仅存内存，临时调试）：本地没有的同名账号才用远端数据补入索引。
	// 直接遍历 scanNext 开始时拷贝的快照（next.remoteDocs），
	// 不需要再拿 g.muRemote（也避免了锁内再 Lock 的死锁问题）。
	for username, raw := range next.remoteDocs {
		if seen[username] {
			continue
		}
		if doc := decodeRawDoc(raw); doc != nil && len(doc.Timeline) > 0 {
			ingestAccountDoc(next, username, doc, mediaByDir, subdirs)
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
		// 同目录导航下标（与 byDir[dir] 排序结果一致）
		if len(items) > 0 {
			idx := make(map[string]int, len(items))
			for i, it := range items {
				idx[it.Path] = i
			}
			next.byDirIndex[dir] = idx
		}
	}

	// Compute recursive counts and tag counts.
	// Tag cloud counts only real account-level tags (m.Tags), not directory-derived
	// account names (m.DirTags) — those are reachable via the account list instead.
	for _, m := range next.media {
		next.recurs[m.Dir]++
		if m.Type == "photo" && m.ModTime.After(next.latestPhoto[m.Dir]) {
			// 预计算每个账号最新一张 photo（行/卡片背景），避免每次请求全量扫描。
			next.latestPhoto[m.Dir] = m.ModTime
			next.bg[m.Dir] = m.URL
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

	// 合并 db 提供的账号索引：即使还没有媒体数据，也要出现在账号列表里。
	if len(next.dbAccounts) > 0 && len(next.dirs[""]) >= 0 {
		have := map[string]bool{}
		for _, n := range next.dirs[""] {
			have[n] = true
		}
		for name := range next.dbAccounts {
			if !have[name] {
				next.dirs[""] = append(next.dirs[""], name)
				have[name] = true
			}
		}
		sort.Strings(next.dirs[""])
	}

	return next, nil
}

// Scan rebuilds the in-memory index and replaces it in place.
// This is only safe before the gallery starts serving concurrent requests;
// after serving begins use Server.rebuild so readers always see immutable snapshots.
func (g *Gallery) Scan() error {
	next, err := g.scanNext()
	if err != nil {
		return err
	}
	g.Replace(next)
	return nil
}

// Replace swaps the index contents under the caller's lock.
// 锁与远程内存文档（remoteDocs）不属于索引，不随拷贝覆盖。
func (g *Gallery) Replace(next *Gallery) {
	root, accountTags := g.root, g.accountTags
	remoteBase, remoteDocs := g.remoteBase, g.remoteDocs
	dbAccounts := g.dbAccounts

	// 逐字段替换索引内容，避免拷贝内嵌锁（vet）
	g.media, g.byID, g.byURL = next.media, next.byID, next.byURL
	g.byPath, g.bySeq = next.byPath, next.bySeq
	g.byDir, g.dirs = next.byDir, next.dirs
	g.byDirIndex = next.byDirIndex
	g.direct, g.recurs = next.direct, next.recurs
	g.accountTagCounts, g.avatars, g.mediaBase = next.accountTagCounts, next.avatars, next.mediaBase
	g.bg = next.bg

	g.root, g.accountTags = root, accountTags
	g.remoteBase, g.remoteDocs = remoteBase, remoteDocs
	g.dbAccounts = dbAccounts
}
