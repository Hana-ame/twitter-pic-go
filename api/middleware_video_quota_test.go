// ============================================================================
// 【视频直连配额中间件】专属测试 (middleware_video_quota_test.go)
//
// 钉住 VideoDirectQuotaMiddleware 的全部判定分支与边界，CI（.github/workflows/test.yml
// 的 go test ./...）每次 push master / PR 都会跑：
//   1. 配额内（计数 <= videoFreeDirectQuota）直连 twimg.moonchan.xyz，不经 ouo.io；
//   2. 配额耗尽（计数 > videoFreeDirectQuota）且分钟门控触发时放行到 HandleVideoRedirectV2 走 ouo.io；
//   3. 配额耗尽但分钟门控未触发时仍直连（缓解硬性配额的用户体验断崖）；
//   4. 不同 IP 独立计数；
//   5. 直连响应带 no-cache 头；
//   6. ?url= 参数式入口同样受配额约束；
//   7. quota=0 关闭直跳（全部走 ouo.io）；
//   8. 缺 path/url 参数时中间件放行，由 handler 返回 400（保持原错误语义）。
//
// 用最小隔离引擎（只挂 middleware + HandleVideoRedirectV2），不引入 ban/limit/DB，
// 避免与封禁表/限流器耦合导致 flaky。
// ============================================================================

package api

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
)

const (
	// videoQuotaRealURL 配额内直连的真实代理地址（host 替换为 twimg.moonchan.xyz）。
	videoQuotaRealURL = "https://twimg.moonchan.xyz/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/test.mp4?tag=12"
	// videoQuotaOuoURL 配额耗尽后 HandleVideoRedirectV2 生成的 ouo.io 广告中转地址。
	videoQuotaOuoURL = "http://ouo.io/qs/6z4kqAz7?s=https%3A%2F%2Ftwimg.moonchan.xyz%2Fext_tw_video%2F1916894180884062208%2Fpu%2Fvid%2Favc1%2F720x1280%2Ftest.mp4%3Ftag%3D12"
	// videoQuotaPath 路径式端点（/*filepath 形态）。
	videoQuotaPath = "/api/twitter/v2/video/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/test.mp4?tag=12"
	// videoQuotaOrig 原始 video.twimg.com 视频 URL（?url= 参数式用）。
	videoQuotaOrig = "https://video.twimg.com/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/test.mp4?tag=12"
)

// newQuotaTestEngine 构造只挂配额中间件 + 视频处理器的最小 gin 引擎。
// registerBare 控制是否注册裸 /api/twitter/v2/video（?url= 参数式）；
// 为 false 时只注册 /*filepath 路径式（gin 不允许同前缀下裸路由与 catch-all 在同一棵树上并存，
// 故按用例分别注册）。
func newQuotaTestEngine(registerBare bool) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	if registerBare {
		r.GET("/api/twitter/v2/video", VideoDirectQuotaMiddleware(), HandleVideoRedirectV2)
	} else {
		r.GET("/api/twitter/v2/video/*filepath", VideoDirectQuotaMiddleware(), HandleVideoRedirectV2)
	}
	return r
}

// withQuota 临时覆盖 videoFreeDirectQuota 并在测试结束后恢复，同时清空计数。
func withQuota(t *testing.T, n int) {
	t.Helper()
	resetVideoVisitCounts()
	prev := videoFreeDirectQuota
	videoFreeDirectQuota = n
	t.Cleanup(func() {
		videoFreeDirectQuota = prev
		resetVideoVisitCounts()
	})
}

// withMinuteChecker 临时覆盖 videoAdMinuteChecker 并在测试结束后恢复。
func withMinuteChecker(t *testing.T, result bool) {
	t.Helper()
	prev := videoAdMinuteChecker
	videoAdMinuteChecker = func() bool { return result }
	t.Cleanup(func() {
		videoAdMinuteChecker = prev
	})
}

// hitPath 以 /*filepath 路径式 + RemoteAddr 模拟一次访问，返回 (状态码, Location)。
func hitPath(r *gin.Engine, ip string) (int, string) {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", videoQuotaPath, nil)
	req.RemoteAddr = ip + ":11111"
	r.ServeHTTP(w, req)
	return w.Code, w.Header().Get("Location")
}

// hitParam 以 ?url= 参数式模拟一次访问。
func hitParam(r *gin.Engine, ip string) (int, string) {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/v2/video?url="+url.QueryEscape(videoQuotaOrig), nil)
	req.RemoteAddr = ip + ":11111"
	r.ServeHTTP(w, req)
	return w.Code, w.Header().Get("Location")
}

// TestVideoDirectQuotaMiddleware 钉住核心分支与按 IP 独立计数。
func TestVideoDirectQuotaMiddleware(t *testing.T) {
	r := newQuotaTestEngine(false)
	withQuota(t, 3)

	// 配额内（计数 1..3 <= 配额 3）：直接 302 到真实代理地址。
	for i := 1; i <= videoFreeDirectQuota; i++ {
		code, loc := hitPath(r, "203.0.113.1")
		if code != http.StatusFound {
			t.Fatalf("in-quota hit %d: expected 302, got %d", i, code)
		}
		if loc != videoQuotaRealURL {
			t.Fatalf("in-quota hit %d: expected real URL %q, got %q", i, videoQuotaRealURL, loc)
		}
	}

	// 第 4 次（计数 4 > 配额 3）：走 ouo.io 广告中转页（与原 HandleVideoRedirectV2 一致）。
	// 设分钟门控触发，确保配额耗尽后走广告路径。
	withMinuteChecker(t, true)
	code, loc := hitPath(r, "203.0.113.1")
	if code != http.StatusFound {
		t.Fatalf("post-quota: expected 302, got %d", code)
	}
	if loc != videoQuotaOuoURL {
		t.Fatalf("post-quota: expected ouo URL %q, got %q", videoQuotaOuoURL, loc)
	}
	// 后续仍持续走 ouo.io。
	if _, loc := hitPath(r, "203.0.113.1"); loc != videoQuotaOuoURL {
		t.Fatalf("post-quota 2nd: expected ouo URL %q, got %q", videoQuotaOuoURL, loc)
	}

	// 不同 IP 独立计数：仍应直连真实地址。
	code, loc = hitPath(r, "203.0.113.2")
	if code != http.StatusFound || loc != videoQuotaRealURL {
		t.Fatalf("new IP: expected real URL %q, got %d %q", videoQuotaRealURL, code, loc)
	}

	// 配额内直连也带 no-cache 头，避免配额耗尽后仍绕过 ouo.io 计费。
	{
		w := httptest.NewRecorder()
		req, _ := http.NewRequest("GET", videoQuotaPath, nil)
		req.RemoteAddr = "203.0.113.3:11111"
		r.ServeHTTP(w, req)
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache, no-store, private, must-revalidate" {
			t.Fatalf("expected no-cache header on direct redirect, got %q", cc)
		}
	}
}

// TestVideoDirectQuotaParamForm 钉住 ?url= 参数式入口同样受配额约束。
func TestVideoDirectQuotaParamForm(t *testing.T) {
	r := newQuotaTestEngine(true)
	withQuota(t, 1)

	// 计数 1 <= 1：直连真实地址。
	if code, loc := hitParam(r, "203.0.113.10"); code != http.StatusFound || loc != videoQuotaRealURL {
		t.Fatalf("in-quota param form: expected 302 %q, got %d %q", videoQuotaRealURL, code, loc)
	}
	// 计数 2 > 1：走 ouo.io（分钟门控触发）。
	withMinuteChecker(t, true)
	if code, loc := hitParam(r, "203.0.113.10"); code != http.StatusFound || loc != videoQuotaOuoURL {
		t.Fatalf("post-quota param form: expected 302 %q, got %d %q", videoQuotaOuoURL, code, loc)
	}
}

// TestVideoDirectQuotaDisabled 钉住 quota=0 关闭直跳：所有请求都走 ouo.io。
func TestVideoDirectQuotaDisabled(t *testing.T) {
	r := newQuotaTestEngine(false)
	withQuota(t, 0)

	// quota=0 时所有请求都走 ouo.io（分钟门控触发）。
	withMinuteChecker(t, true)
	for i := 1; i <= 3; i++ {
		code, loc := hitPath(r, "203.0.113.20")
		if code != http.StatusFound || loc != videoQuotaOuoURL {
			t.Fatalf("quota=0 hit %d: expected 302 %q, got %d %q", i, videoQuotaOuoURL, code, loc)
		}
	}
}

// TestVideoDirectQuotaMissingPathFallsThrough 钉住缺 path/url 参数时中间件放行：
// ExtractAndBuildProxyVideoURL 返回 ok=false -> 不直连 -> c.Next() ->
// HandleVideoRedirectV2 返回 400（保持原错误语义）。
func TestVideoDirectQuotaMissingPathFallsThrough(t *testing.T) {
	r := newQuotaTestEngine(true)
	withQuota(t, 10)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/twitter/v2/video", nil) // 裸端点，无 ?url=
	req.RemoteAddr = "203.0.113.30:11111"
	r.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("missing path: expected 400 from handler, got %d (body=%q)", w.Code, w.Body.String())
	}
}

// TestVideoDirectQuotaMinuteGating 钉住配额耗尽后的分钟门控逻辑：
// 分钟不触发广告时仍直连，分钟触发广告时走 ouo.io。
func TestVideoDirectQuotaMinuteGating(t *testing.T) {
	r := newQuotaTestEngine(false)
	withQuota(t, 1)

	// 配额内（计数 1 <= 1）：直连（不受分钟门控影响）。
	if code, loc := hitPath(r, "203.0.113.40"); code != http.StatusFound || loc != videoQuotaRealURL {
		t.Fatalf("in-quota: expected 302 %q, got %d %q", videoQuotaRealURL, code, loc)
	}

	// 配额外 + 分钟不触发广告：仍直连。
	withMinuteChecker(t, false)
	if code, loc := hitPath(r, "203.0.113.40"); code != http.StatusFound || loc != videoQuotaRealURL {
		t.Fatalf("post-quota minute-not-gated: expected 302 %q, got %d %q", videoQuotaRealURL, code, loc)
	}

	// 配额外 + 分钟触发广告：走 ouo.io。
	withMinuteChecker(t, true)
	if code, loc := hitPath(r, "203.0.113.40"); code != http.StatusFound || loc != videoQuotaOuoURL {
		t.Fatalf("post-quota minute-gated: expected 302 %q, got %d %q", videoQuotaOuoURL, code, loc)
	}
}
