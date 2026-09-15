package gallery

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newVisCfg 造一个带 users 表的完整图站配置：
// alice=SUCCESS（可见）、bob=BANNED（该隐身）、carol 没有 users 行（fail-open，可见）。
// 磁盘上三个账号都有 json.gz —— 这正是旧实现唯一看的依据，所以三个都能访问到。
func newVisCfg(t *testing.T) config {
	t.Helper()
	db := mustDB(t)
	if _, err := db.Exec(`CREATE TABLE users (username TEXT PRIMARY KEY, nick TEXT, status TEXT, last_modify DATETIME)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (username, status) VALUES ('alice','SUCCESS'),('bob','BANNED'),('dave','')`); err != nil {
		t.Fatal(err) // 空串也不是 SUCCESS，同样要挡（与根 API 的 = 'SUCCESS' 互补）
	}
	dir := t.TempDir()
	for _, n := range []string{"alice", "bob", "carol", "dave"} {
		gz, err := os.Create(filepath.Join(dir, n+".json.gz"))
		if err != nil {
			t.Fatal(err)
		}
		w, _ := gzip.NewWriterLevel(gz, gzip.BestSpeed)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"account_info": map[string]any{"unique_id": "1", "name": n},
			"timeline": []any{
				map[string]any{"url": "https://pbs.twimg.com/media/" + n + ".jpg", "tweet_id": 1, "type": "photo"},
			},
		})
		_ = w.Close()
		_ = gz.Close()
	}
	cfg := newTestCfgWithBans(t, db, "")
	cfg.jsonDir = dir
	cfg.pageSize = 12
	cfg.vis = newVis(cfg.tags)
	return cfg
}

// seedTag 直接写一条账号标签（绕过 handler，造出"被封账号也有标签"的前提）。
// 走 seedTags：那是历史权重的形状，投票过程另有用例钉。
func seedTag(t *testing.T, cfg config, user, tag string, w int) {
	t.Helper()
	seedTags(t, cfg, user, map[string]int{tag: w})
}

// statusOf / bodyOf 都走完整路由，而不是直调 handler：
// /raw 的隐身判断写在 mux 注册的路由闭包里，直调 handler 盖不到它。
func statusOf(t *testing.T, cfg config, method, target string) int {
	t.Helper()
	w := httptest.NewRecorder()
	galleryMux(cfg, newReactionStore("")).ServeHTTP(
		w, httptest.NewRequest(method, target, nil))
	return w.Code
}

func bodyOf(t *testing.T, cfg config, target string) string {
	t.Helper()
	w := httptest.NewRecorder()
	galleryMux(cfg, newReactionStore("")).ServeHTTP(
		w, httptest.NewRequest("GET", target, nil))
	b, _ := io.ReadAll(w.Body)
	return string(b)
}

// expireVis 手动让视图过期，等价于"过了一个 TTL 周期"，用来测刷新边界
// （不靠 sleep，否则测试要么慢要么不稳）。
func expireVis(cfg config) {
	cfg.vis.mu.Lock()
	cfg.vis.at = time.Now().Add(-2 * visTTL)
	cfg.vis.mu.Unlock()
}

// TestBannedAccountInvisibleEverywhere 是这次收口的主用例：
// 逐个 URL 验被封账号挡得住、正常账号不误伤。404 而非 403（不确认存在）。
func TestBannedAccountInvisibleEverywhere(t *testing.T) {
	cfg := newVisCfg(t)
	seedTag(t, cfg, "alice", "女性", 1)
	seedTag(t, cfg, "bob", "女性", 1) // 与 alice 同标签：反查列表里必须只出现 alice

	// 主页与账号页
	if got := statusOf(t, cfg, "GET", "/u/alice"); got != 200 {
		t.Fatalf("正常账号主页应 200，实际 %d", got)
	}
	for _, u := range []string{"/u/bob", "/u/dave"} {
		if got := statusOf(t, cfg, "GET", u); got != 404 {
			t.Fatalf("%s 应 404（被封/非 SUCCESS 账号隐身），实际 %d", u, got)
		}
	}
	// 直链快照：完整时间线都在里面，是最大的口子，必须一起挡
	for _, u := range []string{"/raw/bob", "/raw/dave"} {
		if got := statusOf(t, cfg, "GET", u); got != 404 {
			t.Fatalf("%s 应 404（不保留直链），实际 %d", u, got)
		}
	}
	if got := statusOf(t, cfg, "GET", "/raw/alice"); got != 200 {
		t.Fatalf("正常账号 /raw 应 200，实际 %d", got)
	}

	// 首页列表：bob / dave 不能出现在 #a-data 或任何位置
	home := bodyOf(t, cfg, "/")
	if strings.Contains(home, "bob") {
		t.Fatalf("首页仍泄漏被封账号名")
	}
	if !strings.Contains(home, "alice") || !strings.Contains(home, "carol") {
		t.Fatalf("首页误伤了可见账号: alice/carol 缺失")
	}

	// 标签反查：必须走 mux，直调 handler 时 r.PathValue("tag") 是空的
	var rev struct {
		Users []string `json:"users"`
		Count int      `json:"count"`
	}
	if err := json.Unmarshal([]byte(bodyOf(t, cfg, "/api/tag/%E5%A5%B3%E6%80%A7")), &rev); err != nil {
		t.Fatal(err)
	}
	if len(rev.Users) != 1 || rev.Users[0] != "alice" || rev.Count != 1 {
		t.Fatalf("反查列表应只剩 alice，实际 %+v", rev)
	}

	// 批量标签接口：被封的 key 省略，其余照给
	seedTags(t, cfg, "carol", map[string]int{"女性": 1})
	var m map[string]map[string]int
	if err := json.Unmarshal([]byte(bodyOf(t, cfg, "/api/tags?keys=alice,bob,carol,dave")), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["bob"]; ok {
		t.Fatalf("bob 不该出现在返回里: %v", m)
	}
	if _, ok := m["dave"]; ok {
		t.Fatalf("dave(status='') 不该出现在返回里: %v", m)
	}
	if len(m["alice"]) == 0 || len(m["carol"]) == 0 {
		t.Fatalf("可见账号该有返回: %v", m)
	}
}

// TestBannedAccountCannotBeVotedIntoExistence 钉住写侧：给被封账号投票不该
// 把它的标签重新写回真源（否则下一轮首页标签云又把它顶出来）。
func TestBannedAccountCannotBeVotedIntoExistence(t *testing.T) {
	cfg := newVisCfg(t)
	seedTag(t, cfg, "alice", "已存在", 1) // 让 account_tags 非空，便于数行数

	post := func(body string) int {
		r := httptest.NewRequest("POST", "/api/tag", strings.NewReader(body))
		r.Header.Set("CF-Connecting-IP", "8.8.8.8")
		w := httptest.NewRecorder()
		handlePostAccountTag(w, r, cfg)
		return w.Code
	}
	if got := post(`{"user":"bob","tag":"复活","d":1}`); got != 404 {
		t.Fatalf("给被封账号投票应 404，实际 %d", got)
	}
	if got := post(`{"user":"alice","tag":"正常","d":1}`); got != 200 {
		t.Fatalf("给正常账号投票应 200，实际 %d", got)
	}
	if w := cfg.tags.Weights("bob"); len(w) != 0 {
		t.Fatalf("bob 不该被写入任何标签: %+v", w)
	}
	var n int
	if err := cfg.db.QueryRow(`SELECT COUNT(*) FROM request_logs WHERE username='bob'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("被 404 拒掉的投票不该留流水，实际 %d 行", n)
	}
}

// TestCloudSubtractsBannedAccounts 钉住标签云：被封账号的计数要扣掉，
// 只剩被封账号在用的标签整条抹掉（否则点进去是空列表，且等于泄漏存在性）。
func TestCloudSubtractsBannedAccounts(t *testing.T) {
	cfg := newVisCfg(t)
	seedTag(t, cfg, "alice", "共有", 1)
	seedTag(t, cfg, "bob", "共有", 1)
	seedTag(t, cfg, "bob", "独占", 1) // 只有被封账号带它
	got := cfg.vis.cloud()
	m := map[string]int{}
	for _, c := range got {
		m[c.Tag] = c.Count
	}
	if m["共有"] != 1 {
		t.Fatalf("「共有」应扣掉 bob 后剩 1，实际 %+v", m)
	}
	if _, ok := m["独占"]; ok {
		t.Fatalf("只剩被封账号的标签应整条消失，实际 %+v", m)
	}

	// 关键回归：给被封账号**新增**一批标签后，不重启进程、不等 TTL，
	// 刷新后的视图里也不能出现它们。（旧实现按缓存快照事后扣减，这里会漏。）
	many := map[string]int{}
	for i := 0; i < cloudTopN+5; i++ {
		many[fmt.Sprintf("bobonly%d", i)] = 1
	}
	seedTags(t, cfg, "bob", many)
	expireVis(cfg)
	after := cfg.vis.cloud()
	if len(after) != 1 || after[0].Tag != "共有" {
		t.Fatalf("隐身应把 bob 独占的 %d 个标签全排除，只剩「共有」，实际 %+v", len(many), after)
	}
}

// TestVisibilityFailOpen 钉住降级方向：读不到 users（表不存在 / 无库）时
// 不隐身，而不是把整站藏起来。
func TestVisibilityFailOpen(t *testing.T) {
	db := mustDB(t) // 只有 account_tags/request_logs，没有 users
	cfg := newTestCfgWithBans(t, db, "")
	cfg.vis = newVis(cfg.tags)
	if cfg.vis.hidden("whoever") {
		t.Fatal("users 表缺失时不该隐身")
	}
	if n := len(cfg.vis.filter([]string{"a", "b"})); n != 2 {
		t.Fatalf("读不到封禁视图时列表不该被裁剪，实际 %d 项", n)
	}
	// vis 完全没装（nil）也不该 panic 或隐身
	var nilVis *vis
	if nilVis.hidden("x") || len(nilVis.filter([]string{"a"})) != 1 {
		t.Fatal("nil vis 应表现为不隐身")
	}
	// 但 store 存在、查询成功且确实封了人 → 必须隐身（证明 fail-open 不是"永远不隐身"）
	if _, err := db.Exec(`CREATE TABLE users (username TEXT PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (username, status) VALUES ('zed','BANNED')`); err != nil {
		t.Fatal(err)
	}
	cfg.vis = newVis(cfg.tags) // 换掉缓存视图，重新拉
	if !cfg.vis.hidden("zed") {
		t.Fatal("users 表可读后必须隐身")
	}
	if cfg.vis.hidden("no-row") {
		t.Fatal("没有 users 行的账号不该被顺带隐藏（会把整站首页清空）")
	}
}

// TestVisibilityTTLIsBounded 钉住时效窗口：封禁生效最迟等一个 TTL（60s），
// 不需要重启进程。窗口内不刷新是有意的（否则每次首页都全表扫 users）。
func TestVisibilityTTLIsBounded(t *testing.T) {
	cfg := newVisCfg(t)
	if cfg.vis.hidden("alice") {
		t.Fatal("alice 初始应可见")
	}
	if _, err := cfg.db.Exec(`UPDATE users SET status='BANNED' WHERE username='alice'`); err != nil {
		t.Fatal(err)
	}
	if cfg.vis.hidden("alice") {
		t.Fatal("TTL 窗口内不该重新拉库——视图必须是缓存的，否则首页每次都要扫 users")
	}
	expireVis(cfg)
	if !cfg.vis.hidden("alice") {
		t.Fatalf("超过 TTL(%v) 后封禁应自动生效，无需重启", visTTL)
	}
}
