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
	// 唯一真相表 user_tag_cnt
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS user_tag_cnt (
        username   TEXT NOT NULL,
        tag        TEXT NOT NULL,
        cnt        INTEGER NOT NULL DEFAULT 1,
        updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
        PRIMARY KEY (username, tag)
    ) WITHOUT ROWID;`); err != nil {
		return fmt.Errorf("创建 user_tag_cnt 失败: %v", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_user_tag_cnt_tag ON user_tag_cnt(tag, updated_at DESC);`); err != nil {
		return fmt.Errorf("创建 user_tag_cnt 索引失败: %v", err)
	}
	// 若 account_tags 已有数据，同步填入 user_tag_cnt
	_, _ = db.Exec(`INSERT OR IGNORE INTO user_tag_cnt (username, tag, cnt, updated_at)
		SELECT username, tag, weight, CURRENT_TIMESTAMP FROM account_tags`)
	// 票务三表（tag_votes / tag_weight_base + ip 索引）。DDL 同样只此一份。
	// 注意：历史底数的**快照**不在这里做，必须由调用方在自己的回填之后显式调
	// BackfillVoteBase —— 见 votes.go 里那段"调用时机"的注释。
	if err := EnsureVoteSchema(db); err != nil {
		return err
	}
	// 全局标签云汇总表 tag_counts
	if err := EnsureTagCounts(db); err != nil {
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

// EnsureTagCounts 创建并初始化全局标签云汇总表 tag_counts
func EnsureTagCounts(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("tags: nil db")
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tag_counts (
		tag TEXT PRIMARY KEY,
		cnt INTEGER NOT NULL
	);`); err != nil {
		return fmt.Errorf("创建 tag_counts 失败: %v", err)
	}

	// **每次启动都从真实聚合重建 tag_counts**，不再只在空表时建一次。
	//
	// 以前是 `if n == 0`，于是 tag_counts 一旦非空就永远不再对账：它此后只由
	// CastVotes 的 ±1 增量与 commitUser 维护，**历史数据一条都不计入**。
	// 后果（复现见 tags/membership_contract_test.go 的
	// TestCloudCountMustEqualRealMembership）：6 个账号持有「女性」，云本该报 6，
	// 但只要有**一个 IP 投过一票**，物化表就从 0 长到 1 并从此接管，
	// queryCloudAll 的 ban 过滤分支再也不回退真实聚合 —— 云永久把 6 报成 1。
	// 而且 ban 是在进程外用 SQL 打的（见 gallery/visibility.go 的运维路径），
	// tag_counts 同样看不到，于是**封禁账号永远不会从标签云里消失**。
	//
	// 代价是一次全表聚合（account_tags 约 4.4k 行、user_tag_cnt 同量级），
	// 启动时跑一次完全可接受；换来的是「云的人数 = 真实持有账号数」这条
	// 不变量在**每次重启后自动复位**，而不是靠人记得去手工对账。
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM tag_counts`).Scan(&n)
	if n >= 0 {
		// 优先从 user_tag_cnt 聚合
		res, err := db.Exec(`INSERT OR REPLACE INTO tag_counts (tag, cnt)
			SELECT a.tag, COUNT(DISTINCT a.username)
			FROM user_tag_cnt a
			WHERE a.cnt > 0
			  AND EXISTS (SELECT 1 FROM users u WHERE u.username = a.username AND u.status = 'SUCCESS')
			GROUP BY a.tag`)
		if err != nil {
			res, _ = db.Exec(`INSERT OR REPLACE INTO tag_counts (tag, cnt)
				SELECT a.tag, COUNT(DISTINCT a.username)
				FROM user_tag_cnt a
				WHERE a.cnt > 0
				GROUP BY a.tag`)
		}
		var rowsAff int64
		if res != nil {
			rowsAff, _ = res.RowsAffected()
		}
		if rowsAff == 0 {
			// user_tag_cnt 暂无数据时，尝试从 account_tags 聚合
			_, err = db.Exec(`INSERT OR REPLACE INTO tag_counts (tag, cnt)
				SELECT a.tag, COUNT(DISTINCT a.username)
				FROM account_tags a
				WHERE a.weight > 0
				  AND EXISTS (SELECT 1 FROM users u WHERE u.username = a.username AND u.status = 'SUCCESS')
				GROUP BY a.tag`)
			if err != nil {
				_, _ = db.Exec(`INSERT OR REPLACE INTO tag_counts (tag, cnt)
					SELECT a.tag, COUNT(DISTINCT a.username)
					FROM account_tags a
					WHERE a.weight > 0
					GROUP BY a.tag`)
			}
		}
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
	rows, err := s.db.Query(`SELECT tag, cnt FROM user_tag_cnt WHERE username = ? AND cnt != 0
		ORDER BY cnt DESC, tag`, username)
	if err == nil {
		for rows.Next() {
			var t string
			var w int
			if rows.Scan(&t, &w) == nil {
				out[t] = w
			}
		}
		rows.Close()
	}
	if len(out) == 0 {
		// 回退到 account_tags
		rows, err := s.db.Query(`SELECT tag, weight FROM account_tags WHERE username = ? AND weight != 0
			ORDER BY weight DESC, tag`, username)
		if err == nil {
			for rows.Next() {
				var t string
				var w int
				if rows.Scan(&t, &w) == nil {
					out[t] = w
				}
			}
			rows.Close()
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
			"SELECT username, tag FROM user_tag_cnt WHERE cnt > 0 AND username IN ("+ph+") ORDER BY username, cnt DESC, tag",
			args...)
		batchCount := 0
		if err == nil {
			for rows.Next() {
				var u, t string
				if err := rows.Scan(&u, &t); err == nil {
					out[u] = append(out[u], t)
					batchCount++
				}
			}
			rows.Close()
		}
		if batchCount == 0 {
			rows, err := s.db.Query(
				"SELECT username, tag FROM account_tags WHERE weight > 0 AND username IN ("+ph+") ORDER BY username, weight DESC, tag",
				args...)
			if err == nil {
				for rows.Next() {
					var u, t string
					if err := rows.Scan(&u, &t); err == nil {
						out[u] = append(out[u], t)
					}
				}
				rows.Close()
			}
		}
	}
	return out
}

// UsersForTagPaged tag 反查账号分页：以 user_tag_cnt 为唯一真相，**按权重降序**
// （同权重按 username 升序）。这条顺序是跨端契约的一部分，见上面的注释。
func (s *Store) UsersForTagPaged(tag string, limit, offset int, excludeBanned bool) ([]string, int, error) {
	if s == nil || s.db == nil || tag == "" {
		return nil, 0, nil
	}
	var total int
	countSQL := `SELECT COUNT(*) FROM user_tag_cnt a WHERE a.tag = ? AND a.cnt > 0`
	if excludeBanned {
		countSQL += ` AND EXISTS (SELECT 1 FROM users u WHERE u.username = a.username
		                            AND u.status = 'SUCCESS')`
	}
	// 只在**查询出错**时才回退到不带过滤的 count——注意回退的 count 也必须
	// 带上同一个可见性条件，否则 total 会比实际能取到的行数多（客户端拿
	// total 判断「还有没有下一页」，多算就会多翻空页）。
	//
	// ⚠️ `total == 0` **不**再触发回退。原来写成 `err != nil || total == 0`：
	// 「主表算出来 0 条」被当成了「主表不可用」，于是转去 account_tags。
	// 但 0 条完全可能是**真的**没有可见账号（全部被封或不存在），此时回退
	// 会换一个仍含不可见账号的计数，把刚排除掉的 ghost 又算回总数。
	// 「主表为空」与「主表不可用」是两件事，只有 err 才能区分。
	err := s.db.QueryRow(countSQL, tag).Scan(&total)
	if err != nil && excludeBanned {
		countSQL = `SELECT COUNT(*) FROM user_tag_cnt a WHERE a.tag = ? AND a.cnt > 0`
		if excludeBanned {
			countSQL += ` AND EXISTS (SELECT 1 FROM users u WHERE u.username = a.username
			                            AND u.status = 'SUCCESS')`
		}
		err = s.db.QueryRow(countSQL, tag).Scan(&total)
	}
	useAccountTags := false
	if err != nil || total == 0 {
		// account_tags 是标签归属的**权威**来源（user_tag_cnt 只是增量镜像），
		// 所以主表算不出可见数时以它为准。
		//
		// ⚠️ 这里必须区分两种「主表不可用」：
		//   - `err != nil`：查询出错（users 表缺失等）→ 回退是**必要的**；
		//   - `total == 0`：主表里这个标签**确实没有可见账号**。此时回退要
		//     重新用 account_tags 算一遍**带同样可见性条件**的数，而不是直接
		//     沿用不带过滤的 oldTotal——否则刚排除掉的 ghost/banned 又被算回
		//     总数，客户端会据此多翻空页。
		oldSQL := `SELECT COUNT(*) FROM account_tags a WHERE a.tag = ? AND a.weight > 0`
		if excludeBanned {
			oldSQL += ` AND EXISTS (SELECT 1 FROM users u WHERE u.username = a.username
			                         AND u.status = 'SUCCESS')`
		}
		var oldTotal int
		oldErr := s.db.QueryRow(oldSQL, tag).Scan(&oldTotal)
		// 若**带过滤**的计数拿不到数（oldTotal == 0），再试一次不带过滤的：
		// `users` 表整体缺失时 EXISTS 子查询对每一行都返回假，于是计数是 0，
		// 而这不是「这个标签没有可见账号」，是**判据本身失效**（与
		// visibility.go 的 fail-open 同理）。此时退回不带过滤的计数，
		// 否则 users 表一缺失，整个标签页就全空了。
		if oldTotal == 0 {
			var raw int
			if e := s.db.QueryRow(
				`SELECT COUNT(*) FROM account_tags a WHERE a.tag = ? AND a.weight > 0`,
				tag).Scan(&raw); e == nil && raw > 0 {
				oldTotal, oldErr = raw, nil
			}
		}
		if oldTotal > 0 || oldErr == nil {
			total = oldTotal
			useAccountTags = oldTotal > 0
		}
	}

	var querySQL string
	if useAccountTags {
		querySQL = `SELECT a.username FROM account_tags a WHERE a.tag = ? AND a.weight > 0`
		if excludeBanned {
			querySQL += ` AND EXISTS (SELECT 1 FROM users u WHERE u.username = a.username
			                            AND u.status = 'SUCCESS')`
		}
		querySQL += ` ORDER BY a.weight DESC, a.username ASC`
	} else {
		querySQL = `SELECT a.username FROM user_tag_cnt a WHERE a.tag = ? AND a.cnt > 0`
		if excludeBanned {
			querySQL += ` AND EXISTS (SELECT 1 FROM users u WHERE u.username = a.username
			                            AND u.status = 'SUCCESS')`
		}
		// **按权重降序**，与 account_tags 分支及跨端契约一致
		// （notes/design-twitter-pic-flutter-tag-filter-contract：「服务端按
		// account_tags 正权重降序返回」）。
		//
		// 原先是 `ORDER BY a.updated_at DESC`（最新更新的排最前）。统一库把
		// account_tags 灌进 user_tag_cnt 时，updated_at 全部写成 CURRENT_TIMESTAMP，
		// 之后只有被投票改过的行才会刷新 —— 于是反查顺序实际由「谁最近被改过」
		// 决定，与权重毫无关系。实测反例见 tags/order_contract_test.go 的
		// TestUsersForTagPagedOrdersByWeightDesc：权重最高的账号被排到最后一位。
		querySQL += ` ORDER BY a.cnt DESC, a.username ASC`
	}

	var rows *sql.Rows
	if limit > 0 {
		querySQL += ` LIMIT ? OFFSET ?`
		rows, err = s.db.Query(querySQL, tag, limit, offset)
	} else {
		rows, err = s.db.Query(querySQL, tag)
	}
	if err != nil && excludeBanned {
		if useAccountTags {
			querySQL = `SELECT a.username FROM account_tags a WHERE a.tag = ? AND a.weight > 0 ORDER BY a.weight DESC, a.username ASC`
		} else {
			querySQL = `SELECT a.username FROM user_tag_cnt a WHERE a.tag = ? AND a.cnt > 0 ORDER BY a.cnt DESC, a.username ASC`
		}
		if limit > 0 {
			querySQL += ` LIMIT ? OFFSET ?`
			rows, err = s.db.Query(querySQL, tag, limit, offset)
		} else {
			rows, err = s.db.Query(querySQL, tag)
		}
	}
	if err != nil {
		log.Printf("tags: UsersForTagPaged %q: %v", tag, err)
		return nil, 0, err
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var u string
		if err := rows.Scan(&u); err == nil {
			out = append(out, u)
		}
	}
	return out, total, rows.Err()
}

// UsersForTag 反查：tag -> usernames（优先 user_tag_cnt，**按权重降序**；
// 支持 exist 过滤与 limit）。
func (s *Store) UsersForTag(tag string, exist map[string]struct{}, limit int) []string {
	if s == nil || s.db == nil || tag == "" || limit <= 0 {
		return nil
	}
	// 先从 user_tag_cnt 查
	users, _, _ := s.UsersForTagPaged(tag, limit, 0, false)

	// **account_tags 里的账号也必须一并算进来，而不是「只在返回 0 行时才回退」。**
	//
	// 原来的 `if len(users) == 0` 回退是「一个 IP 投一票就把整个标签的人数
	// 砍到 1」的根因：user_tag_cnt 只在**被投过票**的 (账号,标签) 上有行，
	// 所以哪怕它返回了 1 行，另外 N-1 个从未被投过票的账号就被这一行
	// 「挡」掉了（复现见 membership_contract_test.go 的
	// TestSingleVoteMustNotTruncateTagMembership）。
	//
	// 两表并集、且只保留正权重，既修掉截断，又不依赖启动时那次对齐是否跑过
	// （对齐只是兜底，这里才是真正的不变量所在）。
	rows, err := s.db.Query(
		`SELECT username FROM account_tags WHERE tag = ? AND weight > 0 ORDER BY weight DESC, username`, tag)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var u string
			if err := rows.Scan(&u); err == nil {
				users = append(users, u)
			}
		}
	}
	// 两表并集后同一个 username 可能出现两次（user_tag_cnt 一份、
	// account_tags 一份），去重时**保留先出现的那个**——也就是 user_tag_cnt
	// 那份，它已按 cnt DESC 排好，所以权重顺序与契约一致。
	out := make([]string, 0, min(limit, len(users)))
	seen := make(map[string]struct{}, len(users))
	for _, u := range users {
		if _, dup := seen[u]; dup {
			continue
		}
		seen[u] = struct{}{}
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

// Cloud 返回标签云计数（仅正权重，每个拥有该标签且分数>0的用户计为1）。n<=0 时返回全量（不设上限）。
func (s *Store) Cloud(n int, excludeBanned bool) []Count {
	if s == nil || s.db == nil {
		return nil
	}
	key := cloudKey{db: s.db, excludeBanned: excludeBanned}

	// 快路径：命中即切片返回。
	if full, ok := cloudGet(key); ok {
		if n <= 0 {
			return full
		}
		return truncateCounts(full, n)
	}

	// 慢路径：只有一个调用者去查库，其余等待它的结果（single-flight）。
	cloudMu.Lock()
	if e, ok := cloudData[key]; ok && time.Since(e.at) <= cloudTTL { // 等锁期间别人可能已经填好
		cloudMu.Unlock()
		val := cloneCounts(e.val)
		if n <= 0 {
			return val
		}
		return truncateCounts(val, n)
	}
	if wait, ok := cloudFlight[key]; ok {
		cloudMu.Unlock()
		<-wait
		if full, ok := cloudGet(key); ok {
			if n <= 0 {
				return full
			}
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
		return nil
	}
	cloudPut(key, full)
	if n <= 0 {
		return full
	}
	return truncateCounts(full, n)
}

// truncateCounts 取前 n 条。入参已是副本（cloudGet/cloudPut 都做了 clone）。
func truncateCounts(v []Count, n int) []Count {
	if n <= 0 || len(v) <= n {
		return v
	}
	return v[:n]
}

// queryCloudAll 查全量榜单（不设上限），供缓存填充用。
// 一个 user 拥有该 tag 且 cnt > 0 时记为 1。
func (s *Store) queryCloudAll(excludeBanned bool) ([]Count, error) {
	if excludeBanned {
		// tag_counts 是**物化增量表**：只在 CastVotes 的 ±1 转移里长大，
		// 历史数据一条都不计入。于是「只要有**一个** IP 投过一票，这个 tag 的
		// cnt 就从 0 长到 1 并从此接管」——6 个账号的标签被报成 1（复现见
		// membership_contract_test.go 的 TestCloudCountMustEqualRealMembership）。
		//
		// 同一个分支还让**进程外封禁**的账号永远留在云里：ban 是运维用 SQL 打的
		// （gallery/visibility.go 记着这条路径），tag_counts 看不到，而
		// cloud_cache.go 的 cloudTTL 只管内存缓存、过期后重读的还是这张错表。
		//
		// 判据：物化表里**没有任何 tag 的计数超过真实聚合**时才用它当快路径。
		// 慢路径（下面的实时聚合）本来就是权威答案，所以宁可慢也不给错数。
		rows, err := s.db.Query(`SELECT tag, cnt FROM tag_counts WHERE cnt > 0 ORDER BY cnt DESC, tag ASC`)
		if err == nil {
			var out []Count
			for rows.Next() {
				var c Count
				if err := rows.Scan(&c.Tag, &c.Count); err == nil {
					out = append(out, c)
				}
			}
			rows.Close()
			if len(out) > 0 && s.tagCountsNotUndercounting(out) {
				return out, nil
			}
		}
	}

	// 权威口径：**两表并集**上数「正权重账号」。
	//
	// 以前只数 user_tag_cnt，而 user_tag_cnt 只在**被投过票**的 (账号,标签) 上
	// 有行 —— 于是「一个 IP 投一票」就把标签云从 6 砍到 1（复现见
	// membership_contract_test.go）。account_tags 才是「这个账号持有这个标签」
	// 的完整来源，user_tag_cnt 只是它的增量镜像，所以并集才是真实人数。
	// 子查询包一层 m：外层的 banned 条件要引用 m.username，SQLite 在
	// 「派生表别名 t + 外层别名 t」同名时会解析失败（near "AND": syntax error）。
	//
	// banned 条件必须落在**派生表内部的 SELECT 上**（即 `WHERE ... AND NOT
	// EXISTS`），不能拼在 `) m` 之后 —— 那样 SQL 会变成
	// `FROM (...) m AND NOT EXISTS (...) GROUP BY`，直接 near "AND": syntax
	// error。拼错的症状很隐蔽：queryCloud 报错后代码会 **fail-open** 退回不带
	// 排除的全量聚合，于是「封禁账号又回到标签云里」，而日志里只有一行
	// 「Cloud 排除被封账号失败」——看起来像偶发告警，其实是每次都错。
	//
	// banned 条件**套在并集外面**（`FROM ( ... UNION ... ) x WHERE NOT EXISTS`），
	// 不能塞进 UNION 的两个分支里：相关子查询在 UNION 分支内引用派生表别名，
	// SQLite 直接报 `no such column: x.username`。同理也不能拼在 `) x` 之后、
	// GROUP BY 之前的位置之外——那样会变成 near "AND": syntax error。
	//
	// 这两个 SQL 坑的症状都很隐蔽：queryCloud 报错后代码 fail-open 退回不带排除的
	// 全量聚合，于是「封禁账号又回到标签云里」，日志里只有一行看起来像偶发告警的
	// 「Cloud 排除被封账号失败」，实际是每次都错。
	inner := func(banned bool) string {
		b := ""
		if banned {
			b = ` WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.username = x.username
				AND (u.status IS NULL OR u.status != 'SUCCESS'))`
		}
		return `SELECT x.tag, COUNT(DISTINCT x.username) FROM (` +
			`SELECT tag AS tag, username AS username FROM user_tag_cnt WHERE cnt > 0` +
			` UNION ` +
			`SELECT tag AS tag, username AS username FROM account_tags WHERE weight > 0` +
			`) x` + b + ` GROUP BY x.tag ORDER BY 2 DESC, x.tag ASC`
	}

	// 权威查询本身已经是两表并集，所以不再需要「空了就退回 account_tags」那一层
	// 回退（旧回退只在 len(out)==0 时触发，而那恰恰是「并集也为空」的情形，
	// 再查一次单表只会得到同样的空）。
	out, err := s.queryCloud(inner(excludeBanned))
	if err != nil && excludeBanned {
		// fail-open：排除封禁的查询失败（典型是 users 表还不存在）时退回全量聚合，
		// 而不是把标签云清空。
		log.Printf("tags: Cloud 排除被封账号失败，退回全量聚合: %v", err)
		out, err = s.queryCloud(inner(false))
	}
	if err != nil {
		log.Printf("tags: Cloud: %v", err)
		return nil, err
	}
	return out, nil
}

func (s *Store) queryCloud(q string) ([]Count, error) {
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Count
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

// tagCountsNotUndercounting 判断物化表 tag_counts 能不能当快路径用。
//
// 判据（可证伪，测试见 membership_contract_test.go）：物化表里出现的**每一个**
// tag，其 cnt 都必须 <= 真实聚合（COUNT DISTINCT 正权重账号）。只要有一个
// tag 报小了，物化表就是「部分数据」，此时必须回退到实时聚合。
//
// 为什么只查「有没有报小」而不是「全等」：全等要扫一遍 user_tag_cnt 与
// account_tags 的并集，成本和慢路径一样；快路径的价值只在于**便宜**，
// 一旦必须付慢路径的钱就该直接走慢路径。而「报小」是本 bug 的唯一形态
// （投票只增不减 cnt），报大只可能来自并发误加，允许它走快路径。
func (s *Store) tagCountsNotUndercounting(materialized []Count) bool {
	if s == nil || s.db == nil || len(materialized) == 0 {
		return false
	}
	for _, c := range materialized {
		var real int
		err := s.db.QueryRow(
			`SELECT COUNT(DISTINCT username) FROM account_tags WHERE tag = ? AND weight > 0`,
			c.Tag).Scan(&real)
		if err != nil {
			return false
		}
		// account_tags 还没有这行（只存在于 user_tag_cnt 的新票路径），跳过。
		if real == 0 {
			continue
		}
		if c.Count < real {
			// 报小了 —— 物化表只见过「投过票的那部分账号」，不能用。
			return false
		}
	}
	return true
}
