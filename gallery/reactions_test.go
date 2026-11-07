package gallery

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hana-ame/twitter-pic-go/limit"
)

// TestReactionClampingAndAsyncFlush 验证单图赞踩接口：
// 1. DL/DD 钳制在 [-1, 1] 范围内（防恶意传 9999999 一键篡改赞数）；
// 2. apply 仅标 dirty 不在互斥锁内同步写盘，Flush() 才执行原子落盘；
// 3. 挂载 IP 封禁与限流保护。
func TestReactionClampingAndAsyncFlush(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "reactions.json")
	store := &reactionStore{path: tmpFile, m: map[string]*reactionCounts{}}

	cfg := newTestCfgWithBans(t, mustDB(t), "203.0.113.7\n")
	cfg.tagLimit = limit.NewFastLimiter(2)

	// 1. 恶意传参 dl=9999999, dd=8888888 -> 被钳制为 +1 / +1
	r1 := httptest.NewRequest("POST", "/api/react", strings.NewReader(`{"key":"img1","dl":9999999,"dd":8888888}`))
	r1.RemoteAddr = "198.51.100.1:1234"
	w1 := httptest.NewRecorder()
	handlePostReact(w1, r1, store, cfg)
	if w1.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w1.Code, w1.Body.String())
	}
	var resp1 map[string]any
	_ = json.Unmarshal(w1.Body.Bytes(), &resp1)
	if resp1["likes"] != float64(1) || resp1["dislikes"] != float64(1) {
		t.Fatalf("expected clamped likes=1, dislikes=1, got %v", resp1)
	}

	// Flush 前磁盘上不应同步生成文件（异步批量落盘验证）
	if _, err := os.Stat(tmpFile); !os.IsNotExist(err) {
		t.Fatalf("apply should mark dirty without synchronous disk write")
	}

	// 调用 Flush 后落盘成功
	store.Flush()
	if _, err := os.Stat(tmpFile); err != nil {
		t.Fatalf("Flush should persist reactions.json: %v", err)
	}

	// 2. 被封禁 IP 访问 /api/react -> 403
	rBan := httptest.NewRequest("POST", "/api/react", strings.NewReader(`{"key":"img1","dl":1}`))
	rBan.RemoteAddr = "203.0.113.7:1234"
	wBan := httptest.NewRecorder()
	handlePostReact(wBan, rBan, store, cfg)
	if wBan.Code != 403 {
		t.Fatalf("banned IP should get 403 on /api/react, got %d", wBan.Code)
	}

	// 3. 同一 IP 超出配额（limit=2）访问 /api/react -> 429
	r2 := httptest.NewRequest("POST", "/api/react", strings.NewReader(`{"key":"img1","dl":1}`))
	r2.RemoteAddr = "198.51.100.1:1234"
	w2 := httptest.NewRecorder()
	handlePostReact(w2, r2, store, cfg)
	if w2.Code != 200 {
		t.Fatalf("2nd request within quota should be 200, got %d", w2.Code)
	}

	r3 := httptest.NewRequest("POST", "/api/react", strings.NewReader(`{"key":"img1","dl":1}`))
	r3.RemoteAddr = "198.51.100.1:1234"
	w3 := httptest.NewRecorder()
	handlePostReact(w3, r3, store, cfg)
	if w3.Code != 429 {
		t.Fatalf("3rd request exceeding quota should get 429, got %d", w3.Code)
	}
}

// TestShouldUseDiskBundle 验证生产默认优先使用 embed.FS，仅在 GALLERY_DEV=1 或磁盘文件晚于二进制编译时间时覆盖。
func TestShouldUseDiskBundle(t *testing.T) {
	t.Setenv("GALLERY_DEV", "")
	t.Setenv("GALLERY_FRONTEND_DIR", "")

	exeTime := mustParseTime(t, "2026-11-05T12:00:00Z")
	olderDisk := mustParseTime(t, "2026-11-01T12:00:00Z")
	newerDisk := mustParseTime(t, "2026-11-06T12:00:00Z")

	// 默认（GALLERY_DEV 未设）：旧磁盘文件不覆盖新二进制
	if shouldUseDiskBundle("./gallery", olderDisk, exeTime) {
		t.Errorf("stale disk files older than executable must not override embed.FS")
	}
	// 默认：线上通过 --frontend 热更后磁盘文件晚于二进制，自动生效
	if !shouldUseDiskBundle("./gallery", newerDisk, exeTime) {
		t.Errorf("newer disk files after --frontend hot reload should override embed.FS")
	}
	// 显式 GALLERY_DEV=1：始终使用磁盘
	t.Setenv("GALLERY_DEV", "1")
	if !shouldUseDiskBundle("./gallery", olderDisk, exeTime) {
		t.Errorf("GALLERY_DEV=1 must force disk bundle")
	}
	// 显式 GALLERY_DEV=0：始终禁用磁盘
	t.Setenv("GALLERY_DEV", "0")
	if shouldUseDiskBundle("./gallery", newerDisk, exeTime) {
		t.Errorf("GALLERY_DEV=0 must disable disk bundle even if newer")
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return tm
}
