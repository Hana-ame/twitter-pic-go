// 2026.01.01
// 似乎缺少了302的逻辑了。
// 之前的修改版似乎是不见了。

package twitter

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/Hana-ame/twitter-pic-go/Tools/ginkit"
	"github.com/Hana-ame/twitter-pic-go/ipban"
	"github.com/Hana-ame/twitter-pic-go/limit"
	"github.com/gin-gonic/gin"
)

// POST /:username
// ?do_not_tag=true 跳过加tag环节，如果存在则更新，如果不存在则跳过
// ?do_not_renew=true 用来添加tag
func CreateMetaData(c *gin.Context) {
	username := c.Param("username")
	if username == "" || username == "undefined" {
		c.JSON(400, gin.H{
			"error": "username is required",
		})
		return
	}
	c.Set("username", username)

	// 2026.01.01
	// 需要检查 body json，是这次添加的tag。
	// 限制请求体 1MB，防止客户端塞大 payload 打爆内存 / 磁盘。
	// Twitter 用户元数据远小于此。
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)

	// 统一 IP 口径：流水里记的是「这个请求是谁」（按可信跳数取），
	// 不再记整个 XFF 头串——原先两层各记各的，反查同 IP 关联账号时对不上。
	ip := ipban.Principal(c.Request)
	agent := c.Request.UserAgent()

	// do_not_tag flag is not exist.
	_, doNotTag := c.GetQuery("do_not_tag")
	_, doNotRenew := c.GetQuery("do_not_renew")
	if !doNotTag && !doNotRenew {
		// 只能在第一次添加的时候用，因为权重不同。
		user, _ := getUserTags(username)
		if len(user.Tags) > 0 { // 已经添加过了，不要这么做。
			c.JSON(200, gin.H{"message": "already has tags, skipped"})
			return
		}

		// 更新其实也算在这里了。还是会被空结构体绕过额。
		o := make(map[string]int)
		if err := json.NewDecoder(c.Request.Body).Decode(&o); err != nil {
			if ginkit.AbortWithError(c, http.StatusBadRequest, err) {
				return
			}
		}
		if len(o) == 0 {
			ginkit.AbortWithError(c, http.StatusBadRequest, fmt.Errorf("你没加tag，这是不行的"))
			return
		}
		// 归一化到目标值 ±1；0 **保留**（新语义=该 IP 撤票，旧语义=忽略该标签）。
		for k, v := range o {
			if v > 0 {
				o[k] = 1
			} else if v < 0 {
				o[k] = -1
			}
		}

		// 写库失败必须报出去：以前吞掉 error 照样回 200 {"message":"ok"}，
		// 客户端无从判断标签到底进没进 account_tags。
		if err := addTag(username, o, ip, agent); err != nil {
			ginkit.AbortWithError(c, http.StatusInternalServerError, err)
			return
		}

		// 抓取排队失败**不改**本请求结论：标签已经写进去了，caller.py 不在
		// 是开发环境的常态，不该让它把一次成功的写入判成失败。但要留痕。
		if msg, err := curlMetaData(username); err != nil {
			log.Printf("curlMetaData(%s) 排队失败: %v (%s)", username, err, msg)
		}

		c.JSON(200, gin.H{"message": "ok"})
		return
	}

	if doNotTag && !doNotRenew {
		// do_not_tag = true;
		// 如果有 do_not_tag 标记，检查是否有记录，如果没有，则直接return
		_, err := getUserTags(username)
		if ginkit.AbortWithError(c, 403, err) {
			return
		}

		if msg, err := curlMetaData(username); err != nil {
			log.Printf("curlMetaData(%s) 排队失败: %v (%s)", username, err, msg)
		}

		c.JSON(200, gin.H{"message": "ok"})
		return
	}

	// 一定是tag的情况
	// 复用于添加 tag ，使用`do_not_renew=true`规避这次 tag 添加
	if doNotRenew && !doNotTag {
		_, err := getUserTags(username)

		if ginkit.AbortWithError(c, 500, err) {
			return
		}

		o := make(map[string]int)
		if err := json.NewDecoder(c.Request.Body).Decode(&o); err != nil {
			if ginkit.AbortWithError(c, http.StatusBadRequest, err) {
				return
			}
		}
		// 归一化到目标值 ±1；0 **保留**（新语义=该 IP 撤票，旧语义=忽略该标签）。
		for k, v := range o {
			if v > 0 {
				o[k] = 1
			} else if v < 0 {
				o[k] = -1
			}
		}
		// 同上：写失败要报 500，不再回假 200。
		if err := addTag(username, o, ip, agent); err != nil {
			ginkit.AbortWithError(c, http.StatusInternalServerError, err)
			return
		}

		c.JSON(200, gin.H{"message": "ok"})
		return
	}

	c.JSON(400, gin.H{"error": "conflicting flags"})
}

// GET /tags/:username
func GetTags(c *gin.Context) {
	username := c.Param("username")
	user, err := getUserTags(username)
	if ginkit.AbortWithError(c, 500, err) {
		return
	}

	c.JSON(200, user)
}

// GET /:fn
func GetMetaData(c *gin.Context) {
	fn := c.Param("fn")

	username := strings.TrimSuffix(fn, ".json.gz")

	user, err := getUserTags(username)
	if ginkit.AbortWithError(c, 404, err) {
		return
	}

	if user.Status != "SUCCESS" {
		c.File("banned.json")
		return
	}

	if _, ok := c.GetQuery("t"); !ok {
		// 用 URL.Path 而非 URL.String()：后者带 query，带参请求会拼成 /foo?x=y.json.gz?t=...
		c.Redirect(302, c.Request.URL.Path+".json.gz?t="+user.LastModify.String())
		return
	}

	if !strings.HasSuffix(fn, ".json.gz") {
		ginkit.AbortWithError(c, 403, fmt.Errorf("not allowed"))
		return
	}

	// 根据fn打开文件返回
	safeFn := filepath.Base(fn)
	if safeFn != fn {
		// 说明 fn 包含路径分隔符，可能是攻击
		ginkit.AbortWithError(c, 403, fmt.Errorf("not allowed"))
		return
	}
	f, err := os.Open(safeFn) // 都放在同一个文件夹。
	if ginkit.AbortWithError(c, 500, err) {
		return
	}

	fileInfo, err := f.Stat()
	if ginkit.AbortWithError(c, 500, err) {
		return
	}

	c.DataFromReader(200, fileInfo.Size(), "application/json", f, map[string]string{"content-encoding": "gzip"})
}

// :fn
func GetLists(c *gin.Context) {
	list, ok := c.GetQuery("list")
	after, _ := c.GetQuery("after")

	if ok {
		r, err := getList(list, after)
		if ginkit.AbortWithError(c, 500, err) {
			return
		}
		c.JSON(200, r)
		return
	}

	search, ok := c.GetQuery("search")
	if ok {
		by, _ := c.GetQuery("by")
		r, err := getSearch(by, search)
		if err != nil {
			// 之前把错误吞掉、nil 也返回 200，会让调用方无法区分「空结果」与「查询失败」。
			ginkit.AbortWithError(c, http.StatusInternalServerError, err)
			return
		}
		c.JSON(http.StatusOK, r)
		return
	}

	c.String(http.StatusNotImplemented, "not implemented")
}

// verifyDeleteKey 校验 ?delete= 参数与 DELETE_KEY 是否匹配。
// DELETE_KEY 未配置时直接返回 500——空 key 会让 c.Query 与 os.Getenv 都是空串，
// 空 == 空 恒真，等价于任意 DELETE/PUT 都能改用户状态，必须先挡掉。
func verifyDeleteKey(c *gin.Context) bool {
	key := os.Getenv("DELETE_KEY")
	if key == "" {
		c.AbortWithStatus(http.StatusInternalServerError)
		return false
	}
	if c.Query("delete") != key {
		c.AbortWithStatus(http.StatusForbidden)
		return false
	}
	return true
}

func DeleteUser(c *gin.Context) {
	if !verifyDeleteKey(c) {
		return
	}
	if err := commitUser(c.Param("username"), "BANNED"); err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "banned", "username": c.Param("username")})
}

func CreateUser(c *gin.Context) {
	if !verifyDeleteKey(c) {
		return
	}
	if err := commitUser(c.Param("username"), "SUCCESS"); err != nil {
		c.AbortWithError(http.StatusInternalServerError, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "unbanned", "username": c.Param("username")})
}

// 26.02.15
// GLM5改的。

// GET /emojis?username={}
// 获取指定用户的所有 Emoji 计数
func GetEmojis(c *gin.Context) {
	username := c.Query("username")

	if username == "" {
		// 假设生成的数据文件名为 emojis.json，位于当前目录下
		filePath := RankFileJSON

		// 检查文件是否存在，提供更好的错误处理（可选但推荐）
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			c.String(http.StatusNotFound, "统计文件尚未生成")
			return
		}

		// 直接返回文件内容
		c.File(filePath)
		return
	}

	// 调用之前实现的数据库方法
	emojis, err := GetUserEmojis(username)
	if ginkit.AbortWithError(c, http.StatusInternalServerError, err) {
		return
	}

	c.JSON(200, emojis)
}

func GetEmojisGz(c *gin.Context) {
	filePath := RankFileGz

	// 检查文件是否存在，提供更好的错误处理（可选但推荐）
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		c.String(http.StatusNotFound, "统计文件尚未生成")
		return
	}

	f, err := os.Open(filePath) // 都放在同一个文件夹。
	if ginkit.AbortWithError(c, 500, err) {
		return
	}

	fileInfo, err := f.Stat()
	if ginkit.AbortWithError(c, 500, err) {
		return
	}

	c.DataFromReader(200, fileInfo.Size(), "application/json", f, map[string]string{"content-encoding": "gzip"})
}

// POST /emojis?username={}&emoji={}
// 为指定用户的某个 Emoji 投票 (+1)
func VoteUpEmojiHandler(c *gin.Context) {
	username := c.Query("username")
	emoji := c.Query("emoji")

	// Gin 会自动解析 URL 编码的 Emoji
	if username == "" || emoji == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// 调用之前实现的数据库方法
	if err := VoteUpEmoji(username, emoji); err != nil {
		if ginkit.AbortWithError(c, http.StatusInternalServerError, err) {
			return
		}
	}

	// 返回更新后的数据或者简单的成功状态
	emojis, _ := GetUserEmojis(username)
	c.JSON(200, emojis)
}

func AddToGroup(g *gin.RouterGroup) {

	limiter := limit.NewFastLimiter(25)

	// 封禁走 ipban 进程级单例：根 API 与 gallery 必须是同一份内存副本、
	// 同一个热重载协程（协程由 Shared() 内部挂，这里不再自己起，
	// 否则两份各自 reload 会出现「一边已封一边没封」的窗口）。
	banMgr := ipban.Shared()

	g.POST("/:username", StrictIPBanMiddleware(banMgr), limit.RateLimitMiddleware(limiter), limit.GlobalRateLimitMiddleware(), CreateMetaData)
	g.GET("/:fn", GetMetaData)
	g.GET("/tags/:username", GetTags)
	// g.POST("/tags/:username", PostTags) // 使用 ?donotrenew
	g.GET("/", GetLists)
	// admin
	g.DELETE("/:username", DeleteUser)
	g.PUT("/:username", CreateUser)

	// 26.02.15
	// 新增 Emoji 路由
	g.GET("/emojis.json.gz", GetEmojisGz)
	g.GET("/emojis", GetEmojis)
	g.POST("/emojis", VoteUpEmojiHandler)
}
