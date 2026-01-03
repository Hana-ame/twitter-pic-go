package twitter

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/Hana-ame/twitter-pic-go/Tools/sqlite"
)

var db, _ = sqlite.NewSQLiteDB("./twitter.db?parseTime=true&_loc=UTC")

func CreateTable() error {
	// 确保数据库连接有效
	if err := db.Ping(); err != nil {
		return fmt.Errorf("数据库连接不可用: %v", err)
	}

	// 1. 创建表结构
	// 注意：username 已经是 PRIMARY KEY，数据库会自动为它创建索引
	queryTable := `CREATE TABLE IF NOT EXISTS users (
        username TEXT PRIMARY KEY,
        nick TEXT,
		status TEXT,
        last_modify TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );`

	if _, err := db.Exec(queryTable); err != nil {
		return fmt.Errorf("创建表失败: %v", err)
	}

	// 2. 创建复合索引
	// idx_users_status_modify 是索引名称
	// (status, last_modify DESC) 匹配你的查询逻辑：等值过滤 status，倒序排列 last_modify
	queryIndex := `CREATE INDEX IF NOT EXISTS idx_users_status_modify 
                   ON users (status, last_modify DESC);`

	if _, err := db.Exec(queryIndex); err != nil {
		return fmt.Errorf("创建复合索引失败: %v", err)
	}

	log.Println("表和复合索引创建/检查完成")
	return nil
}

// Base query string to avoid repetition
const userSelectQuery = `
	SELECT u.username, u.last_modify, COALESCE(t.tags, '{}')
	FROM users u
	LEFT JOIN user_tags t ON u.username = t.username
`

// 做个delete方法就行了。
func commitUser(username, status string) error {
	query := `UPDATE users 
          SET status = ?, 
              last_modify = CURRENT_TIMESTAMP 
          WHERE username = ?`

	_, err := db.Exec(query, status, username)
	if err != nil {
		return fmt.Errorf("插入/更新用户失败: %v", err)
	}

	log.Printf("用户 %s 已成功提交", username)
	return nil
}

func getUserList() ([]User, error) {
	query := userSelectQuery + `
		WHERE u.status = 'SUCCESS' 
		ORDER BY u.last_modify DESC 
		LIMIT 25`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("查询用户列表失败: %v", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

func getUserListAfter(username string) ([]User, error) {
	// 1. Get the last_modify of the reference user
	var lastModify time.Time

	err := db.QueryRow("SELECT last_modify FROM users WHERE username = ?", username).Scan(&lastModify)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("用户不存在: %s", username)
		}
		return nil, fmt.Errorf("查询用户时间失败: %v", err)
	}

	// 2. Query users older than that time
	// 逻辑是：时间比我早，或者（时间跟我一样，但用户名/ID 比我小）
	query := userSelectQuery + `
    WHERE (u.last_modify < ? OR (u.last_modify = ? AND u.username < ?))
    AND u.status = 'SUCCESS' 
    ORDER BY u.last_modify DESC, u.username DESC 
    LIMIT 25`

	rows, err := db.Query(query, lastModify, lastModify, username)
	if err != nil {
		return nil, fmt.Errorf("查询后续用户失败: %v", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users[1:], nil // 这里会拿到当前这个？
}

func getUserListByNick(nick string) ([]User, error) {
	query := userSelectQuery + `
		WHERE u.nick LIKE ? AND u.status = 'SUCCESS'
		ORDER BY u.last_modify DESC
		LIMIT 15`

	rows, err := db.Query(query, "%"+nick+"%")
	if err != nil {
		return nil, fmt.Errorf("模糊查询用户失败: %v", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

func getUserListByUsername(username string) ([]User, error) {
	query := userSelectQuery + `
		WHERE u.username LIKE ? AND u.status = 'SUCCESS'
		ORDER BY u.last_modify DESC
		LIMIT 15`

	rows, err := db.Query(query, "%"+username+"%")
	if err != nil {
		return nil, fmt.Errorf("模糊查询用户失败: %v", err)
	}
	defer rows.Close()

	var users []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

func getUserTags(username string) (user User, err error) {
	query := userSelectQuery + `WHERE u.username = ?`

	rows, err := db.Query(query, username)
	if err != nil {
		return user, fmt.Errorf("查询用户失败: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		return scanUser(rows)
	}
	return user, fmt.Errorf("查询用户失败: 没有进入 rows.Next()")
}
