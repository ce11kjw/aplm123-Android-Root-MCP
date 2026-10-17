#!/usr/bin/env python3
"""Static universal Root module acceptance checks."""

from __future__ import annotations

import argparse
import hashlib
import io
import pathlib
import stat
import struct
import zipfile


ABIS = ("arm64-v8a", "armeabi-v7a", "x86_64")
ELF_IDENTITIES = {
    "arm64-v8a": (2, 183),
    "armeabi-v7a": (1, 40),
    "x86_64": (2, 62),
}
ANDROID_PAGE_SIZE = 16384
REQUIRED = (
    "module.prop",
    "customize.sh",
    "service.sh",
    "action.sh",
    "uninstall.sh",
    "META-INF/com/google/android/update-binary",
    "META-INF/com/google/android/updater-script",
)
LEGAL_REQUIRED = (
    "LICENSE",
    "NOTICE",
)
LICENSE_SHA256 = "fb981668c18a279e285fc4d83fba1e836cc84dd4daa73c9697d3cfd2d8aca6e0"
# 本地改造相对上游新增的条目：
#   README.md          模块安装说明（面向用户）
#   webroot/index.html KernelSU/APatch 管理器 WebUI 入口
EXTRA_ENTRIES = ("README.md", "webroot/index.html")
# 官方校验脚本一律拒绝 ZIP 里出现 .md/.txt 散落文件，但上面这些是我们有意添加的。
ALLOWED_TEXT_ENTRIES = {"README.md"}
EXPECTED_ENTRIES = set(
    REQUIRED
    + LEGAL_REQUIRED
    + ("common.sh", "post-fs-data.sh", "systemless.sh", "update.sh")
    + EXTRA_ENTRIES
    + tuple(f"bin/{abi}/{binary}" for abi in ABIS for binary in ("aplm123d", "7zz"))
)
NOTICE_SECTIONS = {
    "THIRD_PARTY_NOTICES": "2aa145dbe7c71d1f1c7fda3388d462140bc7c3cab4dc731a4fb0fe6483fd305e",
    "7ZIP_LICENSE": "21ac9ac12b25c8cb7afaf19b2c9ccdf19398c9f84cb691859e173a02426254df",
    "LGPL_2_1": "a1a33180d02960ab1c5de36cf20b1a2f0fe9888d83826ad263da5db52f1b183b",
    "CODER_WEBSOCKET": "f5b036075a5acd58de760ea87d94161d0e9d61593ed7ec3f16680450f095944d",
    "MODELCONTEXTPROTOCOL_GO_SDK": "d426d9220d23d163453059ef4d79a327f3253e9ae76c7ea87036a4fe57c1d783",
    "ULIKUNITZ_XZ": "bd2eccb7489d183d2cd2cf17a61d259dd2fede1a2b406fe410b6d8e6ca138129",
    "GOOGLE_JSONSCHEMA_GO": "0fbd0ba52f6c09ab5aa6f5341c9c158e21dd9b5f23a6a835c40af88caecc184e",
    "SEGMENTIO_ASM": "8ead3a576df03654e934d426cb2e100a6ba87cb7cc10f5f63e7a82f6327d8fe9",
    "SEGMENTIO_ENCODING": "f908c9962b8c37d9c0a0da415eb3774dff30f5747ed86c147c45f09b9f154599",
    "YOSIDA95_URITEMPLATE": "881631f6023fd4cfb0b21a60e53c0c824cac18ed15baeda01c84334d168a3968",
    "GOLANG_X_NET": "22975ebe413674bc619428a26e53412013dccffa69e2f27100dbd8d946fde287",
    "GOLANG_X_OAUTH2": "22975ebe413674bc619428a26e53412013dccffa69e2f27100dbd8d946fde287",
    "GOLANG_X_SYNC": "22975ebe413674bc619428a26e53412013dccffa69e2f27100dbd8d946fde287",
    "GOLANG_X_SYS": "22975ebe413674bc619428a26e53412013dccffa69e2f27100dbd8d946fde287",
    "GOLANG_X_TEXT": "22975ebe413674bc619428a26e53412013dccffa69e2f27100dbd8d946fde287",
    "GOLANG_X_TIME": "22975ebe413674bc619428a26e53412013dccffa69e2f27100dbd8d946fde287",
}


def verify_android_elf(data: bytes, name: str, abi: str) -> None:
    if len(data) < 20 or data[:4] != b"\x7fELF":
        raise SystemExit(f"{name} 不是非空 ELF")
    elf_class = data[4]
    endian = data[5]
    expected_class, expected_machine = ELF_IDENTITIES[abi]
    if elf_class != expected_class or endian not in (1, 2):
        raise SystemExit(f"{name} ELF class/endian 与 {abi} 不匹配")
    order = "<" if endian == 1 else ">"
    try:
        e_type, e_machine = struct.unpack_from(order + "HH", data, 16)
        if elf_class == 2:
            if len(data) < 64:
                raise struct.error("ELF64 header truncated")
            phoff = struct.unpack_from(order + "Q", data, 32)[0]
            phentsize, phnum = struct.unpack_from(order + "HH", data, 54)
            program_format = order + "IIQQQQQQ"
        else:
            if len(data) < 52:
                raise struct.error("ELF32 header truncated")
            phoff = struct.unpack_from(order + "I", data, 28)[0]
            phentsize, phnum = struct.unpack_from(order + "HH", data, 42)
            program_format = order + "IIIIIIII"
    except struct.error as error:
        raise SystemExit(f"{name} ELF 头无效: {error}") from error
    if e_type != 3:
        raise SystemExit(f"{name} 不是 ET_DYN PIE: e_type={e_type}")
    if e_machine != expected_machine:
        raise SystemExit(f"{name} e_machine={e_machine}，期望 {expected_machine}")
    expected_entry_size = struct.calcsize(program_format)
    if phnum == 0 or phentsize < expected_entry_size:
        raise SystemExit(f"{name} program header 表无效")
    if phoff > len(data) or phnum > (len(data) - phoff) // phentsize:
        raise SystemExit(f"{name} program header 表越界")
    load_count = 0
    for index in range(phnum):
        start = phoff + index * phentsize
        fields = struct.unpack_from(program_format, data, start)
        if fields[0] != 1:
            continue
        load_count += 1
        if elf_class == 2:
            _, _, offset, virtual_address, _, _, _, alignment = fields
        else:
            _, offset, virtual_address, _, _, _, _, alignment = fields
        if alignment < ANDROID_PAGE_SIZE:
            raise SystemExit(
                f"{name} LOAD[{index}] p_align={alignment} 小于 {ANDROID_PAGE_SIZE}"
            )
        if offset % ANDROID_PAGE_SIZE != virtual_address % ANDROID_PAGE_SIZE:
            raise SystemExit(f"{name} LOAD[{index}] 不满足 16 KiB 同余")
    if load_count == 0:
        raise SystemExit(f"{name} 没有 PT_LOAD 段")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("zip")
    args = parser.parse_args()
    path = pathlib.Path(args.zip)
    if not path.is_file():
        raise SystemExit(f"ZIP 不存在: {path}")
    with zipfile.ZipFile(path) as archive:
        entry_names = archive.namelist()
        names = set(entry_names)
        if len(entry_names) != len(names):
            raise SystemExit("ZIP 含重复条目")
        for name in REQUIRED + LEGAL_REQUIRED:
            if name not in names:
                raise SystemExit(f"ZIP 缺少 {name}")
            if not archive.read(name):
                raise SystemExit(f"ZIP 中 {name} 为空")
        text_entries = [
            name
            for name in names
            if pathlib.PurePosixPath(name).suffix.lower() in {".txt", ".md"}
            and name not in ALLOWED_TEXT_ENTRIES
        ]
        if text_entries:
            raise SystemExit(f"ZIP 仍含分散文本文件: {sorted(text_entries)}")
        if names != EXPECTED_ENTRIES:
            missing = sorted(EXPECTED_ENTRIES - names)
            unexpected = sorted(names - EXPECTED_ENTRIES)
            raise SystemExit(f"ZIP 条目集合不精确: missing={missing} unexpected={unexpected}")
        if hashlib.sha256(archive.read("LICENSE")).hexdigest() != LICENSE_SHA256:
            raise SystemExit("LICENSE 与项目固定许可证原文不一致")
        notice = archive.read("NOTICE")
        for label, expected_digest in NOTICE_SECTIONS.items():
            begin = f"===== BEGIN {label} =====\n".encode("utf-8")
            end = f"\n===== END {label} =====".encode("utf-8")
            if notice.count(begin) != 1 or notice.count(end) != 1:
                raise SystemExit(f"NOTICE 的 {label} 分段缺失或重复")
            start = notice.index(begin) + len(begin)
            stop = notice.index(end, start)
            if hashlib.sha256(notice[start:stop]).hexdigest() != expected_digest:
                raise SystemExit(f"NOTICE 的 {label} 分段与固定许可证原文不一致")
        prop_data = archive.read("module.prop")
        prop_digest = hashlib.sha256(prop_data).hexdigest().encode("ascii")
        for abi in ABIS:
            for binary in ("aplm123d", "7zz"):
                name = f"bin/{abi}/{binary}"
                if name not in names:
                    raise SystemExit(f"ZIP 缺少 {name}")
                data = archive.read(name)
                verify_android_elf(data, name, abi)
                mode = archive.getinfo(name).external_attr >> 16
                if mode & stat.S_IXUSR == 0:
                    raise SystemExit(f"{name} 没有执行位")
                if binary == "aplm123d" and prop_digest not in data:
                    raise SystemExit(f"{name} 未内置 module.prop SHA-256")
        for name in names:
            if name.endswith(".sh") or name.endswith("update-binary") or name.endswith("updater-script"):
                script_data = archive.read(name)
                if b"\r\n" in script_data:
                    raise SystemExit(f"{name} 不是 LF 换行")
                for index, line in enumerate(script_data.decode("utf-8").splitlines()):
                    stripped = line.lstrip()
                    if not stripped.startswith("#"):
                        continue
                    if index == 0 and (stripped.startswith("#!") or stripped == "#MAGISK"):
                        continue
                    raise SystemExit(f"{name} 仍含注释: {line}")
        prop = prop_data.decode("utf-8")
        # version / versionCode 随发布递增，不写死在脚本里：
        # 从 module.prop 自身取值，再校验其余字段与固定文案一致。
        prop_version = ""
        prop_version_code = ""
        for line in prop.splitlines():
            if line.startswith("version="):
                prop_version = line[len("version="):]
            elif line.startswith("versionCode="):
                prop_version_code = line[len("versionCode="):]
        if not prop_version or not prop_version_code:
            raise SystemExit("module.prop 缺少 version 或 versionCode")
        if not prop_version_code.isdigit():
            raise SystemExit("module.prop 的 versionCode 必须是数字")
        expected_prop = (
            "id=aplm123.root.mcp\n"
            "name=aplm123 Root MCP\n"
            f"version={prop_version}\n"
            f"versionCode={prop_version_code}\n"
            "author=__AUTHOR__\n"
            "description=通用 root MCP 扩展，内置管理器 WebUI 与浏览器控制台（让AI管理手机）\n"
        )
        # module.prop 的 author 是自定义值，这里只校验其余字段固定。
        import re as _re
        normalized = _re.sub(r"^author=.*$", "author=__AUTHOR__", prop, flags=_re.M)
        if normalized != expected_prop:
            raise SystemExit("module.prop 元数据与指定内容不一致")
        action = archive.read("action.sh").decode("utf-8")
        for forbidden in ("音量加", "音量减", "查看 MCP 运行状态", "启动服务", "恢复默认配置"):
            if forbidden in action:
                raise SystemExit("action.sh 仍含旧操作菜单")
        # 安装脚本不应再包含原作者署名或社群跳转。
        for forbidden in ("小骨", "Xiaogu", "tg://", "telegram"):
            if forbidden in action:
                raise SystemExit(f"action.sh 仍含原作者信息: {forbidden}")
        # action.sh 必须从 module.prop 动态显示版本，而不是写死字面量。
        if "当前版本: $zcr_version" not in action:
            raise SystemExit("action.sh 未动态显示版本")
        if 'sed -n \'s/^version=//p\'' not in action:
            raise SystemExit("action.sh 未从 module.prop 读取版本")
        if "zcr_print_summary" not in action:
            raise SystemExit("action.sh 未保留运行摘要")
        common = archive.read("common.sh").decode("utf-8")
        if "/data/adb/aplm123-mcp/MCP地址.txt" not in common:
            raise SystemExit("未生成指定 MCP 地址文件")
        if "局域网地址：当前未获取" in common:
            raise SystemExit("地址文件仍会写入未获取占位值")
        for child in ("downloads", "uploads", "scripts", "projects", "output", "backups", "packages", "modules"):
            if f'"$ZCR_WORK_DIR/{child}"' in common:
                raise SystemExit(f"安装脚本仍会预创建工作区子目录: {child}")
        customize = archive.read("customize.sh").decode("utf-8")
        for forbidden in ("小骨", "Xiaogu", "tg://", "telegram"):
            if forbidden in customize:
                raise SystemExit(f"customize.sh 仍含原作者信息或社群跳转: {forbidden}")
        if "sepolicy.rule" in names:
            raise SystemExit("模块仍包含仅注释的 sepolicy.rule")
        if any(name == "licenses/" or name.startswith("licenses/") for name in names):
            raise SystemExit("模块仍包含 licenses 子目录")
        if "post-fs-data.sh" in names:
            script = archive.read("post-fs-data.sh").decode("utf-8")
            forbidden = ("sleep 30", "aplm123d supervisor", "setenforce 0")
            if any(item in script for item in forbidden):
                raise SystemExit("post-fs-data.sh 含阻塞启动或禁用 SELinux")
    print(f"OK {path}")


if __name__ == "__main__":
    main()
