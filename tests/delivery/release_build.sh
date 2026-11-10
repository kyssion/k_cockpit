#!/usr/bin/env bash
# 发布制品本地演练：与 .github/workflows/release.yml 的构建步骤逐行等价。
#
# 为什么要有这个脚本：release.yml 只在 push `v*` tag 时跑，而「制品里到底
# 有没有 public/ 与 migrations/、SHA256SUMS 能不能校验、版本注入是否生效」
# 这些问题不该等到第一次正式发版才暴露。用同一个脚本在本地产出一份**同构**
# 的制品，就能在没有 tag 决策的前提下把制品内容验一遍。
#
# 用法：
#   tests/delivery/release_build.sh [版本号]
# 环境变量：
#   KC_OUT   输出目录（默认 <repo>/dist-release）
#   KC_WEB   前端产物目录（默认 <repo>/web/dist）
#
# 产出：k_cockpit_<版本>_linux-{amd64,arm64}.tar.gz + SHA256SUMS
set -euo pipefail

VERSION="${1:-v0.1.0-rc.1}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
OUT_ROOT="${KC_OUT:-$ROOT/dist-release}"
WEB_DIST="${KC_WEB:-$ROOT/web/dist}"

if [ ! -f "$WEB_DIST/index.html" ]; then
  echo "缺少前端产物 $WEB_DIST/index.html：先执行 pnpm --dir web build" >&2
  exit 1
fi

BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
LDFLAGS="-s -w -X k_cockpit/internal/platform/version.panelVersion=${VERSION} -X k_cockpit/internal/platform/version.buildTime=${BUILD_TIME} -X k_cockpit/internal/nodeagent.buildVersion=${VERSION}"

rm -rf "$OUT_ROOT"
mkdir -p "$OUT_ROOT"

# macOS 的 bsdtar 会把扩展属性写成 AppleDouble 伴生文件（`._<name>`）塞进包里。
# 这些文件在 Linux 上解包后**不是 SQL 也不是页面**，migrate 会把 `._0001_*.sql`
# 当成一个迁移去执行，报「invalid message format」。制品里不允许出现它们：
# COPYFILE_DISABLE 关掉生成，再显式 --no-xattrs（GNU tar 无此选项，探测后决定）。
TAR_XATTR_FLAG=""
if tar --no-xattrs -cf /dev/null -T /dev/null 2>/dev/null; then
  TAR_XATTR_FLAG="--no-xattrs"
fi
export COPYFILE_DISABLE=1

for ARCH in amd64 arm64; do
  OUT="$OUT_ROOT/linux-${ARCH}"
  mkdir -p "$OUT"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/server" "$ROOT/cmd/server"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/agent" "$ROOT/cmd/agent"
  CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" go build -trimpath -o "$OUT/migrate" "$ROOT/cmd/migrate"
  # STATIC_DIR 默认 public；不拷前端产物会得到一个「只有 API、启动还不报错」的包。
  cp -r "$WEB_DIST" "$OUT/public"
  cp -r "$ROOT/internal/platform/database/migrations" "$OUT/migrations"
  cp "$ROOT/deploy/agent/k_cockpit-agent.service" "$OUT/"
  cp "$ROOT/deploy/controlplane/k-cockpit.service" "$OUT/"
  find "$OUT" -name '._*' -delete
  # 归档时保持相对路径，解包后即为 §2 约定的 dist/ 布局。
  tar $TAR_XATTR_FLAG -czf "$OUT_ROOT/k_cockpit_${VERSION}_linux-${ARCH}.tar.gz" \
    -C "$OUT" server agent migrate public migrations k_cockpit-agent.service k-cockpit.service
done

cd "$OUT_ROOT"
if command -v sha256sum >/dev/null 2>&1; then
  sha256sum k_cockpit_"${VERSION}"_linux-*.tar.gz > SHA256SUMS
else
  shasum -a 256 k_cockpit_"${VERSION}"_linux-*.tar.gz > SHA256SUMS
fi
echo "制品已生成于 $OUT_ROOT"
cat SHA256SUMS
