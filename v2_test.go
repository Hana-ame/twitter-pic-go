package twitter

import (
	"bytes"
	"compress/gzip"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Hana-ame/twitter-pic-go/Tools/sqlite"
	"github.com/gin-gonic/gin"
)

func setupTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sqlite.NewSQLiteDB(fmt.Sprintf("file:%s?mode=memory&cache=shared", t.Name()))
	if err != nil {
		t.Fatalf("sqlite memory: %v", err)
	}
	DB = db
	if err := CreateTableV3(); err != nil {
		t.Fatalf("CreateTableV3: %v", err)
	}
	return db
}

func setupTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := r.Group("/api/twitter")
	AddToGroup(api)

	RegisterV2Routes(r.Group("/api/v2"))
	RegisterV2Routes(r.Group("/api/flutter"))
	return r
}

func TestV2TagUsersExcludesGhostAndBanned(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	// 准备测试数据：
	// 1. alice: 正常用户，女性标签投票 5
	// 2. bob: 被封禁用户 (status = 'BANNED')，女性标签投票 10
	// 3. charlie: 幽灵用户（未在 users 表插入），女性标签投票 20
	// 4. dave: 正常用户，女性标签投票 -1 (负权)
	// 5. eve: 正常用户，女性标签投票 0 (撤票)
	// 6. frank: 正常用户，女性标签投票 2
	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status, last_modify) VALUES
			('alice', '爱丽丝', 'SUCCESS', '2026-10-06 10:00:00'),
			('bob', '鲍勃', 'BANNED', '2026-10-06 09:00:00'),
			('dave', '戴夫', 'SUCCESS', '2026-10-06 08:00:00'),
			('eve', '伊芙', 'SUCCESS', '2026-10-06 07:00:00'),
			('frank', '弗兰克', 'SUCCESS', '2026-10-06 06:00:00');
	`); err != nil {
		t.Fatalf("insert users: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO user_tag_cnt (username, tag, cnt, updated_at) VALUES
			('alice', '女性', 5, '2026-10-06 10:00:00'),
			('bob', '女性', 10, '2026-10-06 09:00:00'),
			('charlie', '女性', 20, '2026-10-06 08:30:00'),
			('dave', '女性', -1, '2026-10-06 08:00:00'),
			('eve', '女性', 0, '2026-10-06 07:00:00'),
			('frank', '女性', 2, '2026-10-06 06:00:00');
		INSERT INTO account_tags (username, tag, weight) VALUES
			('alice', '女性', 5),
			('alice', '自拍', 3),
			('bob', '女性', 10),
			('charlie', '女性', 20),
			('dave', '女性', -1),
			('frank', '女性', 2);
	`); err != nil {
		t.Fatalf("insert tags: %v", err)
	}

	// 请求 v2 标签用户接口
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/v2/tags/女性/users?limit=10", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var resp TagUsersResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal resp: %v", err)
	}

	if resp.Tag != "女性" {
		t.Errorf("expected tag 女性, got %s", resp.Tag)
	}
	// 只有 alice 和 frank 是合法且正权的，bob(banned)、charlie(ghost)、dave(<=0)、eve(0) 必须全被剔除！
	if resp.Total != 2 {
		t.Errorf("expected total 2, got %d", resp.Total)
	}
	if len(resp.Users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(resp.Users))
	}

	// 默认按 updated_at 倒序排列：alice (10:00:00) 在前，frank (06:00:00) 在后
	if resp.Users[0].Username != "alice" {
		t.Errorf("expected first user alice, got %s", resp.Users[0].Username)
	}
	if resp.Users[0].Nick != "爱丽丝" {
		t.Errorf("expected nick 爱丽丝, got %s", resp.Users[0].Nick)
	}
	if resp.Users[0].Tags["女性"] != 5 || resp.Users[0].Tags["自拍"] != 3 {
		t.Errorf("expected alice tags with 女性:5, 自拍:3, got %v", resp.Users[0].Tags)
	}
	if resp.Users[1].Username != "frank" {
		t.Errorf("expected second user frank, got %s", resp.Users[1].Username)
	}

	// 别名 /api/flutter/tags/女性/users 测试
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/flutter/tags/女性/users?limit=10", nil)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("flutter alias expected 200, got %d", w2.Code)
	}
}

func TestV2TagUsersSortByWeight(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status) VALUES
			('alice', '爱丽丝', 'SUCCESS'),
			('frank', '弗兰克', 'SUCCESS');
		INSERT INTO user_tag_cnt (username, tag, cnt, updated_at) VALUES
			('alice', '女性', 2, '2026-10-06 10:00:00'),
			('frank', '女性', 10, '2026-10-06 06:00:00');
		INSERT INTO account_tags (username, tag, weight) VALUES
			('alice', '女性', 2),
			('frank', '女性', 10);
	`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/v2/tags/女性/users?sort=weight", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got code %d", w.Code)
	}

	var resp TagUsersResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if len(resp.Users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(resp.Users))
	}
	// 按权重降序：frank (10) 在前，alice (2) 在后
	if resp.Users[0].Username != "frank" || resp.Users[1].Username != "alice" {
		t.Errorf("weight sort failed: got %s, %s", resp.Users[0].Username, resp.Users[1].Username)
	}
}

func TestV2UsersListPaging(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status, last_modify) VALUES
			('u1', '一号', 'SUCCESS', '2026-10-06 10:00:00'),
			('u2', '二号', 'SUCCESS', '2026-10-06 09:00:00'),
			('u3', '三号', 'SUCCESS', '2026-10-06 08:00:00');
	`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 1. offset 分页
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/v2/users?limit=2&page=1", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got code %d", w.Code)
	}
	var page1 UsersListResponse
	_ = json.Unmarshal(w.Body.Bytes(), &page1)
	if len(page1.Users) != 2 || page1.Users[0].Username != "u1" || page1.Users[1].Username != "u2" {
		t.Errorf("page 1 failed: %v", page1)
	}
	if !page1.HasMore {
		t.Errorf("page 1 should have more")
	}

	// 2. 游标分页
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/twitter/v2/users?limit=2&after=u2", nil)
	r.ServeHTTP(w2, req2)
	var cursorPage UsersListResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &cursorPage)
	if len(cursorPage.Users) != 1 || cursorPage.Users[0].Username != "u3" {
		t.Errorf("cursor page failed: %v", cursorPage)
	}
}

func TestV2BatchUsers(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status) VALUES
			('alice', '爱丽丝', 'SUCCESS'),
			('bob', '鲍勃', 'BANNED');
		INSERT INTO account_tags (username, tag, weight) VALUES
			('alice', '女装', 1);
	`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// GET keys=alice,bob,ghost
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/v2/users/batch?keys=alice,bob,ghost", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("got code %d", w.Code)
	}
	var resp BatchUsersResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)

	// bob(BANNED) 和 ghost(不存在) 不应在返回中
	if resp.Count != 1 || len(resp.Users) != 1 {
		t.Fatalf("expected count 1, got %d", resp.Count)
	}
	if resp.Users[0].Username != "alice" || resp.ByName["alice"].Nick != "爱丽丝" {
		t.Errorf("batch data mismatch: %v", resp)
	}

	// POST {"users": ["alice"]}
	bodyBytes, _ := json.Marshal(map[string]any{"users": []string{"alice"}})
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/v2/users/batch", bytes.NewReader(bodyBytes))
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("post batch got %d", w2.Code)
	}
}

func TestV2SingleUser(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status) VALUES
			('alice', '爱丽丝', 'SUCCESS'),
			('banned_user', '坏人', 'BANNED');
	`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 正常用户
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/v2/users/alice", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got code %d", w.Code)
	}
	var u FlutterUser
	_ = json.Unmarshal(w.Body.Bytes(), &u)
	if u.Username != "alice" || u.Nick != "爱丽丝" {
		t.Errorf("single user failed: %v", u)
	}

	// 被封禁用户 -> 404
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/twitter/v2/users/banned_user", nil)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotFound {
		t.Errorf("banned user should be 404, got %d", w2.Code)
	}

	// 不存在用户 -> 404
	w3 := httptest.NewRecorder()
	req3, _ := http.NewRequest("GET", "/api/twitter/v2/users/ghost", nil)
	r.ServeHTTP(w3, req3)
	if w3.Code != http.StatusNotFound {
		t.Errorf("ghost user should be 404, got %d", w3.Code)
	}
}

func TestV2Search(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status) VALUES
			('catlover', '猫猫控', 'SUCCESS'),
			('doglover', '狗狗控', 'SUCCESS');
	`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/v2/search?q=猫&by=nick", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("search got %d", w.Code)
	}
	var resp SearchUsersResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Count != 1 || resp.Users[0].Username != "catlover" {
		t.Errorf("search by nick failed: %v", resp)
	}
}

func TestOldAPIsRemainUntouchedAndFunctional(t *testing.T) {
	// 验证旧 API 绝无被破坏：
	// 1. GET /api/twitter/tags/:username
	// 2. GET /api/twitter/?list=users
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status, last_modify) VALUES
			('legacy_user', '旧用户', 'SUCCESS', '2026-10-06 10:00:00');
		INSERT INTO account_tags (username, tag, weight) VALUES
			('legacy_user', '经典', 42);
	`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 测试旧接口 GET /api/twitter/tags/:username
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/tags/legacy_user", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("legacy GET tags got %d: %s", w.Code, w.Body.String())
	}
	var legacyUser User
	if err := json.Unmarshal(w.Body.Bytes(), &legacyUser); err != nil {
		t.Fatalf("unmarshal legacy user: %v", err)
	}
	if legacyUser.Username != "legacy_user" || legacyUser.Tags["经典"] != 42 {
		t.Errorf("legacy user tags mismatch: %v", legacyUser)
	}

	// 测试旧接口 GET /api/twitter/?list=users
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/twitter/?list=users", nil)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("legacy GET list got %d", w2.Code)
	}
	var legacyList []User
	if err := json.Unmarshal(w2.Body.Bytes(), &legacyList); err != nil {
		t.Fatalf("unmarshal legacy list: %v", err)
	}
	if len(legacyList) != 1 || legacyList[0].Username != "legacy_user" {
		t.Errorf("legacy list mismatch: %v", legacyList)
	}
}

func TestV2UserMediaAndProfile(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	// 创建临时目录存放 alice.json.gz
	tmpDir := t.TempDir()
	origDir := os.Getenv("GALLERY_JSON_DIR")
	os.Setenv("GALLERY_JSON_DIR", tmpDir)
	defer os.Setenv("GALLERY_JSON_DIR", origDir)

	// 写入模拟的 media_user.json.gz
	docJSON := `{
		"account_info": {
			"name": "MediaUser",
			"nick": "爱丽丝宝贝",
			"profile_image": "https://pbs.twimg.com/avatar_media.jpg"
		},
		"timeline": [
			{"tweet_id": 101, "url": "https://pbs.twimg.com/p1.jpg", "type": "photo", "date": "2026-10-06T10:00:00Z"},
			{"tweet_id": 102, "url": "https://pbs.twimg.com/p2.jpg", "type": "photo", "date": "2026-10-06T09:00:00Z"},
			{"tweet_id": 103, "url": "https://video.twimg.com/v1.mp4", "type": "video", "date": "2026-10-06T08:00:00Z"},
			{"tweet_id": 104, "url": "https://pbs.twimg.com/p3.jpg", "type": "photo", "date": "2026-10-06T07:00:00Z"}
		]
	}`
	gzPath := filepath.Join(tmpDir, "media_user.json.gz")
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(docJSON))
	_ = zw.Close()
	if err := os.WriteFile(gzPath, buf.Bytes(), 0644); err != nil {
		t.Fatalf("write mock gz: %v", err)
	}

	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status, last_modify) VALUES
			('media_user', '爱丽丝', 'SUCCESS', '2026-10-06 10:00:00');
		INSERT INTO account_tags (username, tag, weight) VALUES
			('media_user', '自拍', 10);
	`); err != nil {
		t.Fatalf("insert media_user: %v", err)
	}

	// 1. 测试媒体分页：photo 筛选 + limit=2
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/v2/users/media_user/media?type=photo&limit=2&offset=0", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("get media failed %d: %s", w.Code, w.Body.String())
	}
	var mediaResp UserMediaResponse
	if err := json.Unmarshal(w.Body.Bytes(), &mediaResp); err != nil {
		t.Fatalf("unmarshal media: %v", err)
	}
	if mediaResp.Total != 3 { // 3 张 photo，1 个 video
		t.Errorf("expected total photos 3, got %d", mediaResp.Total)
	}
	if len(mediaResp.Media) != 2 {
		t.Fatalf("expected 2 sliced media, got %d", len(mediaResp.Media))
	}
	if mediaResp.Media[0].TweetID != 101 || mediaResp.Media[1].TweetID != 102 {
		t.Errorf("unexpected media items: %v", mediaResp.Media)
	}
	if !mediaResp.HasMore || mediaResp.NextCursor != 2 {
		t.Errorf("pagination state wrong: hasMore=%v, nextCursor=%d", mediaResp.HasMore, mediaResp.NextCursor)
	}

	// 2. 测试媒体分页：video 筛选
	wVideo := httptest.NewRecorder()
	reqVideo, _ := http.NewRequest("GET", "/api/twitter/v2/users/media_user/media?type=video", nil)
	r.ServeHTTP(wVideo, reqVideo)
	var videoResp UserMediaResponse
	_ = json.Unmarshal(wVideo.Body.Bytes(), &videoResp)
	if videoResp.Total != 1 || len(videoResp.Media) != 1 || videoResp.Media[0].TweetID != 103 {
		t.Errorf("expected 1 video item, got %v", videoResp)
	}

	// 3. 测试用户资料与统计概览
	wProf := httptest.NewRecorder()
	reqProf, _ := http.NewRequest("GET", "/api/twitter/v2/users/media_user/profile", nil)
	r.ServeHTTP(wProf, reqProf)
	if wProf.Code != http.StatusOK {
		t.Fatalf("profile failed %d", wProf.Code)
	}
	var profResp UserProfileResponse
	_ = json.Unmarshal(wProf.Body.Bytes(), &profResp)
	if profResp.Username != "media_user" || profResp.Avatar != "https://pbs.twimg.com/avatar_media.jpg" {
		t.Errorf("profile data wrong: %v", profResp)
	}
	if profResp.PhotoCount != 3 || profResp.VideoCount != 1 || profResp.TotalUrls != 4 {
		t.Errorf("profile counts wrong: %v", profResp)
	}
}

func TestV2Feed(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status) VALUES
			('feed_u1', '活跃一号', 'SUCCESS');
		INSERT INTO tag_counts (tag, cnt) VALUES
			('热门', 99);
	`); err != nil {
		t.Fatalf("insert feed data: %v", err)
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/v2/feed", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("feed got %d", w.Code)
	}
	var resp FeedResponse
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.TotalUsers != 1 || len(resp.TopTags) != 1 || resp.TopTags[0].Tag != "热门" {
		t.Errorf("feed response wrong: %v", resp)
	}

	// 缓存命中验证：即使清空数据库临时表，短时间内依然直接返回缓存响应
	wCached := httptest.NewRecorder()
	reqCached, _ := http.NewRequest("GET", "/api/v2/feed", nil)
	r.ServeHTTP(wCached, reqCached)
	if wCached.Code != http.StatusOK {
		t.Fatalf("cached feed got %d", wCached.Code)
	}
}

func TestV2VoteTags(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status) VALUES
			('voter_target', '目标用户', 'SUCCESS');
	`); err != nil {
		t.Fatalf("insert target: %v", err)
	}

	// 1. 单标签投票：POST {"tag": "新标签", "d": 1}
	voteBody := []byte(`{"tag": "新标签", "d": 1}`)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/twitter/v2/users/voter_target/tags", bytes.NewReader(voteBody))
	req.RemoteAddr = "192.0.2.1:1234"
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("vote single got %d: %s", w.Code, w.Body.String())
	}
	var voteResp VoteTagResponse
	_ = json.Unmarshal(w.Body.Bytes(), &voteResp)
	if voteResp.Tags["新标签"] != 1 {
		t.Errorf("expected tag vote 1, got %v", voteResp.Tags)
	}

	// 2. 批量期望态投票：POST {"tags": {"新标签": 0, "第二标签": 1}} (撤销第一个，新增第二个)
	batchVoteBody := []byte(`{"tags": {"新标签": 0, "第二标签": 1}}`)
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("POST", "/api/twitter/v2/users/voter_target/tags", bytes.NewReader(batchVoteBody))
	req2.RemoteAddr = "192.0.2.1:1234"
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusOK {
		t.Fatalf("vote batch got %d: %s", w2.Code, w2.Body.String())
	}
	var batchResp VoteTagResponse
	_ = json.Unmarshal(w2.Body.Bytes(), &batchResp)
	if val, exists := batchResp.Tags["新标签"]; exists && val != 0 {
		t.Errorf("新标签 should be withdrawn, got %v", batchResp.Tags)
	}
	if batchResp.Tags["第二标签"] != 1 {
		t.Errorf("第二标签 should be 1, got %v", batchResp.Tags)
	}

	// 3. 对不存在用户投票 -> 404
	wGhost := httptest.NewRecorder()
	reqGhost, _ := http.NewRequest("POST", "/api/twitter/v2/users/ghost_user/tags", bytes.NewReader(voteBody))
	reqGhost.RemoteAddr = "192.0.2.1:1234"
	reqGhost.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(wGhost, reqGhost)
	if wGhost.Code != http.StatusNotFound {
		t.Errorf("ghost vote should be 404, got %d", wGhost.Code)
	}
}

func TestV2JsonGzURLAndConditionalCaching(t *testing.T) {
	db := setupTestDB(t)
	defer db.Close()
	r := setupTestEngine()

	// 插入一个测试用户
	modTime := "2026-10-06 10:00:00"
	if _, err := db.Exec(`
		INSERT INTO users (username, nick, status, last_modify) VALUES
			('cache_tester', '缓存测试员', 'SUCCESS', ?);
	`, modTime); err != nil {
		t.Fatalf("insert cache tester: %v", err)
	}

	// 1. 测试 GET /v2/users/cache_tester 返回 JsonGzURL 与 Last-Modified
	w1 := httptest.NewRecorder()
	req1, _ := http.NewRequest("GET", "/api/twitter/v2/users/cache_tester", nil)
	r.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("get user got %d: %s", w1.Code, w1.Body.String())
	}
	var u FlutterUser
	if err := json.Unmarshal(w1.Body.Bytes(), &u); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if u.JsonGzURL == "" {
		t.Errorf("expected non-empty json_gz_url")
	}
	lastModHeader := w1.Header().Get("Last-Modified")
	if lastModHeader == "" {
		t.Errorf("expected Last-Modified header")
	}

	// 2. 带 If-Modified-Since 再次请求单用户 -> 期望 304 Not Modified
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/twitter/v2/users/cache_tester", nil)
	req2.Header.Set("If-Modified-Since", lastModHeader)
	r.ServeHTTP(w2, req2)
	if w2.Code != http.StatusNotModified {
		t.Errorf("expected 304 Not Modified, got %d", w2.Code)
	}

	// 3. 测试 profile 接口的 JsonGzURL 与 304 缓存
	wProf1 := httptest.NewRecorder()
	reqProf1, _ := http.NewRequest("GET", "/api/twitter/v2/users/cache_tester/profile", nil)
	r.ServeHTTP(wProf1, reqProf1)
	if wProf1.Code != http.StatusOK {
		t.Fatalf("profile got %d: %s", wProf1.Code, wProf1.Body.String())
	}
	var profResp UserProfileResponse
	_ = json.Unmarshal(wProf1.Body.Bytes(), &profResp)
	if profResp.JsonGzURL == "" {
		t.Errorf("expected non-empty json_gz_url in profile")
	}

	wProf2 := httptest.NewRecorder()
	reqProf2, _ := http.NewRequest("GET", "/api/twitter/v2/users/cache_tester/profile", nil)
	reqProf2.Header.Set("If-Modified-Since", lastModHeader)
	r.ServeHTTP(wProf2, reqProf2)
	if wProf2.Code != http.StatusNotModified {
		t.Errorf("expected 304 for profile, got %d", wProf2.Code)
	}

	// 4. 测试 media 接口的 304 缓存与 JsonGzURL
	wMedia1 := httptest.NewRecorder()
	reqMedia1, _ := http.NewRequest("GET", "/api/twitter/v2/users/cache_tester/media", nil)
	r.ServeHTTP(wMedia1, reqMedia1)
	if wMedia1.Code != http.StatusOK {
		t.Fatalf("media got %d: %s", wMedia1.Code, wMedia1.Body.String())
	}
	var mediaResp UserMediaResponse
	_ = json.Unmarshal(wMedia1.Body.Bytes(), &mediaResp)
	if mediaResp.JsonGzURL == "" {
		t.Errorf("expected non-empty json_gz_url in media")
	}

	wMedia2 := httptest.NewRecorder()
	reqMedia2, _ := http.NewRequest("GET", "/api/twitter/v2/users/cache_tester/media", nil)
	reqMedia2.Header.Set("If-Modified-Since", lastModHeader)
	r.ServeHTTP(wMedia2, reqMedia2)
	if wMedia2.Code != http.StatusNotModified {
		t.Errorf("expected 304 for media, got %d", wMedia2.Code)
	}
}


