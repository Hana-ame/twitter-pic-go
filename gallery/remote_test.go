package gallery

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func gzipBytes(t *testing.T, raw []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func remoteTimelineDoc(nick string, urls ...string) gzDocument {
	tl := make([]gzTimelineEntry, 0, len(urls))
	for i, u := range urls {
		tl = append(tl, gzTimelineEntry{
			URL:     u,
			Date:    "2024-01-01T00:00:00Z",
			TweetID: int64(1000 + i),
			Type:    "photo",
		})
	}
	return gzDocument{
		AccountInfo: gzAccountInfo{Name: "remoteuser", Nick: nick},
		Timeline:    tl,
	}
}

// 远程 JSON 源懒加载：本地无数据时按账号从 httptest 远端拉取并并入索引。
func TestRemoteLazyFetch(t *testing.T) {
	doc := remoteTimelineDoc("远程用户", "https://pbs.twimg.com/media/ABC.jpg")
	raw, _ := json.Marshal(doc)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/remoteuser.json.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(gzipBytes(t, raw))
	}))
	defer ts.Close()

	g := NewGallery(t.TempDir(), nil)
	g.SetRemoteSource(ts.URL)
	g.SetDBAccounts(map[string]AccountMeta{"remoteuser": {Nick: "远程用户"}})

	ok, err := g.FetchRemoteDoc("remoteuser")
	if !ok || err != nil {
		t.Fatalf("FetchRemoteDoc = (%v, %v), want (true, nil)", ok, err)
	}
	// 第二次命中内存缓存：不发起新请求、返回 false。
	if ok2, _ := g.FetchRemoteDoc("remoteuser"); ok2 {
		t.Errorf("重复 FetchRemoteDoc 应返回 false（已缓存）")
	}
	// scanNext 应把远端文档并入索引（仅存内存，不落盘由 remote.go 保证）。
	next, err := g.scanNext()
	if err != nil {
		t.Fatalf("scanNext: %v", err)
	}
	if len(next.byDir["remoteuser"]) != 1 {
		t.Fatalf("远端账号应并入 1 条媒体，实际 %d", len(next.byDir["remoteuser"]))
	}
	if !containsDir(next.dirs[""], "remoteuser") {
		t.Errorf("远端账号应出现在顶层账号列表")
	}
}

// 远程返回 404 时进入负缓存：后续同账号请求直接跳过（不再打远端）。
func TestRemoteLazyFetchNegativeCache(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer ts.Close()

	g := NewGallery(t.TempDir(), nil)
	g.SetRemoteSource(ts.URL)

	ok, err := g.FetchRemoteDoc("ghost")
	if ok || err == nil {
		t.Fatalf("FetchRemoteDoc(404) = (%v, %v), want (false, error)", ok, err)
	}
	// 负缓存期内再次调用应跳过（返回 false + error，不再发起请求）。
	if ok2, err2 := g.FetchRemoteDoc("ghost"); ok2 || err2 == nil {
		t.Errorf("负缓存期内应跳过，got (ok=%v, err=%v)", ok2, err2)
	}
}

// 非法用户名（含路径穿越）应被拒绝，绝不发起请求。
func TestRemoteLazyFetchInvalidUser(t *testing.T) {
	g := NewGallery(t.TempDir(), nil)
	g.SetRemoteSource("http://example.com")
	for _, bad := range []string{"..", "../evil", "a/b", ""} {
		if _, err := g.FetchRemoteDoc(bad); err == nil {
			t.Errorf("用户名 %q 应被拒绝", bad)
		}
	}
}

// 端到端：访问本地不存在的账号，/browse 触发远程懒加载后正常渲染该账号页。
func TestRemoteLazyFetchViaBrowse(t *testing.T) {
	doc := remoteTimelineDoc("远程用户", "https://pbs.twimg.com/media/ABC.jpg")
	raw, _ := json.Marshal(doc)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/remoteuser.json.gz" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(gzipBytes(t, raw))
	}))
	defer ts.Close()

	g := NewGallery(t.TempDir(), nil)
	g.SetRemoteSource(ts.URL)
	g.SetDBAccounts(map[string]AccountMeta{"remoteuser": {Nick: "远程用户"}})
	if err := g.Scan(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	s := &Server{gallery: g, links: map[string][]AccountLink{}}

	r := httptest.NewRequest(http.MethodGet, "/remoteuser", nil)
	w := httptest.NewRecorder()
	s.handleBrowse(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "remoteuser") {
		t.Errorf("远端账号浏览页应含 remoteuser")
	}
}

func containsDir(list []string, name string) bool {
	for _, d := range list {
		if d == name {
			return true
		}
	}
	return false
}
