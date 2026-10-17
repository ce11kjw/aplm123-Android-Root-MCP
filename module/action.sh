#!/system/bin/sh

MODDIR=${0%/*}
ZCR_MODDIR="$MODDIR"
. "$MODDIR/common.sh"

zcr_prepare_internal >/dev/null 2>&1 || true
zcr_write_mcp_address >/dev/null 2>&1 || true
zcr_version="$(sed -n 's/^version=//p' "$MODDIR/module.prop" 2>/dev/null | head -n 1)"
[ -n "$zcr_version" ] || zcr_version="unknown"
zcr_print "当前版本: $zcr_version"
zcr_print ''
zcr_print_summary
exit 0
