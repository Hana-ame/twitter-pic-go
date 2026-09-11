package twitter

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// 问python做的。
//
// 协议：向 caller.py 的 TCP listener 发送 username。
// 若双方都设置了 CALLER_TOKEN，第一行先发 "token <CALLER_TOKEN>"，第二行才是
// username（caller.py 侧见 token_ok）。未设置时保持单行协议，两端可独立升级。
func curlMetaData(username string) (string, error) {
	// output, err := tools.Command(os.Getenv("TWITTER_DIR"), "/home/lumin/miniconda3/bin/py", "caller.py", username)

	// 连接到 caller.py（默认 127.25.9.19:8080，可用 CALLER_ADDR 覆盖）
	addr := os.Getenv("CALLER_ADDR")
	if addr == "" {
		addr = "127.25.9.19:8080"
	}
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return fmt.Sprintf("无法连接到服务器: %v", err), err
	}
	defer conn.Close()

	// 写入超时：连接建立后再给一次 deadline，防止对端 accept 后不读也不关。
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return fmt.Sprintf("设置写超时失败: %v", err), err
	}

	payload := username
	if token := strings.TrimSpace(os.Getenv("CALLER_TOKEN")); token != "" {
		payload = "token " + token + "\n" + username
	}

	_, err = conn.Write([]byte(payload))
	if err != nil {
		return fmt.Sprintf("未写入: %v", err), err
	}

	return "done", nil
}

func getList(list, after string) ([]User, error) {
	switch list {
	case "users":
		if after == "" {
			return getUserList()
		} else {
			return getUserListAfter(after)
		}
	}
	// not implemented
	return nil, nil
}

func getSearch(by, search string) ([]User, error) {
	if search == "" {
		return nil, fmt.Errorf("search is empty")
	}
	switch by {
	case "username":
		return getUserListByUsername(search)
	case "nick":
		return getUserListByNick(search)
	}
	// not implemented
	return nil, nil
}
