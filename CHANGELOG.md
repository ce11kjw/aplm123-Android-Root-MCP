# 更新日志

## 0.14

_（2026-11-09）_

### 新增

- **模块热更新：装完不用重启手机**。在 APatch 里点更新或刷入新 zip 后，
  约 10~20 秒自动生效，服务中断十几秒，MCP 客户端重连一次即可。

  做法参考同作者的 ChargeControl：`customize.sh` 收尾时 `nohup` 拉起
  `update.sh <解压目录>`，由它在安装器退出后执行：

  1. 等解压目录大小连续两次不变（判据是「安装器写完」，不能靠 `$PPID`
     —— `nohup` 后进程立刻被 init 收养，`$PPID` 恒为 1）
  2. 停旧进程（`pkill -x aplm123d`，TERM 后 15 秒仍在就 KILL）
  3. 备份并整体替换模块目录 —— 备份放 `/data/adb/aplm-mcp-backup`，
     **不能放 `modules/` 下**：管理器用 `module.prop` 的 id 当列表 key，
     同 id 两份会让 APatch 直接崩
  4. 用新目录的 `service.sh` 拉起
  5. 轮询 `/version` 做健康检查，25 秒不通过就用备份**自动回滚**
  6. 全程写 `/data/adb/aplm123-mcp/logs/hot-update.log`

  配置、日志、任务记录都在 `/data/adb/aplm123-mcp`，不在替换范围。
  旧的「下载 + `apd module install` + 重启设备」脚本已替换（那个脚本还指着
  早已不存在的旧仓库 `32463991/aplm123-AI-MCP`，本来就跑不通）。

## 0.13

_（2026-11-09）_

### 变更

- **`limits.compactResultText` 的默认值改为 `off`（完整 JSON）**。

  起因：RikkaHub 2.6+ 开始协商 MCP `2026-07-28`，而服务端对「最新协议」
  默认走精简路径，工具结果只剩一句「OK: 命令执行成功」，看起来像工具坏了。
  实测 3.1.2 装上后立刻复现。

  改法：默认给完整结果，精简（`auto` / `on`）变成**显式选择**。旧配置里缺
  这个字段的，`Normalize` 一律补 `off`。

  ```
  off   （新默认）完整 JSON
  auto  跟随协议：只有 2026-07-28 客户端拿短文本
  on    所有客户端都只拿「OK: 消息」
  ```

### 新增

- **模块 WebUI 增加「工具输出」开关**。此前开关只在服务端面板
  （`http://127.0.0.1:5322/ui`）里，而 APatch 管理器点「WebUI」打开的是
  模块自带的 `webroot/index.html`，是另一个页面，所以在管理器里看不到。

  现在管理器里直接有：当前形态 + 生效说明 + 三个按钮，点完立即生效。
  实现走页面已有的 `exec` 桥 + busybox wget 打回环（服务端 `allowCors`
  固定 `false`，直接 `fetch` 会被跨域拦）。

## 0.12

_（2026-11-05）_

### 新增

- **模块在线更新（`updateJson`）**。`module.prop` 新增：

  ```
  updateJson=https://github.com/ce11kjw/aplm123-Android-Root-MCP/releases/latest/download/update.json
  ```

  APatch / Magisk / KernelSU 的管理器都会读这个字段：远端 `versionCode`
  比本地大时，模块卡片上直接出现更新按钮，点一下就能装新版，不用再手抄
  链接。

  免维护做法：`update.json` 由 CI 在**打 tag 时生成**并随 Release 上传，
  里面的 `zipUrl` / `changelog` 都按当前 tag 拼；`module.prop` 又固定指向
  `releases/latest/download/update.json`，所以以后只要打 tag 就行，
  不用手改 JSON、不用改地址。

## 0.11

_（2026-11-05）_

### 新增

- **`aplm_network http` 的 `headersMode`（safe / full）**。此前响应头只回显
  14 个白名单字段，`Location`、`Link` 这类**显式**打码，`Set-Cookie`、
  `Mcp-Session-Id`、`ETag`、`WWW-Authenticate` 全部丢失——调试需要读它们时
  只能绕道调用 shell 里的 wget，实测被卡住过两次。

  `full` 模式保留全部头部原值，**只**对 `Set-Cookie` 的值打码（凭据不是语义
  元数据）。默认仍是 `safe`，行为不变。

### 说明

- 本轮曾计划加 `autoBackgroundSeconds`（长命令超窗自动转后台），实现完成后
  按需求撤下：`shell` / `script` 的命令文本不在「可持久化参数白名单」内，
  持久任务走不通；而进程内后台任务会额外增加一套任务查询路径，收益不足以
  抵掉复杂度。需要长任务时仍用 `background: true`（受同一策略限制）。

## 0.10

_（2026-11-05）_

### 新增

- **工具输出形态开关（`limits.compactResultText`）**。此前「精简」是隐式的：
  客户端只要协商到最新协议（2026-07-28）就只收到 `OK: 消息` 一行，其他协议
  收到完整 JSON；用户在界面上既无法选择，也无从判断当前是哪种。

  现在改为显式三档：
  - `auto`（默认，保持原有行为）：仅最新协议客户端拿到短文本
  - `on`：所有客户端都只拿到短文本，完整结果仍在 `structuredContent`
  - `off`：所有客户端都拿到完整 JSON

  面板新增「工具输出」区块（`GET`/`POST /api/output`），改完**立即生效**，
  无需重启；也可用 `aplm_config` 的 `limits.compactResultText` 字段设置。

  设计取舍：`structuredContent` 始终是完整对象，开关只影响 `content` 里的
  文本长度。所以切到 `on` 省的是上下文 token，不会丢数据；需要细节时仍然
  可以从结构化对象取。

## 0.09

_（2026-10-16）_

### 修复

- **一次性 `exec` 会在会话池里留下僵尸条目**。`dialSSH` 会把新建会话登记进
  注册表，而一次性执行完成后只 `shutdown()` 关闭连接、**不删除 map 条目**，
  导致每次 `exec` 都占掉一个名额。跑满 8 次后所有后续连接都被拒绝，
  报「SSH 会话数已达上限」却看不到任何活跃连接——这就是该报错最常见的成因。
  现在一次性执行结束时会同时 `shutdown()` 与 `remove()`。

- **空闲会话永不回收**。`sshSessionIdle` 常量此前只定义未使用，
  调用方一旦忘记 `close`，名额就被永久占用。现在新增 `evictIdle()`：
  - 建立新会话前先回收空闲超时的会话（`admit()`），回收后仍有名额则放行。
  - 后台每分钟清扫一次（`startSweeper()`），随 `Manager.Close` 退出。
  - **带活跃端口转发的会话不回收**——转发是长期隧道，杀掉会断掉正在使用的连接。

- **`touch()` 从未被调用**。`lastUsed` 一直停留在创建时刻，
  空闲判断必然失真。现在成功执行与失败后都刷新，保证活跃会话不被误杀。

- **上限报错缺乏可操作信息**。原提示只有「请先关闭不用的会话」，
  调用方无从知道该关哪一条。现在附带每个会话的
  `id(host:port 空闲时长)` 与关闭方式（`close` / `all=true`）。

### 新增

- **`aplm_ssh exec` 支持会话复用**：传 `sessionId` 时在已有会话上执行，
  不再新建连接，也不会关闭该会话。不传时行为与旧版完全一致
  （一次性连接、跑完即断），保持向后兼容。

- **`aplm_ssh exec` 支持四种形态**，把「N 台机器 × M 条命令」
  收敛成一次工具调用：

  | 形态 | 参数 | 说明 |
  |---|---|---|
  | 单条一次性 | `command` | 连上、跑完、断开（原行为） |
  | 复用会话 | `sessionId` + `command` | 在已有会话上执行 |
  | 多条命令 | `commands: []` | 同一连接顺序执行，逐条返回结果 |
  | 同命令扇出 | `targets: []` + `command` | 多个会话并行跑同一条命令 |
  | 异命令批量 | `jobs: [{sessionId, command}]` | 每个会话跑各自的命令 |

  - 批量数量上限 `sshMaxBatch`（32），并发上限 `sshBatchConcurrency`（8，
    与服务端 `totalTasks` 对齐）。
  - 部分失败时整体标记失败，但**逐条结果全部保留**，便于定位。

- **`aplm_ssh connect` 同目标去重**：同一 `host:port:user` 已存在存活会话时
  直接复用并返回 `reused: true`，不再新建占名额。

### 变更

- `sshMaxSessions` 由 8 提升到 **32**，与 frontend 的 `maxConnections` 对齐
  （会话内部复用同一条 TCP 连接，不会放大服务端连接数）。
- 构建链统一到 **Go 1.27.1**（`scripts/build.sh` 的版本校验同步更新）。
- CI 在推送 `v*` 标签时自动创建 Release 并附上模块 zip。

## 0.08

_（2026-09-25）_

### 修复

- **`aplm_ssh` 的 SFTP 传输可能无限期挂住**。SFTP 是请求-响应协议，
  远端收到请求后不再回复时（网络中断、对端异常、中间设备丢包），
  读取会永久阻塞，表现为工具调用一直不返回且没有任何错误。
  现在给底层连接设置读写 deadline：每次交互前刷新，因此长传输不会被
  整体掐断，只有**单次无响应**才超时并返回可诊断的错误。

  相关处理：
  - 会话记录底层 `net.Conn`，用于设置 deadline。
  - 关闭 SFTP 会话时把 deadline 收紧到 3 秒：保留原值会让断开握手
    等到超时才返回（实测白白多等 30 秒），清除 deadline 又会重新引入
    无限等待。
  - 断开握手放到后台收尾并设上限，不拖住已完成的传输结果返回。

- 新增 `aplm123_SSH_DEBUG=1` 环境变量，打开后输出 SFTP 关键步骤日志，
  便于排查长链路问题（默认关闭）。

### 验证

真机端到端（Android 16 / APatch，SSH 服务器与客户端在不同进程）：

| 用例 | 结果 |
|---|---|
| `connect` 密码认证 + 主机密钥指纹 | ✅ 指纹与服务器一致 |
| `exec` 命令执行 / 非零退出码 | ✅ `hello` / exitCode=3 |
| `exec` 输出截断（5 MB） | ✅ 置 `outputTruncated` |
| `forward` 真实穿隧道收发 | ✅ 收到 `ECHO:tunnel-test` |
| `unforward` / `list` / `close` / `keys` | ✅ |
| **SFTP 上传** | ✅ 0.19s，写后读回校验通过 |
| **SFTP 下载** | ✅ 0.17s，远端/本地 SHA-256 一致 |
| 参数校验与错误分类 | ✅ 中文报错、超时精确 |

## 0.07

_（2026-09-25）_

### 新增

- **2026-07-28 协议的自动兼容层**。该协议（SEP-2575）要求请求体
  `params._meta` 携带 `protocolVersion` 与 `clientCapabilities`，
  且 HTTP 头 `Mcp-Method` 必须与请求体一致，否则返回 400。
  多数客户端只发送 `MCP-Protocol-Version` 头，导致能力探测直接失败。
  现在服务端会在请求进入 SDK 之前自动补齐这些字段，
  客户端**只需一个协议头**即可使用新版协议。

  实现要点（`internal/frontend/meta_compat.go`）：
  - `_meta` 写在 `params` 内部（SDK 经 `extractRequestMeta(req.Params)` 读取）
  - 只填空缺，**绝不覆盖**客户端已提供的值：头与请求体不一致属于
    真实协议错误，保持由 SDK 报错，不静默纠正
  - 支持批量请求逐条补全
  - 请求体过大时跳过补全，交由下游返回 413
  - 不改动 `vendor/` 内的 SDK，SDK 日后放宽行为时可直接移除该文件

### 变更

- `/version` 接口的 `author` 由原作者署名改为 `@aplm123`，
  与 `module.prop` 保持一致。
- HTTP 控制台与管理器 WebUI 的页脚不再显示原作者署名。

### 文档

- `README.md` 新增「协议版本与 `_meta`」一节，说明三个协议版本的接入方式，
  并给出 2026-07-28 的最小可用 curl 示例与补全规则表。

## 0.06

_（2026-09-25）_

### 修复

- **管理器 WebUI 显示"未运行"而脚本查是运行中**。两个独立原因叠加：

  1. **桥调用方式错误**。KernelSU 的 `exec` 是**回调式**而非 Promise：
     `ksu.exec(command, optionsJSON, callbackName)`，结果经
     `window[callbackName](errno, stdout, stderr)` 回传。
     此前写成 `await ksu.exec(cmd)`，`exec` 立即返回 `undefined`，
     读取 `.stdout` 抛异常被静默捕获，页面因此永远拿不到状态。
     现按官方 `kernelsu` 包的实现改为回调封装。
  2. **存活判定依赖权限**。`running` 原先只看 PID 文件 + `kill(0)` 信号探测。
     管理器 WebView 的命令桥可能以非 root 身份执行，对 root 进程
     `kill(0)` 返回 EPERM，被误判为"未运行"。
     现改为多来源交叉验证：PID 文件 + `/proc/<pid>` 存在性（不依赖权限）
     + 本地 HTTP 健康检查，任一确认存活即判为运行中，
     并在 `aliveSource` 字段标明判定依据。

### 变更

- **WebUI 桥接层支持多管理器**。按优先级探测 `window.ksu`、
  `window.KernelSU`、`window.magisk`、`window.Magisk`、`window.mm`
  以及全局 `window.exec` 函数，覆盖 KernelSU / KernelSU Next /
  APatch / Magisk 及改版管理器。
- 桥不可用时页面不再只显示一句失败，而是列出**检测到的候选对象及其类型**，
  便于判断是哪种管理器环境。
- `exec` 回调增加 20 秒兜底超时，避免个别实现既不回调也不抛错时页面卡死。

## 0.05

_（2026-09-25）_

### 修复

- **启动失败不再无迹可查**。此前 `service.sh` 把 `zcr_start_supervisor` 的
  全部输出重定向到 `/dev/null`，而该函数在真正拉起守护进程之前有多处提前返回
  （目录不可建、二进制不可执行或不可执行位丢失、启动锁被占、crash-loop 等），
  这些错误全部被丢弃；同时 `supervisor.log` 只在守护进程成功启动后才会产生，
  因此"服务起不来且没有任何日志"。
  现在所有启动路径的成败都写入 `/data/adb/aplm123-mcp/logs/boot.log`，
  并记录原始证据（文件权限、ELF 魔数、supervisor.log 尾部）。

### 新增

- **`aplm123d start-diagnose` 子命令**：逐段检查启动链路并给出结论与修复建议。
  覆盖运行环境、`module.prop`、守护进程二进制及其可执行位、状态目录可写性、
  `manual-stop` / `crash-loop.blocked` 阻断标记、残留启动锁、PID 文件与服务存活、
  配置校验，并附上 `boot.log` 与 `supervisor.log` 的尾部原文。
- **启动后存活确认**：拉起守护进程后等待数秒核验进程仍在；
  若已退出，立即把 `supervisor.log` 尾部记入 `boot.log`，
  而不是留下一个指向死进程的 PID 文件。
- **WebUI 新增「启动诊断」按钮**，并让「查看日志」同时显示
  `boot.log`、`supervisor.log`、`broker.log`、`frontend.log`。

- **`scripts/bump-version.sh`**：版本号单点管理。以 `module/module.prop` 为唯一权威，
  一条命令同步其余 8 处，并提供 `--check` 一致性校验。
- 构建脚本改为从 `module.prop` 读取版本（不再硬编码），
  并新增打包后的版本一致性闸门；`versionCode` 改用日期式。

### 变更

- `post-fs-data.sh` 早期失败不再静默退出：无法创建内部目录时会记录到
  `/data/local/tmp/aplm123-boot.log`（该路径在模块未就绪时仍可写）。

## 0.04

_（2026-09-25）_

### 修复

- **删除 `module.prop` 签名哈希校验**。该机制要求在编译时注入启动时校验，
  任何不一致都会让守护进程拒绝启动（报错"缺少内置 module.prop 签名哈希"），
  表现为安装后服务完全起不来。现已整条移除：调用点、两个校验函数、
  对应测试与构建注入逻辑均已清理。

### 新增

- **管理器 WebUI（`webroot/index.html`）**。按 KernelSU / APatch 规范提供模块页入口，
  经管理器的 `exec` 桥调用本机命令，不依赖 5322 端口。可查看模块信息、服务状态、
  连接池水位、重启/停止服务、查看日志、断开闲置长连接。
- **`aplm123d webui-status` 子命令**。为 WebUI 提供不依赖 HTTP 的服务状态快照，
  直接读配置、PID 文件与 `manual-stop` / `crash-loop.blocked` 标记，
  能解释"服务为什么没在跑"。存活判定以 supervisor 进程为准，避免误报。
- **`scripts/bump-version.sh`**：版本号单点管理。以 `module/module.prop` 为唯一权威，
  一条命令同步其余 8 处，并提供 `--check` 一致性校验。

### 变更

- 安装脚本不再显示原作者署名，移除安装完成后的 Telegram 频道跳转。
- `module.prop`：`author` 改为 `@aplm123`，描述更新为"通用 root MCP 扩展，
  内置管理器 WebUI 与浏览器控制台"。
- 构建脚本改为从 `module.prop` 读取版本（不再硬编码），并新增打包后的版本一致性闸门。
- `versionCode` 改用日期式（`YYYYMMDD` + 当日两位序号）。

## 0.02

- 压缩包内置 `update.sh`：可查询 GitHub Releases 并直接安装新版本。

- 新增第 49 个 `aplm_skill`：2 个内置网页自动化/低 Token MCP 技能按工具/action 自动匹配、正文渐进加载；所有真实完成调用自动生成 v4 脱敏收据与详细自动技能，收据有界压缩并用精确校验账本防止后台任务重放。
- 当前协议改为一次结构化结果、短文本摘要；运行时工具目录移除重复 Schema，默认结果预览降至 64 KiB。
- `aplm_network` 新增持久 Cookie、静态 HTML 提取、系统浏览器截图/内容提取、RSS/Atom 与 WebSocket；修复跨源重定向泄漏敏感头。
- 应用列表、详情和启动支持显示名、包名或组件查询；通过目标用户的 PackageManager 标签和 Launcher Activity 解析隐藏入口，并校验 `am start -W` 的语义结果。
- 发布包将第三方声明与完整许可证合并为单一 `NOTICE`，移除重复的 `.txt`、`.md` 文本条目并保留完整法律内容。

## 0.01

- 首次完整发布。
- 三进程 Root 权限隔离、崩溃退避与五分钟稳定版本回滚。
- 48 个 MCP 工具、2026 无会话协议、旧 Streamable HTTP/SSE、持久任务扩展。
- 三 ABI Android PIE/16 KiB 构建与真实 7-Zip 26.01。
- 断点传输、只读状态页、桌面 STDIO 桥、Root 模块与可复现发布链。
- 模块名称调整为 aplm123 Root MCP，作者为小骨@Xiaogu_zcr521。
- MCP 地址文件支持局域网 IP 动态刷新，默认工作区仅创建空根目录。
- 工具机器名保持兼容并增加规范的中文显示标题。
