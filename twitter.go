package twitter

import (
	"fmt"
	"net"
)

// 问python做的。
func curlMetaData(username string) (string, error) {
	// output, err := tools.Command(os.Getenv("TWITTER_DIR"), "/home/lumin/miniconda3/bin/py", "caller.py", username)

	// 连接到 127.25.9.19:8080, 发送username
	conn, err := net.Dial("tcp", "127.25.9.19:8080")
	if err != nil {
		return fmt.Sprintf("无法连接到服务器: %v", err), err
	}
	defer conn.Close()

	_, err = conn.Write([]byte(username))
	if err != nil {
		return fmt.Sprintf("未写入: %v", err), err
	}

	return "done", nil
}

func getList(list, after string) ([]string, error) {
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

// 2026.01.01
// 显示tag的新功能
func getListV2(list, after string) ([]string, error) {
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

func getSearch(by, search string) ([]string, error) {
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

// 2026.01.01
// 显示tag的新功能
func getSearchV2(by, search string) ([]string, error) {
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
