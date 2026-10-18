package main

import (
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hana-ame/twitter-pic-go/api"
	"github.com/gin-gonic/gin"
	_ "modernc.org/sqlite"
)

func TestUnifiedServerRouting(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "t.db")+
		"?_pragma=busy_timeout(5000)&_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	api.DB = db

	if err := api.CreateTableV3(); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users (username, nick, status) VALUES ('alice', '爱丽丝', 'SUCCESS')`); err != nil {
		t.Fatal(err)
	}

	router := setupRouter(db)

	// 1. V1 API 专属路由: GET /api/twitter/emojis
	reqAPI := httptest.NewRequest("GET", "/api/twitter/emojis", nil)
	wAPI := httptest.NewRecorder()
	router.ServeHTTP(wAPI, reqAPI)
	if wAPI.Code != 200 {
		t.Fatalf("API /api/twitter/emojis 期望 200, 实际 %d: %s", wAPI.Code, wAPI.Body.String())
	}

	// 2. Gallery 首页 SSR: GET /
	reqHome := httptest.NewRequest("GET", "/", nil)
	wHome := httptest.NewRecorder()
	router.ServeHTTP(wHome, reqHome)
	if wHome.Code != 200 {
		t.Fatalf("Gallery SSR / 期望 200, 实际 %d: %s", wHome.Code, wHome.Body.String())
	}

	// 3. Gallery 静态资源（含 JS 与模块化拆分后的 CSS）
	for _, asset := range []string{
		"/static/app.js",
		"/static/home.js",
		"/static/css/base.css",
		"/static/css/home.css",
		"/static/css/account.css",
		"/static/css/lightbox.css",
	} {
		reqStatic := httptest.NewRequest("GET", asset, nil)
		wStatic := httptest.NewRecorder()
		router.ServeHTTP(wStatic, reqStatic)
		if wStatic.Code != 200 {
			t.Fatalf("Gallery static %s 期望 200, 实际 %d", asset, wStatic.Code)
		}
	}

	// 4. Gallery 健康检查: GET /healthz
	reqHealth := httptest.NewRequest("GET", "/healthz", nil)
	wHealth := httptest.NewRecorder()
	router.ServeHTTP(wHealth, reqHealth)
	if wHealth.Code != 200 {
		t.Fatalf("Gallery healthz 期望 200, 实际 %d", wHealth.Code)
	}

	// 5. Gallery 反应 API: GET /api/reactions
	reqReactions := httptest.NewRequest("GET", "/api/reactions", nil)
	wReactions := httptest.NewRecorder()
	router.ServeHTTP(wReactions, reqReactions)
	if wReactions.Code != 200 {
		t.Fatalf("Gallery reactions 期望 200, 实际 %d", wReactions.Code)
	}

	// 6. 新版 Flutter API 路由: /api/twitter/v2/users, /api/v2/users, /api/flutter/users
	for _, path := range []string{"/api/twitter/v2/users", "/api/v2/users", "/api/flutter/users"} {
		reqV2 := httptest.NewRequest("GET", path, nil)
		wV2 := httptest.NewRecorder()
		router.ServeHTTP(wV2, reqV2)
		if wV2.Code != 200 {
			t.Fatalf("Flutter API %s 期望 200, 实际 %d: %s", path, wV2.Code, wV2.Body.String())
		}
	}

	// 7. 跨层同源投票互通验证：通过 Gallery POST /api/tag 投票，立即在 API GET /api/twitter/tags/alice 可见
	reqVote := httptest.NewRequest("POST", "/api/tag", strings.NewReader(`{"user":"alice","tag":"自拍","d":1}`))
	reqVote.RemoteAddr = "198.51.100.10:1234"
	wVote := httptest.NewRecorder()
	router.ServeHTTP(wVote, reqVote)
	if wVote.Code != 200 {
		t.Fatalf("Gallery POST /api/tag 期望 200, 实际 %d: %s", wVote.Code, wVote.Body.String())
	}

	reqCheckAPI := httptest.NewRequest("GET", "/api/twitter/tags/alice", nil)
	wCheckAPI := httptest.NewRecorder()
	router.ServeHTTP(wCheckAPI, reqCheckAPI)
	if wCheckAPI.Code != 200 {
		t.Fatalf("GET /api/twitter/tags/alice 期望 200, 实际 %d", wCheckAPI.Code)
	}
	var userResp api.User
	if err := json.Unmarshal(wCheckAPI.Body.Bytes(), &userResp); err != nil {
		t.Fatal(err)
	}
	if userResp.Tags["自拍"] != 1 {
		t.Fatalf("Gallery 投票结果应立即在 API 层可见，实际: %+v", userResp.Tags)
	}
}
