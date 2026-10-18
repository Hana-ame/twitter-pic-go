package twimg

import (
	"context"
	"log"
	"sync"

	"github.com/gin-gonic/gin"
)

// videoGate 视频限速池（按每 IP 视频累计下载量判定）：
//
//   - 配额：perIPQuota 为每个 IP 的视频累计下载字节上限。
//     按进程存活期累计，每日不清零，进程重启才清空。
//   - 判定：某 IP 累计下载视频 >= 配额后，该 IP 永久进入限速池（一直有效）。
//   - 限速池：池内所有 IP 共享 1 个服务槽（poolSlots 容量 1）——
//     「限速池就一个服务进程」：同一时刻整个池只服务 1 路视频内容，
//     其余请求挂起等待；排队期间**不发任何响应**（HTTP 层保持连接不写
//     字节），由外层 listener 的 TCP keepalive 保活；客户端断开即放弃；
//     等槽期间客户端还活着就照常服务（200/206，Range 语义不变）。
//   - 速度不限：不做任何字节级限速；池内请求也不再走 302 分流（不烧镜像配额）。
//   - 无全局限制：配额只看单 IP。
//
// （背景：用户要求「每 IP 10GB 配额，超限进限速池，池内同时只服务 1 个视频、
// hangup+keepalive 排队、不允许出现任何 429、无全局设限」。
// 下载量统计来自 countingResponseWriter 在流结束后报给 release。
// 测试见 TestVideoGate / TestTwimgV2VideoGateE2E）
type videoGate struct {
	mu         sync.Mutex
	perIPQuota int64               // 每 IP 配额（字节）；0=关闭
	perIP      map[string]int64    // 各 IP 累计服务视频字节（进程存活期累计）
	pool       map[string]struct{} // 限速池成员（永久，重启清空）
	poolSlots  chan struct{}       // 池内唯一服务槽（容量 1）
}

// acquire 视频请求准入。返回 release(本路服务的字节数) 与是否池内。
// 返回 nil release 表示客户端在排队期间已断开（ctx 取消），调用方应直接终止。
func (g *videoGate) acquire(ctx context.Context, ip string) (release func(bytes int64), pooled bool) {
	g.mu.Lock()
	if _, inPool := g.pool[ip]; !inPool {
		if g.perIPQuota <= 0 || g.perIP[ip] < g.perIPQuota {
			// 配额内：直接放行，不排队
			g.mu.Unlock()
			return func(n int64) { g.release(ip, n, false) }, false
		}
		g.pool[ip] = struct{}{}
		log.Printf("[twimg-v2] video ip %s 视频累计下载超配额，已进限速池（池内同时只服务 1 路视频）", ip)
	}
	g.mu.Unlock()

	// 池内：挂起等唯一服务槽。等待期间不写任何响应字节（keepalive 保活），
	// 客户端断开（ctx 取消）立即放弃
	select {
	case g.poolSlots <- struct{}{}:
	case <-ctx.Done():
		return nil, true
	}
	return func(n int64) { g.release(ip, n, true) }, true
}

// release 累计本路服务的字节到该 IP 配额；累计达到配额当场进池（永久）；
// 池内流同时释放唯一服务槽
func (g *videoGate) release(ip string, bytes int64, pooled bool) {
	g.mu.Lock()
	if bytes > 0 {
		g.perIP[ip] += bytes
		if _, inPool := g.pool[ip]; !inPool && g.perIPQuota > 0 && g.perIP[ip] >= g.perIPQuota {
			g.pool[ip] = struct{}{}
			log.Printf("[twimg-v2] video ip %s 视频累计下载达配额，已进限速池（池内同时只服务 1 路视频）", ip)
		}
	}
	g.mu.Unlock()
	if pooled {
		<-g.poolSlots
	}
}

// countingResponseWriter 包一层 gin.ResponseWriter 统计下发的 body 字节数。
// 嵌入转发 Header/WriteHeader/Flush/Hijack 等，流式与 Range 语义不受影响，
// 只多统计 Write/WriteString 的实际字节。
// （背景：gate 按下载量计配额，需要在 StreamVideoProxy 流式拷贝外层数 body）
type countingResponseWriter struct {
	gin.ResponseWriter
	n *int64
}

func (w *countingResponseWriter) Write(b []byte) (int, error) {
	m, err := w.ResponseWriter.Write(b)
	*w.n += int64(m)
	return m, err
}

func (w *countingResponseWriter) WriteString(s string) (int, error) {
	m, err := w.ResponseWriter.WriteString(s)
	*w.n += int64(m)
	return m, err
}
