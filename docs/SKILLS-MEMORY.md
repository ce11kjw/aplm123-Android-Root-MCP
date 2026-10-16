# 技能自动加载与长期记忆设计

## 目标与边界

这套机制解决三个问题：让 AI 在调用工具前得到少量相关经验；让真实完成的 MCP 任务沉淀为下次可复用的技能；在不保存秘密和大段输出的前提下控制磁盘与 Token。它不会猜测客户端未发送的高层意图，也不会把所有技能正文一次塞进模型上下文。

## 三层渐进加载

| 层级 | 内容 | 何时返回 | 上限 |
|---|---|---|---|
| 自动提示 | `id/title/summary/source/score` | MCP 初始化指令要求任务开始时先匹配；普通工具也自动匹配并写入 `_meta.skillHints` | 最多 3 条 |
| 技能索引 | `id/title/summary/source/updatedAt` | 显式调用 `aplm_skill.list` | 默认 20，最大 100，支持 `offset/nextOffset` |
| 完整正文 | 单个 `SKILL.md` | 显式调用 `aplm_skill.get` | 每次一个 |

两个内置技能（Android 网页与屏幕自动化、低 Token MCP 工作流）通过 Go `embed` 编入二进制。启动时只建立轻量索引；匹配优先使用明确的工具名、action 和内置触发目录，再用中英文词元辅助排序。精确工具/action 的权重高于普通关键词，避免“网络”“系统”等宽泛词把无关技能排在前面。

## 自动学习时机

同步调用只有在工具得到真实结果后才提交记忆。后台调用返回 `TASK_ACCEPTED` 时不学习；任务真正进入 `succeeded/failed/cancelled/timed_out` 等终态并已持久化后，完成钩子才写收据。以下控制面调用不会形成经验：

- `aplm_skill`、`aplm_task`、`aplm_status`；
- `aplm_capabilities.get`、`aplm_config.get`；
- 仅表示排队成功的 `TASK_ACCEPTED`。

这样能避免高频轮询把真正的执行经验淹没，也减少索引、磁盘和 Token 增长。

## v4 收据

每次可学习完成记录都生成一个密码学随机 128 位收据 ID，并在锁内分配单调事件序号。文件名包含 UTC 时间、脱敏参数指纹前缀和随机 ID；恢复以收据 ID 判重、以事件序号确定“最近”，不以墙钟时间作水位，因此设备时间回拨不会漏学、重复聚合或把旧结果误当最新结果。

收据只保存：

- 工具、action、任务 ID（若有）和脱敏参数形状；
- success、code、strategy、duration、exitCode、rebootRequired；
- 错误类别及不可逆摘要；
- 产物类型/扩展名，不保存产物路径或文件名。

不保存完整结果、stdout、stderr、响应体、页面正文、WebSocket 消息或命令输出。

## 脱敏模型

参数模板采用“安全白名单 + 默认隐藏”，不是只维护一份容易漏项的秘密字段黑名单。

- Authorization、Cookie、Token、password、secret、headers、env、stdin、body、content、data、message、text、command、script、query、reason、pattern 和通用 value 等字符串只记录类型/长度。
- URL 删除用户信息、查询参数和原始主机，只保留协议、主机结构及路径段数量。
- 路径只保留绝对/相对、段数和白名单扩展名；包名/组件先严格校验；未知字符串默认脱敏。
- 模板最大递归深度 6、最多 256 个节点、序列化后最多 8 KiB；超限时退化成形状摘要。

## 自动技能内容

同一 `tool/action` 聚合为一个 `learned/.../SKILL.md`。正文包含：

- 尝试、成功和“失败后再次成功”的恢复次数；
- 最近结果、策略、耗时、退出码和错误类别；
- 最近已知成功的脱敏参数模板与验收信号；
- 失败类别计数、是否需要重启、产物类型；
- 适用条件、执行步骤与下一次调用的验证规则。

权威副本在 `/data/adb/aplm123-mcp/MCP/skills/learned`，用户可见镜像在 `/storage/emulated/0/aplm123AI/MCP/skills/learned`。内部写入成功而镜像暂时不可用时，权威结果保留，后续启动或写入会修复镜像；`stats.lastPersistenceError` 暴露最近持久化问题。

## 后台任务防重放

任务快照与完成收据是两份独立证据。服务重启后先用完成收据结算仍标记为 running/interrupted 的任务，再决定是否恢复：

1. 找到保留的终态收据：恢复成功/失败终态，不执行工具。
2. 只在压缩账本找到任务 ID：返回 `COMPLETION_COMPACTED`，不重做任务。
3. 没有完成证据时，只有同时标记为只读且幂等的工具可以恢复；其他工具统一返回 `RESTART_REVIEW_REQUIRED`。HTTP POST 等一次性副作用不会自动重放。
4. 完成账本校验失败：返回 `COMPLETION_LEDGER_UNTRUSTED`，所有中断任务故障闭锁。
5. 仅确认未完成且只读幂等的持久任务才重新排队；恢复前必须先成功持久化 queued 状态。

完成账本保存排序后的精确 SHA-256 任务 ID 集合、魔数、条目数和整体 SHA-256 校验和。它不是 Bloom filter，不会因概率误判把未完成任务当成已完成。

## 保留与压缩

原始收据的高水位为 20,000 个或 64 MiB。触发任一阈值后，按最旧优先压缩，直到同时不超过 16,000 个和 48 MiB：

- 统计经验已经聚合在自动技能中；
- 被删除收据的后台任务 ID 先原子写入精确完成账本；
- 账本写入失败时不删除收据；
- 删除后同步清理索引中的已处理收据 ID。

因此磁盘和重建成本有界，同时保留“不重复执行已完成任务”的安全性质。

任务侧还有独立边界：包含 header/body/content/token/命令等敏感载荷的调用拒绝转成持久后台任务；任务 API 从不返回原始 arguments，终态快照在完成收据提交后删除 arguments、stdout/stderr 和错误细节。终态任务快照最多保留 2,000 个或 64 MiB，触发后压缩到 1,500 个且不高于 48 MiB；单任务日志上限 4 MiB。精确完成账本只保留仍存在任务快照的 ID，避免防重放索引无限增长。

## 遗忘语义

`aplm_skill.forget` 只允许删除 `learned/` 技能，并要求 `confirmDangerous=true`。删除顺序先原子递增并持久化 generation tombstone，再移除索引、内部正文和镜像。旧收据仍可用于审计和任务防重放，但其代次不匹配，哪怕时间戳来自未来也不会在重启后复活技能；同一 tool/action 之后的新完成记录使用新代次并从零统计。

## 运维检查

调用 `aplm_skill.stats` 可检查：内置/自动技能数、保留收据数和字节数、保留阈值、权威目录、镜像目录、完成账本可信状态及最近持久化错误。若 `completionLedgerTrusted=false`，先备份内部状态并检查存储损坏；不要删除任务快照后盲目重跑破坏性操作。
