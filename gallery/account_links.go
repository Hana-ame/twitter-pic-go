package gallery

import (
	"database/sql"
	"log"
	"strings"
)

// 平台定义：key → (显示名, 图标 SVG pathData, 域名匹配)
type platform struct {
	Name   string
	Domain string // 用于自动识别 URL 属于哪个平台
	Icon   string // 16x16 SVG pathData（Simple Icons 风格）
}

// 已知平台图标（SVG pathData， viewBox 0 0 24 24）
var platforms = map[string]platform{
	"twitter": {
		Name:   "X / Twitter",
		Domain: "x.com",
		Icon:   "M18.244 2.25h3.308l-7.227 8.26 8.502 11.24H16.17l-5.214-6.817L4.99 21.75H1.68l7.73-8.835L1.254 2.25H8.08l4.713 6.231zm-1.161 17.52h1.833L7.084 4.126H5.117z",
	},
	"pixiv": {
		Name:   "pixiv",
		Domain: "pixiv.net",
		Icon:   "M7.534 0C3.37 0 0 3.37 0 7.534v8.932C0 20.63 3.37 24 7.534 24h8.932C20.63 24 24 20.63 24 16.466V7.534C24 3.37 20.63 0 16.466 0zm1.334 4.167h4.166c.573 0 1.042.469 1.042 1.042v.625c0 .573-.469 1.042-1.042 1.042h-1.042v2.5h1.042c.573 0 1.042.469 1.042 1.042v.625c0 .573-.469 1.042-1.042 1.042h-1.042v3.75c0 .573-.469 1.042-1.042 1.042h-.625c-.573 0-1.042-.469-1.042-1.042v-9.375c0-.573.469-1.042 1.042-1.042zm-3.75 2.5h3.75c.573 0 1.042.469 1.042 1.042v.625c0 .573-.469 1.042-1.042 1.042H6.25c-.573 0-1.042-.469-1.042-1.042V7.709c0-.573.469-1.042 1.042-1.042zm0 5h3.75c.573 0 1.042.469 1.042 1.042v.625c0 .573-.469 1.042-1.042 1.042H6.25c-.573 0-1.042-.469-1.042-1.042v-.625c0-.573.469-1.042 1.042-1.042zm0 5h3.75c.573 0 1.042.469 1.042 1.042v.625c0 .573-.469 1.042-1.042 1.042H6.25c-.573 0-1.042-.469-1.042-1.042v-.625c0-.573.469-1.042 1.042-1.042z",
	},
	"ci-en": {
		Name:   "CI-en",
		Domain: "ci-en.dlsite.com",
		Icon:   "M12 0C5.373 0 0 5.373 0 12s5.373 12 12 12 12-5.373 12-12S18.627 0 12 0zm0 2c5.523 0 10 4.477 10 10s-4.477 10-10 10S2 17.523 2 12 6.477 2 12 2zm-1 5v10l8-5z",
	},
	"bilibili": {
		Name:   "bilibili",
		Domain: "bilibili.com",
		Icon:   "M17.813 4.653h.854c1.51.054 2.769.578 3.773 1.574 1.004.995 1.524 2.249 1.56 3.76v7.36c-.036 1.51-.556 2.769-1.56 3.773s-2.262 1.524-3.773 1.56H5.333c-1.51-.036-2.769-.556-3.773-1.56S.036 18.858 0 17.347v-7.36c.036-1.511.556-2.765 1.56-3.76 1.004-.996 2.262-1.52 3.773-1.574h.774l-1.174-1.12a1.234 1.234 0 0 1-.373-.906c0-.356.124-.658.373-.907l.027-.027c.267-.249.573-.373.92-.373.347 0 .653.124.92.373L9.653 4.44c.071.071.134.142.187.213h4.267a.836.836 0 0 1 .16-.213l2.853-2.747c.267-.249.573-.373.92-.373.347 0 .662.151.929.4.267.249.391.551.391.907 0 .355-.124.657-.373.906zM5.333 7.24c-.746.018-1.373.276-1.88.773-.506.498-.769 1.13-.786 1.894v7.52c.017.764.28 1.395.786 1.893.507.498 1.134.756 1.88.773h13.334c.746-.017 1.373-.275 1.88-.773.506-.498.769-1.129.786-1.893v-7.52c-.017-.765-.28-1.396-.786-1.894-.507-.497-1.134-.755-1.88-.773zM8 11.107c.373 0 .684.124.933.373.25.249.383.569.4.96v1.173c-.017.391-.15.711-.4.96-.249.25-.56.374-.933.374s-.684-.125-.933-.374c-.25-.249-.383-.569-.4-.96V12.44c.017-.391.15-.711.4-.96.249-.249.56-.373.933-.373zm8 0c.373 0 .684.124.933.373.25.249.383.569.4.96v1.173c-.017.391-.15.711-.4.96-.249.25-.56.374-.933.374s-.684-.125-.933-.374c-.25-.249-.383-.569-.4-.96V12.44c.017-.391.15-.711.4-.96.249-.249.56-.373.933-.373z",
	},
	"fanbox": {
		Name:   "FANBOX",
		Domain: "fanbox.cc",
		Icon:   "M0 16.02V7.98C0 7.437.448 7 1 7h3.98c.543 0 .99.448.99 1v8.04c0 .543-.447.99-.99.99H1c-.552 0-1-.447-1-.99zm6.03-6.02V7.98c0-.543.448-.99.99-.99H11c.543 0 .99.447.99.99v2.02c0 .543-.447.99-.99.99H7.02c-.543 0-.99-.447-.99-.99zM12.01 7.98v8.04c0 .543.448.99.99.99H17c.543 0 .99-.447.99-.99V7.98c0-.543-.447-.99-.99-.99h-4.02c-.54 0-.987.447-.987.99z",
	},
	"fantia": {
		Name:   "Fantia",
		Domain: "fantia.jp",
		Icon:   "M12 0C5.373 0 0 5.373 0 12s5.373 12 12 12 12-5.373 12-12S18.627 0 12 0zm4.5 16.5h-3v-3h3v3zm0-4.5h-3v-3h3v3zm0-4.5h-3V4.5h3V7.5z",
	},
	"douyin": {
		Name:   "抖音",
		Domain: "douyin.com",
		Icon:   "M16.6 5.82s.51.5 0 0A4.278 4.278 0 0 1 15.54 3h-3.09v12.4a2.592 2.592 0 0 1-2.59 2.5c-1.42 0-2.6-1.16-2.6-2.6 0-1.72 1.66-3.01 3.37-2.48V9.66c-3.45-.46-6.47 2.22-6.47 5.64 0 3.33 2.76 5.7 5.69 5.7 3.14 0 5.69-2.55 5.69-5.7V9.01a7.35 7.35 0 0 0 4.3 1.38V7.3s-1.88.09-3.24-1.48z",
	},
	"youtube": {
		Name:   "YouTube",
		Domain: "youtube.com",
		Icon:   "M23.498 6.186a3.016 3.016 0 0 0-2.122-2.136C19.505 3.545 12 3.545 12 3.545s-7.505 0-9.377.505A3.017 3.017 0 0 0 .502 6.186C0 8.07 0 12 0 12s0 3.93.502 5.814a3.016 3.016 0 0 0 2.122 2.136c1.871.505 9.376.505 9.376.505s7.505 0 9.377-.505a3.015 3.015 0 0 0 2.122-2.136C24 15.93 24 12 24 12s0-3.93-.502-5.814zM9.545 15.568V8.432L15.818 12l-6.273 3.568z",
	},
	"github": {
		Name:   "GitHub",
		Domain: "github.com",
		Icon:   "M12 .297c-6.63 0-12 5.373-12 12 0 5.303 3.438 9.8 8.205 11.385.6.113.82-.258.82-.577 0-.285-.01-1.04-.015-2.04-3.338.724-4.042-1.61-4.042-1.61C4.422 18.07 3.633 17.7 3.633 17.7c-1.087-.744.084-.729.084-.729 1.205.084 1.838 1.236 1.838 1.236 1.07 1.835 2.809 1.305 3.495.998.108-.776.417-1.305.76-1.605-2.665-.3-5.466-1.332-5.466-5.93 0-1.31.465-2.38 1.235-3.22-.135-.303-.54-1.523.105-3.176 0 0 1.005-.322 3.3 1.23.96-.267 1.98-.399 3-.405 1.02.006 2.04.138 3 .405 2.28-1.552 3.285-1.23 3.285-1.23.645 1.653.24 2.873.12 3.176.765.84 1.23 1.91 1.23 3.22 0 4.61-2.805 5.625-5.475 5.92.42.36.81 1.096.81 2.22 0 1.606-.015 2.896-.015 3.286 0 .315.21.69.825.57C20.565 22.092 24 17.592 24 12.297c0-6.627-5.373-12-12-12",
	},
	"booth": {
		Name:   "BOOTH",
		Domain: "booth.pm",
		Icon:   "M13.958 10.09c0 1.232.029 2.256-.591 3.351-.502.891-1.301 1.44-2.186 1.44-1.214 0-1.922-.924-1.922-2.292 0-2.692 2.415-3.182 4.7-3.182v.683zm3.186 7.705a.659.659 0 0 1-.749.075c-1.053-.876-1.242-1.283-1.818-2.12-1.738 1.772-2.969 2.302-5.218 2.302-2.666 0-4.744-1.645-4.744-4.94 0-2.572 1.394-4.322 3.379-5.178 1.716-.754 4.11-.891 5.943-1.095v-.41c0-.753.058-1.642-.385-2.294-.384-.579-1.124-.82-1.775-.82-1.205 0-2.277.618-2.54 1.897-.054.285-.261.566-.549.58l-3.065-.33c-.259-.058-.546-.266-.472-.66C5.771 1.3 8.92 0 11.08 0c1.075 0 2.49.28 3.33 1.082 1.083 1.025.979 2.389.979 3.905v3.539c0 1.047.434 1.512.836 2.083.143.205.173.454-.009.606-.456.386-1.268 1.108-1.707 1.514l-.002-.003-.398.288z",
	},
	" Skeb": {
		Name:   "Skeb",
		Domain: "skeb.jp",
		Icon:   "M12 0C5.373 0 0 5.373 0 12s5.373 12 12 12 12-5.373 12-12S18.627 0 12 0zm0 2c5.523 0 10 4.477 10 10s-4.477 10-10 10S2 17.523 2 12 6.477 2 12 2z",
	},
}

// initAccountLinksSchema 建表：每个账号（dir）可以有多个外部链接。
func initAccountLinksSchema(db *sql.DB) {
	if db == nil {
		return
	}
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS account_links (
		id       INTEGER PRIMARY KEY AUTOINCREMENT,
		account  TEXT NOT NULL,
		platform TEXT NOT NULL,
		url      TEXT NOT NULL,
		UNIQUE(account, platform)
	)`)
	if err != nil {
		log.Printf("gallery: init account_links schema failed: %v", err)
	}
}

// AccountLink 表示一个外部平台链接。
type AccountLink struct {
	ID       int64  `json:"id"`
	Account  string `json:"account"`
	Platform string `json:"platform"`
	URL      string `json:"url"`
}

// loadAccountLinks 从 DB 加载所有账号链接，返回 account → []AccountLink。
func loadAccountLinks(db *sql.DB) map[string][]AccountLink {
	out := map[string][]AccountLink{}
	if db == nil {
		return out
	}
	rows, err := db.Query(`SELECT id, account, platform, url FROM account_links ORDER BY account, id`)
	if err != nil {
		log.Printf("gallery: load account_links failed: %v", err)
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var l AccountLink
		if err := rows.Scan(&l.ID, &l.Account, &l.Platform, &l.URL); err != nil {
			continue
		}
		out[l.Account] = append(out[l.Account], l)
	}
	return out
}

// upsertAccountLink 插入或更新某个账号的某个平台链接。
func upsertAccountLink(db *sql.DB, account, platform, url string) error {
	_, err := db.Exec(`INSERT INTO account_links (account, platform, url) VALUES (?, ?, ?)
		ON CONFLICT(account, platform) DO UPDATE SET url = excluded.url`,
		account, platform, url)
	return err
}

// deleteAccountLink 删除某个账号的某个平台链接。
func deleteAccountLink(db *sql.DB, account, platform string) error {
	_, err := db.Exec(`DELETE FROM account_links WHERE account = ? AND platform = ?`, account, platform)
	return err
}

// resolvePlatform 根据输入（平台名或 URL）返回标准化的 platform key。
func resolvePlatform(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	// 直接匹配平台名
	if _, ok := platforms[input]; ok {
		return input
	}
	// 从 URL 域名匹配
	for key, p := range platforms {
		if p.Domain != "" && strings.Contains(input, p.Domain) {
			return key
		}
	}
	// 未知平台：用输入作为 key
	return input
}

// platformIcon 返回某个平台的 SVG <path> 内容，未知平台返回通用图标。
func platformIcon(key string) string {
	if p, ok := platforms[key]; ok {
		return p.Icon
	}
	// 通用链接图标
	return "M3.9 12c0-1.71 1.39-3.1 3.1-3.1h4V7H7c-2.76 0-5 2.24-5 5s2.24 5 5 5h4v-1.9H7c-1.71 0-3.1-1.39-3.1-3.1zM8 13h8v-2H8v2zm9-6h-4v1.9h4c1.71 0 3.1 1.39 3.1 3.1s-1.39 3.1-3.1 3.1h-4V17h4c2.76 0 5-2.24 5-5s-2.24-5-5-5z"
}

// platformName 返回平台显示名，未知平台用 key 本身。
func platformName(key string) string {
	if p, ok := platforms[key]; ok {
		return p.Name
	}
	return key
}
