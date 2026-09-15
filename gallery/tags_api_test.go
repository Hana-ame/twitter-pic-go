package gallery

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/Hana-ame/twitter-pic-go/limit"
	"github.com/Hana-ame/twitter-pic-go/tags"
)

// mustDB 开一个带标签真源表结构的临时库，测试结束时关闭。
// 单独抽出来是因为有些用例要既拿 cfg、又直接查库核对「被拒的请求有没有留下数据」。
func mustDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "twitter.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := tags.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

func newTestCfg(t *testing.T) config {
	t.Helper()
	return newTestCfgWithBans(t, mustDB(t), "")
}

// newTestCfgWithBans 用给定 bans.txt 内容构造配置。bans 必须显式注入，
// 不能让测试去碰 Shared() 单例（那会读到仓库里的真 bans.txt，且测试间互相污染）。
func newTestCfgWithBans(t *testing.T, db *sql.DB, bans string) config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bans.txt")
	if err := os.WriteFile(path, []byte(bans), 0o644); err != nil {
		t.Fatal(err)
	}
	return config{
		tags: tags.New(db), writable: true, db: db,
		bans:     ipban.New(path),
		tagLimit: limit.NewFastLimiter(1000), // 默认给足，免得像被限流
	}
}

// TestTagPostWritesAccountTags 钉住用户要求：gallery 的 tag POST 落到
// 与 twitter API 同一张 account_tags，并且照记 request_logs 流水。
func TestTagPostWritesAccountTags(t *testing.T) {
	cfg := newTestCfg(t)

	post := func(body string) map[string]any {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/tag", strings.NewReader(body))
		r.Header.Set("X-Forwarded-For", "5.6.7.8, 10.0.0.1")
		r.Header.Set("User-Agent", "test-agent")
		w := httptest.NewRecorder()
		handlePostAccountTag(w, r, cfg)
		if w.Code != 200 {
			t.Fatalf("POST %s -> %d %s", body, w.Code, w.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	post(`{"user":"alice","tag":"女性","d":1}`)
	res := post(`{"user":"alice","tag":"女性","d":1}`)

	tagsMap, _ := res["tags"].(map[string]any)
	if tagsMap["女性"] != float64(2) {
		t.Fatalf("两次 +1 应累加成 2（写进 account_tags）: %+v", tagsMap)
	}

	// 归零删行：-1 -1 之后标签消失
	post(`{"user":"alice","tag":"女性","d":-1}`)
	res = post(`{"user":"alice","tag":"女性","d":-1}`)
	tagsMap, _ = res["tags"].(map[string]any)
	if _, ok := tagsMap["女性"]; ok {
		t.Fatalf("归零应删行: %+v", tagsMap)
	}

	// 流水：4 次写 = 4 行，且 ip 取 XFF 首个地址
	var n int
	var ip string
	if err := cfg.db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 4 {
		t.Fatalf("request_logs 应有 4 行，实际 %d", n)
	}
	if err := cfg.db.QueryRow(`SELECT ip FROM request_logs LIMIT 1`).Scan(&ip); err != nil {
		t.Fatal(err)
	}
	// 统一 IP 口径后取的是 Principal：XFF 从右往左第 TRUSTED_PROXY_HOPS(默认1) 个，
	// 即「我们的 nginx 看到的那个真实客户端」= 10.0.0.1；左侧 5.6.7.8 是
	// 客户端自带、不可信的部分。旧口径「取首项」既可被伪造，也让两层流水对不上。
	if ip != "10.0.0.1" {
		t.Fatalf("ip 应是 Principal（可信跳数）而不是 XFF 首项，实际 %q", ip)
	}
}

// TestTagsGetUsesTable 钉住 GET 直接用新数据源：表里有就返回，不依赖任何 JSON 文件。
func TestTagsGetUsesTable(t *testing.T) {
	cfg := newTestCfg(t)
	if err := cfg.tags.Add("bob", map[string]int{"COS": 2, "男娘": -1}, "ip", "ua"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "/api/tags?keys=bob,carol", nil)
	w := httptest.NewRecorder()
	handleGetAccountTags(w, r, cfg)

	var out map[string]map[string]int
	b, _ := io.ReadAll(w.Body)
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out["bob"]["COS"] != 2 {
		t.Fatalf("GET 应直接读 account_tags: %s", b)
	}
	if _, ok := out["carol"]; !ok {
		t.Fatalf("请求里的每个 key 都应有返回: %s", b)
	}
}

// TestTagPostRateLimited 钉住新写入侧门的配额：gallery 写的是同一张权威表，
// 每 IP 超额必须 429，而不是无限累加权重。
func TestTagPostRateLimited(t *testing.T) {
	cfg := newTestCfg(t)
	cfg.tagLimit = limit.NewFastLimiter(3)

	code := func(ip string) int {
		r := httptest.NewRequest("POST", "/api/tag", strings.NewReader(`{"user":"a","tag":"t","d":1}`))
		r.Header.Set("X-Forwarded-For", ip)
		w := httptest.NewRecorder()
		handlePostAccountTag(w, r, cfg)
		return w.Code
	}
	for i := 0; i < 3; i++ {
		if got := code("2.2.2.2"); got != 200 {
			t.Fatalf("配额内第 %d 次应 200，实际 %d", i+1, got)
		}
	}
	if got := code("2.2.2.2"); got != 429 {
		t.Fatalf("超额应 429，实际 %d", got)
	}

	// 先查账：配额内 3 次写 + 被拒 1 次 → 表和流水都只该有 3。
	var rows int
	_ = cfg.db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&rows)
	if rows != 3 {
		t.Fatalf("被 429 拒掉的请求不该写流水，实际 %d 行", rows)
	}
	if w := cfg.tags.Weights("a"); w["t"] != 3 {
		t.Fatalf("权重应停在 3，实际 %+v", w)
	}

	// 再验配额按 IP 独立：换 IP 不该被连坐。
	if got := code("3.3.3.3"); got != 200 {
		t.Fatalf("配额按 IP 独立，别的 IP 不该被连坐，实际 %d", got)
	}
}

// TestTagPostRejected 钉住入参守卫：空标签、d=0、路径穿越的 user 都不放行。
func TestTagPostRejected(t *testing.T) {
	cfg := newTestCfg(t)
	for _, body := range []string{
		`{"user":"alice","tag":"","d":1}`,
		`{"user":"alice","tag":"女性","d":0}`,
		`{"user":"../etc/passwd","tag":"女性","d":1}`,
	} {
		r := httptest.NewRequest("POST", "/api/tag", strings.NewReader(body))
		w := httptest.NewRecorder()
		handlePostAccountTag(w, r, cfg)
		if w.Code != 400 {
			t.Fatalf("%s 应被拒（400），实际 %d", body, w.Code)
		}
	}
	var n int
	_ = cfg.db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&n)
	if n != 0 {
		t.Fatalf("被拒的请求不应写库/写流水: %d 行", n)
	}
}

// TestTagPostReadOnlyDegrades 钉住降级：库不可写时 503，而不是静默丢投票。
func TestTagPostReadOnlyDegrades(t *testing.T) {
	cfg := newTestCfg(t)
	cfg.writable = false
	r := httptest.NewRequest("POST", "/api/tag", strings.NewReader(`{"user":"a","tag":"t","d":1}`))
	w := httptest.NewRecorder()
	handlePostAccountTag(w, r, cfg)
	if w.Code != 503 {
		t.Fatalf("只读时应 503，实际 %d", w.Code)
	}
}

// postFrom 从指定 RemoteAddr / XFF 发一次投票，返回 recorder。
func postFrom(t *testing.T, cfg config, remote, xff string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/tag", strings.NewReader(`{"user":"alice","tag":"女性","d":1}`))
	r.RemoteAddr = remote
	if xff != "" {
		r.Header.Set("X-Forwarded-For", xff)
	}
	w := httptest.NewRecorder()
	handlePostAccountTag(w, r, cfg)
	return w
}

// TestTagPostIPTypes 钉住封禁判定：单 IP、CIDR 段、直连 IP、以及**响应体形态**。
func TestTagPostIPTypes(t *testing.T) {
	cfg := newTestCfgWithBans(t, mustDB(t), "203.0.113.7\n198.51.100.0/24\n")

	// 直连命中
	if w := postFrom(t, cfg, "203.0.113.7:5555", ""); w.Code != 403 {
		t.Fatalf("直连被封 IP 应 403，实际 %d", w.Code)
	}
	// CIDR 段内命中
	if w := postFrom(t, cfg, "198.51.100.42:5555", ""); w.Code != 403 {
		t.Fatalf("CIDR 段内应 403，实际 %d", w.Code)
	}
	// 未封放行
	w := postFrom(t, cfg, "203.0.113.8:5555", "")
	if w.Code != 200 {
		t.Fatalf("未封 IP 应 200，实际 %d %s", w.Code, w.Body.String())
	}
	// 响应体与根 API 同一个结构（两层一致不止于判定，也包括返回）
	var body ipban.Denied
	r := httptest.NewRequest("POST", "/api/tag", strings.NewReader(`{"user":"alice","tag":"女性","d":1}`))
	r.RemoteAddr = "203.0.113.7:1"
	rw := httptest.NewRecorder()
	handlePostAccountTag(rw, r, cfg)
	if err := json.Unmarshal(rw.Body.Bytes(), &body); err != nil {
		t.Fatalf("403 响应体不是统一结构: %v %s", err, rw.Body.String())
	}
	if body.Error != "Access Denied" || body.Reason != "Banned IP detected in chain" || body.IP != "203.0.113.7" {
		t.Fatalf("403 响应体不对: %+v", body)
	}
}

// TestTagPostBannedHiddenInChain 是这次补封禁的**核心用例**：
// `X-Forwarded-For: <好人IP>, <被封IP>` 必须仍然被拒。
// gallery 旧口径只看 XFF 首项，这种写法可以直接绕过；根 API 看整条链。
// 统一走 ipban.Decide（链上任一命中）之后两层行为一致。
func TestTagPostBannedHiddenInChain(t *testing.T) {
	cfg := newTestCfgWithBans(t, mustDB(t), "203.0.113.7\n")

	if w := postFrom(t, cfg, "192.0.2.10:1", "8.8.8.8, 203.0.113.7"); w.Code != 403 {
		t.Fatalf("被封 IP 藏在 XFF 链里也应 403，实际 %d（说明又退化成只看首项）", w.Code)
	}
	// 反向也要成立：把被封的放最左，同样得拒
	if w := postFrom(t, cfg, "192.0.2.10:1", "203.0.113.7, 8.8.8.8"); w.Code != 403 {
		t.Fatalf("被封 IP 在链首也应 403，实际 %d", w.Code)
	}
	// 干扰项：非法条目不该让整条链免检
	if w := postFrom(t, cfg, "192.0.2.10:1", "not-an-ip, 203.0.113.7"); w.Code != 403 {
		t.Fatalf("链里有非法条目时仍应命中被封 IP，实际 %d", w.Code)
	}
}

// TestTagPostBannedWritesNothing 钉住第 4 条要求：被拒的请求
// 既不写 account_tags，也不写 request_logs（与空 tag / d==0 / 非法 username 同口径）。
func TestTagPostBannedWritesNothing(t *testing.T) {
	db := mustDB(t)
	cfg := newTestCfgWithBans(t, db, "203.0.113.7\n")

	if w := postFrom(t, cfg, "203.0.113.7:1", ""); w.Code != 403 {
		t.Fatalf("应 403，实际 %d", w.Code)
	}
	var rows, logs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM account_tags`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || logs != 0 {
		t.Fatalf("被封请求留下了数据：account_tags=%d request_logs=%d", rows, logs)
	}

	// 对照：同一 IP 取消封禁后（热重载）应当能写进去——证明拦它的是封禁而不是别的守卫。
	if err := cfg.bans.Reload([]string{"203.0.113.8"}); err != nil {
		t.Fatal(err)
	}
	if w := postFrom(t, cfg, "203.0.113.7:1", ""); w.Code != 200 {
		t.Fatalf("换掉封禁表后应放行，实际 %d %s", w.Code, w.Body.String())
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&logs); err != nil {
		t.Fatal(err)
	}
	if logs != 1 {
		t.Fatalf("放行的请求应留下 1 行流水，实际 %d", logs)
	}
}
