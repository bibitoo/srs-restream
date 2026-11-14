#!/usr/bin/env bash
# 控制面入口（无构建版）：直接用包内预编译的静态二进制，按机器架构选一个跑。
# 依赖基础镜像里已装好的 ffmpeg（录制/混流/转推都靠它）。
set -uo pipefail

DATA_DIR="${DATA_DIR:-/data}"
mkdir -p "$DATA_DIR/recordings" "$DATA_DIR/logs" "$DATA_DIR/assets" "$DATA_DIR/hls"

log() { echo "[control] $*"; }

ARCH="$(uname -m)"
case "$ARCH" in
  x86_64|amd64) BIN="/app/bin/srs-control-linux-amd64" ;;
  aarch64|arm64) BIN="/app/bin/srs-control-linux-arm64" ;;
  *) log "不支持的架构：$ARCH"; exit 1 ;;
esac
[ -f "$BIN" ] || { log "找不到控制面二进制：$BIN"; exit 1; }

# 包内文件可能是只读挂载，先复制到可写区再赋执行位
cp -f "$BIN" /tmp/srs-control
chmod +x /tmp/srs-control

if ! command -v ffmpeg >/dev/null 2>&1; then
  log "警告：镜像里没有 ffmpeg，录制/混流/转推会失败"
fi

log "架构=$ARCH  二进制=$(basename "$BIN")"
log "SRS API   : ${SRS_API:-http://srs:1985}"
log "推流对外  : ${PUBLIC_RTMP:-（未设置，界面按访问地址推断）}"
log "监听      : ${LISTEN:-:1986}"

# SRS 可能还在启动，控制面自身会重试，这里只等一小会儿让首屏数据更完整
for _ in $(seq 1 20); do
  if curl -fsS -o /dev/null "${SRS_API:-http://srs:1985}/api/v1/versions" 2>/dev/null; then
    log "SRS 已就绪"; break
  fi
  sleep 0.5
done

exec /tmp/srs-control
