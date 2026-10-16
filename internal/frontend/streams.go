package frontend

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 连接准入与长连接治理。
//
// 背景：原先所有请求共用同一个信号量池（默认 32）。MCP 的 Streamable HTTP
// 与 legacy SSE 是**长期挂起**的连接，客户端不主动断开就会一直占着槽位。
// 累积到 32 个之后，连 /health 都会被拒绝并返回 CONCURRENCY_LIMIT，
// 表现为“服务卡死”，只能重启进程恢复。
//
// 这里的处理分三层：
//  1. 管理端点是**只读探针**，完全不占配额，任何时候都能回答“服务是否活着”。
//  2. 流式连接走独立的 streamSlots 配额，不挤占普通调用的 slots。
//  3. 每个流连接登记在册并带空闲超时，WebUI 可查看并主动断开。

// 免占配额的管理端点。这些是只读探针，用于在过载时仍能诊断服务状态。
var exemptPaths = map[string]struct{}{
	"/health":      {},
	"/status":      {},
	"/version":     {},
	"/ui":          {},
	"/api/ui":      {},
	"/api/streams": {},
}

// isStreamRequest 判断该请求是否可能长时间挂起。
// 依据路径与方法：MCP 的流式端点在任何方法下都可能保持打开。
func isStreamRequest(r *http.Request) bool {
	path := r.URL.Path
	switch {
	case path == "/sse":
		return true
	case strings.HasPrefix(path, "/messages"):
		return true
	case path == "/mcp":
		// Streamable HTTP：GET 用于建立服务端推送流；POST 在开启
		// tasks 扩展时也可能以流式长响应返回。两者都按流式配额处理，
		// 避免长响应占满普通调用池。
		return true
	}
	return false
}

// admit 执行连接准入。返回 release 函数与是否放行。
func (s *Server) admit(w http.ResponseWriter, r *http.Request) (func(), bool) {
	if _, ok := exemptPaths[r.URL.Path]; ok {
		return func() {}, true
	}
	if isStreamRequest(r) {
		select {
		case s.streamSlots <- struct{}{}:
			id := s.streams.add(r)
			return func() {
				<-s.streamSlots
				s.streams.remove(id)
			}, true
		default:
			w.Header().Set("Retry-After", "2")
			writeAPIError(w, http.StatusTooManyRequests, "STREAM_LIMIT",
				fmt.Sprintf("流式连接已达到上限 %d；普通工具调用不受影响，可在 WebUI 断开闲置连接", cap(s.streamSlots)))
			return nil, false
		}
	}
	select {
	case s.slots <- struct{}{}:
		return func() { <-s.slots }, true
	default:
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, http.StatusTooManyRequests, "CONCURRENCY_LIMIT", "并发连接已达到上限")
		return nil, false
	}
}

// streamEntry 描述一条活跃的流式连接。
type streamEntry struct {
	ID         string    `json:"id"`
	Path       string    `json:"path"`
	Method     string    `json:"method"`
	RemoteAddr string    `json:"remoteAddr"`
	UserAgent  string    `json:"userAgent,omitempty"`
	StartedAt  time.Time `json:"startedAt"`
	LastSeenAt time.Time `json:"lastSeenAt"`
	Protocol   string    `json:"protocol,omitempty"`
}

type streamRegistry struct {
	mu      sync.Mutex
	entries map[string]*streamEntry
	idle    time.Duration
	nextID  uint64
}

func newStreamRegistry(idle time.Duration) *streamRegistry {
	return &streamRegistry{
		entries: map[string]*streamEntry{},
		idle:    idle,
	}
}

func (r *streamRegistry) add(req *http.Request) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nextID++
	id := fmt.Sprintf("stream-%d", r.nextID)
	now := time.Now()
	r.entries[id] = &streamEntry{
		ID:         id,
		Path:       req.URL.Path,
		Method:     req.Method,
		RemoteAddr: req.RemoteAddr,
		UserAgent:  truncateForStatus(req.UserAgent(), 128),
		StartedAt:  now,
		LastSeenAt: now,
		Protocol:   req.Header.Get("MCP-Protocol-Version"),
	}
	// 顺带清理明显超期的条目，避免注册表随崩溃的客户端无限增长。
	r.pruneLocked(now)
	return id
}

func (r *streamRegistry) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}

// pruneLocked 移除超过空闲阈值仍未结束的登记项。
// 真实连接由 http.Server 的超时与客户端关闭负责回收；这里只保证
// WebUI 看到的列表不会因为客户端异常退出而永久残留。
func (r *streamRegistry) pruneLocked(now time.Time) {
	if r.idle <= 0 {
		return
	}
	for id, entry := range r.entries {
		if now.Sub(entry.LastSeenAt) > r.idle*4 {
			delete(r.entries, id)
		}
	}
}

// snapshot 返回当前长连接列表，供 WebUI 展示。
func (r *streamRegistry) snapshot() []streamEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked(time.Now())
	out := make([]streamEntry, 0, len(r.entries))
	for _, entry := range r.entries {
		out = append(out, *entry)
	}
	return out
}

func (r *streamRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.entries)
}

// clear 断开全部登记的长连接。
// 这是“服务卡死”场景下的兜底恢复手段：无需重启进程即可释放流配额。
func (r *streamRegistry) clear() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := len(r.entries)
	r.entries = map[string]*streamEntry{}
	return n
}

func truncateForStatus(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
