#!/system/bin/sh
set -u

SRC="${1:-}"
MOD_DIR="${APLM_HOTUPDATE_TARGET:-/data/adb/modules/aplm123.root.mcp}"
BACKUP_DIR="${APLM_HOTUPDATE_BACKUP:-/data/adb/aplm-mcp-backup}"
NEW_DIR="${APLM_HOTUPDATE_NEW:-/data/adb/aplm-mcp-new}"
STATE_DIR=/data/adb/aplm123-mcp
LOG="$STATE_DIR/logs/hot-update.log"
PORT=5322
DRY_RUN="${APLM_HOTUPDATE_DRY_RUN:-0}"

if command -v busybox >/dev/null 2>&1; then BB=busybox
elif [ -x /data/adb/ap/bin/busybox ]; then BB=/data/adb/ap/bin/busybox
else BB=""; fi

mkdir -p "$STATE_DIR/logs" 2>/dev/null
log() { echo "$(date '+%Y-%m-%d %H:%M:%S') $*" >>"$LOG" 2>/dev/null; }

if [ -z "$SRC" ] || [ ! -f "$SRC/module.prop" ]; then
  log "错误：未提供有效的解压目录（$SRC）"
  exit 1
fi

NEW_VER=$(sed -n 's/^version=//p' "$SRC/module.prop" | head -n 1)
log "=== 热更新开始：目标 $MOD_DIR，新版本 ${NEW_VER:-unknown}，dry-run=$DRY_RUN ==="

sleep 3
prev=""
stable=0
i=0
while [ "$i" -lt 10 ]; do
  if [ -n "$BB" ]; then
    cur=$($BB du -sk "$SRC" 2>/dev/null | $BB awk '{print $1}')
  else
    cur=$(du -sk "$SRC" 2>/dev/null | awk '{print $1}')
  fi
  if [ -n "$cur" ] && [ "$cur" = "$prev" ]; then
    stable=$((stable + 1))
    [ "$stable" -ge 2 ] && break
  else
    stable=0
  fi
  prev="$cur"
  i=$((i + 1))
  sleep 1
done
log "等待安装器收尾完成（大小 ${prev:-?}KB，稳定 ${stable} 次）"

stop_daemon() {
  if [ "$DRY_RUN" = "1" ]; then log "[dry-run] 跳过停止进程"; return 0; fi
  pids=$(pidof aplm123d 2>/dev/null)
  if [ -z "$pids" ]; then log "没有在跑的 aplm123d"; return 0; fi
  log "停止 aplm123d（$pids）"
  pkill -TERM -x aplm123d 2>/dev/null
  n=0
  while [ "$n" -lt 15 ]; do
    [ -z "$(pidof aplm123d 2>/dev/null)" ] && break
    n=$((n + 1))
    sleep 1
  done
  if [ -n "$(pidof aplm123d 2>/dev/null)" ]; then
    log "TERM 后仍在，发 KILL"
    pkill -KILL -x aplm123d 2>/dev/null
    sleep 1
  fi
  return 0
}

start_daemon() {
  if [ "$DRY_RUN" = "1" ]; then log "[dry-run] 跳过启动进程"; return 0; fi
  if [ ! -f "$MOD_DIR/service.sh" ]; then log "错误：缺少 service.sh"; return 1; fi
  chmod 0755 "$MOD_DIR/service.sh" 2>/dev/null
  sh "$MOD_DIR/service.sh" >>"$LOG" 2>&1
  log "已调用 service.sh 拉起新版本"
  return 0
}

wait_healthy() {
  if [ "$DRY_RUN" = "1" ]; then log "[dry-run] 跳过健康检查"; return 0; fi
  if [ -z "$BB" ]; then log "没有 busybox，退化为只看进程"; sleep 6; pidof aplm123d >/dev/null 2>&1; return $?; fi
  n=0
  while [ "$n" -lt 25 ]; do
    if [ -n "$(pidof aplm123d 2>/dev/null)" ]; then
      out=$($BB wget -q -O - --timeout=3 "http://127.0.0.1:$PORT/version" 2>/dev/null)
      case "$out" in
        *'"version"'*) log "健康检查通过：$out"; return 0;;
      esac
    fi
    n=$((n + 1))
    sleep 1
  done
  log "健康检查超时（25s）"
  return 1
}

rollback() {
  log "→ 开始回滚"
  rm -rf "$MOD_DIR" 2>/dev/null
  if [ -d "$BACKUP_DIR" ]; then
    mv "$BACKUP_DIR" "$MOD_DIR" 2>/dev/null && log "已恢复备份目录"
  else
    log "错误：备份目录不存在，无法回滚"
  fi
  start_daemon
  wait_healthy || log "回滚后仍未通过健康检查，需要手动介入"
}

stop_daemon
sleep 1

rm -rf "$BACKUP_DIR" "$NEW_DIR" 2>/dev/null

if [ "$DRY_RUN" = "1" ]; then
  BACKUP_DIR=/data/local/tmp/aplm-dry-backup
  rm -rf "$BACKUP_DIR" "$NEW_DIR" 2>/dev/null
  log "[dry-run] 备份目录临时改为 $BACKUP_DIR"
fi

if ! mv "$MOD_DIR" "$BACKUP_DIR" 2>/dev/null; then
  log "mv 旧目录失败，改用 cp -a 备份"
  if ! cp -a "$MOD_DIR" "$BACKUP_DIR" 2>/dev/null; then
    log "错误：备份失败，放弃本次热更新（不动现网）"
    start_daemon
    exit 1
  fi
  rm -rf "$MOD_DIR" 2>/dev/null
fi

if ! cp -a "$SRC" "$NEW_DIR" 2>/dev/null; then
  log "错误：复制新目录失败"
  rollback
  exit 1
fi

if [ -n "$BB" ]; then
  $BB find "$NEW_DIR" -type d -exec chmod 755 {} \; 2>/dev/null
  $BB find "$NEW_DIR" -maxdepth 1 -name '*.sh' -exec chmod 755 {} \; 2>/dev/null
else
  chmod 755 "$NEW_DIR" 2>/dev/null
fi

if ! mv "$NEW_DIR" "$MOD_DIR" 2>/dev/null; then
  log "错误：替换模块目录失败"
  rollback
  exit 1
fi
log "模块目录已替换"

if ! start_daemon; then
  rollback
  exit 1
fi

if wait_healthy; then
  log "=== 热更新完成：${NEW_VER:-unknown} ==="
  [ "$DRY_RUN" = "1" ] || rm -rf "$BACKUP_DIR" 2>/dev/null
  exit 0
fi

log "新版本未通过健康检查"
rollback
exit 1
