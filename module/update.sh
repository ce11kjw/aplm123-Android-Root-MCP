#!/system/bin/sh
#
# 模块热更新：安装完成后由 customize.sh 后台调用。
#
# ── 设计约束（改动前务必先读，这些是踩过的坑）─────────────────────────
#
# 1) 绝不用 `mv` 替换整个模块目录。
#    Root 管理器（Magisk / KernelSU / APatch）按路径记录模块状态与开机自启
#    登记。把 $MOD_DIR 整目录挪走再挪回来，会让这些引用失效，表现为
#    「装上了但起不来」——模块列表里看着在，实际开机不拉起。
#    这里一律逐文件覆盖，模块目录就地更新。
#
# 2) 健康检查必须校验 /version 的版本号，不能只看进程存活。
#    崩溃重启的进程同样有 pid，只看 pid 会把失败误判成成功，从而跳过回滚。
#
# 3) busybox 要多路径探测。
#    开机 early stage PATH 里可能没有 busybox；APatch / KernelSU / Magisk
#    三家的自带位置不同。
#
# 4) 任何失败都要能逐文件还原到更新前状态，并重新拉起旧版本。
#
set -u

SRC="${1:-}"
MOD_DIR="${APLM_HOTUPDATE_TARGET:-/data/adb/modules/aplm123.root.mcp}"
BACKUP_DIR="${APLM_HOTUPDATE_BACKUP:-/data/adb/aplm-hotupdate-backup}"
STATE_DIR=/data/adb/aplm123-mcp
LOG="$STATE_DIR/logs/hot-update.log"
PORT=5322
DRY_RUN="${APLM_HOTUPDATE_DRY_RUN:-0}"

# ---------- busybox 多路径探测 ----------
BB=""
for cand in /data/adb/ap/bin/busybox /data/adb/ksu/bin/busybox /data/adb/magisk/busybox; do
  [ -x "$cand" ] && BB="$cand" && break
done
if [ -z "$BB" ] && command -v busybox >/dev/null 2>&1; then BB=busybox; fi

mkdir -p "$STATE_DIR/logs" 2>/dev/null
log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >>"$LOG" 2>/dev/null; }
die() { log "错误: $*"; exit 1; }

[ -n "$SRC" ] || die "未提供源目录"
[ -f "$SRC/module.prop" ] || die "源目录缺少 module.prop: $SRC"
[ -d "$MOD_DIR" ] || die "目标模块目录不存在: $MOD_DIR"

NEW_VER=$(sed -n 's/^version=//p' "$SRC/module.prop" | head -n 1)
OLD_VER=$(sed -n 's/^version=//p' "$MOD_DIR/module.prop" 2>/dev/null | head -n 1)
log "=== 热更新开始: ${OLD_VER:-unknown} -> ${NEW_VER:-unknown} (dry-run=$DRY_RUN) ==="

# ---------- 等安装器落盘稳定 ----------
sleep 2
prev=""; stable=0; i=0
while [ "$i" -lt 8 ]; do
  if [ -n "$BB" ]; then
    cur=$($BB du -sk "$SRC" 2>/dev/null | $BB awk '{print $1}')
  else
    cur=$(du -sk "$SRC" 2>/dev/null | awk '{print $1}')
  fi
  if [ -n "$cur" ] && [ "$cur" = "$prev" ]; then
    stable=$((stable + 1)); [ "$stable" -ge 2 ] && break
  else
    stable=0
  fi
  prev="$cur"; i=$((i + 1)); sleep 1
done
log "源目录已稳定（${prev:-?}KB）"

# ---------- 选 ABI ----------
ARCH=$(uname -m 2>/dev/null)
case "$ARCH" in
  arm64|arm64-v8a|aarch64) ABI=arm64-v8a ;;
  arm|armeabi-v7a|armv7l|armv8l) ABI=armeabi-v7a ;;
  x64|x86_64|amd64) ABI=x86_64 ;;
  *) die "不支持的架构: $ARCH" ;;
esac

if [ -d "$SRC/bin/$ABI" ]; then
  [ -s "$SRC/bin/$ABI/aplm123d" ] || die "缺少 bin/$ABI/aplm123d"
  BIN_SRC="$SRC/bin/$ABI"
  log "架构 $ARCH -> bin/$ABI"
else
  [ -s "$SRC/bin/aplm123d" ] || die "包内缺少 aplm123d"
  BIN_SRC="$SRC/bin"
  log "架构 $ARCH -> bin/（旧包结构）"
fi

if [ "$DRY_RUN" = "1" ]; then
  log "[dry-run] 计划覆盖的文件："
  ( cd "$SRC" && find . -type f -not -path './bin/*' ) | while read -r f; do
    log "  ${f#./}"
  done
  log "  二进制: $BIN_SRC/aplm123d （及 7zz 若存在）"
  log "=== dry-run 结束 ==="
  exit 0
fi

# ---------- 备份（逐文件） ----------
rm -rf "$BACKUP_DIR" 2>/dev/null
mkdir -p "$BACKUP_DIR" || die "无法创建备份目录"
cp -a "$MOD_DIR/." "$BACKUP_DIR/" || die "备份当前模块失败"
log "已备份到 $BACKUP_DIR"

restore_all() {
  log "→ 逐文件还原"
  ( cd "$BACKUP_DIR" && find . -type f ) | while read -r f; do
    rel="${f#./}"
    dst="$MOD_DIR/$rel"
    mkdir -p "$(dirname "$dst")" 2>/dev/null
    cp -f "$BACKUP_DIR/$rel" "$dst" 2>/dev/null || log "  还原失败: $rel"
  done
  chmod 0755 "$MOD_DIR"/*.sh 2>/dev/null
  chmod 0755 "$MOD_DIR/bin/aplm123d" "$MOD_DIR/bin/7zz" 2>/dev/null
  log "还原完成"
}

restart_daemon() {
  pids=$(pidof aplm123d 2>/dev/null)
  if [ -n "$pids" ]; then
    log "停止 aplm123d ($pids)"
    pkill -TERM -x aplm123d 2>/dev/null
    n=0
    while [ "$n" -lt 12 ]; do
      [ -z "$(pidof aplm123d 2>/dev/null)" ] && break
      n=$((n + 1)); sleep 1
    done
    if [ -n "$(pidof aplm123d 2>/dev/null)" ]; then
      log "TERM 未退出，改用 KILL"
      pkill -KILL -x aplm123d 2>/dev/null
      sleep 1
    fi
  fi
  [ -f "$MOD_DIR/service.sh" ] || { log "缺少 service.sh"; return 1; }
  sh "$MOD_DIR/service.sh" >>"$LOG" 2>&1
  log "已调用 service.sh"
  return 0
}

# 期望版本必须匹配，否则视为不健康
wait_healthy() {
  want="$1"
  n=0
  while [ "$n" -lt 30 ]; do
    if [ -n "$(pidof aplm123d 2>/dev/null)" ]; then
      out=""
      if [ -n "$BB" ]; then
        out=$($BB wget -q -O - --timeout=3 "http://127.0.0.1:$PORT/version" 2>/dev/null)
      fi
      case "$out" in
        *'"version"'*)
          got=$(echo "$out" | sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')
          log "健康检查: 已响应，版本=$got（期望=${want:-任意}）"
          if [ -z "$want" ] || [ "$got" = "$want" ]; then
            return 0
          fi
          ;;
      esac
    fi
    n=$((n + 1)); sleep 1
  done
  log "健康检查超时或版本不符（30s）"
  return 1
}

# ---------- 覆盖非 bin/ 文件 ----------
FAILED=0
rm -f /tmp/.aplm-upd-fail 2>/dev/null
( cd "$SRC" && find . -type f -not -path './bin/*' ) | while read -r f; do
  rel="${f#./}"
  dst="$MOD_DIR/$rel"
  mkdir -p "$(dirname "$dst")" 2>/dev/null
  cp -f "$SRC/$rel" "$dst" 2>/dev/null || { log "  覆盖失败: $rel"; echo x >> /tmp/.aplm-upd-fail; }
done
[ -s /tmp/.aplm-upd-fail ] && FAILED=1
rm -f /tmp/.aplm-upd-fail 2>/dev/null
log "普通文件覆盖完成（失败=$FAILED）"

# ---------- 二进制原子替换 ----------
mkdir -p "$MOD_DIR/bin" 2>/dev/null
for name in aplm123d 7zz; do
  srcp="$BIN_SRC/$name"
  [ -s "$srcp" ] || { log "  跳过（源不存在）: $name"; continue; }
  if cp -f "$srcp" "$MOD_DIR/bin/$name.new" 2>/dev/null; then
    chmod 0755 "$MOD_DIR/bin/$name.new" 2>/dev/null
    if mv -f "$MOD_DIR/bin/$name.new" "$MOD_DIR/bin/$name" 2>/dev/null; then
      log "  已部署: bin/$name"
    else
      FAILED=1; log "  mv 失败: bin/$name"
    fi
  else
    FAILED=1; log "  复制失败: bin/$name"
  fi
done

# ---------- 清理包内 ABI 子目录 ----------
for d in arm64-v8a armeabi-v7a x86_64; do
  rm -rf "$MOD_DIR/bin/$d" 2>/dev/null
done

# ---------- 权限 ----------
chmod 0755 "$MOD_DIR" 2>/dev/null
for s in common.sh customize.sh post-fs-data.sh service.sh action.sh uninstall.sh systemless.sh update.sh; do
  [ -f "$MOD_DIR/$s" ] && chmod 0755 "$MOD_DIR/$s" 2>/dev/null
done
chmod 0755 "$MOD_DIR/bin/aplm123d" "$MOD_DIR/bin/7zz" 2>/dev/null
chmod 0644 "$MOD_DIR/module.prop" 2>/dev/null

if [ "$FAILED" = "1" ]; then
  restore_all
  restart_daemon
  die "存在文件覆盖失败，已还原"
fi

# ---------- 重启并校验 ----------
restart_daemon || { restore_all; restart_daemon; die "重启失败，已还原"; }

if wait_healthy "$NEW_VER"; then
  log "=== 热更新成功: ${OLD_VER:-?} -> ${NEW_VER:-?} ==="
  rm -rf "$BACKUP_DIR" 2>/dev/null
  exit 0
fi

log "新版本未通过健康检查，回滚"
restore_all
restart_daemon
if wait_healthy "$OLD_VER"; then
  log "已回滚到 ${OLD_VER:-?}，运行正常"
else
  log "回滚后仍不健康，需要手动介入"
fi
exit 1
