# 工具索引

完整机器可读定义以 [`schemas/tools.json`](../schemas/tools.json) 为准。

机器工具名固定使用小写 `aplm_` 前缀和下划线，符合 MCP SDK 的工具名字符约束；客户端展示使用独立的简短中文 `title`，不再把机器标识当作显示名称。

| 分组 | 工具 |
|---|---|
| 服务 | `aplm_status`、`aplm_capabilities`、`aplm_config` |
| 文件 | `aplm_fs_info`、`aplm_fs_read`、`aplm_fs_write`、`aplm_fs_manage`、`aplm_fs_search`、`aplm_fs_hash`、`aplm_archive` |
| 传输 | `aplm_download`、`aplm_transfer_upload`、`aplm_transfer_export` |
| Shell/任务 | `aplm_shell`、`aplm_script`、`aplm_task` |
| 技能/记忆 | `aplm_skill` |
| 应用 | `aplm_app_list`、`aplm_app_info`、`aplm_app_install`、`aplm_app_manage`、`aplm_app_permission`、`aplm_app_export` |
| Root | `aplm_root_info`、`aplm_root_module`、`aplm_systemless` |
| 进程/服务 | `aplm_process`、`aplm_service` |
| 系统设置 | `aplm_property`、`aplm_setting`、`aplm_display`、`aplm_audio`、`aplm_connectivity`、`aplm_locale_time`、`aplm_input_method`、`aplm_app_policy`、`aplm_default_app`、`aplm_notification`、`aplm_accessibility`、`aplm_developer` |
| 设备控制 | `aplm_device_info`、`aplm_power`、`aplm_screen`、`aplm_input` |
| 网络 | `aplm_network` |
| 诊断 | `aplm_log`、`aplm_diagnostics` |
| 自动化 | `aplm_schedule`、`aplm_backup` |

## 应用查询与启动

`aplm_app_list` 可用 `query`、`appName` 或 `name` 按显示名、包名和 Launcher 标签分页查询；`aplm_app_info` 使用相同选择器读取详情。`aplm_app_manage launch` 支持显示名、精确包名或组件，先锁定当前数值用户，再解析该包的 `LAUNCHER`、`LEANBACK_LAUNCHER` 或 `INFO` Activity，并以 `am start -W --user ... -n ...` 校验真实启动结果。同名最高分候选不唯一时返回 `AMBIGUOUS_APP`，不会随机启动；停止、清理、启停和卸载仍只接受精确包名。

## 渐进技能与长期记忆

`aplm_skill` 的 `match` 只返回最多 3 条摘要，`get` 才读取一个完整技能正文，`list` 也只返回分页索引（默认 20、最大 100）；因此服务启动和工具发现不会把全部技能塞进模型上下文。每次普通 MCP 调用结束、以及每个后台任务真正进入终态后，服务都会写入一份脱敏调用收据，并更新对应 `tool/action` 的自动技能；`TASK_ACCEPTED`、任务/状态轮询、能力/配置读取和技能管理调用不会被误学成经验。

| action | 用途 |
|---|---|
| `match` | 按查询匹配内置与自动技能摘要 |
| `get` | 按 ID 加载一个完整技能 |
| `list` | 按 `skillSource=all|builtin|learned`、`offset`、`limit` 分页列索引 |
| `stats` | 查看内置技能、自动技能、调用收据及持久化错误 |
| `forget` | 显式确认后删除自动技能；保留脱敏历史收据 |

权威数据位于 `/data/adb/aplm123-mcp/MCP`，自动技能另镜像到 `/storage/emulated/0/aplm123AI/MCP/skills`。请求体、Cookie、Authorization、Token、密码、环境变量、stdin、WebSocket 消息及内容字段只保存类型和长度；URL/主机/路径只保留结构，stdout/stderr、产物文件名和完整响应也不会进入技能。收据按文件数/字节双阈值压缩，后台任务使用有界精确完成账本防重放；详见 [技能与长期记忆设计](SKILLS-MEMORY.md)。

## 网络与网页 action

`aplm_network` 除原有诊断外，提供 `http`、`web_extract`、`browser_capture`、`feed_parse`、`cookie_list/set/delete/clear` 与 `ws_connect/send/receive/ping/close/list`。Cookie jar 原子持久化且最多激活 64 个，列表同时按条目和序列化字节分页；WebSocket 连接仅在当前进程存活，所有操作执行 TTL 清理，单消息受结果预览预算约束。HTTP 跨源跳转删除认证头和 Referer，并拒绝重放正文；HTML/RSS/Atom 支持传统字符集。`browser_capture` 解析并强制指定默认浏览器包，再组合有界 `screencap` 与 UIAutomator 提取，不会把系统浏览器认证与 MCP Cookie jar 混为一谈。

## 执行规则

- 相对路径从 `/storage/emulated/0/aplm123AI` 解析；绝对路径保持绝对，不施加工作区沙箱。
- 安装时工作区根目录为空；各功能只在实际调用需要时创建对应子目录。
- 修改 Android 设置后，适配器会再次读取并核对；不一致返回失败。
- Android 命令按 API/帮助输出选择 `cmd`、`pm`、`am`、`settings`、`svc`、`dumpsys`、`input` 等策略。
- 缺少命令或 ROM 不支持时返回 `COMMAND_UNAVAILABLE` 或 `UNSUPPORTED`，其他工具继续可用。
- Shell 使用独立进程组；超时先 TERM，宽限后 KILL。输出写任务日志，响应只返回有界预览。
- 归档解压拒绝绝对路径、`..` 穿越和逃离目标目录的符号链接。
- Systemless 不执行 `setenforce 0`，不默认 remount 真实只读分区。
