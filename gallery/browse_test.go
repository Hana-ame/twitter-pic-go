package gallery

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// /?q= 首页搜索：复用 /latest 的「最新」排序口径按用户名/昵称过滤账号。
func TestBrowseHomeSearch(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	r := httptest.NewRequest(http.MethodGet, "/?q=alice", nil)
	w := httptest.NewRecorder()
	s.handleBrowse(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `href="/alice"`) {
		t.Errorf("搜索结果应含 alice 链接")
	}
	if strings.Contains(body, `href="/bob"`) {
		t.Errorf("搜索结果不应含 bob 链接")
	}
	if !strings.Contains(body, "搜索") {
		t.Errorf("搜索结果应显示「搜索」标题（Search）")
	}
	// 首页摘要板块不应出现（已切到账号网格视图）
	if strings.Contains(body, `id="homeSections"`) {
		t.Errorf("首页搜索不应渲染摘要板块 homeSections")
	}
}

// /?q= 无匹配时返回空结果提示，而非报错。
func TestBrowseHomeSearchNoMatch(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	r := httptest.NewRequest(http.MethodGet, "/?q=zzz_no_exist", nil)
	w := httptest.NewRecorder()
	s.handleBrowse(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "没有匹配的账号") {
		t.Errorf("无匹配时应显示空结果提示")
	}
}

// 首页（无 q）应渲染 #homeSections 与 #accountsDropdown，
// 供「下拉」显示模式使用；下拉框含全部顶层账号（含仅 db 索引的 db_only）。
func TestHomeRendersDropdownSelect(t *testing.T) {
	g := newAPITestGallery()
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	s.handleBrowse(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="homeSections"`) {
		t.Errorf("首页应渲染 #homeSections 容器")
	}
	if !strings.Contains(body, `id="accountsDropdown"`) {
		t.Errorf("首页应渲染 #accountsDropdown 选择框")
	}
	for _, name := range []string{"alice", "bob", "db_only"} {
		if !strings.Contains(body, `value="/`+name+`"`) {
			t.Errorf("下拉框缺少账号选项 /%s", name)
		}
	}
}
