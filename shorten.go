// Package twitter —— 短链生成（上游 ouo.io）。
//
// 需求见 issue #5：GET 同一 path 返回自包含前端 HTML 页面，POST 返回 JSON。
//
// 上游协议（已实测取证 2026-10-17，官方文档亦确认）：
//
//	GET {OUO_BASE_URL}/api/{API_KEY}?s={目标URL}
//	→ HTTP 200，body 是纯文本短链（21–22 字节），例如 https://ouo.io/WHctTU
//
// 三个必须处理的上游陷阱（全部实测确认）：
//
//  1. 假成功：`?s=` 缺失或为空时上游返回 **200 + 首页 HTML（约 13.5KB）**，
//     不是 4xx。所以成功判据不能只看状态码，必须再校验 body 匹配短链正则。
//  2. 错误用 302 表达：无效 key 返回 `302 Location: https://ouo.io/`。
//     http.Client 默认跟随重定向，跟完就变成 200 + 首页 HTML、错误被彻底吞掉——
//     所以必须设 CheckRedirect 阻止跟随。
//  3. 只支持 GET：POST 一律 302（`Allow: GET,HEAD`）。
//
// 另一个可选上游入口是 `/qs/{key}?s={url}`：它返回一个带 CSRF `_token` 的
// 自提交表单，POST 后 302 到新建的短链。我们不用它——需要先取 token 再 POST
// 两跳，且拿不到短链文本（只有 Location 头）。
package twitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ==================== 上游配置 ====================

const (
	defaultOuoBaseURL     = "https://ouo.io"
	defaultOuoTimeout     = 15 * time.Second // 实测单请求 1.7–4.2s，5s 默认值会误杀
	maxOuoBodyBytes       = 4096             // 假成功场景 body 约 13.5KB，别整包读进内存
	maxShortenRequestBody = 32 * 1024        // 拒绝超大请求体
	maxTargetURLLen       = 2048
)

// errOuoTimeout 专指「上游超时 / 网络不可达」，区别于「上游返回了但拒绝了」。
var errOuoTimeout = errors.New("upstream timeout")

// ErrShortenNotConfigured：服务端没配 OUO_API_KEY。
//
// 绝不回落到任何内置 key：上游文档里给的是公共示例 key，
// 用它生成的短链收益归文档作者、不归本站。
var ErrShortenNotConfigured = errors.New("shorten not configured: OUO_API_KEY is empty")

// ErrShortenUpstreamReject：上游用 3xx 拒绝（实测：无效 key）。
var ErrShortenUpstreamReject = errors.New("upstream rejected")

// ErrShortenBlockedByWAF：上游 403（实测：Cloudflare WAF 按 payload 拦 javascript: 等）。
var ErrShortenBlockedByWAF = errors.New("upstream blocked by waf")

// ErrShortenBadBody：上游 200 但 body 不是短链（实测：`?s=` 为空时返回首页 HTML）。
var ErrShortenBadBody = errors.New("upstream returned unexpected body")

// ErrShortenInvalidURL：调用方给的 URL 不合法。
var ErrShortenInvalidURL = errors.New("invalid url")

// ouoShortURLRe 是「上游真的给了短链」的判据。
//
// 不能用 url.Parse 代替：假成功时 body 是完整 HTML 页面（含 <!DOCTYPE>），
// 解析出来照样像个合法 URL。必须严格锚定形状。
// 子域前缀放宽到 [a-z0-9.-]* 避免换 CDN 主机时改代码；
// scheme 只放 http/https，javascript:/data: 之类一律拒。
var ouoShortURLRe = regexp.MustCompile(`^https?://[a-z0-9.-]*ouo\.io/[A-Za-z0-9]+$`)

// ouoConfig 返回当前生效的上游配置。每次请求都读一次 env，不缓存：
// 改动立即生效，且这里没有并发安全问题。
func ouoConfig() (baseURL, key string, timeout time.Duration) {
	baseURL = strings.TrimSpace(os.Getenv("OUO_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultOuoBaseURL
	}
	key = strings.TrimSpace(os.Getenv("OUO_API_KEY"))

	timeout = defaultOuoTimeout
	if v := strings.TrimSpace(os.Getenv("OUO_TIMEOUT_MS")); v != "" {
		if ms, err := parseOuoTimeoutMS(v); err == nil {
			timeout = ms
		}
	}
	return baseURL, key, timeout
}

func parseOuoTimeoutMS(v string) (time.Duration, error) {
	var ms int64
	if _, err := fmt.Sscanf(v, "%d", &ms); err != nil {
		return 0, err
	}
	// 100ms 以下没有意义；60s 以上会长时间占住 worker。
	if ms < 100 || ms > 60_000 {
		return 0, fmt.Errorf("out of range: %s", v)
	}
	return time.Duration(ms) * time.Millisecond, nil
}

// ==================== 上游客户端 ====================

// OuoShorten 调上游 ouo.io 生成一条短链，返回短链 URL。
//
// baseURL / key / target / timeout 全部显式传入而不是内部读 env，
// 这样测试可以用 httptest.NewServer 把上游换成 mock（见 shorten_test.go），
// 也能在测试里注入极短超时来验证 errOuoTimeout 分支。
//
// http.Client 每次新建而不复用：上游要求 15s 量级的超时，
// 复用共享 client 会让测试无法独立注入超时与重定向行为。
func OuoShorten(ctx context.Context, baseURL, key, target string, timeout time.Duration) (string, error) {
	if strings.TrimSpace(key) == "" {
		return "", ErrShortenNotConfigured
	}
	if err := validateShortenURL(target); err != nil {
		return "", err
	}

	u, err := url.Parse(strings.TrimRight(baseURL, "/") + "/api/" + url.PathEscape(key))
	if err != nil {
		return "", fmt.Errorf("build upstream url: %w", err)
	}
	// RawQuery 直接赋编码后的值，避免 Query().Encode() 再编码一次。
	u.RawQuery = "s=" + url.QueryEscape(target)

	client := &http.Client{
		Timeout: timeout,
		// 关键：不跟随重定向。否则无效 key 的 302 会被跟成 200 + 首页 HTML，
		// 错误彻底消失，调用方会拿到一个指向 ouo.io 首页的「短链」。
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", fmt.Errorf("build upstream request: %w", err)
	}
	req.Header.Set("User-Agent", "twitter-pic-go/1.0 shorten")

	resp, err := client.Do(req)
	if err != nil {
		return "", errOuoTimeout // 含 ctx 取消与 connect 失败，统一归为超时
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxOuoBodyBytes))
	switch {
	case resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound:
		return "", ErrShortenUpstreamReject
	case resp.StatusCode == http.StatusForbidden:
		return "", ErrShortenBlockedByWAF
	case resp.StatusCode != http.StatusOK:
		return "", &upstreamStatusError{code: resp.StatusCode}
	}

	s := strings.TrimSpace(string(body))
	if !ouoShortURLRe.MatchString(s) {
		return "", ErrShortenBadBody
	}
	return s, nil
}

// validateShortenURL 校验目标 URL：必须是 http/https 且带 host。
//
// 服务端必须自己校验一遍，不能信任前端——前端 new URL() 只能挡 UI，
// 直接打 API 的请求完全绕得过去。
func validateShortenURL(target string) error {
	raw := strings.TrimSpace(target)
	if raw == "" || len(raw) > maxTargetURLLen {
		return ErrShortenInvalidURL
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() || u.Host == "" {
		return ErrShortenInvalidURL
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrShortenInvalidURL
	}
	return nil
}

// ==================== 上游错误 → HTTP 状态码 ====================

// upstreamStatusError 携带上游返回的原始状态码。
//
// 只给「上面三类已命名错误都没覆盖到」的状态码用（比如 401 / 429 / 500）。
// 类型化而不是裸 fmt.Errorf，是因为 shortenHTTPStatus 需要拿到原始状态码
// 才能决定要不要透传；用字符串解析状态码既丑又脆。
type upstreamStatusError struct{ code int }

func (e *upstreamStatusError) Error() string {
	return fmt.Sprintf("upstream status %d", e.code)
}

// shortenHTTPStatus 集中做映射，保证 handler 与测试对同一错误给一致的状态码。
func shortenHTTPStatus(err error) int {
	switch {
	case errors.Is(err, ErrShortenNotConfigured):
		return http.StatusServiceUnavailable // 503：部署配置问题，不是调用方错误
	case errors.Is(err, ErrShortenUpstreamReject),
		errors.Is(err, ErrShortenBlockedByWAF),
		errors.Is(err, ErrShortenBadBody):
		return http.StatusBadGateway // 502：上游可达但拒绝
	case errors.Is(err, errOuoTimeout),
		errors.Is(err, context.Canceled),
		errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout // 504：上游超时
	case errors.Is(err, ErrShortenInvalidURL):
		return http.StatusBadRequest
	}

	// 其它上游状态码原样透传：429 就该是 429，500 就该是 500。
	// 上游是我们的依赖而不是攻击者，透传比一律压成 400 更有诊断价值。
	// 非 HTTP 范围的码（比如 999）兜底为 502。
	var se *upstreamStatusError
	if errors.As(err, &se) && se.code >= 400 && se.code < 600 {
		return se.code
	}
	return http.StatusBadGateway
}

// ==================== Handler ====================

// shortenRequest 是 POST 的入参。
type shortenRequest struct {
	URL string `json:"url"`
}

// shortenResponse 是 POST 的成功响应。
type shortenResponse struct {
	URL      string `json:"url"`
	ShortURL string `json:"short_url"`
	Provider string `json:"provider"`
}

// HandleShortenV2 是 POST /shorten：接收 JSON，返回 JSON。
//
//	请求  {"url": "https://example.com/x"}
//	成功  200 {"url":...,"short_url":"https://ouo.io/ABCD","provider":"ouo.io"}
//
// 错误统一走 gin.H{"error": ...}，与 v2_handlers.go 其余 handler 一致。
func HandleShortenV2(c *gin.Context) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, maxShortenRequestBody+1))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body failed"})
		return
	}
	if len(body) > maxShortenRequestBody {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request body too large"})
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "empty body"})
		return
	}

	var req shortenRequest
	if err := json.Unmarshal(body, &req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid json body"})
		return
	}

	baseURL, key, timeout := ouoConfig()
	shortURL, err := OuoShorten(c.Request.Context(), baseURL, key, req.URL, timeout)
	if err != nil {
		// 不回显上游原文：body 可能是 13.5KB 的首页 HTML，
		// 塞进响应既是浪费也容易被当成短链。
		c.JSON(shortenHTTPStatus(err), gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, shortenResponse{
		URL:      req.URL,
		ShortURL: shortURL,
		Provider: "ouo.io",
	})
}
