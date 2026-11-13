#!/usr/bin/env bash
# 把本项目打包成捷兔桌面（JetDesk）的 .jet 软件包。
#
# .jet 是 gzip 的 tar 包，根目录固定为 jetdesk/，其中 jdconfig 是清单。
# 包内包含：
#   jetdesk/
#   ├── jdconfig                清单（id / type=compose / endpointPort=1986）
#   ├── docker-compose.yml      srs + control 两个服务
#   ├── srs/{Dockerfile,srs.conf,src/}         SRS 引擎（源码快照，首次安装时编译）
#   ├── control/{Dockerfile,entrypoint.sh,src/} 控制面（Go）
#   └── help/index.md           包内使用说明书
#
# 用法：scripts/build-jet.sh [输出路径]
#   SRS_SRC=/path/to/srs   指定 SRS 源码（默认 /home/sunz/AI/srs）
#   JET_SKILL=/path/to/skill  指定 jet-software-builder 技能目录（自动探测）
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRS_SRC="${SRS_SRC:-/home/sunz/AI/srs}"
OUT="${1:-$ROOT/build/jetstream.jet}"
BUILD="$ROOT/build/jet"

# 定位打包脚本（技能目录）
if [ -z "${JET_SKILL:-}" ]; then
  for cand in \
    "$HOME/.jiuwenswarm/agent/workspace/skills/jet-software-builder" \
    "/home/sunz/.jiuwenswarm/agent/workspace/skills/jet-software-builder"; do
    [ -f "$cand/scripts/jet_pack.py" ] && JET_SKILL="$cand" && break
  done
fi
[ -n "${JET_SKILL:-}" ] || { echo "找不到 jet-software-builder 技能目录，请设置 JET_SKILL" >&2; exit 1; }

# 1) 准备 SRS 源码快照（38MB，已在 .gitignore 中）
if [ ! -d "$ROOT/build/srs-src/trunk" ]; then
  echo "[jet] 未找到 SRS 源码快照，先执行 prepare-srs-source.sh"
  SRS_SRC="$SRS_SRC" "$ROOT/scripts/prepare-srs-source.sh"
fi

# 2) 组装 jetdesk 目录
echo "[jet] 组装包目录 → $BUILD/jetdesk"
rm -rf "$BUILD"
mkdir -p "$BUILD"
cp -a "$ROOT/jet" "$BUILD/jetdesk"
cp -a "$ROOT/build/srs-src" "$BUILD/jetdesk/srs/src"

mkdir -p "$BUILD/jetdesk/control/src"
cp "$ROOT"/control/*.go "$ROOT/control/go.mod" "$BUILD/jetdesk/control/src/"
cp -a "$ROOT/control/web" "$BUILD/jetdesk/control/src/web"

chmod +x "$BUILD/jetdesk/control/entrypoint.sh"

# 3) 打包 + 校验
echo "[jet] 打包 → $OUT"
python3 "$JET_SKILL/scripts/jet_pack.py" build "$BUILD/jetdesk" --output "$OUT"
python3 "$JET_SKILL/scripts/jet_pack.py" validate "$OUT"
python3 "$JET_SKILL/scripts/jet_smoke_test.py" "$OUT"

echo
echo "[jet] 完成：$OUT"
ls -la "$OUT"
