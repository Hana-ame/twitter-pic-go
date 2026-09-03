package gallery

import (
	"fmt"
	"testing"
)

// 突发额度：同一 IP 在额度内全部放行，超出后被限流。
func TestRateLimiterBurst(t *testing.T) {
	l := newRateLimiter(20, 60)
	const ip = "1.2.3.4"
	allowed := 0
	for i := 0; i < 60; i++ {
		if l.allow(ip) {
			allowed++
		}
	}
	if allowed != 60 {
		t.Fatalf("应放行前 60 个请求，实际 %d", allowed)
	}
	if l.allow(ip) {
		t.Errorf("超过突发额度（60）后应被限流")
	}
}

// 每 IP 限额独立：不同 IP 各自拥有完整突发额度。
func TestRateLimiterPerIP(t *testing.T) {
	l := newRateLimiter(20, 5)
	a, b := 0, 0
	for i := 0; i < 5; i++ {
		if l.allow("a") {
			a++
		}
		if l.allow("b") {
			b++
		}
	}
	if a != 5 || b != 5 {
		t.Fatalf("每 IP 应独立拥有 5 个突发，got a=%d b=%d", a, b)
	}
	if l.allow("a") || l.allow("b") {
		t.Errorf("两个 IP 都应被限流")
	}
}

// IP 表超过上限时自动淘汰最久未活跃项，避免内存无界增长。
func TestRateLimiterEviction(t *testing.T) {
	l := newRateLimiter(20, 60)
	const n = maxTrackedIPs + 5000
	for i := 0; i < n; i++ {
		l.allow(fmt.Sprintf("10.0.0.%d", i))
	}
	if len(l.perIP) > maxTrackedIPs {
		t.Errorf("IP 表不应超过 %d，实际 %d", maxTrackedIPs, len(l.perIP))
	}
}

// GALLERY_RATE_BURST 环境变量应覆盖默认突发额度。
func TestSecurityPolicyBurstEnv(t *testing.T) {
	t.Setenv("GALLERY_RATE_BURST", "123")
	t.Setenv("GALLERY_ADMIN_KEY", "")
	sp := loadSecurityPolicy()
	if sp.lim.burst != 123 {
		t.Errorf("GALLERY_RATE_BURST 应生效，got %d", sp.lim.burst)
	}
}

// 未设置时回落到默认突发额度。
func TestSecurityPolicyDefaultBurst(t *testing.T) {
	t.Setenv("GALLERY_RATE_BURST", "")
	t.Setenv("GALLERY_ADMIN_KEY", "")
	sp := loadSecurityPolicy()
	if sp.lim.burst != defaultBurst {
		t.Errorf("默认 burst 应为 %d，got %d", defaultBurst, sp.lim.burst)
	}
}

// GALLERY_RATE_LIMIT 环境变量应覆盖默认速率。
func TestSecurityPolicyRateEnv(t *testing.T) {
	t.Setenv("GALLERY_RATE_LIMIT", "5.5")
	t.Setenv("GALLERY_ADMIN_KEY", "")
	sp := loadSecurityPolicy()
	if sp.lim.rps != 5.5 {
		t.Errorf("GALLERY_RATE_LIMIT 应生效，got %v", sp.lim.rps)
	}
}

// 多 IP 并发限流吞吐（测锁竞争开销）。
func BenchmarkRateLimiter(b *testing.B) {
	l := newRateLimiter(100000, 200000)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			l.allow(fmt.Sprintf("ip-%d", i%1000))
			i++
		}
	})
}

// 单 IP 限流调用开销。
func BenchmarkRateLimiterSingleIP(b *testing.B) {
	l := newRateLimiter(float64(b.N), 1000000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		l.allow("same-ip")
	}
}
