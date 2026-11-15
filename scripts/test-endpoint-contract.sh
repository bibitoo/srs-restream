#!/usr/bin/env bash
# 端口 / 地址契约自测：不需要 docker、不需要 SRS。
#
# 直接跑本地编译出来的控制面二进制，模拟捷兔桌面「宿主端口冲突自动改号」的场景，
# 断言 /api/endpoints 报出来的推流端口与告知通道一致（/api/discover 只做最小发现）。
#
# 用法：scripts/test-endpoint-contract.sh [控制面二进制路径]
#   默认用 control/srsrestream（go build -o control/srsrestream ./control 产出）
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="${1:-$ROOT/control/srsrestream}"
PORT="${TEST_PORT:-19186}"
BASE="http://127.0.0.1:$PORT"
TMP="$(mktemp -d)"
PIDS=()

pass=0
fail=0
ok() { echo "  ✅ $*"; pass=$((pass + 1)); }
ng() { echo "  ❌ $*"; fail=$((fail + 1)); }

cleanup() {
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  rm -rf "$TMP"
}
trap cleanup EXIT

[ -x "$BIN" ] || { echo "找不到可执行的控制面：$BIN（先 go build -o control/srsrestream ./control）" >&2; exit 1; }

# start <数据目录名> [环境变量...]
# 注意：后台进程必须挂在**当前 shell** 下（不能放进 $( ) 里，否则子 shell 退出会把进程带走）。
start() {
  local name="$1"; shift
  local dir="$TMP/$name"
  mkdir -p "$dir"
  (
    cd "$TMP" || exit 1
    exec env DATA_DIR="$dir" LISTEN=":$PORT" "$@" "$BIN" >"$TMP/$name.log" 2>&1
  ) &
  PIDS+=("$!")
}

stop_all() {
  for p in "${PIDS[@]:-}"; do [ -n "$p" ] && kill "$p" 2>/dev/null; done
  PIDS=()
  # 等端口释放
  for _ in $(seq 1 40); do
    curl -fsS -o /dev/null "$BASE/api/health" 2>/dev/null || return 0
    sleep 0.1
  done
}

wait_up() {
  for _ in $(seq 1 60); do
    if curl -fsS -o /dev/null "$BASE/api/health" 2>/dev/null; then return 0; fi
    sleep 0.1
  done
  return 1
}

# jget <path> <python 表达式，对象名 d> → 从 JSON 响应里取值
jget() {
  curl -fsS "$BASE$1" 2>/dev/null | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print('')
    raise SystemExit
print($2)
" 2>/dev/null
}

# eps_get <python 表达式，对象名 d> [口令] → 从 /api/endpoints 取值（默认出厂口令 jetool）
eps_get() {
  local expr="$1" key="${2:-jetool}"
  curl -fsS -H "X-Access-Key: $key" "$BASE/api/endpoints" 2>/dev/null | python3 -c "
import sys, json
try:
    d = json.load(sys.stdin)
except Exception:
    print('')
    raise SystemExit
print($expr)
" 2>/dev/null
}

# discover_rtmp_port → 打印 /api/endpoints 的 rtmp_port（需要口令的接口）
discover_rtmp_port() {
  curl -fsS -H 'X-Access-Key: jetool' "$BASE/api/endpoints" 2>/dev/null |
    python3 -c "import sys,json;print(json.load(sys.stdin).get('rtmp_port',''))" 2>/dev/null
}
# endpoint_field <name> <field>
endpoint_field() {
  curl -fsS -H 'X-Access-Key: jetool' "$BASE/api/endpoints" 2>/dev/null |
    python3 -c "
import sys, json
d = json.load(sys.stdin)
for e in d.get('endpoints', []):
    if e.get('name') == '$1':
        print(e.get('$2', ''))
        break
" 2>/dev/null
}

echo "== 场景 1：桌面什么都没告知（老行为，应回退容器内端口 1935）"
start s1
if wait_up; then
  got=$(discover_rtmp_port)
  src=$(endpoint_field rtmp portSource)
  [ "$got" = "1935" ] && ok "rtmp_port=1935" || ng "rtmp_port=$got（期望 1935）"
  [ "$src" = "container-port" ] && ok "portSource=container-port（诚实标记未知）" || ng "portSource=$src"
else
  ng "控制面没起来（见 $TMP/s1.log）"
fi
stop_all

echo "== 场景 2：环境变量通道 JETDESK_PORT_1935=1936（模拟宿主端口被占用改号）"
start s2 JETDESK_PORT_1935=1936
if wait_up; then
  got=$(discover_rtmp_port); src=$(endpoint_field rtmp portSource)
  [ "$got" = "1936" ] && ok "rtmp_port=1936" || ng "rtmp_port=$got（期望 1936）"
  case "$src" in
    "env JETDESK_PORT_1935"*) ok "portSource=$src" ;;
    *) ng "portSource=$src" ;;
  esac
else
  ng "控制面没起来（见 $TMP/s2.log）"
fi
stop_all

echo "== 场景 3：按端点名 JETDESK_ENDPOINT_RTMP_PORT=1937"
start s3 JETDESK_ENDPOINT_RTMP_PORT=1937
if wait_up; then
  got=$(discover_rtmp_port)
  [ "$got" = "1937" ] && ok "rtmp_port=1937" || ng "rtmp_port=$got（期望 1937）"
else
  ng "控制面没起来（见 $TMP/s3.log）"
fi
stop_all

echo "== 场景 4：桌面推送 /jetdesk/config 告知 hostPort=1938（推荐通道）"
start s4
if wait_up; then
  curl -fsS -X POST "$BASE/jetdesk/config" -H 'X-Access-Key: jetool' \
    -H 'Content-Type: application/json' \
    -d '{"self":"control","endpoints":[{"name":"rtmp","hostPort":1938}]}' -o /dev/null
  got=$(discover_rtmp_port); src=$(endpoint_field rtmp portSource)
  [ "$got" = "1938" ] && ok "推送后 rtmp_port=1938" || ng "rtmp_port=$got（期望 1938）"
  [ "$src" = "desktop-push" ] && ok "portSource=desktop-push" || ng "portSource=$src"
  grep -q '"rtmp": 1938' "$TMP/s4/endpoints.json" 2>/dev/null &&
    ok "宿主端口已落盘 data/endpoints.json" || ng "没有落盘 endpoints.json"
else
  ng "控制面没起来（见 $TMP/s4.log）"
fi
stop_all

echo "== 场景 5：容器重建后沿用落盘的宿主端口（首推前也报对端口）"
start s4   # 复用同一数据目录
if wait_up; then
  got=$(discover_rtmp_port)
  [ "$got" = "1938" ] && ok "重建后仍是 1938" || ng "rtmp_port=$got（期望 1938）"
  grep -q "沿用上次落盘" "$TMP/s4.log" && ok "启动日志提示沿用落盘值" || ng "启动日志没有沿用提示"
else
  ng "控制面没起来（见 $TMP/s4.log）"
fi
stop_all

echo "== 场景 6：用户显式配置 PUBLIC_RTMP 时以用户为准"
start s6 PUBLIC_RTMP="rtmp://example.lan:1999/"
if wait_up; then
  got=$(eps_get "d.get('rtmp_base','')")
  [ "$got" = "rtmp://example.lan:1999/" ] && ok "rtmp_base=$got" || ng "rtmp_base=$got（期望 rtmp://example.lan:1999/）"
else
  ng "控制面没起来（见 $TMP/s6.log）"
fi
stop_all

echo "== 场景 7：keyQuery 必须出现在端点里（桌面据此拼口令，漏配前端会 401）"
start s7
if wait_up; then
  kq_http=$(endpoint_field http keyQuery)
  kq_rtmp=$(endpoint_field rtmp keyQuery)
  [ "$kq_http" = "key" ] && ok "http.keyQuery=key" || ng "http.keyQuery=$kq_http（期望 key）"
  [ "$kq_rtmp" = "key" ] && ok "rtmp.keyQuery=key" || ng "rtmp.keyQuery=$kq_rtmp（期望 key）"
  # 桌面会拼成 http://host:port/?key=<口令>，这个形态必须能过鉴权
  code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/overview?key=jetool")
  [ "$code" = "200" ] && ok "带 ?key= 访问受保护接口 → 200" || ng "带 ?key= 访问 → $code（期望 200，说明 keyQuery 与服务端鉴权不一致）"
  code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/overview")
  [ "$code" = "401" ] && ok "不带口令 → 401（证明确实需要 keyQuery）" || ng "不带口令 → $code（期望 401）"
else
  ng "控制面没起来（见 $TMP/s7.log）"
fi
stop_all

echo "== 场景 8：鉴权契约（无过渡期：只认 X-Access-Key；报文与旧头一律不放行）"
start s8
if wait_up; then
  # 8.0 出厂口令阶段也不给后门：不带头、只靠报文塞口令，都必须 401
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/jetdesk/config" \
    -H 'Content-Type: application/json' -d '{"self":"control"}')
  [ "$code" = "401" ] && ok "不带 X-Access-Key 推配置 → 401（出厂口令阶段也照样拦）" || ng "无头推配置 → $code（期望 401）"
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/jetdesk/config" \
    -H 'Content-Type: application/json' -d '{"accessKey":"jetool"}')
  [ "$code" = "401" ] && ok "报文里塞口令 → 401（报文自证已删除）" || ng "报文自证 → $code（期望 401）"

  # 8.1 旧头必须全部失效（没有过渡双读）
  for h in X-API-Key X-JetDesk-Key X-Auth-Token; do
    code=$(curl -s -o /dev/null -w '%{http_code}' -H "$h: jetool" "$BASE/api/overview")
    [ "$code" = "401" ] && ok "$h 已失效 → 401" || ng "$h 仍可用（→ $code），存在过渡双读"
  done
  # 协议规定的 MCP 身份头仍然有效
  code=$(curl -s -o /dev/null -w '%{http_code}' -H 'X-JetDesk-Agent-Token: jetool' "$BASE/api/overview")
  [ "$code" = "200" ] && ok "X-JetDesk-Agent-Token（协议身份头）→ 200" || ng "Agent-Token → $code"
  code=$(curl -s -o /dev/null -w '%{http_code}' -H 'X-Access-Key: jetool' "$BASE/api/overview")
  [ "$code" = "200" ] && ok "X-Access-Key → 200" || ng "X-Access-Key → $code"

  # 8.2 轮换到强口令
  curl -s -o /dev/null -X POST "$BASE/jetdesk/config" -H 'X-Access-Key: jetool' \
    -H 'Content-Type: application/json' -d '{"self":"control","accessKey":"k8-strong"}'
  code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/overview?key=k8-strong")
  [ "$code" = "200" ] && ok "口令已轮换为 k8-strong" || ng "轮换失败（→ $code）"

  # 8.3 configToken 只是防误连：不能替口令，也不能被当口令
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/jetdesk/config" \
    -H 'Authorization: Bearer jetool' -H 'Content-Type: application/json' -d '{"self":"control"}')
  [ "$code" = "401" ] && ok "只有 configToken、没有 X-Access-Key → 401" || ng "configToken 顶替口令（→ $code）"
  code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/jetdesk/config" \
    -H 'Authorization: Bearer bogus-token' -H 'X-Access-Key: k8-strong' \
    -H 'Content-Type: application/json' -d '{"self":"control"}')
  [ "$code" = "401" ] && ok "错误 configToken → 401" || ng "错误 configToken → $code"
  resp=$(curl -s -X POST "$BASE/jetdesk/config" -H 'Authorization: Bearer jetool' \
    -H 'X-Access-Key: k8-strong' -H 'Content-Type: application/json' -d '{"self":"control"}')
  echo "$resp" | grep -q '"ok":true' && ok "configToken + X-Access-Key 均正确 → 200" || ng "正确组合被拒"
  echo "$resp" | grep -q '"keyUpdated":false' && ok "重推幂等 keyUpdated=false" || ng "幂等断言失败"

  # 8.4 轮换后旧口令立即失效
  code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/overview?key=jetool")
  [ "$code" = "401" ] && ok "旧口令 → 401" || ng "旧口令仍可用（→ $code）"

  # 8.5 发现接口拆分：discover 只做最小发现，地址与推流口令在 /api/endpoints（要口令）
  disc=$(curl -s "$BASE/api/discover")
  echo "$disc" | grep -q '"auth_required":true' && ok "/api/discover 声明 auth_required=true" || ng "discover 未声明 auth_required"
  echo "$disc" | grep -q '/api/endpoints' && ok "/api/discover 指向 /api/endpoints" || ng "discover 未给出 endpoints_path"
  if echo "$disc" | grep -qE '"publish_key"|"rtmp_base"|"access_key"|"endpoints"'; then
    ng "/api/discover 仍带敏感字段（应为最小发现）"
  else
    ok "/api/discover 不含 publish_key / rtmp_base / access_key / endpoints"
  fi
  code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/endpoints")
  [ "$code" = "401" ] && ok "/api/endpoints 无口令 → 401" || ng "/api/endpoints 无口令 → $code（期望 401）"

  # 8.6 /api/endpoints（带口令）：publishUrl 用推流口令拼，绝不出现 access_key
  pub=$(eps_get "d.get('publish_key','')" k8-strong)
  pu=$(eps_get "next((e.get('publishUrl','') for e in d.get('endpoints',[]) if e['name']=='rtmp'), '')" k8-strong)
  [ -n "$pub" ] && ok "/api/endpoints 返回 publish_key" || ng "/api/endpoints 未返回 publish_key"
  case "$pu" in
    *"?key=$pub") ok "publishUrl 用 publish_key 拼" ;;
    *) ng "publishUrl=$pu（期望 ?key=<publish_key>）" ;;
  esac
  case "$pu" in
    *"k8-strong"*) ng "publishUrl 里出现了 access_key（绝不允许）" ;;
    *) ok "publishUrl 里没有 access_key" ;;
  esac
  if eps_get "str(d)" k8-strong | grep -q 'access_key'; then
    ng "/api/endpoints 里出现了 access_key"
  else
    ok "/api/endpoints 不返回 access_key"
  fi
else
  ng "控制面没起来（见 $TMP/s8.log）"
fi
stop_all

echo
if [ "$fail" -eq 0 ]; then
  echo "全部通过：$pass 项"
  exit 0
fi
echo "失败 $fail 项 / 通过 $pass 项"
exit 1
