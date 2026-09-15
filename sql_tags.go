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
	"strings"
	"time"

	"github.com/Hana-ame/twitter-pic-go/tags"
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
	//    DDL 与两层读写统一由 tags 包提供（唯一真源），此处只调它。
	//    POST 直接按行 upsert 权重，GET 由 userSelectQuery 现场聚合，
	//    不再读-改-写 JSON 大字段；tag 上建索引供反查（gallery /api/tag 同构）。
	if err := tags.EnsureSchema(DB); err != nil {
		return err
	}

	// 3b. 旧数据一次性回填：account_tags 为空时从 user_tags 的 JSON 展开。
	if err := migrateAccountTags(); err != nil {
		return err
	}

	// 3c. 历史底数快照（幂等），**必须**排在 3b 之后：account_tags 的行是在 3b 里
	//     从 user_tags 灌进来的，快照若跑在前面，首次升级的那 2.3 万行就永远没有
	//     底数、weight = 底数 + Σ票 对它们不成立。写侧另有逐行兜底，但只有这里
	//     能把既有历史一次性纳入审计范围。
	if _, err := tags.BackfillVoteBase(DB); err != nil {
		return err
	}

	// 4. 请求日志表 request_logs 已由 tags.EnsureSchema 建好（DDL 只此一份，
	//    gallery 先启动也不会漏建）。

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

// Store 是标签唯一真源的访问器（account_tags 表 + request_logs 流水）。
// 根 API 与 gallery 走同一套语义与同一个数据源，保证「两边做成一样」。
func Store() *tags.Store { return tags.New(DB) }

// addTag 是根 API（App 入口）的写路径：委托 tags.Store.CastVotes。
//
// inputMap 的 value 现在是**该 IP 的目标值**（+1/-1/0=撤票），不再是变化量——
// 与图站 POST /api/account-tag 的 d 同一个语义，两个入口都"一 IP 一票"。
// 这里保留原有的"归一化到 ±1"（比图站宽：图站对越界直接 400），因为本接口一次收
// 一组标签的批量体，为一个越界值把整批退回去对 App 不友好；归一化不影响
// "同一 IP 重复提交同值不落库"这个幂等结论。
// 0 的旧含义是"忽略这个标签"（从 map 里删掉），新含义是"撤掉这个 IP 的票"。
func addTag(username string, inputMap map[string]int, ip, ua string) error {
	return Store().CastVotes(username, ip, inputMap, ua)
}

// searchLimit 与其他 by 分支（username/nick 都写死 LIMIT 15）保持一致。
const searchLimit = 15

// getUserListByTag 「tag 查 user」：先走 tags 包的同一条反查路径
// （account_tags + idx_account_tags_tag，**权重降序**，只取正权重），
// 再把命中的 username 水合成与其他搜索一致的 []User。
//
// 与 username/nick 两个分支的差异（调用方需要知道）：
//   - 排序按标签权重降序，不是 last_modify DESC；
//   - 精确匹配标签，不是 LIKE 子串。
func getUserListByTag(tag string) ([]User, error) {
	names := Store().UsersForTag(tag, nil, searchLimit)
	if len(names) == 0 {
		return []User{}, nil
	}

	ph := strings.TrimSuffix(strings.Repeat("?,", len(names)), ",")
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	rows, err := DB.Query(userSelectQuery+` WHERE u.status = 'SUCCESS' AND u.username IN (`+ph+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("按标签取用户失败: %v", err)
	}
	defer rows.Close()

	byName := make(map[string]User, len(names))
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		byName[u.Username] = u
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("按标签取用户扫描失败: %v", err)
	}

	// 按反查回来的权重顺序输出（SQL 的 IN 不保证顺序）。
	out := make([]User, 0, len(names))
	for _, n := range names {
		if u, ok := byName[n]; ok {
			out = append(out, u)
		}
	}
	return out, nil
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
