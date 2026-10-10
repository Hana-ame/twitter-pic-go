package twitter

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
	"github.com/gin-gonic/gin"
)

// ==================== 用户媒体与元数据轻量缓存 (Bounded Cache) ====================
// 为适应 528MB / 1 vCPU 的小内存机器，严格限制缓存上限（最多 30 个用户的 timeline），
// 绝不无界膨胀，兼顾极速响应与微量内存占用。

type parsedUserDoc struct {
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
		// 空缓存：记录 2 分钟，避免高频对磁盘 stat 不存在的文件
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

	nick := strings.TrimSpace(raw.AccountInfo.Nick)
	if nick == "" {
		nick = strings.TrimSpace(raw.AccountInfo.Name)
	}
	avatar := strings.TrimSpace(raw.AccountInfo.ProfileImage)
	banner := strings.TrimSpace(raw.AccountInfo.ProfileBanner)

	items := make([]V2MediaItem, len(raw.Timeline))
	photoCnt := 0
	videoCnt := 0
	for i, t := range raw.Timeline {
		tType := strings.ToLower(strings.TrimSpace(t.Type))
		if tType == "video" || tType == "animated_gif" {
			videoCnt++
		} else {
			photoCnt++
		}
		items[i] = V2MediaItem{
			TweetID: t.TweetID,
			URL:     t.URL,
			Type:    t.Type,
			Date:    t.Date,
			Cover:   strings.TrimSpace(t.Cover),
		}
	}

	doc := &parsedUserDoc{
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
		// 控制最多 30 个用户，先进先出淘汰
		if len(docKeys) >= 30 {
			oldest := docKeys[0]
			docKeys = docKeys[1:]
			delete(docCache, oldest)
		}
		docKeys = append(docKeys, username)
	}
	docCache[username] = doc
}

func getUserMeta(username string) (nick, avatar, banner string, totalUrls int) {
	doc := loadUserDoc(username)
	if doc == nil {
		return "", "", "", 0
	}
	return doc.nick, doc.avatar, doc.banner, doc.totalUrls
}

func resolveUserJsonGz(username string) string {
	fn := username + ".json.gz"
	// 1. GALLERY_JSON_DIR
	if dir := os.Getenv("GALLERY_JSON_DIR"); dir != "" {
		p := filepath.Join(dir, fn)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// 2. TWITTER_DIR
	if dir := os.Getenv("TWITTER_DIR"); dir != "" {
		p := filepath.Join(dir, fn)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	// 3. 当前工作目录
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

// checkNotModified 校验 If-Modified-Since 请求头与资源的更新时间。
// 若未发生变更，则设置 304 Not Modified 状态并返回 true，客户端可直接复用本地缓存。
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
	fileNick, avatar, banner, totalUrls := getUserMeta(u.Username)
	if u.Nick == "" {
		u.Nick = fileNick
	}
	u.Avatar = avatar
	u.Banner = banner
	u.TotalUrls = totalUrls
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
//
// 针对 Flutter 痛点的专项设计：
//  1. 严格过滤幽灵账号：JOIN users 表且强制要求 u.status = 'SUCCESS'，彻底消灭 404 死链账号。
//  2. 严格过滤负权账号：只返回当前标签投票 > 0 的有效支持账号。
//  3. 单次返回完整元数据（username, nick, avatar, total_urls, tags）：
//     Flutter 无需再发起 N+1 次并发请求到 /<user>.json.gz，彻底避免阻塞与 25rps 撞墙。
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

	sortBy := strings.ToLower(strings.TrimSpace(c.Query("sort"))) // "weight" or "updated"

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

	// 1. 统计有效用户总数 (只统计 status = 'SUCCESS' 且 tag 计数/权重 > 0 的真正在线账号)
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

	// 2. 查询分页数据
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

	c.Header("Cache-Control", "public, max-age=60, s-maxage=120")
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
// 全局用户列表，支持 after 游标或 page/offset 分页，统一附带丰富元数据。
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
		// 游标分页：先确认游标用户存在
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
		// 普通分页
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

	c.Header("Cache-Control", "public, max-age=60, s-maxage=120")
	c.JSON(http.StatusOK, UsersListResponse{
		Count:      len(users),
		Limit:      limitVal,
		HasMore:    len(users) >= limitVal,
		NextCursor: nextCursor,
		Users:      users,
	})
}

// HandleBatchUsersV2 GET/POST /v2/users/batch
// 批量获取指定用户名的详细元数据，一次请求获取头像、昵称、标签等，替代并发 GET /<user>.json.gz
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

	c.Header("Cache-Control", "public, max-age=120, s-maxage=300")
	c.JSON(http.StatusOK, BatchUsersResponse{
		Count:  len(userList),
		Users:  userList,
		ByName: byName,
	})
}

// HandleGetSingleUserV2 GET /v2/users/:username
// 单用户详情轻量卡片（包含头像、昵称、标签，避免直接下载整个 20MB 的 timeline）
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
		c.Header("Cache-Control", "public, max-age=120, s-maxage=300")
		c.JSON(http.StatusOK, u)
		return
	}

	c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
}

// HandleGetUserMediaV2 GET /v2/users/:username/media
//
// 核心减负接口：针对用户媒体瀑布流的分页查询。
// 客户端无需下载几十兆的完整 .json.gz，单次请求仅需 ~3KB 即可极速渲染首屏瀑布流。
// 支持 limit、offset/cursor、type (all/photo/video)。
func HandleGetUserMediaV2(c *gin.Context) {
	username := strings.TrimSpace(c.Param("username"))
	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username required"})
		return
	}

	// 1. 检查账号是否正常存在且未被封禁
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
	if doc == nil || len(doc.timeline) == 0 {
		c.Header("Cache-Control", "public, max-age=300, s-maxage=600")
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

	// 根据类型筛选
	var filtered []V2MediaItem
	if filter == "photo" {
		filtered = make([]V2MediaItem, 0, doc.photoCount)
		for _, m := range doc.timeline {
			t := strings.ToLower(m.Type)
			if t != "video" && t != "animated_gif" {
				filtered = append(filtered, m)
			}
		}
	} else if filter == "video" {
		filtered = make([]V2MediaItem, 0, doc.videoCount)
		for _, m := range doc.timeline {
			t := strings.ToLower(m.Type)
			if t == "video" || t == "animated_gif" {
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

	c.Header("Cache-Control", "public, max-age=300, s-maxage=600")
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

// HandleGetUserProfileV2 GET /v2/users/:username/profile
// 单用户轻量资料与媒体统计概览（不需要下载完整 timeline，即可获悉照片/视频统计与头像昵称）
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

	c.Header("Cache-Control", "public, max-age=120, s-maxage=300")
	c.JSON(http.StatusOK, UserProfileResponse{
		Username:      u.Username,
		Nick:          nick,
		Avatar:        avatar,
		ProfileBanner: banner,
		TotalUrls:     totalUrls,
		PhotoCount:    photoCnt,
		VideoCount:    videoCnt,
		Tags:          u.Tags,
		LastModify:    u.LastModify,
		Status:        u.Status,
		JsonGzURL:     formatJsonGzURL(u.Username, u.LastModify),
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
// 客户端冷启动聚合接口：一次请求获取热门标签、最新推荐用户与活跃统计。
// 服务端采用 30 秒内存自愈缓存，高并发请求 0.1ms 响应，大幅降低小主机 CPU 与 SQLite 压力。
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

	// 1. 热门标签前 15
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

	// 2. 最新用户前 15
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

	// 3. 用户总数
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
//
// 彻底解决旧接口参数混乱（?do_not_renew=true 等繁复 flag）的问题：
// 1. 支持单标签模式：{"tag": "女性", "d": 1}（d 取 1 投 / -1 减 / 0 撤票）
// 2. 支持批量期望态模式：{"tags": {"女性": 1, "自拍": 0}}
// 3. 严格校验目标账号状态（被封禁/不存在直接 404），投票成功即时重算并返回用户最新全量标签。
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

	// 校验用户有效性
	var status string
	err := DB.QueryRow("SELECT status FROM users WHERE username = ?", username).Scan(&status)
	if err != nil || status != "SUCCESS" {
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found or banned"})
		return
	}

	voteMap := make(map[string]int)

	// 单标签模式
	if req.Tag != "" && req.D != nil {
		d := *req.D
		if d < -1 || d > 1 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "d must be -1, 0 or 1"})
			return
		}
		voteMap[req.Tag] = d
	}

	// 批量期望态模式
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
				voteMap[k] = 0 // 撤票
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

	// 投票成功后立即使首页 feed 缓存失效
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
		// username
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

	c.Header("Cache-Control", "public, max-age=60, s-maxage=120")
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

	c.Header("Cache-Control", "public, max-age=300, s-maxage=600")
	c.JSON(http.StatusOK, TagCloudResponse{
		Total: len(items),
		Tags:  items,
	})
}

// RegisterV2Routes 挂载面向 Flutter / 现代移动端的新版路由。
// 既可以挂载在 /api/twitter/v2 下，也可以挂载在根 /api/v2 或 /api/flutter 下。
func RegisterV2Routes(g *gin.RouterGroup) {
	banMgr := ipban.Shared()

	// 统一 IP 封禁守卫（与根 API 和 gallery 共享同一 bans.txt 单例）
	g.Use(StrictIPBanMiddleware(banMgr))

	// 1. Tag 分页反查用户列表（包含丰富元数据）
	g.GET("/tags/:tag/users", HandleGetTagUsersV2)
	g.GET("/tag/:tag/users", HandleGetTagUsersV2) // 兼容单数别名

	// 2. 全局用户列表（支持 after 游标或 page/offset，包含头像/昵称/标签）
	g.GET("/users", HandleGetUsersListV2)
	g.GET("/users/list", HandleGetUsersListV2) // 兼容别名

	// 3. 批量获取用户卡片元数据（GET /users/batch?keys= 或 POST /users/batch）
	g.GET("/users/batch", HandleBatchUsersV2)
	g.POST("/users/batch", HandleBatchUsersV2)

	// 4. 用户媒体瀑布流分页（首屏仅需几 KB，大幅降低移动端与服务器带宽/内存压力）
	g.GET("/users/:username/media", HandleGetUserMediaV2)

	// 5. 用户轻量资料与媒体统计概览
	g.GET("/users/:username/profile", HandleGetUserProfileV2)

	// 6. 单用户详情卡片
	g.GET("/users/:username", HandleGetSingleUserV2)

	// 7. 标签投票（单标签或全量期望态）
	g.POST("/users/:username/tags", HandleVoteTagsV2)
	g.POST("/tags/vote", HandleVoteTagsV2)

	// 8. 首页聚合 Feed（带内存缓存，冷启动 0.1ms 极速响应）
	g.GET("/feed", HandleGetFeedV2)
	g.GET("/home", HandleGetFeedV2)

	// 9. 增强搜索接口（支持 tag/nick/username 分页）
	g.GET("/search", HandleSearchUsersV2)

	// 10. 标签云
	g.GET("/tags/cloud", HandleGetTagCloudV2)
	g.GET("/tag-cloud", HandleGetTagCloudV2) // 别名
}
