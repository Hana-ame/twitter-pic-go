// 2026.01.01
// 这里是tag的新逻辑。
// 主要实现所有的旧函数返回username和last_modify
// 并且在另一张表中找到tag返回

// TBD：
// 每次请求都需要被记录
// TBD：
// 每次请求都合并到属于username这个key的tags当中
// 以json string格式。

package twitter

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"time"
)

// 分类：
// 主体：男性，女性，男女性交，二次元，其他。
// 类别性质：商业AV，自拍，原创，合集收集，AI
// 露出度：不露，露逼，露屌，露奶，露脸
// 审查：有马，AI去马，无马
// 其他tag：男娘，女装，COS，Lolita，露出，白幼瘦，白虎，大奶，贫乳，

type User struct {
	Username   string         `json:"username"`
	LastModify time.Time      `json:"last_modify"`
	Tags       map[string]int `json:"tags"`
	Status     string         `json:"status"`
}

func CreateTableV2() error {
	// 1. 创建基础用户表
	if err := CreateTable(); err != nil {
		return err
	}

	// 2. 创建独立标签表
	// 使用 username 作为主键，确保一个用户只有一行标签记录
	queryTags := `CREATE TABLE IF NOT EXISTS user_tags (
        username TEXT PRIMARY KEY,
        tags TEXT DEFAULT '{}',
        last_modify TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
        FOREIGN KEY(username) REFERENCES users(username)
    );`
	if _, err := DB.Exec(queryTags); err != nil {
		return fmt.Errorf("创建 user_tags 失败: %v", err)
	}

	// 3. 创建规范标签表 account_tags：一行一个 (username, tag)。
	//    POST 直接按行 upsert 权重，GET 由 userSelectQuery 现场聚合，
	//    不再读-改-写 JSON 大字段；tag 上建索引供反查（gallery /api/tag 同构）。
	queryAccountTags := `CREATE TABLE IF NOT EXISTS account_tags (
        username TEXT NOT NULL,
        tag      TEXT NOT NULL,
        weight   INTEGER NOT NULL DEFAULT 1,
        PRIMARY KEY (username, tag)
    ) WITHOUT ROWID;`
	if _, err := DB.Exec(queryAccountTags); err != nil {
		return fmt.Errorf("创建 account_tags 失败: %v", err)
	}
	if _, err := DB.Exec(`CREATE INDEX IF NOT EXISTS idx_account_tags_tag ON account_tags(tag);`); err != nil {
		return fmt.Errorf("创建 account_tags 标签索引失败: %v", err)
	}

	// 3b. 旧数据一次性回填：account_tags 为空时从 user_tags 的 JSON 展开。
	if err := migrateAccountTags(); err != nil {
		return err
	}

	// 4. 创建请求日志表
	queryLogs := `CREATE TABLE IF NOT EXISTS request_logs (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT,
		tags TEXT,
        ip TEXT,
        ua TEXT,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );`
	if _, err := DB.Exec(queryLogs); err != nil {
		return fmt.Errorf("创建 request_logs 失败: %v", err)
	}

	return nil

}

// migrateAccountTags 把旧 user_tags 的 JSON 权重对象一次性展开进 account_tags。
// 仅在 account_tags 为空（首次升级）时执行；json_each 属 JSON1，modernc 驱动内置。
func migrateAccountTags() error {
	var n int
	if err := DB.QueryRow(`SELECT COUNT(*) FROM account_tags`).Scan(&n); err != nil {
		return fmt.Errorf("检查 account_tags: %v", err)
	}
	if n > 0 {
		return nil
	}
	res, err := DB.Exec(`
		INSERT OR IGNORE INTO account_tags (username, tag, weight)
		SELECT u.username, j.key, CAST(j.value AS INTEGER)
		FROM user_tags u,
		     json_each(CASE WHEN u.tags = '' OR NOT json_valid(u.tags) THEN '{}' ELSE u.tags END) j
		WHERE CAST(j.value AS INTEGER) != 0`)
	if err != nil {
		return fmt.Errorf("回填 account_tags 失败: %v", err)
	}
	if c, _ := res.RowsAffected(); c > 0 {
		log.Printf("account_tags 回填完成：%d 行（来自 user_tags JSON）", c)
	}
	return nil
}

// addTag POST 路径：按行 upsert 进 account_tags（不再读-改-写 JSON）。
// 语义与旧版一致：权重累加，恰好归零则删除该标签行（负权重保留）。
func addTag(username string, inputMap map[string]int, ip, ua string) error {
	// 1. 记录请求流水

	input, err := json.Marshal(inputMap)
	if err != nil {
		return err
	}
	_, err = DB.Exec(`INSERT INTO request_logs (username, tags, ip, ua) VALUES (?, ?, ?, ?)`,
		username, string(input), ip, ua)
	if err != nil {
		log.Printf("Warning: 记录日志失败: %v", err)
	}

	// 2. 事务内逐标签 upsert 累加，最后清扫归零行
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	for key, delta := range inputMap {
		if _, err := tx.Exec(`INSERT INTO account_tags (username, tag, weight) VALUES (?, ?, ?)
			ON CONFLICT (username, tag) DO UPDATE SET weight = weight + excluded.weight`,
			username, key, delta); err != nil {
			return fmt.Errorf("更新标签 %s=%d 失败: %v", key, delta, err)
		}
	}
	if _, err := tx.Exec(`DELETE FROM account_tags WHERE username = ? AND weight = 0`, username); err != nil {
		return fmt.Errorf("清扫归零标签失败: %v", err)
	}

	return tx.Commit()
}

// Helper function to scan rows into a User struct
func scanUser(rows *sql.Rows) (User, error) {
	var u User
	var tagsRaw string

	// We select: u.username, u.last_modify, u.status t.tags
	err := rows.Scan(&u.Username, &u.LastModify, &u.Status, &tagsRaw)
	if err != nil {
		return u, err
	}

	// Unmarshal the JSON string from DB into the map
	// If tagsRaw is empty or '[]' (per your schema default), it handles it
	u.Tags = make(map[string]int)
	if tagsRaw != "" && tagsRaw != "{}" {
		if err := json.Unmarshal([]byte(tagsRaw), &u.Tags); err != nil {
			log.Printf("Warning: failed to unmarshal tags for user %s: %v", u.Username, err)
			// Non-critical error, continue with empty map
		}
	}
	return u, nil
}
