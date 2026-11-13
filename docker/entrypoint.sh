#!/usr/bin/env bash
# 容器入口：同时托管 SRS 与控制面，任一异常退出即整体退出（由 docker restart 策略接管）。
set -uo pipefail

DATA_DIR="${DATA_DIR:-/data}"
export DATA_DIR

mkdir -p "$DATA_DIR/recordings" "$DATA_DIR/logs" "$DATA_DIR/assets"

log() { echo "[entrypoint] $*"; }

# ---------- 准备运行配置 ----------
cd /usr/local/srs || exit 1

# SRS 启动时会校验 max_connections 与文件句柄上限（约 connections*1.128+128 <= nofile）。
# 这里先尽量抬高软限制，再按实际 nofile 生成运行配置，避免默认 ulimit 太低时启动失败。
SOFT_LIMIT=$(ulimit -n)
HARD_LIMIT=$(ulimit -Hn)
if [ "$HARD_LIMIT" != "unlimited" ] && [ "$SOFT_LIMIT" != "unlimited" ] && [ "$HARD_LIMIT" -gt "$SOFT_LIMIT" ] 2>/dev/null; then
  ulimit -n "$HARD_LIMIT" 2>/dev/null || true
fi
NOFILE=$(ulimit -n)
MAXCONN=10000
if [ "$NOFILE" != "unlimited" ]; then
  LIMIT=$(( (NOFILE - 200) * 8 / 9 ))
  if [ "$LIMIT" -lt "$MAXCONN" ]; then MAXCONN=$LIMIT; fi
fi
if [ "$MAXCONN" -lt 100 ]; then MAXCONN=100; fi
sed "s/^max_connections .*/max_connections     $MAXCONN;/" conf/srs.conf > /tmp/srs.runtime.conf
log "文件句柄上限=$NOFILE，max_connections=$MAXCONN"

# ---------- 启动 SRS ----------
log "启动 SRS (rtmp 1935 / api 1985 / http 8080)"
./objs/srs -c /tmp/srs.runtime.conf &
SRS_PID=$!

# 等 SRS HTTP API 就绪，最多 30 秒
for _ in $(seq 1 150); do
  if curl -fsS -o /dev/null "http://127.0.0.1:1985/api/v1/versions" 2>/dev/null; then
    log "SRS 已就绪"
    break
  fi
  if ! kill -0 "$SRS_PID" 2>/dev/null; then
    log "SRS 进程已退出，停止容器"
    exit 1
  fi
  sleep 0.2
done

# ---------- 启动控制面 ----------
log "启动控制面 (listen ${LISTEN:-:1986})"
/usr/local/bin/srs-control &
CTL_PID=$!

term() {
  log "收到退出信号，正在优雅停止…"
  kill -TERM "$CTL_PID" 2>/dev/null
  # 等控制面收尾（停止 ffmpeg、收尾录像）
  for _ in $(seq 1 60); do
    kill -0 "$CTL_PID" 2>/dev/null || break
    sleep 0.2
  done
  kill -TERM "$SRS_PID" 2>/dev/null
  wait "$CTL_PID" 2>/dev/null
  wait "$SRS_PID" 2>/dev/null
  exit 0
}
trap term TERM INT

# 任一进程退出则整体退出
wait -n "$SRS_PID" "$CTL_PID"
CODE=$?
log "子进程退出 (code=$CODE)，容器将停止"
kill -TERM "$CTL_PID" "$SRS_PID" 2>/dev/null
wait 2>/dev/null
exit "$CODE"
