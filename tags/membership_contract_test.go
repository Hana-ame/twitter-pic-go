package tags

import (
	"testing"
)

// F1 的可证伪判据：**任何一个 (account, tag) 被投票，都不许让该 tag 下其他
// 账号从反查/标签云里消失**。
//
// 反例构造（对齐 subagent 的 HTTP 复现）：
//   account_tags 里 3 个账号各持有 tag「女性」权重 1；user_tag_cnt 为空。
//   此时 Cloud 与 UsersForTag 都应返回 3 个。
//   然后**一个 IP 对其中一个账号投一票**（CastVotes 会写 user_tag_cnt）。
//   此刻 user_tag_cnt 只��� 1 行 —— 若读路径「优先 user_tag_cnt、只在返回 0 行
//   时才回退 account_tags」，那另外 2 个账号就被这一票**吃掉**了。
//
// 这条断言写成「结果里含有全部 3 个账号」的集合相等，而不是「len==1」之类的
// 快照数：坏实现与好实现的差别正在集合内容上。
func TestSingleVoteMustNotTruncateTagMembership(t *testing.T) {
	s := newTestStore(t)

	// 3 个账号各有「女性」权重 1（只写 account_tags，user_tag_cnt 仍为空）。
	for _, u := range []string{"alice", "bob", "carol"} {
		if _, err := s.db.Exec(
			`INSERT OR REPLACE INTO account_tags (username, tag, weight) VALUES (?, '女性', 1)`, u); err != nil {
			t.Fatal(err)
		}
	}

	before := s.UsersForTag("女性", nil, 50)
	if len(before) != 3 {
		t.Fatalf("前置条件就不成立：user_tag_cnt 为空时应回退 account_tags 返回 3 个，实际 %v", before)
	}

	// 一个 IP 给 alice 投一票 —— 这会往 user_tag_cnt 写第一行。
	if err := s.CastVotes("alice", "1.2.3.4", map[string]int{"女性": VoteUp}, "ua"); err != nil {
		t.Fatal(err)
	}

	// 关键断言：投一票之后，另外两个账号**不能**从反查结果里消失。
	after := s.UsersForTag("女性", nil, 50)
	got := map[string]bool{}
	for _, u := range after {
		got[u] = true
	}
	for _, want := range []string{"alice", "bob", "carol"} {
		if !got[want] {
			t.Fatalf("一个 IP 投一票就把 %q 从反查结果里挤掉了:\n before = %v\n after = %v", want, before, after)
		}
	}
}

// F2 的可证伪判据：标签云的计数必须等于**实际持有该 tag 的账号数**，
// 不能等于「投过票的 IP 数」。物化表 tag_counts 一旦非空就不再回退，
// 于是一次投票会把 6 报成 1。
func TestCloudCountMustEqualRealMembership(t *testing.T) {
	s := newTestStore(t)

	for _, u := range []string{"alice", "bob", "carol", "dave", "erin", "frank"} {
		if _, err := s.db.Exec(
			`INSERT OR REPLACE INTO account_tags (username, tag, weight) VALUES (?, '女性', 1)`, u); err != nil {
			t.Fatal(err)
		}
	}

	assertCloudCount := func(stage string) {
		cloud := s.Cloud(50, true)
		for _, e := range cloud {
			if e.Tag == "女性" {
				if e.Count != 6 {
					t.Fatalf("%s：标签云把「女性」报成 %d，真实持有账号数是 6", stage, e.Count)
				}
				return
			}
		}
		t.Fatalf("%s：标签云里根本没有「女性」这一项", stage)
	}

	assertCloudCount("投票前")

	// 一个 IP 投一票 —— 物化表 tag_counts 从此非空。
	if err := s.CastVotes("alice", "1.2.3.4", map[string]int{"女性": VoteUp}, "ua"); err != nil {
		t.Fatal(err)
	}

	assertCloudCount("一个 IP 投一票之后")
}