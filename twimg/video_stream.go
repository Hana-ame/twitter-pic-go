package twimg

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

var (
	// videoAbortedCount 客户端中途断开导致的视频流中断次数
	videoAbortedCount atomic.Int64
	// videoRewriteCount 上游无视 Range 返回全量 200、被本层改写为 206 的次数
	videoRewriteCount atomic.Int64
)

// singleRange 解析单段 Range 头：bytes=a-b / bytes=a- / bytes=-n。
// 多段（bytes=0-1,5-9）或非法格式返回 bounded=false，原样透传。
func singleRange(rng string) (start, end, suffix int64, bounded bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(rng, prefix) {
		return 0, 0, 0, false
	}
	s := strings.TrimSpace(rng[len(prefix):])
	if strings.Contains(s, ",") {
		return 0, 0, 0, false
	}
	i := strings.IndexByte(s, '-')
	if i < 0 {
		return 0, 0, 0, false
	}
	if i == 0 {
		n, err := strconv.ParseInt(strings.TrimSpace(s[1:]), 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, 0, false
		}
		return 0, 0, n, true
	}
	start, err := strconv.ParseInt(strings.TrimSpace(s[:i]), 10, 64)
	if err != nil || start < 0 {
		return 0, 0, 0, false
	}
	endS := strings.TrimSpace(s[i+1:])
	if endS == "" {
		return start, 0, 0, true
	}
	end, err2 := strconv.ParseInt(endS, 10, 64)
	if err2 != nil || end < start {
		return 0, 0, 0, false
	}
	return start, end, 0, true
}

var hopHeaders = map[string]bool{
	"Connection":          true,
	"Proxy-Connection":    true,
	"Keep-Alive":          true,
	"Proxy-Authenticate":  true,
	"Proxy-Authorization": true,
	"Te":                  true,
	"Trailer":             true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
}

func copyNonHop(src, dst http.Header) {
	for k, vs := range src {
		if hopHeaders[k] {
			continue
		}
		dst[k] = append([]string(nil), vs...)
	}
}

// closeResponseBody 安全地关闭响应体，并尝试终止底层连接
func closeResponseBody(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}

	// 首先尝试正常关闭
	resp.Body.Close()

	// 对于 HTTP/2 连接，尝试通过 context 取消来终止
	if resp.Request != nil && resp.Request.Context() != nil {
		// Context 已经由上层管理，这里不需要额外操作
	}
}

// StreamVideoProxy 视频专用流式反代，显式处理 Range 请求与客户端中断：
//
//   - Range 兜底：请求头原样透传（含多段 Range）。若客户端请求了有界单段
//     Range、但上游（CDN/缓存层）无视 Range 返回全量 200，本层主动改写为
//     206 + Content-Range，并且**最多只从上游读请求范围内的字节**——杜绝
//     "只要 1MB 却被灌 618MB" 的浪费。
//   - 强制 identity 编码（剥掉 Accept-Encoding）：保证 Content-Length 可
//     计算、Range 语义不被压缩破坏、206 改写永远可行。
//   - ABORT 兜底：客户端断开（context 取消）时立即停止读取上游 body，连接
//     随之被 http.Transport 关闭（RST），上游（video.twimg.com）立刻停止
//     推送，不再空转流量。每轮拷贝前都检查一次客户端上下文。
//   - 上游无响应时 20 秒内返回 504，避免 Go 默认无限等待挂死连接。
func StreamVideoProxy(targetURL string, headerProcesser func(http.Header) http.Header) gin.HandlerFunc {
	target, err := url.Parse(targetURL)
	if err != nil {
		panic(err)
	}

	// 基于 DefaultTransport 克隆：保留环境代理（ProxyFromEnvironment）等默认行为
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSHandshakeTimeout = 5 * time.Second
	transport.ResponseHeaderTimeout = 20 * time.Second
	transport.MaxIdleConns = 100
	transport.MaxConnsPerHost = 200
	transport.IdleConnTimeout = 90 * time.Second

	client := &http.Client{
		Transport: transport,
	}

	return func(c *gin.Context) {
		ctx := c.Request.Context()
		// 整体请求超时：防止上游拿到 header 后慢慢推 body 导致 goroutine 永久阻塞
		// （ResponseHeaderTimeout 只管 header，不管 body；body 读阻塞是唯一退出条件是客户端断开）
		ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()

		// 请求开始前客户端就已断开
		select {
		case <-ctx.Done():
			c.AbortWithStatus(499)
			return
		default:
		}

		rng := c.GetHeader("Range")
		reqStart, reqEnd, suffix, bounded := singleRange(rng)

		req, err := http.NewRequestWithContext(ctx, c.Request.Method,
			target.String()+c.Request.URL.RequestURI(), nil)
		if err != nil {
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		req.Host = target.Host
		copyNonHop(c.Request.Header, req.Header)
		// 强制 identity：Range + 压缩互斥，且保证 206 改写时长度可计算
		req.Header.Del("Accept-Encoding")
		if headerProcesser != nil {
			req.Header = headerProcesser(req.Header)
		}

		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				// 客户端已断，不重复输出 5xx
				c.AbortWithStatus(499)
				return
			}
			c.AbortWithStatus(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()

		status := resp.StatusCode
		h := resp.Header
		limit := int64(-1) // -1 = 无界，读到 EOF

		// 客户端请求了有界 Range、上游却回全量 200：改写为 206，把读取范围钳住
		if bounded && resp.StatusCode == http.StatusOK && resp.ContentLength >= 0 {
			total := resp.ContentLength
			var r0, r1 int64
			ok := false
			switch {
			case suffix > 0:
				if suffix < total {
					r0, r1, ok = total-suffix, total-1, true
				}
			case reqStart < total:
				r0, r1 = reqStart, reqEnd
				if r1 >= total {
					r1 = total - 1
				}
				ok = true
			}
			if ok {
				limit = r1 - r0 + 1
				status = http.StatusPartialContent
				h = h.Clone()
				h.Set("Content-Length", strconv.FormatInt(limit, 10))
				h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", r0, r1, total))
				videoRewriteCount.Add(1)
				log.Printf("[twimg-stream] range-200->206 url=%s range=%d-%d total=%d",
					c.Request.URL.Path, r0, r1, total)
			}
		}

		out := make(http.Header, len(h))
		copyNonHop(h, out)
		if limit >= 0 {
			out.Set("Content-Length", strconv.FormatInt(limit, 10))
		}

		wh := c.Writer.Header()
		for k, vs := range out {
			wh[k] = vs
		}
		c.Writer.WriteHeader(status)

		// 流式拷贝：每轮先查客户端是否断开；读到钳制上限立即停，不再碰上游
		buf := make([]byte, 32*1024)
		flusher, _ := c.Writer.(http.Flusher)
		aborted := false
		var copied int64
		for {
			if limit >= 0 && copied >= limit {
				break
			}
			select {
			case <-ctx.Done():
				aborted = true
			default:
			}
			if aborted {
				break
			}
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				if limit >= 0 && copied+int64(n) > limit {
					n = int(limit - copied)
				}
				wn, werr := c.Writer.Write(buf[:n])
				if werr != nil {
					aborted = true
					break
				}
				copied += int64(wn)
				if flusher != nil {
					flusher.Flush()
				}
			}
			if rerr != nil {
				break
			}
		}

		if aborted || ctx.Err() != nil {
			videoAbortedCount.Add(1)
			log.Printf("[twimg-stream] aborted url=%s got=%d/%d total=%d",
				c.Request.URL.Path, copied, limit, resp.ContentLength)
			// 显式关闭响应体并尝试终止上游连接
			closeResponseBody(resp)
			c.Abort()
			return
		}
	}
}
