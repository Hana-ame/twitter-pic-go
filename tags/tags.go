// Package tags 是账号标签（account_tags 表）的**唯一实现**。
//
// 背景：twitter-pic 有两层对外 API——根包的 twitter REST API
// （/api/twitter/tags/:username 读、POST /api/twitter/:username 写）与
// gallery 图站（/api/account-tags 读、POST /api/account-tag 写）。
// 两层历史上各有一套存储（user_tags JSON 大字段 / gallery 侧的投票 JSON 文件
// + 独立的 tags.db 快照），语义与数据源都不一致。
//
// 本包把读写收敛到**同一张 account_tags 表**（单一 twitter.db）：
//   - 写：Add 在事务内按行累加权重（恰好归零删行、负权重保留），
//     并**保留 request_logs 流水**（每个写请求一行，含 ip/ua）；
//   - 读：Weights / ForUsers / UsersForTag / Cloud / LastModifyFor。
//
// 两层都只调这里，保证「两边做成一样」。
package tags

import (
	"database/sql"
	"encoding/json"
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

// Add 记录一条请求流水，并在事务内把 input 的权重**累加**进 account_tags。
// 语义（两层一致）：weight 累加；累加后恰好为 0 的行删除；负权重保留。
func (s *Store) Add(username string, input map[string]int, ip, ua string) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("tags: store 未初始化")
	}
	if username == "" || len(input) == 0 {
		return nil
	}

	// 1. 请求流水（保留：每次写请求一行）
	if raw, err := json.Marshal(input); err == nil {
		if _, err := s.db.Exec(`INSERT INTO request_logs (username, tags, ip, ua) VALUES (?, ?, ?, ?)`,
			username, string(raw), ip, ua); err != nil {
			log.Printf("Warning: 记录日志失败: %v", err)
		}
	}

	// 2. 事务内逐标签 upsert 累加，最后清扫归零行
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for tag, delta := range input {
		if tag == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO account_tags (username, tag, weight) VALUES (?, ?, ?)
			ON CONFLICT (username, tag) DO UPDATE SET weight = weight + excluded.weight`,
			username, tag, delta); err != nil {
			return fmt.Errorf("更新标签 %s=%d 失败: %v", tag, delta, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM account_tags WHERE username = ? AND weight = 0`, username); err != nil {
		return fmt.Errorf("清扫归零标签失败: %v", err)
	}
	return tx.Commit()
}

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

// Cloud 返回覆盖账号数最多的前 n 个标签（仅正权重）。n<=0 时默认 36。
func (s *Store) Cloud(n int) []Count {
	if s == nil || s.db == nil {
		return nil
	}
	if n <= 0 {
		n = 36
	}
	rows, err := s.db.Query(
		`SELECT tag, COUNT(DISTINCT username) FROM account_tags WHERE weight > 0
		 GROUP BY tag ORDER BY 2 DESC, tag LIMIT ?`, n)
	if err != nil {
		log.Printf("tags: Cloud: %v", err)
		return nil
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
	if err := rows.Err(); err != nil {
		log.Printf("tags: Cloud scan: %v", err)
	}
	return out
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
