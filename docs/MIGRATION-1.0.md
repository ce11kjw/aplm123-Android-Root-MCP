# 从 0.09 升级到 1.0

本版本引入**认证**与**监听范围语义调整**，两处都可能影响既有调用方。
升级前请先读完本文，按「检查 → 升级 → 验证」三步走。

## 一、升级前检查

### 1.1 你是否从局域网访问

```sh
# 在手机上确认当前监听范围
grep listenLan /data/adb/<模块目录>/config/config.json
```

- **`listenLan: false`** → 只有本机可访问。升级后本机仍然免认证，**无需任何改动**。
- **`listenLan: true`** → 同网段设备可访问。升级后这些设备**必须携带 token**。

### 1.2 记录当前使用的配置项

`network.listenLoopback` 在 1.0 中被移除。旧配置里存在该字段**不会导致加载失败**
（JSON 解码忽略未知字段），但通过 `aplm_config` 的 schema 校验时会因
`additionalProperties: false` 被拒绝。请先把它删掉。

## 二、两处行为变更

### 变更 1：认证默认开启（对局域网）

| 访问来源 | 0.09 | 1.0 默认 |
|---|---|---|
| `127.0.0.1` / `::1` | 放行 | **放行**（`loopbackAnonymous=true`） |
| 局域网 | 放行 | **需要 `Authorization: Bearer <token>`** |

**本机用法零影响。** 这是刻意设计：绝大多数客户端（MCP 客户端、
你的脚本）都通过环回访问。

### 变更 2：`listenLoopback` 被移除

该字段**从未真正控制监听**——环回始终被监听，把它设成 `false` 既不会
关闭环回，也不带来任何保护。1.0 删除它，只保留一个有实际作用的开关：

| `network.listenLan` | 实际行为 |
|---|---|
| `false` | 仅监听 `127.0.0.1` / `::1` |
| `true` | 额外监听 `0.0.0.0` / `::`（受认证约束） |

## 三、升级步骤

### 3.1 安装新模块

从 Release 下载 `aplm123-Android-Root-MCP-v1.0-universal.zip`，
在 APatch / KernelSU / Magisk 管理器中安装并重启。

### 3.2 取回 token

首次启动会自动生成：

```sh
cat /data/adb/<模块目录>/token
# 或通过面板查看前 8 位
```

文件权限为 `0600`、属主 root。

### 3.3 按需调整

**如果你想彻底关掉局域网访问**（最安全）：

```sh
# 方式一：WebUI 面板
#   http://127.0.0.1:5322/ui → 安全 → 取消勾选「允许局域网访问」

# 方式二：命令行
aplm123d config   # 或用 aplm_config 工具（见 docs/CONFIG.md）
```

改完**需要重启服务**才生效（监听器在启动时绑定）。

**如果你希望局域网继续免认证**（不推荐）：

```json
{ "security": { "requireToken": false } }
```

## 四、验证

```sh
# 1. 本机免认证仍可用
curl -s http://127.0.0.1:5322/health

# 2. 探活端点不需要 token
curl -s http://127.0.0.1:5322/version

# 3. 局域网未带 token → 401
curl -s -o /dev/null -w '%{http_code}\n' http://<手机IP>:5322/mcp

# 4. 带 token → 200
curl -s -H "Authorization: Bearer $(cat token)" http://<手机IP>:5322/mcp
```

## 五、回滚

1. 重新刷入 0.09 的模块 zip。
2. 如果配置已被 1.0 改写（缺少 `listenLoopback`），旧版本加载时会用默认值
   `true` 补齐——不影响启动。

## 六、常见问题

**Q：升级后本机 MCP 客户端连不上了？**
A：检查是否把 `loopbackAnonymous` 关成了 `false`。改回 `true` 即可。
若已无法通过面板操作，直接编辑配置文件后重启。

**Q：忘记 token 了？**
A：WebUI 面板可轮换；或 `cat <stateDir>/token`；或删掉该文件后重启自动重建。

**Q：能不能完全不设认证？**
A：可以（`requireToken: false`），但不建议：同网段任何设备都能拿到完整 Root。

**Q：`network.listenLoopback` 报「未知字段」？**
A：该字段已移除。从配置里删掉即可。
