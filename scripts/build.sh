#!/usr/bin/env bash
# Lujiang 跨平台构建矩阵 (bash)。
# 用法：
#   scripts/build.sh                       # 当前平台 + 当前架构
#   scripts/build.sh linux amd64           # 覆盖
#   scripts/build.sh --skip-web darwin arm64
#
# 产物：dist/lujiang-{server,client}.{os}-{arch}[.exe]
# 依赖：Go 1.25+、Node 20+（用于 web 资源）。
# modernc.org/sqlite 是 pure-Go，因此 CGO_ENABLED=0 可用。

set -euo pipefail

SKIP_WEB=0
TARGET_OS=""
TARGET_ARCH=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --skip-web) SKIP_WEB=1; shift ;;
        windows|linux|darwin) TARGET_OS="$1"; shift ;;
        amd64|arm64) TARGET_ARCH="$1"; shift ;;
        *) echo "Unknown arg: $1" >&2; exit 2 ;;
    esac
done

[[ -z "$TARGET_OS" ]]   && TARGET_OS="$(go env GOOS)"
[[ -z "$TARGET_ARCH" ]] && TARGET_ARCH="$(go env GOARCH)"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

echo "==> Target: $TARGET_OS/$TARGET_ARCH"

if [[ "$SKIP_WEB" -eq 0 ]]; then
    echo "==> Building web frontend"
    (
        cd web
        npm install --no-audit --no-fund
        npm run build
    )
fi

EXT=""
[[ "$TARGET_OS" == "windows" ]] && EXT=".exe"

mkdir -p "$REPO_ROOT/dist"

export CGO_ENABLED=0 GOOS="$TARGET_OS" GOARCH="$TARGET_ARCH"

for target in lujiang-server lujiang-client; do
    out="$REPO_ROOT/dist/$target.$TARGET_OS-$TARGET_ARCH$EXT"
    echo "==> Building $target -> $out"
    go build -trimpath -ldflags "-s -w" -o "$out" "./cmd/$target"
done

echo "==> Done. Artifacts in $REPO_ROOT/dist"
