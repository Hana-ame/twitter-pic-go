// 2026.02.15
// 这里是emoji的新逻辑。
// 作为评分互动模块

package twitter

import "fmt"

func CreateTableV3() error {
	// 1. 确保之前的表结构存在 (基于 V2)
	if err := CreateTableV2(); err != nil {
		return err
	}

	// 2. 创建表情计数表
	// username 和 emoji 组合作为联合主键 (Prime Key)
	// counts 用于存储计数
	queryEmoji := `CREATE TABLE IF NOT EXISTS emoji_counts (
        username TEXT,
        emoji TEXT,
        counts INTEGER DEFAULT 0,
        PRIMARY KEY (username, emoji)
    );`
	if _, err := DB.Exec(queryEmoji); err != nil {
		return fmt.Errorf("创建 emoji_counts 失败: %v", err)
	}

	return nil
}

// GetUserEmojis 根据 username 读取各个 emoji 以及对应计数
// 返回一个 map[string]int，key 是 emoji，value 是计数
func GetUserEmojis(username string) (map[string]int, error) {
	// 初始化返回的 map
	emojis := make(map[string]int)

	// 查询指定用户的所有 emoji 记录
	query := `SELECT emoji, counts FROM emoji_counts WHERE username = ?`
	rows, err := DB.Query(query, username)
	if err != nil {
		return nil, fmt.Errorf("查询用户表情失败: %v", err)
	}
	defer rows.Close()

	// 遍历结果集
	for rows.Next() {
		var emoji string
		var count int
		if err := rows.Scan(&emoji, &count); err != nil {
			return nil, fmt.Errorf("扫描数据失败: %v", err)
		}
		emojis[emoji] = count
	}

	// 检查遍历过程中是否发生错误
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历数据失败: %v", err)
	}

	return emojis, nil
}

// VoteUpEmoji 为指定用户的某个 emoji 计数 +1
// 如果该用户从未有过此 emoji，则创建新记录并设为 1
func VoteUpEmoji(username string, emoji string) error {
	// 使用 UPSERT 语法：
	// 1. 尝试插入
	// 2. 如果主键冲突说明记录已存在，则执行更新
	query := `
    INSERT INTO emoji_counts (username, emoji, counts) 
    VALUES (?, ?, 1) 
    ON CONFLICT(username, emoji) 
    DO UPDATE SET counts = counts + 1;
    `

	if _, err := DB.Exec(query, username, emoji); err != nil {
		return fmt.Errorf("投票失败: %v", err)
	}

	return nil
}
