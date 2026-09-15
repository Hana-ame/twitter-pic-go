package twitter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestRootGetMatchesStoreWeights 钉住「两层做成一样」的读侧口径：
// 根 API 的 tags 字段（userSelectQuery 的 json_group_object）必须与共享包的
// Store.Weights 逐键相等——同一个过滤规则、同一份数据源。
//
// 这条测试存在的原因：写侧早就统一了（addTag → tags.Store.Add），但根 API 的
// 读侧曾是裸的 json_group_object，**不带 weight 过滤**，于是库面上一旦出现
// weight=0 的行（外部 sqlite3 运维写入、或历史脏数据），根 API 会返回它而
// gallery 不会——同一账号两层给出不同标签集。
func TestRootGetMatchesStoreWeights(t *testing.T) {
	setupTagsDB(t)
	if err := CreateTableV2(); err != nil {
		t.Fatal(err)
	}
	if _, err := DB.Exec(`INSERT INTO users (username, status) VALUES ('x','SUCCESS')`); err != nil {
		t.Fatal(err)
	}
	if err := addTag("x", map[string]int{"女性": 2, "COS": 1, "男娘": -1}, "ip", "ua"); err != nil {
		t.Fatal(err)
	}
	// 手工塞一条 weight=0 的行（Add 自己不会产生，但读侧口径必须能挡住它）。
	if _, err := DB.Exec(`INSERT INTO account_tags (username, tag, weight) VALUES ('x','零权',0)`); err != nil {
		t.Fatal(err)
	}

	u, err := getUserTags("x")
	if err != nil {
		t.Fatal(err)
	}
	want := Store().Weights("x")

	if _, ok := u.Tags["零权"]; ok {
		t.Fatalf("根 API 读侧漏掉了 weight 过滤，返回了零权行: %+v", u.Tags)
	}
	if !reflect.DeepEqual(u.Tags, want) {
		t.Fatalf("两层读侧口径不一致\n 根 API: %+v\n Store : %+v", u.Tags, want)
	}
	// 负权重两边都应保留（归零删行是写侧的事，读侧不擅自抹掉）。
	if want["男娘"] != -1 || u.Tags["男娘"] != -1 {
		t.Fatalf("负权重应保留: api=%+v store=%+v", u.Tags, want)
	}
}

// TestWriteFailureNotReportedAsOK 钉住另一个吞错点：addTag 失败时根 API 必须
// 报 500，而不是照旧回 200 {"message":"ok"}——否则客户端无法知道标签进没进库，
// 而 gallery 侧是有 400/500/503 的，两层口径就不一样了。
func TestWriteFailureNotReportedAsOK(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setupTagsDB(t)
	if err := CreateTableV2(); err != nil {
		t.Fatal(err)
	}
	if _, err := DB.Exec(`INSERT INTO users (username, status) VALUES ('x','SUCCESS')`); err != nil {
		t.Fatal(err)
	}
	// 用一个触发器让写失败，同时**不影响读**（直接 DROP 表会把 getUserTags 一起打挂，
	// 那样 500 是从前置检查来的，测不到我们要测的那一行）。
	if _, err := DB.Exec(`CREATE TRIGGER boom BEFORE INSERT ON account_tags
		BEGIN SELECT RAISE(ABORT, 'injected write failure'); END`); err != nil {
		t.Fatal(err)
	}

	r := gin.New()
	r.POST("/api/twitter/:username", CreateMetaData)
	req := httptest.NewRequest("POST", "/api/twitter/x?do_not_renew=true",
		strings.NewReader(`{"新标签":1}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Fatalf("写库失败却回了 200：%s", w.Body.String())
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("应回 500，实际 %d：%s", w.Code, w.Body.String())
	}
	// 失败不能留下半截数据。
	u, err := getUserTags("x")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(u.Tags); string(b) != "{}" {
		t.Fatalf("写失败后不应有任何标签落库: %s", b)
	}
}
