#!/usr/bin/env python3
"""Stage canonical LICENSE and NOTICE files for the flashable module."""

from __future__ import annotations

import argparse
import pathlib

SECTIONS = (
    ("THIRD_PARTY_NOTICES", "THIRD_PARTY_NOTICES.md"),
    ("7ZIP_LICENSE", "third_party/7zip/LICENSE-7ZIP.txt"),
    ("LGPL_2_1", "third_party/7zip/src/DOC/copying.txt"),
    ("CODER_WEBSOCKET", "vendor/github.com/coder/websocket/LICENSE.txt"),
    ("MODELCONTEXTPROTOCOL_GO_SDK", "vendor/github.com/modelcontextprotocol/go-sdk/LICENSE"),
    ("ULIKUNITZ_XZ", "vendor/github.com/ulikunitz/xz/LICENSE"),
    ("GOOGLE_JSONSCHEMA_GO", "vendor/github.com/google/jsonschema-go/LICENSE"),
    ("SEGMENTIO_ASM", "vendor/github.com/segmentio/asm/LICENSE"),
    ("SEGMENTIO_ENCODING", "vendor/github.com/segmentio/encoding/LICENSE"),
    ("YOSIDA95_URITEMPLATE", "vendor/github.com/yosida95/uritemplate/v3/LICENSE"),
    ("GOLANG_X_NET", "vendor/golang.org/x/net/LICENSE"),
    ("GOLANG_X_OAUTH2", "vendor/golang.org/x/oauth2/LICENSE"),
    ("GOLANG_X_SYNC", "vendor/golang.org/x/sync/LICENSE"),
    ("GOLANG_X_SYS", "vendor/golang.org/x/sys/LICENSE"),
    ("GOLANG_X_TEXT", "vendor/golang.org/x/text/LICENSE"),
    ("GOLANG_X_TIME", "vendor/golang.org/x/time/LICENSE"),
)


def normalized_bytes(path: pathlib.Path) -> bytes:
    return path.read_bytes().replace(b"\r\n", b"\n").replace(b"\r", b"\n")


def notice_license(path: pathlib.Path) -> bytes:
    return normalized_bytes(path).rstrip(b"\n")


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--repo", required=True)
    parser.add_argument("--license", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    repo = pathlib.Path(args.repo).resolve()
    license_output = pathlib.Path(args.license).resolve()
    notice_output = pathlib.Path(args.output).resolve()
    license_output.parent.mkdir(parents=True, exist_ok=True)
    notice_output.parent.mkdir(parents=True, exist_ok=True)

    license_source = repo / "LICENSE"
    if not license_source.is_file():
        raise SystemExit(f"Required license is missing: {license_source}")
    license_output.write_bytes(normalized_bytes(license_source))

    with notice_output.open("wb") as notice:
        for name, relative in SECTIONS:
            source = repo / relative
            if not source.is_file():
                raise SystemExit(f"Required legal notice is missing: {source}")
            notice.write(f"===== BEGIN {name} =====\n".encode("ascii"))
            notice.write(notice_license(source))
            notice.write(f"\n===== END {name} =====\n\n".encode("ascii"))


if __name__ == "__main__":
    main()
