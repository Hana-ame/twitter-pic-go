package twimg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestSingleRange(t *testing.T) {
	cases := []struct {
		rng     string
		start   int64
		end     int64
		suffix  int64
		bounded bool
	}{
		{"bytes=0-1023", 0, 1023, 0, true},
		{"bytes=512-", 512, 0, 0, true},
		{"bytes=-100", 0, 0, 100, true},
		{"bytes=100-50", 0, 0, 0, false},
		{"bytes=0-1,5-9", 0, 0, 0, false},
		{"Range: bytes=0-1", 0, 0, 0, false},
		{"", 0, 0, 0, false},
		{"bytes=-0", 0, 0, 0, false},
	}
	for _, c := range cases {
		s, e, su, b := singleRange(c.rng)
		if s != c.start || e != c.end || su != c.suffix || b != c.bounded {
			t.Errorf("singleRange(%q) = (%d,%d,%d,%v), want (%d,%d,%d,%v)",
				c.rng, s, e, su, b, c.start, c.end, c.suffix, c.bounded)
		}
	}
}

// countWriter 统计实际写出的字节数（用于验证上游没有被多拉）
type countWriter struct {
	w io.Writer
	n *int64
}

func (cw *countWriter) Write(p []byte) (int, error) {
	m, err := cw.w.Write(p)
	atomic.AddInt64(cw.n, int64(m))
	return m, err
}

// newVideoUpstream 模拟 video.twimg.com 上游：
//   - misbehave=true：无视 Range，永远返回 200 全量
//   - honorRange=true：正常返回 206 分段
//   - blockAfter 写满该字节数后阻塞，等待 release 通道（abort 测试用）
func newVideoUpstream(t *testing.T, size int64, misbehave, honorRange bool,
	pulled *int64, blockAfter int64, release chan struct{}) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cw := &countWriter{w: w, n: pulled}
		var start, end int64
		gotRange := false
		if !misbehave && honorRange {
			if rng := r.Header.Get("Range"); rng != "" {
				s0, e0, _, ok := singleRange(rng)
				if ok && e0 >= s0 {
					start, end, gotRange = s0, e0, true
				}
			}
		}
		if gotRange {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			written := int64(0)
			total := end - start + 1
			for written < total {
				chunk := int64(64 * 1024)
				if total-written < chunk {
					chunk = total - written
				}
				n, err := cw.Write(make([]byte, chunk))
				written += int64(n)
				if err != nil {
					return
				}
			}
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		written := int64(0)
		for written < size {
			chunk := int64(1024 * 1024)
			if size-written < chunk {
				chunk = size - written
			}
			if blockAfter > 0 && written >= blockAfter {
				if release != nil {
					<-release
				}
			}
			n, err := cw.Write(make([]byte, chunk))
			written += int64(n)
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func newVideoHandler(upstream string) http.HandlerFunc {
	r := gin.New()
	gin.SetMode(gin.TestMode)
	r.GET("/*any", StreamVideoProxy(upstream, nil))
	r.HEAD("/*any", StreamVideoProxy(upstream, nil))
	return r.Handler().ServeHTTP
}

func TestVideoProxyRangeCap(t *testing.T) {
	const size int64 = 64 << 20 // 64MB 全量
	var pulled int64
	upstream := newVideoUpstream(t, size, true, false, &pulled, 0, nil)
	h := newVideoHandler(upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/amplify_video/x.mp4", nil)
	req.Header.Set("Range", "bytes=0-1023")

	h(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206 (上游无视 Range 需被改写)", rec.Code)
	}
	if rec.Body.Len() != 1024 {
		t.Fatalf("body len = %d, want 1024", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != "1024" {
		t.Errorf("Content-Length = %q, want 1024", got)
	}
	if got := rec.Header().Get("Content-Range"); got != fmt.Sprintf("bytes 0-1023/%d", size) {
		t.Errorf("Content-Range = %q, want bytes 0-1023/%d", got, size)
	}
	// 上游只应该被读走 ~1KB + socket 缓冲，绝不能是 64MB
	if pulled > 16<<20 {
		t.Errorf("上游被拉走 %d bytes（请求只要 1024），Range 钳制失效", pulled)
	}
}

func TestVideoProxy206Passthrough(t *testing.T) {
	var pulled int64
	upstream := newVideoUpstream(t, 10<<20, false, true, &pulled, 0, nil)
	h := newVideoHandler(upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/tweet_video/y.mp4", nil)
	req.Header.Set("Range", "bytes=512-1023")

	h(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if rec.Body.Len() != 512 {
		t.Fatalf("body len = %d, want 512", rec.Body.Len())
	}
	if pulled != 512 {
		t.Errorf("上游被拉走 %d bytes, want 512", pulled)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 512-1023/10485760" {
		t.Errorf("Content-Range = %q", got)
	}
}

// abortRW 可编程断连的 ResponseWriter
type abortRW struct {
	header http.Header
	mu     sync.Mutex
	fail   bool
	buf    []byte
	status int
}

func (w *abortRW) Header() http.Header { return w.header }
func (w *abortRW) WriteHeader(s int)   { w.status = s }
func (w *abortRW) Flush()              {}
func (w *abortRW) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fail {
		return 0, errors.New("client disconnected")
	}
	w.buf = append(w.buf, p...)
	return len(p), nil
}
func (w *abortRW) copied() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return int64(len(w.buf))
}

func TestVideoProxyAbort(t *testing.T) {
	before := videoAbortedCount.Load()
	var pulled int64
	release := make(chan struct{})
	// 上游写满 2MB 后阻塞 —— 客户端此时中断
	upstream := newVideoUpstream(t, 512<<20, true, false, &pulled, 2<<20, release)
	h := newVideoHandler(upstream.URL)

	rw := &abortRW{header: make(http.Header), status: 200}
	req := httptest.NewRequest("GET", "/amplify_video/abort.mp4", nil)
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	req = req.WithContext(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		h(rw, req)
	}()

	// 等客户端先拿到数据，然后模拟断开
	deadline := time.Now().Add(3 * time.Second)
	for rw.copied() < 1<<20 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("客户端断开后 handler 3 秒未退出")
	}
	close(release)

	if videoAbortedCount.Load() != before+1 {
		t.Errorf("aborted 计数未增加: %d -> %d", before, videoAbortedCount.Load())
	}
	// 上游必须被掐住：512MB 的文件只被拉走 2MB 出头
	if pulled > 8<<20 {
		t.Errorf("客户端断开后上游仍被拉走 %d bytes", pulled)
	}
	if rw.copied() == 0 {
		t.Errorf("客户端一行数据都没收到")
	}
}

func TestVideoProxyHead(t *testing.T) {
	var pulled int64
	upstream := newVideoUpstream(t, 10<<20, true, false, &pulled, 0, nil)
	h := newVideoHandler(upstream.URL)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("HEAD", "/amplify_video/h.mp4", nil)
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD 不应有 body, got %d", rec.Body.Len())
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.FormatInt(10<<20, 10) {
		t.Errorf("HEAD Content-Length = %q, want %d", got, 10<<20)
	}
}
