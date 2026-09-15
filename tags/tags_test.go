package tags

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "tags.db"))
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

// TestAddSemantics 钉住两层的写语义：累加、恰好归零删行、负权重保留。
func TestAddSemantics(t *testing.T) {
	s := newTestStore(t)

	if err := s.Add("u1", map[string]int{"女性": 1, "COS": 2}, "1.2.3.4", "ua"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("u1", map[string]int{"女性": 1, "COS": -2, "男娘": -1}, "1.2.3.4", "ua"); err != nil {
		t.Fatal(err)
	}

	w := s.Weights("u1")
	if w["女性"] != 2 {
		t.Fatalf("累加失败: %+v", w)
	}
	if _, ok := w["COS"]; ok {
		t.Fatalf("恰好归零应删行: %+v", w)
	}
	if w["男娘"] != -1 {
		t.Fatalf("负权重应保留: %+v", w)
	}
}

// TestRequestLogRetained 钉住用户要求：每次写请求都要留下 request_logs 流水。
func TestRequestLogRetained(t *testing.T) {
	s := newTestStore(t)

	if err := s.Add("u1", map[string]int{"女性": 1}, "9.9.9.9", "curl/8"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("u1", map[string]int{"足控": 1}, "9.9.9.9", "curl/8"); err != nil {
		t.Fatal(err)
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

	if err := s.Add("a", map[string]int{"女性": 3, "男娘": -1}, "ip", "ua"); err != nil {
		t.Fatal(err)
	}
	if err := s.Add("b", map[string]int{"女性": 1}, "ip", "ua"); err != nil {
		t.Fatal(err)
	}

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
	cloud := s.Cloud(10)
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
	if err := s.Add("u", map[string]int{"t": 1}, "ip", "ua"); err == nil {
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
