// ============================================================================
// 【视频直连配额中间件】 (middleware_video_quota.go)
//
// 职责：
//   在 HandleVideoRedirectV2 之前拦截视频专属端点（/api/twitter/v2/video/*、
//   /api/v2/video/*、/api/flutter/video/*、/api/twitter/video/*），按「访问者 IP」
//   对每次点击计数，并决定是直连真实代理地址还是走 ouo.io 广告中转页：
//
//     - 计数 <= videoFreeDirectQuota（默认 50）：
//       直接 302 重定向到真实代理地址 https://twimg.moonchan.xyz/<path>?<query>，
//       绕过 ouo.io 广告页（c.Abort，不再进入 HandleVideoRedirectV2）。
//     - 计数 > videoFreeDirectQuota 且当前分钟能被 videoAdMinuteDivisor 整除：
//       放行 c.Next()，由 HandleVideoRedirectV2 按「与原代码一致」的逻辑
//       经 ouo.io/qs（或 ouo.io/api）302 跳转到广告中转页。
//     - 计数 > videoFreeDirectQuota 但当前分钟不能被 videoAdMinuteDivisor 整除：
//       仍直连真实代理地址（配额耗尽后的广告窗口仅在特定分钟生效）。
//
// 设计要点：
//   1. IP 口径复用 ipban.Principal(r)——与封禁、限流同一份退化顺序
//      （CF-Connecting-IP → XFF 右数第 N 个 → RemoteAddr），保证全栈一致。
//   2. 计数为进程内内存计数（sync.RWMutex 保护），按 IP 分桶；每次访问都 +1，
//      含最终走 ouo.io 的访问，符合「每一次访问计数」。
//   3. 真实地址的构造复用 ExtractAndBuildProxyVideoURL，与 HandleVideoRedirectV2
//      使用同一段 host 替换逻辑，确保直连与广告落地地址完全一致。
//   4. 分钟门控（videoAdMinuteChecker）让配额耗尽后的广告跳转只在特定分钟窗口
//      生效（默认每 3 分钟一次），其余时间仍直连，缓解硬性配额的用户体验断崖。
// ============================================================================

package api

import (
	"net/http"
	"sync"
	"time"

	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/gin-gonic/gin"
)

// videoFreeDirectQuota 每个访客 IP 的「直连真实地址」免费配额。
// 计数 <= 该值时直连 https://twimg.moonchan.xyz/...；计数 > 该值时，
// 仅当当前分钟能被 videoAdMinuteDivisor 整除时才走 ouo.io 广告中转。
// 默认 50：即每个 IP 前 50 次点击直连。
// 用 var 而非 const，便于测试按需覆盖为更小的值以快速走通两个分支。
var videoFreeDirectQuota = 50

// videoAdMinuteDivisor 配额耗尽后的分钟门控间隔：
// 仅当前分钟能被此值整除时才触发广告跳转，其余分钟仍直连。
// 默认 3：即 0,3,6,...,57 分钟跳广告，其余分钟仍直连（2/3 时间窗口直连）。
var videoAdMinuteDivisor = 3

// videoAdMinuteChecker 判断当前分钟是否应触发广告跳转。
// 默认为「分钟能被 videoAdMinuteDivisor 整除」；测试可覆盖为固定值。
var videoAdMinuteChecker = func() bool {
	return time.Now().Minute()%videoAdMinuteDivisor == 0
}

var (
	videoVisitMu    sync.RWMutex
	videoVisitCount = make(map[string]int)
)

// videoVisitInc 将指定 IP 的视频点击计数 +1 并返回自增后的计数值。
func videoVisitInc(ip string) int {
	videoVisitMu.Lock()
	defer videoVisitMu.Unlock()
	videoVisitCount[ip]++
	return videoVisitCount[ip]
}

// resetVideoVisitCounts 清空全部 IP 的视频点击计数（仅用于测试隔离）。
func resetVideoVisitCounts() {
	videoVisitMu.Lock()
	videoVisitCount = make(map[string]int)
	videoVisitMu.Unlock()
}

// VideoDirectQuotaMiddleware 是按访客 IP 计数的视频直连配额中间件。
//
// 挂载顺序：VideoDirectQuotaMiddleware(...) -> HandleVideoRedirectV2。
//   - 配额内（计数 <= videoFreeDirectQuota）：302 直连真实代理地址并 Abort，
//     不会进入 HandleVideoRedirectV2；
//   - 配额外（计数 > videoFreeDirectQuota）且当前分钟不触发广告
//     （videoAdMinuteChecker() == false）：仍 302 直连真实代理地址并 Abort；
//   - 配额外且当前分钟触发广告（videoAdMinuteChecker() == true）：c.Next()，
//     由 HandleVideoRedirectV2 经 ouo.io 跳转（与原代码一致）；
//   - 当请求无法构造出真实代理地址（缺 path/url 参数）时，即使仍在配额内也放行，
//     交由 HandleVideoRedirectV2 返回 400，保持原有错误语义。
func VideoDirectQuotaMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		ip := ipban.Principal(c.Request)
		count := videoVisitInc(ip)

		// 配额未满，或配额已满但当前分钟不触发广告：直连真实代理地址，绕过 ouo.io 广告中转页。
		if count <= videoFreeDirectQuota || !videoAdMinuteChecker() {
			proxyVideoURL, ok := ExtractAndBuildProxyVideoURL(c.Request, c.Param("filepath"))
			if ok && proxyVideoURL != "" {
				// 与 HandleVideoRedirectV2 一致地禁缓存，避免浏览器/CDN 粘住直连地址
				// 而在配额耗尽后仍绕过 ouo.io 计费。
				c.Header("Cache-Control", "no-cache, no-store, private, must-revalidate")
				c.Redirect(http.StatusFound, proxyVideoURL)
				c.Abort()
				return
			}
			// 无法构造真实地址：放行，由 handler 返回 400（保持原错误语义）。
		}

		// 配额已满且当前分钟触发广告：放行到 HandleVideoRedirectV2，经 ouo.io 跳转（与原代码一致）。
		c.Next()
	}
}
