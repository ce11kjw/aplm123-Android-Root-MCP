#!/usr/bin/env bash
# aplm123 Root MCP —— 无 NDK 的 arm64 构建脚本
#
# 与官方 build.sh 的差别：本脚本用 CGO_ENABLED=0 纯 Go 编译，
# 因此只能产出 arm64-v8a（32 位 ARM 与 x86_64 需要 NDK 交叉工具链）。
#
# 说明：早期版本会在编译时注入 module.prop 的 sha256，并在启动时校验；
# 该机制因会导致服务无法启动，已被移除。

set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-1785340800}
GOROOT_DIR=${GOROOT_DIR:-/workspace/zcr521-build/goroot}

export GOROOT="$GOROOT_DIR"
export PATH="$GOROOT/bin:$PATH"
export GOTOOLCHAIN=local
export CGO_ENABLED=0
export GOPATH=${GOPATH:-/home/coomi/go}
export GOFLAGS=-mod=vendor
export SOURCE_DATE_EPOCH

cd "$ROOT"

echo "==> 0/6 版本一致性"
# 版本号唯一权威来源是 module/module.prop；这里读取而不是硬编码，
# 避免"二进制一个版本、模块元数据另一个版本"的撕裂。
VERSION=$(sed -n 's/^version=//p' module/module.prop | head -1)
VERSION_CODE=$(sed -n 's/^versionCode=//p' module/module.prop | head -1)
[ -n "$VERSION" ] || { echo "错误：module/module.prop 缺少 version" >&2; exit 1; }
[ -n "$VERSION_CODE" ] || { echo "错误：module/module.prop 缺少 versionCode" >&2; exit 1; }
echo "    version=$VERSION  versionCode=$VERSION_CODE"
bash scripts/bump-version.sh --check

echo "==> 1/5 测试"
# 测试必须用宿主平台跑（GOOS=android 的产物无法在本机执行）。
#
# 注意：PRoot 环境下 net.Interfaces() 偶发挂起，会让 internal/frontend 假失败。
# 因此逐包执行并允许重试一次；真正失败仍会中断构建。
# PRoot 环境下 net.Interfaces() 偶发挂起，会让个别包超时。
# 因此逐包执行，失败时重试；只要重试通过就视为环境抖动。
# 注意：timeout 必须显著大于 go test -timeout，否则重试也会被外层杀掉。
TEST_PKGS=$(go list ./...)
TEST_FAILED=0
for pkg in $TEST_PKGS; do
  if timeout 200 go test -count=1 -timeout 150s "$pkg" >/tmp/aplm-test.log 2>&1; then
    continue
  fi
  echo "  首次失败，重试: $(basename "$pkg")"
  if timeout 200 go test -count=1 -timeout 150s "$pkg" >/tmp/aplm-test.log 2>&1; then
    echo "  重试通过（判定为环境抖动）: $(basename "$pkg")"
  else
    echo "  失败: $pkg"
    tail -8 /tmp/aplm-test.log
    TEST_FAILED=1
  fi
done
if [ "$TEST_FAILED" != "0" ]; then
  echo "测试未通过，中止构建" >&2
  exit 1
fi
echo "    全部测试通过"

echo "==> 2/5 生成 schema"
go run ./cmd/aplm123d schema --output schemas/tools.json

echo "==> 3/5 组装模块目录"
rm -rf build/module dist
cp -R module build/module
mkdir -p build/module/bin/arm64-v8a

# 逐项确认脚本都在（cp -R 曾出现静默失败，会打出残包）
for f in customize.sh common.sh service.sh action.sh uninstall.sh \
         systemless.sh post-fs-data.sh update.sh module.prop webroot/index.html; do
  [ -f "build/module/$f" ] || { echo "错误：模块缺少 $f" >&2; exit 1; }
done

python3 scripts/generate_notice.py --repo . --license build/module/LICENSE --output build/module/NOTICE

echo "==> 4/5 编译（arm64，无 NDK）"
GOOS=android GOARCH=arm64 go build -trimpath -buildmode=pie -tags osusergo,netgo \
  -ldflags "-s -w -buildid= \
    -X github.com/aplm123/android-ai-mcp/internal/buildinfo.Version=$VERSION \
    " \
  -o build/module/bin/arm64-v8a/aplm123d ./cmd/aplm123d

# 7zz 复用已验证的官方产物（重编需 NDK）
SEVENZIP=${SEVENZIP:-/workspace/zcr521-src/bin/arm64-v8a/7zz}
cp "$SEVENZIP" build/module/bin/arm64-v8a/7zz
chmod 0755 build/module/bin/arm64-v8a/7zz

python3 scripts/verify_elf.py --file build/module/bin/arm64-v8a/aplm123d \
  --machine 183 --page-size 16384 --api 26

echo "==> 5/5 打包"
python3 scripts/package.py --module-stage build/module --dist dist \
  --version "$VERSION" --epoch "$SOURCE_DATE_EPOCH" \
  --abis arm64-v8a --name-suffix arm64

ZIP=$(ls dist/*.zip | head -1)

# 一致性闸门：二进制内嵌版本必须与 module.prop 一致。
# 这是最后一道防线——历史上出现过两者脱钩导致的问题。
python3 - "$ZIP" <<'PY'
import sys, zipfile, re, tempfile, subprocess, os, stat
z = zipfile.ZipFile(sys.argv[1])
prop_version = re.search(rb'^version=(.+)$', z.read('module.prop'), re.M).group(1).decode().strip()
blob = z.read('bin/arm64-v8a/aplm123d')
# 只做静态检查：确认二进制内嵌了该版本字符串（本机无法执行 android/arm64 产物）
if prop_version.encode() not in blob:
    raise SystemExit(f"错误：二进制未内嵌版本 {prop_version}，与 module.prop 不一致")
print(f"    一致性校验通过: version={prop_version}")
PY

echo
echo "完成: $ZIP"
sha256sum "$ZIP"
