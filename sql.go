package twitter

import (
	"database/sql"
	"fmt"
	"log"
	"time"
)

var DB *sql.DB

func CreateTable() error {

	// 确保数据库连接有效
	if err := DB.Ping(); err != nil {
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

	if _, err := DB.Exec(queryTable); err != nil {
		return fmt.Errorf("创建表失败: %v", err)
	}

	// 2. 创建复合索引
	// idx_users_status_modify 是索引名称
	// (status, last_modify DESC) 匹配你的查询逻辑：等值过滤 status，倒序排列 last_modify
	queryIndex := `CREATE INDEX IF NOT EXISTS idx_users_status_modify 
                   ON users (status, last_modify DESC);`

	if _, err := DB.Exec(queryIndex); err != nil {
		return fmt.Errorf("创建复合索引失败: %v", err)
	}

	log.Println("表和复合索引创建/检查完成")
	return nil
}

// Base query string to avoid repetition
// 2026.09.15：tags 改从规范表 account_tags 聚合（json_group_object 现场序列化，
// 空标签返回 NULL → COALESCE '{}'），不再读旧的 user_tags JSON 大字段。
// 全部 GET 路径（getUserTags/getUserList/邻居列表等）共用本查询，一处切换。
//
// `weight != 0` 不是可选的：它必须与共享包 tags.Store.Weights 的过滤口径逐字一致，
// 否则同一账号在根 API 与 gallery 两层会给出不同标签集（外部 sqlite3 运维写入或
// 历史脏数据一旦出现 weight=0 行就会暴露）。归零删行是写侧（tags.Add）的责任，
// 读侧这里只是不再依赖它一定发生过。负权重两边都保留。
const userSelectQuery = `
	SELECT u.username, u.last_modify, u.status,
	       COALESCE((SELECT json_group_object(a.tag, a.weight)
	                 FROM account_tags a
	                 WHERE a.username = u.username AND a.weight != 0), '{}')
	FROM users u
`

// 做个delete方法就行了。
func commitUser(username, status string) error {
	query := `UPDATE users 
          SET status = ?, 
              last_modify = CURRENT_TIMESTAMP 
          WHERE username = ?`

	_, err := DB.Exec(query, status, username)
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

	rows, err := DB.Query(query)
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

	err := DB.QueryRow("SELECT last_modify FROM users WHERE username = ?", username).Scan(&lastModify)
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

	rows, err := DB.Query(query, lastModify, lastModify, username)
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
	// 注意：查询条件已经排除了 after 用户本身（last_modify < ? OR (last_modify = ? AND username < ?)），
	// 所以这里不需要再跳过第一条。此前用 users[1:] 会导致每页静默丢一个用户，
	// 且最后一页只剩 1 条时返回空数组，前端（LoadMoreButton append 模式）误判 noMore。
	// 发现背景：review 代码 + 阅读前端 ~/twitter-pic-react/src/components/LoadMoreButton.jsx 后确认。
	return users, nil
}

func getUserListByNick(nick string) ([]User, error) {
	query := userSelectQuery + `
		WHERE u.nick LIKE ? AND u.status = 'SUCCESS'
		ORDER BY u.last_modify DESC
		LIMIT 15`

	rows, err := DB.Query(query, "%"+nick+"%")
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

	rows, err := DB.Query(query, "%"+username+"%")
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

	rows, err := DB.Query(query, username)
	if err != nil {
		return user, fmt.Errorf("查询用户失败: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		return scanUser(rows)
	}
	return user, fmt.Errorf("查询用户失败: 没有进入 rows.Next()")
}
