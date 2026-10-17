# aplm123 Root MCP

Android Root 能力 MCP 服务。把一台已 root 的手机变成可被 AI 直接驱动的设备，
提供文件、Shell、应用、系统、网络、界面自动化等 **53 个工具**。

> **本模块基于 [ZCR521 AI MCP](https://github.com/32463991/ZCR521-AI-MCP) v0.02 修改而来。**
> 原模块作者：**小骨@Xiaogu_zcr521**（GPL-3.0-or-later）。
> 原始模块的完整功能与设计均予保留，并注明出处；本分支仅在其基础上做增强与重命名。

- 模块 id：`aplm123.root.mcp`
- 服务端口：`5322`
- 工作目录：`/storage/emulated/0/aplm123AI`
- 内部状态：`/data/adb/aplm123-mcp`
- 支持框架：Magisk 20.4+ / KernelSU / APatch（**仅 arm64-v8a**）

---

## 一、安装

1. 在 Magisk / KernelSU / APatch 管理器中安装 `aplm123-Android-Root-MCP-*.zip`
2. **重启设备**
3. 查看 MCP 地址：`/data/adb/aplm123-mcp/MCP地址.txt`，或访问 `http://<手机IP>:5322/ui`

> ⚠️ **从其他 id 的模块升级**：本模块 id 与 `zcr521.android.mcp` 不同，
> 会被识别为**独立模块**。安装前请先卸载旧模块，否则两者会同时抢占 5322 端口。

---

## 二、安全须知（务必先读）

**默认配置在局域网内提供匿名完整 Root 能力。** 同一网络中的任何设备都可能控制本机。

- 不要连接不可信的 Wi-Fi
- 不要将 5322 端口映射到公网
- 可在 `config` 中关闭 `security.anonymous` 或限制 `listenLan`

---

## 三、控制台（两个入口）

本模块同时提供**两个**控制台入口，各有用途：

### 入口 1：管理器内 WebUI（`webroot/`）

在 **KernelSU / APatch 管理器**的模块页面直接打开「WebUI」即可。
页面位于 `webroot/index.html`，由管理器用自己的 WebView 渲染，
通过管理器的 `exec` 桥调用本机命令，**不依赖 5322 端口是否可访问**。

可以做的事：
- 查看模块 ID / 版本 / Root 框架
- 查看服务状态、连接池水位、工作目录
- 重启服务 / 停止服务
- 查看 supervisor 日志
- 断开全部流式长连接

> 在普通浏览器里打开这个页面时没有 `exec` 桥，页面会明确提示你改用入口 2。

### 入口 2：HTTP 面板（浏览器直连）

```
http://<手机IP>:5322/ui
```

适合在电脑或手机浏览器里看，数据源是 `/api/ui` 与 `/api/streams`（免占连接配额）。

面板提供（**只读**）：

| 区块 | 内容 |
|---|---|
| 概要 | MCP 地址、版本、运行时长、Root 框架、任务数 |
| **连接池水位** | 普通请求 / 流式连接分开显示占用与上限 |
| **挂起的长连接** | 逐条列出路径、来源、持续时长，可**一键断开** |

### 这个面板能解决「服务卡死」

**问题**：MCP 的流式连接（Streamable HTTP / SSE）会长期挂起。客户端不主动断开时，
连接配额会被逐渐占满，最终连 `/health` 都返回 `CONCURRENCY_LIMIT`，只能重启设备。

**已修复**：连接配额分为两条独立通道——

| 通道 | 默认上限 | 用途 |
|---|---|---|
| `maxConnections` | 32 | 普通请求（工具调用、状态查询） |
| `maxStreams` | 64 | 流式长连接（`/mcp`、`/sse`） |

并且 `/ui`、`/health`、`/status`、`/api/ui`、`/api/streams` **完全不占配额**，
所以**服务过载时面板依然打得开**，打开就能看到是谁占着连接，点「断开全部长连接」即可恢复——
**无需重启**。

两个入口都能做这件事：管理器 WebUI 用 `exec` 调命令，HTTP 面板直接打接口。

---

## 四、工具清单（53 个）

### 文件与数据

| 工具 | 说明 |
|---|---|
| `aplm_fs_info` | 列目录、元数据、磁盘与挂载 |
| `aplm_fs_read` | 流式读取文本/二进制/行/尾部 |
| `aplm_fs_write` | 创建、追加、截断、补丁、改时间戳 |
| `aplm_fs_manage` | 目录、复制、移动、删除、权限、链接 |
| `aplm_fs_search` | 按名称/内容搜索，查大文件与重复 |
| `aplm_fs_hash` | 计算或校验哈希 |
| `aplm_archive` | ZIP/TAR/GZIP/XZ/7z 创建与解压 |

### 执行与远程

| 工具 | 说明 |
|---|---|
| `aplm_shell` | 以 root/shell/指定 UID 执行命令 |
| `aplm_script` | 执行多行脚本 |
| **`aplm_ssh`** | **SSH 客户端**：exec / SFTP 上传下载 / 端口转发 |
| `aplm_process` | 进程列表、信号、优先级、fd |

### 手机交互（本分支重点增强）

| 工具 | 说明 |
|---|---|
| **`aplm_clipboard`** | **剪贴板**读写、清空、监听新内容 |
| **`aplm_ui`** | **按元素定位**点击/输入/等待/滚动（不再依赖坐标） |
| **`aplm_share`** | **交给系统处理**：打开、分享、安装 APK |
| `aplm_input` | 底层输入：点击、滑动、按键（按坐标） |
| `aplm_screen` | 截图、录屏、前台 Activity |
| `aplm_notification` | 通知与监听器权限 |

### 应用

`aplm_app_list` `aplm_app_info` `aplm_app_install` `aplm_app_manage`
`aplm_app_permission` `aplm_app_export` `aplm_default_app`

### 系统

`aplm_device_info` `aplm_property` `aplm_setting` `aplm_display` `aplm_audio`
`aplm_connectivity` `aplm_locale_time` `aplm_input_method` `aplm_app_policy`
`aplm_accessibility` `aplm_developer` `aplm_power` `aplm_service`

### 网络

`aplm_network`（HTTP、Cookie jar、HTML 提取、浏览器捕获、RSS/Atom、WebSocket）
`aplm_download` `aplm_transfer_upload` `aplm_transfer_export`

### 运维

`aplm_status` `aplm_capabilities` `aplm_config` `aplm_log` `aplm_task`
`aplm_diagnostics` `aplm_root_info` `aplm_root_module` `aplm_systemless`
`aplm_backup` `aplm_schedule` `aplm_skill`

---

## 五、常用示例

**读剪贴板**（在任意 App 复制后直接调用）：
```json
{"action":"get"}
```

**按文字点击**（换分辨率也不失效）：
```json
{"action":"click","selector":{"text":"发送"}}
```

**等元素出现再点**（解决加载/动画时序）：
```json
{"action":"wait","selector":{"text":"登录成功"},"timeoutMs":10000}
```

**滚动列表找目标**：
```json
{"action":"scroll_to","selector":{"text":"设置"},"direction":"down","maxSwipes":10}
```

**把生成的文件分享到微信**：
```json
{"action":"share","path":"/storage/emulated/0/aplm123AI/output/a.png","package":"com.tencent.mm"}
```

**SSH 连服务器执行命令**：
```json
{"action":"exec","host":"10.0.0.5","user":"root","keyFile":"id_ed25519","command":"uptime"}
```

**SSH 建隧道**（配合联邦/内网穿透）：
```json
{"action":"forward","host":"server","user":"root","password":"...",
 "targetHost":"127.0.0.1","targetPort":8080}
```

---

## 六、配置

### 协议版本与 `_meta`

服务同时支持三个协议版本：

| 协议 | 接入方式 |
|---|---|
| `2025-11-25` | 会话制（initialize → 拿 `Mcp-Session-Id` → 后续请求带上） |
| `2024-11-05` | 旧 SSE（`/sse` + `/messages`） |
| `2026-07-28` | 无会话（**只需带 `MCP-Protocol-Version` 头**） |

**关于 2026-07-28 的 `_meta`**：该版本的规范（SEP-2575）要求请求体
`params._meta` 中携带 `protocolVersion` 与 `clientCapabilities`，
且 HTTP 头 `Mcp-Method` 必须与请求体一致，否则服务端返回 400。

**本模块已在服务端自动补齐这些字段**，因此客户端无需手动构造：

```bash
# 只要这一个头就能用
curl -X POST http://127.0.0.1:5322/mcp \
  -H 'Content-Type: application/json' \
  -H 'Accept: application/json, text/event-stream' \
  -H 'MCP-Protocol-Version: 2026-07-28' \
  -d '{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}'
```

补全规则（只填空缺，**不覆盖**客户端已提供的值）：

| 缺失项 | 补什么 |
|---|---|
| `params._meta.protocolVersion` | 头里的版本值 |
| `params._meta.clientCapabilities` | `{}` |
| `Mcp-Method` 头 | 请求体的 `method` |
| `Mcp-Name` 头 | 请求体 `params.name`（仅 `tools/call` 等） |

若客户端自己提供了这些值，服务端一律原样保留——头与请求体不一致属于
真实的协议错误，会照常报错而不是被静默纠正。

读取当前配置：
```json
{"tool":"aplm_config","arguments":{"action":"get"}}
```

关键项：

| 配置 | 默认 | 说明 |
|---|---|---|
| `network.port` | 5322 | 监听端口 |
| `network.listenLan` | true | 是否允许局域网访问 |
| `limits.maxConnections` | 32 | 普通请求并发上限 |
| `limits.maxStreams` | 64 | 流式连接上限 |
| `limits.streamIdleTtlSeconds` | 600 | 流空闲判定阈值 |
| `limits.shellTimeoutSeconds` | 120 | Shell 默认超时 |
| `security.anonymous` | true | 匿名访问（建议改为 false） |
| `security.allowCORS` | false | 跨域（不建议开启） |

---

## 七、本分支相对原模块的变更

1. **重命名**：模块 id / 显示名 / 全部工具前缀改为 `aplm123` / `aplm_`
2. **新增 `aplm_ssh`**：SSH 执行、SFTP 传输、端口转发（含主机密钥校验）
3. **新增 `aplm_clipboard`**：剪贴板读写与监听
4. **新增 `aplm_ui`**：无障碍元素级自动化，替代脆弱的坐标点击
5. **新增 `aplm_share`**：打开 / 分享 / 安装
6. **修复并发卡死**：长短连接配额分离 + 管理端点免占配额 + 一键释放
7. **WebUI 控制台**：连接池水位可视化与长连接管理

原模块的核心设计（渐进披露、脱敏学习回执、写后读回校验、任务持久化、
自升级回滚、Systemless 覆盖）全部保留。

---

## 八、许可与致谢

- 本项目沿用原模块许可：**GPL-3.0-or-later**（见 `LICENSE`）
- 第三方组件声明见 `NOTICE`
- **原模块**：[ZCR521 AI MCP](https://github.com/32463991/ZCR521-AI-MCP)，作者 **小骨@Xiaogu_zcr521**
- 原模块交流社群与 Telegram 频道沿用原作者信息，本分支未做改动

如果这个模块对你有用，请一并支持原作者。


---

## 模块脚本内联注释归档

模块规范不允许脚本内出现 `#` 注释（只保留 shebang），
以下注释原位于各脚本内部，为便于维护者理解实现意图，改在此集中保存。

### `module/common.sh`

```
# 启动链路的落盘日志。
#
# 存在的原因：service.sh / post-fs-data.sh 由 root 管理器在开机时调用，
# 它们的 stdout/stderr 通常被丢弃。一旦启动在早期失败（目录不可建、二进制
# 不可执行、锁被占等），用户既看不到输出、也找不到 supervisor.log——
# 因为那个文件只在守护进程成功起来之后才会有内容。
#
# 因此所有启动路径的成败都写到 boot.log，与 supervisor.log 同目录。
  # 注意：ZCR_LOG_DIR 由 common.sh 顶部定义，此处必须复用同一个值，
  # 不能改写它——否则调用方的路径约定会被破坏（早期版本踩过这个坑）。
  # 超过 256 KiB 时轮转一次，避免无界增长
  # 短暂等待后核验进程是否真的活下来了。
  # 守护进程启动即退（例如早期的哈希校验失败）时，这里能捕获到并记录退出原因。
```

### `module/post-fs-data.sh`

```
  # 早期失败必须留痕：此前这里静默退出，导致启动问题完全无法排查。
```

### `module/service.sh`

```
# 不要把输出丢进 /dev/null：zcr_start_supervisor 在拉起守护进程之前
# 有多处提前返回（目录不可建、二进制不可执行、锁被占等），
# 这些错误必须能从 boot.log 读到，否则启动失败时毫无痕迹。
```
