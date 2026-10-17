#!/system/bin/sh

MODDIR=${0%/*}
ZCR_MODDIR="$MODDIR"
. "$MODDIR/common.sh"

umask 077
zcr_prepare_internal || {
  mkdir -p /data/local/tmp 2>/dev/null
  printf '%s [error] post-fs-data: 无法创建内部目录 %s\n' \
    "$(date -u '+%Y-%m-%dT%H:%M:%SZ' 2>/dev/null || printf unknown)" \
    "$ZCR_INTERNAL_DIR" >> /data/local/tmp/aplm123-boot.log 2>/dev/null
  exit 0
}
zcr_boot_log "info" "post-fs-data 开始（boot_id=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)）"

zcr_existing_pid="$(zcr_read_pid 2>/dev/null)"
if [ -z "$zcr_existing_pid" ] || ! zcr_pid_is_supervisor "$zcr_existing_pid"; then
  rm -f "$ZCR_PID_FILE" "$ZCR_RUN_DIR/control.sock" "$ZCR_RUN_DIR/http.sock"
  rm -f "$ZCR_START_LOCK/owner" 2>/dev/null || true
  rmdir "$ZCR_START_LOCK" 2>/dev/null || true
fi

zcr_framework="$(zcr_detect_framework)"
zcr_tmp="$ZCR_INTERNAL_DIR/framework.tmp.$$"
{
  printf 'framework=%s\n' "$zcr_framework"
  printf 'version=%s\n' "$(zcr_framework_version)"
  printf 'android_api=%s\n' "$(getprop ro.build.version.sdk 2>/dev/null)"
  printf 'boot_id=%s\n' "$(cat /proc/sys/kernel/random/boot_id 2>/dev/null)"
} > "$zcr_tmp"
chmod 0600 "$zcr_tmp" 2>/dev/null || true
mv -f "$zcr_tmp" "$ZCR_INTERNAL_DIR/framework.info"

zcr_boot_log "info" "post-fs-data 完成（framework=$zcr_framework）"
exit 0
