// 2026.02.15
// 这里是emoji的新逻辑。
// 作为评分互动模块
// 策略：启动时计算全量数据 -> 压缩写入单一JSON文件 -> 查询时读取文件。

package twitter

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
)

// 假设 DB 是全局数据库连接
// var DB *sql.DB

// ================= 数据结构定义 =================

// RankItem 排名条目
type RankItem struct {
	Username string `json:"username"`
	Votes    int    `json:"votes"`
}

// EmojiRankData 单个Emoji的时间维度数据
type EmojiRankData struct {
	Day   []RankItem `json:"day"`
	Week  []RankItem `json:"week"`
	Month []RankItem `json:"month"`
}

// AllRankData 全量排名数据
// Key 是 emoji，Value 是该 emoji 的多维度排名
type AllRankData map[string]EmojiRankData

// ================= 文件常量 =================

const RankFileJSON = "emoji_ranks.json"
const RankFileGz = ".emoji_ranks.json.gz"

// ================= 数据库初始化 =================

func CreateTableV3() error {
	// 1. 确保之前的表结构存在
	if err := CreateTableV2(); err != nil {
		return err
	}

	// 2. 创建数据库表
	queries := []string{
		`CREATE TABLE IF NOT EXISTS emoji_counts (
            username TEXT,
            emoji TEXT,
            counts INTEGER DEFAULT 0,
            PRIMARY KEY (username, emoji)
        );`,

		`CREATE TABLE IF NOT EXISTS emoji_logs (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            username TEXT,
            emoji TEXT,
            created_at DATETIME DEFAULT CURRENT_TIMESTAMP
        );`,
		`CREATE INDEX IF NOT EXISTS idx_logs_time ON emoji_logs(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_logs_emoji ON emoji_logs(emoji);`,
	}

	for _, q := range queries {
		if _, err := DB.Exec(q); err != nil {
			return fmt.Errorf("初始化表结构失败: %v", err)
		}
	}

	// 3. 启动时执行数据处理
	fmt.Println("[Emoji] 正在进行每日榜单数据结算...")
	if err := RefreshAllRankings(); err != nil {
		fmt.Printf("[Emoji] 错误：榜单结算失败 -> %v\n", err)
	} else {
		fmt.Println("[Emoji] 榜单结算完成，已生成压缩文件。")
	}

	return nil
}

// ================= 投票逻辑 =================

func VoteUpEmoji(username string, emoji string) error {
	tx, err := DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// 更新总量
	if _, err := tx.Exec(`
        INSERT INTO emoji_counts (username, emoji, counts) 
        VALUES (?, ?, 1) 
        ON CONFLICT(username, emoji) 
        DO UPDATE SET counts = counts + 1;`, username, emoji); err != nil {
		return err
	}

	// 写入流水
	if _, err := tx.Exec(`INSERT INTO emoji_logs (username, emoji) VALUES (?, ?);`, username, emoji); err != nil {
		return err
	}

	return tx.Commit()
}

// ================= 榜单计算与持久化 =================

// RefreshAllRankings 计算并生成压缩 JSON 文件
func RefreshAllRankings() error {
	// 1. 从数据库计算数据
	// 返回 map[emoji][]RankItem
	dayMap, err := calculateRankMap("-1 day")
	if err != nil {
		return err
	}
	weekMap, err := calculateRankMap("-7 days")
	if err != nil {
		return err
	}
	monthMap, err := calculateRankMap("-30 days")
	if err != nil {
		return err
	}

	// 2. 合并数据到 AllRankData 结构
	allData := make(AllRankData)

	// 收集所有出现过的 emoji
	allEmojis := make(map[string]bool)
	for k := range dayMap {
		allEmojis[k] = true
	}
	for k := range weekMap {
		allEmojis[k] = true
	}
	for k := range monthMap {
		allEmojis[k] = true
	}

	// 填充结构
	for emoji := range allEmojis {
		data := EmojiRankData{
			Day:   dayMap[emoji],
			Week:  weekMap[emoji],
			Month: monthMap[emoji],
		}
		// 处理空切片，使其在 JSON 中显示为 [] 而不是 null
		if data.Day == nil {
			data.Day = []RankItem{}
		}
		if data.Week == nil {
			data.Week = []RankItem{}
		}
		if data.Month == nil {
			data.Month = []RankItem{}
		}

		allData[emoji] = data
	}

	// 3. 写入 Gzip 压缩文件
	// if err := saveGzFile(RankFileGz, allData); err != nil {
	// 	return err
	// }
	if err := saveJSONFile(RankFileJSON, allData); err != nil {
		return err
	}

	// 4. 清理旧流水
	DB.Exec(`DELETE FROM emoji_logs WHERE created_at < datetime('now', 'localtime', '-31 days')`)

	return nil
}

// calculateRankMap 辅助函数：统计指定时间范围的数据
func calculateRankMap(timeRange string) (map[string][]RankItem, error) {
	result := make(map[string][]RankItem)
	query := `
        SELECT emoji, username, COUNT(*) as votes
        FROM emoji_logs
        WHERE created_at >= datetime('now', 'localtime', ?)
        GROUP BY emoji, username
        ORDER BY votes DESC;
    `

	rows, err := DB.Query(query, timeRange)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var emoji, username string
		var votes int
		if err := rows.Scan(&emoji, &username, &votes); err != nil {
			return nil, err
		}
		result[emoji] = append(result[emoji], RankItem{Username: username, Votes: votes})
	}
	return result, nil
}

// saveGzFile 将数据 Gzip 压缩后写入文件
func saveGzFile(filename string, data AllRankData) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	// 使用 BestCompression 最大化压缩文本数据
	gzWriter, err := gzip.NewWriterLevel(file, gzip.BestCompression)
	if err != nil {
		return err
	}
	defer gzWriter.Close()

	encoder := json.NewEncoder(gzWriter)
	// 不缩进，进一步减小体积
	return encoder.Encode(data)
}

// saveJSONFile 将数据 Gzip 压缩后写入文件
func saveJSONFile(filename string, data AllRankData) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	return encoder.Encode(data)
}

// ================= 查询接口 (读取压缩文件) =================

type RankType string

const (
	RankDay   RankType = "day"
	RankWeek  RankType = "week"
	RankMonth RankType = "month"
)

// GetEmojiRank 获取指定 Emoji 的排名
// 逻辑：打开 Gzip 文件 -> 解压 -> 解析 JSON -> 返回对应数据
func GetEmojiRank(emoji string, rankType RankType, limit int) ([]RankItem, error) {
	// 1. 打开文件
	file, err := os.Open(RankFileGz)
	if err != nil {
		if os.IsNotExist(err) {
			return []RankItem{}, nil // 文件不存在视为无数据
		}
		return nil, err
	}
	defer file.Close()

	// 2. Gzip 解压读取
	gzReader, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	defer gzReader.Close()

	// 3. 解析 JSON
	var allData AllRankData
	decoder := json.NewDecoder(gzReader)
	if err := decoder.Decode(&allData); err != nil {
		return nil, err
	}

	// 4. 提取数据
	emojiData, exists := allData[emoji]
	if !exists {
		return []RankItem{}, nil
	}

	var list []RankItem
	switch rankType {
	case RankDay:
		list = emojiData.Day
	case RankWeek:
		list = emojiData.Week
	case RankMonth:
		list = emojiData.Month
	default:
		return nil, fmt.Errorf("未知榜单类型: %s", rankType)
	}

	// 5. 截取数量
	if limit > 0 && len(list) > limit {
		return list[:limit], nil
	}

	return list, nil
}

// GetUserEmojis 获取用户总榜 (保留原功能)
func GetUserEmojis(username string) (map[string]int, error) {
	emojis := make(map[string]int)
	query := `SELECT emoji, counts FROM emoji_counts WHERE username = ?`
	rows, err := DB.Query(query, username)
	if err != nil {
		return nil, fmt.Errorf("查询用户表情失败: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var emoji string
		var count int
		if err := rows.Scan(&emoji, &count); err != nil {
			return nil, err
		}
		emojis[emoji] = count
	}
	return emojis, nil
}
