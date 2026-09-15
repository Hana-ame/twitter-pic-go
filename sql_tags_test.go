package twitter

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func setupTagsDB(t *testing.T) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	DB = db
	t.Cleanup(func() { db.Close() })
}

func TestAccountTagsRoundTrip(t *testing.T) {
	setupTagsDB(t)

	// 旧表先塞一行 legacy JSON，验证建表时的回填
	if err := CreateTable(); err != nil {
		t.Fatal(err)
	}
	if _, err := DB.Exec("CREATE TABLE IF NOT EXISTS user_tags (username TEXT PRIMARY KEY, tags TEXT DEFAULT '{}', last_modify TIMESTAMP)"); err != nil {
		t.Fatal(err)
	}
	if _, err := DB.Exec(`INSERT INTO user_tags (username, tags) VALUES ('legacy1', '{"COS":2,"零":0}')`); err != nil {
		t.Fatal(err)
	}
	if _, err := DB.Exec(`INSERT INTO users (username, status) VALUES ('legacy1','SUCCESS'),('u1','SUCCESS')`); err != nil {
		t.Fatal(err)
	}
	if err := CreateTableV2(); err != nil {
		t.Fatal(err)
	}

	// 回填：COS=2 进新表，权重 0 不迁
	u, err := getUserTags("legacy1")
	if err != nil {
		t.Fatal(err)
	}
	if u.Tags["COS"] != 2 || len(u.Tags) != 1 {
		t.Fatalf("回填错误: %+v", u.Tags)
	}

	// POST：新账号打标签
	if err := addTag("u1", map[string]int{"女性": 1, "足控": 1}, "ip", "ua"); err != nil {
		t.Fatal(err)
	}
	u, _ = getUserTags("u1")
	if u.Tags["女性"] != 1 || u.Tags["足控"] != 1 {
		t.Fatalf("addTag: %+v", u.Tags)
	}

	// 累加与归零删除（旧语义：恰好 0 删行，负权重保留）
	if err := addTag("u1", map[string]int{"女性": 1, "足控": -1, "男娘": -1}, "ip", "ua"); err != nil {
		t.Fatal(err)
	}
	u, _ = getUserTags("u1")
	if u.Tags["女性"] != 2 {
		t.Fatalf("累加: %+v", u.Tags)
	}
	if _, ok := u.Tags["足控"]; ok {
		t.Fatalf("归零应删除: %+v", u.Tags)
	}
	if u.Tags["男娘"] != -1 {
		t.Fatalf("负权重应保留: %+v", u.Tags)
	}

	// 反查走 tag 索引
	rows, err := DB.Query(`SELECT username FROM account_tags WHERE tag = ?`, "女性")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		got = append(got, s)
	}
	rows.Close()
	if len(got) != 1 || got[0] != "u1" {
		t.Fatalf("反查: %v", got)
	}

	// user_tags 不再被 POST 写入
	var n int
	DB.QueryRow(`SELECT COUNT(*) FROM user_tags WHERE username='u1'`).Scan(&n)
	if n != 0 {
		t.Fatal("POST 不应再写 user_tags")
	}
}

// TestGetUserListByTag 钉住「tag 查 user」端点的契约：
// 权重降序、精确匹配（非 LIKE）、只给 SUCCESS 账号、空结果返回 [] 而不是 null。
func TestGetUserListByTag(t *testing.T) {
	setupTagsDB(t)
	if err := CreateTableV2(); err != nil {
		t.Fatal(err)
	}
	if _, err := DB.Exec(`INSERT INTO users (username, nick, status) VALUES
		('heavy','H','SUCCESS'),('light','L','SUCCESS'),('other','O','SUCCESS'),('ban','B','BANNED')`); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		u, tag string
		w      int
	}{
		{"light", "女性", 1}, {"heavy", "女性", 5},
		{"other", "男女性交", 3}, {"ban", "女性", 99},
	} {
		if err := addTag(c.u, map[string]int{c.tag: c.w}, "ip", "ua"); err != nil {
			t.Fatal(err)
		}
	}

	got, err := getUserListByTag("女性")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Username != "heavy" || got[1].Username != "light" {
		t.Fatalf("应按权重降序且排除 BANNED: %+v", got)
	}
	// 返回形态与其他 by 分支一致：带 tags / last_modify / status
	if got[0].Tags["女性"] != 5 || got[0].Status != "SUCCESS" || got[0].LastModify.IsZero() {
		t.Fatalf("User 形态不对: %+v", got[0])
	}

	// 精确匹配：「女」不该命中「女性」
	if r, _ := getUserListByTag("女"); len(r) != 0 {
		t.Fatalf("标签必须精确匹配，不该子串命中: %+v", r)
	}

	// 经 getSearch 派发（真实 HTTP 分支）
	r, err := getSearch("tag", "女性")
	if err != nil || len(r) != 2 {
		t.Fatalf("getSearch(by=tag): %v %+v", err, r)
	}
	// 空结果要是 [] 不是 null，否则客户端 JSON 解析会炸
	e, err := getSearch("tag", "没这个标签")
	if err != nil || e == nil || len(e) != 0 {
		t.Fatalf("空结果应是非 nil 空切片: err=%v got=%v", err, e)
	}
	if b, _ := json.Marshal(e); string(b) != "[]" {
		t.Fatalf("空结果序列化应为 []，实际 %s", b)
	}
}
