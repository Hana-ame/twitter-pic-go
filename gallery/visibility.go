package gallery

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/Hana-ame/twitter-pic-go/tags"
)

// 被封账号在图站隐身。
//
// 真源与根 API 同一个：twitter.db 的 users.status。根侧一直是
// `WHERE u.status = 'SUCCESS'`（GET /:fn 回 banned.json、by=tag 直接不出现），
// 图站原先只看「磁盘上有没有 <name>.json.gz」，于是同一批数据在两层表现不同：
// 实测把 demo 置 BANNED 后 `GET /api/twitter/?by=tag&search=…` 回 []，
// 而 gallery 的 /api/tag/{tag} 仍回 {"users":["demo"]}、/u/demo 仍 200。
// 本文件把图站所有会泄漏被封账号的读路径统一到这一个判定器上。
//
// 降级方向是**读不到就不隐身**（fail-open）：把整站藏起来等于自我 DoS，
// 而"谁被封"这件事本身没坏，坏的只是可见性。与 ipban 的
// 「bans.txt 缺失 = 空表放行」同向。
const visTTL = 60 * time.Second

// vis 缓存封禁视图：单个账号的隐身判定 + 已排除被封账号的标签云。
// 多个 handler 并发读，故加锁；刷新按 TTL 惰性触发（不常驻协程，
// 首页以外的低频页面不会白拉库）。
type vis struct {
	mu    sync.Mutex
	store *tags.Store
	ttl   time.Duration
	at    time.Time

	banned    map[string]struct{} // users 表里显式非 SUCCESS 的账号
	cloudView []tags.Count        // 已在 SQL 侧排除被封账号的标签云
	haveView  bool                // 是否成功刷新过至少一次（决定 fail-open）
	warned    bool
}

func newVis(store *tags.Store) *vis {
	return &vis{store: store, ttl: visTTL}
}

// refreshLocked 按 TTL 拉一次封禁视图。调用方必须已持有 mu。
func (v *vis) refreshLocked() {
	if v.store == nil || time.Since(v.at) < v.ttl {
		return
	}
	list, err := v.store.BannedUsernames()
	if err != nil {
		// 读不到：保留上一次视图（首次就是"不隐身"），并吼一声。
		// 只吼第一次，避免 users 表缺失时每次刷新都刷一行日志。
		if !errors.Is(err, tags.ErrNoStore) || !v.warned {
			log.Printf("gallery: 封禁账号视图刷新失败，暂不隐身（fail-open）: %v", err)
			v.warned = true
		}
		return
	}
	m := make(map[string]struct{}, len(list))
	for _, u := range list {
		m[u] = struct{}{}
	}
	v.banned = m
	// 标签云必须在**同一次刷新**里重算：它靠 SQL 排除，不接受"用旧聚合事后扣减"
	// ——扣减用的是快照，窗口内被封账号新增的标签会扣不掉（实测过这个缺陷）。
	v.cloudView = v.store.Cloud(cloudTopN, true)
	v.at = time.Now()
	v.haveView = true
	v.warned = false
}

// hidden 判定某个账号是否该在图站隐身。
func (v *vis) hidden(name string) bool {
	if v == nil {
		return false
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refreshLocked()
	if !v.haveView {
		return false // 从没成功刷新过 → 不隐身（fail-open）
	}
	_, ok := v.banned[name]
	return ok
}

// filter 返回可见账号（保持入参顺序），供首页列表与标签反查复用。
func (v *vis) filter(names []string) []string {
	if v == nil {
		return names
	}
	v.mu.Lock()
	v.refreshLocked()
	haveView, banned := v.haveView, v.banned
	v.mu.Unlock()
	if !haveView {
		return names
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, isBanned := banned[n]; isBanned {
			continue
		}
		out = append(out, n)
	}
	return out
}

// cloud 返回标签云（已排除被封账号）。视图没建立时返回 nil，
// 前端会自己从（已过滤的）#a-data 重算，不会因此漏出被封账号。
func (v *vis) cloud() []tags.Count {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.refreshLocked()
	return v.cloudView
}
