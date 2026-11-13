#!/usr/bin/env bash
# 控制面入口：只负责启动 srs-control（SRS 在另一个容器里，由 compose 编排）
set -uo pipefail

DATA_DIR="${DATA_DIR:-/data}"
export DATA_DIR
mkdir -p "$DATA_DIR/recordings" "$DATA_DIR/logs" "$DATA_DIR/assets" "$DATA_DIR/hls"

log() { echo "[control] $*"; }

log "SRS API   : ${SRS_API:-http://srs:1985}"
log "SRS HTTP  : ${SRS_HTTP_BASE:-http://srs:8080}"
log "RTMP 内部 : ${RTMP_BASE:-rtmp://srs:1935/}"
log "推流对外 : ${PUBLIC_RTMP:-（未设置，界面按访问地址推断）}"
log "监听      : ${LISTEN:-:1986}"

# SRS 可能还在启动中，控制面自身会重试，这里只等一小会儿让首屏数据更完整
for _ in $(seq 1 20); do
  if curl -fsS -o /dev/null "${SRS_API:-http://srs:1985}/api/v1/versions" 2>/dev/null; then
    log "SRS 已就绪"
    break
  fi
  sleep 0.5
done

exec /usr/local/bin/srs-control
