// Package ipban 是 IP 封禁的唯一实现：bans.txt 编译成 radix trie、链上任一命中即封、
// 热重载，以及**两层共用的 IP 口径**。
//
// 为什么单独成包：根 API（gin）与 gallery（标准库 http.ServeMux）都要做同一件事——
// 判断这个请求是不是该拒。原先它只长在根包里，gallery 要复用就得把整个 twitter 包
// 连 gin 一起拖进来。本包只依赖 net/http + net/netip + go-iptrie，两层各自薄薄包一层，
// 判定规则、bans.txt 解析、响应体都只有一份。
//
// 两条 IP 口径（务必分清，混用就会出现「补了封禁仍可绕过」或「限流可被伪造」）：
//
//   - Chain / Manager.Decide：**封禁判定**用。取 RemoteAddr + X-Forwarded-For 的
//     全部条目，任一命中即封。伪造左侧条目不会漏封（真实连接 IP 仍在链上）。
//   - Principal：**「这个请求是谁」用于限流分桶与 request_logs.ip** 用。
//     默认 = XFF 右往左第 N 个（env TRUSTED_PROXY_HOPS，默认 2 = CF + nginx 两跳），
//     再退化 RemoteAddr。CF-Connecting-IP 只有在显式开 CF_CONNECTING_IP=1 时才采信。
//
// ⚠️ 依赖部署前提（代码自证不了）：① 退化到数 XFF 跳数时，TRUSTED_PROXY_HOPS 必须
// 等于真实层数（nginx 追加=2、透传=1），配错的表现是限流可被换桶绕过；②
// CF_CONNECTING_IP 默认关。**源站"只允许 CF 回源"这条前提已在 2026-09-16 被实测
// 证伪**（iptables 零规则 / 无 ufw / nginx 0.0.0.0:443 / 直连源站 IP 得 200），
// 详见 EnvTrustCFHeader 上方。封禁用「链上任一」不受这两条影响。
// 启动时 LogEffectiveConfig 会打印生效口径，归属退化由 warnPrincipalAnomaly 告警。
//
// 关于"封禁覆盖到哪些入口"的现状（免得后人误以为 GET 也被封禁覆盖）：
//   - 根 API：只有 `POST /api/twitter/:username` 挂了 StrictIPBanMiddleware
//     （twitter_handlers.go），`PUT`/`DELETE /api/twitter/:username` 与
//     `POST /api/twitter/emojis` 都没有；所有 GET 都不做 IP 封禁。
//   - gallery：只有 `POST /api/tag`（及别名）在 handler 内手写调用 Decide。
//   - 即"封禁"目前只覆盖 2 个标签写入口，不是全站策略。这是**已知现状**，
//     覆盖面是否扩大另议（见 ipban.Middleware 的说明）。
package ipban

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/phemmer/go-iptrie"
)

// Manager 持有一份可原子热替换的封禁 trie。零值不可用，用 New / Shared 构造。
type Manager struct {
	trie atomic.Pointer[iptrie.Trie]
	size atomic.Int64

	path  string
	once  sync.Once // 保证自动重载协程只有一个
	stop  chan struct{}
	stopO sync.Once
}

// New 读一次 path（文件不存在/读失败 → 空表放行，与历史行为一致：
// 封禁清单缺失不该变成全站拒绝）。
func New(path string) *Manager {
	m := &Manager{path: path, stop: make(chan struct{})}
	m.trie.Store(iptrie.NewTrie())
	if err := m.ReloadFromFile(); err != nil {
		if os.IsNotExist(err) {
			log.Printf("ipban: %s 不存在，按空封禁表启动", path)
		} else {
			log.Printf("ipban: 初次加载 %s 失败（按空封禁表启动）: %v", path, err)
		}
	}
	return m
}

// shared 是进程级单例：根 API 与 gallery 拿到的必须是**同一份**，
// 否则两份内存副本各自 reload 会出现「一边已封一边没封」的窗口。
var (
	sharedOnce sync.Once
	shared     *Manager
)

// Shared 返回进程级单例（路径取 BANS_FILE，默认 "bans.txt"），并挂上唯一的
// 自动重载协程（周期取 BAN_RELOAD_MINUTES，默认 10 分钟）。
func Shared() *Manager {
	sharedOnce.Do(func() {
		shared = New(EnvBanFile())
		shared.StartAutoReload(EnvReloadEvery())
	})
	return shared
}

// EnvBanFile 是 bans.txt 路径（默认 "bans.txt"，与 systemd 的 WorkingDirectory 同源）。
func EnvBanFile() string {
	if v := strings.TrimSpace(os.Getenv("BANS_FILE")); v != "" {
		return v
	}
	return "bans.txt"
}

// EnvReloadEvery 是热重载周期，默认 10 分钟（沿用根 API 原有节奏）。
func EnvReloadEvery() time.Duration {
	if v := strings.TrimSpace(os.Getenv("BAN_RELOAD_MINUTES")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Minute
		}
	}
	return 10 * time.Minute
}

// TrustedHops 是可信反向代理的跳数：Principal 从 XFF 右往左数第 N 个才是真实客户端。
//
// ⚠️ 自 2026-09-16 起这是**主路径**（CF_CONNECTING_IP 默认关，见 EnvTrustCFHeader），
// 不再是"CF 头不可用时的兜底"。所以这个值的正确性直接决定限流分桶与流水归属。
//
// 本站默认 2（Cloudflare 在 nginx 之前、nginx 追加 XFF）。依据是线上 request_logs
// 里的 XFF 形态 "183.34.64.0, 104.22.109.48"——左为真实客户端、右为 CF 边缘 IP
// （104.22 / 104.23 都是 CF 段），说明 nginx 用的是 $proxy_add_x_forwarded_for（追加）。
// 部署核查会话已确认 nginx 配置原文：
//
//	proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
//
// 判据（追加还是透传，配错都会错一格）：
//   - nginx 设了 proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for（追加）
//     → CF 边缘 IP 落在链尾，真实客户端是右数第 2 个 → N=2
//   - nginx 原样透传（没有那一行）
//     → 链尾就是 CF 给的最后一项，真实客户端右数第 1 个 → N=1
//
// 注意：经 CF 的流量走这条能拿到真实客户端；但**直连源站**的请求链更短，会退化到
// 最左（客户端自报项）→ 仍可伪造。堵这条路要靠防火墙只放行 CF 网段，或按对端可信判定。
func TrustedHops() int {
	if v := strings.TrimSpace(os.Getenv("TRUSTED_PROXY_HOPS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 2
}

// EnvTrustCFHeader 控制是否采信 CF-Connecting-IP，**默认关**（设 1/true/on/yes 才开）。
//
// 默认关是 2026-09-16 实测之后的决定，不是保守估计。原先默认开，前提写作
// 「源站只允许 CF 回源」并标注"未验证"；部署核查会话把这条前提**实测证伪**了：
//
//   - `iptables -S` 只有三条全 ACCEPT 的默认策略、零规则；`nft list tables` 为空
//   - `ufw: command not found`；firewalld inactive
//   - nginx 监听 `0.0.0.0:443` 且无来源限制；`general-deny.conf` **从未被 include**
//   - `curl --resolve x.moonchan.xyz:443:97.64.30.221`（直连源站 IP）拿到 **HTTP 200**，
//     源站日志留下对应记录 → **任何人不经 Cloudflare 就能直连源站**
//   - 直连时送 `CF-Connecting-IP: 198.51.100.77` → HTTP 200 且该值被**原样采信**
//
// 在这种拓扑下采信这个头，等于把**限流配额与 request_logs.ip 归属**交给请求方自报：
// 每个假值一个全新的 25/h 桶（配额等于不存在），流水被投毒（"反查同一 IP 关联账号"
// 直接失效）。这与之前修掉的 XFF 投毒是同一类问题，只是换了个头。
//
// 开启条件（两者缺一不可）：① **防火墙层只放行 CF 网段**——注意封禁名单（bans.txt）
// 与访问控制是两件事，前者拦人、后者拦"绕过 CDN"这条路；② 再显式设
// CF_CONNECTING_IP=1。代码自证不了 ①，所以默认必须是"不信"。
//
// 关掉之后经 CF 的正常流量走 XFF 右数第 TrustedHops() 个（nginx 用
// `$proxy_add_x_forwarded_for` 把 CF 边缘 IP 拼在链尾 → 右数第 2 = 真实客户端），
// 取值与开启时相同；但**直连场景下 XFF 同样可伪造**（链长不足会退化到最左，
// 而最左是客户端自报项）。所以这一改只是"不主动采信一个更好伪造的头"，
// 要真正堵住直连伪造仍需防火墙白名单 CF 网段或按对端可信判定。
func EnvTrustCFHeader() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CF_CONNECTING_IP"))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// PrincipalSource 是 Principal 的取值来源，用于诊断输出。
type PrincipalSource string

const (
	FromCFHeader   PrincipalSource = "cf-connecting-ip"
	FromXFFHop     PrincipalSource = "xff-hop"
	FromXFFClamped PrincipalSource = "xff-clamped" // 链比可信跳数短，退化到最左（客户端可自报段）
	FromRemoteAddr PrincipalSource = "remote-addr"
)

// 来源计数：让「配错了」在日志里看得见，而不是默默假绿。
var (
	srcCF     atomic.Int64
	srcXFF    atomic.Int64
	srcClamp  atomic.Int64
	srcRemote atomic.Int64
	srcCFBad  atomic.Int64 // CF 头存在但不是合法 IP：不采信，继续退化
)

// Principal 返回「这个请求是谁」——用于限流分桶与 request_logs.ip。
//
// 退化顺序：XFF 从右往左第 TrustedHops() 个 → RemoteAddr host；
// 仅当显式设 CF_CONNECTING_IP=1 时才先看 CF-Connecting-IP。
//
// TODO(部署): 两个前提代码层面自证不了，见 TrustedHops / EnvTrustCFHeader 上方注释——
//  1. TRUSTED_PROXY_HOPS 是否等于真实层数（有 srcClamp 计数 + 重载时的告警兜底）；
//  2. 源站是否只能被 CF 回源（要靠防火墙白名单 CF 网段）。**本条已于 2026-09-16
//     实测证伪**（可直连源站），所以 CF_CONNECTING_IP 默认关。
func Principal(r *http.Request) string {
	ip, _ := PrincipalWithSource(r)
	return ip
}

// PrincipalWithSource 同 Principal，并返回取值来源（来源计数也在此处做）。
func PrincipalWithSource(r *http.Request) (string, PrincipalSource) {
	if r == nil {
		return "", FromRemoteAddr
	}
	if EnvTrustCFHeader() {
		if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
			if a, err := netip.ParseAddr(v); err == nil {
				srcCF.Add(1)
				return a.String(), FromCFHeader
			}
			srcCFBad.Add(1) // 值在但不是合法 IP：不采信，继续往下退化
		}
	}
	var xff []string
	for _, part := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
		if s := strings.TrimSpace(part); s != "" {
			xff = append(xff, s)
		}
	}
	if len(xff) > 0 {
		i := len(xff) - TrustedHops()
		if i < 0 {
			// 链比可信跳数短：退化取最左（宁可多算，不放行到不可控值）并计数。
			// 这个计数持续增长就说明拓扑与 TRUSTED_PROXY_HOPS 不符。
			srcClamp.Add(1)
			return xff[0], FromXFFClamped
		}
		srcXFF.Add(1)
		return xff[i], FromXFFHop
	}
	srcRemote.Add(1)
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host, FromRemoteAddr
	}
	return r.RemoteAddr, FromRemoteAddr
}

// PrincipalStats 是各来源的累计命中数（诊断用）。
type PrincipalStats struct {
	CFHeader, XFFHop, XFFClamped, RemoteAddr, CFHeaderBad int64
}

// PrincipalStatsNow 返回当前计数快照。
func PrincipalStatsNow() PrincipalStats {
	return PrincipalStats{
		CFHeader: srcCF.Load(), XFFHop: srcXFF.Load(), XFFClamped: srcClamp.Load(),
		RemoteAddr: srcRemote.Load(), CFHeaderBad: srcCFBad.Load(),
	}
}

// LogEffectiveConfig 在启动时打印实际生效的 IP 口径。只陈述、不校验——代码自证不了
// 拓扑，但至少排查时第一眼就能看到「现在按什么取 IP」「封禁表加载了几条」，
// 而不是猜。
func LogEffectiveConfig() {
	mode := "off（默认；经 CF 的流量走 XFF 右数第 N 个）"
	if EnvTrustCFHeader() {
		mode = "ON（⚠️ 采信请求方自报的 CF-Connecting-IP）"
	}
	log.Printf("ipban: IP 口径 -> CF-Connecting-IP %s | 退化=XFF 右数第 %d 个 | RemoteAddr 兜底 | bans=%s 已加载 %d 条，每 %v 重载",
		mode, TrustedHops(), EnvBanFile(), Shared().Count(), EnvReloadEvery())
	if EnvTrustCFHeader() {
		log.Printf("ipban: ⚠️ CF_CONNECTING_IP=1 已开启：这要求**防火墙层只放行 CF 网段**。" +
			"源站可被直连时（2026-09-16 实测：iptables 零规则、无 ufw、nginx 0.0.0.0:443、" +
			"直连源站 IP 得 200），任何客户端都能自报这个头 → 每个假值一个全新限流桶 + 流水 ip 被投毒。")
	}
}

// warnPrincipalAnomaly 在每次热重载时检查归属退化是否增长：
// xff-clamped 增长 = 有请求的 XFF 链撑不起配置的跳数，多半是拓扑与配置不符。
func warnPrincipalAnomaly() {
	st := PrincipalStatsNow()
	if st.XFFClamped == lastClamp {
		return
	}
	log.Printf("ipban: 注意 IP 归属退化 xff-clamped %d -> %d（链长 < TRUSTED_PROXY_HOPS=%d）："+
		"该入口可能不经 CF 或 nginx 未追加 XFF，此时归属落在客户端可自报的最左项；"+
		"来源分布 cf=%d xff-hop=%d remote=%d cf-bad=%d",
		lastClamp, st.XFFClamped, TrustedHops(), st.CFHeader, st.XFFHop, st.RemoteAddr, st.CFHeaderBad)
	lastClamp = st.XFFClamped
}

var lastClamp int64

// StartAutoReload 启动本 Manager 的热重载协程；对同一实例重复调用不会起第二个。
func (m *Manager) StartAutoReload(d time.Duration) {
	m.once.Do(func() {
		if d <= 0 {
			return
		}
		go func() {
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					if err := m.ReloadFromFile(); err != nil {
						log.Printf("ipban: 热重载 %s 失败（沿用上一份）: %v", m.path, err)
						continue
					}
					warnPrincipalAnomaly()
				case <-m.stop:
					return
				}
			}
		}()
	})
}

// StopAutoReload 停掉协程（进程内测试与优雅退出用）。
func (m *Manager) StopAutoReload() { m.stopO.Do(func() { close(m.stop) }) }

// Reload 把字符串清单编译成 trie（条目非法则整体报错，供程序化设置时使用）。
func (m *Manager) Reload(networks []string) error {
	t := iptrie.NewTrie()
	for _, s := range networks {
		p, err := parsePrefix(strings.TrimSpace(s))
		if err != nil {
			return err
		}
		t.Insert(p, struct{}{})
	}
	m.trie.Store(t)
	m.size.Store(int64(len(networks)))
	return nil
}

// ReloadFromFile 重读文件、编译新 trie、原子换指针（读侧永不阻塞）。
// 非法行跳过并告警，不因一行脏数据把整张表丢掉。
func (m *Manager) ReloadFromFile() error {
	f, err := os.Open(m.path)
	if err != nil {
		return err
	}
	defer f.Close()

	t := iptrie.NewTrie()
	var n int
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024) // 单行上限 1MB
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p, err := parsePrefix(line)
		if err != nil {
			log.Printf("ipban: 跳过封禁表里的非法条目: %s", line)
			continue
		}
		t.Insert(p, struct{}{})
		n++
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	m.trie.Store(t)
	m.size.Store(int64(n))
	return nil
}

// Count 是当前生效的封禁条目数（测试与自检用）。
func (m *Manager) Count() int {
	if m == nil {
		return 0
	}
	return int(m.size.Load())
}

// IsBanned 判断单个 IP 串。解析失败按「未封」处理——不能因为对端写了个
// 乱七八糟的头就把正常流量拒掉；封禁判定看的是 Chain，不是这一个值。
func (m *Manager) IsBanned(ipStr string) bool {
	a, err := netip.ParseAddr(strings.TrimSpace(ipStr))
	if err != nil {
		return false
	}
	return m.IsBannedAddr(a)
}

// IsBannedAddr 是 trie 的高速点查。
func (m *Manager) IsBannedAddr(a netip.Addr) bool {
	if m == nil {
		return false
	}
	t := m.trie.Load()
	if t == nil {
		return false
	}
	return t.Contains(a)
}

// Chain 返回这个请求涉及的所有 IP：RemoteAddr 的 host + XFF 全部条目（左→右）
// + CF-Connecting-IP（合法才收，值与 Principal 同样规到 netip 规范化形态）。
//
// 统一口径的意义：封禁必须看整条链。只看 XFF 首项会被
// `X-Forwarded-For: <好人IP>, <被封IP>` 直接绕过——原先 gallery 就是这个只看首项的写法。
// 注意 IPv6 的 `fe80::1%eth0` 这类 zone 会被去掉，`[::1]:8080` 会剥掉端口。
//
// 为什么 CF 头也要进链（2026-09-16 实测补）：存在**只设 CF 头、XFF 里没有那个 IP**
// 的上游（CF Tunnel 一类，或中间层把 XFF 改写掉），那时被封 IP 只出现在 CF 头里，
// 只看 XFF+RemoteAddr 就会漏封。
// 与 Principal 不同，这里**不受 CF_CONNECTING_IP 开关影响**：两条口径的哲学本就不同——
// 封禁是「链上任何一处报到被封 IP 就挡」（XFF 同样是不可信头也照样查），身份是
// 「只取我信任的那个值」。加它也不会给攻击者新增陷害手段：往 CF 头里塞别人的被封 IP
// 只会让**自己**这个请求被挡（封禁是逐请求判定，不会因此把那人加进名单），
// 也不会成为绕过手段——RemoteAddr 与 XFF 仍在链上，只会多封不会少封。
func Chain(r *http.Request) []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, 4)
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		out = append(out, strings.TrimSpace(host))
	} else if r.RemoteAddr != "" {
		out = append(out, strings.TrimSpace(r.RemoteAddr))
	}
	for _, part := range strings.Split(r.Header.Get("X-Forwarded-For"), ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	// CF 头放最后：XFF 里也能查到同一个值，命中时报告的字符串不变（保持既有响应形态）。
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		if a, err := netip.ParseAddr(v); err == nil {
			out = append(out, a.String())
		}
		// 非法值不进链（也不像 Principal 那样计数：这里不是身份判定，脏值当没看见即可）
	}
	return out
}

// Decide 是封禁判定的唯一入口：链上**任一** IP 命中即拒，返回命中的那个。
// 两层都必须走它，不要各自再写一遍循环。
func (m *Manager) Decide(r *http.Request) (string, bool) {
	if m == nil {
		return "", false
	}
	for _, s := range Chain(r) {
		a, err := netip.ParseAddr(strings.TrimSpace(s))
		if err != nil {
			continue // 非法条目忽略：它不该让请求被拒，也不该让后面的真 IP 免检
		}
		if m.IsBannedAddr(a) {
			return a.String(), true
		}
	}
	return "", false
}

// Denied 是被封禁时的响应体。两层共用同一个结构，字段与根 API 原有响应一致。
type Denied struct {
	Error  string `json:"error"`
	Reason string `json:"reason"`
	IP     string `json:"ip"`
}

// WriteDenied 写 403 + 统一响应体（gallery 侧用；gin 侧用 AbortWithStatusJSON 配
// 同一个 Denied 结构）。
func WriteDenied(w http.ResponseWriter, bannedIP string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(Denied{
		Error: "Access Denied", Reason: "Banned IP detected in chain", IP: bannedIP,
	})
}

// Middleware 是标准库版封禁中间件（gallery 用，不吃 gin）。被拒的请求**不会**
// 走到 next，因此也不会写库、不会写 request_logs。
func Middleware(m *Manager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ip, banned := m.Decide(r); banned {
				WriteDenied(w, ip)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// parsePrefix 同时吃 "1.1.1.1" 与 "1.1.0.0/24"。
func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		return netip.ParsePrefix(s)
	}
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	return netip.PrefixFrom(a, a.BitLen()), nil
}
