package gallery

import (
	"database/sql"
	"log"
)

// 平坦（无分级）的 per-image 标签体系，与账号级 tag（user_tags）完全分离。
// 参考 ../twitter-pic-react 的标签思路：服务端只存扁平 tag，
// 「高亮/屏蔽」是前端 viewer 的本地偏好，不进服务端模型。

const (
	// 赞/倒赞 作为第一类 per-image 标签（普通扁平 tag，无特殊分级）。
	ReactionLike    = "👍"
	ReactionDislike = "👎"
)

// initMediaTagSchema 建表。tag 体系完全扁平：tags 只是名字字典，
// media_tags 是 media_id -> tag 的多对多关联（带 voter 以便聚合计数）。
func initMediaTagSchema(db *sql.DB) {
	if db == nil {
		return
	}
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS tags (
			id         INTEGER PRIMARY KEY,
			name       TEXT UNIQUE NOT NULL,
			post_count INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS media_tags (
			media_id  TEXT NOT NULL,
			tag_id    INTEGER NOT NULL,
			voter     TEXT NOT NULL DEFAULT '',
			added_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (media_id, tag_id, voter),
			FOREIGN KEY (tag_id) REFERENCES tags(id)
		)`,
		`CREATE TABLE IF NOT EXISTS tag_aliases (
			alias   TEXT UNIQUE NOT NULL,
			tag_id  INTEGER NOT NULL,
			FOREIGN KEY (tag_id) REFERENCES tags(id)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			log.Printf("gallery: init media tag schema failed: %v", err)
			return
		}
	}
}

// ensureTag 返回（或创建）某个扁平 tag 的 id。
func ensureTag(db *sql.DB, name string) (int64, error) {
	var id int64
	err := db.QueryRow(`SELECT id FROM tags WHERE name = ?`, name).Scan(&id)
	if err == sql.ErrNoRows {
		res, e := db.Exec(`INSERT INTO tags (name) VALUES (?)`, name)
		if e != nil {
			return 0, e
		}
		return res.LastInsertId()
	}
	if err != nil {
		return 0, err
	}
	return id, nil
}

// React 设置/取消某个 media 的 emoji 反应（扁平 tag）。
// 同一 voter 只能有一个反应：点同款=取消，点另一款=切换。
// 返回该 media 最新的赞/倒赞聚合计数。
func React(db *sql.DB, mediaID, emoji, voter string) (likes, dislikes int, err error) {
	tagID, err := ensureTag(db, emoji)
	if err != nil {
		return 0, 0, err
	}

	// 该 voter 当前已有的反应
	var cur int64
	_ = db.QueryRow(`SELECT tag_id FROM media_tags WHERE media_id=? AND voter=? LIMIT 1`, mediaID, voter).Scan(&cur)

	// 清掉该 voter 在此 media 上的所有反应
	if _, e := db.Exec(`DELETE FROM media_tags WHERE media_id=? AND voter=?`, mediaID, voter); e != nil {
		return 0, 0, e
	}
	// 若之前不是同款，则写入新反应（否则即取消）
	if cur != tagID {
		if _, e := db.Exec(`INSERT OR IGNORE INTO media_tags (media_id, tag_id, voter) VALUES (?,?,?)`, mediaID, tagID, voter); e != nil {
			return 0, 0, e
		}
	}

	likes, dislikes, err = reactionCounts(db, mediaID)
	return likes, dislikes, err
}

// reactionCounts 返回某个 media 的赞/倒赞聚合计数（跨所有 voter）。
func reactionCounts(db *sql.DB, mediaID string) (likes, dislikes int, err error) {
	likeID, _ := ensureTag(db, ReactionLike)
	dislikeID, _ := ensureTag(db, ReactionDislike)
	row := db.QueryRow(`
		SELECT
			COALESCE(SUM(CASE WHEN tag_id=? THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN tag_id=? THEN 1 ELSE 0 END), 0)
		FROM media_tags WHERE media_id=?`, likeID, dislikeID, mediaID)
	err = row.Scan(&likes, &dislikes)
	return likes, dislikes, err
}

// AttachReactions 把持久化的 per-image 标签（含赞/倒赞计数）回填到内存索引。
// 必须在 Scan() 之后、对外服务之前调用；重新扫描后也需再调用一次。
func (g *Gallery) AttachReactions(db *sql.DB) {
	if db == nil {
		return
	}
	rows, err := db.Query(`SELECT m.media_id, t.name FROM media_tags m JOIN tags t ON t.id = m.tag_id`)
	if err != nil {
		return
	}
	defer rows.Close()

	type agg struct {
		likes, dislikes int
		tags            []string
	}
	acc := map[string]*agg{}
	for rows.Next() {
		var mid, name string
		if err := rows.Scan(&mid, &name); err != nil {
			continue
		}
		a := acc[mid]
		if a == nil {
			a = &agg{}
			acc[mid] = a
		}
		switch name {
		case ReactionLike:
			a.likes++
		case ReactionDislike:
			a.dislikes++
		default:
			a.tags = append(a.tags, name)
		}
	}

	for mid, a := range acc {
		if m, ok := g.byID[mid]; ok {
			m.LikeCount = a.likes
			m.DislikeCount = a.dislikes
			m.PerImageTags = a.tags
		}
	}
}
