#!/usr/bin/env bash
# 访问密钥生命周期回归测试（无需 SRS / ffmpeg，直接拉起控制面进程）
#
# 覆盖规范 desktop-container.md §5.1「两道门」的放行条件与换口令语义：
#   A. 首次推送：生效口令仍是出厂默认 jetool → 放行，并接受 body 里的新口令
#   B. 换口令后再推配置：带新口令可推；只带 configToken(公开常量) 不放行；报文自证放行
#   C. 重启进程：新口令持久化，不回落 jetool
#   D. ACCESS_KEY 只作为初值：已有持久化口令时不被覆盖
#   E. 全新实例：ACCESS_KEY 作为初值
#   F. JETDESK_CONFIG_STRICT=0 时放宽（仅排障）
set -u
PORT=${PORT:-17999}
BIN=${BIN:-/tmp/jetstream-control}
BASE="http://127.0.0.1:$PORT"
PASS=0; FAIL=0; PID=""
ok()   { PASS=$((PASS+1)); printf '  ✅ %s\n' "$1"; }
bad()  { FAIL=$((FAIL+1)); printf '  ❌ %s\n' "$1"; }
check(){ [ "$2" = "$3" ] && ok "$1（HTTP $3）" || bad "$1（期望 $3，实际 $2）"; }

stop() { if [ -n "$PID" ]; then kill "$PID" 2>/dev/null; wait "$PID" 2>/dev/null; fi; PID=""; sleep 0.3; }

start() { # $1=data dir；其余为 KEY=VALUE 环境变量
  local dir=$1; shift
  stop
  # 端口必须真的空闲：否则新进程 bind 失败而 health 探针命中旧进程，测试会「假通过」
  if ss -ltn 2>/dev/null | grep -qE "[:.]$PORT[[:space:]]"; then
    echo "端口 $PORT 已被占用，先清理后再跑"; return 1
  fi
  env DATA_DIR="$dir" LISTEN=":$PORT" SRS_API="http://127.0.0.1:59999" "$@" \
      "$BIN" >"$dir/out.log" 2>&1 &
  PID=$!
  for _ in $(seq 1 60); do
    curl -fsS -o /dev/null "$BASE/api/health" 2>/dev/null && return 0
    kill -0 "$PID" 2>/dev/null || break
    sleep 0.1
  done
  echo "启动失败（dir=$dir），日志："; tail -5 "$dir/out.log"; PID=""; return 1
}

# 鉴权探针：/api/settings 需要密钥但不依赖 SRS
probe() { curl -sS -o /dev/null -w '%{http_code}' -H "$1" "$BASE/api/settings"; }

# 推配置：push <body> [curl 参数...]
push() {
  local body=$1; shift
  curl -sS -o /dev/null -w '%{http_code}' -X POST "$BASE/jetdesk/config" \
    -H 'Content-Type: application/json' "$@" -d "$body"
}

D1=$(mktemp -d); D2=$(mktemp -d); D3=$(mktemp -d)
trap 'stop; rm -rf "$D1" "$D2" "$D3"' EXIT

echo "【场景 A】首次推送 → 默认 jetool 生效 → 下发新密钥"
start "$D1" || exit 1
check "未改口令时 jetool 可访问业务 API" "$(probe 'X-Access-Key: jetool')" 200
check "首次推配置（Bearer configToken=jetool，body 带新密钥）" \
      "$(push '{"self":"jetstream","accessKey":"Kx9-new-secret","instanceId":"d-x:jetstream"}' -H 'Authorization: Bearer jetool')" 200
check "换口令后：新密钥可访问业务 API" "$(probe 'X-Access-Key: Kx9-new-secret')" 200
check "换口令后：jetool 失效（业务 API）" "$(probe 'X-Access-Key: jetool')" 401

echo "【场景 B】换口令后再推配置"
check "带新口令 X-API-Key 推配置" \
      "$(push '{"self":"jetstream","communityBaseUrl":"http://192.168.0.14:8090"}' -H 'X-API-Key: Kx9-new-secret')" 200
check "只带 configToken(jetool) 推配置 → 拒绝（不开放后门）" \
      "$(push '{"self":"evil","accessKey":"evil"}' -H 'Authorization: Bearer jetool')" 401
check "无任何凭据推配置 → 拒绝" "$(push '{"self":"evil"}')" 401
check "老客户端报文自证（body 带当前口令）→ 放行" \
      "$(push '{"self":"jetstream","accessKey":"Kx9-new-secret"}')" 200
check "错误口令推配置（含覆盖意图）→ 拒绝" \
      "$(push '{"self":"x","accessKey":"evil"}' -H 'X-API-Key: wrong')" 401
check "拒绝后当前口令仍然有效" "$(probe 'X-Access-Key: Kx9-new-secret')" 200
check "注入的 evil 口令没有生效" "$(probe 'X-Access-Key: evil')" 401

echo "【场景 C】重启持久化：不回落 jetool"
start "$D1" || exit 1
check "重启后新口令仍有效" "$(probe 'X-Access-Key: Kx9-new-secret')" 200
check "重启后 jetool 仍无效" "$(probe 'X-Access-Key: jetool')" 401
grep -q 'Kx9-new-secret' "$D1/config.json" && ok "config.json 持久化了新口令" || bad "config.json 里没有新口令"

echo "【场景 D】ACCESS_KEY 只作为初始值"
start "$D1" ACCESS_KEY=env-should-not-win || exit 1
check "已有用户口令时，env ACCESS_KEY 不覆盖它" "$(probe 'X-Access-Key: Kx9-new-secret')" 200
check "env 值不作为生效口令" "$(probe 'X-Access-Key: env-should-not-win')" 401

echo "【场景 E】全新实例：ACCESS_KEY 作为初值"
start "$D2" ACCESS_KEY=init-from-env || exit 1
check "全新实例用 env 初值" "$(probe 'X-Access-Key: init-from-env')" 200
check "全新实例 jetool 无效（初值不是它）" "$(probe 'X-Access-Key: jetool')" 401

echo "【场景 F】JETDESK_CONFIG_STRICT=0 排障放宽"
start "$D3" JETDESK_CONFIG_STRICT=0 || exit 1
check "严格开关关闭时任意推送放行（仅排障）" "$(push '{"self":"jetstream"}')" 200

stop
echo
echo "通过 $PASS 项，失败 $FAIL 项"
[ "$FAIL" -eq 0 ]
