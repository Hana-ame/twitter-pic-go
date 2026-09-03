package gallery

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 构造一个固定的测试 gallery：3 个账号，4 张媒体；其中一个账号只有图片，另一个有视频。
func newAPITestGallery() *Gallery {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	g := &Gallery{
		media: []*Media{
			{ID: "alice/1", Path: "alice/1.jpg", Dir: "alice", Name: "1.jpg", Type: "photo", ModTime: old, Size: 1000, DirTags: []string{"alice"}},
			{ID: "alice/2", Path: "alice/2.mp4", Dir: "alice", Name: "2.mp4", Type: "video", ModTime: old.Add(time.Hour), Size: 5000, DirTags: []string{"alice"}},
			{ID: "bob/1", Path: "bob/1.jpg", Dir: "bob", Name: "1.jpg", Type: "photo", ModTime: old.Add(2 * time.Hour), Size: 2000, DirTags: []string{"bob"}},
			{ID: "bob/2", Path: "bob/2.jpg", Dir: "bob", Name: "2.jpg", Type: "photo", ModTime: old.Add(3 * time.Hour), Size: 1500, DirTags: []string{"bob"}},
		},
		dbAccounts: map[string]AccountMeta{
			"alice":   {Nick: "爱丽丝", LastModify: old},
			"bob":     {Nick: "鲍勃", LastModify: old},
			"db_only": {Nick: "", LastModify: old},
		},
		dirs: map[string][]string{"": {"alice", "bob", "db_only"}},
	}
	// 重建 byDir / byPath 索引（与 scanNext 一致）
	g.byDir = map[string][]*Media{}
	g.byPath = map[string]*Media{}
	g.bySeq = map[string]int{}
	for i, m := range g.media {
		g.byDir[m.Dir] = append(g.byDir[m.Dir], m)
		g.byPath[m.Path] = m
		g.bySeq[m.Path] = i
	}
	return g
}

func decodeAPIAccounts(t *testing.T, body []byte) (total, page, pageSize, totalPages int, accounts []map[string]any) {
	t.Helper()
	var resp struct {
		Total      int              `json:"total"`
		Page       int              `json:"page"`
		PageSize   int              `json:"page_size"`
		TotalPages int              `json:"total_pages"`
		Accounts   []map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode accounts: %v\n%s", err, body)
	}
	return resp.Total, resp.Page, resp.PageSize, resp.TotalPages, resp.Accounts
}

func decodeAPIMedia(t *testing.T, body []byte) (total, page, pageSize, totalPages int, items []map[string]any) {
	t.Helper()
	var resp struct {
		Total      int              `json:"total"`
		Page       int              `json:"page"`
		PageSize   int              `json:"page_size"`
		TotalPages int              `json:"total_pages"`
		Items      []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode media: %v\n%s", err, body)
	}
	return resp.Total, resp.Page, resp.PageSize, resp.TotalPages, resp.Items
}

// /api/health 返回 200 + 总账号/媒体数
func TestAPIHealth(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	w := httptest.NewRecorder()
	s.handleAPIHealth(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		StatusStatus string `json:"status"`
		Accounts     int    `json:"accounts"`
		Media        int    `json:"media"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.StatusStatus != "ok" {
		t.Errorf("status = %q, want ok", resp.StatusStatus)
	}
	if resp.Accounts != 3 || resp.Media != 4 {
		t.Errorf("counts accounts=%d media=%d, want 3/4", resp.Accounts, resp.Media)
	}
}

// /api/accounts 不带 q 时返回全部账号（含 db-only）
func TestAPIAccountsAll(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/accounts", nil)
	w := httptest.NewRecorder()
	s.handleAPIAccounts(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	total, _, _, _, accs := decodeAPIAccounts(t, w.Body.Bytes())
	if total != 3 || len(accs) != 3 {
		t.Fatalf("want 3 accounts, got total=%d count=%d", total, len(accs))
	}
	names := map[string]bool{}
	for _, a := range accs {
		names[a["name"].(string)] = true
	}
	for _, want := range []string{"alice", "bob", "db_only"} {
		if !names[want] {
			t.Errorf("账号 %s 未出现", want)
		}
	}
}

// /api/accounts?q= 按用户名 / 昵称子串过滤（大小写不敏感）
func TestAPIAccountsQuery(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	cases := []struct {
		q      string
		expect []string
	}{
		{"alice", []string{"alice"}},
		{"ALICE", []string{"alice"}},
		{"爱丽", []string{"alice"}}, // 按昵称子串
		{"bob", []string{"bob"}},
		{"xyz_no_match", nil},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/api/accounts?q="+c.q, nil)
		w := httptest.NewRecorder()
		s.handleAPIAccounts(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("q=%s status = %d", c.q, w.Code)
		}
		total, _, _, _, accs := decodeAPIAccounts(t, w.Body.Bytes())
		if total != len(c.expect) || len(accs) != len(c.expect) {
			t.Errorf("q=%s: total=%d count=%d, want %d", c.q, total, len(accs), len(c.expect))
		}
		got := map[string]bool{}
		for _, a := range accs {
			got[a["name"].(string)] = true
		}
		for _, e := range c.expect {
			if !got[e] {
				t.Errorf("q=%s: 缺少账号 %s", c.q, e)
			}
		}
	}
}

// /api/accounts?page_size=&page= 翻页
func TestAPIAccountsPagination(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/accounts?page_size=2&page=2", nil)
	w := httptest.NewRecorder()
	s.handleAPIAccounts(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	total, page, pageSize, totalPages, accs := decodeAPIAccounts(t, w.Body.Bytes())
	if total != 3 || page != 2 || pageSize != 2 || totalPages != 2 {
		t.Fatalf("got total=%d page=%d size=%d pages=%d, want 3/2/2/2", total, page, pageSize, totalPages)
	}
	if len(accs) != 1 { // 第二页只剩 db_only
		t.Fatalf("第 2 页应剩 1 条，实际 %d", len(accs))
	}
	if accs[0]["name"].(string) != "db_only" {
		t.Errorf("第 2 页首项应为 db_only，实际 %v", accs[0]["name"])
	}
}

// /api/media 不带 dir 返回全站媒体
func TestAPIMediaAll(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/media", nil)
	w := httptest.NewRecorder()
	s.handleAPIMedia(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	total, _, _, _, items := decodeAPIMedia(t, w.Body.Bytes())
	if total != 4 || len(items) != 4 {
		t.Fatalf("want 4 media, got total=%d count=%d", total, len(items))
	}
}

// /api/media?dir=alice 返回仅 alice 的媒体
func TestAPIMediaDir(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/media?dir=alice", nil)
	w := httptest.NewRecorder()
	s.handleAPIMedia(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	total, _, _, _, items := decodeAPIMedia(t, w.Body.Bytes())
	if total != 2 {
		t.Fatalf("alice 应有 2 条，实际 total=%d", total)
	}
	for _, it := range items {
		if it["dir"] != "alice" {
			t.Errorf("dir 过滤泄漏：%v", it["dir"])
		}
	}
}

// /api/media?type=video 只留视频
func TestAPIMediaTypeFilter(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/media?type=video", nil)
	w := httptest.NewRecorder()
	s.handleAPIMedia(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	total, _, _, _, items := decodeAPIMedia(t, w.Body.Bytes())
	if total != 1 {
		t.Fatalf("video 应只剩 1 条，实际 total=%d", total)
	}
	if items[0]["type"] != "video" {
		t.Errorf("type 过滤错误：%v", items[0]["type"])
	}
}

// /api/media?sort=name 按文件名升序
func TestAPIMediaSortName(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/media?dir=alice&sort=name", nil)
	w := httptest.NewRecorder()
	s.handleAPIMedia(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	_, _, _, _, items := decodeAPIMedia(t, w.Body.Bytes())
	if len(items) != 2 {
		t.Fatalf("want 2, got %d", len(items))
	}
	// alice: 1.jpg, 2.mp4 → 按 name 字典序 1.jpg < 2.mp4
	if items[0]["name"] != "1.jpg" || items[1]["name"] != "2.mp4" {
		t.Errorf("按 name 排序错：%v %v", items[0]["name"], items[1]["name"])
	}
}

// /api/media?sort=size 按大小降序
func TestAPIMediaSortSize(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/media?dir=alice&sort=size", nil)
	w := httptest.NewRecorder()
	s.handleAPIMedia(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	_, _, _, _, items := decodeAPIMedia(t, w.Body.Bytes())
	if len(items) != 2 {
		t.Fatalf("want 2, got %d", len(items))
	}
	// 1.jpg=1000, 2.mp4=5000 → 2.mp4 在前
	if items[0]["name"] != "2.mp4" || items[1]["name"] != "1.jpg" {
		t.Errorf("按 size 降序错：%v %v", items[0]["name"], items[1]["name"])
	}
}

// /api/media?page_size= 翻页
func TestAPIMediaPagination(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/media?page_size=2&page=2", nil)
	w := httptest.NewRecorder()
	s.handleAPIMedia(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	total, page, pageSize, totalPages, items := decodeAPIMedia(t, w.Body.Bytes())
	if total != 4 || page != 2 || pageSize != 2 || totalPages != 2 {
		t.Fatalf("got total=%d page=%d size=%d pages=%d, want 4/2/2/2", total, page, pageSize, totalPages)
	}
	if len(items) != 2 {
		t.Fatalf("第 2 页应剩 2 条，实际 %d", len(items))
	}
}

// /api/media/view?path= 返回单条详情 + 前后导航
func TestAPIMediaViewByPath(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/media/view?path=alice/1.jpg", nil)
	w := httptest.NewRecorder()
	s.handleAPIView(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Media struct {
			Path string `json:"path"`
			Dir  string `json:"dir"`
			Name string `json:"name"`
		} `json:"media"`
		Index    int    `json:"index"`
		Total    int    `json:"total"`
		Prev     string `json:"prev"`
		Next     string `json:"next"`
		DirTotal int    `json:"dirTotal"`
		DirPrev  string `json:"dirPrev"`
		DirNext  string `json:"dirNext"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	if resp.Media.Path != "alice/1.jpg" {
		t.Errorf("media.path = %q, want alice/1.jpg", resp.Media.Path)
	}
	if resp.Total != 4 {
		t.Errorf("total = %d, want 4", resp.Total)
	}
	if resp.DirTotal != 2 {
		t.Errorf("dirTotal = %d, want 2", resp.DirTotal)
	}
	if resp.Prev != "" {
		t.Errorf("第一条 prev 应为空，得到 %q", resp.Prev)
	}
	if resp.Next != "alice/2.mp4" {
		t.Errorf("next 应为 alice/2.mp4，得到 %q", resp.Next)
	}
	if resp.DirNext != "alice/2.mp4" {
		t.Errorf("dirNext 应为 alice/2.mp4，得到 %q", resp.DirNext)
	}
}

// /api/media/view?path=... 不存在返回 404
func TestAPIMediaViewNotFound(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/media/view?path=alice/missing.jpg", nil)
	w := httptest.NewRecorder()
	s.handleAPIView(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// /api/media/view 缺少 path 与 dir+name 返回 400
func TestAPIMediaViewMissingParams(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/media/view", nil)
	w := httptest.NewRecorder()
	s.handleAPIView(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

// /api/media/view?dir=...&name=... 也应能定位
func TestAPIMediaViewByDirAndName(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}
	r := httptest.NewRequest(http.MethodGet, "/api/media/view?dir=alice&name=1.jpg", nil)
	w := httptest.NewRecorder()
	s.handleAPIView(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"path":"alice/1.jpg"`) {
		t.Errorf("未找到 path=alice/1.jpg：%s", w.Body.String())
	}
}
