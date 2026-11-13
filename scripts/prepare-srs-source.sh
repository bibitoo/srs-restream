#!/usr/bin/env bash
# 把本地 SRS 源码快照到 build/srs-src，供 docker build 使用。
# 说明：SRS 目录里有指向仓库外部的软链接（oryx/state-threads 等），
#      docker build 的构建上下文无法跟随这些软链接，因此先快照一份自包含的源码。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
SRS_SRC="${SRS_SRC:-/home/sunz/AI/srs}"
DST="$ROOT/build/srs-src"

if [ ! -d "$SRS_SRC/trunk" ]; then
  echo "找不到 SRS 源码: $SRS_SRC（可用 SRS_SRC=/path/to/srs 指定）" >&2
  exit 1
fi

echo "快照 SRS 源码: $SRS_SRC → $DST"
mkdir -p "$DST"
rsync -a --delete \
  --exclude '.git' \
  --exclude 'objs' \
  --exclude 'website' \
  --exclude 'dev-docker' \
  --exclude 'oryx' \
  --exclude 'state-threads' \
  --exclude '/doc' \
  --exclude '/tools' \
  --exclude '/skills' \
  --exclude '/.github' \
  --exclude '/.kiro' \
  --exclude '/.claude' \
  --exclude '/.agents' \
  --exclude '/.openclaw' \
  --exclude '/.vs' \
  --exclude '/.vscode' \
  --exclude '/.run' \
  --exclude '/trunk/3rdparty/ffmpeg-4-fit' \
  --exclude '/trunk/3rdparty/openssl-1.1-fit' \
  --exclude '/trunk/3rdparty/gperftools-2-fit' \
  --exclude '/trunk/3rdparty/gprof' \
  --exclude '/trunk/3rdparty/gtest-fit' \
  --exclude '/trunk/3rdparty/srs-bench' \
  --exclude '/trunk/3rdparty/srs-docs' \
  --exclude '/trunk/3rdparty/httpx-static' \
  --exclude '/trunk/3rdparty/openssl-OpenSSL_1_0_2u.tar.gz' \
  "$SRS_SRC/" "$DST/"

# 记录源码版本，方便追溯镜像来源
VERSION="$(cd "$SRS_SRC" && git describe --tags --always 2>/dev/null || true)"
printf '%s\n' "${VERSION:-unknown}" > "$DST/SRS_VERSION.txt"
echo "完成（源码版本: ${VERSION:-unknown}，体积: $(du -sh "$DST" | cut -f1)）"
