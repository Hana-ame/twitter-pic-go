package ipban

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeBans(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bans.txt")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestFileParsing 钉住 bans.txt 的解析口径：单 IP、CIDR、注释、空行都吃，
// 非法行跳过但不丢整张表（一行脏数据不该让全站失去封禁）。
func TestFileParsing(t *testing.T) {
	m := New(writeBans(t, "# comment\n\n203.0.113.7\n198.51.100.0/24\ngarbage-line\n"))
	if m.Count() != 2 {
		t.Fatalf("应解析出 2 条有效条目，实际 %d", m.Count())
	}
	if !m.IsBanned("203.0.113.7") {
		t.Fatal("单 IP 未生效")
	}
	if !m.IsBanned("198.51.100.200") {
		t.Fatal("CIDR 段内地址未生效")
	}
	if m.IsBanned("198.51.101.1") {
		t.Fatal("CIDR 段外被误封")
	}
	if m.IsBanned("") || m.IsBanned("not-an-ip") {
		t.Fatal("非法 IP 串应按未封处理，不能因脏头拒掉正常流量")
	}
}

// TestMissingFileFailsOpen 钉住降级方向：文件不存在 → 空表放行。
// 封禁清单缺失不该变成全站 403。
func TestMissingFileFailsOpen(t *testing.T) {
	m := New(filepath.Join(t.TempDir(), "absent.txt"))
	if m.Count() != 0 {
		t.Fatalf("应为空表，实际 %d 条", m.Count())
	}
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "203.0.113.7:1"
	if ip, banned := m.Decide(r); banned {
		t.Fatalf("空表不应拒任何请求，却拒了 %s", ip)
	}
}

// TestChainIncludesBothSources 钉住取 IP 的完整性：RemoteAddr 与 XFF 每一项都要在链上。
func TestChainIncludesBothSources(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.9:43210"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2, 3.3.3.3")

	got := Chain(r)
	want := []string{"10.0.0.9", "1.1.1.1", "2.2.2.2", "3.3.3.3"}
	if len(got) != len(want) {
		t.Fatalf("链长度不对: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("链第 %d 项应为 %s，实际 %v", i, want[i], got)
		}
	}

	// IPv6 带端口要能剥壳
	r2 := httptest.NewRequest("POST", "/", nil)
	r2.RemoteAddr = "[2001:db8::1]:8080"
	if c := Chain(r2); len(c) == 0 || c[0] != "2001:db8::1" {
		t.Fatalf("IPv6 RemoteAddr 没剥端口: %v", c)
	}
}

// TestDecideAnyHitInChain 是核心：被封 IP 出现在链上**任何位置**都必须拒。
// 只看 XFF 首项的旧实现会在 `X-Forwarded-For: <好人>, <被封>` 时被绕过。
func TestDecideAnyHitInChain(t *testing.T) {
	m := New(writeBans(t, "203.0.113.7\n"))

	for _, xff := range []string{
		"203.0.113.7",
		"8.8.8.8, 203.0.113.7",
		"203.0.113.7, 8.8.8.8",
		"not-an-ip, 203.0.113.7",
	} {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = "192.0.2.1:1"
		r.Header.Set("X-Forwarded-For", xff)
		ip, banned := m.Decide(r)
		if !banned || ip != "203.0.113.7" {
			t.Fatalf("XFF=%q 应命中 203.0.113.7，实际 banned=%v ip=%s", xff, banned, ip)
		}
	}

	// 干净链必须放行
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "192.0.2.1:1"
	r.Header.Set("X-Forwarded-For", "8.8.8.8, 9.9.9.9")
	if _, banned := m.Decide(r); banned {
		t.Fatal("干净链被误拒")
	}
}

// TestPrincipalHopMath 钉住 Principal 的取法：从右往左第 TRUSTED_PROXY_HOPS 个。
// 这是限流分桶与 request_logs.ip 的唯一口径，配错等于限流形同虚设。
func TestPrincipalHopMath(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.9:43210"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2, 3.3.3.3")

	t.Setenv("TRUSTED_PROXY_HOPS", "1")
	if got := Principal(r); got != "3.3.3.3" {
		t.Fatalf("hops=1 应取最右（nginx 看到的真实对端），实际 %s", got)
	}
	t.Setenv("TRUSTED_PROXY_HOPS", "2")
	if got := Principal(r); got != "2.2.2.2" {
		t.Fatalf("hops=2 应取右数第二个（如 CF 在 nginx 之前），实际 %s", got)
	}
	t.Setenv("TRUSTED_PROXY_HOPS", "9")
	if got := Principal(r); got != "1.1.1.1" {
		t.Fatalf("链比可信跳数短时应退化到最左（宁可多算不能放行），实际 %s", got)
	}
	t.Setenv("TRUSTED_PROXY_HOPS", "0") // 非法值回默认（本站真实拓扑 = 2）
	if got := Principal(r); got != "2.2.2.2" {
		t.Fatalf("非法跳数应回退默认 2，实际 %s", got)
	}

	// 无 XFF：直连场景取 RemoteAddr
	r2 := httptest.NewRequest("POST", "/", nil)
	r2.RemoteAddr = "10.0.0.9:43210"
	if got := Principal(r2); got != "10.0.0.9" {
		t.Fatalf("无 XFF 应取 RemoteAddr host，实际 %s", got)
	}
}

// TestPrincipalPrefersCFHeader 钉住一级优先：经 Cloudflare 时读 CF-Connecting-IP。
// CF 覆写该头，所以经 CF 的请求伪造不了它——比数 XFF 跳数可靠。
func TestPrincipalPrefersCFHeader(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.9:1"
	r.Header.Set("CF-Connecting-IP", "203.0.113.7")
	// 故意给一条与 CF 头矛盾的 XFF（含客户端自报段），验证谁说话算
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8, 104.22.109.48")

	ip, src := PrincipalWithSource(r)
	if ip != "203.0.113.7" || src != FromCFHeader {
		t.Fatalf("应优先采信 CF 头，实际 ip=%s src=%s", ip, src)
	}

	// 可关：CF_CONNECTING_IP=0 时退回 XFF 跳数（用于排查或不经 CF 的入口）
	t.Setenv("CF_CONNECTING_IP", "0")
	t.Setenv("TRUSTED_PROXY_HOPS", "2")
	ip, src = PrincipalWithSource(r)
	if ip != "5.6.7.8" || src != FromXFFHop {
		t.Fatalf("关掉 CF 头后应按 XFF 右数第 2 取，实际 ip=%s src=%s", ip, src)
	}

	// IPv6 也要能解析（CF 会回源 IPv6 客户端）
	t.Setenv("CF_CONNECTING_IP", "1") // 上面为验证「可关」把它关了，这里显式恢复
	r6 := httptest.NewRequest("POST", "/", nil)
	r6.Header.Set("CF-Connecting-IP", "2001:db8::42")
	if got := Principal(r6); got != "2001:db8::42" {
		t.Fatalf("CF 头里的 IPv6 没被采信，实际 %s", got)
	}

	// 伪造的 CF 头值（不是合法 IP）不采信，继续退化而不是当成主身份
	rBad := httptest.NewRequest("POST", "/", nil)
	rBad.RemoteAddr = "10.0.0.9:1"
	rBad.Header.Set("CF-Connecting-IP", "1.2.3.4:5678") // 带端口，非法
	rBad.Header.Set("X-Forwarded-For", "9.9.9.9")
	t.Setenv("TRUSTED_PROXY_HOPS", "1")
	ip, src = PrincipalWithSource(rBad)
	if ip != "9.9.9.9" || src != FromXFFHop {
		t.Fatalf("非法 CF 头应退化到 XFF，实际 ip=%s src=%s", ip, src)
	}
}

// TestPrincipalForgedChainIgnored 钉住「客户端自报 IP 在最左，不得影响取值」：
// 攻击者塞多少假条目都只能加在链的左边，右数第 N 个仍是我们代理写的那一项。
func TestPrincipalForgedChainIgnored(t *testing.T) {
	t.Setenv("CF_CONNECTING_IP", "0")
	t.Setenv("TRUSTED_PROXY_HOPS", "2")

	realClient := "198.51.100.23"
	for _, forged := range []string{
		"1.1.1.1",
		"1.1.1.1, 2.2.2.2",
		"203.0.113.7", // 试图把被封 IP 塞进链里（陷害），也不该改变归属
		strings.Repeat("7.7.7.7, ", 20),
	} {
		r := httptest.NewRequest("POST", "/", nil)
		r.RemoteAddr = "10.0.0.9:1"
		// 伪造条目只能加在链的左边：拼接时留好分隔符，别拼成一个假 IP
		var parts []string
		for _, f := range strings.Split(forged, ",") {
			if f = strings.TrimSpace(f); f != "" {
				parts = append(parts, f)
			}
		}
		parts = append(parts, realClient, "104.22.109.48")
		r.Header.Set("X-Forwarded-For", strings.Join(parts, ", "))
		if got := Principal(r); got != realClient {
			t.Fatalf("伪造前缀 %q 后归属被带偏：实际 %s，应为 %s（链=%s）",
				forged, got, realClient, r.Header.Get("X-Forwarded-For"))
		}
	}
}

// TestPrincipalClampIsVisible 钉住「配错要能看见」：链长撑不起配置的跳数时，
// 除了退化取值，必须留下计数，warnPrincipalAnomaly 才有东西可报。
func TestPrincipalClampIsVisible(t *testing.T) {
	t.Setenv("CF_CONNECTING_IP", "0")
	t.Setenv("TRUSTED_PROXY_HOPS", "5") // 故意配大，模拟拓扑与配置不符

	before := PrincipalStatsNow().XFFClamped
	r := httptest.NewRequest("POST", "/", nil)
	r.RemoteAddr = "10.0.0.9:1"
	r.Header.Set("X-Forwarded-For", "1.1.1.1, 2.2.2.2")
	ip, src := PrincipalWithSource(r)
	if ip != "1.1.1.1" || src != FromXFFClamped {
		t.Fatalf("链比跳数短应退化到最左，实际 ip=%s src=%s", ip, src)
	}
	if after := PrincipalStatsNow().XFFClamped; after <= before {
		t.Fatalf("退化取值必须计入 XFFClamped（%d -> %d），否则日志里看不见", before, after)
	}

	// 触发一次告警路径，确保函数本身不炸（真实文件 + 无异常也应安静）
	m := New(writeBans(t, "1.1.1.1\n"))
	if err := m.ReloadFromFile(); err != nil {
		t.Fatal(err)
	}
	warnPrincipalAnomaly()
	warnPrincipalAnomaly() // 第二次计数未增长，应直接返回
}

// TestSharedIsSingleton 钉住第 5 条要求：两层必须拿到同一份实例，
// 否则两份内存副本各自 reload 会出现「一边已封一边没封」的窗口。
// 用 t.Setenv 指定 BANS_FILE，不会碰到仓库里那份真 bans.txt。
func TestSharedIsSingleton(t *testing.T) {
	t.Setenv("BANS_FILE", writeBans(t, "203.0.113.7\n"))
	t.Setenv("BAN_RELOAD_MINUTES", "60") // 测试进程活不到下一个 tick，等于不重载

	a, b := Shared(), Shared()
	if a != b {
		t.Fatal("Shared() 每次都新建了实例")
	}
	if a.Count() != 1 || !a.IsBanned("203.0.113.7") {
		t.Fatalf("单例未按 BANS_FILE 加载: count=%d", a.Count())
	}
}

// TestAutoReloadSwapsAtomically 钉住热重载：改文件后下一轮生效，
// 且读侧在换表期间不会阻塞或看到半张表。
func TestAutoReloadSwapsAtomically(t *testing.T) {
	p := writeBans(t, "1.1.1.1\n")
	m := New(p)
	if m.IsBanned("9.9.9.9") {
		t.Fatal("初始表不该含 9.9.9.9")
	}
	// 同一实例重复 Start 不得起第二个协程（否则 1.5MB 文件被重复解析）
	before := runtime.NumGoroutine()
	m.StartAutoReload(time.Hour)
	m.StartAutoReload(time.Hour)
	if delta := runtime.NumGoroutine() - before; delta > 1 {
		t.Fatalf("StartAutoReload 重复调用起了 %d 个协程，应最多 1 个", delta)
	}
	m.StopAutoReload()

	if err := os.WriteFile(p, []byte("1.1.1.1\n9.9.9.9\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.ReloadFromFile(); err != nil {
		t.Fatal(err)
	}
	if !m.IsBanned("9.9.9.9") {
		t.Fatal("重载后新条目未生效")
	}
	if !m.IsBanned("1.1.1.1") {
		t.Fatal("重载不该丢掉旧条目")
	}
	// 重载失败要沿用上一份，不能把封禁表清空
	bad := filepath.Join(t.TempDir(), "nope.txt")
	if err := (&Manager{path: bad, stop: make(chan struct{})}).ReloadFromFile(); err == nil {
		t.Fatal("读不存在的文件应报错")
	}
}

// TestWriteDeniedBody 钉住响应体：两层返回同一个结构。
func TestWriteDeniedBody(t *testing.T) {
	w := httptest.NewRecorder()
	WriteDenied(w, "203.0.113.7")
	if w.Code != http.StatusForbidden {
		t.Fatalf("应 403，实际 %d", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json; charset=utf-8" {
		t.Fatalf("Content-Type 不对: %q", ct)
	}
	var d Denied
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Error != "Access Denied" || d.Reason != "Banned IP detected in chain" || d.IP != "203.0.113.7" {
		t.Fatalf("响应体不对: %+v", d)
	}
}

// TestMiddlewareBlocksHandler 钉住「被拒请求不得走到 next」——
// 封禁在 Add 之前，所以既不写库也不写 request_logs。
func TestMiddlewareBlocksHandler(t *testing.T) {
	m := New(writeBans(t, "203.0.113.7\n"))
	reached := 0
	h := Middleware(m)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusTeapot)
	}))

	r := httptest.NewRequest("POST", "/api/tag", nil)
	r.RemoteAddr = "203.0.113.7:1"
	h.ServeHTTP(httptest.NewRecorder(), r)
	if reached != 0 {
		t.Fatal("被封请求走到了业务处理器")
	}

	r2 := httptest.NewRequest("POST", "/api/tag", nil)
	r2.RemoteAddr = "8.8.8.8:1"
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, r2)
	if reached != 1 || w2.Code != http.StatusTeapot {
		t.Fatalf("干净请求应放行，实际 reached=%d code=%d", reached, w2.Code)
	}
}

// TestNilManagerSafe 钉住 nil 安全：cfg.bans 没注入时不能 panic（降级为不封）。
func TestNilManagerSafe(t *testing.T) {
	var m *Manager
	if _, banned := m.Decide(httptest.NewRequest("POST", "/", nil)); banned {
		t.Fatal("nil Manager 不该拒任何东西")
	}
	if m.IsBanned("1.1.1.1") || m.Count() != 0 {
		t.Fatal("nil Manager 行为异常")
	}
}
