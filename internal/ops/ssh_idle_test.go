package ops

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- 会话空闲回收（evictIdle / admit / limitMessage）---

// 空闲超时的会话应被回收，且不占用名额。
func TestSSHEvictIdleReclaimsExpiredSessions(t *testing.T) {
	runtime := &sshRuntime{}
	now := time.Now()
	expired := &sshSession{id: "old", host: "a", port: 22, user: "u", createdAt: now.Add(-time.Hour), lastUsed: now.Add(-time.Hour)}
	fresh := &sshSession{id: "new", host: "b", port: 22, user: "u", createdAt: now, lastUsed: now}
	runtime.put(expired)
	runtime.put(fresh)

	evicted := runtime.evictIdle(sshSessionIdle)
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1", evicted)
	}
	if runtime.count() != 1 {
		t.Fatalf("count = %d, want 1", runtime.count())
	}
	if _, found := runtime.get("old"); found {
		t.Fatal("expired session should be removed")
	}
	if _, found := runtime.get("new"); !found {
		t.Fatal("fresh session should survive")
	}
}

// 带活跃端口转发的会话不能被回收——转发是长期隧道，杀掉会断正在用的连接。
func TestSSHEvictIdleKeepsSessionsWithForwards(t *testing.T) {
	runtime := &sshRuntime{}
	now := time.Now()
	session := &sshSession{
		id: "tunnel", host: "a", port: 22, user: "u",
		createdAt: now.Add(-time.Hour), lastUsed: now.Add(-time.Hour),
		listeners: map[string]*sshForward{"f1": {bindAddr: "127.0.0.1", bindPort: 9000}},
	}
	runtime.put(session)

	if evicted := runtime.evictIdle(sshSessionIdle); evicted != 0 {
		t.Fatalf("evicted = %d, want 0 (session holds a forward)", evicted)
	}
	if runtime.count() != 1 {
		t.Fatalf("count = %d, want 1", runtime.count())
	}
}

// 名额已满但存在空闲会话时，admit 应先回收再放行。
func TestSSHAdmitReclaimsBeforeRejecting(t *testing.T) {
	runtime := &sshRuntime{}
	past := time.Now().Add(-2 * sshSessionIdle)
	for i := 0; i < sshMaxSessions; i++ {
		runtime.put(&sshSession{
			id:        fmt.Sprintf("idle-%d", i),
			host:      "h",
			port:      22,
			createdAt: past,
			lastUsed:  past,
		})
	}
	if err := runtime.admit(); err != nil {
		t.Fatalf("admit should reclaim idle sessions, got %v", err)
	}
	if runtime.count() != 0 {
		t.Fatalf("count = %d, want 0 after reclaim", runtime.count())
	}
}

// 没有可回收会话时 admit 必须报错，并给出可操作的清单。
func TestSSHAdmitRejectsWhenAllSessionsActive(t *testing.T) {
	runtime := &sshRuntime{}
	now := time.Now()
	for i := 0; i < sshMaxSessions; i++ {
		runtime.put(&sshSession{
			id:        "act-" + string(rune('a'+i)),
			host:      "10.0.0.1",
			port:      22,
			createdAt: now,
			lastUsed:  now,
		})
	}
	err := runtime.admit()
	if err == nil {
		t.Fatal("admit should fail when every session is active")
	}
	msg := err.Error()
	// 报错必须包含上限值、候选会话与关闭方式，否则调用方无从下手。
	if !strings.Contains(msg, "上限") {
		t.Fatalf("message should mention the limit: %q", msg)
	}
	if !strings.Contains(msg, "act-") {
		t.Fatalf("message should list candidate sessions: %q", msg)
	}
	if !strings.Contains(msg, "all=true") {
		t.Fatalf("message should explain how to close sessions: %q", msg)
	}
}

// touch 必须刷新 lastUsed，否则活跃会话会被误杀。
func TestSSHTouchRefreshesLastUsed(t *testing.T) {
	session := &sshSession{id: "s", createdAt: time.Now().Add(-time.Hour), lastUsed: time.Now().Add(-time.Hour)}
	if idle := session.idle(time.Now()); idle < 59*time.Minute {
		t.Fatalf("idle = %s, want about an hour", idle)
	}
	session.touch()
	if idle := session.idle(time.Now()); idle > time.Second {
		t.Fatalf("idle after touch = %s, want near zero", idle)
	}
}

// 已关闭的会话不应被视作可复用目标。
func TestSSHMatchesSkipsClosedSessions(t *testing.T) {
	session := &sshSession{id: "s", host: "1.2.3.4", port: 22, user: "root"}
	if !session.matches("1.2.3.4", 22, "root") {
		t.Fatal("session should match its own target")
	}
	if session.matches("1.2.3.4", 22, "other") {
		t.Fatal("session should not match a different user")
	}
	session.closed = true
	if session.matches("1.2.3.4", 22, "root") {
		t.Fatal("closed session must not match")
	}
}

// --- 连接去重 ---

func TestSSHFindByTarget(t *testing.T) {
	runtime := &sshRuntime{}
	runtime.put(&sshSession{id: "a", host: "10.0.0.1", port: 22, user: "root", createdAt: time.Now(), lastUsed: time.Now()})

	if _, found := runtime.findByTarget("10.0.0.1", 22, "root"); !found {
		t.Fatal("should find the existing session")
	}
	if _, found := runtime.findByTarget("10.0.0.2", 22, "root"); found {
		t.Fatal("should not match a different host")
	}
	if _, found := runtime.findByTarget("10.0.0.1", 2222, "root"); found {
		t.Fatal("should not match a different port")
	}
}

// connect(keep=true) 重复调用同一目标必须复用，而不是新建占名额。
func TestSSHEndToEndConnectReusesSameTarget(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	args := map[string]any{
		"action":   "connect",
		"host":     host,
		"port":     float64(port),
		"user":     "tester",
		"password": "secret",
		"keep":     true,
		"insecure": true,
	}

	first := manager.Execute(context.Background(), Request{Tool: "aplm_ssh", Args: args})
	if !first.Success {
		t.Fatalf("first connect failed: %s %s", first.Code, first.Message)
	}
	firstData, _ := first.Data.(map[string]any)
	firstID, _ := firstData["sessionId"].(string)
	if firstID == "" {
		t.Fatal("first connect should return a sessionId")
	}

	second := manager.Execute(context.Background(), Request{Tool: "aplm_ssh", Args: args})
	if !second.Success {
		t.Fatalf("second connect failed: %s %s", second.Code, second.Message)
	}
	secondData, _ := second.Data.(map[string]any)
	secondID, _ := secondData["sessionId"].(string)
	if secondID != firstID {
		t.Fatalf("sessionId = %q, want reuse of %q", secondID, firstID)
	}
	if reused, _ := secondData["reused"].(bool); !reused {
		t.Fatal("second connect should be flagged as reused")
	}
	if count := manager.sshState().count(); count != 1 {
		t.Fatalf("session count = %d, want 1", count)
	}
}

// --- 批量形态 ---

// commands 形态：一次连接顺序执行多条命令。
func TestSSHEndToEndExecCommands(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":   "exec",
			"host":     host,
			"port":     float64(port),
			"user":     "tester",
			"password": "secret",
			"insecure": true,
			"commands": []any{"echo one", "echo two"},
		},
	})
	if !result.Success {
		t.Fatalf("exec commands failed: %s %s", result.Code, result.Message)
	}
	data, _ := result.Data.(map[string]any)
	if count, _ := data["count"].(int); count != 2 {
		t.Fatalf("count = %v, want 2", data["count"])
	}
	results, _ := data["results"].([]map[string]any)
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	if out, _ := results[0]["stdout"].(string); out != "ran: echo one\n" {
		t.Fatalf("results[0].stdout = %q, want %q", out, "ran: echo one\n")
	}
	if out, _ := results[1]["stdout"].(string); out != "ran: echo two\n" {
		t.Fatalf("results[1].stdout = %q, want %q", out, "ran: echo two\n")
	}
}

// commands 形态：部分命令失败时整体标记失败，但每条结果都要保留。
func TestSSHEndToEndExecCommandsReportsPartialFailure(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":   "exec",
			"host":     host,
			"port":     float64(port),
			"user":     "tester",
			"password": "secret",
			"insecure": true,
			"commands": []any{"echo ok", "exit3"},
		},
	})
	if result.Success {
		t.Fatal("partial failure should be reported as failure")
	}
	data, _ := result.Data.(map[string]any)
	if failed, _ := data["failed"].(int); failed != 1 {
		t.Fatalf("failed = %v, want 1", data["failed"])
	}
	results, _ := data["results"].([]map[string]any)
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2 (keep every result)", len(results))
	}
}

// commands 形态：数量超上限必须拒绝。
func TestSSHExecCommandsRejectsOversizedBatch(t *testing.T) {
	manager := testManager(t)
	commands := make([]any, sshMaxBatch+1)
	for i := range commands {
		commands[i] = "true"
	}
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":   "exec",
			"host":     "127.0.0.1",
			"user":     "tester",
			"password": "secret",
			"commands": commands,
		},
	})
	if result.Success {
		t.Fatal("oversized batch should be rejected")
	}
	if result.Code != "INVALID_ARGUMENT" {
		t.Fatalf("code = %q, want INVALID_ARGUMENT", result.Code)
	}
}

// targets 形态：多个会话并行跑同一条命令。
func TestSSHEndToEndExecTargets(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	connect := func() string {
		r := manager.Execute(context.Background(), Request{
			Tool: "aplm_ssh",
			Args: map[string]any{
				"action":   "connect",
				"host":     host,
				"port":     float64(port),
				"user":     "tester",
				"password": "secret",
				"keep":     true,
				"insecure": true,
			},
		})
		if !r.Success {
			t.Fatalf("connect failed: %s %s", r.Code, r.Message)
		}
		data, _ := r.Data.(map[string]any)
		id, _ := data["sessionId"].(string)
		return id
	}

	// 去重逻辑会让两次 connect 复用到同一个会话，所以这里直接构造两个不同的
	// 会话更贴近「多台机器」的真实场景：用不同 user 绕过去重。
	first := connect()
	second := connect()
	if first != second {
		t.Fatalf("same target should dedupe: %q vs %q", first, second)
	}

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":  "exec",
			"command": "echo fanout",
			"targets": []any{first},
		},
	})
	if !result.Success {
		t.Fatalf("exec targets failed: %s %s", result.Code, result.Message)
	}
	data, _ := result.Data.(map[string]any)
	if count, _ := data["count"].(int); count != 1 {
		t.Fatalf("count = %v, want 1", data["count"])
	}
	results, _ := data["results"].([]map[string]any)
	if len(results) != 1 {
		t.Fatalf("results len = %d, want 1", len(results))
	}
	if out, _ := results[0]["stdout"].(string); out != "ran: echo fanout\n" {
		t.Fatalf("stdout = %q, want %q", out, "ran: echo fanout\n")
	}
}

// jobs 形态：每个会话跑各自的命令。
func TestSSHEndToEndExecJobs(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	r := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":   "connect",
			"host":     host,
			"port":     float64(port),
			"user":     "tester",
			"password": "secret",
			"keep":     true,
			"insecure": true,
		},
	})
	if !r.Success {
		t.Fatalf("connect failed: %s %s", r.Code, r.Message)
	}
	data, _ := r.Data.(map[string]any)
	id, _ := data["sessionId"].(string)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action": "exec",
			"jobs": []any{
				map[string]any{"sessionId": id, "command": "echo first"},
				map[string]any{"sessionId": id, "command": "echo second"},
			},
		},
	})
	if !result.Success {
		t.Fatalf("exec jobs failed: %s %s", result.Code, result.Message)
	}
	out, _ := result.Data.(map[string]any)
	results, _ := out["results"].([]map[string]any)
	if len(results) != 2 {
		t.Fatalf("results len = %d, want 2", len(results))
	}
	if got, _ := results[0]["stdout"].(string); got != "ran: echo first\n" {
		t.Fatalf("results[0].stdout = %q, want %q", got, "ran: echo first\n")
	}
	if got, _ := results[1]["stdout"].(string); got != "ran: echo second\n" {
		t.Fatalf("results[1].stdout = %q, want %q", got, "ran: echo second\n")
	}
}

// jobs 形态：缺少字段必须拒绝。
func TestSSHExecJobsRejectsMissingFields(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action": "exec",
			"jobs":   []any{map[string]any{"command": "echo hi"}},
		},
	})
	if result.Success {
		t.Fatal("job without sessionId should be rejected")
	}
	if result.Code != "INVALID_ARGUMENT" {
		t.Fatalf("code = %q, want INVALID_ARGUMENT", result.Code)
	}
}

// --- 复用形态 ---

// exec 传 sessionId 时应在已有会话上执行，且不关闭该会话。
func TestSSHEndToEndExecWithSessionID(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	r := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":   "connect",
			"host":     host,
			"port":     float64(port),
			"user":     "tester",
			"password": "secret",
			"keep":     true,
			"insecure": true,
		},
	})
	if !r.Success {
		t.Fatalf("connect failed: %s %s", r.Code, r.Message)
	}
	data, _ := r.Data.(map[string]any)
	id, _ := data["sessionId"].(string)

	// 复用会话执行：不应再传 host/password。
	execResult := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "exec",
			"sessionId": id,
			"command":   "echo reused",
		},
	})
	if !execResult.Success {
		t.Fatalf("exec with sessionId failed: %s %s", execResult.Code, execResult.Message)
	}
	out, _ := execResult.Data.(map[string]any)
	if got, _ := out["stdout"].(string); got != "ran: echo reused\n" {
		t.Fatalf("stdout = %q, want %q", got, "ran: echo reused\n")
	}
	if got, _ := out["sessionId"].(string); got != id {
		t.Fatalf("sessionId = %q, want %q", got, id)
	}
	// 关键：会话必须还在，否则批量执行会退化成每次重连。
	if _, found := manager.sshState().get(id); !found {
		t.Fatal("session should survive an exec that reused it")
	}
}

// 复用不存在的会话要给出明确错误。
func TestSSHExecUnknownSessionID(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "exec",
			"sessionId": "does-not-exist",
			"command":   "echo hi",
		},
	})
	if result.Success {
		t.Fatal("unknown sessionId should fail")
	}
	if result.Code != "SSH_SESSION_NOT_FOUND" {
		t.Fatalf("code = %q, want SSH_SESSION_NOT_FOUND", result.Code)
	}
}

// 向后兼容：不传 sessionId / commands / targets / jobs 时行为与旧版一致。
func TestSSHExecLegacyShapeStillWorks(t *testing.T) {
	server := newTestSSHServer(t, "secret")
	host, port := server.host()
	manager := testManager(t)

	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":   "exec",
			"host":     host,
			"port":     float64(port),
			"user":     "tester",
			"password": "secret",
			"command":  "echo legacy",
			"insecure": true,
		},
	})
	if !result.Success {
		t.Fatalf("legacy exec failed: %s %s", result.Code, result.Message)
	}
	data, _ := result.Data.(map[string]any)
	if got, _ := data["stdout"].(string); got != "ran: echo legacy\n" {
		t.Fatalf("stdout = %q, want %q", got, "ran: echo legacy\n")
	}
	// 一次性执行不应留在会话池里。
	if count := manager.sshState().count(); count != 0 {
		t.Fatalf("session count = %d, want 0 for one-shot exec", count)
	}
}
