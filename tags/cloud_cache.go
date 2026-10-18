// 本文件是标签云的进程内缓存与跨层数据变更通知钩子。
//
// 刷新时机是数据变化时失效，不是定时轮询：
//   - 标签权重变化  → CastVotes 提交成功后失效（所有前端的写路径都汇到那里）
//   - 账号被封/解封 → API commitUser 改完 users.status 后失效
//   - 历史底数回填  → BackfillVoteBase 完成后失效
package tags

import (
	"database/sql"
	"sync"
	"time"
)

// cloudTTL 是兜底过期时间（覆盖进程外的 sqlite3 直改）。主机制是显式失效。
const cloudTTL = 10 * time.Minute

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

	hooksMu sync.RWMutex
	hooks   []func(*sql.DB)
)

// OnDataChange 注册当标签权重或用户可见状态发生变更时的回调（例如通知 Gallery 首页 SSR 缓存与 API V2 Feed 缓存失效）。
// 通过在共享的 tags 层提供发布-订阅钩子，API 与 Gallery 无需互相耦合即可在同进程单端口下保持缓存实时同步。
func OnDataChange(fn func(*sql.DB)) {
	if fn == nil {
		return
	}
	hooksMu.Lock()
	hooks = append(hooks, fn)
	hooksMu.Unlock()
}

func notifyDataChange(db *sql.DB) {
	hooksMu.RLock()
	list := make([]func(*sql.DB), len(hooks))
	copy(list, hooks)
	hooksMu.RUnlock()
	for _, fn := range list {
		fn(db)
	}
}

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

// InvalidateCloud 清掉某个库的标签云缓存，并触发跨层变更回调。
func InvalidateCloud(db *sql.DB) {
	cloudMu.Lock()
	for k := range cloudData {
		if db == nil || k.db == db {
			delete(cloudData, k)
		}
	}
	cloudMu.Unlock()
	notifyDataChange(db)
}

// RefreshCloud 同步刷新指定库的标签云缓存，读 tag_counts 并填入 cloudData，同时通知关联缓存失效。
func (s *Store) RefreshCloud() {
	if s == nil || s.db == nil {
		return
	}
	key := cloudKey{db: s.db, excludeBanned: true}
	full, err := s.queryCloudAll(true)
	if err == nil && len(full) > 0 {
		cloudPut(key, full)
		notifyDataChange(s.db)
	} else {
		InvalidateCloud(s.db)
	}
}

// RefreshCloudByDB 同步刷新指定 sql.DB 的标签云缓存（供 API 包调用）。
func RefreshCloudByDB(db *sql.DB) {
	if db == nil {
		return
	}
	s := &Store{db: db}
	s.RefreshCloud()
}
