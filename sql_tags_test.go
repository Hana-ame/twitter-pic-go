package twitter

import (
	"database/sql"
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
