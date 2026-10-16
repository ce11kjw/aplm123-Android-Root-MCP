#!/usr/bin/env python3
"""Create the deterministic flashable module ZIP."""

from __future__ import annotations

import argparse
import datetime as dt
import pathlib
import re
import shutil
import zipfile


# 默认三架构；但在缺少 NDK 交叉工具链时，允许通过 --abis 只打包实际构建出的
# 架构（例如只编了 arm64-v8a）。customize.sh 会按设备架构挑选对应的 bin/<abi>。
ABIS = ("arm64-v8a", "armeabi-v7a", "x86_64")


def normalized_mode(relative: str) -> int:
    name = pathlib.PurePosixPath(relative)
    if (
        relative.endswith(".sh")
        or str(name).startswith("bin/")
        or relative.endswith("update-binary")
    ):
        return 0o755
    return 0o644


def deterministic_zip(source: pathlib.Path, destination: pathlib.Path, epoch: int) -> None:
    timestamp = dt.datetime.fromtimestamp(max(epoch, 315532800), dt.timezone.utc)
    zip_time = (
        timestamp.year,
        timestamp.month,
        timestamp.day,
        timestamp.hour,
        timestamp.minute,
        timestamp.second,
    )
    entries = sorted(
        (path.relative_to(source).as_posix(), path)
        for path in source.rglob("*")
        if path.is_file()
    )
    with zipfile.ZipFile(
        destination,
        "w",
        compression=zipfile.ZIP_DEFLATED,
        compresslevel=9,
    ) as archive:
        for relative, path in entries:
            info = zipfile.ZipInfo(relative, zip_time)
            info.create_system = 3
            info.external_attr = (normalized_mode(relative) & 0xFFFF) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            with path.open("rb") as content:
                with archive.open(info, "w", force_zip64=True) as target:
                    shutil.copyfileobj(content, target, length=1024 * 1024)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--module-stage", required=True)
    parser.add_argument("--dist", required=True)
    parser.add_argument("--version", required=True)
    parser.add_argument("--epoch", required=True, type=int)
    parser.add_argument(
        "--abis",
        default=",".join(ABIS),
        help="逗号分隔的 ABI 列表；默认全部三架构。",
    )
    parser.add_argument(
        "--name-suffix",
        default="universal",
        help="产物文件名中的架构后缀，默认 universal。",
    )
    args = parser.parse_args()

    module = pathlib.Path(args.module_stage).resolve()
    dist = pathlib.Path(args.dist).resolve()
    dist.mkdir(parents=True, exist_ok=True)
    selected_abis = tuple(part.strip() for part in args.abis.split(",") if part.strip())
    if not selected_abis:
        raise SystemExit("--abis 不能为空")
    unknown = [abi for abi in selected_abis if abi not in ABIS]
    if unknown:
        raise SystemExit(f"未知 ABI: {', '.join(unknown)}")

    if any(path.is_file() for path in (module / "licenses").rglob("*")):
        raise SystemExit("模块源码仍包含 licenses 子目录")

    for abi in selected_abis:
        for binary in ("aplm123d", "7zz"):
            candidate = module / "bin" / abi / binary
            if not candidate.is_file() or candidate.stat().st_size == 0:
                raise SystemExit(f"缺少真实构建产物: {candidate}")
            if candidate.read_bytes()[:4] != b"\x7fELF":
                raise SystemExit(f"Android 产物不是 ELF: {candidate}")

    # 未选中的 ABI 目录必须移除，否则 customize.sh 会在安装时校验失败。
    for abi in ABIS:
        if abi in selected_abis:
            continue
        stale = module / "bin" / abi
        if stale.exists():
            shutil.rmtree(stale)

    cleanup_patterns = (
        re.compile(r"^aplm123-Android-Root-MCP-v.*\.(?:zip|json)$"),
        re.compile(r"^aplm123-bridge-"),
        re.compile(r"^(?:tools\.json|SHA256SUMS)$"),
    )
    for path in dist.iterdir():
        if path.is_file() and any(pattern.search(path.name) for pattern in cleanup_patterns):
            path.unlink()

    module_zip = dist / f"aplm123-Android-Root-MCP-v{args.version}-{args.name_suffix}.zip"
    deterministic_zip(module, module_zip, args.epoch)
    print(module_zip)


if __name__ == "__main__":
    main()
