package limit

import (
	"net"
	"sync"
	"time"
)

type ipEntry struct {
	count  uint32 // 访问次数
	hourID uint16 // 对应的小时 ID (time.Now().Unix() / 3600)
}

type FastLimiter struct {
	// 使用 uint32 作为 key (仅限 IPv4)，如果是 IPv6 建议使用 [16]byte
	ips map[uint32]ipEntry
	mu  sync.Mutex
	max int
}

func NewFastLimiter(max int) *FastLimiter {
	l := &FastLimiter{
		ips: make(map[uint32]ipEntry),
		max: max,
	}
	// 每小时彻底清理一次死数据，或者根据逻辑增量清理
	go l.vacuum()
	return l
}

func (l *FastLimiter) Allow(ipStr string) bool {
	// 1. 将字符串 IP 转为 uint32 (极度节约内存的关键)
	ipInt := ipToUint32(ipStr)
	if ipInt == 0 && ipStr != "0.0.0.0" {
		return false // 解析失败
	}

	// 2. 获取当前是第几个小时
	currHour := uint16((time.Now().Unix() / 3600) % 65535)

	l.mu.Lock()
	defer l.mu.Unlock()

	entry, exists := l.ips[ipInt]

	// 3. 如果小时变了，重置计数器
	if !exists || entry.hourID != currHour {
		l.ips[ipInt] = ipEntry{count: 1, hourID: currHour}
		return true
	}

	// 4. 判断是否超限
	if int(entry.count) >= l.max {
		return false
	}

	// 5. 计数增加
	entry.count++
	l.ips[ipInt] = entry
	return true
}

// 辅助函数：IPv4 转 uint32
func ipToUint32(ipStr string) uint32 {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
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
