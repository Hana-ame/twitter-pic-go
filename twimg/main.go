package twimg

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

func Run(addr string) {
	// 1. 配置目标地址
	target := "https://pbs.twimg.com"
	targetURL, err := url.Parse(target)
	if err != nil {
		log.Fatal("Invalid target URL:", err)
	}

	// 2. 创建反向代理
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// 3. 关键步骤：修改 Request 的 Host 头
	// 标准库的 ReverseProxy 默认只修改 URL Scheme 和 Host，
	// 但 Header 里的 Host 还是客户端请求时的 (比如 localhost:8080)。
	// 必须重写，否则目标服务器可能会拒绝。
	originalDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		// NOTE: 是将配置写到req当中
		originalDirector(req)

		// 强制将 Host 头设置为目标的 Host
		req.Host = targetURL.Host

		// 如果需要，可以在这里设置 User-Agent 防止被目标简单的反爬
		// req.Header.Set("User-Agent", "Mozilla/5.0...")
	}

	// 4. 启动服务
	// httputil.ReverseProxy 实现了 http.Handler 接口，可以直接传给 ListenAndServe
	log.Printf("Proxy server running on :8080 -> %s", target)

	// 这里直接把 proxy 作为 handler，意味着所有请求都会进这个代理
	if err := http.ListenAndServe(addr, proxy); err != nil {
		log.Fatal(err)
	}
}
