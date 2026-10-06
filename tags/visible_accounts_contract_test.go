package tags

import (
	"database/sql"
	"testing"
)

// 「标签反查返回的账号必须真的点得进去」这条契约。
//
// 2026-10-06 实测线上（x.moonchan.xyz）抓到的故障：
//
//	GET /api/tag/女性?limit=25&offset=0  → 200, users=25
//	把这 25 人逐个 GET /api/twitter/<u>.json.gz → 12 个 200、**13 个 404**
//	404 体固定为 {"error":"查询用户失败: 没有进入 rows.Next()"}
//
// 根因：标签索引（account_tags / user_tag_cnt）里可以有**在 users 表里根本
// 没有对应行**的账号。原先的封禁排除写成
//
//	AND NOT EXISTS (SELECT 1 FROM users u WHERE u.username = a.username
//	                 AND (u.status IS NULL OR u.status != 'SUCCESS'))
//
// 这个 NOT EXISTS 是**双重否定**：它只排除「在 users 表里且 status 不对」的
// 行。而「压根不在 users 表」的账号让整个子查询无行 → NOT EXISTS 为真 →
// **被放行**。于是服务端把一批注定 404 的账号发给了客户端，
// 客户端每页要发起 25 次注定失败的往返（累计实测 30s），这就是「下一页极慢」。
//
// 正确口径是**存在性**：只有真的在 users 表里、且 status = 'SUCCESS' 的账号，
// 才对客户端可见。

func seedUser(t *testing.T, s *Store, username, status string) {
	t.Helper()
	if _, err := s.db.Exec(`INSERT INTO users (username, status) VALUES (?, ?)`, username, status); err != nil {
		t.Fatal(err)
	}
}

func TestUsersForTagPagedExcludesAccountsMissingFromUsersTable(t *testing.T) {
	s := newBareStore(t)
	openUsers(t, s)

	// alive：在 users 表里且 status 正常。
	seedUser(t, s, "alive", "SUCCESS")
	seedWeights(t, s, "alive", map[string]int{"女性": 3})

	// ghost：**users 表里根本没有这一行**（标签索引有、用户表无）。
	seedWeights(t, s, "ghost", map[string]int{"女性": 2})

	// banned：在 users 表里但被封。
	seedUser(t, s, "banned", "BANNED")
	seedWeights(t, s, "banned", map[string]int{"女性": 1})

	users, total, err := s.UsersForTagPaged("女性", 25, 0, true)
	if err != nil {
		t.Fatal(err)
	}

	for _, u := range users {
		if u == "ghost" {
			t.Fatal("ghost 不在 users 表里，点进去必然 404，绝不能出现在标签反查结果中")
		}
	}
	if len(users) != 1 || users[0] != "alive" {
		t.Fatalf("只应返回 alive，实际 %v", users)
	}

	// total 同样不能把 ghost 算进去：客户端拿它算「已列出 N 人（共 T 人）」，
	// 也据此判断是否还有下一页（TagUserPage.isLastPage）。
	if total != 1 {
		t.Fatalf("total 应为 1（ghost 不存在、banned 被封），实际 %d", total)
	}
}

// 顺序仍按权重降序，且**存在的账号**之间不受影响——这条防止修复时
// 把排除条件写得过宽，把好账号也误伤。
func TestUsersForTagPagedKeepsWeightOrderAmongVisible(t *testing.T) {
	s := newBareStore(t)
	openUsers(t, s)

	for _, f := range []struct {
		name   string
		weight int
	}{{"low", 1}, {"mid", 5}, {"high", 9}} {
		seedUser(t, s, f.name, "SUCCESS")
		seedWeights(t, s, f.name, map[string]int{"女性": f.weight})
	}
	// 权重最高、但用户表里没有——必须被排除，且排除后剩下的仍按权重降序。
	seedWeights(t, s, "ghost_top", map[string]int{"女性": 100})

	users, _, err := s.UsersForTagPaged("女性", 25, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"high", "mid", "low"}
	if len(users) != len(want) {
		t.Fatalf("期望 %v，实际 %v", want, users)
	}
	for i := range want {
		if users[i] != want[i] {
			t.Fatalf("期望 %v，实际 %v（ghost_top 权重最高仍须被排除）", want, users)
		}
	}
}

// 分页 offset 建立在「total 已排除不可见账号」之上。若 total 把 ghost 算进去，
// 客户端会一直翻页翻到空页为止。
func TestUsersForTagPagedPagingDoesNotDriftPastVisibleTotal(t *testing.T) {
	s := newBareStore(t)
	openUsers(t, s)

	seedUser(t, s, "alive1", "SUCCESS")
	seedWeights(t, s, "alive1", map[string]int{"女性": 5})
	seedWeights(t, s, "ghost1", map[string]int{"女性": 4})
	seedWeights(t, s, "ghost2", map[string]int{"女性": 3})

	_, total, err := s.UsersForTagPaged("女性", 25, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Fatalf("total 应为 1，实际 %d —— ghost 被算进去会让客户端多翻两页空页", total)
	}

	users, _, err := s.UsersForTagPaged("女性", 25, 1, true) // offset 已在可见集合之外
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 0 {
		t.Fatalf("offset=1 应为空，实际 %v", users)
	}
}

var _ = sql.ErrNoRows
