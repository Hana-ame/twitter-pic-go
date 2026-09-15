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
//   - Principal：**「这个请求是谁」用于限流分桶与 request_logs.ip** 用。按可信代理
//     跳数（env TRUSTED_PROXY_HOPS，默认 1）从右往左数，避免客户端自带 XFF 就能换桶。
//
// ⚠️ 依赖部署前提：Principal 的正确性要求前置代理**追加** XFF（nginx 的
// $proxy_add_x_forwarded_for）。gin 侧从未调用 SetTrustedProxies（默认信任所有代理），
// 所以限流至今仍可被伪造 XFF 绕过；封禁用「链上任一」不受此影响。见 TODO。
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
// 默认 1（本站只有一层 nginx）。⚠️ 待验证：Cloudflare 在 nginx 之前时必须改成 2
// （或者改读 CF-Connecting-IP），配错了限流就等于没配。
func TrustedHops() int {
	if v := strings.TrimSpace(os.Getenv("TRUSTED_PROXY_HOPS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 1
}

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

// Chain 返回这个请求涉及的所有 IP：RemoteAddr 的 host + XFF 全部条目（左→右）。
//
// 统一口径的意义：封禁必须看整条链。只看 XFF 首项会被
// `X-Forwarded-For: <好人IP>, <被封IP>` 直接绕过——原先 gallery 就是这个只看首项的写法。
// 注意 IPv6 的 `fe80::1%eth0` 这类 zone 会被去掉，`[::1]:8080` 会剥掉端口。
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

// Principal 返回「这个请求是谁」——用于限流分桶与 request_logs.ip。
// 从 XFF 右往左数第 TrustedHops() 个；没有 XFF 或不够长则退化到 RemoteAddr。
//
// TODO(部署): 上线前必须确认 TRUSTED_PROXY_HOPS 与实际代理链层数一致，且
// nginx 是**追加**而非覆写 X-Forwarded-For。gin 侧从未调用 SetTrustedProxies
// （默认信任所有代理），所以 c.ClientIP() 与本函数在配置错误时都可能被伪造 XFF
// 换桶。封禁不受影响（看整条链），受影响的是 25/IP/h 配额与流水里的归属 IP。
func Principal(r *http.Request) string {
	if r == nil {
		return ""
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
			i = 0 // 链比可信跳数还短：说明有伪造，退化取最左（宁可多算也别放行）
		}
		return xff[i]
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
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
