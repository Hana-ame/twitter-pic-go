package tags

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// openStore 用**与生产一致的连接串**开一个临时库。
// 两个参数都不是可选的装饰：
//   - `_txlock=immediate`：CastVotes 靠它在第一条语句前拿到写锁，缺了就可能在
//     并发下丢票（见 CastVotes 的注释）。不加它，本文件的并发测试测的是另一条路径。
//   - `busy_timeout`：撞锁时排队等待而不是立刻 SQLITE_BUSY 失败。
func openStore(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "twitter.db")+
		"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

// TestSameIPHundredVotesCountOnce 需求 1：同一 IP 对同一 (账号,标签) 连发 100 次 +1，
// 权重只能 +1。这是"一 IP 一票"的定义性用例，且不依赖任何浏览器本地状态。
func TestSameIPHundredVotesCountOnce(t *testing.T) {
	s := openStore(t)
	for i := 0; i < 100; i++ {
		if err := s.CastVotes("alice", "1.2.3.4", map[string]int{"女性": VoteUp}, "ua"); err != nil {
			t.Fatalf("第 %d 次: %v", i, err)
		}
	}
	if w := s.Weights("alice")["女性"]; w != 1 {
		t.Fatalf("连点 100 次后权重应为 1，实际 %d", w)
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tag_votes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("票账本应只有 1 行，实际 %d", n)
	}
	// 流水仍然每次一行（幂等的是权重，不是审计）
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM request_logs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 100 {
		t.Fatalf("request_logs 应记 100 行，实际 %d", n)
	}
}

// TestSecondIPGetsSecondVote 需求 2：换一个 IP 再投 +1 → 权重 +2。
func TestSecondIPGetsSecondVote(t *testing.T) {
	s := openStore(t)
	if err := s.CastVotes("alice", "1.1.1.1", map[string]int{"女性": 1}, "ua"); err != nil {
		t.Fatal(err)
	}
	if err := s.CastVotes("alice", "2.2.2.2", map[string]int{"女性": 1}, "ua"); err != nil {
		t.Fatal(err)
	}
	if w := s.Weights("alice")["女性"]; w != 2 {
		t.Fatalf("两个 IP 各投 +1，权重应为 2，实际 %d", w)
	}
	// 同分标签仍只出现一次（一个 IP 一行，一个 (账号,标签) 在 account_tags 也只一行）
	if got := s.UsersForTag("女性", nil, 10); len(got) != 1 {
		t.Fatalf("反查不该出现重复账号: %v", got)
	}
}

// TestFlipAndWithdraw 需求 3：同一 IP 从 +1 改成 -1 → 净变化 -2（夹取夹的是目标值，
// 允许差值 ±2）；再撤票 → 回到本次改动之前的值。
func TestFlipAndWithdraw(t *testing.T) {
	s := openStore(t)
	ip := "9.9.9.9"
	if err := s.CastVotes("bob", ip, map[string]int{"露脸": 1}, "ua"); err != nil {
		t.Fatal(err)
	}
	if w := s.Weights("bob")["露脸"]; w != 1 {
		t.Fatalf("首投后应为 1，实际 %d", w)
	}
	// +1 → -1：变化量是 -2。旧实现夹输入到 ±1 会把这一步变成"归零"，那是死结。
	if err := s.CastVotes("bob", ip, map[string]int{"露脸": -1}, "ua"); err != nil {
		t.Fatal(err)
	}
	if w := s.Weights("bob")["露脸"]; w != -1 {
		t.Fatalf("反向改票应得 -1（夹目标值，差值可达 ±2），实际 %d", w)
	}
	// 撤票 → 回到 -1 之前、也就是首投 +1 之后？不对：撤票是清掉这个 IP 的票，
	// 该 IP 之外没有别人的票，底数是 0 → 权重回 0 → 整行被删（归零删行）。
	if err := s.CastVotes("bob", ip, map[string]int{"露脸": 0}, "ua"); err != nil {
		t.Fatal(err)
	}
	if w := s.Weights("bob")["露脸"]; w != 0 {
		t.Fatalf("撤票后权重应归零，实际 %d", w)
	}
	if m := s.Weights("bob"); len(m) != 0 {
		t.Fatalf("归零应删 account_tags 行，实际剩 %v", m)
	}
}

// TestWithdrawDeletesVoteRow 需求 4：撤票删票行，不留在账本里影响后续；
// 且删掉之后同一 IP 还能重新投。
func TestWithdrawDeletesVoteRow(t *testing.T) {
	s := openStore(t)
	ip := "8.8.8.8"
	_ = s.CastVotes("carol", ip, map[string]int{"自拍": 1}, "ua")
	_ = s.CastVotes("carol", ip, map[string]int{"自拍": 0}, "ua")

	vs, err := s.VotesFor("carol", "自拍")
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 0 {
		t.Fatalf("撤票后票行必须被删掉，实际 %+v", vs)
	}
	// 重新投票必须能生效（这条断言的是"撤票没把 IP 永久拉黑"）
	if err := s.CastVotes("carol", ip, map[string]int{"自拍": 1}, "ua"); err != nil {
		t.Fatal(err)
	}
	if w := s.Weights("carol")["自拍"]; w != 1 {
		t.Fatalf("撤票后同一 IP 应能重投，权重实际 %d", w)
	}
	// 无意义的撤票（本来就没票）不得造出任何行
	if err := s.CastVotes("carol", "7.7.7.7", map[string]int{"不存在": 0}, "ua"); err != nil {
		t.Fatal(err)
	}
	if m := s.Weights("carol"); len(m) != 1 {
		t.Fatalf("空撤票不该产生副作用，实际 %v", m)
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM tag_weight_base WHERE tag='不存在'`).Scan(&n)
	if n != 0 {
		t.Fatal("空撤票不该顺带造出底数行")
	}
}

// TestBaseSnapshotPreservesHistory 需求 5：迁移前已有的权重原样保留当底数，
// 迁移只快照、不清零，之后 weight = 底数 + Σ票 成立。
func TestBaseSnapshotPreservesHistory(t *testing.T) {
	s := openStore(t)
	// 模拟历史数据：绕过写路径直接插权重（旧库回填后的样子）
	if _, err := s.db.Exec(`INSERT INTO account_tags VALUES ('dave','女性',37),('dave','大奶',-4)`); err != nil {
		t.Fatal(err)
	}

	n, err := BackfillVoteBase(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应快照 2 行底数，实际 %d", n)
	}
	// 幂等：重复执行不重复快照、不改已有底数
	for i := 0; i < 3; i++ {
		if got, err := BackfillVoteBase(s.db); err != nil || got != 0 {
			t.Fatalf("第 %d 次重复执行应快照 0 行: %d %v", i, got, err)
		}
	}
	if w := s.Weights("dave")["女性"]; w != 37 {
		t.Fatalf("底数迁移不该改动权重，实际 %d", w)
	}
	// 新投票在底数之上叠加
	if err := s.CastVotes("dave", "1.1.1.1", map[string]int{"女性": 1, "大奶": -1}, "ua"); err != nil {
		t.Fatal(err)
	}
	w := s.Weights("dave")
	if w["女性"] != 38 || w["大奶"] != -5 {
		t.Fatalf("应在底数 37/-4 上各叠加 1 票，实际 %v", w)
	}
	// 恒等式
	v, err := s.InvariantViolations()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 0 {
		t.Fatalf("weight = 底数 + Σ票 被破坏: %+v", v)
	}
	// 底数不可变：投完之后 tag_weight_base 里仍是历史值
	var base int
	if err := s.db.QueryRow(`SELECT base FROM tag_weight_base WHERE username='dave' AND tag='女性'`).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if base != 37 {
		t.Fatalf("底数必须永远是历史原值 37，实际被改成 %d", base)
	}
}

// TestZeroWeightDeletesRollupKeepsVotes 需求 7：权重归零删 account_tags 行，
// 但票行与底数都留着，之后能凭账本重算回来。
func TestZeroWeightDeletesRollupKeepsVotes(t *testing.T) {
	s := openStore(t)
	if _, err := s.db.Exec(`INSERT INTO account_tags VALUES ('erin','露出',1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := BackfillVoteBase(s.db); err != nil {
		t.Fatal(err)
	}
	if err := s.CastVotes("erin", "5.5.5.5", map[string]int{"露出": -1}, "ua"); err != nil {
		t.Fatal(err)
	}
	if len(s.Weights("erin")) != 0 {
		t.Fatal("归零应删 account_tags 行")
	}
	// 票行必须在：删了它，这个 IP 以后就再也投不了这个标签，且权重无法重算
	var vn int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM tag_votes WHERE username='erin'`).Scan(&vn)
	if vn != 1 {
		t.Fatalf("归零后票行必须保留，实际 %d 行", vn)
	}
	// 凭账本重算：撤掉这张票就该回到底数 1
	if err := s.CastVotes("erin", "5.5.5.5", map[string]int{"露出": 0}, "ua"); err != nil {
		t.Fatal(err)
	}
	if w := s.Weights("erin")["露出"]; w != 1 {
		t.Fatalf("撤票后应由底数重建为 1，实际 %d", w)
	}
	v, err := s.InvariantViolations()
	if err != nil || len(v) != 0 {
		t.Fatalf("重建后应自洽: %+v %v", v, err)
	}
}

// TestConcurrentDistinctIPsNoLostVote 需求 6：8 个不同 IP 并发写同一行不丢更新。
// 这是 _txlock=immediate + busy_timeout 存在的理由：deferred 事务下两边可能各自
// 读到旧 Σ 再写回，票会静默丢失。
func TestConcurrentDistinctIPsNoLostVote(t *testing.T) {
	s := openStore(t)
	const n = 8
	var wg sync.WaitGroup
	vote := func(ip string, v int) {
		if err := s.CastVotes("frank", ip, map[string]int{"标签": v}, "ua"); err != nil {
			t.Errorf("CastVotes(%s,%d): %v", ip, v, err)
		}
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ip := fmt.Sprintf("10.0.0.%d", i+1)
			for k := 0; k < 5; k++ {
				vote(ip, VoteUp) // 重复同向：考验"值没变就不写"那条 WHERE
			}
			if i%2 == 0 {
				vote(ip, VoteDown) // 反向：制造 ±2 跳与删除/更新交错
				vote(ip, VoteUp)
				vote(ip, VoteUndo) // 撤票：DELETE 与别人的 INSERT 抢同一行的滚动值
				vote(ip, VoteUp)
			}
		}(i)
	}
	wg.Wait()

	// 终态：4 个奇数 IP 各留一张 +1，4 个偶数 IP 最后也回到 +1 → Σ=8
	if w := s.Weights("frank")["标签"]; w != n {
		t.Fatalf("并发后权重应为 %d（丢票了就是滚动值被互相覆盖），实际 %d", n, w)
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tag_votes WHERE username='frank'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != n {
		t.Fatalf("票账本应有 %d 行，实际 %d（少一行就是丢票）", n, rows)
	}
	if v, err := s.InvariantViolations(); err != nil || len(v) != 0 {
		t.Fatalf("并发后必须仍自洽: %+v %v", v, err)
	}
}

// TestRejectsEmptyPrincipal 拿不到 IP 时必须拒绝，而不是把全站塌成一个票桶。
func TestRejectsEmptyPrincipal(t *testing.T) {
	s := openStore(t)
	err := s.CastVotes("grace", "", map[string]int{"女性": 1}, "ua")
	if err == nil {
		t.Fatal("空 IP 必须报错")
	}
	if !errorsIs(err, ErrNoPrincipal) {
		t.Fatalf("应返回 ErrNoPrincipal，实际 %v", err)
	}
	if len(s.Weights("grace")) != 0 {
		t.Fatal("被拒的写入不该留下任何行")
	}
}

// TestMultiTagSingleTransaction 一个请求里多个标签共享事务：中途失败整体回滚。
func TestMultiTagSingleTransaction(t *testing.T) {
	s := openStore(t)
	targets := map[string]int{"a": 1, "b": 1, "c": 1}
	if err := s.CastVotes("henry", "3.3.3.3", targets, "ua"); err != nil {
		t.Fatal(err)
	}
	w := s.Weights("henry")
	if len(w) != 3 {
		t.Fatalf("三个标签应都写进去，实际 %v", w)
	}
	// 一次全部撤掉
	for k := range targets {
		targets[k] = 0
	}
	if err := s.CastVotes("henry", "3.3.3.3", targets, "ua"); err != nil {
		t.Fatal(err)
	}
	if len(s.Weights("henry")) != 0 {
		t.Fatalf("全部撤票后应清空，实际 %v", s.Weights("henry"))
	}
	var left int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM tag_votes WHERE username='henry'`).Scan(&left)
	if left != 0 {
		t.Fatalf("票行应全删，剩 %d", left)
	}
}

// TestInvariantDetectsTampering 校验 SQL 必须真的能抓到不一致（否则它是摆设）。
func TestInvariantDetectsTampering(t *testing.T) {
	s := openStore(t)
	_ = s.CastVotes("ivy", "4.4.4.4", map[string]int{"女装": 1}, "ua")
	if v, err := s.InvariantViolations(); err != nil || len(v) != 0 {
		t.Fatalf("干净状态应无违例: %+v %v", v, err)
	}
	// 有人绕过写路径直接改权重 → 必须被抓到
	if _, err := s.db.Exec(`UPDATE account_tags SET weight = 99 WHERE username='ivy'`); err != nil {
		t.Fatal(err)
	}
	v, err := s.InvariantViolations()
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 || v[0].Weight != 99 || v[0].Votes != 1 {
		t.Fatalf("篡改必须被抓到，实际 %+v", v)
	}
	// 下一张票自愈：滚动值是按账本重算的，不依赖它此前被改坏
	if err := s.CastVotes("ivy", "6.6.6.6", map[string]int{"女装": 1}, "ua"); err != nil {
		t.Fatal(err)
	}
	if w := s.Weights("ivy")["女装"]; w != 2 {
		t.Fatalf("重算应把被改坏的 99 修回 2，实际 %d", w)
	}
	if v, err := s.InvariantViolations(); err != nil || len(v) != 0 {
		t.Fatalf("自愈后应无违例: %+v %v", v, err)
	}
	var unsnap int
	if err := s.db.QueryRow(strings.TrimSpace(UnsnapshottedCountSQL)).Scan(&unsnap); err != nil {
		t.Fatal(err)
	}
	if unsnap != 0 {
		t.Fatalf("被票管过的行不该留在未快照集合里，实际 %d", unsnap)
	}
}

func errorsIs(err, target error) bool {
	return err != nil && strings.Contains(err.Error(), target.Error())
}
