package ops

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// SSH 工具的测试聚焦在"能在没有真实服务器的情况下验证的部分"：
// 参数解析、安全边界、错误分类、会话注册表。真实握手需要外部服务器，
// 不作为单元测试依赖。

func TestSSHIsRegisteredAsPublicTool(t *testing.T) {
	found := false
	for _, name := range SupportedTools() {
		if name == "aplm_ssh" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("aplm_ssh is not among the public tools")
	}
}

func TestSSHUnknownActionIsRejected(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{"action": "definitely_not_real"},
	})
	if result.Success {
		t.Fatalf("unknown action unexpectedly succeeded: %+v", result)
	}
	if !strings.Contains(result.Message, "不支持") {
		t.Fatalf("expected readable Chinese rejection, got %q", result.Message)
	}
}

func TestSSHRequiresCredentials(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action": "connect",
			"host":   "127.0.0.1",
		},
	})
	if result.Success {
		t.Fatal("connect without credentials should fail")
	}
	if result.Code != "SSH_ARGUMENT_INVALID" {
		t.Fatalf("unexpected code %q (%s)", result.Code, result.Message)
	}
	if !strings.Contains(result.Message, "password") {
		t.Fatalf("error should mention accepted credential fields, got %q", result.Message)
	}
}

func TestSSHRejectsInvalidPort(t *testing.T) {
	manager := testManager(t)
	for _, port := range []any{float64(0), float64(70000), float64(-1)} {
		result := manager.Execute(context.Background(), Request{
			Tool: "aplm_ssh",
			Args: map[string]any{
				"action":   "connect",
				"host":     "127.0.0.1",
				"port":     port,
				"password": "x",
			},
		})
		if result.Success {
			t.Fatalf("port %v should be rejected", port)
		}
	}
}

func TestSSHExecRequiresCommand(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":   "exec",
			"host":     "127.0.0.1",
			"password": "x",
		},
	})
	if result.Success {
		t.Fatal("exec without command should fail")
	}
	if result.Code != "INVALID_ARGUMENT" {
		t.Fatalf("unexpected code %q (%s)", result.Code, result.Message)
	}
}

func TestSSHExecRejectsOversizedCommand(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":   "exec",
			"host":     "127.0.0.1",
			"password": "x",
			"command":  strings.Repeat("a", sshMaxCommandBytes+1),
		},
	})
	if result.Success {
		t.Fatal("oversized command should be rejected")
	}
	if !strings.Contains(result.Message, "上限") {
		t.Fatalf("expected a size-limit message, got %q", result.Message)
	}
}

func TestSSHConnectFailureIsClassified(t *testing.T) {
	manager := testManager(t)
	// 保留一个必定拒绝连接的端口，避免测试依赖外部网络。
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":    "connect",
			"host":      "127.0.0.1",
			"port":      float64(1),
			"user":      "nobody",
			"password":  "x",
			"timeoutMs": float64(500),
		},
	})
	if result.Success {
		t.Skip("port 1 unexpectedly accepted a connection")
	}
	if result.Code != "SSH_CONNECT_FAILED" && result.Code != "SSH_HANDSHAKE_FAILED" {
		t.Fatalf("unexpected failure code %q (%s)", result.Code, result.Message)
	}
}

func TestSSHListIsEmptyInitially(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{"action": "list"},
	})
	if !result.Success {
		t.Fatalf("list failed: %+v", result)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data shape %T", result.Data)
	}
	if count, _ := data["count"].(int); count != 0 {
		t.Fatalf("expected zero sessions, got %v", data["count"])
	}
}

func TestSSHCloseMissingSession(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{"action": "close", "sessionId": "does-not-exist"},
	})
	if result.Success {
		t.Fatal("closing a missing session should fail")
	}
	if result.Code != "SSH_SESSION_NOT_FOUND" {
		t.Fatalf("unexpected code %q (%s)", result.Code, result.Message)
	}
}

func TestSSHCloseAllIsIdempotent(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{"action": "close", "all": true},
	})
	if !result.Success {
		t.Fatalf("close all failed: %+v", result)
	}
}

func TestSSHKeysListsEmptyDirectory(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{"action": "keys"},
	})
	if !result.Success {
		t.Fatalf("keys failed: %+v", result)
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("unexpected data shape %T", result.Data)
	}
	if _, ok := data["keys"]; !ok {
		t.Fatalf("keys payload missing: %+v", data)
	}
	wantDir := filepath.Join(manager.cfg.WorkDir, ".aplm123-state", "ssh", "keys")
	if data["dir"] != wantDir {
		t.Fatalf("key dir = %v, want %v", data["dir"], wantDir)
	}
}

func TestSSHForwardRequiresTarget(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":   "forward",
			"host":     "127.0.0.1",
			"password": "x",
		},
	})
	if result.Success {
		t.Fatal("forward without target should fail")
	}
	if !strings.Contains(result.Message, "targetHost") {
		t.Fatalf("expected targetHost in error, got %q", result.Message)
	}
}

func TestSSHForwardRejectsInvalidTargetPort(t *testing.T) {
	manager := testManager(t)
	result := manager.Execute(context.Background(), Request{
		Tool: "aplm_ssh",
		Args: map[string]any{
			"action":     "forward",
			"host":       "127.0.0.1",
			"password":   "x",
			"targetHost": "example.com",
			"targetPort": float64(0),
		},
	})
	if result.Success {
		t.Fatal("targetPort 0 should be rejected")
	}
	if !strings.Contains(result.Message, "targetPort") {
		t.Fatalf("expected targetPort in error, got %q", result.Message)
	}
}

func TestSSHSFTPRequiresPaths(t *testing.T) {
	manager := testManager(t)
	base := map[string]any{"host": "127.0.0.1", "password": "x"}

	upload := map[string]any{"action": "upload"}
	for key, value := range base {
		upload[key] = value
	}
	result := manager.Execute(context.Background(), Request{Tool: "aplm_ssh", Args: upload})
	if result.Success || !strings.Contains(result.Message, "localPath") {
		t.Fatalf("upload should demand localPath, got %+v", result)
	}

	download := map[string]any{"action": "download"}
	for key, value := range base {
		download[key] = value
	}
	result = manager.Execute(context.Background(), Request{Tool: "aplm_ssh", Args: download})
	if result.Success || !strings.Contains(result.Message, "remotePath") {
		t.Fatalf("download should demand remotePath, got %+v", result)
	}
}

// TestSSHShellQuoteEscapesCorrectly 校验 cwd 注入防护。
func TestSSHShellQuoteEscapesCorrectly(t *testing.T) {
	cases := map[string]string{
		"/tmp":             `'/tmp'`,
		"/tmp/a b":         `'/tmp/a b'`,
		"/tmp/it's":        `'/tmp/it'\''s'`,
		"/tmp/$(rm -rf /)": `'/tmp/$(rm -rf /)'`,
		"/tmp/`whoami`":    `'/tmp/` + "`whoami`" + `'`,
		"/tmp/a;b":         `'/tmp/a;b'`,
	}
	for input, want := range cases {
		if got := shellSingleQuote(input); got != want {
			t.Errorf("shellSingleQuote(%q) = %s, want %s", input, got, want)
		}
	}
}

// TestSSHBoundedWriterTruncates 校验输出上限。
func TestSSHBoundedWriterTruncates(t *testing.T) {
	writer := &boundedSSHWriter{limit: 10}
	n, err := writer.Write([]byte("0123456789ABCDEF"))
	if err != nil {
		t.Fatalf("write error: %v", err)
	}
	if n != 16 {
		t.Fatalf("writer must report full consumption to avoid short-write loops, got %d", n)
	}
	if got := writer.String(); got != "0123456789" {
		t.Fatalf("buffered %q, want the first 10 bytes", got)
	}
	if !writer.truncated {
		t.Fatal("writer should be marked truncated")
	}
}

func TestSSHSanitizeRemovesCredentialWords(t *testing.T) {
	err := errorString("ssh: handshake failed with password authentication")
	got := sanitizeSSHError(err)
	if strings.Contains(got, "password") {
		t.Fatalf("sanitized error still mentions credentials: %q", got)
	}
}

func TestSSHKeyErrorIsReadable(t *testing.T) {
	if got := sshKeyError(errorString("ssh: no key found")); got == "" {
		t.Fatal("expected a readable message for an unparsable key")
	}
	if got := sshKeyError(nil); got != "" {
		t.Fatalf("nil error should map to empty string, got %q", got)
	}
}

// TestSSHSessionRegistryBounds 校验会话数量上限。
func TestSSHSessionRegistryBounds(t *testing.T) {
	manager := testManager(t)
	runtime := manager.sshState()
	if runtime.count() != 0 {
		t.Fatalf("fresh registry should be empty, got %d", runtime.count())
	}
	session := &sshSession{
		id:        "test-session",
		host:      "example.com",
		port:      22,
		user:      "root",
		policy:    sshHostKeyPinned,
		createdAt: time.Now(),
		lastUsed:  time.Now(),
	}
	runtime.put(session)
	if runtime.count() != 1 {
		t.Fatalf("registry should hold one session, got %d", runtime.count())
	}
	summary := session.summary()
	if summary["hostKeyMode"] != string(sshHostKeyPinned) {
		t.Fatalf("unexpected host key mode %v", summary["hostKeyMode"])
	}
	session.shutdown()
	runtime.remove(session.id)
	if runtime.count() != 0 {
		t.Fatalf("registry should be empty after removal, got %d", runtime.count())
	}
}

// TestSSHInsecureSessionWarns 校验 insecure 模式一定带安全警告。
func TestSSHInsecureSessionWarns(t *testing.T) {
	session := &sshSession{
		id:        "insecure-test",
		host:      "example.com",
		port:      22,
		policy:    sshHostKeyInsecure,
		createdAt: time.Now(),
		lastUsed:  time.Now(),
	}
	summary := session.summary()
	if _, ok := summary["securityWarning"]; !ok {
		t.Fatal("insecure sessions must carry a security warning in their summary")
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
