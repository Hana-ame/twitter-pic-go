package twitter

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"github.com/Hana-ame/twitter-pic-go/Tools/sqlite"
)

var db, _ = sqlite.NewSQLiteDB("./twitter.db")

func CreateTable() error {
	// 确保数据库连接有效
	if err := db.Ping(); err != nil {
		return fmt.Errorf("数据库连接不可用: %v", err)
	}

	// 补全 SQL 语句（添加 last_modify 字段的类型和默认值）
	query := `CREATE TABLE IF NOT EXISTS users (
        username TEXT PRIMARY KEY,
        nick TEXT,
		status TEXT,
        last_modify TIMESTAMP DEFAULT CURRENT_TIMESTAMP
    );`

	// 执行 SQL 并检查错误
	_, err := db.Exec(query)
	if err != nil {
		return fmt.Errorf("创建表失败: %v", err)
	}

	log.Println("表创建或已存在检查完成")
	return nil
}

// 其实是python这边在做。
// 将内容插入表中/更新（UPSERT操作）
func commitUser(username, nick, status string) error {
	// 使用INSERT OR REPLACE实现UPSERT（插入或更新）
	query := `INSERT OR REPLACE INTO users (username, nick, status, last_modify)
              VALUES (?, ?, ?, CURRENT_TIMESTAMP)`

	_, err := db.Exec(query, username, nick, status)
	if err != nil {
		return fmt.Errorf("插入/更新用户失败: %v", err)
	}

	log.Printf("用户 %s 已成功提交", username)
	return nil
}

// 按照时间倒序得到最新修改的10个status为success的用户名
func getUserList() ([]string, error) {
	query := `SELECT username FROM users 
              WHERE status = 'SUCCESS' 
              ORDER BY last_modify DESC 
              LIMIT 10`

	rows, err := db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("查询用户列表失败: %v", err)
	}
	defer rows.Close()

	var userList []string = make([]string, 0)
	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			return nil, fmt.Errorf("读取数据失败: %v", err)
		}
		userList = append(userList, username)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历行时发生错误: %v", err)
	}

	return userList, nil
}

// 按照时间倒序得到某个用户名之后的10个用户名
func getUserListAfter(username string) ([]string, error) {
	// 先获取指定用户的last_modify时间
	var targetTime string
	query := "SELECT last_modify FROM users WHERE username = ?"
	err := db.QueryRow(query, username).Scan(&targetTime)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("用户不存在: %s", username)
		}
		return nil, fmt.Errorf("查询用户时间失败: %v", err)
	}
	parsedTime, _ := time.Parse("2006-01-02T15:04:05Z", targetTime) // 如果精确到秒且带Z

	// 按照时间戳寻找之后的10个用户
	query = `SELECT username FROM users 
             WHERE last_modify < ? AND status = 'SUCCESS' 
             ORDER BY last_modify DESC 
	  		 LIMIT 10`

	rows, err := db.Query(query, parsedTime.Format("2006-01-02 15:04:05"))
	if err != nil {
		return nil, fmt.Errorf("查询后续用户失败: %v", err)
	}
	defer rows.Close()

	var userList []string = make([]string, 0)
	for rows.Next() {
		var nextUser string
		if err := rows.Scan(&nextUser); err != nil {
			return nil, fmt.Errorf("读取数据失败: %v", err)
		}
		userList = append(userList, nextUser)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历行时发生错误: %v", err)
	}

	return userList, nil
}

// 通过nick查找，先查找"nick%"，再查找"%nick%"
func getUserListByNick(nick string) ([]string, error) {
	// 先查找以nick开头的用户
	// query := `SELECT username FROM users
	//           WHERE nick LIKE ? AND status = 'SUCCESS'
	//           ORDER BY last_modify DESC`

	// rows, err := db.Query(query, nick+"%")
	// if err != nil {
	// 	return nil, fmt.Errorf("查询用户失败: %v", err)
	// }
	// defer rows.Close()

	var userList []string = make([]string, 0)
	// for rows.Next() {
	// 	var username string
	// 	if err := rows.Scan(&username); err != nil {
	// 		return nil, fmt.Errorf("读取数据失败: %v", err)
	// 	}
	// 	userList = append(userList, username)
	// }

	// if err := rows.Err(); err != nil {
	// 	return nil, fmt.Errorf("遍历行时发生错误: %v", err)
	// }

	// // 如果没有找到完全匹配的，再查找包含nick的用户
	// if len(userList) == 0 {
	query := `SELECT username FROM users 
                 WHERE nick LIKE ? AND status = 'SUCCESS'
                 ORDER BY last_modify DESC
				 LIMIT 10`

	rows, err := db.Query(query, "%"+nick+"%")
	if err != nil {
		return nil, fmt.Errorf("模糊查询用户失败: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			return nil, fmt.Errorf("读取数据失败: %v", err)
		}
		userList = append(userList, username)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历行时发生错误: %v", err)
	}
	// }

	return userList, nil
}

// 通过nick查找，先查找"nick%"，再查找"%nick%"
func getUserListByUsername(username string) ([]string, error) {

	query := `SELECT username FROM users 
	WHERE username LIKE ? AND status = 'SUCCESS'
	ORDER BY last_modify DESC
	LIMIT 10`

	rows, err := db.Query(query, "%"+username+"%")
	if err != nil {
		return nil, fmt.Errorf("模糊查询用户失败: %v", err)
	}
	defer rows.Close()

	var userList []string = make([]string, 0)

	for rows.Next() {
		var username string
		if err := rows.Scan(&username); err != nil {
			return nil, fmt.Errorf("读取数据失败: %v", err)
		}
		userList = append(userList, username)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历行时发生错误: %v", err)
	}

	return userList, nil
}
