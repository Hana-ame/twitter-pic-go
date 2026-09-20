package gallery

import (
	"log"
	"sync"
	"time"
)

// homeCacheTTL 是首页整页 SSR 结果的默认保鲜期（秒）。见 homeCache 的说明。
const homeCacheTTL = 60 * time.Second

// homeCache 缓存**整页首页 HTML**。
//
// 为什么值得缓存：构造一次首页要 os.ReadDir 列全部账号、对全部账号分块查标签与
// 更新时间（ForUsers + LastModifyFor，各 40+ 条 IN 查询）、把上万条记录序列化成
// #a-data、再渲染整页模板。bwh 是 1 vCPU / 528MB 且长期用着 swap 的机器，同一次
// 构造实测从 0.5s 到 40s+ 都有；而首页内容对每个访客完全一致（每人自己的投票
// 状态由前端另拉 /api/tags），所以整页可以直接缓存。
//
// 语义是 stale-while-revalidate：
//   - 有缓存页 → 直接发；**过期也先发旧页**，顺手触发一次后台重建；
//   - 并发过期 → busy 标志保证同一时刻只有一份后台重建；
//   - 冷启动（还没有任何页）→ 同步构造，并发请求在 buildMu 上串行（single-flight），
//     后来的请求等第一个构造完直接复用，不做 N 份重复重活；
//   - 构造失败 → 保留旧页继续服务（宁可旧，不可空），错误只写日志。
//
// 于是除进程刚启动后的第一个访客外，任何人都不必等构造；启动时另有一次预热
// （warm，bwh 每日 04:25 重启）。
type homeCache struct {
	ttl   time.Duration
	build func() ([]byte, error)

	buildMu sync.Mutex // 构造串行化：冷启动与后台重建共用，任何时刻只有一份在跑
	mu      sync.Mutex // 保护以下字段
	body    []byte
	at      time.Time
	have    bool
	busy    bool
}

// newHomeCache 建首页缓存；ttl <= 0 表示关闭缓存（返回 nil，调用方每次现构造）。
func newHomeCache(ttl time.Duration, build func() ([]byte, error)) *homeCache {
	if ttl <= 0 || build == nil {
		return nil
	}
	return &homeCache{ttl: ttl, build: build}
}

// html 取可发的首页 HTML：命中（含过期的旧页）直接返回，冷启动则同步构造。
func (c *homeCache) html() ([]byte, error) {
	if b, ok := c.fresh(); ok {
		return b, nil
	}
	return c.buildOnce()
}

// fresh 返回缓存页；ok=false 表示还没有任何页可发（冷启动）。
// 发现页已过期时顺手起一次后台重建，**不**阻塞本次请求。
func (c *homeCache) fresh() ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.have {
		return nil, false
	}
	if time.Since(c.at) > c.ttl && !c.busy {
		c.busy = true
		go c.refresh()
	}
	return c.body, true
}

// refresh 是后台重建路径：失败只记日志，旧页继续服务。
func (c *homeCache) refresh() {
	if _, err := c.buildOnce(); err != nil {
		log.Printf("gallery: 首页缓存后台重建失败（继续用旧页）: %v", err)
	}
}

// buildOnce 构造一次首页并写进缓存。
//
// buildMu 保证任何时刻只有一份构造在跑（冷启动并发与后台重建一起串行）；
// 等锁期间缓存若已被别人填成新鲜页，直接复用，不做重复重活。
// 构造失败时保留旧页，把错误交回调用方。
func (c *homeCache) buildOnce() ([]byte, error) {
	c.buildMu.Lock()
	defer c.buildMu.Unlock()

	c.mu.Lock()
	if c.have && time.Since(c.at) <= c.ttl {
		b := c.body
		c.mu.Unlock()
		return b, nil
	}
	c.mu.Unlock()

	b, err := c.build()

	c.mu.Lock()
	c.busy = false
	if err == nil {
		c.body, c.at, c.have = b, time.Now(), true
	}
	old := c.body
	c.mu.Unlock()

	if err != nil {
		return old, err
	}
	return b, nil
}

// invalidate 把缓存标记为过期：旧页仍在（供 stale-while-revalidate 先发），
// 下一个访客触发一次后台重建。标签写入后调用——首页的标签云与账号标签
// 排序都来自 account_tags。
func (c *homeCache) invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

// warm 预热一次；启动时在后台调用，让重启后的第一个访客不必付构造代价。
func (c *homeCache) warm() {
	if c == nil {
		return
	}
	start := time.Now()
	if _, err := c.buildOnce(); err != nil {
		log.Printf("gallery: 首页 SSR 缓存预热失败: %v", err)
		return
	}
	log.Printf("gallery: 首页 SSR 缓存已预热（%s，TTL %s）", time.Since(start), c.ttl)
}
