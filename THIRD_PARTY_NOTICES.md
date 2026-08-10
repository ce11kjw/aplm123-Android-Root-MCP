# 第三方软件声明

发布包包含或依赖以下组件。固定版本以源码树中的 `go.sum` 与 `vendor/modules.txt` 为准；发布 ZIP 根目录的 `NOTICE` 按独立分段原样收录第三方声明及每个固定依赖的完整许可证文本。

## Model Context Protocol Go SDK

- 模块：`github.com/modelcontextprotocol/go-sdk`
- 固定版本：`v1.7.0-pre.3`
- 对应提交：`827f90b`
- 许可证：处于 MIT → Apache-2.0 迁移期；本固定版本同时包含 Apache-2.0 与尚未重许可的 MIT 贡献，SPDX 表达式记为 `Apache-2.0 AND MIT`
- 上游：<https://github.com/modelcontextprotocol/go-sdk>

## 7-Zip

- 版本：26.01
- 固定源码包：`7z2601-src.tar.xz`
- SHA-256：`b2389e0e930b2f9a348cf0fe7d9870a46482a8ec044ee0bdf42e2136db31c3d6`
- 上游：<https://github.com/ip7z/7zip/releases/tag/26.01>
- 完整对应源码：<https://github.com/ip7z/7zip/archive/refs/tags/26.01.tar.gz>

7-Zip 不是单一许可证：多数源码为 GNU LGPL v2.1 或更高版本；RAR 解码相关源码同时受 unRAR 限制；部分 LZFSE、Zstandard、XXH64 源码使用 BSD 或 public-domain 条款。官方 `DOC/License.txt` 与 GNU LGPL v2.1 全文在发布 ZIP 的 `NOTICE` 中以独立分段原样提供，不得从发布包删除。

## Go 扩展库

- `golang.org/x/sys`：BSD-3-Clause
- `golang.org/x/net`：BSD-3-Clause，用于 HTML5 解析与 Public Suffix Cookie 边界
- `golang.org/x/text`：BSD-3-Clause，由 HTML/Feed 字符集转换间接使用
- `github.com/coder/websocket`：ISC，用于 WebSocket 客户端会话
- `github.com/ulikunitz/xz`：BSD-3-Clause
- MCP SDK 的传递依赖：`github.com/google/jsonschema-go`、`github.com/segmentio/asm`、`github.com/segmentio/encoding`、`github.com/yosida95/uritemplate/v3`、`golang.org/x/oauth2`、`golang.org/x/sync` 与 `golang.org/x/time`；各自许可证全文均作为 `NOTICE` 的独立分段发布。

本项目自身使用 GPL-3.0-or-later；第三方组件继续适用各自许可证。
