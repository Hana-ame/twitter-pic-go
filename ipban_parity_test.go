package twitter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/gin-gonic/gin"
)

// newBanFile 写一份临时 bans.txt。刻意不碰仓库里那份 1.5MB 的真清单。
func newBanFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bans.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGinMiddlewareMatchesDecide 是回归栅栏：gin 侧中间件必须**只是** ipban.Decide
// 的薄包装。判定规则只允许有一份——若将来有人在包装里又写一遍 XFF 循环，
// 这条测试会因为「中间件放行了但 Decide 说该拒」而失败。
func TestGinMiddlewareMatchesDecide(t *testing.T) {
	gin.SetMode(gin.TestMode)
	m := ipban.New(newBanFile(t, "203.0.113.7\n198.51.100.0/24\n"))

	r := gin.New()
	r.POST("/x", StrictIPBanMiddleware(m), func(c *gin.Context) {
		c.JSON(http.StatusTeapot, gin.H{"reached": true})
	})

	cases := []struct {
		name    string
		remote  string
		xff     string
		want403 bool
	}{
		{"直连命中", "203.0.113.7:1", "", true},
		{"CIDR 段内", "198.51.100.6:1", "", true},
		{"藏在 XFF 链尾（旧口径可绕过）", "192.0.2.1:1", "8.8.8.8, 203.0.113.7", true},
		{"藏在 XFF 链首", "192.0.2.1:1", "203.0.113.7, 8.8.8.8", true},
		{"链里有非法条目", "192.0.2.1:1", "bogus, 203.0.113.7", true},
		{"干净链放行", "192.0.2.1:1", "8.8.8.8, 9.9.9.9", false},
		{"无 XFF 且直连干净", "192.0.2.1:1", "", false},
	}

	for _, tc := range cases {
		req := httptest.NewRequest("POST", "/x", nil)
		req.RemoteAddr = tc.remote
		if tc.xff != "" {
			req.Header.Set("X-Forwarded-For", tc.xff)
		}

		// 基准：共享包的判定
		_, wantBanned := m.Decide(req)
		if wantBanned != tc.want403 {
			t.Fatalf("%s: ipban.Decide 与用例期望不符 (banned=%v want403=%v)", tc.name, wantBanned, tc.want403)
		}

		// 中间件行为必须与基准一致
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if tc.want403 {
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s: 应 403，实际 %d %s", tc.name, w.Code, w.Body.String())
			}
			var d ipban.Denied
			if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
				t.Fatalf("%s: 403 响应体不是统一结构: %v %s", tc.name, err, w.Body.String())
			}
			if d.Error != "Access Denied" || d.Reason != "Banned IP detected in chain" || d.IP == "" {
				t.Fatalf("%s: 403 响应体不对: %+v", tc.name, d)
			}
		} else if w.Code != http.StatusTeapot {
			t.Fatalf("%s: 应放行到业务处理器，实际 %d", tc.name, w.Code)
		}
	}
}

// TestBanManagerSharedAcrossLayers 钉住第 5 条要求：根 API 的 AddToGroup 与
// gallery 用的是同一个进程级单例，因此热重载一次两层同时生效。
func TestBanManagerSharedAcrossLayers(t *testing.T) {
	p := newBanFile(t, "203.0.113.7\n")
	t.Setenv("BANS_FILE", p)
	t.Setenv("BAN_RELOAD_MINUTES", "60")

	a, b := ipban.Shared(), ipban.Shared()
	if a != b {
		t.Fatal("Shared() 没有返回同一份实例：两层会各持一份封禁表")
	}

	// 换文件 + 一次 reload，两层看到的必须是同一个新状态。
	if err := os.WriteFile(p, []byte("203.0.113.7\n198.51.100.0/24\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !a.IsBanned("203.0.113.7") {
		t.Fatal("单例初始状态不对")
	}
	if err := a.ReloadFromFile(); err != nil {
		t.Fatal(err)
	}
	if !b.IsBanned("198.51.100.9") {
		t.Fatal("reload 后另一层看不到新封禁：说明不是同一份实例")
	}
}

// TestWritePathIPUsesPrincipal 钉住流水里的 IP 与限流分桶同一个口径。
// 原先根 API 记的是整个 XFF 头串（"1.1.1.1, 2.2.2.2"），gallery 记的是首项，
// 反查同 IP 关联账号时两层根本对不上。
func TestWritePathIPUsesPrincipal(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTagsDB(t)
	if err := CreateTableV2(); err != nil {
		t.Fatal(err)
	}
	if _, err := DB.Exec(`INSERT INTO users (username, status) VALUES ('x','SUCCESS')`); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TRUSTED_PROXY_HOPS", "1")

	r := gin.New()
	r.POST("/api/twitter/:username", CreateMetaData)
	req := httptest.NewRequest("POST", "/api/twitter/x?do_not_renew=true",
		strings.NewReader(`{"女性":1}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "1.1.1.1, 203.0.113.9")
	req.Header.Set("User-Agent", "parity-test")
	r.ServeHTTP(httptest.NewRecorder(), req)

	var got string
	if err := DB.QueryRow(`SELECT ip FROM request_logs ORDER BY id DESC LIMIT 1`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "203.0.113.9" {
		t.Fatalf("request_logs.ip 应是 Principal(203.0.113.9)，实际 %q", got)
	}
}
