# 安全模型

## 三层防护

本服务授予 MCP 工具完整 Root 能力，因此暴露面需要分层收敛。三层叠加，任一层失效仍有保护：

| 层 | 机制 | 作用 |
|---|---|---|
| 1. 监听范围 | `network.listenLan=false` | 根本不监听局域网地址，端口中不存在 |
| 2. 来源校验 | `onLinkOnly` + Host/Origin 校验 | 只接受本机与直接连接的同网段来源 |
| 3. 身份认证 | Bearer Token | 非环回来源需携带凭证 |

### 第 3 层：Bearer Token

| 访问来源 | 默认行为 |
|---|---|
| `127.0.0.1` / `::1` | 免认证（`loopbackAnonymous=true`） |
| 局域网 | 需要 `Authorization: Bearer <token>`（`requireToken=true`） |

- token 首次启动自动生成，32 字节随机、hex 编码，文件权限 `0600`。
- 校验使用常量时间比较，避免时序侧信道。
- 可在 WebUI 面板轮换；轮换后旧 token 立即失效。
- `/health` 与 `/version` 始终免认证——否则监控与排障脚本会全部失效，
  反而掩盖真实故障。这两个端点不返回敏感信息。
- 配置要求认证但 token 存储缺失时**拒绝请求而非放行**，避免退化成裸奔。

## 默认风险

LAN 模式开启时，同一直接连接网段内能访问 5322 端口的设备可读写文件、执行
Root Shell、安装/卸载应用、改变系统设置甚至重启或关机。认证只能挡住无凭证的
访问者，不能防御已被信任的客户端被滥用。

不要把端口转发到公网，不要在访客 Wi‑Fi、酒店网络或不可信热点使用 LAN 模式。
不需要局域网访问时，直接把 `listenLan` 设为 `false`。

## 权限边界

- supervisor：Root，只有进程管理、退避、日志和版本回滚职责。
- broker：Root，唯一执行特权操作的进程。
- frontend：优先 UID 2000、无 Linux capabilities；只负责 HTTP/MCP 解析和传输。
- broker socket：使用 `SO_PEERCRED` 核对 UID/GID，并把 peer PID 与 supervisor 写入的 frontend 子进程 PID 精确比对。

若 ROM 阻止降权，supervisor 会以 Root 重启 frontend；服务保持可用，但 `/status.securityDegraded=true`，状态页显示原因。

## 网络边界

- 回环始终允许。
- LAN 只允许接口路由表中“直接连接”的 on-link 地址。
- 非本机 Host、跨源 Origin、CORS 预检、过大请求和超并发请求被拒绝。
- 状态页没有外部脚本、字体、图片或统计请求，并设置 CSP/Frame/Referrer/Permissions 等安全头。

## 文件与命令

绝对路径能力是刻意设计，不是遗漏。调用方必须承担 Root 绝对路径修改风险。实现仍保证：

- 原子写配置和任务快照。
- 归档穿越检查。
- 下载/上传使用随机 ID、范围校验、SHA-256 与 TTL。
- 进程终止针对独立进程组。
- 卸载脚本在发信号前检查 PID 与 `/proc/<pid>/cmdline`。
- 不自动禁用 SELinux、不默认 remount 系统分区。

## 认证状态与长期记忆

- 命名 Cookie jar 以 `0600` 原子文件保存在内部 Root 状态目录；值默认不通过 MCP 返回，只有 `includeValues=true` 且 `confirmSensitive=true` 才显式读取。
- Authorization 不持久化；跨源重定向会剥离 Authorization、Cookie 等敏感请求头，TLS 证书验证不可关闭。
- 长期记忆对 Authorization、Cookie、Token、密码、secret、headers、env、stdin、body、content、data、WebSocket 消息及通用 value 只保存类型和长度；URL、主机和路径只保存结构，产物扩展名使用白名单。不会记录完整 stdout、stderr、产物文件名或响应体；递归模板受深度、节点数和 8 KiB 大小限制，未知字符串默认脱敏。
- 后台任务完成防重放不使用有误判概率的 Bloom filter，而使用带整体校验和的精确 SHA-256 任务 ID 集合；账本损坏时停止自动恢复，不以“可能没完成”为由重做任务。
- 系统浏览器拥有独立的 Cookie/账号空间；`browser_capture` 不注入 MCP Cookie jar，也不会自动填写凭据。
- WebSocket 有连接数、消息大小和接收队列上限，服务关闭即断开，不自动重连或重发非幂等消息。

## 建议加固

当前 0.02 为保持既有默认行为，不启用 Token。生产环境至少采取一项：

1. 关闭 LAN，只使用 `adb forward`。
2. 在可信 VPN/私有 USB 网络中使用。
3. 用设备防火墙把 5322 限制到固定管理主机。
4. 后续启用认证配置后使用高熵 Token。

发现漏洞时不要附带真实设备隐私或可直接利用的公网地址。
