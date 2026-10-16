package frontend

import (
	"net/http"
	"time"
)

// WebUI 的只读状态与控制端点。
//
// 这些端点全部在 exemptPaths 白名单里：**不占用任何连接配额**，
// 因此在「流连接占满、普通请求全被 429」的情况下依然可用。
// 这正是解决“服务并发卡死只能重启”的关键——打开 WebUI 就能看到
// 到底是谁占着连接，并一键释放。

// uiStreamsDispatch 按方法把 /api/streams 分派到只读列表或断开操作。
func (s *Server) uiStreamsDispatch(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet, http.MethodHead:
		s.uiStreams(w, r)
	case http.MethodPost, http.MethodDelete:
		s.uiDropStreams(w, r)
	default:
		methodNotAllowed(w, "GET, HEAD, POST, DELETE")
	}
}

// uiStreams 列出当前挂起的流式连接。
func (s *Server) uiStreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	streams := s.streams.snapshot()
	now := time.Now()
	items := make([]map[string]any, 0, len(streams))
	for _, entry := range streams {
		items = append(items, map[string]any{
			"id":         entry.ID,
			"path":       entry.Path,
			"method":     entry.Method,
			"remoteAddr": entry.RemoteAddr,
			"userAgent":  entry.UserAgent,
			"protocol":   entry.Protocol,
			"startedAt":  entry.StartedAt.UTC().Format(time.RFC3339),
			"ageSeconds": int(now.Sub(entry.StartedAt).Seconds()),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"count":       len(items),
		"limit":       cap(s.streamSlots),
		"idleTtl":     int(s.options.StreamIdleTimeout.Seconds()),
		"streams":     items,
		"connections": s.connectionStatus(),
	})
}

// uiDropStreams 断开全部已登记的流式连接。
// 无需重启进程即可释放流配额；短请求配额不受影响。
func (s *Server) uiDropStreams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodDelete {
		methodNotAllowed(w, "POST, DELETE")
		return
	}
	dropped := s.streams.clear()
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"dropped":     dropped,
		"message":     "已断开登记的长连接；流配额将随连接关闭逐步归还",
		"connections": s.connectionStatus(),
	})
}

// uiSummary 是 WebUI 首屏数据：把 status + 连接水位 + 长连接列表合成一次返回，
// 避免面板刷新时打多个请求（在过载时尤其重要）。
func (s *Server) uiSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, "GET, HEAD")
		return
	}
	payload := map[string]any{
		"ok":              true,
		"service":         "running",
		"version":         s.options.Version,
		"mcpAddress":      requestBaseURL(r) + "/mcp",
		"workDir":         s.options.WorkDir,
		"connections":     s.connectionStatus(),
		"securityWarning": anonymousRootLANWarning,
	}
	// broker 不可用时也要能打开面板（这正是最需要它的时候）。
	if s.options.Broker != nil {
		if status, err := s.options.Broker.Status(r.Context()); err == nil {
			payload["broker"] = status
			if value, ok := status["uptimeSeconds"]; ok {
				payload["uptimeSeconds"] = value
			}
			if value, ok := status["rootAvailable"]; ok {
				payload["rootAvailable"] = value
			}
			if value, ok := status["rootFramework"]; ok {
				payload["rootFramework"] = value
			}
			if value, ok := status["taskCount"]; ok {
				payload["taskCount"] = value
			}
		} else {
			payload["brokerError"] = err.Error()
		}
	}
	writeJSON(w, http.StatusOK, payload)
}
