#!/usr/bin/env python3
"""提交前敏感信息扫描 —— 防止凭据、真实聊天记录样本进入版本库。

设计目标：这是「绝不把敏感数据推到 GitHub」这条硬约束的执行点。
本地 pre-commit 钩子与 CI 共用同一份逻辑，避免两边规则漂移。

用法:
    python3 scripts/secret_scan.py            # 扫描已跟踪文件（默认）
    python3 scripts/secret_scan.py --staged   # 只扫描暂存区（pre-commit 用）
    python3 scripts/secret_scan.py --all      # 扫描全部历史（排查用）

退出码 0 表示干净，1 表示命中。命中时打印文件、行号与命中规则。
"""

from __future__ import annotations

import argparse
import pathlib
import re
import subprocess
import sys

# 第三方代码不属于我们的责任范围，且体量大、误报多。
EXCLUDE_PREFIXES = (
    "vendor/",
    "third_party/",
    "node_modules/",
    "dist/",
    "build/",
)

# --- 规则 1：已知凭据形态 ---
# 只匹配足以确定是真实密钥的长度与前缀，避免把示例文本判为泄漏。
SECRET_PATTERNS = (
    ("github-token", re.compile(r"\bghp_[A-Za-z0-9]{36}\b")),
    ("github-pat", re.compile(r"\bgithub_pat_[A-Za-z0-9_]{20,}\b")),
    ("openai-key", re.compile(r"\bsk-[A-Za-z0-9]{32,}\b")),
    ("anthropic-key", re.compile(r"\bsk-ant-[A-Za-z0-9_-]{20,}\b")),
    ("aws-key", re.compile(r"\bAKIA[0-9A-Z]{16}\b")),
    ("slack-token", re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{10,}\b")),
    ("private-key", re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----")),
)

# --- 规则 2：绝不允许入库的文件形态 ---
# 这些是 AI App 的聊天记录与配置，即使内容「看起来无害」也不该进公开仓库。
FORBIDDEN_PATH_PATTERNS = (
    ("chat-db-sample", re.compile(r"(^|/)rikka_hub(-shm|-wal)?$")),
    ("preferences-pb", re.compile(r"\.preferences_pb$")),
    ("probe-dir", re.compile(r"(^|/)ai-chat-probe/")),
    ("backup-worktree", re.compile(r"(^|/)(save-v009-worktree|ssh-v009-backup|module-backup)/")),
)

# 允许出现在仓库里的示例值：测试夹具与文档里的占位符。
ALLOWLIST_SUBSTRINGS = (
    "not-a-real-key",
    "EXAMPLE",
    "your-api-key",
    "sk-xxxx",
    "REPLACE_ME",
)


def run_git(args: list[str]) -> list[str]:
    result = subprocess.run(
        ["git", *args], capture_output=True, text=True, check=False
    )
    if result.returncode != 0:
        # 某些 git 子命令在空仓库下会非零返回，按空结果处理。
        return []
    return [line for line in result.stdout.splitlines() if line.strip()]


def tracked_files() -> list[str]:
    return run_git(["ls-files"])


def staged_files() -> list[str]:
    return run_git(["diff", "--cached", "--name-only", "--diff-filter=ACMR"])


def excluded(path: str) -> bool:
    return any(path.startswith(prefix) for prefix in EXCLUDE_PREFIXES)


def is_allowlisted(text: str) -> bool:
    return any(marker in text for marker in ALLOWLIST_SUBSTRINGS)


def check_paths(files: list[str]) -> list[str]:
    findings: list[str] = []
    for path in files:
        if excluded(path):
            continue
        for name, pattern in FORBIDDEN_PATH_PATTERNS:
            if pattern.search(path):
                findings.append(f"{path}: 命中禁止入库的文件形态 [{name}]")
                break
    return findings


def check_contents(files: list[str]) -> list[str]:
    findings: list[str] = []
    for path in files:
        if excluded(path):
            continue
        target = pathlib.Path(path)
        if not target.is_file():
            continue
        try:
            data = target.read_bytes()
        except OSError:
            continue
        # 二进制文件跳过：既读不出可行动的信息，又容易误报。
        if b"\x00" in data[:8192]:
            continue
        try:
            text = data.decode("utf-8")
        except UnicodeDecodeError:
            continue
        for lineno, line in enumerate(text.splitlines(), start=1):
            if is_allowlisted(line):
                continue
            for name, pattern in SECRET_PATTERNS:
                if pattern.search(line):
                    findings.append(f"{path}:{lineno}: 疑似凭据 [{name}]")
    return findings


def check_history() -> list[str]:
    findings: list[str] = []
    for name, pattern in SECRET_PATTERNS:
        # git grep 覆盖全部历史提交，避免遗漏「提交后又删除」的情况。
        commits = run_git(["rev-list", "--all"])
        for commit in commits:
            hits = run_git(["grep", "-nE", pattern.pattern, commit, "--", "."])
            for hit in hits:
                if excluded(hit) or is_allowlisted(hit):
                    continue
                findings.append(f"{commit[:8]}: 历史提交含疑似凭据 [{name}]")
    return findings


def main() -> int:
    parser = argparse.ArgumentParser(description="敏感信息扫描")
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--staged", action="store_true", help="只扫描暂存区")
    group.add_argument("--all", action="store_true", help="扫描全部历史")
    args = parser.parse_args()

    if args.all:
        findings = check_history()
        scope = "全部历史"
    else:
        files = staged_files() if args.staged else tracked_files()
        findings = check_paths(files) + check_contents(files)
        scope = f"暂存区 {len(files)} 个文件" if args.staged else f"已跟踪 {len(files)} 个文件"

    if findings:
        print(f"敏感信息扫描失败（{scope}）：", file=sys.stderr)
        for item in findings:
            print(f"  - {item}", file=sys.stderr)
        print(
            "\n这些内容不得进入版本库。若确属示例，请改用占位符（如 sk-xxxx 或 EXAMPLE）。",
            file=sys.stderr,
        )
        return 1

    print(f"敏感信息扫描通过（{scope}）")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
