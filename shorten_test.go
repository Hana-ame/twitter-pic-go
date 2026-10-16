package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Hana-ame/twitter-pic-go/limit"
	"github.com/gin-gonic/gin"
)

// ==================== 上游 mock ====================

// newMockUpstream 起一个假的 ouo.io，记录它收到的原始 query string。
//
// status + body 由 handler 决定，方便精确复现上游的每一种病态响应。
func newMockUpstream(t *testing.T, status int, body string) (*httptest.Server, *[]string) {
	t.Helper()
	var rawQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawQueries = append(rawQueries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "text/html; charset=UTF-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &rawQueries
}

// 短链成功响应体，各用例复用。
const mockShortURL = "https://ouo.io/4pYOjJr"

// mockTimeoutUpstream 起一个永远卡住的假上游，用于验证超时分支。
func mockTimeoutUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	return srv
}

// 首页 HTML（约 13.5KB），用于复现「200 + 假成功」这条最关键的陷阱。
func fakeHomePage() string {
	return `<!DOCTYPE html>
<html lang="en"><head><title>OuO</title></head>
<body><div id="app">` + strings.Repeat("x", 13000) + `</div></body></html>`
}

// ==================== OuoShorten：成功与假成功 ====================

func TestOuoShortenSuccess(t *testing.T) {
	srv, _ := newMockUpstream(t, http.StatusOK, mockShortURL)

	got, err := OuoShorten(context.Background(), srv.URL, "testkey", "https://example.com", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != mockShortURL {
		t.Fatalf("got %q", got)
	}
}

// 全套测试里最关键的一条：上游 200 + 首页 HTML 时绝不能当短链返回。
func TestOuoShortenFakeSuccessHomePage(t *testing.T) {
	srv, _ := newMockUpstream(t, http.StatusOK, fakeHomePage())

	got, err := OuoShorten(context.Background(), srv.URL, "testkey", "https://example.com", 5*time.Second)
	if err == nil {
		t.Fatalf("expected ErrShortenBadBody, got short url %q", got)
	}
	if !errors.Is(err, ErrShortenBadBody) {
		t.Fatalf("expected ErrShortenBadBody, got %v", err)
	}
	if got != "" {
		t.Fatalf("short url must be empty on error, got %q", got)
	}
}

// 上游返回其它非短链内容（JSON 错误、纯文本提示、错误域名）同样必须拒绝。
// 首尾空白不算——那属于合法的宽松容错，见 TestOuoShortenTrimsSurroundingWhitespace。
func TestOuoShortenBadBodyVariants(t *testing.T) {
	bad := []struct {
		name string
		body string
	}{
		{"json error", `{"error":"invalid key"}`},
		{"plain text", "invalid key"},
		{"empty", ""},
		{"wrong domain", "https://evil.com/ABCD"},
		{"illegal code chars", "https://ouo.io/abcd-_-/"},
		{"trailing query", "https://ouo.io/abcd1?a=1"},
		{"null byte", "https://ouo.io/abcd1\x00"},
		{"not a url", "OK"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newMockUpstream(t, http.StatusOK, tc.body)
			got, err := OuoShorten(context.Background(), srv.URL, "testkey", "https://example.com", 5*time.Second)
			if err == nil {
				t.Fatalf("expected error, got short url %q", got)
			}
			if !errors.Is(err, ErrShortenBadBody) {
				t.Fatalf("expected ErrShortenBadBody, got %v", err)
			}
			if got != "" {
				t.Fatalf("short url must be empty on error, got %q", got)
			}
		})
	}
}

// TrimSpace 只该去掉首尾空白，不能改变码本身。
func TestOuoShortenTrimsSurroundingWhitespace(t *testing.T) {
	srv, _ := newMockUpstream(t, http.StatusOK, "  https://ouo.io/abcd1 \n")

	got, err := OuoShorten(context.Background(), srv.URL, "testkey", "https://example.com", 5*time.Second)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if got != "https://ouo.io/abcd1" {
		t.Fatalf("got %q", got)
	}
}

// ==================== OuoShorten：上游错误码 ====================

// 无效 key → 302。必须阻止重定向跟随，否则会跟成 200 + 首页 HTML。
func TestOuoShortenRedirectNotFollowed(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Location", "https://ouo.io/")
		w.WriteHeader(http.StatusFound)
		_, _ = w.Write([]byte("redirect"))
	}))
	t.Cleanup(srv.Close)

	_, err := OuoShorten(context.Background(), srv.URL, "badkey", "https://example.com", 5*time.Second)
	if !errors.Is(err, ErrShortenUpstreamReject) {
		t.Fatalf("expected ErrShortenUpstreamReject, got %v", err)
	}
	if hits != 1 {
		t.Fatalf("expected exactly 1 upstream request (no redirect following), got %d", hits)
	}
}

func TestOuoShorten301AlsoRejected(t *testing.T) {
	srv, _ := newMockUpstream(t, http.StatusMovedPermanently, "")

	_, err := OuoShorten(context.Background(), srv.URL, "badkey", "https://example.com", 5*time.Second)
	if !errors.Is(err, ErrShortenUpstreamReject) {
		t.Fatalf("expected ErrShortenUpstreamReject for 301, got %v", err)
	}
}

func TestOuoShortenWAF403(t *testing.T) {
	srv, _ := newMockUpstream(t, http.StatusForbidden, "Sorry, you have been blocked")

	_, err := OuoShorten(context.Background(), srv.URL, "testkey", "https://example.com", 5*time.Second)
	if !errors.Is(err, ErrShortenBlockedByWAF) {
		t.Fatalf("expected ErrShortenBlockedByWAF, got %v", err)
	}
}

// 其它未定义的状态码走 fmt.Errorf 分支，状态码原样回传给调用方。
func TestOuoShortenOtherStatusCodes(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusServiceUnavailable,
	} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			srv, _ := newMockUpstream(t, status, "whatever")
			_, err := OuoShorten(context.Background(), srv.URL, "testkey", "https://example.com", 5*time.Second)
			if err == nil {
				t.Fatal("expected error")
			}
			for _, known := range []error{
				ErrShortenUpstreamReject, ErrShortenBlockedByWAF,
				ErrShortenBadBody, ErrShortenNotConfigured,
			} {
				if errors.Is(err, known) {
					t.Fatalf("status %d: unexpected known error %v", status, err)
				}
			}
			if got := shortenHTTPStatus(err); got != status {
				t.Fatalf("status %d: expected passthrough, got %d", status, got)
			}
		})
	}
}

func TestOuoShortenTimeout(t *testing.T) {
	srv := mockTimeoutUpstream(t)

	_, err := OuoShorten(context.Background(), srv.URL, "testkey", "https://example.com", 200*time.Millisecond)
	if !errors.Is(err, errOuoTimeout) {
		t.Fatalf("expected errOuoTimeout, got %v", err)
	}
}

func TestOuoShortenContextCanceled(t *testing.T) {
	srv := mockTimeoutUpstream(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立刻取消
	_, err := OuoShorten(ctx, srv.URL, "testkey", "https://example.com", 5*time.Second)
	if !errors.Is(err, errOuoTimeout) {
		t.Fatalf("expected errOuoTimeout for canceled ctx, got %v", err)
	}
}

func TestOuoShortenEmptyKey(t *testing.T) {
	for _, key := range []string{"", "   ", "\t\n"} {
		t.Run(fmt.Sprintf("key=%q", key), func(t *testing.T) {
			_, err := OuoShorten(context.Background(), "https://ouo.io", key, "https://example.com", 5*time.Second)
			if !errors.Is(err, ErrShortenNotConfigured) {
				t.Fatalf("expected ErrShortenNotConfigured, got %v", err)
			}
		})
	}
}

// ==================== OuoShorten：URL 编码 ====================

func TestOuoShortenURLIsEncoded(t *testing.T) {
	cases := []string{
		"https://example.com/a b/c",          // 空格
		"https://example.com/你好",             // 中文
		"https://example.com/a?x=1&y=2#frag", // & 与 #
		"https://example.com/中文/你好%20",       // 已含百分号
		"http://example.com/plain",
	}
	for _, target := range cases {
		t.Run(target, func(t *testing.T) {
			srv, rawQueries := newMockUpstream(t, http.StatusOK, mockShortURL)
			_, err := OuoShorten(context.Background(), srv.URL, "testkey", target, 5*time.Second)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(*rawQueries) != 1 {
				t.Fatalf("expected 1 upstream request, got %d", len(*rawQueries))
			}
			raw := (*rawQueries)[0]

			q, err := url.ParseQuery(raw)
			if err != nil {
				t.Fatalf("raw query not parseable %q: %v", raw, err)
			}
			if len(q) != 1 {
				t.Fatalf("raw query %q: expected exactly one param", raw)
			}
			if got := q.Get("s"); got != target {
				t.Fatalf("raw query %q: decoded s = %q, want %q", raw, got, target)
			}
			// 原始 query 里不能出现裸空格（会导致上游/代理解析错乱）。
			if strings.Contains(raw, " ") {
				t.Fatalf("raw query contains unencoded space: %q", raw)
			}
			// 原始 query 里不能出现裸的 CJK 字符。
			for _, r := range raw {
				if r >= 0x4e00 && r <= 0x9fff {
					t.Fatalf("raw query contains unencoded CJK: %q", raw)
				}
			}
		})
	}
}

// key 必须进路径段，而不是 header 或 query（上游实测只认路径段）。
func TestOuoShortenKeyGoesInPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/testkey" {
			t.Errorf("key must be a path segment, got path %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Api-Key") != "" {
			t.Error("key must not be sent as a header")
		}
		if _, has := r.URL.Query()["key"]; has {
			t.Error("key must not be a query param")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockShortURL))
	}))
	t.Cleanup(srv.Close)

	_, err := OuoShorten(context.Background(), srv.URL, "testkey", "https://example.com", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// baseURL 尾部斜杠必须被吃掉，否则会拼成 /api//testkey。
func TestOuoShortenBaseURLTrailingSlash(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(mockShortURL))
	}))
	t.Cleanup(srv.Close)

	_, err := OuoShorten(context.Background(), srv.URL+"/", "testkey", "https://example.com", 5*time.Second)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if path != "/api/testkey" {
		t.Fatalf("path = %q, want /api/testkey", path)
	}
}

// ==================== validateShortenURL ====================

func TestValidateShortenURL(t *testing.T) {
	valid := []string{
		"https://example.com",
		"http://example.com/a/b?c=d",
		"https://例子.com/路径",
		"  https://example.com  ", // 首尾空白可容忍
	}
	for _, tc := range valid {
		if err := validateShortenURL(tc); err != nil {
			t.Errorf("expected valid %q, got %v", tc, err)
		}
	}

	invalid := []struct {
		name   string
		target string
	}{
		{"empty", ""},
		{"blank", "   "},
		{"javascript", "javascript:alert(1)"},
		{"data", "data:text/html;base64,AAAA"},
		{"ftp", "ftp://example.com/x"},
		{"file", "file:///etc/passwd"},
		{"no scheme", "example.com/no-scheme"},
		{"no host", "https://"},
		{"path only", "https:///path-only"},
	}
	for _, tc := range invalid {
		if err := validateShortenURL(tc.target); !errors.Is(err, ErrShortenInvalidURL) {
			t.Errorf("%s (%q): expected ErrShortenInvalidURL, got %v", tc.name, tc.target, err)
		}
	}

	if err := validateShortenURL(strings.Repeat("a", 3000)); !errors.Is(err, ErrShortenInvalidURL) {
		t.Errorf("over-long target must be rejected, got %v", err)
	}
}

// ==================== 上游错误 → HTTP 状态码映射 ====================

func TestShortenHTTPStatusMapping(t *testing.T) {
	for _, tc := range []struct {
		name   string
		err    error
		status int
	}{
		{"not configured", ErrShortenNotConfigured, http.StatusServiceUnavailable},
		{"upstream reject", ErrShortenUpstreamReject, http.StatusBadGateway},
		{"waf", ErrShortenBlockedByWAF, http.StatusBadGateway},
		{"bad body", ErrShortenBadBody, http.StatusBadGateway},
		{"timeout", errOuoTimeout, http.StatusGatewayTimeout},
		{"ctx canceled", context.Canceled, http.StatusGatewayTimeout},
		{"ctx deadline", context.DeadlineExceeded, http.StatusGatewayTimeout},
		{"invalid url", ErrShortenInvalidURL, http.StatusBadRequest},
		// 非 HTTP 范围的状态码兜底为 502
		{"upstream out of range", &upstreamStatusError{code: 999}, http.StatusBadGateway},
		{"upstream 429 passthrough", &upstreamStatusError{code: 429}, http.StatusTooManyRequests},
		{"unknown error", errors.New("some other failure"), http.StatusBadGateway},
	} {
		if got := shortenHTTPStatus(tc.err); got != tc.status {
			t.Errorf("%s: expected %d, got %d", tc.name, tc.status, got)
		}
	}
}

// ==================== 上游配置解析 ====================

func TestOuoConfig(t *testing.T) {
	t.Setenv("OUO_BASE_URL", "")
	t.Setenv("OUO_API_KEY", "  mykey  ")
	t.Setenv("OUO_TIMEOUT_MS", "")

	base, key, timeout := ouoConfig()
	if base != "https://ouo.io" {
		t.Errorf("base = %q, want default https://ouo.io", base)
	}
	if key != "mykey" {
		t.Errorf("key = %q, want trimmed mykey", key)
	}
	if timeout != 15*time.Second {
		t.Errorf("timeout = %v, want 15s default", timeout)
	}

	t.Setenv("OUO_BASE_URL", "https://custom.example/")
	t.Setenv("OUO_API_KEY", "")
	t.Setenv("OUO_TIMEOUT_MS", "3000")
	base, key, timeout = ouoConfig()
	if base != "https://custom.example/" {
		t.Errorf("base = %q", base)
	}
	if key != "" {
		t.Errorf("key = %q, want empty", key)
	}
	if timeout != 3*time.Second {
		t.Errorf("timeout = %v, want 3s", timeout)
	}
}

func TestParseOuoTimeoutMS(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"15000", 15 * time.Second, true},
		{"100", 100 * time.Millisecond, true},
		{"60000", 60 * time.Second, true},
		{"99", 0, false},    // 下限
		{"60001", 0, false}, // 上限
		{"0", 0, false},
		{"abc", 0, false},
		{"", 0, false},
	} {
		got, err := parseOuoTimeoutMS(tc.in)
		if tc.ok && err != nil {
			t.Errorf("in %q: unexpected error %v", tc.in, err)
		}
		if !tc.ok && err == nil {
			t.Errorf("in %q: expected error, got %v", tc.in, got)
		}
		if tc.ok && got != tc.want {
			t.Errorf("in %q: got %v, want %v", tc.in, got, tc.want)
		}
	}
}

// ==================== gin 路由：GET 页面 ====================

func TestHandleShortenFormGET(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v2/shorten", HandleShortenFormV2)
	r.GET("/shorten", HandleShortenFormV2) // server/main.go 里的友好别名

	for _, path := range []string{"/api/v2/shorten", "/shorten"} {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		r.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, w.Code)
		}
		if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
			t.Fatalf("%s: content-type %q, want text/html", path, ct)
		}
		body := w.Body.String()
		for _, want := range []string{
			"<!DOCTYPE html>",
			"<input",
			"<script",
			`id="targetUrl"`,
			"application/json",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("%s: body missing %q (len=%d)", path, want, len(body))
			}
		}
		// 页面里不能出现明文 key（key 只存在于服务端）。
		t.Setenv("OUO_API_KEY", "LEAKEDKEY123")
		w2 := httptest.NewRecorder()
		r.ServeHTTP(w2, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(w2.Body.String(), "LEAKEDKEY123") {
			t.Fatalf("%s: api key leaked into html", path)
		}
	}
}

// ==================== gin 路由：POST JSON ====================

func TestHandleShortenV2Success(t *testing.T) {
	t.Setenv("OUO_API_KEY", "testkey")

	srv, _ := newMockUpstream(t, http.StatusOK, mockShortURL)
	t.Setenv("OUO_BASE_URL", srv.URL)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v2/shorten", HandleShortenV2)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/shorten",
		strings.NewReader(`{"url":"https://example.com/very/long/path"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	var resp shortenResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not json: %v body=%s", err, w.Body.String())
	}
	if resp.ShortURL != mockShortURL {
		t.Fatalf("short_url = %q", resp.ShortURL)
	}
	if resp.Provider != "ouo.io" {
		t.Fatalf("provider = %q", resp.Provider)
	}
	if resp.URL != "https://example.com/very/long/path" {
		t.Fatalf("url = %q", resp.URL)
	}
}

func TestHandleShortenV2NotConfigured(t *testing.T) {
	t.Setenv("OUO_API_KEY", "")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v2/shorten", HandleShortenV2)

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/shorten",
		strings.NewReader(`{"url":"https://example.com"}`))
	r.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "OUO_API_KEY") {
		t.Fatalf("error message must name the missing env var, got %s", w.Body.String())
	}
}

// 上游病态响应必须翻成 502，且响应体不能回显上游整段 HTML。
func TestHandleShortenV2UpstreamFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"fake success home page", http.StatusOK, fakeHomePage()},
		{"302 reject", http.StatusFound, "redirect"},
		{"403 waf", http.StatusForbidden, "blocked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("OUO_API_KEY", "testkey")
			srv, _ := newMockUpstream(t, tc.status, tc.body)
			t.Setenv("OUO_BASE_URL", srv.URL)

			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.POST("/api/v2/shorten", HandleShortenV2)

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v2/shorten",
				strings.NewReader(`{"url":"https://example.com"}`))
			r.ServeHTTP(w, req)

			if w.Code != http.StatusBadGateway {
				t.Fatalf("expected 502, got %d body=%s", w.Code, w.Body.String())
			}
			if n := len(w.Body.Bytes()); n > 512 {
				t.Fatalf("response body too large (%d bytes): %s", n, w.Body.String()[:200])
			}
		})
	}
}

func TestHandleShortenV2BadRequests(t *testing.T) {
	t.Setenv("OUO_API_KEY", "testkey")
	t.Setenv("OUO_BASE_URL", "https://ouo.io")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v2/shorten", HandleShortenV2)

	cases := []struct {
		name string
		body string
	}{
		{"empty body", ""},
		{"invalid json", `{not json`},
		{"missing url", `{}`},
		{"javascript scheme", `{"url":"javascript:alert(1)"}`},
		{"ftp scheme", `{"url":"ftp://example.com/x"}`},
		{"no scheme", `{"url":"example.com/x"}`},
		{"url too long", `{"url":"https://x.com/` + strings.Repeat("a", 3000) + `}"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/v2/shorten", strings.NewReader(tc.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("expected 400, got %d body=%s", w.Code, w.Body.String())
			}
		})
	}

	// 超大请求体（超过 maxShortenRequestBody）
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/shorten",
		strings.NewReader(`{"url":"`+strings.Repeat("a", 40*1024)+`}"`))
	r.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", w.Code)
	}
}

// ==================== 限速 ====================

func TestShortenRateLimit(t *testing.T) {
	t.Setenv("OUO_API_KEY", "testkey")
	srv, _ := newMockUpstream(t, http.StatusOK, mockShortURL)
	t.Setenv("OUO_BASE_URL", srv.URL)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	// 额度设成 2，验证「POST 真的挂了限速」这件事。
	r.POST("/api/v2/shorten", limit.RateLimitMiddleware(limit.NewFastLimiter(2)), HandleShortenV2)

	codes := make([]int, 0, 4)
	for i := 0; i < 4; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v2/shorten",
			strings.NewReader(`{"url":"https://example.com"}`))
		req.RemoteAddr = "203.0.113.7:1234"
		r.ServeHTTP(w, req)
		codes = append(codes, w.Code)
	}
	if codes[0] != http.StatusOK || codes[1] != http.StatusOK {
		t.Fatalf("first two requests should pass, got %v", codes)
	}
	for i := 2; i < len(codes); i++ {
		if codes[i] != http.StatusTooManyRequests {
			t.Fatalf("request %d: expected 429, got %d", i, codes[i])
		}
	}
}

// 不同 IP 各自独立计数，不能互相拖累。
func TestShortenRateLimitPerIP(t *testing.T) {
	t.Setenv("OUO_API_KEY", "testkey")

	srv, _ := newMockUpstream(t, http.StatusOK, fakeHomePage()) // 故意返回坏 body
	t.Setenv("OUO_BASE_URL", srv.URL)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v2/shorten", limit.RateLimitMiddleware(limit.NewFastLimiter(1)), HandleShortenV2)

	hit := func(ip string) int {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/api/v2/shorten",
			strings.NewReader(`{"url":"https://example.com"}`))
		req.RemoteAddr = ip
		r.ServeHTTP(w, req)
		return w.Code
	}

	if got := hit("203.0.113.1:1"); got != http.StatusBadGateway {
		t.Fatalf("ip1 first request should reach handler, got %d", got)
	}
	if got := hit("203.0.113.1:1"); got != http.StatusTooManyRequests {
		t.Fatalf("ip1 second request should be limited, got %d", got)
	}
	// ip2 有自己的桶，第一条仍然能打到 handler。
	if got := hit("203.0.113.2:1"); got != http.StatusBadGateway {
		t.Fatalf("ip2 should have its own bucket, got %d", got)
	}
}

// ==================== RegisterV2Routes 接线 ====================

// 直接调 RegisterV2Routes 验证 /shorten 真的被挂上了，
// 并确认 GET 页面不受限速影响（连打 30 次全 200）。
func TestRegisterV2RoutesWiresShorten(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterV2Routes(r.Group("/api/v2"))

	for i := 0; i < 30; i++ {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/v2/shorten", nil)
		req.RemoteAddr = "203.0.113.9:1"
		r.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("GET #%d: expected 200, got %d body=%s",
				i, w.Code, w.Body.String()[:200])
		}
	}
}

// POST 必须被 IP 封禁中间件保护（RegisterV2Routes 顶层 g.Use 的产物）。
func TestShortenRouteRespectsIPBanMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	RegisterV2Routes(r.Group("/api/v2"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v2/shorten",
		strings.NewReader(`{"url":"https://example.com"}`))
	req.RemoteAddr = "203.0.113.50:1" // 不在 bans.txt 里
	r.ServeHTTP(w, req)

	// 未封禁的 IP 必须放行到 handler（因缺 key 返回 503），而不是被封禁拦成 403。
	if w.Code == http.StatusForbidden {
		t.Fatalf("unbanned ip must not be blocked, got 403 body=%s", w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected handler to run (503), got %d", w.Code)
	}
}

// GET 页面上游挂了也不该 500——页面本身不依赖上游。
func TestShortenFormIndependentOfUpstream(t *testing.T) {
	t.Setenv("OUO_API_KEY", "")
	t.Setenv("OUO_BASE_URL", "http://127.0.0.1:1")

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/v2/shorten", HandleShortenFormV2)

	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/v2/shorten", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET page must work without upstream, got %d", w.Code)
	}
}
