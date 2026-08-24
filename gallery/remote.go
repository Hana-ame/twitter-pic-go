package gallery

// 远程 JSON 源（临时功能，仅供本地调试）。
//
// 设置环境变量 GALLERY_REMOTE_JSON_BASE（如 https://x.moonchan.xyz/api/twitter）后，
// 当访问的账号没有本地数据时，才按需从远端 API 拉取该账号的元数据，
// 直接保存在内存中（g.remoteDocs），绝不落盘；随后走原有扫描逻辑建索引。
//
// 不设置该环境变量时本文件完全不起作用；原有目录扫描逻辑零改动。

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// SetRemoteSource 注入远程源地址，必须在首次 Scan() 之前调用。
func (g *Gallery) SetRemoteSource(base string) {
	g.remoteBase = strings.TrimRight(base, "/")
}

// RemoteBase 返回远程源地址（空串表示未启用）。
func (g *Gallery) RemoteBase() string { return g.remoteBase }

var remoteHTTPClient = &http.Client{Timeout: 60 * time.Second}

// FetchRemoteDoc 按需拉取某账号的元数据，仅存内存（g.remoteDocs），不写磁盘。
// 已在内存中的直接跳过。返回是否新拉取了数据。
func (g *Gallery) FetchRemoteDoc(username string) (bool, error) {
	if g.remoteBase == "" || username == "" {
		return false, fmt.Errorf("remote json source disabled")
	}
	if strings.ContainsAny(username, "/\\") || username == "." || username == ".." {
		return false, fmt.Errorf("invalid username")
	}
	g.muRemote.Lock()
	defer g.muRemote.Unlock()
	if _, ok := g.remoteDocs[username]; ok {
		return false, nil // 内存里已有
	}

	u := g.remoteBase + "/" + url.PathEscape(username) + ".json.gz?t=1"
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("User-Agent", "twitter-pic-gallery-debug/0.1")

	resp, err := remoteHTTPClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("http %s", resp.Status)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return false, err
	}

	// 先统一解成明文 JSON 再校验（服务端可能回 gzip 或明文两种形态）
	plain := raw
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return false, err
		}
		plain, err = io.ReadAll(io.LimitReader(zr, 64<<20))
		if err != nil {
			return false, err
		}
	}

	// 结构校验：能解出 timeline 才算有效数据（防止 banned.json 之类被收进索引）
	var probe gzDocument
	if err := json.Unmarshal(plain, &probe); err != nil {
		return false, fmt.Errorf("not a timeline json: %v", err)
	}
	if len(probe.Timeline) == 0 {
		return false, fmt.Errorf("empty timeline for %s", username)
	}

	// 统一压成 gzip 存内存，peekRemoteDoc 无需感知两种形态
	rz, err := normalizeGzip(plain)
	if err != nil {
		return false, err
	}
	g.remoteDocs[username] = rz
	return true, nil
}

// normalizeGzip 统一为 gzip 字节流：已是 gzip 原样返回；明文 JSON 则压成 gzip。
func normalizeGzip(raw []byte) ([]byte, error) {
	if len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b {
		return raw, nil
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// peekRemoteDoc 解出内存中的文档（供扫描用）；不存在返回 nil。
func (g *Gallery) peekRemoteDoc(username string) *gzDocument {
	g.muRemote.Lock()
	defer g.muRemote.Unlock()
	raw, ok := g.remoteDocs[username]
	if !ok {
		return nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil
	}
	defer zr.Close()
	var doc gzDocument
	if err := json.NewDecoder(zr).Decode(&doc); err != nil {
		return nil
	}
	return &doc
}
