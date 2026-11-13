#!/usr/bin/env bash
# 端到端冒烟测试：验证 SRS 推流、转推（直转/混流）、录制、录像播放。
#
# 默认在容器内运行（可用环境变量覆盖）：
#   API=http://127.0.0.1:1986  RTMP=rtmp://127.0.0.1:1935
# 在宿主机验证映射到其他端口的容器：
#   API=http://127.0.0.1:1986 RTMP=rtmp://127.0.0.1:1935 \
#   TARGET_RTMP=rtmp://127.0.0.1:1935 ./scripts/smoke-test.sh
set -uo pipefail

API="${API:-http://127.0.0.1:1986}"
RTMP="${RTMP:-rtmp://127.0.0.1:1935}"
# 用 SRS 自身冒充「第三方平台」来接收转推结果，便于自测
TARGET_RTMP="${TARGET_RTMP:-$RTMP}"

PASS=0; FAIL=0
ok()   { PASS=$((PASS+1)); echo "  ✅ $*"; }
bad()  { FAIL=$((FAIL+1)); echo "  ❌ $*"; }
hr()   { echo; echo "== $* =="; }

need() { command -v "$1" >/dev/null 2>&1 || { echo "缺少命令: $1"; exit 1; }; }
need curl; need ffmpeg; need jq

jsonget() { jq -r "$1" 2>/dev/null; }

TMP="$(mktemp -d)"
PIDS=()
cleanup() {
  echo; echo "-- 清理 --"
  for p in "${PIDS[@]:-}"; do kill "$p" 2>/dev/null; done
  for j in "${JOBS[@]:-}"; do curl -s -X POST "$API/api/jobs/$j/stop" >/dev/null 2>&1; curl -s -X DELETE "$API/api/jobs/$j" >/dev/null 2>&1; done
  for t in "${TARGETS[@]:-}"; do curl -s -X DELETE "$API/api/targets/$t" >/dev/null 2>&1; done
  for r in "${RECS[@]:-}"; do curl -s -X POST "$API/api/recordings/$r/stop" >/dev/null 2>&1; curl -s -X DELETE "$API/api/recordings/$r" >/dev/null 2>&1; done
  rm -rf "$TMP"
}
trap cleanup EXIT
JOBS=(); TARGETS=(); RECS=()

hr "1. 服务健康"
curl -sf "$API/api/health" >/dev/null && ok "控制面 /api/health 正常" || { bad "控制面不可访问: $API"; exit 1; }
VER=$(curl -s "$API/api/overview" | jsonget '.srs_version')
[ -n "$VER" ] && ok "SRS 版本: $VER" || bad "SRS API 不可访问"

hr "2. 推两路测试流"
ffmpeg -hide_banner -loglevel error -re -f lavfi -i "testsrc2=size=640x480:rate=25" \
  -f lavfi -i "sine=frequency=440:sample_rate=44100" -c:v libx264 -preset ultrafast -g 50 \
  -c:a aac -b:a 96k -f flv "$RTMP/live/smoke_a" >"$TMP/a.log" 2>&1 &
PIDS+=($!)
ffmpeg -hide_banner -loglevel error -re -f lavfi -i "smptebars=size=640x480:rate=25" \
  -f lavfi -i "sine=frequency=880:sample_rate=44100" -c:v libx264 -preset ultrafast -g 50 \
  -c:a aac -b:a 96k -f flv "$RTMP/live/smoke_b" >"$TMP/b.log" 2>&1 &
PIDS+=($!)
sleep 5
ACTIVE=$(curl -s "$API/api/streams" | jsonget '[.streams[] | select(.active and (.key|startswith("live/smoke")))] | length')
[ "$ACTIVE" = "2" ] && ok "两路流已上线并被 SRS 识别" || bad "流未上线（online=$ACTIVE）"

hr "3. 单流转推（direct，-c copy）"
TG=$(curl -s -X POST "$API/api/targets" -H 'Content-Type: application/json' \
  -d "{\"name\":\"smoke-direct\",\"url\":\"$TARGET_RTMP/mix/smoke_direct\",\"enabled\":true}" | jsonget '.id')
TARGETS+=("$TG")
[ -n "$TG" ] && ok "已创建推流目标 $TG" || bad "创建推流目标失败"
J1=$(curl -s -X POST "$API/api/jobs" -H 'Content-Type: application/json' \
  -d "{\"name\":\"smoke-direct\",\"mode\":\"direct\",\"sources\":[\"live/smoke_a\"],\"target_ids\":[\"$TG\"],\"enabled\":true}" | jsonget '.id')
JOBS+=("$J1")
curl -s -X POST "$API/api/jobs/$J1/start" >/dev/null
sleep 8
RES=$(curl -s "$API/api/streams" | jsonget '.streams[] | select(.key=="mix/smoke_direct" and .active) | "\(.width) \(.height)"' | head -1)
[ "$RES" = "640 480" ] && ok "第三方目标收到原分辨率 640x480（未转码）" || bad "直转输出异常: $RES"

hr "4. 多流混流（circle 圆窗口，转码）"
J2=$(curl -s -X POST "$API/api/jobs" -H 'Content-Type: application/json' \
  -d "{\"name\":\"smoke-mix\",\"mode\":\"mix\",\"sources\":[\"live/smoke_a\",\"live/smoke_b\"],\"layout\":\"circle\",\"width\":1280,\"height\":720,\"fps\":25,\"video_bitrate\":2000,\"pip_size\":30,\"pip_pos\":\"topright\",\"target_ids\":[\"$TG\"],\"enabled\":true}" | jsonget '.id')
JOBS+=("$J2")
curl -s -X POST "$API/api/jobs/$J2/start" >/dev/null
sleep 14
ST=$(curl -s "$API/api/jobs/$J2" | jsonget '.runtime.status')
RES=$(curl -s "$API/api/streams" | jsonget ".streams[] | select(.key==\"mix/$J2\" and .active) | \"\(.width) \(.height)\"" | head -1)
[ "$ST" = "running" ] && ok "混流任务运行中" || bad "混流任务状态: $ST"
[ "$RES" = "1280 720" ] && ok "混流输出 1280x720（圆窗口叠加）" || bad "混流输出异常: $RES"

hr "5. 录制与录像播放"
REC=$(curl -s -X POST "$API/api/recordings" -H 'Content-Type: application/json' -d '{"stream":"live/smoke_a"}' | jsonget '.id')
RECS+=("$REC")
sleep 8
curl -s -X POST "$API/api/recordings/$REC/stop" >/dev/null
sleep 3
INFO=$(curl -s "$API/api/recordings" | jsonget ".recordings[] | select(.id==\"$REC\") | \"\(.size) \(.duration) \(.file)\"" | head -1)
SIZE=$(echo "$INFO" | awk '{print $1}'); DUR=$(echo "$INFO" | awk '{print $2}'); FILE=$(echo "$INFO" | awk '{print $3}')
[ -n "${SIZE:-}" ] && [ "$SIZE" -gt 10000 ] 2>/dev/null && ok "录像文件已生成: $FILE (${SIZE}B / ${DUR}s)" || bad "录像为空: $INFO"
echo "     ffprobe 校验：$(ffprobe -v error -show_entries format=duration -of csv=p=0 "$API/media/$FILE" 2>/dev/null | head -1)s（HTTP 读取）"
CODE=$(curl -s -o /dev/null -w '%{http_code}' -H 'Range: bytes=0-1023' "$API/media/$FILE")
[ "$CODE" = "206" ] && ok "录像支持 Range 播放（HTTP 206）" || bad "录像 Range 请求返回 $CODE"

hr "6. 源流下线后任务自动等待"
kill "${PIDS[1]}" 2>/dev/null; PIDS[1]=
sleep 8
ST2=$(curl -s "$API/api/jobs/$J2" | jsonget '.runtime.status')
case "$ST2" in running|retrying|waiting|starting) ok "任务未崩溃，当前状态: $ST2" ;; *) bad "任务异常状态: $ST2" ;; esac

hr "结果"
echo "通过 $PASS 项，失败 $FAIL 项"
[ "$FAIL" -eq 0 ] && echo "SMOKE_TEST_OK" || echo "SMOKE_TEST_FAILED"
exit "$FAIL"
