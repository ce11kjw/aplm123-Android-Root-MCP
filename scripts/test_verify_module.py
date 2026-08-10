#!/usr/bin/env python3

from __future__ import annotations

import argparse
import pathlib
import subprocess
import sys
import tempfile
import warnings
import zipfile


def rewrite(source: pathlib.Path, target: pathlib.Path, mode: str) -> None:
    with zipfile.ZipFile(source) as incoming, zipfile.ZipFile(target, "w") as outgoing:
        for info in incoming.infolist():
            data = incoming.read(info.filename)
            if mode == "changed_notice" and info.filename == "NOTICE":
                marker = b"===== BEGIN LGPL_2_1 =====\n"
                offset = data.index(marker) + len(marker)
                data = data[:offset] + bytes((data[offset] ^ 1,)) + data[offset + 1 :]
            if mode == "changed_license" and info.filename == "LICENSE":
                data = bytes((data[0] ^ 1,)) + data[1:]
            outgoing.writestr(info, data)
        if mode == "extra_text":
            outgoing.writestr("extra.txt", b"forbidden")
        if mode == "duplicate_entry":
            with warnings.catch_warnings():
                warnings.simplefilter("ignore", UserWarning)
                outgoing.writestr("action.sh", b"duplicate")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("zip")
    args = parser.parse_args()
    source = pathlib.Path(args.zip).resolve()
    verifier = pathlib.Path(__file__).with_name("verify_module.py")
    expected = {
        "changed_notice": "NOTICE 的 LGPL_2_1 分段与固定许可证原文不一致",
        "changed_license": "LICENSE 与项目固定许可证原文不一致",
        "extra_text": "ZIP 仍含分散文本文件",
        "duplicate_entry": "ZIP 含重复条目",
    }
    with tempfile.TemporaryDirectory(prefix="zcr521-module-negative-") as directory:
        root = pathlib.Path(directory)
        for mode, message in expected.items():
            candidate = root / f"{mode}.zip"
            rewrite(source, candidate, mode)
            result = subprocess.run(
                [sys.executable, str(verifier), str(candidate)],
                check=False,
                capture_output=True,
                text=True,
            )
            output = result.stdout + result.stderr
            if result.returncode == 0 or message not in output:
                raise SystemExit(f"{mode} negative gate failed: code={result.returncode} output={output!r}")
            print(f"OK reject {mode}")


if __name__ == "__main__":
    main()
