package main

import (
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Hana-ame/twitter-pic-go"
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
	twitter.DB = db

	if err := twitter.CreateTableV3(); err != nil {
		t.Fatal(err)
	}

	router := setupRouter(db)

	// 1. 原 API 专属路由: GET /api/twitter/emojis
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

	// 3. Gallery 静态资源: GET /static/app.js
	reqStatic := httptest.NewRequest("GET", "/static/app.js", nil)
	wStatic := httptest.NewRecorder()
	router.ServeHTTP(wStatic, reqStatic)
	if wStatic.Code != 200 {
		t.Fatalf("Gallery static 期望 200, 实际 %d", wStatic.Code)
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
}
