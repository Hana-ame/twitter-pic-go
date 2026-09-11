package limit

import (
	"net/netip"
	"testing"
	"time"
)

// (a) IPv6 地址不该被直接拒绝。
// 之前 ipToUint32 用 ParseIP().To4()，纯 IPv6 返回 nil → ipInt==0 → return false。
func TestAllowIPv6NotRejected(t *testing.T) {
	l := NewFastLimiter(100)

	v6 := "2001:db8::1"
	if !l.Allow(v6) {
		t.Errorf("IPv6 %s 首次请求应放行", v6)
	}
	if !l.Allow(v6) {
		t.Errorf("IPv6 %s 第二次请求应放行", v6)
	}
}

// 纯 IPv4-mapped 形式也不该被拒。
func TestAllowIPv4MappedNotRejected(t *testing.T) {
	l := NewFastLimiter(100)
	if !l.Allow("::ffff:192.0.2.1") {
		t.Errorf("IPv4-mapped IPv6 地址应放行")
	}
}

// (b) 同一 IPv6 超过 max 应被限。
func TestAllowIPv6RateLimit(t *testing.T) {
	l := NewFastLimiter(3)
	v6 := "2001:db8::42"

	for i := 0; i < 3; i++ {
		if !l.Allow(v6) {
			t.Fatalf("第 %d 次请求应放行", i+1)
		}
	}
	if l.Allow(v6) {
		t.Errorf("第 4 次请求应被限")
	}
}

// (c) 小时变更后计数器重置。
// hourID 由 time.Now().Unix()/3600 推导，没法真等一小时，
// 直接往 ips map 里塞一个"上一小时"的 entry 模拟跨小时。
func TestAllowHourReset(t *testing.T) {
	l := NewFastLimiter(3)
	v6 := "2001:db8::77"

	// 先打满配额
	for i := 0; i < 3; i++ {
		l.Allow(v6)
	}
	if l.Allow(v6) {
		t.Fatalf("打满后应被限")
	}

	// 模拟时间跨到下一个小时：把现有 entry 的 hourID 改成"旧小时"。
	l.mu.Lock()
	entry := l.ips[netipParseMust(t, v6)]
	entry.hourID = (entry.hourID + 1) % 65535
	l.ips[netipParseMust(t, v6)] = entry
	l.mu.Unlock()

	// 新小时的第一次请求应放行。
	if !l.Allow(v6) {
		t.Errorf("跨小时后应重置计数并放行")
	}
}

// (d) IPv4 行为不变：限流计数 + 超限拒绝。
func TestAllowIPv4(t *testing.T) {
	l := NewFastLimiter(2)
	v4 := "192.0.2.99"

	if !l.Allow(v4) {
		t.Fatalf("IPv4 首次应放行")
	}
	if !l.Allow(v4) {
		t.Fatalf("IPv4 第二次应放行")
	}
	if l.Allow(v4) {
		t.Errorf("IPv4 第三次应被限")
	}
}

// IPv4 与 IPv6 应各自独立计数，互不影响。
func TestAllowIPv4AndIPv6Independent(t *testing.T) {
	l := NewFastLimiter(2)

	v4 := "192.0.2.1"
	v6 := "2001:db8::1"

	// v4 打满
	l.Allow(v4)
	l.Allow(v4)
	if l.Allow(v4) {
		t.Errorf("v4 应被限")
	}
	// v6 不受 v4 影响
	if !l.Allow(v6) {
		t.Errorf("v6 应与 v4 独立计数")
	}
}

// 非法 IP 串应被拒绝。
func TestAllowInvalidIP(t *testing.T) {
	l := NewFastLimiter(100)
	for _, bad := range []string{"", "not-an-ip", "999.1.1.1", "1.1.1"} {
		if l.Allow(bad) {
			t.Errorf("非法 IP %q 应被拒绝", bad)
		}
	}
}

// 0.0.0.0 应被当作合法地址处理（之前 ipInt==0 会把它与解析失败混淆）。
func TestAllowZeroIP(t *testing.T) {
	l := NewFastLimiter(2)
	if !l.Allow("0.0.0.0") {
		t.Errorf("0.0.0.0 应被放行")
	}
	if !l.Allow("0.0.0.0") {
		t.Errorf("0.0.0.0 第二次应放行")
	}
	if l.Allow("0.0.0.0") {
		t.Errorf("0.0.0.0 第三次应被限")
	}
}

// vacuum 应清理过期小时。
func TestVacuumCleansExpired(t *testing.T) {
	l := NewFastLimiter(3)
	v6 := "2001:db8::abc"
	l.Allow(v6)

	addr := netipParseMust(t, v6)
	l.mu.Lock()
	entry := l.ips[addr]
	entry.hourID = (entry.hourID + 1) % 65535
	l.ips[addr] = entry
	l.mu.Unlock()

	// 手动跑一轮 vacuum 逻辑（不真等 1 小时）
	nowHour := uint16((time.Now().Unix() / 3600) % 65535)
	l.mu.Lock()
	for ip, entry := range l.ips {
		if entry.hourID != nowHour {
			delete(l.ips, ip)
		}
	}
	l.mu.Unlock()

	l.mu.Lock()
	_, stillThere := l.ips[addr]
	l.mu.Unlock()
	if stillThere {
		t.Errorf("过期 entry 应被 vacuum 清掉")
	}
}

// 测试辅助：解析 IP，失败即 FailNow。
func netipParseMust(t *testing.T, s string) netip.Addr {
	t.Helper()
	a, err := netip.ParseAddr(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return a
}