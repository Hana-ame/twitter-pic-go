// 2026.01.01
// 似乎缺少了302的逻辑了。
// 之前的修改版似乎是不见了。

package twitter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/Hana-ame/twitter-pic-go/Tools/ginkit"
	"github.com/Hana-ame/twitter-pic-go/limit"
	"github.com/gin-gonic/gin"
)

// POST /:username
// ?do_not_tag=true 跳过加tag环节，如果存在则更新，如果不存在则跳过
// ?do_not_renew=true 用来添加tag
func CreateMetaData(c *gin.Context) {
	username := c.Param("username")
	if username == "" {
		c.JSON(400, gin.H{
			"error": "username is required",
		})
		return
	}
	c.Set("username", username)

	// 2026.01.01
	// 需要检查 body json，是这次添加的tag。
	ip := c.GetHeader(ginkit.XForwardedFor)
	agent := c.Request.UserAgent()

	// do_not_tag flag is not exist.
	_, doNotTag := c.GetQuery("do_not_tag")
	_, doNotRenew := c.GetQuery("do_not_renew")
	if !doNotTag && !doNotRenew {
		// 只能在第一次添加的时候用，因为权重不同。
		user, _ := getUserTags(username)
		if len(user.Tags) > 0 { // 已经添加过了，不要这么做。
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
		for k, v := range o {
			if v > 0 {
				o[k] = 5
			} else if v < 0 {
				o[k] = -1
			} else {
				delete(o, k)
			}
		}

		addTag(username, o, ip, agent)

		curlMetaData(username)

		return
	}

	if doNotTag && !doNotRenew {
		// do_not_tag = true;
		// 如果有 do_not_tag 标记，检查是否有记录，如果没有，则直接return
		_, err := getUserTags(username)
		if ginkit.AbortWithError(c, 403, err) {
			return
		}

		curlMetaData(username)

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
		for k, v := range o {
			if v > 0 {
				o[k] = 1
			} else if v < 0 {
				o[k] = -1
			} else {
				delete(o, k)
			}
		}
		addTag(username, o, ip, agent)

		return
	}

	c.AbortWithStatus(200)
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

	if _, ok := c.GetQuery("t"); !ok {
		username := fn
		user, err := getUserTags(username)
		if ginkit.AbortWithError(c, 404, err) {
			return
		}
		c.Redirect(302, c.Request.URL.String()+".json.gz?t="+user.LastModify.String())
		return
	}

	if !strings.HasSuffix(fn, "json.gz") {
		ginkit.AbortWithError(c, 403, fmt.Errorf("not allowed"))
		return
	}

	// 根据fn打开文件返回

	f, err := os.Open(fn) // 都放在同一个文件夹。
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
		r, _ := getSearch(by, search)
		c.JSON(200, r)
		return
	}

	c.String(http.StatusNotImplemented, "not implemented")
}

func DeleteUser(c *gin.Context) {
	if c.Query("delete") == os.Getenv("DELETE_KEY") {
		commitUser(c.Param("username"), "BANNED")
	}
}
func CreateUser(c *gin.Context) {
	if c.Query("delete") == os.Getenv("DELETE_KEY") {
		commitUser(c.Param("username"), "SUCCESS")
	}
}

func AddToGroup(g *gin.RouterGroup) {

	limiter := limit.NewIPRateLimiter(2, 5)

	g.POST("/:username", limit.RateLimitMiddleware(limiter), CreateMetaData)
	g.GET("/:fn", GetMetaData)
	g.GET("/tags/:username", GetTags)
	// g.POST("/tags/:username", PostTags) // 使用 ?donotrenew
	g.GET("/", GetLists)
	// admin
	g.DELETE("/:username", DeleteUser)
	g.PUT("/:username", CreateUser)
}
