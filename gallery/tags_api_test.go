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
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "twitter.db")+
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := tags.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}

// seedTags 造"历史底数"：绕过写路径直接插权重 + 快照底数，等价于票系统上线前
// 就攒下的权重。投票过程由本文件的 HTTP 用例与 tags 包各自钉，种子不该依赖它。
func seedTags(t *testing.T, cfg config, user string, w map[string]int) {
	t.Helper()
	for tag, v := range w {
		if _, err := cfg.db.Exec(`INSERT OR REPLACE INTO account_tags (username, tag, weight) VALUES (?,?,?)`,
			user, tag, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tags.BackfillVoteBase(cfg.db); err != nil {
		t.Fatal(err)
	}
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

// TestTagPostOneVotePerIP 钉住「一个 IP 一票」的 HTTP 契约与写库归属：
//   - 同一身份重复提交只算一票（幂等发生在服务端，不依赖 localStorage）；
//   - 换一个身份才加第二票；
//   - 反向改票（+1 → -1）落库差值 ±2，会把该行推到归零删行；
//   - 撤票（d=0）是合法请求，删票行、权重回落到剩余票之和；
//   - request_logs.ip 记的是 Principal，与票桶同一个值。
func TestTagPostOneVotePerIP(t *testing.T) {
	cfg := newTestCfg(t)

	// post 发一次投票：cf 非空则带 CF-Connecting-IP（线上经 CF 的真实形态是
	// CF 覆写该头 + nginx 追加 XFF "<真实客户端>, <CF 边缘>"）。
	post := func(cf, xff, body string) map[string]any {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/tag", strings.NewReader(body))
		if cf != "" {
			r.Header.Set("CF-Connecting-IP", cf)
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
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
	// alice 投一票（CF 头身份 5.6.7.8）
	vote := func(cf, xff string, d int) map[string]any {
		return post(cf, xff, `{"user":"alice","tag":"女性","d":`+itoa(d)+`}`)
	}
	weight := func() (int, bool) {
		var w int
		err := cfg.db.QueryRow(`SELECT weight FROM account_tags WHERE username='alice' AND tag='女性'`).Scan(&w)
		if err == sql.ErrNoRows {
			return 0, false
		}
		if err != nil {
			t.Fatal(err)
		}
		return w, true
	}
	const me = "5.6.7.8"         // 经 CF 的真实访客（CF 头优先）
	const other = "198.51.100.9" // 不经 CF 头时按 XFF 右数第 2 个取到的另一个访客

	// 1) 同一身份连投三次 +1 → 只有一票。
	vote(me, me+", 104.22.109.48", 1)
	got := vote(me, me+", 104.22.109.48", 1)
	tagsMap, _ := got["tags"].(map[string]any)
	if tagsMap["女性"] != float64(1) {
		t.Fatalf("同一 IP 连投两次 +1 必须是 1（一 IP 一票，且不看 localStorage）: %+v", tagsMap)
	}
	if w, _ := weight(); w != 1 {
		t.Fatalf("写进 account_tags 的权重应为 1，实际 %d", w)
	}

	// 2) 换一个身份 → 第二票。
	vote("", other+", 104.22.109.48", 1)
	if w, _ := weight(); w != 2 {
		t.Fatalf("两个不同 IP 各投 +1 应是 2，实际 %d", w)
	}

	// 3) 第一个身份反向改票：它贡献的变化量是 -2 → Σ=0 → 恰好归零删行。
	vote(me, me+", 104.22.109.48", -1)
	if w, ok := weight(); ok {
		t.Fatalf("反向改票后应归零删行，实际权重 %d", w)
	}

	// 4) 第一个身份撤票（d=0 合法）：它的票被删，只剩第二个 IP 的 +1 → 权重 1。
	if got = vote(me, me+", 104.22.109.48", 0); got["tags"] == nil {
		t.Fatal("撤票后响应里应还能看到剩余标签")
	}
	if w, ok := weight(); !ok || w != 1 {
		t.Fatalf("撤票后应由剩余票重建为 1，实际 %d/%v", w, ok)
	}
	// 票行必须只剩一张（撤的那张要删掉，否则它以后再也投不了这个标签）
	var n int
	if err := cfg.db.QueryRow(`SELECT COUNT(*) FROM tag_votes WHERE username='alice'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("alice 的票行应剩 1 行，实际 %d", n)
	}

	// 5) 流水：5 次写请求 5 行（幂等的是权重，不是审计），ip 记的是 Principal 而不是
	//    整个 XFF 头串。
	var ips []string
	rows, err := cfg.db.Query(`SELECT ip FROM request_logs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var ip string
		if err := rows.Scan(&ip); err != nil {
			t.Fatal(err)
		}
		ips = append(ips, ip)
	}
	rows.Close()
	if len(ips) != 5 {
		t.Fatalf("应记 5 行流水，实际 %d", len(ips))
	}
	for _, ip := range ips {
		if ip != me && ip != other {
			t.Fatalf("流水 ip 必须是 Principal（单个地址，不是 XFF 串）: %q", ip)
		}
	}

	// 6) 恒等式在整条 HTTP 链路上也成立。
	v, err := tags.New(cfg.db).InvariantViolations()
	if err != nil || len(v) != 0 {
		t.Fatalf("HTTP 写入后必须自洽: %+v %v", v, err)
	}
}

// TestVoteBucketIsPrincipalNotHeaderName 钉住「一 IP 一票」的取值机制：
// 票桶的键是 ipban.Principal **解析后的地址**，不是头名字。所以
//
//	① 同一个地址、两种不同来源头（CF 头 vs XFF 退化）必须落进**同一个**票桶；
//	② 两个不同地址、即便都只给 XFF，也必须落进**两个**票桶。
//
// 配错口径（例如 CF_CONNECTING_IP 关掉、或跳数配错导致归属退化到客户端可自报的
// 最左项）会让全站塌成同一个桶，这条测试就是那件事的哨兵。
func TestVoteBucketIsPrincipalNotHeaderName(t *testing.T) {
	cfg := newTestCfg(t)
	body := `{"user":"alice","tag":"女性","d":1}`
	post := func(cf, xff string) int {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/tag", strings.NewReader(body))
		if cf != "" {
			r.Header.Set("CF-Connecting-IP", cf)
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		w := httptest.NewRecorder()
		handlePostAccountTag(w, r, cfg)
		if w.Code != 200 {
			t.Fatalf("POST -> %d %s", w.Code, w.Body.String())
		}
		var wt int
		err := cfg.db.QueryRow(`SELECT weight FROM account_tags WHERE username='alice' AND tag='女性'`).Scan(&wt)
		if err == sql.ErrNoRows {
			return 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return wt
	}

	// ① 同一个地址两种来源：CF 头给 5.6.7.8；只给 XFF 时右数第 2 个也是 5.6.7.8
	//    （默认 TRUSTED_PROXY_HOPS=2，链尾是 CF 边缘）→ 同一票桶。
	if w := post("5.6.7.8", "5.6.7.8, 104.22.109.48"); w != 1 {
		t.Fatalf("首次投票应为 1，实际 %d", w)
	}
	if w := post("", "5.6.7.8, 104.22.109.48"); w != 1 {
		t.Fatalf("同一地址换一种头来源必须还是 1（票桶=解析后的 Principal），实际 %d", w)
	}
	// ② 不同地址（XFF 右数第 2 个不同）→ 第二个桶
	if w := post("", "198.51.100.9, 104.22.109.48"); w != 2 {
		t.Fatalf("不同真实访客应落进第二个票桶，实际 %d", w)
	}
	// ③ 直连（无任何代理头，例如本机/健康检查）→ 第三个桶
	if w := post("203.0.113.9", ""); w != 3 {
		t.Fatalf("直连访客应是独立票桶，实际 %d", w)
	}
	var ips []string
	rows, _ := cfg.db.Query(`SELECT ip FROM tag_votes ORDER BY ip`)
	for rows.Next() {
		var ip string
		rows.Scan(&ip)
		ips = append(ips, ip)
	}
	rows.Close()
	want := []string{"198.51.100.9", "203.0.113.9", "5.6.7.8"}
	if strings.Join(ips, ",") != strings.Join(want, ",") {
		t.Fatalf("票桶必须正好是这三个 Principal 地址，实际 %v", ips)
	}
	// ④ 伪造的前缀不参与：把 8.8.8.8 塞在最左，仍归到 5.6.7.8 那个桶，不加票。
	if w := post("", "8.8.8.8, 5.6.7.8, 104.22.109.48"); w != 3 {
		t.Fatalf("最左项可自报，不该被当成新访客，实际 %d", w)
	}
}

// itoa 小工具，避免为拼 JSON 引入 strconv 到测试顶部之外。
func itoa(v int) string {
	if v < 0 {
		return "-" + itoa(-v)
	}
	if v < 10 {
		return string(rune('0' + v))
	}
	return itoa(v/10) + string(rune('0'+v%10))
}

// TestTagsGetUsesTable 钉住 GET 直接用新数据源：表里有就返回，不依赖任何 JSON 文件。
func TestTagsGetUsesTable(t *testing.T) {
	cfg := newTestCfg(t)
	seedTags(t, cfg, "bob", map[string]int{"COS": 2, "男娘": -1})

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
		// 用 CF-Connecting-IP 表达「不同访客」：Principal 优先读它，等价于线上
		// 两个不同真实客户端。XFF 留给 ipban 自己的用例覆盖。
		r.Header.Set("CF-Connecting-IP", ip)
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
	// 配额按**请求次数**扣，票按 (账号,标签,IP) 记：同一个人发 3 次请求扣 3 点配额，
	// 但只有 1 票。这两件事分开计数是"一 IP 一票"的必然结果。
	if w := cfg.tags.Weights("a"); w["t"] != 1 {
		t.Fatalf("同一 IP 的 3 次重复投票权重应停在 1，实际 %+v", w)
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
		`{"user":"alice","tag":"女性","d":2}`, // 目标值只允许 {-1,0,1}，越界是客户端 bug，不静默夹
		`{"user":"alice","tag":"女性","d":-2}`,
		`{"user":"alice","tag":"女性"}`, // d 缺失：0 是撤票，不能拿缺省值当撤票
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
