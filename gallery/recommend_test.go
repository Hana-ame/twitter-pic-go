package gallery

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mkMedia 造一条带指定标签的测试媒体。
func mkMedia(dir, name string, tags ...string) *Media {
	return &Media{
		ID:      dir + "/" + name,
		Path:    dir + "/" + name,
		Dir:     dir,
		Name:    name,
		Type:    "photo",
		ModTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Tags:    tags,
		DirTags: []string{dir},
	}
}

// newTestGallery 把媒体列表装进一个 Gallery 并返回（聚类只读 g.media）。
func newTestGallery(items []*Media) *Gallery {
	return &Gallery{media: items, accountTags: map[string][]string{}}
}

func TestClustersEmpty(t *testing.T) {
	if out := newTestGallery(nil).Clusters(6, 12); len(out) != 0 {
		t.Fatalf("空库应返回空切片，得到 %d 个簇", len(out))
	}
}

func TestClustersSingle(t *testing.T) {
	out := newTestGallery([]*Media{mkMedia("a", "x.jpg")}).Clusters(0, 12)
	if len(out) != 1 {
		t.Fatalf("1 条媒体应得 1 个簇，得到 %d", len(out))
	}
	if out[0].Count != 1 || len(out[0].Items) != 1 {
		t.Fatalf("簇统计错误: count=%d items=%d", out[0].Count, len(out[0].Items))
	}
}

// TestClustersGroupsByTag 是聚类页的核心正确性断言：
// 标签集合相同的媒体必须落在同一个簇里。
func TestClustersGroupsByTag(t *testing.T) {
	items := []*Media{}
	// 两组完全不同的标签，各 6 条，中间塞 2 条无标签的。
	for i := 0; i < 6; i++ {
		items = append(items, mkMedia("anime_a", fmt4(i), "anime", "moe"))
	}
	for i := 0; i < 6; i++ {
		items = append(items, mkMedia("photo_b", fmt4(i), "photo", "landscape"))
	}
	items = append(items, mkMedia("misc_c", fmt4(60)), mkMedia("misc_c", fmt4(61)))

	out := newTestGallery(items).Clusters(3, 12)
	if len(out) != 3 {
		t.Fatalf("k=3 应得 3 个簇，得到 %d: %+v", len(out), out)
	}

	// 按「簇里装了哪个账号」定位簇：标签同频时簇名按字典序取，不能靠名字猜。
	clusterOf := func(dir string) *Cluster {
		for i := range out {
			if len(out[i].Items) > 0 && out[i].Items[0].Dir == dir {
				return &out[i]
			}
		}
		return nil
	}
	anime := clusterOf("anime_a")
	photo := clusterOf("photo_b")
	if anime == nil {
		t.Fatalf("没有 anime_a 所在的簇: %+v", out)
	}
	if photo == nil {
		t.Fatalf("没有 photo_b 所在的簇: %+v", out)
	}
	if anime.Count != 6 {
		t.Fatalf("anime 簇应有 6 条，实际 %d", anime.Count)
	}
	if photo.Count != 6 {
		t.Fatalf("photo 簇应有 6 条，实际 %d", photo.Count)
	}
	if len(anime.Tags) == 0 || (anime.Tags[0] != "anime" && anime.Tags[0] != "moe") {
		t.Fatalf("anime 簇标签应为 anime/moe，实际 %v", anime.Tags)
	}
	for _, m := range anime.Items {
		if m.Dir != "anime_a" {
			t.Fatalf("anime 簇混入了 %s", m.Dir)
		}
	}
	for _, m := range photo.Items {
		if m.Dir != "photo_b" {
			t.Fatalf("photo 簇混入了 %s", m.Dir)
		}
	}
}

// TestClustersDeterministic 同一批数据必须每次产出同样的聚类。
func TestClustersDeterministic(t *testing.T) {
	items := []*Media{}
	for _, tags := range [][]string{
		{"anime", "moe"}, {"anime", "cosplay"}, {"photo", "landscape"},
		{"photo", "portrait"}, {"anime", "moe"}, {"gaming", "cosplay"},
		{}, {"photo", "landscape"},
	} {
		items = append(items, mkMedia("acc", "m.jpg", tags...))
	}
	g := newTestGallery(items)
	a := g.Clusters(4, 12)
	b := g.Clusters(4, 12)
	if len(a) != len(b) {
		t.Fatalf("簇数不稳定: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Label != b[i].Label || a[i].Count != b[i].Count {
			t.Fatalf("第 %d 簇不稳定: %+v vs %+v", i, a[i], b[i])
		}
	}
}

// TestClustersKClamped k 超过媒体数时应收敛到媒体数。
// 用互相不同的标签，确保每条媒体都能独立成簇。
func TestClustersKClamped(t *testing.T) {
	items := []*Media{
		mkMedia("a", "1.jpg", "t1"),
		mkMedia("a", "2.jpg", "t2"),
		mkMedia("a", "3.jpg", "t3"),
	}
	out := newTestGallery(items).Clusters(999, 12)
	if len(out) != 3 {
		t.Fatalf("k=999/3 条媒体应得 3 个簇，得到 %d", len(out))
	}
	total := 0
	for _, c := range out {
		total += c.Count
	}
	if total != 3 {
		t.Fatalf("簇内总数应为 3，实际 %d", total)
	}
}

// TestClustersIdenticalVectors 所有媒体标签完全相同时没有区分度，
// 正确行为是退化成 1 个簇，而不是硬造出 3 个空簇。
func TestClustersIdenticalVectors(t *testing.T) {
	items := []*Media{}
	for i := 0; i < 3; i++ {
		items = append(items, mkMedia("a", fmt4(i), "same", "tags"))
	}
	out := newTestGallery(items).Clusters(3, 12)
	if len(out) != 1 {
		t.Fatalf("无区分度数据应退化为 1 个簇，得到 %d", len(out))
	}
	if out[0].Count != 3 {
		t.Fatalf("唯一簇应包含全部 3 条媒体，实际 %d", out[0].Count)
	}
}

// TestClustersItemLimit 每簇预览不应超过 itemLimit。
func TestClustersItemLimit(t *testing.T) {
	items := []*Media{}
	// 3 组标签各 13 条，让 k=3 真正分出 3 个簇。
	for i := 0; i < 13; i++ {
		items = append(items, mkMedia("big_a", fmt4(i), "g1"))
		items = append(items, mkMedia("big_b", fmt4(i), "g2"))
		items = append(items, mkMedia("big_c", fmt4(i), "g3"))
	}
	out := newTestGallery(items).Clusters(3, 5)
	if len(out) == 0 {
		t.Fatal("应返回至少 1 个簇")
	}
	for _, c := range out {
		if len(c.Items) > 5 {
			t.Fatalf("簇 %s 预览 %d 条，超过 itemLimit=5", c.ID, len(c.Items))
		}
		if c.Count < len(c.Items) {
			t.Fatalf("簇 %s Count(%d) 小于 Items(%d)", c.ID, c.Count, len(c.Items))
		}
	}
	total := 0
	for _, c := range out {
		total += c.Count
	}
	if total != len(items) {
		t.Fatalf("簇内总数应为 %d，实际 %d", len(items), total)
	}
}

// TestClustersUntagged 完全没有标签的媒体应聚到「未分类」簇。
func TestClustersUntagged(t *testing.T) {
	items := []*Media{}
	for i := 0; i < 4; i++ {
		items = append(items, mkMedia("untag", fmt4(i)))
	}
	out := newTestGallery(items).Clusters(1, 12)
	if len(out) != 1 || out[0].Label != "未分类" {
		t.Fatalf("无标签媒体应为「未分类」簇，得到 %+v", out)
	}
}

// TestClustersTopTagsSorted 簇标签应按出现次数降序，同频时按字典序。
func TestClustersTopTagsSorted(t *testing.T) {
	items := []*Media{
		mkMedia("a", "1.jpg", "common", "r2"),
		mkMedia("a", "2.jpg", "common", "r3"),
		mkMedia("a", "3.jpg", "common", "r4"),
		mkMedia("a", "4.jpg", "common", "r5"),
		mkMedia("a", "5.jpg", "rare"),
	}
	assignments := []int{0, 0, 0, 0, 0}

	label, tags := topTags(items, assignments, 0, -1)
	if label != "common" {
		t.Fatalf("簇标签应为 common，实际 %q", label)
	}
	want := []string{"common", "r2", "r3", "r4", "r5", "rare"}
	if len(tags) != len(want) {
		t.Fatalf("topTags(-1) 应返回全部 %d 个标签，实际 %d: %v", len(want), len(tags), tags)
	}
	for i, w := range want {
		if tags[i] != w {
			t.Fatalf("第 %d 位应为 %q，实际 %q（全序: %v）", i, w, tags[i], tags)
		}
	}

	// maxTags 截断：只保留前 3 个。
	if _, top3 := topTags(items, assignments, 0, 3); len(top3) != 3 || top3[2] != "r3" {
		t.Fatalf("topTags(3) 应为前 3 个，实际 %v", top3)
	}
}

// TestClustersHandlerEmpty 空库时聚类页必须正常渲染（不能 panic，也不能吐出空壳）。
func TestClustersHandlerEmpty(t *testing.T) {
	g := NewGallery("/nonexistent", map[string][]string{})
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	for _, target := range []string{"/clusters", "/clusters?k=12&item_limit=5"} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		s.handleClustersPage(w, r)

		if w.Code != http.StatusOK {
			t.Fatalf("%s: 状态码 %d，body: %s", target, w.Code, w.Body.String())
		}
		body := w.Body.String()
		if !strings.Contains(body, "聚类") {
			t.Fatalf("%s: 页面缺少「聚类」标题\n%s", target, body[:500])
		}
		if !strings.Contains(body, "暂无媒体") {
			t.Fatalf("%s: 空库应显示「暂无媒体」占位\n%s", target, body[:500])
		}
		if !strings.Contains(body, "</html>") {
			t.Fatalf("%s: 页面未正常闭合（模板执行可能出错）\n尾部: %s", target, body[len(body)-300:])
		}
	}
}

// TestClustersHandlerRenders 有数据时聚类页应渲染出簇卡片与簇数预设按钮。
func TestClustersHandlerRenders(t *testing.T) {
	items := []*Media{}
	for i := 0; i < 5; i++ {
		items = append(items, mkMedia("a", fmt4(i), "anime", "moe"))
		items = append(items, mkMedia("b", fmt4(i), "photo", "landscape"))
	}
	g := NewGallery("/nonexistent", map[string][]string{})
	g.media = items
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	r := httptest.NewRequest(http.MethodGet, "/clusters", nil)
	w := httptest.NewRecorder()
	s.handleClustersPage(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("状态码 %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"<h2>自动聚类</h2>", "class=\"cluster\"", "暂无媒体"} {
		if want == "暂无媒体" {
			if strings.Contains(body, want) {
				t.Fatalf("有数据时不应出现「暂无媒体」占位")
			}
		} else if !strings.Contains(body, want) {
			t.Fatalf("页面缺少 %q\n%s", want, body[:600])
		}
	}
	// 侧边栏应有聚类入口并处于 active 状态。
	if !strings.Contains(body, `href="/clusters" class="active">聚类`) {
		t.Fatalf("侧边栏缺少 active 的聚类入口")
	}
}

// fmt4 生成唯一的图片文件名。
func fmt4(i int) string {
	return "m_" + strconv.Itoa(i) + ".jpg"
}
