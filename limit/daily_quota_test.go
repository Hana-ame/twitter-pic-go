package limit

import "testing"

// TestDailyRestartQuota_ClaimFirstN 钉住「前 N 个放行、第 N+1 个起拒绝」：
// 与用户 2026-10-21 钦定的「每 IP 每天前 10 个直跳真实链接」一致。
func TestDailyRestartQuota_ClaimFirstN(t *testing.T) {
	q := NewDailyRestartQuota(3)
	const ip = "198.51.100.10" // TEST-NET-2，永不与真实客户端撞桶
	for i := 1; i <= 3; i++ {
		if !q.Claim(ip) {
			t.Fatalf("request %d: expected within quota (true), got false", i)
		}
	}
	if q.Claim(ip) {
		t.Fatal("request 4: expected over quota (false), got true")
	}
	// 超额后继续自增（便于后续观察溢出量），判定仍为 false
	if q.Claim(ip) {
		t.Fatal("request 5: expected over quota (false), got true")
	}
}

// TestDailyRestartQuota_PerIPIndependent 钉住「每 IP 独立计数」。
func TestDailyRestartQuota_PerIPIndependent(t *testing.T) {
	q := NewDailyRestartQuota(2)
	a, b := "198.51.100.11", "198.51.100.12"
	if !q.Claim(a) || !q.Claim(a) {
		t.Fatal("ip a: first two should be within quota")
	}
	if q.Claim(a) {
		t.Fatal("ip a: third should be over quota")
	}
	if !q.Claim(b) {
		t.Fatal("ip b: first should be within quota (independent of a)")
	}
}

// TestDailyRestartQuota_InvalidIPConservative 钉住保守口径：
// 拿不到合法 IP（空串/非法串）一律 false，不放行直跳，保守走 ouo.io。
func TestDailyRestartQuota_InvalidIPConservative(t *testing.T) {
	q := NewDailyRestartQuota(5)
	for _, bad := range []string{"", "not-an-ip", "999.999.999.999"} {
		if q.Claim(bad) {
			t.Fatalf("Claim(%q): expected conservative false (no free pass for missing IP)", bad)
		}
	}
}

// TestDailyRestartQuota_MaxZeroDisables 钉住「额度为 0 即关闭直跳」。
func TestDailyRestartQuota_MaxZeroDisables(t *testing.T) {
	q := NewDailyRestartQuota(0)
	if q.Claim("198.51.100.13") {
		t.Fatal("max=0: Claim should always be false (direct bypass disabled)")
	}
	if q.Max() != 0 {
		t.Fatalf("max=0: Max() = %d, want 0", q.Max())
	}
}

// TestDailyRestartQuota_NilSafe 钉住 nil 接收者安全（不 panic）。
func TestDailyRestartQuota_NilSafe(t *testing.T) {
	var q *DailyRestartQuota
	if q.Claim("198.51.100.14") {
		t.Fatal("nil receiver: Claim should be false")
	}
	if q.Max() != 0 {
		t.Fatalf("nil receiver: Max() = %d, want 0", q.Max())
	}
}
