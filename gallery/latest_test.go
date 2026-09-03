package gallery

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHandleLatestOrdersByLatest 验证 GET /latest 按「账号最新媒体时间」降序排列
// （无媒体的 db-only 账号用 users.last_modify 兜底），与首页「最新」板块同口径
// （README「页面路由」一节承诺的行为）。
func TestHandleLatestOrdersByLatest(t *testing.T) {
	old := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2023, 5, 5, 0, 0, 0, 0, time.UTC)
	new := time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC)

	g := &Gallery{
		media: []*Media{
			{ID: "old/1.jpg", Path: "old_acct/1.jpg", Dir: "old_acct", Name: "1.jpg", Type: "photo", ModTime: old},
			{ID: "mid/2.jpg", Path: "mid_acct/2.jpg", Dir: "mid_acct", Name: "2.jpg", Type: "photo", ModTime: mid},
			{ID: "new/3.jpg", Path: "new_acct/3.jpg", Dir: "new_acct", Name: "3.jpg", Type: "photo", ModTime: new},
		},
		dbAccounts: map[string]AccountMeta{
			"db_only": {LastModify: time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)},
		},
		dirs: map[string][]string{"": {"db_only", "old_acct", "mid_acct", "new_acct"}},
	}
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	r := httptest.NewRequest(http.MethodGet, "/latest", nil)
	w := httptest.NewRecorder()
	s.handleLatest(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// 期望顺序（最新在前）：
	//   new_acct(2026) > db_only(2025, db 兜底) > mid_acct(2023) > old_acct(2020)
	idx := func(name string) int { return strings.Index(body, `href="/`+name+`"`) }
	want := []string{"new_acct", "db_only", "mid_acct", "old_acct"}
	prev := -1
	for _, n := range want {
		i := idx(n)
		if i < 0 {
			t.Fatalf("账号 %s 未出现在 /latest 页面", n)
		}
		if i < prev {
			t.Fatalf("排序错误：%s 应该排在更前面（body 中含 %s 的卡片出现在其后）", n, n)
		}
		prev = i
	}
}

// TestHandleLatestQuery 验证 /latest?q= 按用户名/昵称子串过滤（大小写不敏感）。
func TestHandleLatestQuery(t *testing.T) {
	g := &Gallery{
		media: []*Media{
			{ID: "a/1", Path: "alice/1.jpg", Dir: "alice", Name: "1.jpg", Type: "photo", ModTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{ID: "b/1", Path: "bob/1.jpg", Dir: "bob", Name: "1.jpg", Type: "photo", ModTime: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
		},
		dbAccounts: map[string]AccountMeta{},
		dirs:       map[string][]string{"": {"alice", "bob"}},
	}
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	for _, q := range []string{"AL", "ali", "bob"} {
		r := httptest.NewRequest(http.MethodGet, "/latest?q="+q, nil)
		w := httptest.NewRecorder()
		s.handleLatest(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("q=%s status = %d", q, w.Code)
		}
		body := w.Body.String()
		lower := strings.ToLower(q)
		if strings.Contains(lower, "al") && !strings.Contains(body, `href="/alice"`) {
			t.Fatalf("q=%s 应包含 alice 卡片", q)
		}
		if strings.Contains(lower, "bob") && !strings.Contains(body, `href="/bob"`) {
			t.Fatalf("q=%s 应包含 bob 卡片", q)
		}
		if q == "bob" && strings.Contains(body, `href="/alice"`) {
			t.Fatalf("q=bob 不应包含 alice 卡片")
		}
	}
}

// TestAccountsGridRendersDropdownSelect 保证账号列表中始终渲染
// <select id="accountsDropdown"> 作「下拉」显示模式的选择框（README 设置项）。
// 服务端只需输出 select；网格/下拉切换由前端 applyDisplay 控制。
func TestAccountsGridRendersDropdownSelect(t *testing.T) {
	g := &Gallery{
		media: []*Media{
			{ID: "a/1", Path: "alice/1.jpg", Dir: "alice", Name: "1.jpg", Type: "photo", ModTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{ID: "b/1", Path: "bob/1.jpg", Dir: "bob", Name: "1.jpg", Type: "photo", ModTime: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
		},
		dbAccounts: map[string]AccountMeta{},
		dirs:       map[string][]string{"": {"alice", "bob"}},
	}
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	for _, target := range []string{"/latest", "/hot"} {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		if target == "/latest" {
			s.handleLatest(w, r)
		} else {
			s.handleHot(w, r)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d", target, w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, `id="accountsDropdown"`) {
			t.Fatalf("%s: 缺少下拉选择框 <select id=accountsDropdown>", target)
		}
		if !strings.Contains(body, `<option value="/alice">`) && !strings.Contains(body, `<option value="alice">`) {
			t.Fatalf("%s: 下拉框缺少 alice 选项", target)
		}
	}
}

// TestHandleHotOrdersByVotes 验证 /hot 按账号 👍 总票数降序排列（README 承诺）。
func TestHandleHotOrdersByVotes(t *testing.T) {
	g := &Gallery{
		media: []*Media{
			{ID: "a/1", Path: "a/1.jpg", Dir: "a", Name: "1.jpg", Type: "photo", ModTime: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Counts: &mediaCounts{likes: 3}},
			{ID: "b/1", Path: "b/1.jpg", Dir: "b", Name: "1.jpg", Type: "photo", ModTime: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Counts: &mediaCounts{likes: 5}},
			{ID: "c/1", Path: "c/1.jpg", Dir: "c", Name: "1.jpg", Type: "photo", ModTime: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), Counts: &mediaCounts{likes: 1}},
		},
		dbAccounts: map[string]AccountMeta{},
		dirs:       map[string][]string{"": {"a", "b", "c"}},
	}
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	r := httptest.NewRequest(http.MethodGet, "/hot", nil)
	w := httptest.NewRecorder()
	s.handleHot(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	idx := func(name string) int { return strings.Index(body, `href="/`+name+`"`) }
	want := []string{"b", "a", "c"} // 5 > 3 > 1
	prev := -1
	for _, n := range want {
		i := idx(n)
		if i < 0 {
			t.Fatalf("账号 %s 未出现在 /hot 页面", n)
		}
		if i < prev {
			t.Fatalf("排序错误：%s 应排在更前面", n)
		}
		prev = i
	}
}
