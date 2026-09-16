package tags

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "tags.db")+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	// 时间排序源在统一库裡是 users 表，测试里补一张最小结构。
	if _, err := db.Exec(`CREATE TABLE users (username TEXT PRIMARY KEY, last_modify TIMESTAMP)`); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

// seedWeights 造"历史底数"：绕过写路径直接插 account_tags 权重，再快照底数。
// 这是升级后旧数据的真实形状（权重是票系统之前攒下来的），读路径用例只关心
// 最终权重值，不该依赖投票过程——过程由 votes_test.go 钉。
func seedWeights(t *testing.T, s *Store, user string, w map[string]int) {
	t.Helper()
	for tag, v := range w {
		if _, err := s.db.Exec(`INSERT OR REPLACE INTO account_tags (username, tag, weight) VALUES (?, ?, ?)`,
			user, tag, v); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := BackfillVoteBase(s.db); err != nil {
		t.Fatal(err)
	}
}

// voteN 从 n 个不同 IP 各投 target 票，等价于"贡献 n×target 的权重增量"。
func voteN(t *testing.T, s *Store, user, tag string, n int, target int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if err := s.CastVotes(user, fmt.Sprintf("10.1.%d.%d", n, i), map[string]int{tag: target}, "ua"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestVoteKeepsLegacyRollupSemantics 钉住滚动值的三条既有语义：
// 多个 IP 叠加、恰好归零删行、负权重保留。
func TestVoteKeepsLegacyRollupSemantics(t *testing.T) {
	s := newTestStore(t)

	seedWeights(t, s, "u1", map[string]int{"COS": 2, "男娘": -1})
	voteN(t, s, "u1", "女性", 2, VoteUp)    // 两个 IP 各 +1
	voteN(t, s, "u1", "COS", 2, VoteDown) // 把底数 2 减到 0
	voteN(t, s, "u1", "男娘", 1, VoteDown)  // -1 底数上再 -1 → -2

	w := s.Weights("u1")
	if w["女性"] != 2 {
		t.Fatalf("累加失败: %+v", w)
	}
	if _, ok := w["COS"]; ok {
		t.Fatalf("恰好归零应删行: %+v", w)
	}
	if w["男娘"] != -2 {
		t.Fatalf("负权重应保留可续投: %+v", w)
	}
}

// TestRequestLogRetained 钉住用户要求：每次写请求都要留下 request_logs 流水。
func TestRequestLogRetained(t *testing.T) {
	s := newTestStore(t)

	for _, tag := range []string{"女性", "足控"} {
		if err := s.CastVotes("u1", "9.9.9.9", map[string]int{tag: VoteUp}, "curl/8"); err != nil {
			t.Fatal(err)
		}
	}

	var n int
	var ip, ua, tagJSON string
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("两次写应有两行流水，实际 %d", n)
	}
	if err := s.DB().QueryRow(`SELECT username, tags, ip, ua FROM request_logs ORDER BY id LIMIT 1`).
		Scan(new(string), &tagJSON, &ip, &ua); err != nil {
		t.Fatal(err)
	}
	if ip != "9.9.9.9" || ua != "curl/8" {
		t.Fatalf("流水缺 ip/ua: %q %q", ip, ua)
	}
	if tagJSON != `{"女性":1}` {
		t.Fatalf("流水未记录本次标签: %s", tagJSON)
	}
}

// TestReadPaths 钉住读侧：展示按权重、反查与标签云只算正权重。
func TestReadPaths(t *testing.T) {
	s := newTestStore(t)

	seedWeights(t, s, "a", map[string]int{"女性": 3, "男娘": -1})
	seedWeights(t, s, "b", map[string]int{"女性": 1})

	if got := s.ForUsers([]string{"a", "b"})["a"]; len(got) != 1 || got[0] != "女性" {
		t.Fatalf("ForUsers 应只给正权重标签: %v", got)
	}
	users := s.UsersForTag("女性", nil, 10)
	if len(users) != 2 || users[0] != "a" { // 权重降序
		t.Fatalf("UsersForTag 顺序/内容错: %v", users)
	}
	if got := s.UsersForTag("男娘", nil, 10); len(got) != 0 {
		t.Fatalf("负权重不应被反查到: %v", got)
	}
	cloud := s.Cloud(10, false)
	if len(cloud) != 1 || cloud[0].Tag != "女性" || cloud[0].Count != 2 {
		t.Fatalf("Cloud 错: %+v", cloud)
	}

	if _, err := s.DB().Exec(`INSERT INTO users (username, last_modify) VALUES ('a','2026-09-15 10:00:00')`); err != nil {
		t.Fatal(err)
	}
	if lm := s.LastModifyFor([]string{"a"})["a"]; lm == "" {
		t.Fatal("LastModifyFor 应能从 users 表读到时间")
	}
}

// TestNilStoreDegrades 钉住降级：库不可用时读路径返回空而不是 panic。
func TestNilStoreDegrades(t *testing.T) {
	var s *Store
	if len(s.Weights("u")) != 0 || len(s.ForUsers([]string{"u"})) != 0 || s.UsersForTag("t", nil, 10) != nil {
		t.Fatal("nil store 读路径应安全降级")
	}
	if err := (*Store)(nil).CastVotes("u", "ip", map[string]int{"t": 1}, "ua"); err == nil {
		t.Fatal("nil store 写入应报错")
	}
}

// TestTimestampOrderingInvariant 钉住首页排序依赖的不变量：
// 无论驱动回 time.Time / RFC3339 / 裸文本，输出都是定宽格式，字典序 == 时间序。
func TestTimestampOrderingInvariant(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.DB().Exec(`INSERT INTO users (username, last_modify) VALUES
		('old','2026-01-02 03:04:05'),
		('new','2026-09-15 10:00:00')`); err != nil {
		t.Fatal(err)
	}
	m := s.LastModifyFor([]string{"old", "new"})
	if len(m) != 2 {
		t.Fatalf("两行都应读到: %+v", m)
	}
	for u, v := range m {
		if len(v) != len("2026-01-02 15:04:05") {
			t.Fatalf("%s 的时间不是定宽 %q（排序会退化成字典序错乱）", u, v)
		}
	}
	if !(m["new"] > m["old"]) {
		t.Fatalf("字典序应等于时间序: new=%q old=%q", m["new"], m["old"])
	}
}

// newBareStore 只建 tags 包拥有的表，**不**建 users——用于测「users 表不存在」这条
// 降级路径（newTestStore 为了时间排序会预建一张最小 users，盖不到这个分支）。
func newBareStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "twitter.db")+"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

// openUsers 建带 status 的 users 表（真源里由根包 CreateTableV2 建，tags 包不拥有它，
// 但 BannedUsernames 与 Cloud 的封禁排除都要读它，所以测试自己造）。
func openUsers(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.db.Exec(`CREATE TABLE users (username TEXT PRIMARY KEY, status TEXT)`); err != nil {
		t.Fatal(err)
	}
}

// TestBannedUsernamesSemantics 钉住"谁算被封"的判定，以及两种"空"必须可分。
func TestBannedUsernamesSemantics(t *testing.T) {
	s := newBareStore(t)
	seedWeights(t, s, "ok", map[string]int{"女性": 1})
	seedWeights(t, s, "ghost", map[string]int{"女性": 1})

	// 没有 users 表 = 读不到（error 非 nil），调用方据此 fail-open；
	// 这必须与"有表但一个都没封"（error 为 nil + 空切片）区分开。
	if _, err := s.BannedUsernames(); err == nil {
		t.Fatal("users 表缺失时必须报 error，不能当成『一个都没封』")
	}

	openUsers(t, s)
	list, err := s.BannedUsernames()
	if err != nil || len(list) != 0 {
		t.Fatalf("有表但无行应返回空且无错: %v %v", list, err)
	}

	for _, tc := range []struct{ u, status any }{
		{"banned", "BANNED"},
		{"empty", ""},    // 根 API 要求 = 'SUCCESS'，空串同样算被封
		{"nullish", nil}, // NULL 同理
	} {
		if _, err := s.db.Exec(`INSERT INTO users (username, status) VALUES (?, ?)`, tc.u, tc.status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO users (username, status) VALUES ('ok', 'SUCCESS')`); err != nil {
		t.Fatal(err)
	}
	list, err = s.BannedUsernames()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, u := range list {
		got[u] = true
	}
	for _, u := range []string{"banned", "empty", "nullish"} {
		if !got[u] {
			t.Fatalf("%s 应算被封: %v", u, list)
		}
	}
	if got["ok"] {
		t.Fatalf("SUCCESS 不该出现在封禁列表: %v", list)
	}
	// ghost 没有 users 行 → 不在列表里（"没注册过" ≠ "被封"）
	if got["ghost"] {
		t.Fatalf("无 users 行的账号不该被顺带隐藏: %v", list)
	}
}

// TestCloudBanExclusion 钉住标签云的两种模式：false = 全量（根 API 原语义），
// true = SQL 侧排除被封账号；且 users 表缺失时 true 要能退回而不是返回空。
func TestCloudBanExclusion(t *testing.T) {
	s := newBareStore(t)
	seedWeights(t, s, "alice", map[string]int{"共有": 1, "仅正常": 1})
	seedWeights(t, s, "bob", map[string]int{"共有": 1, "仅被封": 1})

	// 表缺失：不排除才有结果；排除应自动退回全量（fail-open）而不是清空标签云
	if c := s.Cloud(10, true); len(c) != 3 {
		t.Fatalf("users 表缺失时应退回全量聚合，实际 %+v", c)
	}

	openUsers(t, s)
	if _, err := s.db.Exec(`INSERT INTO users VALUES ('alice','SUCCESS'),('bob','BANNED')`); err != nil {
		t.Fatal(err)
	}
	InvalidateCloud(s.db)
	all := s.Cloud(10, false)
	if len(all) != 3 {
		t.Fatalf("不带排除时应看到全部 3 个标签，实际 %+v", all)
	}
	vis := s.Cloud(10, true)
	m := map[string]int{}
	for _, c := range vis {
		m[c.Tag] = c.Count
	}
	if len(vis) != 2 {
		t.Fatalf("「仅被封」该整条消失，实际 %+v", vis)
	}
	if m["共有"] != 1 {
		t.Fatalf("「共有」应扣成 1，实际 %+v", m)
	}
	if _, ok := m["仅被封"]; ok {
		t.Fatalf("只剩被封账号的标签不该出现: %+v", m)
	}
	// 负权重本来就不进云；确认排除没把它带回来
	seedWeights(t, s, "alice", map[string]int{"负": -1})
	for _, c := range s.Cloud(10, true) {
		if c.Tag == "负" {
			t.Fatalf("排除封禁不该改变权重口径: %+v", s.Cloud(10, true))
		}
	}
}
