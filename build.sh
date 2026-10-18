#!/usr/bin/env bash
set -euo pipefail

# ==============================================================================
# twitter-pic-go 构建与部署脚本
#
# 用法：
#   ./build.sh              # 完整构建并部署：编译统一单端口服务二进制 + 同步前端资源 + 同步爬虫脚本
#   ./build.sh --frontend   # 仅热更前端（零停机、免编译 Go）：仅同步 gallery/templates 与 gallery/static
#   ./build.sh --build-only # 仅在本地编译二进制（./server/server），不执行远端上传
# ==============================================================================

REMOTE_HOST="${REMOTE_HOST:-root@vps.moonchan.xyz}"
REMOTE_DIR="${REMOTE_DIR:-~/twitter}"
SCP_CMD="${SCP_CMD:-$HOME/script/scp.sh}"

MODE="full"
if [[ "${1:-}" == "--frontend" || "${1:-}" == "-f" ]]; then
  MODE="frontend"
elif [[ "${1:-}" == "--build-only" || "${1:-}" == "-b" ]]; then
  MODE="build-only"
fi

if [[ "$MODE" == "frontend" ]]; then
  echo "[deploy] 正在执行 Gallery 前端热更新（免编译 Go 二进制，服务端磁盘监控自动生效）..."
  "$SCP_CMD" ./gallery/templates "$REMOTE_HOST:$REMOTE_DIR/gallery/"
  "$SCP_CMD" ./gallery/static "$REMOTE_HOST:$REMOTE_DIR/gallery/"
  echo "[deploy] 前端热更新完成: $(date)"
  exit 0
fi

echo "[build] 正在编译统一单端口服务 (API + Gallery SSR)..."
(
  cd server
  go build -o server .
)
echo "[build] 编译完成: ./server/server"

if [[ "$MODE" == "build-only" ]]; then
  exit 0
fi

echo "[deploy] 正在上传二进制、前端模板/静态资源与 Python 抓取脚本..."
"$SCP_CMD" ./server/server "$REMOTE_HOST:$REMOTE_DIR/temp"
if [[ -f ./.env ]]; then
  "$SCP_CMD" ./.env "$REMOTE_HOST:$REMOTE_DIR"
fi
"$SCP_CMD" ./crawler/get_meta_data.py "$REMOTE_HOST:$REMOTE_DIR"
# get2.py 是线上主力抓取脚本（带 CT0 / 源 IP 绑定）
"$SCP_CMD" ./crawler/get2.py "$REMOTE_HOST:$REMOTE_DIR"
"$SCP_CMD" ./crawler/caller.py "$REMOTE_HOST:$REMOTE_DIR"
"$SCP_CMD" ./bans.txt "$REMOTE_HOST:$REMOTE_DIR"

# 同步前端目录以支持线上直接修改或后续 --frontend 免编译热更新
"$SCP_CMD" ./gallery/templates "$REMOTE_HOST:$REMOTE_DIR/gallery/" || true
"$SCP_CMD" ./gallery/static "$REMOTE_HOST:$REMOTE_DIR/gallery/" || true

date
