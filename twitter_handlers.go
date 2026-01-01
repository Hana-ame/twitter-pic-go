// 2026.01.01
// 似乎缺少了302的逻辑了。
// 之前的修改版似乎是不见了。

package twitter

import (
	"net/http"
	"os"

	ginkit "github.com/Hana-ame/twitter-pic-go/Tools/ginkit"
	"github.com/gin-gonic/gin"
)

// POST /?username=
func CreateMetaData(c *gin.Context) {
	username, ok := c.GetQuery("username")
	if !ok || username == "" {
		c.JSON(400, gin.H{
			"error": "username is required",
		})
		return
	}
	c.Set("username", username)

	_, err := curlMetaData(username)
	if ginkit.AbortWithError(c, 500, err) {
		return
	}

	c.AbortWithStatus(200)
}

// GET /:fn
func GetMetaData(c *gin.Context) {
	fn := c.Param("fn")
	if fn == "" {
		c.JSON(400, gin.H{
			"error": "username is required",
		})
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
		r, err := getSearch(by, search)
		if ginkit.AbortWithError(c, 500, err) {
			return
		}
		c.JSON(200, r)
		return
	}

	c.String(http.StatusNotImplemented, "not implemented")

	return
}

func AddToGroup(g *gin.RouterGroup) {
	g.POST("/", CreateMetaData)
	g.GET("/:fn", GetMetaData)
	g.GET("/", GetLists)
}
