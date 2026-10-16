#!/system/bin/sh

MODDIR=${0%/*}
ZCR_MODDIR="$MODDIR"
. "$MODDIR/common.sh"

[ -e "$MODDIR/disable" ] && exit 0
[ -e "$MODDIR/remove" ] && exit 0

# 不要把输出丢进 /dev/null：zcr_start_supervisor 在拉起守护进程之前
# 有多处提前返回（目录不可建、二进制不可执行、锁被占等），
# 这些错误必须能从 boot.log 读到，否则启动失败时毫无痕迹。
zcr_start_supervisor auto >>"$ZCR_LOG_DIR/service.out" 2>&1 &
exit 0
