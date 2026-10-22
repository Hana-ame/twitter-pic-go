package api

import (
	"time"
)

// ============================================================================
// 【新版 API 数据模型 (V2 Models)】
// 适用对象：
//   - twitter-pic-react 新分支 (v2 branch)
//   - twitter-pic-flutter 客户端
//   - Gallery 现代 Web 前端
//
// 核心设计：
//   1. 彻底消除 N+1 请求：FlutterUser 单次携带 username、nick、avatar、banner、total_urls、tags、last_modify。
//   2. 视频封面 + 直接代理链接（2026-10-23 起广告跳转已关闭）：V2MediaItem 为所有视频条目直接提供
//      - cover: pbs.moonchan.xyz 上的封面图地址（如 https://pbs.moonchan.xyz/ext_tw_video_thumb/...?format=jpg&name=orig）
//      - ouo_url: 历史契约字段名，值为本后端视频端点 /api/twitter/v2/video/<path>?<query>，
//        点击后服务端 302 直连 https://twimg.moonchan.xyz/...（不再经 ouo.io 广告页）
//   3. 专为 Cloudflare 边缘缓存与浏览器本地缓存（localStorage + HTTP 304）设计。
// ============================================================================

type FlutterUser struct {
	Username string `json:"username"`
	Nick     string `json:"nick"`
	Avatar   string `json:"avatar"`
	// Banner 是资料背景图（pbs.twimg.com/profile_banners/…）；没有时为空。
	Banner     string         `json:"banner,omitempty"`
	TotalUrls  int            `json:"total_urls"`
	PhotoCount int            `json:"photo_count,omitempty"`
	VideoCount int            `json:"video_count,omitempty"`
	Tags       map[string]int `json:"tags"`
	LastModify time.Time      `json:"last_modify"`
	Status     string         `json:"status"`
	JsonGzURL  string         `json:"json_gz_url,omitempty"` // 指向该用户的全量静态缓存文件（带时间戳）
}

// TagUsersResponse 是按标签分页查询用户的统一响应结构。
type TagUsersResponse struct {
	Tag     string        `json:"tag"`
	Total   int           `json:"total"`
	Count   int           `json:"count"`
	Page    int           `json:"page"`
	Limit   int           `json:"limit"`
	Offset  int           `json:"offset"`
	HasMore bool          `json:"has_more"`
	Users   []FlutterUser `json:"users"`
}

// UsersListResponse 是全局用户列表分页响应结构。
type UsersListResponse struct {
	Total      int           `json:"total,omitempty"`
	Count      int           `json:"count"`
	Page       int           `json:"page,omitempty"`
	Limit      int           `json:"limit"`
	Offset     int           `json:"offset,omitempty"`
	HasMore    bool          `json:"has_more"`
	NextCursor string        `json:"next_cursor,omitempty"`
	Users      []FlutterUser `json:"users"`
}

// BatchUsersResponse 是批量获取用户卡片信息的响应结构。
type BatchUsersResponse struct {
	Count  int                    `json:"count"`
	Users  []FlutterUser          `json:"users"`
	ByName map[string]FlutterUser `json:"by_name"`
}

// SearchUsersResponse 是搜索用户接口的统一响应结构。
type SearchUsersResponse struct {
	Query   string        `json:"query"`
	By      string        `json:"by"`
	Total   int           `json:"total"`
	Count   int           `json:"count"`
	Page    int           `json:"page,omitempty"`
	Limit   int           `json:"limit"`
	HasMore bool          `json:"has_more"`
	Users   []FlutterUser `json:"users"`
}

// TagCountItem 是标签云条目。
type TagCountItem struct {
	Tag   string `json:"tag"`
	Count int    `json:"count"`
}

// TagCloudResponse 是标签云响应结构。
type TagCloudResponse struct {
	Total int            `json:"total"`
	Tags  []TagCountItem `json:"tags"`
}

// V2MediaItem 是单个媒体条目（图片或视频）。
type V2MediaItem struct {
	TweetID int64  `json:"tweet_id"`
	URL     string `json:"url"`
	Type    string `json:"type"` // "photo", "video", "animated_gif", "card", "article:cover"
	Date    string `json:"date"`
	// Cover 是这条媒体的封面/缩略图：视频与 GIF 取 gallery_dl 给出的 poster 图并重写到 pbs.moonchan.xyz
	// （例如 https://pbs.moonchan.xyz/ext_tw_video_thumb/1916894180884062208/pu/img/X472TmC88Oy-kvGn?format=jpg&name=orig）。
	Cover string `json:"cover,omitempty"`
	// OuoURL 是点开视频时的请求地址（JSON 字段名沿用历史契约 ouo_url）：
	// 值为本后端视频端点 /api/twitter/v2/video/<path>?<query>，点击后由服务端 302 直连
	// https://twimg.moonchan.xyz/...（2026-10-23 起广告跳转已关闭，不再经 ouo.io）。
	OuoURL string `json:"ouo_url,omitempty"`
}

// UserMediaResponse 是用户媒体分页查询响应。
type UserMediaResponse struct {
	Username   string        `json:"username"`
	Total      int           `json:"total"`
	Count      int           `json:"count"`
	Page       int           `json:"page"`
	Limit      int           `json:"limit"`
	Offset     int           `json:"offset"`
	NextCursor int           `json:"next_cursor,omitempty"`
	HasMore    bool          `json:"has_more"`
	Filter     string        `json:"filter"`                // "all", "photo", "video"
	JsonGzURL  string        `json:"json_gz_url,omitempty"` // 该用户全量静态缓存文件 URL
	Media      []V2MediaItem `json:"media"`
}

// V2AccountInfo 兼容 twitter-pic-react 的 account_info 结构。
type V2AccountInfo struct {
	Name          string `json:"name"`
	Nick          string `json:"nick"`
	ProfileImage  string `json:"profile_image"`
	ProfileBanner string `json:"profile_banner,omitempty"`
}

// UserTimelineResponse 是面向新版 twitter-pic-react 的单用户完整视图响应：
// 一次请求返回 account_info、tags、emojis 与经过 cover + ouo_url（直连视频端点）增强的 timeline，
// 支持基于 ?t=<last_modify> 的 Cloudflare 边缘强缓存与浏览器本地缓存。
type UserTimelineResponse struct {
	Username      string         `json:"username"`
	AccountInfo   V2AccountInfo  `json:"account_info"`
	TotalUrls     int            `json:"total_urls"`
	PhotoCount    int            `json:"photo_count"`
	VideoCount    int            `json:"video_count"`
	Tags          map[string]int `json:"tags"`
	Emojis        map[string]int `json:"emojis"`
	LastModify    time.Time      `json:"last_modify"`
	Status        string         `json:"status"`
	Timeline      []V2MediaItem  `json:"timeline"`
}

// FeedResponse 是首页聚合 Feed 响应结构。
type FeedResponse struct {
	TopTags     []TagCountItem `json:"top_tags"`
	RecentUsers []FlutterUser  `json:"recent_users"`
	TotalUsers  int            `json:"total_users"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// UserProfileResponse 是单用户轻量概况统计信息。
type UserProfileResponse struct {
	Username      string         `json:"username"`
	Nick          string         `json:"nick"`
	Avatar        string         `json:"avatar"`
	ProfileBanner string         `json:"profile_banner,omitempty"`
	TotalUrls     int            `json:"total_urls"`
	PhotoCount    int            `json:"photo_count"`
	VideoCount    int            `json:"video_count"`
	Tags          map[string]int `json:"tags"`
	Emojis        map[string]int `json:"emojis,omitempty"`
	LastModify    time.Time      `json:"last_modify"`
	Status        string         `json:"status"`
	JsonGzURL     string         `json:"json_gz_url,omitempty"`
}

// VoteTagRequest 是标签投票入参。
type VoteTagRequest struct {
	User string         `json:"user,omitempty"`
	Tag  string         `json:"tag"`
	D    *int           `json:"d"`
	Tags map[string]int `json:"tags"`
}

// VoteTagResponse 是标签投票成功响应。
type VoteTagResponse struct {
	Username string         `json:"username"`
	Tags     map[string]int `json:"tags"`
	Message  string         `json:"message"`
}

// RankingV2Response 是新版 Emoji 排行榜响应，内嵌所有上榜用户的卡片信息（users），
// 使新版 twitter-pic-react 打开排行榜只需 1 次请求（彻底消除旧版 N+1 拉取 json.gz）。
type RankingV2Response struct {
	Rankings AllRankData            `json:"rankings"`
	Users    map[string]FlutterUser `json:"users"`
}
