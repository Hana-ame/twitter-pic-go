// ============================================================================
// 【新版 API 处理器 (New V2 API Handlers)】
// 路由前缀：/api/twitter/v2/* （同时挂载根别名 /api/v2/* 与 /api/flutter/*）
// 适用对象：
//   - twitter-pic-react 新分支 (v2 重置版)
//   - twitter-pic-flutter 移动端
//   - Gallery 现代 Web 组件
//
// 新版 API 核心改进与列表：
//   1. 视频封面 + 直接代理链接（2026-10-23 起广告跳转已关闭）：
//      所有视频条目自动重写 cover 为 https://pbs.moonchan.xyz/... 封面图地址，
//      并生成 ouo_url（历史契约字段名）= 本后端视频端点 /api/twitter/v2/video/...，
//      点击后由服务端 302 直连 https://twimg.moonchan.xyz/...（不再经 ouo.io 广告页）。
//   2. 消除 N+1 请求风暴：
//      - GET  /api/twitter/v2/users                        (带头像/昵称/标签/媒体统计的分页用户卡片列表)
//      - GET  /api/twitter/v2/search                       (支持 username/nick/tag 的富卡片搜索)
//      - GET  /api/twitter/v2/tags/:tag/users              (按标签分页反查富卡片列表，过滤 BANNED 与幽灵账号)
//      - GET  /api/twitter/v2/users/batch                  (批量获取多用户富卡片，供收藏夹等单次请求水合)
//      - POST /api/twitter/v2/users/batch
//      - GET  /api/twitter/v2/users/:username              (单用户富卡片)
//      - GET  /api/twitter/v2/users/:username/profile      (单用户轻量资料、图片/视频计数、标签与 Emoji)
//      - GET  /api/twitter/v2/users/:username/media        (分页媒体流，支持 ?type=all|photo|video&cursor=&limit=)
//      - GET  /api/twitter/v2/users/:username/timeline     (单用户完整时间线视图，内含 cover 与 ouo_url，支持 ?t= 强缓存)
//      - POST /api/twitter/v2/users/:username              (新增用户并提交初始标签，触发爬虫队列)
//      - POST /api/twitter/v2/users/:username/refresh      (触发已存在用户的爬虫更新队列)
//      - POST /api/twitter/v2/users/:username/tags         (一 IP 一票标签投票)
//      - POST /api/twitter/v2/tags/vote                    (一 IP 一票标签投票通用入口)
//      - GET  /api/twitter/v2/users/:username/emojis       (获取单用户 Emoji 计数)
//      - POST /api/twitter/v2/users/:username/emojis       (为单用户 Emoji 投票 +1)
//      - POST /api/twitter/v2/emojis/vote                  (为单用户 Emoji 投票通用入口)
//      - GET  /api/twitter/v2/ranking                      (Emoji 日/周/月排行榜 + 内嵌上榜用户富卡片字典 users)
//      - GET  /api/twitter/v2/feed · /home                 (冷启动聚合：热门标签 + 最新用户 + 总数)
//      - GET  /api/twitter/v2/tags/cloud · /tag-cloud      (热门标签云)
// ============================================================================

package api

import (
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/Hana-ame/twitter-pic-go/limit"
	"github.com/Hana-ame/twitter-pic-go/tags"
	"github.com/gin-gonic/gin"
)

const (
	v2DefaultMediaBase = "https://pbs.moonchan.xyz"
	v2DefaultVideoBase = "https://twimg.moonchan.xyz"
)

func v2ResolveMediaBase() string {
	if v := strings.TrimSpace(os.Getenv("GALLERY_MEDIA_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return v2DefaultMediaBase
}

func v2ResolveVideoBase() string {
	if v := strings.TrimSpace(os.Getenv("GALLERY_VIDEO_BASE")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return v2DefaultVideoBase
}

// v2CoverURL 将 gallery_dl 抓取的 cover 地址重写到唯一资源 CDN https://pbs.moonchan.xyz
// 例如：https://pbs.moonchan.xyz/ext_tw_video_thumb/1916894180884062208/pu/img/X472TmC88Oy-kvGn?format=jpg&name=orig
// v2CoverURL 将 gallery_dl 抓取的 cover 地址直接替换 host 为 pbs.moonchan.xyz
// 例如：https://pbs.moonchan.xyz/ext_tw_video_thumb/1916894180884062208/pu/img/X472TmC88Oy-kvGn?format=jpg&name=orig
func v2CoverURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return ReplaceHostDirectly(raw, v2ResolveMediaBase())
}

// v2VideoTargetURL 将原始视频地址直接替换 host 为 twimg.moonchan.xyz（完整保留 path+query）
func v2VideoTargetURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	return ReplaceHostDirectly(raw, v2ResolveVideoBase())
}

// v2BuildOuoURL 构造前端点击视频封面时指向本后端专属视频重定向端点的地址：
// /api/twitter/v2/video/<video_path>?<video_query>
// 当用户点击该地址时，由 HandleVideoRedirectV2 替换 host 为 twimg.moonchan.xyz 并 302 直连代理地址。
// JSON 字段名沿用历史契约 ouo_url（2026-10-23 起值已不含任何 ouo.io 跳转）。
func v2BuildOuoURL(rawVideoURL string) string {
	return BuildVideoEndpointURL(rawVideoURL)
}

// ==================== 用户媒体与元数据轻量缓存 (Bounded Cache) ====================
// 为适应 528MB / 1 vCPU 的小内存机器，严格限制缓存上限（最多 30 个用户的 timeline），
// 绝不无界膨胀，兼顾极速响应与微量内存占用。

type parsedUserDoc struct {
	name       string
	nick       string
	avatar     string
	banner     string // 资料背景图，可能为空（老账号 / 旧版 API）
	totalUrls  int
	photoCount int
	videoCount int
	timeline   []V2MediaItem
	expiresAt  time.Time
}

var (
	docCacheMu sync.RWMutex
	docCache   = make(map[string]*parsedUserDoc)
	docKeys    []string
)

func invalidateUserDocCache(username string) {
	docCacheMu.Lock()
	delete(docCache, username)
	docCacheMu.Unlock()
}

func loadUserDoc(username string) *parsedUserDoc {
	docCacheMu.RLock()
	entry, ok := docCache[username]
	docCacheMu.RUnlock()

	now := time.Now()
	if ok && entry.expiresAt.After(now) {
		return entry
	}

	fp := resolveUserJsonGz(username)
	if fp == "" {
		empty := &parsedUserDoc{expiresAt: now.Add(2 * time.Minute)}
		saveDocCache(username, empty)
		return empty
	}

	f, err := os.Open(fp)
	if err != nil {
		empty := &parsedUserDoc{expiresAt: now.Add(2 * time.Minute)}
		saveDocCache(username, empty)
		return empty
	}
	defer f.Close()

	zr, err := gzip.NewReader(f)
	if err != nil {
		empty := &parsedUserDoc{expiresAt: now.Add(2 * time.Minute)}
		saveDocCache(username, empty)
		return empty
	}
	defer zr.Close()

	var raw struct {
		AccountInfo struct {
			Name          string `json:"name"`
			Nick          string `json:"nick"`
			ProfileImage  string `json:"profile_image"`
			ProfileBanner string `json:"profile_banner"`
		} `json:"account_info"`
		Timeline []struct {
			URL     string `json:"url"`
			Date    string `json:"date"`
			TweetID int64  `json:"tweet_id"`
			Type    string `json:"type"`
			Cover   string `json:"cover"`
		} `json:"timeline"`
	}

	if err := json.NewDecoder(zr).Decode(&raw); err != nil && err != io.EOF {
		empty := &parsedUserDoc{expiresAt: now.Add(2 * time.Minute)}
		saveDocCache(username, empty)
		return empty
	}

	name := strings.TrimSpace(raw.AccountInfo.Name)
	if name == "" {
		name = username
	}
	nick := strings.TrimSpace(raw.AccountInfo.Nick)
	if nick == "" {
		nick = name
	}
	avatar := strings.TrimSpace(raw.AccountInfo.ProfileImage)
	banner := strings.TrimSpace(raw.AccountInfo.ProfileBanner)

	// 按 URL 去重并填充视频 cover (pbs.moonchan.xyz) 与 ouo_url（历史契约字段名；值为本后端视频端点 /api/twitter/v2/video/...，点击后 302 直连 twimg.moonchan.xyz）
	seen := make(map[string]struct{}, len(raw.Timeline))
	items := make([]V2MediaItem, 0, len(raw.Timeline))
	photoCnt := 0
	videoCnt := 0
	for _, t := range raw.Timeline {
		rawURL := strings.TrimSpace(t.URL)
		if rawURL == "" {
			continue
		}
		if _, dup := seen[rawURL]; dup {
			continue
		}
		seen[rawURL] = struct{}{}

		tType := strings.ToLower(strings.TrimSpace(t.Type))
		isVideo := tType == "video" || tType == "animated_gif" || tType == "article:video"
		coverURL := ""
		ouoURL := ""
		if isVideo {
			videoCnt++
			coverURL = v2CoverURL(t.Cover)
			ouoURL = v2BuildOuoURL(rawURL)
		} else {
			photoCnt++
			if strings.TrimSpace(t.Cover) != "" {
				coverURL = v2CoverURL(t.Cover)
			}
		}
		items = append(items, V2MediaItem{
			TweetID: t.TweetID,
			URL:     rawURL,
			Type:    t.Type,
			Date:    t.Date,
			Cover:   coverURL,
			OuoURL:  ouoURL,
		})
	}

	doc := &parsedUserDoc{
		name:       name,
		nick:       nick,
		avatar:     avatar,
		banner:     banner,
		totalUrls:  len(items),
		photoCount: photoCnt,
		videoCount: videoCnt,
		timeline:   items,
		expiresAt:  now.Add(10 * time.Minute),
	}

	saveDocCache(username, doc)
	return doc
}

func saveDocCache(username string, doc *parsedUserDoc) {
	docCacheMu.Lock()
	defer docCacheMu.Unlock()

	if _, exists := docCache[username]; !exists {
		if len(docKeys) >= 30 {
			oldest := docKeys[0]
			docKeys = docKeys[1:]
			delete(docCache, oldest)
		}
		docKeys = append(docKeys, username)
	}
	docCache[username] = doc
}

func getUserMeta(username string) (nick, avatar, banner string, totalUrls, photoCnt, videoCnt int) {
	doc := loadUserDoc(username)
	if doc == nil {
		return "", "", "", 0, 0, 0
	}
	return doc.nick, doc.avatar, doc.banner, doc.totalUrls, doc.photoCount, doc.videoCount
}

func resolveUserJsonGz(username string) string {
	fn := username + ".json.gz"
	if dir := os.Getenv("GALLERY_JSON_DIR"); dir != "" {
		p := filepath.Join(dir, fn)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if dir := os.Getenv("TWITTER_DIR"); dir != "" {
		p := filepath.Join(dir, fn)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	if _, err := os.Stat(fn); err == nil {
		return fn
	}
	return ""
}

func formatJsonGzURL(username string, lastModify time.Time) string {
	if username == "" {
		return ""
	}
	if !lastModify.IsZero() {
		return fmt.Sprintf("/api/twitter/%s.json.gz?t=%s", username, url.QueryEscape(lastModify.Format(time.RFC3339)))
	}
	return fmt.Sprintf("/api/twitter/%s.json.gz", username)
}

func checkNotModified(c *gin.Context, modtime time.Time) bool {
	if modtime.IsZero() {
		return false
	}
	modtime = modtime.Truncate(time.Second)
	c.Header("Last-Modified", modtime.UTC().Format(http.TimeFormat))

	ims := c.GetHeader("If-Modified-Since")
	if ims != "" {
		t, err := http.ParseTime(ims)
		if err == nil && !modtime.After(t) {
			c.Status(http.StatusNotModified)
			return true
		}
	}
	return false
}

func enrichUserMeta(u *FlutterUser) {
	if u == nil {
		return
	}
	if u.Tags == nil {
		u.Tags = make(map[string]int)
	}
	fileNick, avatar, banner, totalUrls, photoCnt, videoCnt := getUserMeta(u.Username)
	if u.Nick == "" {
		u.Nick = fileNick
	}
	u.Avatar = avatar
	u.Banner = banner
	u.TotalUrls = totalUrls
	u.PhotoCount = photoCnt
	u.VideoCount = videoCnt
	u.JsonGzURL = formatJsonGzURL(u.Username, u.LastModify)
}

// ==================== 核心查询逻辑 ====================

const v2UserSelectQuery = `
	SELECT u.username, COALESCE(u.nick, ''), u.status, u.last_modify,
	       COALESCE((SELECT json_group_object(a.tag, a.weight)
	                 FROM account_tags a
	                 WHERE a.username = u.username AND a.weight != 0), '{}')
	FROM users u
`

func scanFlutterUser(rows *sql.Rows) (FlutterUser, error) {
	var u FlutterUser
	var tagsRaw string

	err := rows.Scan(&u.Username, &u.Nick, &u.Status, &u.LastModify, &tagsRaw)
	if err != nil {
		return u, err
	}

	u.Tags = make(map[string]int)
	if tagsRaw != "" && tagsRaw != "{}" {
		if err := json.Unmarshal([]byte(tagsRaw), &u.Tags); err != nil {
			log.Printf("v2: unmarshal tags for %s: %v", u.Username, err)
		}
	}

	enrichUserMeta(&u)
	return u, nil
}

// ==================== HTTP Handlers ====================

// HandleGetTagUsersV2 GET /v2/tags/:tag/users 或 /v2/tag/:tag/users
func HandleGetTagUsersV2(c *gin.Context) {
	tag := strings.TrimSpace(c.Param("tag"))
	if tag == "" || len(tag) > 64 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid tag"})
		return
	}

	limitVal := 25
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			limitVal = n
		}
	}

	pageVal := 1
	if p := c.Query("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			pageVal = n
		}
	}

	offsetVal := (pageVal - 1) * limitVal
	if o := c.Query("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offsetVal = n
			if limitVal > 0 {
				pageVal = (offsetVal / limitVal) + 1
			}
		}
	}

	sortBy := strings.ToLower(strings.TrimSpace(c.Query("sort")))

	if DB == nil {
		c.JSON(http.StatusOK, TagUsersResponse{
			Tag:     tag,
			Total:   0,
			Count:   0,
			Page:    pageVal,
			Limit:   limitVal,
			Offset:  offsetVal,
			HasMore: false,
			Users:   []FlutterUser{},
		})
		return
	}

	var total int
	countSQL := `
		SELECT COUNT(*)
		FROM users u
		JOIN user_tag_cnt a ON a.username = u.username
		WHERE u.status = 'SUCCESS' AND a.tag = ? AND a.cnt > 0`
	err := DB.QueryRow(countSQL, tag).Scan(&total)
	if err != nil || total == 0 {
		fallbackCountSQL := `
			SELECT COUNT(*)
			FROM users u
			JOIN account_tags a ON a.username = u.username
			WHERE u.status = 'SUCCESS' AND a.tag = ? AND a.weight > 0`
		_ = DB.QueryRow(fallbackCountSQL, tag).Scan(&total)
	}

	if total == 0 {
		c.JSON(http.StatusOK, TagUsersResponse{
			Tag:     tag,
			Total:   0,
			Count:   0,
			Page:    pageVal,
			Limit:   limitVal,
			Offset:  offsetVal,
			HasMore: false,
			Users:   []FlutterUser{},
		})
		return
	}

	var orderClause string
	if sortBy == "weight" {
		orderClause = "ORDER BY a.cnt DESC, u.username ASC"
	} else {
		orderClause = "ORDER BY a.updated_at DESC, u.username ASC"
	}

	querySQL := `
		SELECT u.username, COALESCE(u.nick, ''), u.status, u.last_modify,
		       COALESCE((SELECT json_group_object(at.tag, at.weight)
		                 FROM account_tags at
		                 WHERE at.username = u.username AND at.weight != 0), '{}')
		FROM users u
		JOIN user_tag_cnt a ON a.username = u.username
		WHERE u.status = 'SUCCESS' AND a.tag = ? AND a.cnt > 0
		` + orderClause + `
		LIMIT ? OFFSET ?`

	rows, err := DB.Query(querySQL, tag, limitVal, offsetVal)
	if err != nil {
		if sortBy == "updated" {
			orderClause = "ORDER BY u.last_modify DESC, u.username ASC"
		} else {
			orderClause = "ORDER BY a.weight DESC, u.username ASC"
		}
		fallbackQuerySQL := `
			SELECT u.username, COALESCE(u.nick, ''), u.status, u.last_modify,
			       COALESCE((SELECT json_group_object(at.tag, at.weight)
			                 FROM account_tags at
			                 WHERE at.username = u.username AND at.weight != 0), '{}')
			FROM users u
			JOIN account_tags a ON a.username = u.username
			WHERE u.status = 'SUCCESS' AND a.tag = ? AND a.weight > 0
			` + orderClause + `
			LIMIT ? OFFSET ?`
		rows, err = DB.Query(fallbackQuerySQL, tag, limitVal, offsetVal)
	}

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query tag users failed: " + err.Error()})
		return
	}
	defer rows.Close()

	users := make([]FlutterUser, 0, limitVal)
	for rows.Next() {
		u, err := scanFlutterUser(rows)
		if err == nil {
			users = append(users, u)
		}
	}

	hasMore := (offsetVal + len(users)) < total

	c.Header("Cache-Control", "public, max-age=60, s-maxage=120, stale-while-revalidate=60")
	c.JSON(http.StatusOK, TagUsersResponse{
		Tag:     tag,
		Total:   total,
		Count:   len(users),
		Page:    pageVal,
		Limit:   limitVal,
		Offset:  offsetVal,
		HasMore: hasMore,
		Users:   users,
	})
}

// HandleGetUsersListV2 GET /v2/users
func HandleGetUsersListV2(c *gin.Context) {
	after := strings.TrimSpace(c.Query("after"))
	limitVal := 25
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			limitVal = n
		}
	}

	if DB == nil {
		c.JSON(http.StatusOK, UsersListResponse{
			Count: 0,
			Limit: limitVal,
			Users: []FlutterUser{},
		})
		return
	}

	var users []FlutterUser
	var err error

	if after != "" {
		var dummy int
		err = DB.QueryRow("SELECT 1 FROM users WHERE username = ?", after).Scan(&dummy)
		if err != nil {
			if err == sql.ErrNoRows {
				c.JSON(http.StatusNotFound, gin.H{"error": "cursor user not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query cursor failed: " + err.Error()})
			return
		}

		query := v2UserSelectQuery + `
			WHERE (u.last_modify < (SELECT last_modify FROM users WHERE username = ?)
			       OR (u.last_modify = (SELECT last_modify FROM users WHERE username = ?) AND u.username < ?))
			AND u.status = 'SUCCESS'
			ORDER BY u.last_modify DESC, u.username DESC
			LIMIT ?`
		rows, qErr := DB.Query(query, after, after, after, limitVal)
		if qErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query users failed: " + qErr.Error()})
			return
		}
		defer rows.Close()

		for rows.Next() {
			u, err := scanFlutterUser(rows)
			if err == nil {
				users = append(users, u)
			}
		}
	} else {
		pageVal := 1
		if p := c.Query("page"); p != "" {
			if n, err := strconv.Atoi(p); err == nil && n > 0 {
				pageVal = n
			}
		}
		offsetVal := (pageVal - 1) * limitVal
		if o := c.Query("offset"); o != "" {
			if n, err := strconv.Atoi(o); err == nil && n >= 0 {
				offsetVal = n
			}
		}

		query := v2UserSelectQuery + `
			WHERE u.status = 'SUCCESS'
			ORDER BY u.last_modify DESC, u.username DESC
			LIMIT ? OFFSET ?`
		rows, qErr := DB.Query(query, limitVal, offsetVal)
		if qErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "query users failed: " + qErr.Error()})
			return
		}
		defer rows.Close()

		for rows.Next() {
			u, err := scanFlutterUser(rows)
			if err == nil {
				users = append(users, u)
			}
		}
	}

	if users == nil {
		users = []FlutterUser{}
	}

	var nextCursor string
	if len(users) > 0 {
		nextCursor = users[len(users)-1].Username
	}

	c.Header("Cache-Control", "public, max-age=60, s-maxage=120, stale-while-revalidate=60")
	c.JSON(http.StatusOK, UsersListResponse{
		Count:      len(users),
		Limit:      limitVal,
		HasMore:    len(users) >= limitVal,
		NextCursor: nextCursor,
		Users:      users,
	})
}

// HandleBatchUsersV2 GET/POST /v2/users/batch
func HandleBatchUsersV2(c *gin.Context) {
	var names []string

	if c.Request.Method == http.MethodGet {
		keys := c.Query("keys")
		if keys == "" {
			keys = c.Query("users")
		}
		for _, name := range strings.Split(keys, ",") {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				names = append(names, trimmed)
			}
		}
	} else {
		bodyBytes, err := io.ReadAll(c.Request.Body)
		if err == nil && len(bodyBytes) > 0 {
			var wrapper struct {
				Users []string `json:"users"`
			}
			if err := json.Unmarshal(bodyBytes, &wrapper); err == nil && len(wrapper.Users) > 0 {
				names = wrapper.Users
			} else {
				var directList []string
				if err := json.Unmarshal(bodyBytes, &directList); err == nil {
					names = directList
				}
			}
		}
	}

	if len(names) == 0 {
		c.JSON(http.StatusOK, BatchUsersResponse{
			Count:  0,
			Users:  []FlutterUser{},
			ByName: map[string]FlutterUser{},
		})
		return
	}

	uniqueNames := make([]string, 0, len(names))
	seen := make(map[string]struct{})
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, exists := seen[n]; !exists {
			seen[n] = struct{}{}
			uniqueNames = append(uniqueNames, n)
			if len(uniqueNames) >= 100 {
				break
			}
		}
	}

	if DB == nil {
		c.JSON(http.StatusOK, BatchUsersResponse{
			Count:  0,
			Users:  []FlutterUser{},
			ByName: map[string]FlutterUser{},
		})
		return
	}

	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(uniqueNames)), ",")
	args := make([]any, len(uniqueNames))
	for i, n := range uniqueNames {
		args[i] = n
	}

	query := v2UserSelectQuery + ` WHERE u.status = 'SUCCESS' AND u.username IN (` + placeholders + `)`
	rows, err := DB.Query(query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query batch users failed: " + err.Error()})
		return
	}
	defer rows.Close()

	byName := make(map[string]FlutterUser)
	for rows.Next() {
		u, err := scanFlutterUser(rows)
		if err == nil {
			byName[u.Username] = u
		}
	}

	userList := make([]FlutterUser, 0, len(byName))
	for _, n := range uniqueNames {
		if u, ok := byName[n]; ok {
			userList = append(userList, u)
		}
	}

	c.Header("Cache-Control", "public, max-age=120, s-maxage=300, stale-while-revalidate=120")
	c.JSON(http.StatusOK, BatchUsersResponse{
		Count:  len(userList),
		Users:  userList,
		ByName: byName,
	})
}

// HandleGetSingleUserV2 GET /v2/users/:username
func HandleGetSingleUserV2(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))
	if username == "" || username == "batch" || username == "list" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid username"})
		return
	}

	if DB == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "database not available"})
		return
	}

	query := v2UserSelectQuery + ` WHERE u.username = ?`
	rows, err := DB.Query(query, username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query user failed: " + err.Error()})
		return
	}
	defer rows.Close()

	if rows.Next() {
		u, err := scanFlutterUser(rows)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "scan user failed: " + err.Error()})
			return
		}
		if u.Status != "SUCCESS" {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found or banned"})
			return
		}
		if checkNotModified(c, u.LastModify) {
			return
		}
		c.Header("Cache-Control", "public, max-age=120, s-maxage=300, stale-while-revalidate=120")
		c.JSON(http.StatusOK, u)
		return
	}

	c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
}

// HandleGetUserMediaV2 GET /v2/users/:username/media
func HandleGetUserMediaV2(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))
	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username required"})
		return
	}

	var status string
	var lastModify time.Time
	if DB != nil {
		err := DB.QueryRow("SELECT status, last_modify FROM users WHERE username = ?", username).Scan(&status, &lastModify)
		if err != nil || status != "SUCCESS" {
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found or banned"})
			return
		}
	}

	if checkNotModified(c, lastModify) {
		return
	}

	limitVal := 24
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			limitVal = n
		}
	}

	offsetVal := 0
	if cur := c.Query("cursor"); cur != "" {
		if n, err := strconv.Atoi(cur); err == nil && n >= 0 {
			offsetVal = n
		}
	} else if off := c.Query("offset"); off != "" {
		if n, err := strconv.Atoi(off); err == nil && n >= 0 {
			offsetVal = n
		}
	} else if p := c.Query("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			offsetVal = (n - 1) * limitVal
		}
	}

	filter := strings.ToLower(strings.TrimSpace(c.Query("type")))
	if filter == "" {
		filter = "all"
	}

	doc := loadUserDoc(username)
	cacheHeader := "public, max-age=300, s-maxage=600, stale-while-revalidate=300"
	if c.Query("t") != "" {
		cacheHeader = "public, max-age=3600, s-maxage=86400, stale-while-revalidate=3600"
	}

	if doc == nil || len(doc.timeline) == 0 {
		c.Header("Cache-Control", cacheHeader)
		c.JSON(http.StatusOK, UserMediaResponse{
			Username:  username,
			Total:     0,
			Count:     0,
			Page:      (offsetVal / limitVal) + 1,
			Limit:     limitVal,
			Offset:    offsetVal,
			HasMore:   false,
			Filter:    filter,
			JsonGzURL: formatJsonGzURL(username, lastModify),
			Media:     []V2MediaItem{},
		})
		return
	}

	var filtered []V2MediaItem
	if filter == "photo" {
		filtered = make([]V2MediaItem, 0, doc.photoCount)
		for _, m := range doc.timeline {
			t := strings.ToLower(m.Type)
			if t != "video" && t != "animated_gif" && t != "article:video" {
				filtered = append(filtered, m)
			}
		}
	} else if filter == "video" {
		filtered = make([]V2MediaItem, 0, doc.videoCount)
		for _, m := range doc.timeline {
			t := strings.ToLower(m.Type)
			if t == "video" || t == "animated_gif" || t == "article:video" {
				filtered = append(filtered, m)
			}
		}
	} else {
		filter = "all"
		filtered = doc.timeline
	}

	total := len(filtered)
	start := offsetVal
	if start > total {
		start = total
	}
	end := start + limitVal
	if end > total {
		end = total
	}

	sliced := filtered[start:end]
	hasMore := end < total
	nextCursor := end

	c.Header("Cache-Control", cacheHeader)
	c.JSON(http.StatusOK, UserMediaResponse{
		Username:   username,
		Total:      total,
		Count:      len(sliced),
		Page:       (start / limitVal) + 1,
		Limit:      limitVal,
		Offset:     start,
		NextCursor: nextCursor,
		HasMore:    hasMore,
		Filter:     filter,
		JsonGzURL:  formatJsonGzURL(username, lastModify),
		Media:      sliced,
	})
}

// HandleGetUserTimelineV2 GET /v2/users/:username/timeline
// 面向新版 twitter-pic-react 的完整时间线接口：
// 单次返回 account_info、tags、emojis 与附带 cover (pbs.moonchan.xyz) + ouo_url（本后端视频端点，302 直连 twimg.moonchan.xyz，无广告跳转）的 timeline。
// 配合 ?t=<last_modify> 参数，可被 Cloudflare 边缘缓存 24 小时及浏览器本地缓存复用。
func HandleGetUserTimelineV2(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))
	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username required"})
		return
	}

	if DB == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "database not available"})
		return
	}

	var u FlutterUser
	var tagsRaw string
	query := `SELECT u.username, COALESCE(u.nick, ''), u.status, u.last_modify,
	          COALESCE((SELECT json_group_object(a.tag, a.weight)
	                    FROM account_tags a
	                    WHERE a.username = u.username AND a.weight != 0), '{}')
	          FROM users u WHERE u.username = ?`
	err := DB.QueryRow(query, username).Scan(&u.Username, &u.Nick, &u.Status, &u.LastModify, &tagsRaw)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if u.Status != "SUCCESS" {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found or banned"})
		return
	}

	if checkNotModified(c, u.LastModify) {
		return
	}

	u.Tags = make(map[string]int)
	if tagsRaw != "" && tagsRaw != "{}" {
		_ = json.Unmarshal([]byte(tagsRaw), &u.Tags)
	}

	emojis, err := GetUserEmojis(username)
	if err != nil || emojis == nil {
		emojis = make(map[string]int)
	}

	doc := loadUserDoc(username)
	name := u.Username
	nick := u.Nick
	avatar := ""
	banner := ""
	totalUrls := 0
	photoCnt := 0
	videoCnt := 0
	var timeline []V2MediaItem
	if doc != nil {
		if doc.name != "" {
			name = doc.name
		}
		if nick == "" {
			nick = doc.nick
		}
		avatar = doc.avatar
		banner = doc.banner
		totalUrls = doc.totalUrls
		photoCnt = doc.photoCount
		videoCnt = doc.videoCount
		timeline = doc.timeline
	}
	if timeline == nil {
		timeline = []V2MediaItem{}
	}

	cacheHeader := "public, max-age=300, s-maxage=600, stale-while-revalidate=300"
	if c.Query("t") != "" {
		cacheHeader = "public, max-age=3600, s-maxage=86400, stale-while-revalidate=3600"
	}
	c.Header("Cache-Control", cacheHeader)
	c.JSON(http.StatusOK, UserTimelineResponse{
		Username: u.Username,
		AccountInfo: V2AccountInfo{
			Name:          name,
			Nick:          nick,
			ProfileImage:  avatar,
			ProfileBanner: banner,
		},
		TotalUrls:  totalUrls,
		PhotoCount: photoCnt,
		VideoCount: videoCnt,
		Tags:       u.Tags,
		Emojis:     emojis,
		LastModify: u.LastModify,
		Status:     u.Status,
		Timeline:   timeline,
	})
}

// HandleGetUserProfileV2 GET /v2/users/:username/profile
func HandleGetUserProfileV2(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))
	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username required"})
		return
	}

	if DB == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "database not available"})
		return
	}

	var u FlutterUser
	var tagsRaw string
	query := `SELECT u.username, COALESCE(u.nick, ''), u.status, u.last_modify,
	          COALESCE((SELECT json_group_object(a.tag, a.weight)
	                    FROM account_tags a
	                    WHERE a.username = u.username AND a.weight != 0), '{}')
	          FROM users u WHERE u.username = ?`
	err := DB.QueryRow(query, username).Scan(&u.Username, &u.Nick, &u.Status, &u.LastModify, &tagsRaw)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		return
	}
	if u.Status != "SUCCESS" {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found or banned"})
		return
	}

	if checkNotModified(c, u.LastModify) {
		return
	}

	u.Tags = make(map[string]int)
	if tagsRaw != "" && tagsRaw != "{}" {
		_ = json.Unmarshal([]byte(tagsRaw), &u.Tags)
	}

	emojis, _ := GetUserEmojis(username)

	doc := loadUserDoc(username)
	nick := u.Nick
	if nick == "" && doc != nil {
		nick = doc.nick
	}
	avatar := ""
	banner := ""
	totalUrls := 0
	photoCnt := 0
	videoCnt := 0
	if doc != nil {
		avatar = doc.avatar
		banner = doc.banner
		totalUrls = doc.totalUrls
		photoCnt = doc.photoCount
		videoCnt = doc.videoCount
	}

	c.Header("Cache-Control", "public, max-age=120, s-maxage=300, stale-while-revalidate=120")
	c.JSON(http.StatusOK, UserProfileResponse{
		Username:      u.Username,
		Nick:          nick,
		Avatar:        avatar,
		ProfileBanner: banner,
		TotalUrls:     totalUrls,
		PhotoCount:    photoCnt,
		VideoCount:    videoCnt,
		Tags:          u.Tags,
		Emojis:        emojis,
		LastModify:    u.LastModify,
		Status:        u.Status,
		JsonGzURL:     formatJsonGzURL(u.Username, u.LastModify),
	})
}

// ==================== 新增用户与触发更新 (V2 现代接口) ====================

// HandleCreateUserV2 POST /v2/users/:username
// 提交新账号（附带初始标签）并触发后台抓取。
func HandleCreateUserV2(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))
	if username == "" || username == "undefined" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username required"})
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)

	ip := ipban.Principal(c.Request)
	agent := c.Request.UserAgent()

	var req struct {
		Tags map[string]int `json:"tags"`
	}
	bodyBytes, _ := io.ReadAll(c.Request.Body)
	tagMap := make(map[string]int)
	if len(bodyBytes) > 0 {
		if err := json.Unmarshal(bodyBytes, &req); err == nil && len(req.Tags) > 0 {
			tagMap = req.Tags
		} else {
			_ = json.Unmarshal(bodyBytes, &tagMap)
		}
	}

	if len(tagMap) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "initial tags are required"})
		return
	}

	for k, v := range tagMap {
		if v > 0 {
			tagMap[k] = 1
		} else if v < 0 {
			tagMap[k] = -1
		}
	}

	if existing, err := getUserTags(username); err == nil && len(existing.Tags) > 0 {
		c.JSON(http.StatusOK, gin.H{"message": "already has tags, skipped", "username": username})
		return
	}

	if err := addTag(username, tagMap, ip, agent); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	if msg, err := curlMetaData(username); err != nil {
		log.Printf("v2 curlMetaData(%s) 排队失败: %v (%s)", username, err, msg)
	}

	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.JSON(http.StatusOK, gin.H{"message": "ok", "username": username})
}

// HandleRefreshUserV2 POST /v2/users/:username/refresh
// 触发已存在用户的后台抓取更新（无需再拼接 ?do_not_tag=true）。
func HandleRefreshUserV2(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))
	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username required"})
		return
	}
	if _, err := getUserTags(username); err != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "user not registered"})
		return
	}
	if msg, err := curlMetaData(username); err != nil {
		log.Printf("v2 refresh curlMetaData(%s) 排队失败: %v (%s)", username, err, msg)
	}
	invalidateUserDocCache(username)
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.JSON(http.StatusOK, gin.H{"message": "ok", "username": username})
}

// ==================== Emoji 互动与排行榜 (V2 现代接口) ====================

// HandleGetUserEmojisV2 GET /v2/users/:username/emojis
func HandleGetUserEmojisV2(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))
	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username required"})
		return
	}
	emojis, err := GetUserEmojis(username)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	if emojis == nil {
		emojis = make(map[string]int)
	}
	c.Header("Cache-Control", "public, max-age=30, s-maxage=60")
	c.JSON(http.StatusOK, emojis)
}

// HandleVoteEmojiV2 POST /v2/users/:username/emojis 或 POST /v2/emojis/vote
func HandleVoteEmojiV2(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))
	emoji := strings.TrimSpace(c.Query("emoji"))

	if c.Request.ContentLength != 0 {
		var req struct {
			Username string `json:"username"`
			User     string `json:"user"`
			Emoji    string `json:"emoji"`
		}
		if err := c.ShouldBindJSON(&req); err == nil {
			if username == "" {
				username = strings.TrimSpace(req.Username)
				if username == "" {
					username = strings.TrimSpace(req.User)
				}
			}
			if emoji == "" {
				emoji = strings.TrimSpace(req.Emoji)
			}
		}
	}
	if username == "" {
		username = strings.TrimSpace(c.Query("username"))
	}

	if username == "" || emoji == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username and emoji required"})
		return
	}

	if err := VoteUpEmoji(username, emoji); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	emojis, _ := GetUserEmojis(username)
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.JSON(http.StatusOK, emojis)
}

// HandleGetRankingV2 GET /v2/ranking 或 GET /v2/emojis/ranking
// 返回全量日/周/月榜单以及所有上榜用户的富卡片字典（users），
// 新版 twitter-pic-react 只需 1 次请求即可渲染整个排行榜，无需 N+1 拉取 json.gz。
func HandleGetRankingV2(c *gin.Context) {
	var allData AllRankData
	if b, err := os.ReadFile(RankFileJSON); err == nil {
		_ = json.Unmarshal(b, &allData)
	}
	if allData == nil {
		allData = make(AllRankData)
	}

	// 收集所有上榜用户名，一次性批量查询富卡片
	nameSet := make(map[string]struct{})
	for _, ed := range allData {
		for _, item := range ed.Day {
			if item.Username != "" {
				nameSet[item.Username] = struct{}{}
			}
		}
		for _, item := range ed.Week {
			if item.Username != "" {
				nameSet[item.Username] = struct{}{}
			}
		}
		for _, item := range ed.Month {
			if item.Username != "" {
				nameSet[item.Username] = struct{}{}
			}
		}
	}

	usersMap := make(map[string]FlutterUser, len(nameSet))
	if DB != nil && len(nameSet) > 0 {
		names := make([]string, 0, len(nameSet))
		for n := range nameSet {
			names = append(names, n)
		}
		const chunkSize = 100
		for i := 0; i < len(names); i += chunkSize {
			end := i + chunkSize
			if end > len(names) {
				end = len(names)
			}
			chunk := names[i:end]
			ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
			args := make([]any, len(chunk))
			for j, v := range chunk {
				args[j] = v
			}
			rows, err := DB.Query(v2UserSelectQuery+` WHERE u.status = 'SUCCESS' AND u.username IN (`+ph+`)`, args...)
			if err == nil {
				for rows.Next() {
					if u, sErr := scanFlutterUser(rows); sErr == nil {
						usersMap[u.Username] = u
					}
				}
				rows.Close()
			}
		}
	}

	c.Header("Cache-Control", "public, max-age=300, s-maxage=600, stale-while-revalidate=300")
	c.JSON(http.StatusOK, RankingV2Response{
		Rankings: allData,
		Users:    usersMap,
	})
}

// ==================== 首页 Feed 聚合与缓存 ====================

var (
	feedCacheMu sync.Mutex
	feedCache   *FeedResponse
	feedCacheAt time.Time
	feedTTL     = 30 * time.Second
)

// HandleGetFeedV2 GET /v2/feed 或 /v2/home
func HandleGetFeedV2(c *gin.Context) {
	feedCacheMu.Lock()
	now := time.Now()
	if feedCache != nil && now.Sub(feedCacheAt) < feedTTL {
		cached := *feedCache
		feedCacheMu.Unlock()
		c.Header("Cache-Control", "public, max-age=30, s-maxage=60")
		c.JSON(http.StatusOK, cached)
		return
	}
	feedCacheMu.Unlock()

	if DB == nil {
		c.Header("Cache-Control", "public, max-age=30, s-maxage=60")
		c.JSON(http.StatusOK, FeedResponse{
			TopTags:     []TagCountItem{},
			RecentUsers: []FlutterUser{},
			TotalUsers:  0,
			UpdatedAt:   now,
		})
		return
	}

	var topTags []TagCountItem
	tagRows, err := DB.Query(`SELECT tag, cnt FROM tag_counts WHERE cnt > 0 ORDER BY cnt DESC, tag ASC LIMIT 15`)
	if err == nil {
		defer tagRows.Close()
		for tagRows.Next() {
			var t TagCountItem
			if err := tagRows.Scan(&t.Tag, &t.Count); err == nil {
				topTags = append(topTags, t)
			}
		}
	}
	if topTags == nil {
		topTags = []TagCountItem{}
	}

	var recentUsers []FlutterUser
	userRows, err := DB.Query(v2UserSelectQuery + ` WHERE u.status = 'SUCCESS' ORDER BY u.last_modify DESC, u.username DESC LIMIT 15`)
	if err == nil {
		defer userRows.Close()
		for userRows.Next() {
			if u, err := scanFlutterUser(userRows); err == nil {
				recentUsers = append(recentUsers, u)
			}
		}
	}
	if recentUsers == nil {
		recentUsers = []FlutterUser{}
	}

	var totalUsers int
	_ = DB.QueryRow(`SELECT COUNT(*) FROM users WHERE status = 'SUCCESS'`).Scan(&totalUsers)

	resp := FeedResponse{
		TopTags:     topTags,
		RecentUsers: recentUsers,
		TotalUsers:  totalUsers,
		UpdatedAt:   now,
	}

	feedCacheMu.Lock()
	feedCache = &resp
	feedCacheAt = now
	feedCacheMu.Unlock()

	c.Header("Cache-Control", "public, max-age=30, s-maxage=60")
	c.JSON(http.StatusOK, resp)
}

// ==================== 标签投票 (统一现代 API) ====================

// HandleVoteTagsV2 POST /v2/users/:username/tags 或 POST /v2/tags/vote
func HandleVoteTagsV2(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))

	var req VoteTagRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "bad request body: " + err.Error()})
		return
	}

	if username == "" {
		username = strings.TrimSpace(req.User)
	}
	if username == "" {
		username = strings.TrimSpace(c.Query("username"))
	}

	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username required"})
		return
	}

	if DB == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "database not available"})
		return
	}

	var status string
	err := DB.QueryRow("SELECT status FROM users WHERE username = ?", username).Scan(&status)
	if err != nil || status != "SUCCESS" {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found or banned"})
		return
	}

	voteMap := make(map[string]int)

	if req.Tag != "" && req.D != nil {
		d := *req.D
		if d < -1 || d > 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "d must be -1, 0 or 1"})
			return
		}
		voteMap[req.Tag] = d
	}

	if len(req.Tags) > 0 {
		for k, v := range req.Tags {
			k = strings.TrimSpace(k)
			if k == "" {
				continue
			}
			if v > 0 {
				voteMap[k] = 1
			} else if v < 0 {
				voteMap[k] = -1
			} else {
				voteMap[k] = 0
			}
		}
	}

	if len(voteMap) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no valid tag votes provided"})
		return
	}

	ip := ipban.Principal(c.Request)
	ua := c.Request.UserAgent()

	if err := addTag(username, voteMap, ip, ua); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "write votes failed: " + err.Error()})
		return
	}

	feedCacheMu.Lock()
	feedCache = nil
	feedCacheMu.Unlock()

	updatedWeights := Store().Weights(username)
	c.Header("Cache-Control", "no-cache, no-store, must-revalidate")
	c.JSON(http.StatusOK, VoteTagResponse{
		Username: username,
		Tags:     updatedWeights,
		Message:  "votes applied successfully",
	})
}

// HandleSearchUsersV2 GET /v2/search?q=xxx&by=tag|nick|username&limit=25&page=1
func HandleSearchUsersV2(c *gin.Context) {
	q := strings.TrimSpace(c.Query("q"))
	if q == "" {
		q = strings.TrimSpace(c.Query("search"))
	}
	if q == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "search query required"})
		return
	}

	by := strings.ToLower(strings.TrimSpace(c.Query("by")))
	if by == "" {
		by = "username"
	}

	limitVal := 25
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			if n > 100 {
				n = 100
			}
			limitVal = n
		}
	}

	pageVal := 1
	if p := c.Query("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			pageVal = n
		}
	}
	offsetVal := (pageVal - 1) * limitVal
	if o := c.Query("offset"); o != "" {
		if n, err := strconv.Atoi(o); err == nil && n >= 0 {
			offsetVal = n
		}
	}

	if DB == nil {
		c.JSON(http.StatusOK, SearchUsersResponse{
			Query: q,
			By:    by,
			Count: 0,
			Users: []FlutterUser{},
		})
		return
	}

	if by == "tag" {
		c.Request.URL.RawQuery = fmt.Sprintf("limit=%d&page=%d&offset=%d", limitVal, pageVal, offsetVal)
		c.Params = gin.Params{gin.Param{Key: "tag", Value: q}}
		HandleGetTagUsersV2(c)
		return
	}

	var query string
	var args []any
	if by == "nick" {
		query = v2UserSelectQuery + ` WHERE u.nick LIKE ? AND u.status = 'SUCCESS' ORDER BY u.last_modify DESC LIMIT ? OFFSET ?`
		args = []any{"%" + q + "%", limitVal, offsetVal}
	} else {
		query = v2UserSelectQuery + ` WHERE u.username LIKE ? AND u.status = 'SUCCESS' ORDER BY u.last_modify DESC LIMIT ? OFFSET ?`
		args = []any{"%" + q + "%", limitVal, offsetVal}
	}

	rows, err := DB.Query(query, args...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "search failed: " + err.Error()})
		return
	}
	defer rows.Close()

	users := make([]FlutterUser, 0, limitVal)
	for rows.Next() {
		u, err := scanFlutterUser(rows)
		if err == nil {
			users = append(users, u)
		}
	}

	c.Header("Cache-Control", "public, max-age=60, s-maxage=120, stale-while-revalidate=60")
	c.JSON(http.StatusOK, SearchUsersResponse{
		Query:   q,
		By:      by,
		Count:   len(users),
		Page:    pageVal,
		Limit:   limitVal,
		HasMore: len(users) >= limitVal,
		Users:   users,
	})
}

// HandleGetTagCloudV2 GET /v2/tags/cloud
func HandleGetTagCloudV2(c *gin.Context) {
	limitVal := 50
	if l := c.Query("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limitVal = n
		}
	}

	if DB == nil {
		c.Header("Cache-Control", "public, max-age=300, s-maxage=600")
		c.JSON(http.StatusOK, TagCloudResponse{
			Total: 0,
			Tags:  []TagCountItem{},
		})
		return
	}

	query := `SELECT tag, cnt FROM tag_counts WHERE cnt > 0 ORDER BY cnt DESC, tag ASC LIMIT ?`
	rows, err := DB.Query(query, limitVal)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "query tag cloud failed: " + err.Error()})
		return
	}
	defer rows.Close()

	items := make([]TagCountItem, 0, limitVal)
	for rows.Next() {
		var item TagCountItem
		if err := rows.Scan(&item.Tag, &item.Count); err == nil {
			items = append(items, item)
		}
	}

	c.Header("Cache-Control", "public, max-age=300, s-maxage=600, stale-while-revalidate=300")
	c.JSON(http.StatusOK, TagCloudResponse{
		Total: len(items),
		Tags:  items,
	})
}

// RegisterV2Routes 挂载面向新版 twitter-pic-react / Flutter / 现代客户端的新版路由。
func RegisterV2Routes(g *gin.RouterGroup) {
	banMgr := ipban.Shared()
	limiter := limit.NewFastLimiter(25)

	g.Use(StrictIPBanMiddleware(banMgr))

	// 1. Tag 分页反查用户列表（包含丰富元数据）
	g.GET("/tags/:tag/users", HandleGetTagUsersV2)
	g.GET("/tag/:tag/users", HandleGetTagUsersV2)

	// 2. 全局用户列表（支持 after 游标或 page/offset，包含头像/昵称/标签）
	g.GET("/users", HandleGetUsersListV2)
	g.GET("/users/list", HandleGetUsersListV2)

	// 3. 批量获取用户卡片元数据（GET /users/batch?keys= 或 POST /users/batch）
	g.GET("/users/batch", HandleBatchUsersV2)
	g.POST("/users/batch", HandleBatchUsersV2)

	// 4. 用户媒体瀑布流分页（附带视频 cover 与 ouo_url）
	g.GET("/users/:username/media", HandleGetUserMediaV2)

	// 5. 用户完整时间线视图（供新版 twitter-pic-react 单次获取资料+标签+Emoji+带 cover/ouo_url 的 timeline）
	g.GET("/users/:username/timeline", HandleGetUserTimelineV2)

	// 6. 用户轻量资料与媒体统计概览
	g.GET("/users/:username/profile", HandleGetUserProfileV2)

	// 7. 单用户详情卡片
	g.GET("/users/:username", HandleGetSingleUserV2)

	// 8. 新增用户与触发更新（替代旧版复杂的 ?do_not_tag / ?do_not_renew 参数）
	g.POST("/users/:username", limit.RateLimitMiddleware(limiter), limit.GlobalRateLimitMiddleware(), HandleCreateUserV2)
	g.POST("/users/:username/refresh", limit.RateLimitMiddleware(limiter), limit.GlobalRateLimitMiddleware(), HandleRefreshUserV2)

	// 9. 标签投票（单标签或全量期望态，一 IP 一票）
	g.POST("/users/:username/tags", limit.RateLimitMiddleware(limiter), HandleVoteTagsV2)
	g.POST("/tags/vote", limit.RateLimitMiddleware(limiter), HandleVoteTagsV2)

	// 10. Emoji 计数、投票与内嵌富卡片的排行榜
	g.GET("/users/:username/emojis", HandleGetUserEmojisV2)
	g.POST("/users/:username/emojis", HandleVoteEmojiV2)
	g.POST("/emojis/vote", HandleVoteEmojiV2)
	g.GET("/ranking", HandleGetRankingV2)
	g.GET("/emojis/ranking", HandleGetRankingV2)

	// 11. 首页聚合 Feed（带内存缓存，冷启动 0.1ms 极速响应）
	g.GET("/feed", HandleGetFeedV2)
	g.GET("/home", HandleGetFeedV2)

	// 12. 增强搜索接口（支持 tag/nick/username 分页）
	g.GET("/search", HandleSearchUsersV2)

	// 13. 标签云
	g.GET("/tags/cloud", HandleGetTagCloudV2)
	g.GET("/tag-cloud", HandleGetTagCloudV2)

	// 14. 视频链接专属重定向端点（接收视频 path 或 ?url= -> 替换 host 为 twimg.moonchan.xyz -> 302 直连代理地址）
	// 2026-10-23 起广告跳转已关闭（视频直连配额中间件随广告一并移除）：直接 302，不再经 ouo.io 广告页。
	g.GET("/video", HandleVideoRedirectV2)
	g.GET("/video/*filepath", HandleVideoRedirectV2)
}

func registerFeedInvalidationHook() {
	tags.OnDataChange(func(_ *sql.DB) {
		feedCacheMu.Lock()
		feedCache = nil
		feedCacheMu.Unlock()
	})
}
