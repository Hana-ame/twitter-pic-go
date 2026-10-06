package twitter

import (
	"time"
)

// FlutterUser 是专为 Flutter 及现代客户端设计的丰富用户卡片模型。
// 字段与 Flutter 客户端 models/user.dart 中的 TwitterUser 契约完全一致。
// 客户端只需单次请求即可获取全部渲染所需信息（用户名、昵称、头像、媒体总数、标签权重字典、更新时间），
// 彻底解决旧接口只有裸用户名导致的 N+1 次并发请求、卡死以及幽灵 404 账号问题。
type FlutterUser struct {
	Username   string         `json:"username"`
	Nick       string         `json:"nick"`
	Avatar     string         `json:"avatar"`
	TotalUrls  int            `json:"total_urls"`
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
// 同时提供数组形式与以 username 为 key 的 map 形式，极大方便客户端按需读取。
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
	Type    string `json:"type"` // "photo", "video", "animated_gif"
	Date    string `json:"date"`
}

// UserMediaResponse 是用户媒体分页查询响应。
// 客户端无需再下载几十兆的完整 .json.gz，单次请求仅需几 KB 即可完成首屏媒体瀑布流渲染。
// 同时保留 json_gz_url 字段，客户端仍可用于离线归档或全量缓存。
type UserMediaResponse struct {
	Username   string        `json:"username"`
	Total      int           `json:"total"`
	Count      int           `json:"count"`
	Page       int           `json:"page"`
	Limit      int           `json:"limit"`
	Offset     int           `json:"offset"`
	NextCursor int           `json:"next_cursor,omitempty"`
	HasMore    bool          `json:"has_more"`
	Filter     string        `json:"filter"` // "all", "photo", "video"
	JsonGzURL  string        `json:"json_gz_url,omitempty"` // 该用户全量静态缓存文件 URL
	Media      []V2MediaItem `json:"media"`
}

// FeedResponse 是首页聚合 Feed 响应结构。
// 客户端冷启动时一次请求即可获取热门标签、最新活跃用户与统计，避免多次往返请求。
type FeedResponse struct {
	TopTags     []TagCountItem `json:"top_tags"`
	RecentUsers []FlutterUser  `json:"recent_users"`
	TotalUsers  int            `json:"total_users"`
	UpdatedAt   time.Time      `json:"updated_at"`
}

// UserProfileResponse 是单用户轻量概况统计信息。
// 避免为了展示资料卡头图与媒体计数而传输全部媒体列表。
type UserProfileResponse struct {
	Username   string         `json:"username"`
	Nick       string         `json:"nick"`
	Avatar     string         `json:"avatar"`
	TotalUrls  int            `json:"total_urls"`
	PhotoCount int            `json:"photo_count"`
	VideoCount int            `json:"video_count"`
	Tags       map[string]int `json:"tags"`
	LastModify time.Time      `json:"last_modify"`
	Status     string         `json:"status"`
	JsonGzURL  string         `json:"json_gz_url,omitempty"` // 该用户全量静态缓存文件 URL
}

// VoteTagRequest 是标签投票入参。
// 支持单标签模式（{tag, d}）与批量全量期望态模式（{tags: {女性: 1, 自拍: 0}}）。
type VoteTagRequest struct {
	User string         `json:"user,omitempty"` // 账号名（当未在 URL 路径提供时生效）
	Tag  string         `json:"tag"`            // 单标签名
	D    *int           `json:"d"`              // 目标票值：1 投 / -1 减 / 0 撤票
	Tags map[string]int `json:"tags"`           // 批量全量期望态字典
}

// VoteTagResponse 是标签投票成功响应。
type VoteTagResponse struct {
	Username string         `json:"username"`
	Tags     map[string]int `json:"tags"`
	Message  string         `json:"message"`
}

