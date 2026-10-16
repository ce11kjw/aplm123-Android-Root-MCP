package frontend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// 这些测试针对「并发卡死」这一具体缺陷：
// 修复前，长短连接共用一个配额池，客户端不关闭的流式连接会把池占满，
// 之后连 /health 都会返回 429，只能重启进程恢复。
//
// 修复后必须满足：
//  1. 管理端点在配额耗尽时依然可用。
//  2. 长连接占满流配额不会影响普通请求。
//  3. 普通请求占满不影响流连接建立。
//  4. 可通过控制端点释放流配额，无需重启。

// occupyStreams 占满全部流配额，返回释放函数。
func occupyStreams(t *testing.T, server *Server) func() {
	t.Helper()
	total := cap(server.streamSlots)
	released := make([]func(), 0, total)
	for i := 0; i < total; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5322/mcp", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		recorder := httptest.NewRecorder()
		release, admitted := server.admit(recorder, req)
		if !admitted {
			t.Fatalf("流配额在第 %d 次就被拒绝，容量应为 %d", i+1, total)
		}
		released = append(released, release)
	}
	return func() {
		for _, release := range released {
			release()
		}
	}
}

func occupyShortSlots(t *testing.T, server *Server) func() {
	t.Helper()
	total := cap(server.slots)
	released := make([]func(), 0, total)
	for i := 0; i < total; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5322/version", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		recorder := httptest.NewRecorder()
		release, admitted := server.admit(recorder, req)
		if !admitted {
			t.Fatalf("短请求配额在第 %d 次就被拒绝，容量应为 %d", i+1, total)
		}
		released = append(released, release)
	}
	return func() {
		for _, release := range released {
			release()
		}
	}
}

// TestManagementEndpointsSurviveExhaustedStreams 是本次修复的核心断言：
// 流配额被占满时，/health 与 /status 必须仍然可用。
func TestManagementEndpointsSurviveExhaustedStreams(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	release := occupyStreams(t, server)
	defer release()

	for _, path := range []string{"/health", "/status", "/api/ui", "/api/streams"} {
		req := localRequest(http.MethodGet, "http://127.0.0.1:5322"+path, nil)
		recorder := httptest.NewRecorder()
		server.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s 在流配额耗尽时返回 %d（应保持可用）：%s",
				path, recorder.Code, recorder.Body.String())
		}
	}
}

// TestShortRequestsUnaffectedByExhaustedStreams 验证流连接不再挤占普通调用。
func TestShortRequestsUnaffectedByExhaustedStreams(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	// 先占满流配额，再确认普通请求仍有完整配额可用。
	releaseStreams := occupyStreams(t, server)
	defer releaseStreams()

	// 普通请求配额应完全未被占用。
	if got := len(server.slots); got != 0 {
		t.Fatalf("流连接占用了普通请求槽位：%d", got)
	}
	// 并且真的能连续放行满额普通请求。
	releaseShort := occupyShortSlots(t, server)
	defer releaseShort()
}

// TestStreamSlotsIndependentFromShortSlots 反向验证：普通请求占满不影响流连接。
func TestStreamSlotsIndependentFromShortSlots(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	releaseShort := occupyShortSlots(t, server)
	defer releaseShort()

	if got := len(server.streamSlots); got != 0 {
		t.Fatalf("普通请求占用了流配额槽位：%d", got)
	}
	releaseStreams := occupyStreams(t, server)
	defer releaseStreams()
}

// TestStreamLimitIsReportedDistinctly 验证超限时给出的是可区分的错误码，
// 而不是把所有问题都归为同一个 CONCURRENCY_LIMIT。
func TestStreamLimitIsReportedDistinctly(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	release := occupyStreams(t, server)
	defer release()

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5322/mcp", nil)
	req.RemoteAddr = "127.0.0.1:40000"
	recorder := httptest.NewRecorder()
	if _, admitted := server.admit(recorder, req); admitted {
		t.Fatal("流配额已满时不应再放行")
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("状态码 = %d，期望 429", recorder.Code)
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("响应不是 JSON：%s", recorder.Body.String())
	}
	errObj, _ := payload["error"].(map[string]any)
	if code, _ := errObj["code"].(string); code != "STREAM_LIMIT" {
		t.Fatalf("错误码 = %q，期望 STREAM_LIMIT（与短请求的 CONCURRENCY_LIMIT 区分开）", code)
	}
}

// TestDropStreamsRecoversWithoutRestart 验证控制端点能释放流配额。
func TestDropStreamsRecoversWithoutRestart(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	// 占满流配额并登记到注册表。
	total := cap(server.streamSlots)
	for i := 0; i < total; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5322/mcp", nil)
		req.RemoteAddr = "127.0.0.1:40000"
		release, admitted := server.admit(httptest.NewRecorder(), req)
		if !admitted {
			t.Fatalf("第 %d 次流准入失败", i+1)
		}
		_ = release
	}
	if got := server.streams.count(); got != total {
		t.Fatalf("注册的长连接数 = %d，期望 %d", got, total)
	}

	// 通过控制端点清空注册表。
	req := localRequest(http.MethodPost, "http://127.0.0.1:5322/api/streams", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("断开端点返回 %d：%s", recorder.Code, recorder.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if dropped, _ := payload["dropped"].(float64); int(dropped) != total {
		t.Fatalf("dropped = %v，期望 %d", payload["dropped"], total)
	}
	if got := server.streams.count(); got != 0 {
		t.Fatalf("注册表未清空，仍有 %d 条", got)
	}
}

// TestStreamRegistrySnapshotIsBounded 验证注册表快照包含必要字段。
func TestStreamRegistrySnapshotIsBounded(t *testing.T) {
	registry := newStreamRegistry(10 * time.Minute)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5322/mcp", nil)
	req.RemoteAddr = "192.168.1.9:5555"
	req.Header.Set("User-Agent", "probe/1.0")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	id := registry.add(req)
	if id == "" {
		t.Fatal("登记未返回 id")
	}
	snapshot := registry.snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("快照条数 = %d，期望 1", len(snapshot))
	}
	entry := snapshot[0]
	if entry.Path != "/mcp" || entry.RemoteAddr != "192.168.1.9:5555" {
		t.Fatalf("快照字段不正确：%+v", entry)
	}
	if entry.Protocol != "2026-07-28" {
		t.Fatalf("协议字段 = %q", entry.Protocol)
	}
	registry.remove(id)
	if registry.count() != 0 {
		t.Fatal("移除后注册表应为空")
	}
}

// TestStreamRegistryConcurrentAccess 并发压力测试，替代不可用的 race detector。
func TestStreamRegistryConcurrentAccess(t *testing.T) {
	registry := newStreamRegistry(10 * time.Minute)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 40; j++ {
				req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5322/mcp", nil)
				req.RemoteAddr = "127.0.0.1:40000"
				id := registry.add(req)
				_ = registry.snapshot()
				_ = registry.count()
				registry.remove(id)
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("并发访问注册表发生死锁")
	}
	if got := registry.count(); got != 0 {
		t.Fatalf("并发结束后注册表残留 %d 条", got)
	}
}

// TestIsStreamRequestClassification 验证流式路径识别。
func TestIsStreamRequestClassification(t *testing.T) {
	cases := map[string]bool{
		"/mcp":            true,
		"/sse":            true,
		"/messages":       true,
		"/messages/123":   true,
		"/health":         false,
		"/status":         false,
		"/api/ui":         false,
		"/api/streams":    false,
		"/version":        false,
		"/transfer/files": false,
	}
	for path, want := range cases {
		req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:5322"+path, nil)
		if got := isStreamRequest(req); got != want {
			t.Errorf("isStreamRequest(%q) = %v，期望 %v", path, got, want)
		}
	}
}

// TestPanelPageIsServed 验证面板页可直接访问且不占配额。
func TestPanelPageIsServed(t *testing.T) {
	server, err := New(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	req := localRequest(http.MethodGet, "http://127.0.0.1:5322/ui", nil)
	recorder := httptest.NewRecorder()
	server.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("/ui 返回 %d", recorder.Code)
	}
	body := recorder.Body.String()
	if len(body) == 0 {
		t.Fatal("/ui 返回空内容")
	}
	// 面板必须包含连接池与断开入口，否则无法用于恢复卡死状态。
	for _, marker := range []string{"连接池", "断开全部长连接", "/api/streams"} {
		if !containsString(body, marker) {
			t.Errorf("面板缺少关键元素 %q", marker)
		}
	}
}

func containsString(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
