# Android 网页与屏幕自动化

## 能力分层

1. 静态网页：HTTP 请求、持久 Cookie jar、HTML 标题/正文/链接/元数据提取。
2. Feed：RSS 2.0、RSS 1.0/RDF 与 Atom 解析。
3. 动态网页：在 Android 系统浏览器中打开 URL，等待渲染，再组合 `screencap` 和 `uiautomator dump` 获取可见截图与无障碍文本。
4. 实时通道：受限的 WebSocket 会话，显式 connect/send/receive/ping/close。

## 重要限制

本服务是无 Activity 的 Go Root 守护进程，不能在进程内“嵌入 WebView View”。系统浏览器捕获是最小、兼容且可维护的动态渲染方案。静态 HTML 提取不会执行 JavaScript；页面依赖 JS 时应切换 browser_capture。

## 安全规则

- 仅允许 http/https 或 ws/wss；限制重定向、响应体、消息大小、队列和会话数。
- Cookie 按命名 jar 隔离并以 0600 原子持久化；拒绝 public suffix/跨域 Cookie，jar 数量和列表序列化字节均有界。
- 不持久化 Authorization；跨源重定向删除 Authorization、Cookie、Referer，且不重放请求正文；错误信息也必须删除 URL query/userinfo。
- WebSocket 所有操作先清理 TTL，消息大小包含 Base64 膨胀后仍不得超过结果预览预算，关闭必须有超时和 `CloseNow` 兜底。
- 动态捕获先解析并强制默认浏览器包；截图校验 PNG，UI XML 和节点输出有界。
