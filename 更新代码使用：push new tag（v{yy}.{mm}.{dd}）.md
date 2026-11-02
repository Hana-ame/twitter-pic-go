# 更新代码：push new tag（v{yy}.{mm}.{dd}）

twitter-pic-go 的发布链路是 **推 tag → CI 构建二进制 → bwh 自更新脚本拉取部署**。
代码改完不要直接改线上二进制，走这条正典路径。

## tag 命名

```
v{yy}.{mm}.{dd}
```

- 两位年份、两位月份、两位日期，按当天日期取。
- 例：2026-11-03 → `v26.11.03`
- 同一天多次发布：追加序号 `v26.11.03.2` 或换 `v26.11.03a`（看历史习惯，本仓此前都是 `v26.10.25` 单段）。

## 流程

```bash
cd /mnt/d/WorkPlace/twitter-pic/go

# 1. 改完、本地 go test ./gallery/ 过（只测受影响包；全量 ./... 在本地可能因 twimg 网络测试假阴）
# 2. 提交
git add -A
git commit -m "perf(gallery): ..."

# 3. 推 master
git push origin master

# 4. 打当天 tag 并推（触发 .github/workflows/release.yml）
git tag v26.11.03
git push origin v26.11.03
```

CI（`release.yml`）在 ubuntu-latest 上 `go test ./...` + `CGO_ENABLED=0 go build ./server/`，产出资产 `twitter-linux-amd64` 并创建 release。看状态：

```bash
gh run list --limit 4        # Test + Build and Release 两条都要 success
gh release view v26.11.03    # 确认 asset: twitter-linux-amd64
```

## 部署到 bwh

bwh 由 systemd `twitter.service` 托管，`/usr/local/bin/twitter-pic-go-update.sh` 是正典部署脚本（开机自更新也是它）：查 `releases/latest` → `download_asset.py` 下 `.tmp` → 对 GitHub digest 做 sha256 校验 → 备份 `twitter.bin.bak.<ts>` → `mv` 原子换入 + `chmod +x` → 写标记 → `systemctl --no-block restart twitter`。

```bash
# 本地经 SOCKS 代理 ssh 到 bwh（端口 26275）
~/script/ssh/bwh.sh '/usr/local/bin/twitter-pic-go-update.sh'
```

输出关键行：
```
latest release: v26.11.03 (current marker: v26.10.25)
sha256 verified: <hash>
backup: /root/twitter/twitter.bin.bak.<ts>
deployed: v26.11.03 (sha256 <hash>)
twitter.service restart queued (--no-block)
```

## ⚠️ 前端文件改动必须额外同步磁盘

bwh `/root/twitter/gallery/`（`templates/` + `static/`）有一份磁盘前端副本，`gallery/assets.go` 双模加载器在磁盘文件存在时**覆盖 `//go:embed` 内嵌资源**。所以：

> **只要这次 commit 碰了 `gallery/templates/*.html` 或 `gallery/static/*`，换完二进制后必须再跑一次前端热更，否则线上前端是旧的。**

```bash
# 本地 build.sh --frontend，SCP_CMD 要带 bwh 端口 26275（默认脚本是 vps，端口 22）
cd /mnt/d/WorkPlace/twitter-pic/go
REMOTE_HOST=root@bwh.moonchan.xyz REMOTE_DIR=/root/twitter SCP_CMD=<带-p26275的scp包装> ./build.sh --frontend
```

`build.sh` 的 `SCP_CMD` 默认 `~/script/scp.sh`（无 `-P`、目标是 vps），对 bwh 不通。可临时造个包装：

```bash
cat > /tmp/scp-bwh.sh <<'EOF'
#!/usr/bin/env bash
source ~/script/env.source >/dev/null 2>&1
exec scp -r -P 26275 -o "ProxyCommand=nc -X5 -x $SOCKS_PROXY %h %p" -o ServerAliveInterval=30 "$@"
EOF
chmod +x /tmp/scp-bwh.sh
REMOTE_HOST=root@bwh.moonchan.xyz REMOTE_DIR=/root/twitter SCP_CMD=/tmp/scp-bwh.sh ./build.sh --frontend
```

> 注：`build.sh --frontend` 内部 `scp` 不带 `-r`，传目录会报 `not a regular file`——包装里必须带 `-r`（上面的脚本已带）。

没碰前端文件就跳过这步。

## 验证（线上）

```bash
# 服务起来 + 二进制版本对
~/script/ssh/bwh.sh 'systemctl is-active twitter; sha256sum /root/twitter/twitter.bin; go version -m /root/twitter/twitter.bin | grep vcs.revision'
# 健康检查
curl -s -o /dev/null -w "%{http_code}\n" https://xx.4545810.xyz/healthz   # 200
# 抓首页 HTML 确认前端真换了（看新代码字符串、卡片数）
curl -s https://xx.4545810.xyz/ | grep -o 'const STEP = [0-9]*'
```

浏览器冷测（清 localStorage）数首屏请求：

```js
performance.getEntriesByType('resource').filter(n=>n.includes('/api/twitter/')).length   // 预览请求数
document.querySelectorAll('.acct').length                                                 // 卡片数
document.querySelectorAll('.acct img').length                                             // 图数
```

## 回滚

```bash
~/script/ssh/bwh.sh 'cd /root/twitter && cp twitter.bin.bak.<ts> twitter.bin && chmod +x twitter.bin && systemctl restart twitter'
```

标记文件回写：部署脚本把版本写进某个 marker（`current marker` 那行的来源）；回滚旧二进制后建议手动改回旧 tag，避免下次开机自更新又跳回新版。

## 现场参考

2026-11-03 `v26.11.03`（commit `d2104bd`）：gallery 首屏懒加载。CI run 41494276664（Build and Release）+ 41493973538（Test）双绿；bwh 部署 sha256 `97c1252b…`；`go version -m` = `vcs.revision=d2104bd`。前端因磁盘副本覆盖内嵌，补跑 `--frontend` 才生效。
