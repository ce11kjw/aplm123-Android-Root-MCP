#!/bin/sh
# 安装 git 本地钩子（当前只装 pre-commit 敏感信息扫描）。
#
# 新克隆仓库后执行一次：
#   sh scripts/install-hooks.sh
#
# 钩子文件保存在 scripts/hooks/ 下并纳入版本控制，
# 这样规则本身可审查、可追溯，而不是散落在各人 .git/hooks 里。

set -eu

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
HOOKS_DIR="$ROOT/.git/hooks"

if [ ! -d "$HOOKS_DIR" ]; then
    printf '%s\n' "未找到 .git/hooks，请在仓库根目录执行。" >&2
    exit 1
fi

for hook in "$ROOT"/scripts/hooks/*; do
    [ -f "$hook" ] || continue
    name=$(basename "$hook")
    cp "$hook" "$HOOKS_DIR/$name"
    chmod +x "$HOOKS_DIR/$name"
    printf '%s\n' "已安装钩子: $name"
done
