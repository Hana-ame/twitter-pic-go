// 本文件是「一个 IP 一票」的实现。三张表构成一个可审计的票务系统：
//
//	tag_votes        —— 票账本：谁（IP）给谁的哪个标签投了什么**目标值**（±1）。撤票=删行。
//	tag_weight_base  —— 历史底数：某行第一次被票碰到时，当时的 weight 快照（之后永不改写）。
//	account_tags     —— **物化的滚动值**，不是真源。恒等式：
//	                   weight = IFNULL(base,0) + IFNULL(Σ votes.value,0)
//
// 为什么 account_tags 留着而不是每次读时现算：五条读路径（Weights/ForUsers/UsersForTag/
// Cloud 及根 API 的聚合）全部只看 account_tags，改「读时求和」要动全部 SQL 与索引、
// 且 Cloud 的 COUNT(DISTINCT) 变成带 GROUP BY 的子查询。所以把改动面压在写侧：
// 每次记票在同一事务里把该行的滚动值**重算**回去（不是累加），语义等价且自愈——
// 滚动值被外力改坏，下一张票就把它修回来。
//
// 为什么底数是**独立表**而不是 account_tags 加一列：account_tags 的行在权重恰好归零时
// 会被删除（既有语义），加列会跟着行一起消失，下次投票重建该行时无从恢复底数。

package tags

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
)

// 票的目标值（请求侧 d 的取值集合）。
const (
	VoteUp   = 1
	VoteDown = -1
	VoteUndo = 0 // 撤票
)

// ErrNoPrincipal 表示拿不到客户端 IP。
//
// 必须报错而不是退化成空串：票是按 (账号,标签,IP) 记的，IP 取成 "" 会把**所有**
// 访客塌成同一个票桶，一 IP 一票当场变成"全站一人一票"。取不到身份时宁可拒绝写入。
var ErrNoPrincipal = errors.New("tags: 无法确定客户端 IP")

// Vote 是一条票（读账本时用）。
type Vote struct {
	Username string
	Tag      string
	IP       string
	Value    int
}

// EnsureVoteSchema 建票务三表与 ip 反查索引（幂等）。由 tags.EnsureSchema 统一调用。
func EnsureVoteSchema(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("tags: nil db")
	}
	// 票账本。PK 即"同一 IP 对同一 (账号,标签) 只有一张票"，无需额外唯一约束。
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tag_votes (
		username   TEXT    NOT NULL,
		tag        TEXT    NOT NULL,
		ip         TEXT    NOT NULL,
		value      INTEGER NOT NULL,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (username, tag, ip)
	) WITHOUT ROWID;`); err != nil {
		return fmt.Errorf("创建 tag_votes 失败: %v", err)
	}
	// 运维口径：「这个 IP 都投过谁」——记原始 IP（用户已定不哈希），所以这张表
	// 是隐私敏感的，导出/备份时按 bans.txt 同等待遇对待。
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_tag_votes_ip ON tag_votes(ip);`); err != nil {
		return fmt.Errorf("创建 tag_votes ip 索引失败: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS tag_weight_base (
		username   TEXT    NOT NULL,
		tag        TEXT    NOT NULL,
		base       INTEGER NOT NULL,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY (username, tag)
	) WITHOUT ROWID;`); err != nil {
		return fmt.Errorf("创建 tag_weight_base 失败: %v", err)
	}
	return nil
}

// BackfillVoteBase 给"从没被票管过"的 account_tags 行快照历史底数（幂等，可重复执行）。
//
// ⚠️ 调用时机：必须在**任何**权重回填之后。根包的 CreateTableV2 里 account_tags 会先从
// 旧 user_tags JSON 回填 2.3 万行，若底数快照排在它之前，那些行就永远没有底数
// （直到下次启动）。所以本函数不由 EnsureSchema 自动调用，而是由两层在各自回填
// 完成之后显式调用一次；写侧另有逐行兜底（见 CastVotes 里的 baseGuardSQL），
// 两者之间出现的新行也不会漏。
//
// 已有底数的行不动（底数不可变），已有票的行也不动（它的权重已经是票算出来的结果）。
func BackfillVoteBase(db *sql.DB) (int64, error) {
	if db == nil {
		return 0, ErrNoStore
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT OR IGNORE INTO tag_weight_base (username, tag, base)
		SELECT a.username, a.tag, a.weight FROM account_tags a
		WHERE NOT EXISTS (SELECT 1 FROM tag_votes v
		                  WHERE v.username = a.username AND v.tag = a.tag)`)
	if err != nil {
		return 0, fmt.Errorf("快照历史底数失败: %v", err)
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if n > 0 {
		log.Printf("tags: 历史底数快照完成，%d 行（此后不可变，作为 weight 的起点）", n)
	}
	return n, nil
}

// baseGuardSQL：某行第一次被票碰到时，把当时的 weight 记成底数。
// 已有底数或已有票 → WHERE 不成立，什么都不插。这是 BackfillVoteBase 的逐行兜底，
// 让"快照之后才出现的行"（新标签、或底数迁移与首次投票之间的窗口）也不会漏底数。
const baseGuardSQL = `INSERT OR IGNORE INTO tag_weight_base (username, tag, base)
	SELECT ?, ?, IFNULL((SELECT weight FROM account_tags WHERE username = ? AND tag = ?), 0)
	WHERE NOT EXISTS (SELECT 1 FROM tag_votes WHERE username = ? AND tag = ?)`

// rollupSQL：把该行权重**重算**为 底数 + Σ票（不是累加）。
// 归零的行由 sweepZeroSQL 删掉，与既有语义一致。
const rollupSQL = `INSERT INTO account_tags (username, tag, weight)
	SELECT ?, ?, IFNULL((SELECT base FROM tag_weight_base WHERE username = ? AND tag = ?), 0)
	             + IFNULL((SELECT SUM(value) FROM tag_votes WHERE username = ? AND tag = ?), 0)
	ON CONFLICT (username, tag) DO UPDATE SET weight = excluded.weight`

// CastVotes 按「目标值」给 username 的一组标签记票：targets 的 value 是**该 IP 想要的值**
// （+1 投、-1 减、0 撤），不是本次变化量。
//
// 幂等性：同一 IP 对同一 (账号,标签) 连投 100 次同向，第一次之后账本值不变、
// 权重不变（所以不依赖任何浏览器本地状态，换浏览器/无痕也只是重复投同一张票）。
// 反向改票（+1 → -1）落库差值是 ±2 —— 这是**故意的**：旧实现把输入夹到 ±1，
// 于是"从减分改成加分"会被夹成 0 分归零；改成目标值语义后这个死结自然消失。
// 夹取的位置因此从"夹输入"移到"夹目标值"（见 clampTarget）。
//
// 单个请求里多个标签共享一个事务：要么全记要么全不记。
func (s *Store) CastVotes(username, ip string, targets map[string]int, ua string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("tags: store 未初始化")
	}
	if username == "" || len(targets) == 0 {
		return nil
	}
	if ip == "" {
		return ErrNoPrincipal
	}

	// 1. 流水照记（每个写请求一行）。tags 字段记的是**目标值**，与旧数据里
	//    "本次变化量"的形态不同——同名列两种语义，靠本注释与本行区分。
	if raw, err := json.Marshal(targets); err == nil {
		if _, err := s.db.Exec(`INSERT INTO request_logs (username, tags, ip, ua) VALUES (?, ?, ?, ?)`,
			username, string(raw), ip, ua); err != nil {
			log.Printf("Warning: 记录日志失败: %v", err)
		}
	}

	// 2. 事务内记票 + 重算滚动值。
	//    这个事务必须以 BEGIN IMMEDIATE 起步：它先读 Σ 再写回滚动值，
	//    两个不同 IP 并发写同一 (账号,标签) 时若以 deferred 起步，双方都可能先拿到
	//    读快照、各自算出旧 Σ、后写的一方覆盖先写的一方 —— 票就丢了。
	//    IMMEDIATE 在第一条语句前就取写锁，把写入整体串行。
	//    ⚠️ 由**连接串**保证：`_txlock=immediate`（modernc 驱动把它拼成 BEGIN IMMEDIATE），
	//    两处打开 twitter.db 的地方（server/main.go 与 gallery 的 openTagStore）都已带上；
	//    另需 `_pragma=busy_timeout(…)`，否则撞锁直接失败而不是排队等待。
	//    测试里自建连接也必须带这两个参数，否则测的是另一条路径。
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for tag, want := range targets {
		if tag == "" {
			continue
		}
		target := clampTarget(want)

		if target == VoteUndo {
			// 撤票 = 删行（不置 0）。理由：置 0 会让账本永远长出一堆无意义的行，
			// 而 Σ 加 0 与不加一样，删行不丢任何信息；该 IP 之后仍可重新投（是新行）。
			res, err := tx.Exec(`DELETE FROM tag_votes WHERE username = ? AND tag = ? AND ip = ?`,
				username, tag, ip)
			if err != nil {
				return fmt.Errorf("撤票 %s %q 失败: %v", username, tag, err)
			}
			if n, _ := res.RowsAffected(); n == 0 {
				continue // 这个 IP 本来就没投过：无事发生，连底数行都不该被顺带造出来
			}
		} else {
			if _, err := tx.Exec(baseGuardSQL, username, tag, username, tag, username, tag); err != nil {
				return fmt.Errorf("快照底数 %s %q 失败: %v", username, tag, err)
			}
			// WHERE 让"重复投同一张票"在库层面什么都不写（值没变就不动 updated_at，
			// 免得连点 100 次把时间戳刷成噪声）。
			if _, err := tx.Exec(`INSERT INTO tag_votes (username, tag, ip, value) VALUES (?, ?, ?, ?)
				ON CONFLICT (username, tag, ip) DO UPDATE
				SET value = excluded.value, updated_at = CURRENT_TIMESTAMP
				WHERE tag_votes.value IS NOT excluded.value`,
				username, tag, ip, target); err != nil {
				return fmt.Errorf("记票 %s %q=%d 失败: %v", username, tag, target, err)
			}
		}

		if _, err := tx.Exec(rollupSQL, username, tag, username, tag, username, tag); err != nil {
			return fmt.Errorf("重算权重 %s %q 失败: %v", username, tag, err)
		}
	}
	// 恰好归零删行（既有语义）；票行**保留**——账本才是真源，删了账本这个 IP
	// 就再也投不了这个标签，且 weight 再也无法从底数重算出来。
	if _, err := tx.Exec(`DELETE FROM account_tags WHERE username = ? AND weight = 0`, username); err != nil {
		return fmt.Errorf("清扫归零标签失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// 缓存失效放在**提交之后**：提交前失效会让并发读把尚未提交的旧值重新填进缓存，
	// 那次写入就永远看不见了。提交后失效只保证"新值可见"，不保证"读到的必是新值"
	// （另一个读者可能已在事务外抢了一次查询），这与"最终一致 + TTL 兜底"一致。
	InvalidateCloud(s.db)
	return nil
}

// clampTarget 把目标值夹进 {-1,0,+1}。注意夹的是**目标值**，不是请求带来的差值，
// 所以施加到 weight 上的实际变化量可以是 ±2。
func clampTarget(v int) int {
	switch {
	case v > VoteUp:
		return VoteUp
	case v < VoteDown:
		return VoteDown
	default:
		return v
	}
}

// Violation 是一行不自洽的记录（校验用）。
type Violation struct {
	Username string
	Tag      string
	Weight   int
	Base     int
	Votes    int
}

// InvariantCheckSQL 是可以直接粘进 sqlite3 跑的一致性校验：
// 返回**被票系统管过**（有底数行或有票行）却不自洽的 account_tags 行，期望 0 行。
const InvariantCheckSQL = `SELECT a.username, a.tag, a.weight,
       IFNULL(b.base, 0) AS base, IFNULL(v.s, 0) AS votes
FROM account_tags a
LEFT JOIN tag_weight_base b ON b.username = a.username AND b.tag = a.tag
LEFT JOIN (SELECT username, tag, SUM(value) s FROM tag_votes GROUP BY username, tag) v
       ON v.username = a.username AND v.tag = a.tag
WHERE (b.username IS NOT NULL OR v.username IS NOT NULL)
  AND a.weight <> IFNULL(b.base, 0) + IFNULL(v.s, 0);`

// OrphanVotesSQL 查反向不一致：有票、按恒等式应当有权重行，account_tags 里却没有。
const OrphanVotesSQL = `SELECT k.username, k.tag, IFNULL(b.base, 0) + IFNULL(k.s, 0) AS should_be
FROM (SELECT username, tag, SUM(value) s FROM tag_votes GROUP BY username, tag) k
LEFT JOIN tag_weight_base b ON b.username = k.username AND b.tag = k.tag
LEFT JOIN account_tags a ON a.username = k.username AND a.tag = k.tag
WHERE IFNULL(b.base, 0) + IFNULL(k.s, 0) <> 0 AND a.username IS NULL;`

// UnsnapshottedCountSQL 查还没快照底数的历史行（不是错误，是"下次启动会补上"；
// 只要它不为 0，就说明底数迁移没跑完或有人绕过写路径直接改了库）。
const UnsnapshottedCountSQL = `SELECT COUNT(*) FROM account_tags a
WHERE NOT EXISTS (SELECT 1 FROM tag_weight_base b WHERE b.username = a.username AND b.tag = a.tag)
  AND NOT EXISTS (SELECT 1 FROM tag_votes v WHERE v.username = a.username AND v.tag = a.tag);`

// InvariantViolations 在库内跑上面两条校验，返回违例行（两类的 Username/Tag 都填好，
// Weight 位放"实际值或应有值"）。调用方只要看 len()==0 判断健康。
func (s *Store) InvariantViolations() ([]Violation, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoStore
	}
	var out []Violation
	scan := func(q string, label string) error {
		rows, err := s.db.Query(q)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var v Violation
			if label == "rollup" {
				if err := rows.Scan(&v.Username, &v.Tag, &v.Weight, &v.Base, &v.Votes); err != nil {
					return err
				}
			} else {
				var want int
				if err := rows.Scan(&v.Username, &v.Tag, &want); err != nil {
					return err
				}
				v.Weight = want
			}
			out = append(out, v)
		}
		return rows.Err()
	}
	if err := scan(InvariantCheckSQL, "rollup"); err != nil {
		return nil, err
	}
	if err := scan(OrphanVotesSQL, "orphan"); err != nil {
		return nil, err
	}
	return out, nil
}

// VotesFor 返回某账号某标签的全部票（审计/排障用；读路径不用它）。
func (s *Store) VotesFor(username, tag string) ([]Vote, error) {
	if s == nil || s.db == nil {
		return nil, ErrNoStore
	}
	rows, err := s.db.Query(`SELECT username, tag, ip, value FROM tag_votes
		WHERE username = ? AND tag = ? ORDER BY ip`, username, tag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Vote
	for rows.Next() {
		var v Vote
		if err := rows.Scan(&v.Username, &v.Tag, &v.IP, &v.Value); err == nil {
			out = append(out, v)
		}
	}
	return out, rows.Err()
}
