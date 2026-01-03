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
	if _, err := db.Exec(queryTags); err != nil {
		return fmt.Errorf("创建 user_tags 失败: %v", err)
	}

	// 3. 创建请求日志表
	queryLogs := `CREATE TABLE IF NOT EXISTS request_logs (
        id INTEGER PRIMARY KEY AUTOINCREMENT,
		username TEXT,
		tags TEXT,
        ip TEXT,
        ua TEXT,
        created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );`
	if _, err := db.Exec(queryLogs); err != nil {
		return fmt.Errorf("创建 request_logs 失败: %v", err)
	}

	return nil

}

func addTag(username string, inputMap map[string]int, ip, ua string) error {
	// 1. 记录请求流水

	input, err := json.Marshal(inputMap)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO request_logs (username, tags, ip, ua) VALUES (?, ?, ?, ?)`,
		username, string(input), ip, ua)
	if err != nil {
		log.Printf("Warning: 记录日志失败: %v", err)
	}

	// 2. 开启事务
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 3. 获取现有的权重数据
	var oldJSON string
	err = tx.QueryRow(`SELECT tags FROM user_tags WHERE username = ?`, username).Scan(&oldJSON)

	currentWeights := make(map[string]int)
	if err == nil && oldJSON != "" && oldJSON != "{}" {
		json.Unmarshal([]byte(oldJSON), &currentWeights)
	}

	// 4. 核心逻辑：带权重的合并
	for key, delta := range inputMap {
		// 检查该标签在数据库中是否已存在
		currentWeights[key] = currentWeights[key] + delta
		if currentWeights[key] == 0 {
			delete(currentWeights, key)
		}
	}

	// 5. 如果合并后 map 为空，存 {} 或者是更精简的处理
	var finalJSON string
	if len(currentWeights) == 0 {
		finalJSON = "{}"
	} else {
		newJSONBytes, _ := json.Marshal(currentWeights)
		finalJSON = string(newJSONBytes)
	}

	// 6. 写入数据库
	_, err = tx.Exec(`INSERT OR REPLACE INTO user_tags (username, tags, last_modify) 
                    VALUES (?, ?, CURRENT_TIMESTAMP)`, username, finalJSON)

	return tx.Commit()
}

// Helper function to scan rows into a User struct
func scanUser(rows *sql.Rows) (User, error) {
	var u User
	var tagsRaw string

	// We select: u.username, u.last_modify, t.tags
	err := rows.Scan(&u.Username, &u.LastModify, &tagsRaw)
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
