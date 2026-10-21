package limit

import (
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
)

// DailyRestartQuota 是「每个真实客户端 IP 每日前 N 个请求放行」的进程内计数器。
//
// 刻意不做任何按时间/按日期的重置——依赖 twitter-pic-go 每日重启
// （systemd 日重启 + ExecStartPre 拉取最新 release）清空进程内存，
// 从而天然实现「每天归零」，无需记录日期、无需持久化、无需后台清理协程。
// 这与用户 2026-10-21 钦定的「每日重启 → 每日 mem 都会重置」机制对齐：
// 计数只在进程生命周期内累计，重启即重置。若部署改为不重启，计数会一直累计，
// 属已知折衷（届时再加按日重置即可）。
//
// 计数 key 用 netip.Addr 的规范化形态：与 ipban.Principal 的取值口径一致，
// IPv4/IPv6 同一张表，避免 "1.2.3.4" 与 "::ffff:1.2.3.4" 被算成两个桶。
type DailyRestartQuota struct {
	mu    sync.Mutex
	count map[netip.Addr]int
	max   int
}

// NewDailyRestartQuota 构造一个每日每 IP 额度为 max 的计数器。
// max <= 0 表示关闭放行（Claim 永远返回 false，全部走既有路径）。
func NewDailyRestartQuota(max int) *DailyRestartQuota {
	if max < 0 {
		max = 0
	}
	return &DailyRestartQuota{count: make(map[netip.Addr]int), max: max}
}

// Claim 记一次该 IP 的请求，返回是否仍在每日免费额度内（应直跳真实链接）。
//
// 失败保守原则：拿不到合法 IP（空串、非法格式）一律返回 false，
// 保守走既有 ouo.io 变现路径，不因 IP 缺失而放行——既不漏计也不白嫖。
// 超过 max 后继续自增，便于后续观察溢出量（不影响判定结果）。
func (q *DailyRestartQuota) Claim(ipStr string) bool {
	if q == nil || q.max <= 0 {
		return false
	}
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.count[addr]++
	return q.count[addr] <= q.max
}

// Max 返回每日每 IP 免费额度（0 表示关闭放行）。
func (q *DailyRestartQuota) Max() int {
	if q == nil {
		return 0
	}
	return q.max
}

// videoDirectQuota 是进程级「视频直跳」额度单例：
// 每个真实 IP 每日前 N 个视频请求直跳 twimg.moonchan.xyz、不经 ouo.io。
//
// env VIDEO_DIRECT_QUOTA 覆写额度，默认 10（与用户钦定的「前 10 个」一致）；
// 设 0 关闭直跳（全部走 ouo.io 变现）。仅在进程初始化时读一次 env，
// 运行期改 env 需重启生效（与「每日重启」机制自洽）。
var videoDirectQuota = newVideoDirectQuotaFromEnv()

func newVideoDirectQuotaFromEnv() *DailyRestartQuota {
	const defaultQuota = 10
	max := defaultQuota
	if v := strings.TrimSpace(os.Getenv("VIDEO_DIRECT_QUOTA")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			max = n
		}
	}
	return NewDailyRestartQuota(max)
}

// VideoDirectQuota 返回进程级视频直跳额度单例。
// api（/api/twitter/v2/video/*filepath）与 gallery（/video/{filepath...}）两条
// 视频重定向入口共用同一份计数，确保「每个 IP 每天前 10 个」跨入口不超额。
func VideoDirectQuota() *DailyRestartQuota {
	return videoDirectQuota
}
