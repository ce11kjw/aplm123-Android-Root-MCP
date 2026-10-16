# 配置

主配置为 `/data/adb/aplm123-mcp/config.json`，Schema 版本为 1。文件使用同目录临时文件、fsync 和原子替换；JSON 损坏或校验失败时，原文件会重命名为带 UTC 时间戳的 `.corrupt-*`，随后恢复默认值并在状态中告警。

关键默认值：

| 项 | 默认值 |
|---|---|
| 端口 | `5322` |
| 监听 | 回环 + LAN |
| 认证 | 匿名 |
| 旧 SSE | 开启 |
| 工作目录 | `/storage/emulated/0/aplm123AI` |
| 总任务并发 | 8 |
| 重任务并发 | 2 |
| Shell 超时 | 120 秒 |
| 同步结果预览 | 64 KiB |
| 传输块 | 4 MiB |
| 传输总大小 | 0，表示仅受磁盘限制 |
| frontend 目标 UID | 2000 |

持久运行数据不是配置项：MCP 脱敏收据/技能保存在 `paths.stateDir/MCP`，Cookie jar 保存在 `paths.stateDir/network/cookie-jars`，WebSocket 会话只存在于内存。更改工作目录只改变自动技能的用户侧镜像位置，不会迁移权威状态目录。

默认只创建空的工作区根目录；配置中的下载、上传和产物路径只是按需使用的目标，不会在安装或服务启动时预创建。

CLI：

```sh
aplm123d config get
aplm123d config validate
aplm123d config reset
```

MCP 的 `aplm_config` 支持 `get`、`validate`、`update`、`export` 和 `reset`。更新先完整校验再原子替换；端口、监听和并发变化在服务重启后完全生效。
