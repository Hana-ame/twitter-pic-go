// 本文件是标签云的进程内缓存。
//
// 为什么必须缓存：Cloud 的查询是
//
//	SELECT tag, COUNT(DISTINCT username) FROM account_tags a
//	 WHERE weight > 0 AND NOT EXISTS (SELECT 1 FROM users u
//	         WHERE u.username = a.username AND (u.status IS NULL OR u.status != 'SUCCESS'))
//	 GROUP BY tag ORDER BY 2 DESC, tag
//
// 即 account_tags(23k 行) 对 users(17k 行) 的**反连接**再 GROUP BY。线上实测
// 单次 ~0.8s。而它挂在两个高频入口上：首页 SSR 与 GET /api/tags/cloud。
// 不缓存就是每个请求烧 0.8s 的 CPU 与 IO。
//
// 刷新时机是**数据变化时失效**，不是定时轮询：
//   - 标签权重变化  → CastVotes 提交成功后失效（两个前端的写路径都汇到那里）
//   - 账号被封/解封 → 根包 commitUser 改完 users.status 后失效
//   - 历史底数回填  → BackfillVoteBase 完成后失效
// 这样才能满足"重启不负责刷新"：重启只是让缓存从空开始，之后由写入驱动。
//
// cloudTTL 只是**兜底**：运维直接用 sqlite3 改 users.status（[[proj-twitter-pic-ban-flow]]
// 里的手工封禁路径）绕过了进程，没有事件可挂钩；给个上限保证那种改动最终会体现。
// 它不是主机制——主机制始终是失效。
package tags

import (
	"database/sql"
	"sync"
	"time"
)

// cloudTTL 是兜底过期时间（覆盖进程外的 sqlite3 直改）。主机制是显式失效。
const cloudTTL = 10 * time.Minute

// 标签云不设数量上限：缓存全量榜单（而不是按请求的 limit 分别缓存），
// 取全量后按需切片是 O(1)。

// cloudKey 按 **库** 区分：同一个进程里测试会开多个 twitter.db，若只按
// (n, excludeBanned) 做键，不同库的结果会互相串。生产里 API 与 gallery
// 共用同一个 *sql.DB 指针，所以它们命中的是同一条缓存。
type cloudKey struct {
	db            *sql.DB
	excludeBanned bool
}

type cloudEntry struct {
	at  time.Time
	val []Count
}

var (
	cloudMu     sync.Mutex
	cloudData   = map[cloudKey]cloudEntry{}
	cloudFlight = map[cloudKey]chan struct{}{}
)

// cloneCounts 返回副本：缓存里的切片绝不能交到调用方手里被改。
func cloneCounts(v []Count) []Count {
	if v == nil {
		return nil
	}
	out := make([]Count, len(v))
	copy(out, v)
	return out
}

func cloudGet(key cloudKey) ([]Count, bool) {
	cloudMu.Lock()
	defer cloudMu.Unlock()
	e, ok := cloudData[key]
	if !ok {
		return nil, false
	}
	if time.Since(e.at) > cloudTTL {
		delete(cloudData, key)
		return nil, false
	}
	return cloneCounts(e.val), true
}

func cloudPut(key cloudKey, val []Count) {
	cloudMu.Lock()
	cloudData[key] = cloudEntry{at: time.Now(), val: cloneCounts(val)}
	cloudMu.Unlock()
}

// InvalidateCloud 清掉某个库的标签云缓存。写路径改完 account_tags 或 users.status
// 后必须调它，否则读到的还是旧榜单。
//
// db 传 nil 时清全部（测试/兜底用）。生产中两个前端共用同一个指针，所以
// 任一侧的写入都会让另一侧立刻看到新值。
func InvalidateCloud(db *sql.DB) {
	cloudMu.Lock()
	for k := range cloudData {
		if db == nil || k.db == db {
			delete(cloudData, k)
		}
	}
	cloudMu.Unlock()
}

// RefreshCloud 同步刷新指定库的标签云缓存，读 tag_counts 并填入 cloudData。
// posttag 写动作提交事务后立即调用，使读请求永远命中热缓存（0ms 响应）。
func (s *Store) RefreshCloud() {
	if s == nil || s.db == nil {
		return
	}
	key := cloudKey{db: s.db, excludeBanned: true}
	full, err := s.queryCloudAll(true)
	if err == nil && len(full) > 0 {
		cloudPut(key, full)
	} else {
		InvalidateCloud(s.db)
	}
}

// RefreshCloudByDB 同步刷新指定 sql.DB 的标签云缓存（供根包调用）
func RefreshCloudByDB(db *sql.DB) {
	if db == nil {
		return
	}
	s := &Store{db: db}
	s.RefreshCloud()
}
