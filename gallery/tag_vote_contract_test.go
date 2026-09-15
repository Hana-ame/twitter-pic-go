package gallery

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件钉住**前端与后端的投票契约**（gallery/static/app.js 与 Flutter 端都按它实现）。
// 契约内容同时抄在根 README 的「一 IP 一票」一节，改这里请一起改那里。
//
// 一句话：`d` 是"该 IP 的目标值"（+1 / -1 / 0=撤票），不是本次变化量。

// postVote 用固定身份发一次投票，返回 (状态码, 响应里的 tags)。
func postVote(t *testing.T, cfg config, body string) (int, map[string]int) {
	t.Helper()
	r := httptest.NewRequest("POST", "/api/tag", strings.NewReader(body))
	// 不带任何代理头 → Principal 取 RemoteAddr，httptest 默认 192.0.2.1:1234，
	// 即本文件内所有请求都是**同一个访客**（幂等性正是靠这个前提来验）。
	w := httptest.NewRecorder()
	handlePostAccountTag(w, r, cfg)
	var out struct {
		User string         `json:"user"`
		Tags map[string]int `json:"tags"`
	}
	if w.Code == 200 {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应体不是合法 JSON: %v (%s)", err, w.Body.String())
		}
	}
	return w.Code, out.Tags
}

// TestVoteContractTargetValue 钉住目标值语义的四个面。
func TestVoteContractTargetValue(t *testing.T) {
	cfg := newTestCfg(t)

	// ① 重复提交同值 = 幂等，且仍回 200（前端可以无脑重发，不必判断"是否已投过"）。
	code, tags := postVote(t, cfg, `{"user":"u","tag":"t","d":1}`)
	if code != 200 || tags["t"] != 1 {
		t.Fatalf("首投应 200 且权重 1，实际 %d %v", code, tags)
	}
	if _, tags := postVote(t, cfg, `{"user":"u","tag":"t","d":1}`); tags["t"] != 1 {
		t.Fatalf("同值重复提交必须幂等（一 IP 一票），实际 %v", tags)
	}

	// ② 反向改票：+1 → -1，**单次请求**对权重的影响是 -2（旧口径夹输入到 ±1，
	//    这一步会算出 0 分——那个死结就是这次改语义要消掉的）。
	//    前端因此绝不能再自己按 ±1 累加显示值，必须以响应里的 tags 为权威（见 ③）。
	if _, tags := postVote(t, cfg, `{"user":"u","tag":"t","d":-1}`); tags["t"] != -1 {
		t.Fatalf("+1 改 -1 后权重应是 -1（净变化 -2），实际 %v", tags)
	}
	// 负权重在传输层保留（与 GET 同口径），只有显示层过滤 >0；再投 +1 应回到 1。
	if _, tags := postVote(t, cfg, `{"user":"u","tag":"t","d":1}`); tags["t"] != 1 {
		t.Fatalf("从 -1 改回 +1 后应是 1，实际 %v", tags)
	}

	// ③ 响应体的 tags 是该账号的**权威全量**（含负权重，不含归零行）：
	//    前端直接覆盖本地缓存即可，不需要自己算增量。
	if _, tags := postVote(t, cfg, `{"user":"u","tag":"t","d":1}`); tags["t"] != 1 {
		t.Fatalf("重新投票应回 1，实际 %v", tags)
	}
	postVote(t, cfg, `{"user":"u","tag":"其他","d":-1}`)
	_, tags = postVote(t, cfg, `{"user":"u","tag":"t","d":1}`)
	if tags["其他"] != -1 {
		t.Fatalf("响应 tags 应包含负权重标签（与 GET 同口径），实际 %v", tags)
	}

	// ④ 越界与缺失都是 400，不静默夹取：d=5 的客户端是有 bug 的，
	//    夹成 1 会让它以为投了 5 票；d 缺失不能当成撤票（0 现在是有效值）。
	for _, body := range []string{
		`{"user":"u","tag":"t","d":5}`,
		`{"user":"u","tag":"t","d":-5}`,
		`{"user":"u","tag":"t"}`,
	} {
		if code, _ := postVote(t, cfg, body); code != 400 {
			t.Fatalf("%s 应 400，实际 %d", body, code)
		}
	}
}

// TestVoteContractWithdraw 撤票：d=0 有效、票行删除、权重回落到剩余票之和。
func TestVoteContractWithdraw(t *testing.T) {
	cfg := newTestCfg(t)
	postVote(t, cfg, `{"user":"u","tag":"t","d":1}`)
	code, tags := postVote(t, cfg, `{"user":"u","tag":"t","d":0}`)
	if code != 200 {
		t.Fatalf("撤票应是合法请求，实际 %d", code)
	}
	if _, ok := tags["t"]; ok {
		t.Fatalf("撤票后权重归零，tags 不该再有 t：%v", tags)
	}
	var n int
	if err := cfg.db.QueryRow(`SELECT COUNT(*) FROM tag_votes WHERE username='u'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("撤票必须删票行（留着它，该 IP 以后就投不了这个标签了），实际 %d 行", n)
	}
	// 撤完还能重投，且底数仍是 0
	if _, tags := postVote(t, cfg, `{"user":"u","tag":"t","d":1}`); tags["t"] != 1 {
		t.Fatalf("撤票后应能重投，实际 %v", tags)
	}
}
