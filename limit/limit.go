package limit

import (
	"net/netip"
	"sync"
	"time"
)

type ipEntry struct {
	count  uint32 // 访问次数
	hourID uint16 // 对应的小时 ID (time.Now().Unix() / 3600)
}

type FastLimiter struct {
	// key 用 netip.Addr 同时支持 IPv4 / IPv6。
	// 之前用 uint32 + ParseIP().To4()，纯 IPv6 返回 nil → ipInt==0 → 永久 429。
	ips map[netip.Addr]ipEntry
	mu  sync.Mutex
	max int
}

func NewFastLimiter(max int) *FastLimiter {
	l := &FastLimiter{
		ips: make(map[netip.Addr]ipEntry),
		max: max,
	}
	// 每小时彻底清理一次死数据，或者根据逻辑增量清理
	go l.vacuum()
	return l
}

func (l *FastLimiter) Allow(ipStr string) bool {
	// 1. 解析 IP；netip 同时支持 IPv4 / IPv6 / 未格式化的字符串。
	// 解析失败直接拒绝（之前的 ipInt==0 分支会把非法串与 0.0.0.0 混淆）。
	addr, err := netip.ParseAddr(ipStr)
	if err != nil {
		return false
	}

	// 2. 获取当前是第几个小时
	currHour := uint16((time.Now().Unix() / 3600) % 65535)

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, exists := l.ips[addr]

	// 3. 如果小时变了，重置计数器
	if !exists || entry.hourID != currHour {
		l.ips[addr] = ipEntry{count: 1, hourID: currHour}
		return true
	}

	// 4. 判断是否超限
	if int(entry.count) >= l.max {
		return false
	}

	// 5. 计数增加
	entry.count++
	l.ips[addr] = entry
	return true
}

func (l *FastLimiter) vacuum() {
	for {
		time.Sleep(1 * time.Hour)
		nowHour := uint16((time.Now().Unix() / 3600) % 65535)

		l.mu.Lock()
		for ip, entry := range l.ips {
			// 清理超过 1 小时未访问的 IP
			if entry.hourID != nowHour {
				delete(l.ips, ip)
			}
		}
		l.mu.Unlock()
	}
}