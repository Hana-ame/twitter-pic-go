package sqlite

import (
	"database/sql"
	"strings"

	_ "modernc.org/sqlite"
)

// NewSQLiteDB 使用纯 Go 的 modernc.org/sqlite 驱动打开 SQLite 数据库连接并校验可用性。
// 内置到仓库中以消除外部 Git Submodule 依赖，保证 CGO_ENABLED=0 开箱即编译。
func NewSQLiteDB(dsn string) (*sql.DB, error) {
	cleanDSN := strings.TrimSpace(dsn)
	if cleanDSN != "" && !strings.HasPrefix(cleanDSN, "file:") && !strings.HasPrefix(cleanDSN, ":memory:") {
		cleanDSN = "file:" + cleanDSN
	}
	db, err := sql.Open("sqlite", cleanDSN)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}
