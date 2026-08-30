package gallery

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMediaMarshalJSONViaValueCopy 验证 []Media 值拷贝序列化时
// 仍走 MarshalJSON 的原子快照（回归测试：items = append(items, *m) 这类写法）。
func TestMediaMarshalJSONViaValueCopy(t *testing.T) {
	m := &Media{ID: "abc", Path: "alice/a.jpg", Counts: &mediaCounts{}}
	m.SetCounts(5, 2)

	got := make([]Media, 1)
	got[0] = *m // 值拷贝，指针共享

	b, err := json.Marshal(map[string]any{"items": got})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var out struct {
		Items []struct {
			ID           string `json:"id"`
			LikeCount    int32  `json:"like_count"`
			DislikeCount int32  `json:"dislike_count"`
			Counts       any    `json:"counts"`
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Items) != 1 {
		t.Fatalf("items len = %d, want 1", len(out.Items))
	}
	it := out.Items[0]
	if it.ID != "abc" {
		t.Errorf("id = %q, want abc", it.ID)
	}
	if it.LikeCount != 5 {
		t.Errorf("like_count = %d, want 5", it.LikeCount)
	}
	if it.DislikeCount != 2 {
		t.Errorf("dislike_count = %d, want 2", it.DislikeCount)
	}
	if it.Counts != nil {
		t.Errorf("Counts 泄漏进 JSON: %v", it.Counts)
	}
}

// TestMediaNilCounts 零值 Media（未初始化 Counts）不应 panic。
func TestMediaNilCounts(t *testing.T) {
	m := Media{ID: "empty"}
	if m.LikeCount() != 0 || m.DislikeCount() != 0 {
		t.Errorf("zero counts = (%d,%d), want (0,0)", m.LikeCount(), m.DislikeCount())
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !contains(b, `"like_count":0`) || !contains(b, `"dislike_count":0`) {
		t.Errorf("json missing zero counts: %s", b)
	}
}

// TestMediaCountsAtomicIsolation 指针共享下，值拷贝能看到后续更新。
func TestMediaCountsAtomicIsolation(t *testing.T) {
	m := &Media{ID: "x", Counts: &mediaCounts{}}
	m.SetCounts(1, 1)
	cp := *m // 拷贝后原对象继续改
	m.SetCounts(9, 9)
	if cp.LikeCount() != 9 {
		t.Errorf("copy sees %d, want 9（指针共享）", cp.LikeCount())
	}
	if m.LikeCount() != 9 {
		t.Errorf("orig sees %d, want 9", m.LikeCount())
	}
}

func TestMediaViewIndexes(t *testing.T) {
	g := &Gallery{
		media: []*Media{
			{Path: "a/1.jpg", Dir: "a", Name: "1.jpg"},
			{Path: "a/2.jpg", Dir: "a", Name: "2.jpg"},
			{Path: "b/3.jpg", Dir: "b", Name: "3.jpg"},
		},
		byPath: map[string]*Media{},
		bySeq:  map[string]int{},
		byDir: map[string][]*Media{
			"a": {{Path: "a/1.jpg"}, {Path: "a/2.jpg"}},
			"b": {{Path: "b/3.jpg"}},
		},
		byDirIndex: map[string]map[string]int{
			"a": {"a/1.jpg": 0, "a/2.jpg": 1},
			"b": {"b/3.jpg": 0},
		},
	}
	for i, m := range g.media {
		g.byPath[m.Path] = m
		g.bySeq[m.Path] = i
	}

	cases := []struct {
		path     string
		index    int
		total    int
		prev     string
		next     string
		dirIndex int
		dirTotal int
		dirPrev  string
		dirNext  string
	}{
		{"a/1.jpg", 0, 3, "", "a/2.jpg", 0, 2, "", "a/2.jpg"},
		{"a/2.jpg", 1, 3, "a/1.jpg", "b/3.jpg", 1, 2, "a/1.jpg", ""},
		{"b/3.jpg", 2, 3, "a/2.jpg", "", 0, 1, "", ""},
	}
	for _, c := range cases {
		m := g.byPath[c.path]
		if m == nil {
			t.Fatalf("%s: not found in byPath", c.path)
		}
		idx := g.bySeq[c.path]
		if idx != c.index {
			t.Errorf("%s: index = %d, want %d", c.path, idx, c.index)
		}
		if len(g.media) != c.total {
			t.Errorf("%s: total = %d, want %d", c.path, len(g.media), c.total)
		}
		var prev, next string
		if idx > 0 {
			prev = g.media[idx-1].Path
		}
		if idx < len(g.media)-1 {
			next = g.media[idx+1].Path
		}
		if prev != c.prev || next != c.next {
			t.Errorf("%s: prev/next = (%q,%q), want (%q,%q)", c.path, prev, next, c.prev, c.next)
		}
		dirIdx := g.byDirIndex[m.Dir][c.path]
		dirMedia := g.byDir[m.Dir]
		if dirIdx != c.dirIndex {
			t.Errorf("%s: dirIndex = %d, want %d", c.path, dirIdx, c.dirIndex)
		}
		if len(dirMedia) != c.dirTotal {
			t.Errorf("%s: dirTotal = %d, want %d", c.path, len(dirMedia), c.dirTotal)
		}
		var dp, dn string
		if dirIdx > 0 {
			dp = dirMedia[dirIdx-1].Path
		}
		if dirIdx < len(dirMedia)-1 {
			dn = dirMedia[dirIdx+1].Path
		}
		if dp != c.dirPrev || dn != c.dirNext {
			t.Errorf("%s: dirPrev/dirNext = (%q,%q), want (%q,%q)", c.path, dp, dn, c.dirPrev, c.dirNext)
		}
	}
}

func contains(b []byte, sub string) bool {
	return strings.Contains(string(b), sub)
}
