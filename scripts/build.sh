#!/usr/bin/env bash
# build.sh 本地构建 wb2api-panel（参数与 .github/workflows/binaries.yml 对齐）。
#
# 事实源是 CI：二进制名（wb2api / wb2api.exe）、包结构（dist/<os>-<arch>/ + 同名 zip，
# 内含二进制 + config.example.json + README.md）、编译参数（CGO_ENABLED=0、-trimpath、
# -ldflags "-s -w -X main.appVersion=<版本>"）都与 binaries.yml:46-68 保持一致。
# 改这里之前先看那边，否则本地产物和 Release 产物会不一样。
#
# 用法：
#   ./scripts/build.sh                      # 当前平台，不打包
#   ./scripts/build.sh --zip                # 当前平台 + zip
#   ./scripts/build.sh --all                # 六个平台全套 zip（CI 的全量产物）
#   ./scripts/build.sh --all -v 2026.11.24.912
#
# 版本号缺省取上海时间 yyyy.M.d.Hmm。注意本机（git-bash）常常没有 tzdata，
# `TZ=Asia/Shanghai date` 会**静默回落 UTC**、打出来整整早 8 小时，故先探测再兜底。
set -euo pipefail

cd "$(dirname "$0")/.."

ALL=0
ZIP=0
VERSION="${VERSION:-}"

usage() {
    sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
    case "$1" in
        --all) ALL=1 ;;
        --zip) ZIP=1 ;;
        -v | --version)
            VERSION="${2:-}"
            [ -n "$VERSION" ] || { echo "--version 需要一个值" >&2; exit 2; }
            shift
            ;;
        -h | --help)
            usage
            exit 0
            ;;
        *)
            echo "未知参数：$1（--all / --zip / --version/-v / --help）" >&2
            exit 2
            ;;
    esac
    shift
done

if [ -z "$VERSION" ]; then
    VERSION=$(TZ=Asia/Shanghai date +%Y.%-m.%-d.%-H%M)
    if [ "$(TZ=UTC date +%H)" = "$(TZ=Asia/Shanghai date +%H)" ]; then
        VERSION=$(date -u -d '+8 hours' +%Y.%-m.%-d.%-H%M)
    fi
fi

if [ "$ALL" = "1" ]; then
    OSES="windows linux darwin"
    ARCHS="amd64 arm64"
    ZIP=1
else
    OSES=$(go env GOOS)
    ARCHS=$(go env GOARCH)
fi

echo "版本 $VERSION"
echo "目标：$(for os in $OSES; do for arch in $ARCHS; do printf '%s/%s ' "$os" "$arch"; done; done)"

export CGO_ENABLED=0
mkdir -p dist

# pack <os> <arch> <二进制名>：把 dist/<os>-<arch>/ 打成一个 zip（条目在压缩包根部，
# 与 CI 的 python zipfile 写法一致）。
pack() {
    local os="$1" arch="$2" name="$3"
    local dir="dist/$os-$arch"
    local zipname="dist/wb2api-panel-${VERSION}-${os}-${arch}.zip"
    rm -f "$zipname"
    if command -v python3 >/dev/null 2>&1; then
        python3 -c 'import os,sys,zipfile; n,d=sys.argv[1:]; z=zipfile.ZipFile(n,"w",zipfile.ZIP_DEFLATED); [z.write(os.path.join(d,f), f) for f in sorted(os.listdir(d))]; z.close()' "$zipname" "$dir"
    elif command -v zip >/dev/null 2>&1; then
        (cd "$dir" && zip -q -r "../$(basename "$zipname")" .)
    else
        echo "缺少打包工具：需要 python3 或 zip" >&2
        exit 1
    fi
    echo "  打包完成 $zipname"
}

for os in $OSES; do
    for arch in $ARCHS; do
        name=wb2api
        [ "$os" = "windows" ] && name=wb2api.exe
        dir="dist/$os-$arch"
        mkdir -p "$dir"
        GOOS="$os" GOARCH="$arch" \
            go build -trimpath -ldflags="-s -w -X main.appVersion=${VERSION}" -o "$dir/$name" ./cmd/server
        cp config.example.json README.md "$dir/"
        echo "  构建完成 $os/$arch -> $dir/$name"
        if [ "$ZIP" = "1" ]; then
            pack "$os" "$arch" "$name"
        fi
    done
done

echo "全部完成。产物在 dist/"
