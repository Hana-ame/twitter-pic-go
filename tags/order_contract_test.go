package tags

import (
	"fmt"
	"testing"
)

// 本文件钉的是**跨端反查契约**（见 notes/design-twitter-pic-flutter-tag-filter-contract）：
// 「服务端按 account_tags 正权重降序返回」。
//
// 判据刻意写成**可证伪**的顺序断言，而不是断言「结果里含有某某」——
// 后者在实现退化成「按插入顺序返回」时照样通过。这正是本项目反复踩的
// 「断言方向写反 / 断言太弱」反模式（见 notes/ci-twitter-pic-flutter-v05-churn-patterns
// 模式 E）：测试全绿不能证明行为正确，**错误实现也要能让它变红**。

// 反向验证（必须做）：把 UsersForTagPaged 里的
//   `ORDER BY a.updated_at DESC, a.username ASC`
// 改成 `ORDER BY a.username ASC`（假装排序没了）后，本文件的
// TestUsersForTagPagedOrdersByWeightDesc 断言必须失败。

// seedWeightRows 直接铺 user_tag_cnt，并让 updated_at 与 weight **反向**：
// 权重高的行 updated_at 更旧。这样「按 updated_at 排」与「按权重降序排」
// 会给出**相反**的顺序，任何一方的实现都骗不过另一方的断言。
func seedWeightRows(t *testing.T, s *Store, rows map[string]map[string]int) {
	t.Helper()
	// username -> tag -> cnt；updated_at 反序排：第一个 username 最新。
	order := []string{"d_newest", "c_middle", "b_older", "a_oldest", "e_heaviest_old"}
	for i, u := range order {
		for tag, cnt := range rows[u] {
			// updated_at 递减：i 越大越旧
			ts := fmt.Sprintf("2020-01-%02d 00:00:00", 28-i)
			if _, err := s.db.Exec(
				`INSERT OR REPLACE INTO user_tag_cnt (username, tag, cnt, updated_at) VALUES (?, ?, ?, ?)`,
				u, tag, cnt, ts); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestUsersForTagPagedOrdersByWeightDesc 证明主路径（user_tag_cnt）按权重降序。
//
// 修复前：`ORDER BY updated_at DESC` 会返回 newest→oldest 的 d,c,b,a,e，
// 而 e 权重最高 —— 与契约相反，本断言失败。
func TestUsersForTagPagedOrdersByWeightDesc(t *testing.T) {
	s := newTestStore(t)
	seedWeightRows(t, s, map[string]map[string]int{
		"a_oldest":     {"COS": 1},
		"b_older":      {"COS": 5},
		"c_middle":     {"COS": 3},
		"d_newest":     {"COS": 2},
		"e_heaviest_old": {"COS": 9},
	})

	got, _, err := s.UsersForTagPaged("COS", 0, 0, false)
	if err != nil {
		t.Fatalf("UsersForTagPaged: %v", err)
	}

	want := []string{"e_heaviest_old", "b_older", "c_middle", "d_newest", "a_oldest"}
	if !sameOrder(got, want) {
		t.Fatalf("按权重降序契约失败:\n got = %v\nwant = %v", got, want)
	}
}

// TestUsersForTagPagedStableOnEqualWeight 同权重按 username 升序（稳定序）。
// 这条同样可证伪：把 tie-break 去掉（只 ORDER BY cnt DESC）后排序由 SQLite
// 自行决定，断言不再稳定。
func TestUsersForTagPagedStableOnEqualWeight(t *testing.T) {
	s := newTestStore(t)
	seedWeightRows(t, s, map[string]map[string]int{
		"a_oldest":       {"COS": 3},
		"b_older":        {"COS": 3},
		"c_middle":       {"COS": 3},
		"d_newest":       {"COS": 3},
		"e_heaviest_old": {"COS": 3},
	})

	got, _, err := s.UsersForTagPaged("COS", 0, 0, false)
	if err != nil {
		t.Fatalf("UsersForTagPaged: %v", err)
	}
	want := []string{"a_oldest", "b_older", "c_middle", "d_newest", "e_heaviest_old"}
	if !sameOrder(got, want) {
		t.Fatalf("同权重应按 username 升序:\n got = %v\nwant = %v", got, want)
	}
}

// TestUsersForTagPagedExcludesNonPositive 反查只认正权重：cnt<=0 的账号
// 不该出现在结果里（与 account_tags 分支的 `weight > 0` 对齐）。
// 可证伪：把 `cnt > 0` 改成 `cnt >= -99` 后 neg/zero 两行会混进结果。
func TestUsersForTagPagedExcludesNonPositive(t *testing.T) {
	s := newTestStore(t)
	seedWeightRows(t, s, map[string]map[string]int{
		"a_oldest":     {"COS": 1},
		"b_older":      {"COS": 0}, // 撤票后归零：不该出现
		"c_middle":     {"COS": -1},
		"d_newest":     {"COS": 2},
		"e_heaviest_old": {"COS": 5},
	})

	got, _, err := s.UsersForTagPaged("COS", 0, 0, false)
	if err != nil {
		t.Fatalf("UsersForTagPaged: %v", err)
	}
	want := []string{"e_heaviest_old", "d_newest", "a_oldest"}
	if !sameOrder(got, want) {
		t.Fatalf("只应返回正权重账号（且权重降序）:\n got = %v\nwant = %v", got, want)
	}
}

// TestUsersForTagPagedExcludesBannedByDefault 封禁账号不该出现在反查里。
// 可证伪：把 excludeBanned 传 true 的分支去掉后 banned 用户会混进结果。
func TestUsersForTagPagedExcludesBannedByDefault(t *testing.T) {
	s := newTestStore(t)
	// newTestStore 建的是最小 users 表（无 status 列），这里补齐。
	if _, err := s.db.Exec(`ALTER TABLE users ADD COLUMN status TEXT`); err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"a_oldest", "b_older", "c_middle", "d_newest", "e_heaviest_old"} {
		st := "SUCCESS"
		if u == "b_older" {
			st = "BANNED"
		}
		if _, err := s.db.Exec(`INSERT OR REPLACE INTO users (username, status) VALUES (?, ?)`, u, st); err != nil {
			t.Fatal(err)
		}
	}
	seedWeightRows(t, s, map[string]map[string]int{
		"a_oldest": {"COS": 1}, "b_older": {"COS": 5}, "c_middle": {"COS": 3},
		"d_newest": {"COS": 2}, "e_heaviest_old": {"COS": 4},
	})

	got, _, err := s.UsersForTagPaged("COS", 0, 0, true)
	if err != nil {
		t.Fatalf("UsersForTagPaged: %v", err)
	}
	for _, u := range got {
		if u == "b_older" {
			t.Fatalf("封禁账号 b_older 不该出现在反查里，got = %v", got)
		}
	}
}

func sameOrder(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}