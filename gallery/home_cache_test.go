package gallery

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 首页缓存的并发/失效语义测试。
//
// 时间语义一律用 10ms 级 TTL + 轮询等待，不用固定 sleep 猜后台重建何时完成
// ——重建是后台 goroutine，"已经发生"只能轮询确认（SOP-G2 的期望值对照）。

// waitFor 轮询等待条件成立；超时即 Fatal（并说明等的是什么）。
func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("超时等不到：%s", what)
}

// 冷启动并发：N 个请求同时进来，构造只做一次（single-flight），
// 其余请求等第一个构造完直接复用同一份结果。
func TestHomeCacheColdStartSingleFlight(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	c := newHomeCache(time.Minute, func() ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		<-release // 卡住第一次构造，让其余请求堆在 buildMu 上
		return []byte("v1"), nil
	})

	const n = 16
	var wg sync.WaitGroup
	bodies := make([][]byte, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bodies[i], errs[i] = c.html()
		}(i)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&calls) >= 1 }, "第一次构造开始")
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("冷启动并发构造次数 = %d，期望 1（single-flight）", got)
	}
	for i := range bodies {
		if errs[i] != nil || string(bodies[i]) != "v1" {
			t.Fatalf("第 %d 个请求拿到 %q err=%v，期望 v1/nil", i, bodies[i], errs[i])
		}
	}
}

// TTL 内命中缓存，不重复构造。
func TestHomeCacheHitWithinTTL(t *testing.T) {
	var calls int32
	c := newHomeCache(time.Minute, func() ([]byte, error) {
		n := atomic.AddInt32(&calls, 1)
		return []byte(fmt.Sprintf("v%d", n)), nil
	})
	for i := 0; i < 5; i++ {
		b, err := c.html()
		if err != nil || string(b) != "v1" {
			t.Fatalf("第 %d 次 html() = %q err=%v，期望 v1（TTL 内不该重建）", i, b, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("TTL 内构造次数 = %d，期望 1", got)
	}
}

// 过期后先发旧页（stale-while-revalidate，访客不等），后台补新后转为新页。
func TestHomeCacheServesStaleWhileRevalidate(t *testing.T) {
	var calls int32
	c := newHomeCache(10*time.Millisecond, func() ([]byte, error) {
		n := atomic.AddInt32(&calls, 1)
		return []byte(fmt.Sprintf("v%d", n)), nil
	})
	if b, err := c.html(); err != nil || string(b) != "v1" {
		t.Fatalf("首次 html() = %q err=%v，期望 v1/nil", b, err)
	}
	time.Sleep(20 * time.Millisecond) // 越过 TTL

	b, err := c.html()
	if err != nil || string(b) != "v1" {
		t.Fatalf("过期后应立刻发旧页，实得 %q err=%v", b, err)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&calls) >= 2 }, "后台重建被触发")
	waitFor(t, func() bool {
		b, err := c.html()
		return err == nil && string(b) == "v2"
	}, "后台重建结果生效")
}

// invalidate（标签写入后调用）只标脏：旧页立刻可发，重建在后台。
func TestHomeCacheInvalidateKeepsServingOldPage(t *testing.T) {
	var calls int32
	c := newHomeCache(time.Hour, func() ([]byte, error) {
		n := atomic.AddInt32(&calls, 1)
		return []byte(fmt.Sprintf("v%d", n)), nil
	})
	if b, err := c.html(); err != nil || string(b) != "v1" {
		t.Fatalf("首次 html() = %q err=%v，期望 v1/nil", b, err)
	}
	c.invalidate()

	b, err := c.html()
	if err != nil || string(b) != "v1" {
		t.Fatalf("标脏后应立刻发旧页，实得 %q err=%v", b, err)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&calls) >= 2 }, "标脏触发后台重建")
	waitFor(t, func() bool {
		b, err := c.html()
		return err == nil && string(b) == "v2"
	}, "标脏后的新页生效")
}

// 构造失败：保留旧页继续服务，且 busy 复位后还能再试。
func TestHomeCacheKeepsOldPageOnBuildError(t *testing.T) {
	var calls int32
	var fail atomic.Bool
	c := newHomeCache(10*time.Millisecond, func() ([]byte, error) {
		n := atomic.AddInt32(&calls, 1)
		if fail.Load() {
			return nil, errors.New("boom")
		}
		return []byte(fmt.Sprintf("v%d", n)), nil
	})
	if b, err := c.html(); err != nil || string(b) != "v1" {
		t.Fatalf("首次 html() = %q err=%v，期望 v1/nil", b, err)
	}

	fail.Store(true)
	c.invalidate()
	time.Sleep(20 * time.Millisecond)
	if b, err := c.html(); err != nil || string(b) != "v1" {
		t.Fatalf("过期后应先发旧页，实得 %q err=%v", b, err)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&calls) >= 2 }, "失败的那次后台重建")

	// 失败不写缓存：旧页还在，且下一次请求仍会再试（busy 已复位）。
	fail.Store(false)
	time.Sleep(20 * time.Millisecond)
	if b, err := c.html(); err != nil || string(b) != "v1" {
		t.Fatalf("失败后应继续发旧页，实得 %q err=%v", b, err)
	}
	waitFor(t, func() bool {
		b, err := c.html()
		return err == nil && string(b) == "v3"
	}, "失败后的重试成功并生效")
}

// TTL <= 0 表示关闭缓存：newHomeCache 返回 nil，调用方退回每次现构造。
func TestHomeCacheDisabledByZeroTTL(t *testing.T) {
	c := newHomeCache(0, func() ([]byte, error) { return []byte("v1"), nil })
	if c != nil {
		t.Fatalf("TTL=0 应关闭缓存（返回 nil），实得 %#v", c)
	}
}
