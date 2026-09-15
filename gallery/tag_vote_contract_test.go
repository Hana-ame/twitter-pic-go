package gallery

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTagVoteDeltaClampedPerRequest 钉住前端依赖的服务端口径：
// **单次** 写请求对单标签最多贡献 ±1（与根 API 的 per-request 归一化同口径）。
// 前端因此把反向切换（净差 ±2）拆成两笔同向 ±1；若哪天服务端放宽了限幅，
// 这条测试会先红，提醒同步检查 gallery/static/app.js 的 postATag 拆笔逻辑。
func TestTagVoteDeltaClampedPerRequest(t *testing.T) {
	cfg := newTestCfg(t)
	post := func(body string) map[string]int {
		r := httptest.NewRequest("POST", "/api/account-tag", strings.NewReader(body))
		w := httptest.NewRecorder()
		handlePostAccountTag(w, r, cfg)
		if w.Code != 200 {
			t.Fatalf("POST %s -> %d %s", body, w.Code, w.Body.String())
		}
		var out struct {
			Tags map[string]int `json:"tags"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Tags
	}

	if got := post(`{"user":"u","tag":"t","d":10}`); got["t"] != 1 {
		t.Fatalf("d=+10 应限幅为 +1，实际 %v", got)
	}
	if got := post(`{"user":"u","tag":"t","d":-10}`); got["t"] != 0 {
		t.Fatalf("-1 后再 d=-10 应止步于 0（行删除），实际 %v", got)
	}
	// 前端拆笔后的等效路径：两笔 +1 共 +2
	post(`{"user":"u","tag":"t","d":1}`)
	if got := post(`{"user":"u","tag":"t","d":1}`); got["t"] != 2 {
		t.Fatalf("两笔 +1 应累加成 2，实际 %v", got)
	}
}
