package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/Hana-ame/twitter-pic-go/limit"
)

// TestHandleVideoRedirectV2_DirectBypassFirstN 钉住 2026-10-21 钦定行为：
// 同一真实客户端 IP，每天（进程生命周期内，重启即重置）前 VIDEO_DIRECT_QUOTA 个
// 视频请求直跳 twimg.moonchan.xyz、不经 ouo.io；超出后回退 ouo.io 变现路径。
//
// 用 TEST-NET-2 保留段 IP，避免与其它测试或真实流量撞桶。
func TestHandleVideoRedirectV2_DirectBypassFirstN(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/twitter/v2/video/*filepath", HandleVideoRedirectV2)

	const ip = "198.51.100.42" // TEST-NET-2
	const videoPath = "/api/twitter/v2/video/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/test.mp4?tag=12"
	const wantDirect = "https://twimg.moonchan.xyz/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/test.mp4?tag=12"

	quota := limit.VideoDirectQuota().Max()
	if quota <= 0 {
		t.Skipf("VIDEO_DIRECT_QUOTA=%d, bypass disabled; skip", quota)
	}

	for i := 1; i <= quota; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, videoPath, nil)
		req.Header.Set("CF-Connecting-IP", ip)
		r.ServeHTTP(w, req)
		if w.Code != http.StatusFound {
			t.Fatalf("req %d: status=%d, want 302", i, w.Code)
		}
		if got := w.Header().Get("Location"); got != wantDirect {
			t.Fatalf("req %d (within quota): Location=%q, want direct %q", i, got, wantDirect)
		}
	}

	// 超出额度：应回退 ouo.io（默认 qs 模式，未设 OUO_MODE/GALLERY_OUO_PREFIX/OUO_PREFIX）
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, videoPath, nil)
	req.Header.Set("CF-Connecting-IP", ip)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("over-quota req: status=%d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	const wantOuoPrefix = "http://ouo.io/qs/6z4kqAz7?s="
	if !strings.HasPrefix(loc, wantOuoPrefix) {
		t.Fatalf("over-quota req: Location=%q, want prefix %q (ouo.io fallback)", loc, wantOuoPrefix)
	}
	if !strings.Contains(loc, "twimg.moonchan.xyz") {
		t.Fatalf("over-quota req: ouo target should wrap twimg.moonchan.xyz, got %q", loc)
	}
}

// TestHandleVideoRedirectV2_NoIPConservative 钉住保守口径：
// 取不到真实客户端 IP（无 CF 头 + 无 XFF + RemoteAddr 空）时不放行直跳，
// 保守走 ouo.io 变现——不因 IP 缺失而白嫖。
//
// 注意：httptest.NewRequest 默认把 RemoteAddr 设成 "192.0.2.1:1234"（RFC 5737），
// 会成为一个「合法可解析 IP」从而被当成首个请求放行。要真正模拟「拿不到 IP」，
// 必须显式清空 RemoteAddr。
func TestHandleVideoRedirectV2_NoIPConservative(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/twitter/v2/video/*filepath", HandleVideoRedirectV2)

	const videoPath = "/api/twitter/v2/video/ext_tw_video/1916894180884062208/pu/vid/avc1/720x1280/test.mp4?tag=12"
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, videoPath, nil)
	req.RemoteAddr = "" // 清空，模拟「完全取不到真实客户端 IP」
	// 不设任何 IP 头 → ipban.Principal 返回 "" → Claim 返回 false
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("no-ip req: status=%d, want 302", w.Code)
	}
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "http://ouo.io/") {
		t.Fatalf("no-ip req: Location=%q, want ouo.io (conservative, no free pass for missing IP)", loc)
	}
}
