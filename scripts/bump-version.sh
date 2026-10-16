#!/usr/bin/env bash
#
# aplm123 Root MCP —— 版本号单点管理
#
# 权威来源：module/module.prop（管理器直接读取它，且必须存在于发布包内）。
# 本脚本把该版本同步到其余所有出现位置，避免"版本号撕裂"。
#
# 用法：
#   bash scripts/bump-version.sh 0.04              # 改版本，versionCode 自动按日期生成
#   bash scripts/bump-version.sh 0.04 2026092502   # 改版本，显式指定 versionCode
#   bash scripts/bump-version.sh --check           # 只校验各处是否一致，不修改
#   bash scripts/bump-version.sh --current         # 打印当前版本
#
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

PROP="module/module.prop"
ACTION="module/action.sh"
CUSTOMIZE="module/customize.sh"
VERIFY="scripts/verify_module.py"
BUILDINFO="internal/buildinfo/buildinfo.go"
BUILD="scripts/build-arm64.sh"
CHANGELOG="CHANGELOG.md"

die() { printf '错误：%s\n' "$1" >&2; exit 1; }

read_prop() {
  # 读取 module.prop 中的某个键
  sed -n "s/^$1=//p" "$PROP" | head -1
}

# ---------- --current ----------
if [ "${1:-}" = "--current" ]; then
  printf 'version=%s\nversionCode=%s\n' "$(read_prop version)" "$(read_prop versionCode)"
  exit 0
fi

# ---------- --check ----------
if [ "${1:-}" = "--check" ]; then
  VERSION=$(read_prop version)
  CODE=$(read_prop versionCode)
  [ -n "$VERSION" ] || die "module.prop 缺少 version"
  [ -n "$CODE" ] || die "module.prop 缺少 versionCode"

  problems=0
  note() { printf '  ✗ %-42s %s\n' "$1" "$2"; problems=$((problems + 1)); }

  printf '权威来源: %s  version=%s  versionCode=%s\n\n' "$PROP" "$VERSION" "$CODE"

  # 1) action.sh 显示版本
  grep -q "当前版本: $VERSION" "$ACTION" || note "$ACTION" "未同步（期望 当前版本: $VERSION）"
  # 2) customize.sh 安装时显示版本（全角冒号）
  grep -q "当前版本：$VERSION" "$CUSTOMIZE" || note "$CUSTOMIZE" "未同步（期望 当前版本：$VERSION）"
  # 3) verify_module.py 期望的 version
  grep -q "\"version=$VERSION" "$VERIFY" || note "$VERIFY" "version 期望值未同步"
  # 4) verify_module.py 期望的 versionCode
  grep -q "\"versionCode=$CODE" "$VERIFY" || note "$VERIFY" "versionCode 期望值未同步"
  # 5) verify_module.py 对 action.sh 的断言
  grep -q "当前版本: $VERSION" "$VERIFY" || note "$VERIFY" "action.sh 版本断言未同步"
  # 6) buildinfo.go 默认版本
  grep -q "DefaultVersion    = \"$VERSION\"" "$BUILDINFO" || note "$BUILDINFO" "DefaultVersion 未同步"
  # 7) 构建脚本不应再硬编码版本（必须从 module.prop 读）
  if grep -qE '^VERSION=\$\{VERSION:-[0-9]' "$BUILD"; then
    note "$BUILD" "仍在硬编码默认版本，应与 module.prop 解耦"
  fi
  # 8) CHANGELOG 应存在对应版本段
  grep -q "^## $VERSION" "$CHANGELOG" || note "$CHANGELOG" "缺少 '## $VERSION' 段落"

  if [ "$problems" -eq 0 ]; then
    printf '全部一致 ✅\n'
    exit 0
  fi
  printf '\n共 %d 处不一致\n' "$problems" >&2
  exit 1
fi

# ---------- 修改模式 ----------
NEW_VERSION="${1:-}"
[ -n "$NEW_VERSION" ] || die "用法: bump-version.sh <version> [versionCode]  或  --check / --current"
case "$NEW_VERSION" in
  [0-9]*.[0-9]*) ;;
  *) die "版本号格式无效：$NEW_VERSION（期望形如 0.04）" ;;
esac

if [ -n "${2:-}" ]; then
  NEW_CODE="$2"
else
  # 日期式：YYYYMMDD + 两位当日序号
  TODAY=$(date -u '+%Y%m%d')
  EXISTING=$(read_prop versionCode)
  SEQ=1
  case "$EXISTING" in
    "$TODAY"*)  # 同一天已有构建，序号递增
      suffix=${EXISTING#"$TODAY"}
      case "$suffix" in
        [0-9][0-9]) SEQ=$((10#$suffix + 1)) ;;
      esac
      ;;
  esac
  [ "$SEQ" -le 99 ] || die "当日构建次数超过 99，请显式指定 versionCode"
  NEW_CODE=$(printf '%s%02d' "$TODAY" "$SEQ")
fi

case "$NEW_CODE" in
  *[!0-9]*) die "versionCode 必须是纯数字：$NEW_CODE" ;;
esac

OLD_VERSION=$(read_prop version)
OLD_CODE=$(read_prop versionCode)
printf '版本更新: %s (%s)  →  %s (%s)\n\n' "$OLD_VERSION" "$OLD_CODE" "$NEW_VERSION" "$NEW_CODE"

# 1) module.prop
sed -i "s/^version=.*/version=$NEW_VERSION/" "$PROP"
sed -i "s/^versionCode=.*/versionCode=$NEW_CODE/" "$PROP"

# 2) action.sh（半角冒号）
sed -i "s/^zcr_print '当前版本: .*'/zcr_print '当前版本: $NEW_VERSION'/" "$ACTION"

# 3) customize.sh（全角冒号）
sed -i "s|^zcr_ui \"- 当前版本：.*\"|zcr_ui \"- 当前版本：$NEW_VERSION\"|" "$CUSTOMIZE"

# 4/5) verify_module.py 的期望值与断言
python3 - "$VERIFY" "$NEW_VERSION" "$NEW_CODE" <<'PY'
import re, sys, pathlib
path, ver, code = sys.argv[1], sys.argv[2], sys.argv[3]
p = pathlib.Path(path); s = p.read_text(encoding='utf-8')
s = re.sub(r'"version=[^"]*\\n"', f'"version={ver}\\\\n"', s, count=1)
s = re.sub(r'"versionCode=[^"]*\\n"', f'"versionCode={code}\\\\n"', s, count=1)
s = re.sub(r'当前版本: [0-9][^"]*(?=" not in action)', f'当前版本: {ver}', s, count=1)
p.write_text(s, encoding='utf-8')
PY

# 6) buildinfo.go
sed -i "s/^\(\s*DefaultVersion\s*=\s*\)\"[^\"]*\"/\1\"$NEW_VERSION\"/" "$BUILDINFO"

# 7) CHANGELOG：在 "# 更新日志" 之后、第一个 "## " 之前插入新段
if ! grep -q "^## $NEW_VERSION" "$CHANGELOG"; then
  TODAY_ISO=$(date -u '+%Y-%m-%d')
  python3 - "$CHANGELOG" "$NEW_VERSION" "$TODAY_ISO" <<'PY'
import sys, pathlib
path, ver, day = sys.argv[1], sys.argv[2], sys.argv[3]
p = pathlib.Path(path); lines = p.read_text(encoding='utf-8').split('\n')
out, inserted = [], False
for i, ln in enumerate(lines):
    out.append(ln)
    if not inserted and ln.startswith('# '):
        # 跳过紧随标题的空行，再插入
        pass
    if not inserted and ln.startswith('## '):
        out.insert(len(out) - 1, f'## {ver}\n\n_（{day}）_\n')
        inserted = True
if not inserted:
    out.append(f'\n## {ver}\n\n_（{day}）_\n')
p.write_text('\n'.join(out), encoding='utf-8')
PY
  echo "已在 CHANGELOG.md 插入 '## $NEW_VERSION' 段落（请补充变更说明）"
fi

printf '\n=== 同步结果 ===\n'
printf '  %-38s %s\n' "$PROP" "$(read_prop version) / $(read_prop versionCode)"
printf '  %-38s %s\n' "$ACTION" "$(grep -o '当前版本: .*' "$ACTION" | head -1)"
printf '  %-38s %s\n' "$CUSTOMIZE" "$(grep -o '当前版本：.*' "$CUSTOMIZE" | head -1)"
printf '  %-38s %s\n' "$VERIFY" "$(grep -o 'version=[0-9.]*' "$VERIFY" | head -1) / $(grep -o 'versionCode=[0-9]*' "$VERIFY" | head -1)"
printf '  %-38s %s\n' "$BUILDINFO" "$(grep -o 'DefaultVersion *= *"[^"]*"' "$BUILDINFO" | head -1)"
printf '\n运行 bash scripts/bump-version.sh --check 确认一致性。\n'
