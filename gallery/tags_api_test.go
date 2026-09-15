package gallery

import (
	"database/sql"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Hana-ame/twitter-pic-go/limit"
	"github.com/Hana-ame/twitter-pic-go/tags"
)

func newTestCfg(t *testing.T) config {
	t.Helper()
	db, err := sql.Open("sqlite", t.TempDir()+"/twitter.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := tags.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return config{tags: tags.New(db), writable: true, db: db}
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
	if ip != "5.6.7.8" {
		t.Fatalf("ip 应取 XFF 首个地址，实际 %q", ip)
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
