// Package tags 是账号标签（account_tags 表）的**唯一实现**。
//
// 背景：twitter-pic 有两层对外 API——根包的 twitter REST API
// （/api/twitter/tags/:username 读、POST /api/twitter/:username 写）与
// gallery 图站（/api/account-tags 读、POST /api/account-tag 写）。
// 两层历史上各有一套存储（user_tags JSON 大字段 / gallery 侧的投票 JSON 文件
// + 独立的 tags.db 快照），语义与数据源都不一致。
//
// 本包把读写收敛到**同一张 account_tags 表**（单一 twitter.db）：
//   - 写：CastVotes（votes.go）按「(账号,标签,IP) → 目标值」记票，流水照记；
//     account_tags.weight 是**物化滚动值** = 历史底数 + Σ票，恰好归零删行、负权重保留；
//   - 读：Weights / ForUsers / UsersForTag / Cloud / LastModifyFor。
//
// 两层都只调这里，保证「两边做成一样」。
package tags

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 驱动，CGO_ENABLED=0 可用
)

// Count 是标签云条目：标签名 + 覆盖账号数。
type Count struct {
	Tag   string
	Count int
}

// ErrNoStore 表示没有可用的库连接（twitter.db 缺失或打开失败）。
// 调用方据此决定降级方向：读不到 ≠ 一个都没有。
var ErrNoStore = errors.New("tags: 无可用数据库")

// Store 是 account_tags 的访问器。零值不可用，用 New 构造。
type Store struct {
	db *sql.DB
}

// New 用已打开的 *sql.DB 构造访问器（调用方负责驱动与连接串）。
func New(db *sql.DB) *Store { return &Store{db: db} }

// DB 返回底层连接（调用方需要自己跑语句时用）。
func (s *Store) DB() *sql.DB {
	if s == nil {
		return nil
	}
	return s.db
}

// EnsureSchema 建 account_tags / request_logs 与 tag 反查索引（幂等）。
// 根服务的 CreateTableV2 与 gallery 启动都调它，DDL 只此一份；
// request_logs 一并建，保证任一层先启动时写流水都不会失败。
func EnsureSchema(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("tags: nil db")
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS account_tags (
        username TEXT NOT NULL,
        tag      TEXT NOT NULL,
        weight   INTEGER NOT NULL DEFAULT 1,
        PRIMARY KEY (username, tag)
    ) WITHOUT ROWID;`)
	if err != nil {
		return fmt.Errorf("创建 account_tags 失败: %v", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_account_tags_tag ON account_tags(tag);`); err != nil {
		return fmt.Errorf("创建 account_tags 标签索引失败: %v", err)
	}
	// 票务三表（tag_votes / tag_weight_base + ip 索引）。DDL 同样只此一份。
	// 注意：历史底数的**快照**不在这里做，必须由调用方在自己的回填之后显式调
	// BackfillVoteBase —— 见 votes.go 里那段"调用时机"的注释。
	if err := EnsureVoteSchema(db); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS request_logs (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT,
		tags TEXT,
        ip TEXT,
        ua TEXT,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );`); err != nil {
		return fmt.Errorf("创建 request_logs 失败: %v", err)
	}
	return nil
}

// 写路径只有 CastVotes 一条（见 votes.go）。这里刻意不留 Add/累加型的旧接口：
// 旧语义是 weight += delta，与新语义 weight = 底数 + Σ票 互斥，留着就等于留一条
// 会静默破坏恒等式的旁路。读侧函数全部保持原语义不变。

// Weights 返回单个账号的 {tag: weight}（非零行，权重降序、同权按名字）。
// 读路径的规范形态：根 API 的 GET 与 gallery 的展示都以此为准。
func (s *Store) Weights(username string) map[string]int {
	out := map[string]int{}
	if s == nil || s.db == nil || username == "" {
		return out
	}
	rows, err := s.db.Query(`SELECT tag, weight FROM account_tags WHERE username = ? AND weight != 0
		ORDER BY weight DESC, tag`, username)
	if err != nil {
		log.Printf("tags: Weights %q: %v", username, err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var t string
		var w int
		if rows.Scan(&t, &w) == nil {
			out[t] = w
		}
	}
	return out
}

// ForUsers 按 username 分块查**正权重**标签，每账号按权重降序（分块 IN 走 PK 前缀）。
func (s *Store) ForUsers(names []string) map[string][]string {
	out := map[string][]string{}
	if s == nil || s.db == nil || len(names) == 0 {
		return out
	}
	const chunk = 400 // 远低于 SQLite 变量数上限
	for i := 0; i < len(names); i += chunk {
		batch := names[i:min(i+chunk, len(names))]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for k, n := range batch {
			args[k] = n
		}
		rows, err := s.db.Query(
			"SELECT username, tag FROM account_tags WHERE weight > 0 AND username IN ("+ph+") ORDER BY username, weight DESC, tag",
			args...)
		if err != nil {
			log.Printf("tags: ForUsers chunk@%d failed: %v", i, err)
			return out
		}
		for rows.Next() {
			var u, t string
			if err := rows.Scan(&u, &t); err != nil {
				continue
			}
			out[u] = append(out[u], t)
		}
		rows.Close()
	}
	return out
}

// UsersForTag 反查：tag -> usernames（正权重，权重降序，走 idx_account_tags_tag 索引）。
// exist 非 nil 时只保留其中存在的账号；**边扫边滤**（不能先 LIMIT 再过滤，
// 否则存在但权重靠后的账号会被截断丢掉），攒满 limit 提前停。
func (s *Store) UsersForTag(tag string, exist map[string]struct{}, limit int) []string {
	if s == nil || s.db == nil || tag == "" || limit <= 0 {
		return nil
	}
	rows, err := s.db.Query(
		`SELECT username FROM account_tags WHERE tag = ? AND weight > 0 ORDER BY weight DESC, username`, tag)
	if err != nil {
		log.Printf("tags: UsersForTag %q: %v", tag, err)
		return nil
	}
	defer rows.Close()
	out := make([]string, 0, min(limit, 512))
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			continue
		}
		if exist != nil {
			if _, ok := exist[u]; !ok {
				continue
			}
		}
		out = append(out, u)
		if len(out) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("tags: UsersForTag scan: %v", err)
	}
	return out
}

// UsersForTagAfter 反查：tag -> usernames（正权重，按 weight DESC, username ASC 排序，取 after 游标之后的账号）。
func (s *Store) UsersForTagAfter(tag string, after string, exist map[string]struct{}, limit int) []string {
	if s == nil || s.db == nil || tag == "" || limit <= 0 {
		return nil
	}
	if after == "" {
		return s.UsersForTag(tag, exist, limit)
	}

	var afterWeight int
	err := s.db.QueryRow(`SELECT weight FROM account_tags WHERE tag = ? AND username = ?`, tag, after).Scan(&afterWeight)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		log.Printf("tags: UsersForTagAfter scan afterWeight: %v", err)
		return nil
	}

	rows, err := s.db.Query(
		`SELECT username FROM account_tags 
		 WHERE tag = ? AND weight > 0 AND (weight < ? OR (weight = ? AND username > ?))
		 ORDER BY weight DESC, username`, tag, afterWeight, afterWeight, after)
	if err != nil {
		log.Printf("tags: UsersForTagAfter %q after %q: %v", tag, after, err)
		return nil
	}
	defer rows.Close()

	out := make([]string, 0, min(limit, 512))
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			continue
		}
		if exist != nil {
			if _, ok := exist[u]; !ok {
				continue
			}
		}
		out = append(out, u)
		if len(out) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("tags: UsersForTagAfter scan: %v", err)
	}
	return out
}

// UsersForTagOffset 反查：tag -> usernames（跳过 offset 个，取 limit 个）。
func (s *Store) UsersForTagOffset(tag string, offset int, exist map[string]struct{}, limit int) []string {
	if s == nil || s.db == nil || tag == "" || limit <= 0 {
		return nil
	}
	if offset <= 0 {
		return s.UsersForTag(tag, exist, limit)
	}
	rows, err := s.db.Query(
		`SELECT username FROM account_tags WHERE tag = ? AND weight > 0 ORDER BY weight DESC, username`, tag)
	if err != nil {
		log.Printf("tags: UsersForTagOffset %q: %v", tag, err)
		return nil
	}
	defer rows.Close()

	skipped := 0
	out := make([]string, 0, min(limit, 512))
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err != nil {
			continue
		}
		if exist != nil {
			if _, ok := exist[u]; !ok {
				continue
			}
		}
		if skipped < offset {
			skipped++
			continue
		}
		out = append(out, u)
		if len(out) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		log.Printf("tags: UsersForTagOffset scan: %v", err)
	}
	return out
}

// BannedUsernames 返回 users 表里**显式标记为非 SUCCESS** 的账号（封禁隐身用）。
//
// 返回值区分两种"空"：error != nil 才是读不到（库/表缺失），error == nil 且切片为空
// 表示「确实一个都没封」。调用方必须按这个区分决定降级方向——把"读不到"当成
// "一个都没封"是安全的（不隐身），反过来（读不到就全隐藏）会清空整站。
//
// 只认显式标记：磁盘上有 json.gz 但 users 表里没行的账号**不在**结果里——那是
// "没注册过"，不是"被封"；把它们一起藏起来会在数据形状不符合预期时把首页清空。
//
// 判定条件与根 API 的 `u.status = 'SUCCESS'` 严格互补：根侧要求等于 SUCCESS 才出现，
// 这里要求"有行且不等于 SUCCESS"才隐藏。
func (s *Store) BannedUsernames() ([]string, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoStore
	}
	rows, err := s.db.Query(
		`SELECT username FROM users WHERE status IS NULL OR status != 'SUCCESS'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err == nil {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}

// Cloud 返回覆盖账号数最多的前 n 个标签（仅正权重）。n<=0 时默认 36。
//
// excludeBanned=true 时在 SQL 里就排掉被封账号（users 表里 status 非 SUCCESS 的行），
// 而不是"先全量聚合、再由调用方按缓存扣减"——后者有个实测出来的正确性缺陷：
// 扣减用的是缓存快照，窗口内被封账号新增的标签根本扣不掉。
// users 表不存在时自动退回不过滤（fail-open：读不到封禁状态不该把标签云清空）。
//
// 结果走进程内缓存（见 cloud_cache.go）：查询是 23k×17k 的反连接 + GROUP BY，
// 线上实测 ~0.8s，而首页与 /api/tags/cloud 都是高频入口。缓存键含 excludeBanned
// 但**不含 n**——缓存全量榜单再切片，否则 ?limit=37/38/… 每个值都是一次全表聚合。
// 失效由写路径显式触发（CastVotes / commitUser / BackfillVoteBase），另有
// cloudTTL 兜底进程外直改。所以**重启不承担刷新职责**。
func (s *Store) Cloud(n int, excludeBanned bool) []Count {
	if s == nil || s.db == nil {
		return nil
	}
	if n <= 0 {
		n = 36
	}
	key := cloudKey{db: s.db, excludeBanned: excludeBanned}

	// 快路径：命中即切片返回。
	if full, ok := cloudGet(key); ok {
		return truncateCounts(full, n)
	}

	// 慢路径：只有一个调用者去查库，其余等待它的结果（single-flight）。
	// 没有这层的话，缓存失效瞬间的并发请求会各自触发一次 0.8s 全表聚合，
	// 正是要避免的"访问代价过高"。
	cloudMu.Lock()
	if full, ok := cloudGet(key); ok { // 等锁期间别人可能已经填好
		cloudMu.Unlock()
		return truncateCounts(full, n)
	}
	if wait, ok := cloudFlight[key]; ok {
		cloudMu.Unlock()
		<-wait
		if full, ok := cloudGet(key); ok {
			return truncateCounts(full, n)
		}
		return nil // 那次刷新失败：返回 nil 让调用方降级
	}
	wait := make(chan struct{})
	cloudFlight[key] = wait
	cloudMu.Unlock()

	full, err := s.queryCloudAll(excludeBanned)
	cloudMu.Lock()
	delete(cloudFlight, key)
	cloudMu.Unlock()
	close(wait)

	if err != nil {
		// 查询失败不写缓存（免得把失败固化 TTL 那么久），返回 nil 让调用方降级。
		return nil
	}
	cloudPut(key, full)
	return truncateCounts(full, n)
}

// truncateCounts 取前 n 条。入参已是副本（cloudGet/cloudPut 都做了 clone）。
func truncateCounts(v []Count, n int) []Count {
	if n <= 0 || len(v) <= n {
		return v
	}
	return v[:n]
}

// queryCloudAll 查全量榜单（上限 cloudMaxEntries），供缓存填充用。
// 一次查询服务所有 n，避免按 limit 分别打库。
func (s *Store) queryCloudAll(excludeBanned bool) ([]Count, error) {
	const base = `SELECT tag, COUNT(DISTINCT username) FROM account_tags a WHERE weight > 0`
	const banned = ` AND NOT EXISTS (SELECT 1 FROM users u WHERE u.username = a.username
	                             AND (u.status IS NULL OR u.status != 'SUCCESS'))`
	const tail = ` GROUP BY tag ORDER BY 2 DESC, tag LIMIT ?`

	q := base + tail
	if excludeBanned {
		q = base + banned + tail
	}
	out, err := s.queryCloud(q, cloudMaxEntries)
	if err != nil && excludeBanned {
		// 多半是 users 表还没建（老库/独立部署的图站库）：退回不过滤并说一声。
		log.Printf("tags: Cloud 排除被封账号失败，退回全量聚合: %v", err)
		out, err = s.queryCloud(base+tail, cloudMaxEntries)
	}
	if err != nil {
		log.Printf("tags: Cloud: %v", err)
		return nil, err
	}
	return out, nil
}

func (s *Store) queryCloud(q string, n int) ([]Count, error) {
	rows, err := s.db.Query(q, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Count, 0, n)
	for rows.Next() {
		var c Count
		if err := rows.Scan(&c.Tag, &c.Count); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// LastModifyFor 按 username 分块查 users.last_modify（PK 点查）。
// 统一后的单一库里没有独立的 accounts 表，时间排序源就是 users。
//
// 返回值一律规范成定宽 "YYYY-MM-DD HH:MM:SS"（UTC）——首页排序依赖
// 「字典序 == 时间序」，而 sqlite 的 TIMESTAMP 列经驱动可能回来
// text / time.Time / RFC3339 三种形态，不规范化就会随驱动行为漂移。
func (s *Store) LastModifyFor(names []string) map[string]string {
	out := map[string]string{}
	if s == nil || s.db == nil || len(names) == 0 {
		return out
	}
	const chunk = 400
	for i := 0; i < len(names); i += chunk {
		batch := names[i:min(i+chunk, len(names))]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for k, n := range batch {
			args[k] = n
		}
		rows, err := s.db.Query("SELECT username, last_modify FROM users WHERE username IN ("+ph+")", args...)
		if err != nil {
			log.Printf("tags: LastModifyFor: %v", err)
			return out
		}
		for rows.Next() {
			var u string
			var lm any
			if err := rows.Scan(&u, &lm); err != nil {
				continue
			}
			out[u] = normTimestamp(lm)
		}
		rows.Close()
	}
	return out
}

// tsLayout 是首页排序依赖的定宽格式：字典序 == 时间序。
const tsLayout = "2006-01-02 15:04:05"

// normTimestamp 把驱动可能返回的多种时间形态（time.Time / []byte / RFC3339 /
// 裸文本）统一成定宽 "YYYY-MM-DD HH:MM:SS"（UTC）。认不出的原样返回，
// 不猜时区——宁缺勿错，错乱的排序比缺值更难查。
func normTimestamp(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case time.Time:
		return x.UTC().Format(tsLayout)
	case []byte:
		return normTimestampStr(string(x))
	case string:
		return normTimestampStr(x)
	default:
		return fmt.Sprint(x)
	}
}

func normTimestampStr(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "<nil>" {
		return ""
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, tsLayout} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC().Format(tsLayout)
		}
	}
	return s
}
