#!/system/bin/sh

MODDIR=${0%/*}
ZCR_MODDIR="$MODDIR"
. "$MODDIR/common.sh"

zcr_repo="32463991/ZCR521-AI-MCP"
zcr_current="$(zcr_module_version)"
zcr_latest=""
if zcr_command_exists curl; then
  zcr_latest="$(curl -fsSL --max-time 20 "https://api.github.com/repos/$zcr_repo/releases/latest" 2>/dev/null | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
elif zcr_command_exists wget; then
  zcr_latest="$(wget -qO- --timeout=20 "https://api.github.com/repos/$zcr_repo/releases/latest" 2>/dev/null | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
else
  zcr_print "内置更新需要 curl 或 wget"
  exit 1
fi
case "$zcr_latest" in
  v*)
    zcr_latest="${zcr_latest#v}"
    ;;
  *)
    zcr_print "获取最新版本失败"
    exit 1
    ;;
esac
if [ "$zcr_latest" = "$zcr_current" ]; then
  zcr_print "当前已是最新版本 $zcr_current"
  exit 0
fi
zcr_tmp="/data/local/tmp/zcr521-update-$$.zip"
if zcr_command_exists curl; then
  curl -fsSL --max-time 120 -o "$zcr_tmp" "https://github.com/$zcr_repo/releases/download/v$zcr_latest/ZCR521-Android-AI-MCP-v$zcr_latest-universal.zip"
else
  wget -qO "$zcr_tmp" --timeout=120 "https://github.com/$zcr_repo/releases/download/v$zcr_latest/ZCR521-Android-AI-MCP-v$zcr_latest-universal.zip"
fi
zcr_code=$?
if [ "$zcr_code" -ne 0 ] || [ ! -s "$zcr_tmp" ]; then
  rm -f "$zcr_tmp"
  zcr_print "下载更新包失败"
  exit 1
fi
zcr_prop="$("$ZCR_7ZZ" x -so "$zcr_tmp" module.prop 2>/dev/null)"
case "$zcr_prop" in
  *"id=zcr521.android.mcp"*)
    ;;
  *)
    rm -f "$zcr_tmp"
    zcr_print "更新包校验失败"
    exit 1
    ;;
esac
zcr_framework="$(zcr_detect_framework)"
case "$zcr_framework" in
  Magisk)
    magisk --install-module "$zcr_tmp"
    zcr_code=$?
    ;;
  KernelSU*)
    ksud module install "$zcr_tmp"
    zcr_code=$?
    ;;
  APatch)
    apd module install "$zcr_tmp"
    zcr_code=$?
    ;;
  *)
    rm -f "$zcr_tmp"
    zcr_print "未检测到可用的模块安装器"
    exit 1
    ;;
esac
rm -f "$zcr_tmp"
if [ "$zcr_code" -eq 0 ]; then
  zcr_print "已安装 $zcr_latest，请重启设备"
  exit 0
fi
zcr_print "安装失败（$zcr_code）"
exit "$zcr_code"