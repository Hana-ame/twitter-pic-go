// Package twitter 提供根模块（github.com/Hana-ame/twitter-pic-go）向 api 子包的向后兼容转发层。
//
// 架构重构后，项目核心分为两个对等的子包：
//   - api/     (github.com/Hana-ame/twitter-pic-go/api)：后端 V1/V2 REST API 与统一路由挂载
//   - gallery/ (github.com/Hana-ame/twitter-pic-go/gallery)：Web 图站 SSR 与前端资源系统
//
// 本文件保留根包的全部导出符号别名，确保外部代码或旧引用无需改动即可无缝编译。
package twitter

import (
	"database/sql"

	"github.com/Hana-ame/twitter-pic-go/api"
	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/Hana-ame/twitter-pic-go/tags"
	"github.com/gin-gonic/gin"
)

type (
	User                = api.User
	FlutterUser         = api.FlutterUser
	TagUsersResponse    = api.TagUsersResponse
	UsersListResponse   = api.UsersListResponse
	BatchUsersResponse  = api.BatchUsersResponse
	SearchUsersResponse = api.SearchUsersResponse
	TagCountItem        = api.TagCountItem
	TagCloudResponse    = api.TagCloudResponse
	V2MediaItem         = api.V2MediaItem
	UserMediaResponse   = api.UserMediaResponse
	FeedResponse        = api.FeedResponse
	UserProfileResponse  = api.UserProfileResponse
	V2AccountInfo        = api.V2AccountInfo
	UserTimelineResponse = api.UserTimelineResponse
	RankingV2Response    = api.RankingV2Response
	VoteTagRequest      = api.VoteTagRequest
	VoteTagResponse     = api.VoteTagResponse
	RankItem            = api.RankItem
	EmojiRankData       = api.EmojiRankData
	AllRankData         = api.AllRankData
	RankType            = api.RankType
	PostTagRequest      = api.PostTagRequest
)

const (
	RankFileJSON = api.RankFileJSON
	RankFileGz   = api.RankFileGz
	RankDay      = api.RankDay
	RankWeek     = api.RankWeek
	RankMonth    = api.RankMonth
)

// DB 与 api.DB 保持双向同步（建议新代码直接使用 api.DB 或 api.SetupRouter(db)）。
var DB *sql.DB

func syncDB() {
	if DB != nil && api.DB != DB {
		api.DB = DB
	} else if DB == nil && api.DB != nil {
		DB = api.DB
	}
}

func SetupRouter(db *sql.DB) *gin.Engine {
	if db != nil {
		DB = db
		api.DB = db
	}
	return api.SetupRouter(api.DB)
}

func CreateTable() error {
	syncDB()
	return api.CreateTable()
}

func CreateTableV2() error {
	syncDB()
	return api.CreateTableV2()
}

func CreateTableV3() error {
	syncDB()
	return api.CreateTableV3()
}

func RefreshAllRankings() error {
	syncDB()
	return api.RefreshAllRankings()
}

func Store() *tags.Store {
	syncDB()
	return api.Store()
}

func AddToGroup(g *gin.RouterGroup) {
	syncDB()
	api.AddToGroup(g)
}

func RegisterV2Routes(g *gin.RouterGroup) {
	syncDB()
	api.RegisterV2Routes(g)
}

func StrictIPBanMiddleware(m *ipban.Manager) gin.HandlerFunc {
	return api.StrictIPBanMiddleware(m)
}
